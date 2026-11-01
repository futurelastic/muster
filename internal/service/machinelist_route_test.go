package service

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/machinelist"
)

// muster #352: GET and PUT /v1/machine-list. The holder is a fake: what is
// under test here is the order of the checks (the grant before the body), the
// body cap, the signed digest, the audit trail and the pass-through of the
// holder's answers. Verifying and applying a list is the holder's job and is
// tested where the holder lives.

type fakeHolder struct {
	mu          sync.Mutex
	held        []byte
	heldErr     *fleet.Error
	canAccept   *fleet.Error
	acceptErr   *fleet.Error
	receipt     fleet.MachineListReceipt
	accepted    [][]byte
	acceptedBy  []string
	canAcceptN  int
	acceptCalls int
}

func (f *fakeHolder) Held() ([]byte, *fleet.Error) { return f.held, f.heldErr }
func (f *fakeHolder) CanAccept() *fleet.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canAcceptN++
	return f.canAccept
}
func (f *fakeHolder) Accept(raw []byte, by string) (fleet.MachineListReceipt, *fleet.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acceptCalls++
	f.accepted = append(f.accepted, append([]byte(nil), raw...))
	f.acceptedBy = append(f.acceptedBy, by)
	if f.acceptErr != nil {
		return fleet.MachineListReceipt{}, f.acceptErr
	}
	return f.receipt, nil
}

// countingBody is a request body that records how much of it was read.
type countingBody struct {
	r     io.Reader
	reads int
	bytes int
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads++
	n, err := c.r.Read(p)
	c.bytes += n
	return n, err
}
func (c *countingBody) Close() error { return nil }

// listMux is machine "owner" with a key principal "entry" and token principals
// whose grants the test chooses.
type listFixture struct {
	mux    *http.ServeMux
	holder *fakeHolder
	key    *keyPeer
}

func newListFixture(t *testing.T, holder *fakeHolder, principals ...Principal) *listFixture {
	t.Helper()
	svc := New("owner")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 500}); err != nil {
		t.Fatal(err)
	}
	k := newKeyPeer(t, "entry")
	cfg := Config{Principals: append([]Principal{
		{Name: "entry", PublicKey: k.pub, Grants: []Grant{GrantRead, GrantPushList}},
		{Name: "entry-nogrant", Token: "tok-nogrant", Grants: []Grant{GrantRead, GrantSend, GrantRelay}},
		{Name: "pusher", Token: "tok-push", Grants: []Grant{GrantPushList}},
		{Name: "reader", Token: "tok-read", Grants: []Grant{GrantRead}},
	}, principals...)}
	if holder != nil {
		cfg.MachineList = holder
	}
	return &listFixture{mux: NewMux(svc, cfg), holder: holder, key: k}
}

func (f *listFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec
}

func putList(token string, body io.Reader, contentLength int64) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/v1/machine-list", body)
	r.ContentLength = contentLength
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// signedPut builds a signed PUT whose digest is for signedBody, carrying the
// body actually sent (usually the same).
func (f *listFixture) signedPut(t *testing.T, signedBody, sentBody []byte, rd io.Reader) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/v1/machine-list", rd)
	if err := f.key.signer.Sign(r, signedBody, "owner"); err != nil {
		t.Fatal(err)
	}
	r.ContentLength = int64(len(sentBody))
	return r
}

func errOf(t *testing.T, rec *httptest.ResponseRecorder) fleet.Error {
	t.Helper()
	var env fleet.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not an error envelope (%d): %s", rec.Code, rec.Body.String())
	}
	return env.Error
}

func TestPushList_RefusedBeforeTheBodyIsRead(t *testing.T) {
	big := int64(machinelist.MaxBytes + 1)
	cases := []struct {
		name             string
		holder           *fakeHolder
		build            func(t *testing.T, f *listFixture, body *countingBody) *http.Request
		wantStatus       int
		wantReason       string
		noPrincipalTable bool
	}{
		{"bearer without the grant", &fakeHolder{}, func(t *testing.T, f *listFixture, b *countingBody) *http.Request {
			r := putList("tok-nogrant", b, 100)
			return r
		}, http.StatusUnauthorized, "", false},
		{"bearer with read only", &fakeHolder{}, func(t *testing.T, f *listFixture, b *countingBody) *http.Request {
			return putList("tok-read", b, 100)
		}, http.StatusUnauthorized, "", false},
		{"signed without the grant", &fakeHolder{}, func(t *testing.T, f *listFixture, b *countingBody) *http.Request {
			// The fixture's key principal holds push-list; rebuild without it.
			return nil
		}, http.StatusUnauthorized, "", false},
		{"the holder says the list is off", &fakeHolder{canAccept: machineListOff()}, func(t *testing.T, f *listFixture, b *countingBody) *http.Request {
			return putList("tok-push", b, 100)
		}, http.StatusNotImplemented, fleet.ReasonMachineListOff, false},
		{"the holder has no state directory", &fakeHolder{canAccept: &fleet.Error{Kind: fleet.ErrorUnsupported, Reason: fleet.ReasonMachineListNoStateDir, Message: "no state dir"}}, func(t *testing.T, f *listFixture, b *countingBody) *http.Request {
			return putList("tok-push", b, 100)
		}, http.StatusNotImplemented, fleet.ReasonMachineListNoStateDir, false},
		{"Content-Length over the cap", &fakeHolder{}, func(t *testing.T, f *listFixture, b *countingBody) *http.Request {
			return putList("tok-push", b, big)
		}, http.StatusBadRequest, fleet.ReasonMachineListTooLarge, false},
		{"no holder at all", nil, func(t *testing.T, f *listFixture, b *countingBody) *http.Request {
			return putList("tok-push", b, 100)
		}, http.StatusNotImplemented, fleet.ReasonMachineListOff, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newListFixture(t, tc.holder)
			body := &countingBody{r: strings.NewReader(strings.Repeat("x", 100))}
			r := tc.build(t, f, body)
			if r == nil { // the signed-without-the-grant case
				svc := New("owner")
				k := newKeyPeer(t, "entry")
				h := &fakeHolder{}
				mux := NewMux(svc, Config{MachineList: h, Principals: []Principal{{Name: "entry", PublicKey: k.pub, Grants: []Grant{GrantRead, GrantSend}}}})
				r = httptest.NewRequest(http.MethodPut, "/v1/machine-list", body)
				if err := k.signer.Sign(r, []byte(strings.Repeat("x", 100)), "owner"); err != nil {
					t.Fatal(err)
				}
				r.ContentLength = 100
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, r)
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.wantStatus)
				}
				if body.reads != 0 {
					t.Fatalf("the body was read %d time(s) (%d bytes) before the refusal", body.reads, body.bytes)
				}
				if h.acceptCalls != 0 || h.canAcceptN != 0 {
					t.Fatalf("the holder was consulted (CanAccept %d, Accept %d) for a caller without the grant", h.canAcceptN, h.acceptCalls)
				}
				return
			}
			rec := f.serve(r)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			if got := errOf(t, rec).Reason; got != tc.wantReason {
				t.Errorf("reason = %q, want %q", got, tc.wantReason)
			}
			if body.reads != 0 {
				t.Fatalf("the body was read %d time(s) (%d bytes) before the refusal", body.reads, body.bytes)
			}
			if tc.holder != nil && tc.holder.acceptCalls != 0 {
				t.Fatalf("Accept was called %d time(s) on a refused push", tc.holder.acceptCalls)
			}
			if tc.name == "bearer without the grant" || tc.name == "bearer with read only" {
				if msg := errOf(t, rec).Message; !strings.Contains(msg, "push-list") {
					t.Errorf("the refusal should name the grant, got %q", msg)
				}
				if tc.holder.canAcceptN != 0 {
					t.Errorf("the holder was asked CanAccept for a caller without the grant")
				}
			}
		})
	}
}

func TestPushList_NoPrincipalTableIsRefusedBeforeTheBody(t *testing.T) {
	svc := New("owner")
	h := &fakeHolder{}
	mux := NewMux(svc, Config{Token: "legacy", AllowLocalMutations: true, AllowPeerRelay: true, MachineList: h})
	body := &countingBody{r: strings.NewReader("x")}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, putList("legacy", body, 1))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d (%s), want 401: legacy mode must never allow a push", rec.Code, rec.Body.String())
	}
	if body.reads != 0 || h.acceptCalls != 0 {
		t.Fatalf("body reads %d, Accept calls %d", body.reads, h.acceptCalls)
	}
}

// A principal with every grant except push-list is still refused: nothing else
// implies it.
func TestPushList_IsItsOwnGrantNotImpliedByAnythingElse(t *testing.T) {
	var all []Grant
	for _, g := range Grants() {
		if g != GrantPushList {
			all = append(all, g)
		}
	}
	h := &fakeHolder{}
	f := newListFixture(t, h, Principal{Name: "everything", Token: "tok-all", Grants: all})
	rec := f.serve(putList("tok-all", strings.NewReader("x"), 1))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if msg := errOf(t, rec).Message; !strings.Contains(msg, "push-list") {
		t.Errorf("message %q does not name push-list", msg)
	}
	if h.acceptCalls != 0 {
		t.Fatal("Accept was called")
	}
}

func TestPushList_SignedBodyMustMatchTheSignedDigest(t *testing.T) {
	h := &fakeHolder{}
	f := newListFixture(t, h)
	r := f.signedPut(t, []byte("the list that was signed"), []byte("another list"), strings.NewReader("another list"))
	rec := f.serve(r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d (%s), want 401", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errOf(t, rec).Message, "digest") {
		t.Errorf("message = %q, want the digest refusal", errOf(t, rec).Message)
	}
	if h.acceptCalls != 0 {
		t.Fatal("Accept was called with a body the sender did not sign")
	}
}

func TestPushList_SignedCallerWithTheGrantIsAcceptedAndTheBodyArrivesIntact(t *testing.T) {
	h := &fakeHolder{receipt: fleet.MachineListReceipt{Fleet: "f", Seq: 7, PreviousSeq: 6, Mode: "only", Machines: 2}}
	f := newListFixture(t, h)
	body := []byte(`{"format":"muster-machine-list/1"}`)
	rec := f.serve(f.signedPut(t, body, body, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var got fleet.MachineListReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got != h.receipt {
		t.Fatalf("receipt = %+v (%v), want %+v", got, err, h.receipt)
	}
	if len(h.accepted) != 1 || !bytes.Equal(h.accepted[0], body) || h.acceptedBy[0] != "entry" {
		t.Fatalf("holder saw %q by %v", h.accepted, h.acceptedBy)
	}
}

func TestPushList_BodyOverTheCapIsRefused(t *testing.T) {
	h := &fakeHolder{}
	f := newListFixture(t, h)
	// A chunked body (no Content-Length) one byte over the cap.
	r := putList("tok-push", io.LimitReader(zeroReader{}, machinelist.MaxBytes+1), -1)
	rec := f.serve(r)
	if rec.Code != http.StatusBadRequest || errOf(t, rec).Reason != fleet.ReasonMachineListTooLarge {
		t.Fatalf("status = %d (%s), want 400 machine-list-too-large", rec.Code, rec.Body.String())
	}
	if h.acceptCalls != 0 {
		t.Fatal("Accept was called with an oversized body")
	}
	// Exactly at the cap is read in full.
	r = putList("tok-push", io.LimitReader(zeroReader{}, machinelist.MaxBytes), -1)
	if rec := f.serve(r); rec.Code != http.StatusOK {
		t.Fatalf("a body exactly at the cap: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(h.accepted) != 1 || len(h.accepted[0]) != machinelist.MaxBytes {
		t.Fatalf("holder saw %d bytes", len(h.accepted[0]))
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestPushList_RefusalPassesThroughWithItsKindAndReason(t *testing.T) {
	for _, tc := range []struct {
		err    *fleet.Error
		status int
	}{
		{&fleet.Error{Kind: fleet.ErrorConflict, Reason: fleet.ReasonMachineListNotNewer, Message: "this machine has accepted seq 9"}, http.StatusConflict},
		{&fleet.Error{Kind: fleet.ErrorInvalid, Reason: fleet.ReasonMachineListSignature, Message: "bad"}, http.StatusBadRequest},
		{&fleet.Error{Kind: fleet.ErrorUnsupported, Reason: fleet.ReasonMachineListNotWritable, Message: "no"}, http.StatusNotImplemented},
	} {
		h := &fakeHolder{acceptErr: tc.err}
		f := newListFixture(t, h)
		rec := f.serve(putList("tok-push", strings.NewReader("x"), 1))
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.err.Reason, rec.Code, tc.status)
		}
		if got := errOf(t, rec); got.Reason != tc.err.Reason || got.Kind != tc.err.Kind || got.Message != tc.err.Message {
			t.Errorf("error = %+v, want %+v", got, *tc.err)
		}
	}
}

func TestPushList_AuditNamesTheOutcomeNeverTheBody(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	h := &fakeHolder{receipt: fleet.MachineListReceipt{Seq: 2}}
	f := newListFixture(t, h)
	secret := "BODY-MARKER-" + strings.Repeat("z", 20)
	if rec := f.serve(putList("tok-push", strings.NewReader(secret), int64(len(secret)))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec := f.serve(putList("tok-read", strings.NewReader(secret), int64(len(secret)))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, `verb=push-list`) || !strings.Contains(out, `actor="pusher"`) || !strings.Contains(out, "outcome=performed") {
		t.Errorf("no audit line for the accepted push in:\n%s", out)
	}
	if !strings.Contains(out, "outcome=DENIED") || !strings.Contains(out, `actor="reader"`) {
		t.Errorf("no audit line for the denied push in:\n%s", out)
	}
	if strings.Contains(out, "BODY-MARKER") {
		t.Errorf("the body reached the log:\n%s", out)
	}
}

func TestGetMachineList_ServesTheHeldBytesUnderRead(t *testing.T) {
	held := []byte("{\"format\":\"muster-machine-list/1\",\"payload\":\"abc\"}\n")
	h := &fakeHolder{held: held}
	f := newListFixture(t, h)

	get := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/machine-list", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return f.serve(r)
	}
	if rec := get("tok-push"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a principal without read: status = %d, want 401", rec.Code)
	}
	rec := get("tok-read")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), held) {
		t.Fatalf("status = %d, body %q, want 200 and the held bytes", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	h.heldErr = &fleet.Error{Kind: fleet.ErrorNotFound, Reason: fleet.ReasonMachineListNone, Message: "none"}
	if rec := get("tok-read"); rec.Code != http.StatusNotFound || errOf(t, rec).Reason != fleet.ReasonMachineListNone {
		t.Errorf("nothing in force: status = %d (%s)", rec.Code, rec.Body.String())
	}

	nof := newListFixture(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/machine-list", nil)
	r.Header.Set("Authorization", "Bearer tok-read")
	if rec := nof.serve(r); rec.Code != http.StatusNotImplemented || errOf(t, rec).Reason != fleet.ReasonMachineListOff {
		t.Errorf("no holder: status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGrantForVerb_PutMachineListIsPushList(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/v1/machine-list", nil)
	if g := grantForVerb(r); g != GrantPushList {
		t.Fatalf("grant = %s, want push-list", g)
	}
	r = httptest.NewRequest(http.MethodGet, "/v1/machine-list", nil)
	if g := grantForVerb(r); g != GrantRead {
		t.Fatalf("GET grant = %s, want read", g)
	}
}

// Only the push route verifies a signed caller before reading its body. Every
// other route reads the body first, so a refusal there still costs the read the
// caller asked for; this pins that the deferral did not leak.
func TestOnlyThePushRouteDefersTheSignedBody(t *testing.T) {
	h := &fakeHolder{}
	f := newListFixture(t, h)
	body := &countingBody{r: strings.NewReader(`{"text":"hi"}`)}
	r := httptest.NewRequest(http.MethodPost, "/v1/machines/owner/sessions/s1/input", body)
	if err := f.key.signer.Sign(r, []byte(`{"text":"hi"}`), "owner"); err != nil {
		t.Fatal(err)
	}
	f.serve(r)
	if body.reads == 0 {
		t.Fatal("a signed request on an ordinary route must have its body read by the authenticator")
	}
}

func TestMachineListRoutesAreNeverRelayed(t *testing.T) {
	h := &fakeHolder{}
	f := newListFixture(t, h)
	for _, path := range []string{"/v1/machines/other/machine-list", "/v1/machine-list/other"} {
		r := httptest.NewRequest(http.MethodPut, path, strings.NewReader("x"))
		r.Header.Set("Authorization", "Bearer tok-push")
		if rec := f.serve(r); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want no such route", path, rec.Code)
		}
	}
	if h.acceptCalls != 0 {
		t.Fatal("a route with a target reached the holder")
	}
}
