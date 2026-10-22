package service

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/state"
)

// muster #350: a principal that authenticates by signature, beside one that
// authenticates by token, on the same machine.

type keyPeer struct {
	pub    ed25519.PublicKey
	path   string
	signer *peerkey.Signer
}

func newKeyPeer(t *testing.T, machine string) *keyPeer {
	t.Helper()
	pub, priv, err := peerkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "machine.key")
	if err := peerkey.SavePrivate(path, priv); err != nil {
		t.Fatal(err)
	}
	s, err := peerkey.NewSigner(path, machine)
	if err != nil {
		t.Fatal(err)
	}
	return &keyPeer{pub: pub, path: path, signer: s}
}

type signedFixture struct {
	srv  *httptest.Server
	svc  *Service
	key  *keyPeer
	self string
}

// signedOwner is machine "owner" with: a key principal "entry" (send +
// acting-person), a token principal "agent", and an auditor. withState gives it
// a state directory, which rotation needs.
func signedOwner(t *testing.T, withState bool) *signedFixture {
	t.Helper()
	var svc *Service
	if withState {
		st, err := state.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		svc, err = NewWithState("owner", st)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		svc = New("owner")
	}
	if err := svc.RegisterLocalDriver("stub", &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("entry", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	k := newKeyPeer(t, "entry")
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "entry", PublicKey: k.pub, Grants: []Grant{GrantRead, GrantSend, GrantActingPerson}},
		{Name: "agent", Token: "agent-token", Grants: []Grant{GrantRead, GrantSend}},
		{Name: "auditor", Token: "auditor-token", Grants: []Grant{GrantRead}},
	}}))
	t.Cleanup(srv.Close)
	return &signedFixture{srv: srv, svc: svc, key: k, self: "owner"}
}

func (f *signedFixture) do(t *testing.T, s *peerkey.Signer, method, path string, body any, target string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if err := s.Sign(req, raw, target); err != nil {
		t.Fatal(err)
	}
	return f.send(t, req)
}

func (f *signedFixture) send(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func errorMessage(b []byte) string {
	var env fleet.ErrorEnvelope
	_ = json.Unmarshal(b, &env)
	return env.Error.Message
}

const inputPath = "/v1/machines/owner/sessions/s1/input"

func TestSigned_AcceptedAndRecordedAsThePrincipal(t *testing.T) {
	f := signedOwner(t, false)
	resp, b := f.do(t, f.key.signer, "POST", inputPath, map[string]any{"text": "signed hello"}, "owner", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	_, page := getAudit(t, f.srv, "owner", "auditor-token", "sha256="+textSHA256("signed hello"))
	if len(page.Records) != 1 || page.Records[0].Principal != "entry" {
		t.Fatalf("records = %+v", page.Records)
	}
}

func TestSigned_ReplayedIsRefused(t *testing.T) {
	f := signedOwner(t, false)
	body, _ := json.Marshal(map[string]any{"text": "once"})
	req, _ := http.NewRequest("POST", f.srv.URL+inputPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if err := f.key.signer.Sign(req, body, "owner"); err != nil {
		t.Fatal(err)
	}
	clone := func() *http.Request {
		c := req.Clone(req.Context())
		c.Body = ioNopCloser(body)
		return c
	}
	if resp, b := f.send(t, clone()); resp.StatusCode != http.StatusOK {
		t.Fatalf("first: %d %s", resp.StatusCode, b)
	}
	resp, b := f.send(t, clone())
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(errorMessage(b), "already used") {
		t.Fatalf("replay: %d %s", resp.StatusCode, b)
	}
	_, page := getAudit(t, f.srv, "owner", "auditor-token", "sha256="+textSHA256("once"))
	if len(page.Records) != 1 {
		t.Fatalf("the replay was delivered: %+v", page.Records)
	}
}

func TestSigned_ExpiredAndFutureAreRefused(t *testing.T) {
	f := signedOwner(t, false)
	for name, at := range map[string]time.Time{
		"expired": time.Now().Add(-5 * time.Minute),
		"future":  time.Now().Add(5 * time.Minute),
	} {
		f.key.signer.SetClock(func() time.Time { return at })
		resp, b := f.do(t, f.key.signer, "POST", inputPath, map[string]any{"text": name}, "owner", nil)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(errorMessage(b), "clock") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	f.key.signer.SetClock(time.Now)
}

func TestSigned_WrongTargetIsRefused(t *testing.T) {
	f := signedOwner(t, false)
	resp, b := f.do(t, f.key.signer, "POST", inputPath, map[string]any{"text": "misaddressed"}, "someone-else", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a request addressed to another machine was accepted: %d %s", resp.StatusCode, b)
	}
}

func TestSigned_TamperedRequestIsRefused(t *testing.T) {
	f := signedOwner(t, false)
	build := func() (*http.Request, []byte) {
		body, _ := json.Marshal(map[string]any{"text": "genuine"})
		req, _ := http.NewRequest("POST", f.srv.URL+inputPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if err := f.key.signer.Sign(req, body, "owner"); err != nil {
			t.Fatal(err)
		}
		return req, body
	}

	req, _ := build()
	req.Body = ioNopCloser([]byte(`{"text":"altered"}`))
	req.ContentLength = int64(len(`{"text":"altered"}`))
	if resp, _ := f.send(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("altered body: %d", resp.StatusCode)
	}

	req, body := build()
	req.Body = ioNopCloser(body)
	req.URL.Path = "/v1/machines/owner/sessions/other/input"
	if resp, _ := f.send(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("altered path: %d", resp.StatusCode)
	}

	req, body = build()
	req.Body = ioNopCloser(body)
	req.Header.Set("Fleet-Human-Relay", "1")
	req.Header.Set("Fleet-On-Behalf-Of", "forged")
	if resp, _ := f.send(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("injected relay assertions: %d", resp.StatusCode)
	}
}

func TestSigned_UnknownSenderAndForgeryReadTheSame(t *testing.T) {
	f := signedOwner(t, false)
	stranger := newKeyPeer(t, "stranger")
	forger := newKeyPeer(t, "entry") // claims to be a listed machine, holds another key
	_, b1 := f.do(t, stranger.signer, "GET", "/v1/health", nil, "owner", nil)
	_, b2 := f.do(t, forger.signer, "GET", "/v1/health", nil, "owner", nil)
	if errorMessage(b1) != errorMessage(b2) || errorMessage(b1) == "" {
		t.Errorf("a stranger and a forger must not be distinguishable: %q vs %q", errorMessage(b1), errorMessage(b2))
	}
}

// A principal with a key cannot be reached with a token, and a token peer is
// untouched by any of this.
func TestSigned_TokenPeerStillWorksBesideAKeyPeer(t *testing.T) {
	f := signedOwner(t, false)
	if resp := postInput(t, f.srv, "agent-token", nil, map[string]any{"text": "by token"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("token peer: %d", resp.StatusCode)
	}
	if resp, b := f.do(t, f.key.signer, "POST", inputPath, map[string]any{"text": "by key"}, "owner", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("key peer: %d %s", resp.StatusCode, b)
	}
	// "entry" has no token: presenting its name or anything as a bearer fails.
	if resp := postInput(t, f.srv, "", nil, map[string]any{"text": "no credential"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty bearer: %d", resp.StatusCode)
	}
}

func TestSigned_GrantsStillApply(t *testing.T) {
	f := signedOwner(t, false)
	resp, b := f.do(t, f.key.signer, "DELETE", "/v1/machines/owner/sessions/s1", nil, "owner", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(errorMessage(b), "close") {
		t.Fatalf("a key peer without close must be refused close: %d %s", resp.StatusCode, b)
	}
}

// A signed request carries an acting person under the same trust rule.
func TestSigned_ActingPersonFromAGrantedKeyPeer(t *testing.T) {
	f := signedOwner(t, false)
	resp, b := f.do(t, f.key.signer, "POST", inputPath, map[string]any{"text": "with person"}, "owner",
		map[string]string{onBehalfOfHeader: "webui", actingPersonHeader: "u-1", actingIssuerHeader: "idp"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	_, page := getAudit(t, f.srv, "owner", "auditor-token", "sha256="+textSHA256("with person"))
	if len(page.Records) != 1 || page.Records[0].Person != "u-1@idp" || page.Records[0].Principal != "webui via entry" {
		t.Fatalf("records = %+v", page.Records)
	}
}

// ---- rotation ----

func rotateTo(t *testing.T, f *signedFixture, s *peerkey.Signer, next ed25519.PublicKey) (*http.Response, []byte) {
	t.Helper()
	return f.do(t, s, "POST", "/v1/peer-key", map[string]string{"publicKey": peerkey.FormatPublicKey(next)}, "owner", nil)
}

func TestSigned_RotationKeepsThePeerAndRetiresTheOldKeyAfterGrace(t *testing.T) {
	f := signedOwner(t, true)
	oldSigner := f.key.signer

	newPub, newPriv, _ := peerkey.Generate()
	if resp, b := rotateTo(t, f, oldSigner, newPub); resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d %s", resp.StatusCode, b)
	}
	newPath := filepath.Join(t.TempDir(), "new.key")
	if err := peerkey.SavePrivate(newPath, newPriv); err != nil {
		t.Fatal(err)
	}
	newSigner, _ := peerkey.NewSigner(newPath, "entry")

	// Both keys work during the grace period: the peer is not dropped.
	if resp, b := f.do(t, newSigner, "POST", inputPath, map[string]any{"text": "new key"}, "owner", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("new key: %d %s", resp.StatusCode, b)
	}
	if resp, b := f.do(t, oldSigner, "POST", inputPath, map[string]any{"text": "old key in grace"}, "owner", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("old key in grace: %d %s", resp.StatusCode, b)
	}

	// After the grace period only the new one does.
	f.svc.peerKeys.mu.Lock()
	rec := f.svc.peerKeys.recs["entry"]
	rec.PreviousUntil = time.Now().Add(-time.Second)
	f.svc.peerKeys.recs["entry"] = rec
	f.svc.peerKeys.mu.Unlock()
	if resp, _ := f.do(t, oldSigner, "POST", inputPath, map[string]any{"text": "old key late"}, "owner", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a retired key past its grace was accepted: %d", resp.StatusCode)
	}
	if resp, b := f.do(t, newSigner, "POST", inputPath, map[string]any{"text": "new key later"}, "owner", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("new key later: %d %s", resp.StatusCode, b)
	}
}

// The rotation survives a restart: the state file is the only copy.
func TestSigned_RotationIsDurable(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	svc, err := NewWithState("owner", st)
	if err != nil {
		t.Fatal(err)
	}
	cfgKey, _, _ := peerkey.Generate()
	next, _, _ := peerkey.Generate()
	if _, err := svc.peerKeys.rotate("entry", cfgKey, next, time.Now()); err != nil {
		t.Fatal(err)
	}
	st2, _ := state.Open(dir)
	svc2, err := NewWithState("owner", st2)
	if err != nil {
		t.Fatal(err)
	}
	got := svc2.peerKeys.keys("entry", cfgKey, time.Now())
	if len(got) != 2 || !got[0].Equal(next) || !got[1].Equal(cfgKey) {
		t.Fatalf("keys after restart = %v", got)
	}
	// An operator who changes the configured key by hand wins over a stale rotation.
	other, _, _ := peerkey.Generate()
	if got := svc2.peerKeys.keys("entry", other, time.Now()); len(got) != 1 || !got[0].Equal(other) {
		t.Fatalf("a stale rotation overrode the configuration: %v", got)
	}
}

func TestSigned_RetiredKeyCannotRotate(t *testing.T) {
	f := signedOwner(t, true)
	oldSigner := f.key.signer
	second, _, _ := peerkey.Generate()
	if resp, b := rotateTo(t, f, oldSigner, second); resp.StatusCode != http.StatusOK {
		t.Fatalf("first rotation: %d %s", resp.StatusCode, b)
	}
	// The old key is now retired-in-grace. A thief holding it must not be able
	// to take the peer over by rotating to a key of their own.
	thief, _, _ := peerkey.Generate()
	resp, b := rotateTo(t, f, oldSigner, thief)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(errorMessage(b), "retired") {
		t.Fatalf("retired key rotated: %d %s", resp.StatusCode, b)
	}
	// Repeating the rotation that already happened is harmless (a re-run).
	if resp, b := rotateTo(t, f, oldSigner, second); resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent repeat: %d %s", resp.StatusCode, b)
	}
}

func TestSigned_RotationNeedsAStateDirAndASignature(t *testing.T) {
	f := signedOwner(t, false)
	next, _, _ := peerkey.Generate()
	resp, b := rotateTo(t, f, f.key.signer, next)
	if resp.StatusCode == http.StatusOK || !strings.Contains(errorMessage(b), "state directory") {
		t.Fatalf("an in-memory rotation would lock the peer out at restart: %d %s", resp.StatusCode, b)
	}

	// A token cannot rotate a signing key.
	g := signedOwner(t, true)
	body, _ := json.Marshal(map[string]string{"publicKey": peerkey.FormatPublicKey(next)})
	req, _ := http.NewRequest("POST", g.srv.URL+"/v1/peer-key", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-token")
	if resp, _ := g.send(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bearer token rotated a key: %d", resp.StatusCode)
	}
}

func ioNopCloser(b []byte) *nopCloser { return &nopCloser{Reader: bytes.NewReader(b)} }

type nopCloser struct{ *bytes.Reader }

func (*nopCloser) Close() error { return nil }
