package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// keySender is a driver that can press a key, so the route can be exercised
// without a multiplexer. It records what it was asked for, because the
// endpoint's job is to hand the driver an already-validated key and the
// caller's own digest — nothing more, and nothing less.
type keySender struct {
	stub.Driver
	gotKey    fleet.KeyName
	gotExpect string
	receipt   fleet.DeliveryReceipt
}

func (d *keySender) Keys(ctx context.Context, req fleet.Request, ref fleet.SessionRef, key fleet.KeyName, expect string) (fleet.DeliveryReceipt, error) {
	d.gotKey, d.gotExpect = key, expect
	return d.receipt, nil
}

func keyRequest(t *testing.T, srv *httptest.Server, token, body, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/machines/testbox/sessions/s1/keys"+query, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The endpoint forwards the key and the caller's own digest, and returns the
// driver's receipt as an ordinary 200 — a refusal included, as for input.
func TestKeys_ForwardsTheKeyAndTheCallersDigest(t *testing.T) {
	svc := New("testbox")
	d := &keySender{
		Driver:  stub.Driver{DeadlineMs: 1000},
		receipt: fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted, Reason: "sent Down"},
	}
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	defer srv.Close()

	resp := keyRequest(t, srv, testToken, `{"key":"Down"}`, "?expect=abc123")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if d.gotKey != fleet.KeyDown {
		t.Errorf("driver got key %q, want Down", d.gotKey)
	}
	if d.gotExpect != "abc123" {
		t.Errorf("driver got expect %q; the caller's own digest must reach the driver "+
			"unchanged, or the corroboration is against something the caller never saw", d.gotExpect)
	}
}

// The closed set is enforced before any driver is consulted, so a key name this
// API never defined cannot reach a substrate that might have its own opinion
// about what it means.
func TestKeys_RejectsAKeyOutsideTheVocabularyBeforeTheDriver(t *testing.T) {
	svc := New("testbox")
	d := &keySender{Driver: stub.Driver{DeadlineMs: 1000}}
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	defer srv.Close()

	for _, body := range []string{`{"key":"C-c"}`, `{"key":"a"}`, `{}`, `{"key":"Tab"}`, `{"key":"btab"}`} {
		resp := keyRequest(t, srv, testToken, body, "?expect=abc")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", body, resp.StatusCode)
		}
		env := decodeError(t, resp)
		resp.Body.Close()
		if !strings.Contains(env.Error.Message, "Enter") {
			t.Errorf("message = %q; it should name the vocabulary a caller may use", env.Error.Message)
		}
		if d.gotKey != "" {
			t.Fatalf("an invalid key reached the driver as %q", d.gotKey)
		}
	}
}

// #188 (ruled option A): BTab rides the EXISTING `keys` grant. It cycles the
// runtime's permission mode, so this is the test that pins the consequence the
// docs state — a principal holding `keys` and nothing else can send it, a
// principal holding `send` and every other grant but `keys` cannot — and that
// no separate grant was slipped in. If a later ruling gives mode changes a
// grant of their own, this test is the one that should fail first.
func TestKeys_BTabTravelsUnderTheKeysGrantAlone(t *testing.T) {
	svc := New("testbox")
	d := &keySender{
		Driver:  stub.Driver{DeadlineMs: 1000},
		receipt: fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted, Reason: "sent BTab"},
	}
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	// Computed from the one list of grants, so a grant added later is included
	// here automatically and this stays "every grant except keys".
	var allButKeys []Grant
	for _, g := range Grants() {
		if g != GrantKeys {
			allButKeys = append(allButKeys, g)
		}
	}
	srv := httptest.NewServer(NewMux(svc, Config{
		Token: testToken,
		Principals: []Principal{
			{Name: "keyer", Token: "tok-keys", Grants: []Grant{GrantRead, GrantKeys}},
			{Name: "everything-but-keys", Token: "tok-other", Grants: allButKeys},
		},
	}))
	defer srv.Close()

	resp := keyRequest(t, srv, "tok-other", `{"key":"BTab"}`, "?expect=abc")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a principal without `keys` reached BTab: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), string(GrantKeys)) {
		t.Errorf("refusal did not name the missing grant: %s", body)
	}
	if d.gotKey != "" {
		t.Fatalf("a refused BTab reached the driver as %q", d.gotKey)
	}

	resp = keyRequest(t, srv, "tok-keys", `{"key":"BTab"}`, "?expect=abc")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("the keys grant did not admit BTab: %d %s", resp.StatusCode, body)
	}
	if d.gotKey != fleet.KeyBTab {
		t.Errorf("driver got key %q, want BTab", d.gotKey)
	}
	if d.gotExpect != "abc" {
		t.Errorf("driver got expect %q; the caller's digest must reach it unchanged", d.gotExpect)
	}
}

// A driver that cannot press a key says so. Nothing here approximates one out
// of `input`, which would break input's own guarantee that a message never
// becomes a keystroke (§5.6).
func TestKeys_UnsupportedDriverIs501NotAnEmulation(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	defer srv.Close()

	resp := keyRequest(t, srv, testToken, `{"key":"Enter"}`, "?expect=abc")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// `keys` is its own grant. A principal permitted to answer recognised prompts
// must not thereby be permitted to press Enter on a screen nobody classified —
// the blast-radius argument that folds respond into send does not reach here.
func TestKeys_IsItsOwnGrantAndNotImpliedBySend(t *testing.T) {
	svc := New("testbox")
	d := &keySender{
		Driver:  stub.Driver{DeadlineMs: 1000},
		receipt: fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted},
	}
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{
		Token: testToken,
		Principals: []Principal{
			{Name: "driver-only", Token: "tok-send", Grants: []Grant{GrantRead, GrantSend}},
			{Name: "keyer", Token: "tok-keys", Grants: []Grant{GrantRead, GrantKeys}},
		},
	}))
	defer srv.Close()

	resp := keyRequest(t, srv, "tok-send", `{"key":"Enter"}`, "?expect=abc")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Errorf("send alone reached the keys endpoint: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), string(GrantKeys)) {
		t.Errorf("refusal did not name the missing grant: %s", body)
	}

	resp = keyRequest(t, srv, "tok-keys", `{"key":"Enter"}`, "?expect=abc")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("the keys grant did not admit its own endpoint: %d %s", resp.StatusCode, body)
	}
}

// muster #68: a fresh deployment refuses every keypress until an
// operator explicitly grants `keys` — expected, since every grant defaults
// to denied, but the refusal used to say only what was missing, not that
// missing it is the normal state of an unconfigured principal. Measured
// consequence: a caller reads `deliversRawKeys: true`, gets refused, and
// reasonably concludes the endpoint is not ready rather than that a setup
// step was skipped. The refusal now says so.
func TestKeys_RefusalSaysTheGrantIsDeniedByDefault(t *testing.T) {
	svc := New("testbox")
	d := &keySender{Driver: stub.Driver{DeadlineMs: 1000}}
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{
		Token: testToken,
		Principals: []Principal{
			{Name: "bare", Token: "tok-bare", Grants: []Grant{GrantRead}},
		},
	}))
	defer srv.Close()

	resp := keyRequest(t, srv, "tok-bare", `{"key":"Enter"}`, "?expect=abc")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "denied") || !strings.Contains(string(body), "not a bug") {
		t.Errorf("refusal did not say the grant defaults to denied and that this is "+
			"expected rather than a bug (#68): %s", body)
	}
}

// A refusal from the driver is a 200 carrying an outcome, exactly as for input
// and respond. Mapping it to 4xx would train a client to retry a keypress the
// driver deliberately declined to make.
func TestKeys_ARefusalIsNotAnHTTPError(t *testing.T) {
	svc := New("testbox")
	d := &keySender{
		Driver: stub.Driver{DeadlineMs: 1000},
		receipt: fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "the composer holds unsent text",
		},
	}
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	defer srv.Close()

	resp := keyRequest(t, srv, testToken, `{"key":"Enter"}`, "?expect=abc")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; a refusal is an outcome, not a fault", resp.StatusCode)
	}
	var receipt fleet.DeliveryReceipt
	if err := json.NewDecoder(resp.Body).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != fleet.OutcomeRefused {
		t.Errorf("outcome = %q, want refused", receipt.Outcome)
	}
}

// The grant set had three definitions and adding to one of them once made the
// service refuse to start on a config an operator had every reason to think was
// valid. Anything that enumerates grants must go through the one list.
func TestGrants_AreDefinedInExactlyOnePlace(t *testing.T) {
	for _, g := range Grants() {
		if !ValidGrant(g) {
			t.Errorf("%q is defined and cannot be granted", g)
		}
	}
	if ValidGrant(Grant("not-a-grant")) {
		t.Error("an unknown grant was accepted")
	}
	if !ValidGrant(GrantRemoteControl) {
		t.Error("the newest grant is the one most likely to be missing from a second list")
	}
}

var _ driver.KeySender = (*keySender)(nil)
