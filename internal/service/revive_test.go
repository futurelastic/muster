package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #427: POST …/revive. The route's own contract — body, grant, relay, the
// driver's refusals kept — with a driver that records what it was asked for. The
// end-to-end oracle (create, restart, revive, read back) is the opencode driver's
// own, in its package, because the runtime stand-in it needs is that package's test
// binary.

// reviveDriver is a driver with the optional capability, recording each call.
type reviveDriver struct {
	stub.Driver
	calls []driver.ReviveSpec
	refs  []fleet.SessionRef
	err   error
}

func (d *reviveDriver) Revive(ctx context.Context, req fleet.Request, ref fleet.SessionRef, spec driver.ReviveSpec) (fleet.Session, error) {
	d.calls = append(d.calls, spec)
	d.refs = append(d.refs, ref)
	if d.err != nil {
		return fleet.Session{}, d.err
	}
	started := time.Date(2026, 11, 21, 1, 2, 3, 0, time.UTC)
	return fleet.Session{
		SessionRef: ref, StartedAt: &started,
		State: fleet.ObservedState(fleet.StatusIdle, "revived", nil),
	}, nil
}

var _ driver.Reviver = (*reviveDriver)(nil)

func newReviveDriver() *reviveDriver { return &reviveDriver{Driver: stub.Driver{DeadlineMs: 1000}} }

func reviveRequest(t *testing.T, srv *httptest.Server, token, machine, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/"+machine+"/sessions/s1/revive", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func grantsWithout(skip ...Grant) []Grant {
	var out []Grant
next:
	for _, g := range Grants() {
		for _, s := range skip {
			if g == s {
				continue next
			}
		}
		out = append(out, g)
	}
	return out
}

func TestRevive_RouteReturnsTheSession(t *testing.T) {
	d := newReviveDriver()
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})

	code, body := reviveRequest(t, srv, testToken, "testbox", `{"env":{"MODEL_KEY":"v1"},"contextRef":"/work/brief.md"}`)
	if code != http.StatusOK {
		t.Fatalf("→ %d %s; want 200", code, body)
	}
	for _, want := range []string{`"id":"s1"`, `"status":"idle"`, `"startedAt"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s lacks %s", body, want)
		}
	}
	if len(d.calls) != 1 {
		t.Fatalf("driver saw %d calls; want 1", len(d.calls))
	}
	if d.calls[0].Env["MODEL_KEY"] != "v1" || d.calls[0].ContextRef != "/work/brief.md" {
		t.Errorf("driver saw %+v; want the env and contextRef from the body", d.calls[0])
	}
	if d.refs[0].ID != "s1" {
		t.Errorf("driver saw ref %+v", d.refs[0])
	}
	// No principal table (the legacy flag): mutating() has already decided, so the
	// bypass pairing does not refuse.
	if !d.calls[0].MayBypass {
		t.Error("MayBypass = false with no principal table")
	}
}

func TestRevive_ABodyThatIsNotARevive_IsRefusedBeforeTheDriver(t *testing.T) {
	d := newReviveDriver()
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})
	cases := map[string]string{
		"empty object":    `{}`,
		"empty env":       `{"env":{}}`,
		"null env":        `{"env":null}`,
		"not json":        `not json`,
		"unknown field":   `{"env":{"A":"b"},"name":"x"}`,
		"env as a string": `{"env":"A=b"}`,
	}
	for name, body := range cases {
		code, raw := reviveRequest(t, srv, testToken, "testbox", body)
		if code != http.StatusBadRequest {
			t.Errorf("%s → %d %s; want 400", name, code, raw)
		}
	}
	if len(d.calls) != 0 {
		t.Fatalf("an invalid body reached the driver: %v", d.calls)
	}
	_, raw := reviveRequest(t, srv, testToken, "testbox", `{"env":{"A":"b"},"name":"x"}`)
	if !strings.Contains(raw, `unknown field \"name\"`) {
		t.Errorf("the refusal does not name the field: %s", raw)
	}
}

// Revive is a create-grant act: send, close and every other verb do not imply it, and
// the refusal names the grant.
func TestRevive_NeedsTheCreateGrant(t *testing.T) {
	d := newReviveDriver()
	srv := rcServer(t, d, Config{Principals: []Principal{
		{Name: "no-create", Token: "tok-other", Grants: grantsWithout(GrantCreate)},
		{Name: "creator", Token: "tok-create", Grants: []Grant{GrantRead, GrantCreate}},
	}})

	code, raw := reviveRequest(t, srv, "tok-other", "testbox", `{"env":{"A":"b"}}`)
	if code != http.StatusUnauthorized || !strings.Contains(raw, "create grant") {
		t.Fatalf("without the grant → %d %s; want 401 naming the create grant", code, raw)
	}
	if len(d.calls) != 0 {
		t.Fatalf("a refused call reached the driver: %v", d.calls)
	}
	if code, raw := reviveRequest(t, srv, "tok-create", "testbox", `{"env":{"A":"b"}}`); code != http.StatusOK {
		t.Fatalf("with the grant → %d %s; want 200", code, raw)
	}
}

// A route that forgets to appear in grantForVerb is read as a read: this is the line
// that keeps a revive from being open to anyone who may list sessions.
func TestGrantForVerb_ReviveIsCreate(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/machines/m/sessions/s1/revive", nil)
	if g := grantForVerb(r); g != GrantCreate {
		t.Errorf("grantForVerb(POST …/revive) = %q; want %q", g, GrantCreate)
	}
	// And a read-only principal is refused over the wire.
	srv := rcServer(t, newReviveDriver(), Config{Principals: []Principal{{Name: "reader", Token: "tok-r", Grants: []Grant{GrantRead}}}})
	if code, _ := reviveRequest(t, srv, "tok-r", "testbox", `{"env":{"A":"b"}}`); code != http.StatusUnauthorized {
		t.Errorf("a read-only principal → %d; want 401", code)
	}
}

func TestRevive_DriverWithoutReviverIsUnsupported(t *testing.T) {
	srv := rcServer(t, &stub.Driver{DeadlineMs: 1000}, Config{Token: testToken, AllowLocalMutations: true})
	code, raw := reviveRequest(t, srv, testToken, "testbox", `{"env":{"A":"b"}}`)
	if code != http.StatusNotImplemented || !strings.Contains(raw, "unsupported") {
		t.Fatalf("→ %d %s; want 501 unsupported", code, raw)
	}
}

// A driver's refusal keeps its kind and its reason on the wire, so a client can tell
// "this session is not revivable" from "try again".
func TestRevive_AdoptsTheDriversTypedRefusals(t *testing.T) {
	d := newReviveDriver()
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})

	d.err = &fleet.Error{Kind: fleet.ErrorConflict, Reason: fleet.ReasonNotRevivable, Message: "revive: the session is running"}
	code, raw := reviveRequest(t, srv, testToken, "testbox", `{"env":{"A":"b"}}`)
	if code != http.StatusConflict || !strings.Contains(raw, `"reason":"not-revivable"`) || strings.Contains(raw, `"retryable":true`) {
		t.Fatalf("→ %d %s; want 409 with reason not-revivable, not retryable", code, raw)
	}

	d.err = &fleet.Error{Kind: fleet.ErrorUnreachable, Retryable: true, Message: "revive: the runtime could not be started"}
	code, raw = reviveRequest(t, srv, testToken, "testbox", `{"env":{"A":"b"}}`)
	if code != http.StatusGatewayTimeout || !strings.Contains(raw, `"retryable":true`) {
		t.Fatalf("→ %d %s; want 504 retryable", code, raw)
	}

	d.err = &fleet.Error{Kind: fleet.ErrorInvalid, Message: "revive: env must name exactly the variables the session was created with"}
	if code, _ := reviveRequest(t, srv, testToken, "testbox", `{"env":{"A":"b"}}`); code != http.StatusBadRequest {
		t.Fatalf("an invalid env → %d; want 400", code)
	}
}

// An environment naming a variable a delivery module reserves is refused as a create's
// would be, before the driver.
type reservingReviveDriver struct {
	reviveDriver
}

func (d *reservingReviveDriver) ReservedEnv() []string { return []string{"FLEET_RESERVED"} }

func TestRevive_ReservedEnvIsRefusedBeforeTheDriver(t *testing.T) {
	d := &reservingReviveDriver{reviveDriver: *newReviveDriver()}
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})
	code, raw := reviveRequest(t, srv, testToken, "testbox", `{"env":{"FLEET_RESERVED":"x"}}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "FLEET_RESERVED") {
		t.Fatalf("→ %d %s; want 400 naming the variable", code, raw)
	}
	if len(d.calls) != 0 {
		t.Fatal("the driver was asked")
	}
}

// A session created in the permission-bypass posture is started only for a caller who
// may send; the service says which, and the driver enforces it.
func TestRevive_MayBypassFollowsTheSendGrant(t *testing.T) {
	d := newReviveDriver()
	srv := rcServer(t, d, Config{Principals: []Principal{
		{Name: "create-only", Token: "tok-c", Grants: []Grant{GrantRead, GrantCreate}},
		{Name: "create-send", Token: "tok-cs", Grants: []Grant{GrantRead, GrantCreate, GrantSend}},
	}})
	reviveRequest(t, srv, "tok-c", "testbox", `{"env":{"A":"b"}}`)
	reviveRequest(t, srv, "tok-cs", "testbox", `{"env":{"A":"b"}}`)
	if len(d.calls) != 2 || d.calls[0].MayBypass || !d.calls[1].MayBypass {
		t.Fatalf("MayBypass per call = %+v; want false for create-only, true with send", d.calls)
	}
}

// A revive for a session on a peer needs the relay grant, as every relayed mutation
// does; the create grant is the PEER's to check against its own table.
func TestRevive_ATargetOnAPeerNeedsRelay(t *testing.T) {
	d := newReviveDriver()
	srv := rcServer(t, d, Config{Principals: []Principal{
		{Name: "creator", Token: "tok-c", Grants: []Grant{GrantRead, GrantCreate}},
	}})
	code, raw := reviveRequest(t, srv, "tok-c", "otherbox", `{"env":{"A":"b"}}`)
	if code != http.StatusUnauthorized || !strings.Contains(raw, "relay grant") {
		t.Fatalf("→ %d %s; want 401 naming the relay grant", code, raw)
	}
	if len(d.calls) != 0 {
		t.Fatal("a refused relay reached a driver")
	}
}

// The audit line and the answer carry no environment value: not on success, not on a
// refusal the driver words.
func TestRevive_NoValueReachesTheAuditLineOrTheAnswer(t *testing.T) {
	const secret = "sekrit-value-revive-427"
	logs := captureCredLog(t)
	d := newReviveDriver()
	srv := rcServer(t, d, Config{Principals: []Principal{
		{Name: "creator", Token: "tok-c", Grants: []Grant{GrantRead, GrantCreate}},
	}})

	var answers []string
	for _, body := range []string{
		`{"env":{"MODEL_KEY":"` + secret + `"}}`,
		`{"env":{"MODEL_KEY":"` + secret + `"},"name":"x"}`,
		`{"env":{"MODEL_KEY":"` + secret + `"},`,
	} {
		_, raw := reviveRequest(t, srv, "tok-c", "testbox", body)
		answers = append(answers, raw)
	}
	d.err = &fleet.Error{Kind: fleet.ErrorConflict, Reason: fleet.ReasonNotRevivable, Message: "revive: the session is running"}
	_, raw := reviveRequest(t, srv, "tok-c", "testbox", `{"env":{"MODEL_KEY":"`+secret+`"}}`)
	answers = append(answers, raw)

	for i, a := range answers {
		if strings.Contains(a, secret) {
			t.Errorf("answer %d carries the value: %s", i, a)
		}
	}
	line := logs()
	if strings.Contains(line, secret) {
		t.Errorf("the log carries the value:\n%s", line)
	}
	if !strings.Contains(line, "route=") || !strings.Contains(line, "revive") {
		t.Errorf("no audit line for the revive was written:\n%s", line)
	}
}
