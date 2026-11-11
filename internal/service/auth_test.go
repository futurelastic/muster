package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

func principalSrv(t *testing.T, ps []Principal) *httptest.Server {
	t.Helper()
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("otherbox", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: ps}))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// §6 requirement 3, finally expressible as written: opt-in PER VERB, PER
// PRINCIPAL. A shared token can express neither.
func TestGrantsAreCheckedPerVerbPerPrincipal(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
		{Name: "operator", Token: "tok-op", Grants: []Grant{GrantRead, GrantSend}},
		{Name: "destroyer", Token: "tok-kill", Grants: []Grant{GrantRead, GrantClose}},
	})
	denied := func(c int) bool { return c == 401 || c == 403 }

	// Reads for everyone who holds read.
	if denied(call(t, srv, http.MethodGet, "/v1/sessions", "tok-watch")) {
		t.Error("watcher holds read and was refused a read")
	}
	// The distinction a single mutate bit cannot make.
	if !denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-watch")) {
		t.Error("watcher has no send grant but was allowed to send")
	}
	if denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-op")) {
		t.Error("operator holds send and was refused")
	}
	if !denied(call(t, srv, http.MethodDelete, "/v1/machines/testbox/sessions/s1", "tok-op")) {
		t.Error("operator has no close grant but was allowed to destroy")
	}
	if denied(call(t, srv, http.MethodDelete, "/v1/machines/testbox/sessions/s1", "tok-kill")) {
		t.Error("destroyer holds close and was refused")
	}
}

// #80: the gap itself, and the test every other grant test in this file
// happened to leave unwritten by always handing the test principal `read`
// alongside whatever grant was under test. A principal scoped to a single
// mutating verb — never `read` — must not be able to list sessions,
// read a session's state, or subscribe to the event stream, on any peer or
// none: reads were reachable by any authenticated principal regardless of
// its grants until this was checked at all.
func TestReadRequiresItsOwnGrant(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "sender-only", Token: "tok-send", Grants: []Grant{GrantSend}},
		{Name: "no-grants", Token: "tok-none"},
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
	})
	denied := func(c int) bool { return c == 401 || c == 403 }

	reads := []struct{ method, path string }{
		{http.MethodGet, "/v1/health"},
		{http.MethodGet, "/v1/machines"},
		{http.MethodGet, "/v1/runtimes"},
		{http.MethodGet, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions/watch?wait=0"}, // wait=0 only arms the feed; never blocks
		{http.MethodGet, "/v1/machines/testbox/sessions/s1"},
		{http.MethodGet, "/v1/machines/testbox/sessions/s1/environment"},
		{http.MethodGet, "/v1/events"},
		// A fleet-scoped read reaching a peer must be refused on `read`
		// alone here too — this is deliberately NOT testing whether it
		// should ALSO need `relay` (muster #81, undecided on purpose).
		{http.MethodGet, "/v1/machines/otherbox/sessions/s1"},
	}
	for _, r := range reads {
		if !denied(call(t, srv, r.method, r.path, "tok-send")) {
			t.Errorf("%s %s: a principal holding send but not read was allowed to read", r.method, r.path)
		}
		if !denied(call(t, srv, r.method, r.path, "tok-none")) {
			t.Errorf("%s %s: a principal with no grants at all was allowed to read", r.method, r.path)
		}
		if denied(call(t, srv, r.method, r.path, "tok-watch")) {
			t.Errorf("%s %s: a principal holding read was refused", r.method, r.path)
		}
	}
}

// An unrecognised credential is unauthorized, and says nothing about which
// part was wrong.
func TestUnknownCredentialIsRefused(t *testing.T) {
	srv := principalSrv(t, []Principal{{Name: "a", Token: "good", Grants: []Grant{GrantRead}}})
	if code := call(t, srv, http.MethodGet, "/v1/sessions", "not-a-real-token"); code != 401 && code != 403 {
		t.Errorf("status = %d, want unauthorized", code)
	}
}

// D6's host/client split survives as a grant: relaying to a peer is a separate
// permission from mutating locally.
func TestRelayIsASeparateGrantFromLocalMutation(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "client-only", Token: "tok-c", Grants: []Grant{GrantRead, GrantRelay}},
		{Name: "host-only", Token: "tok-h", Grants: []Grant{GrantRead, GrantSend}},
	})
	denied := func(c int) bool { return c == 401 || c == 403 }

	// Hardened host, full client.
	if !denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-c")) {
		t.Error("client-only mutated a local session")
	}
	if denied(call(t, srv, http.MethodPost, "/v1/machines/otherbox/sessions/s1/input", "tok-c")) {
		t.Error("client-only holds relay but could not reach a peer")
	}
	// And the mirror.
	if denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-h")) {
		t.Error("host-only holds send and was refused locally")
	}
	if !denied(call(t, srv, http.MethodPost, "/v1/machines/otherbox/sessions/s1/input", "tok-h")) {
		t.Error("host-only has no relay grant but reached a peer")
	}
}

// §6 requirement 4 wants an actor. With a table, the actor is a name; a
// relayed request names who asked and who relayed, because an audit line
// saying only "the peer did it" cannot answer who asked the peer.
func TestCallerIsANameAndRelayNamesBothParties(t *testing.T) {
	p := Principal{Name: "peer-b", Token: "t", Grants: []Grant{GrantRead}}

	direct := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	if got := callerFor(p, direct).Principal; got != "peer-b" {
		t.Errorf("principal = %q, want the identity", got)
	}

	relayed := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	relayed.Header.Set(onBehalfOfHeader, "dashboard")
	got := callerFor(p, relayed).Principal
	if !strings.Contains(got, "dashboard") || !strings.Contains(got, "peer-b") {
		t.Errorf("principal = %q, want both the original asker and the relay", got)
	}
}

// Grants default to nothing. §6's default is denied, and an empty list must
// not read as "unrestricted" the way an empty filter does elsewhere.
func TestNoGrantsMeansNothingPermitted(t *testing.T) {
	p := Principal{Name: "mute", Token: "t"}
	for _, g := range []Grant{GrantRead, GrantSend, GrantCreate, GrantClose, GrantInterrupt, GrantRelay} {
		if p.Allows(g) {
			t.Errorf("a principal with no grants must not hold %q", g)
		}
	}
}

var _ = fleet.MachineId("")

// §6 requirement 4 wants the ACTOR, and for a relayed request the actor is
// whoever asked — not the machine that carried it. A live relayed mutation
// logged only the relay, because the audit path derived the name separately
// from the caller path.
func TestAuditActorMatchesTheCallerItLogs(t *testing.T) {
	p := Principal{Name: "relaybox", Token: "t", Grants: []Grant{GrantSend}}
	r := httptest.NewRequest(http.MethodPost, "/v1/machines/x/sessions/s1/input", nil)
	r.Header.Set(onBehalfOfHeader, "operator")

	actor := actorOf(p, r)
	if actor != callerFor(p, r).Principal {
		t.Fatalf("audit actor %q differs from the caller %q; two derivations of "+
			"one fact will drift", actor, callerFor(p, r).Principal)
	}
	if !strings.Contains(actor, "operator") {
		t.Errorf("actor = %q; an audit line naming only the relay cannot answer "+
			"who asked the peer", actor)
	}
}

// `trustCwd` is a keypress wearing a create's clothes.
//
// It asks the driver to answer a dialog on the caller's behalf, which is what
// `respond` does and what `send` grants. A principal holding only "start
// sessions" must not reach it — otherwise the create route quietly becomes a
// second, unaudited way to drive a session's dialogs, and nobody configuring
// grants would see it.
func TestTrustCwdNeedsSendOnTopOfCreate(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "spawner", Token: "tok-new", Grants: []Grant{GrantRead, GrantCreate}},
		{Name: "spawner", Token: "tok-new-consents", Grants: []Grant{GrantRead, GrantCreate}},
		{Name: "spawner", Token: "tok-new-consents-external-imports", Grants: []Grant{GrantRead, GrantCreate}},
		{Name: "spawner", Token: "tok-new-permissionMode", Grants: []Grant{GrantRead, GrantCreate}},
		{Name: "spawner", Token: "tok-new-mcpConfig", Grants: []Grant{GrantRead, GrantCreate}},
		{Name: "driver", Token: "tok-drive", Grants: []Grant{GrantRead, GrantCreate, GrantSend}},
	})
	create := func(token, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/v1/machines/testbox/sessions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "k-"+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	const plain = `{"runtime":"stub","cwd":"/w"}`
	const consenting = `{"runtime":"stub","cwd":"/w","trustCwd":true}`
	denied := func(c int) bool { return c == 401 || c == 403 }

	if denied(create("tok-new", plain)) {
		t.Error("a create grant no longer buys an ordinary create")
	}
	if !denied(create("tok-new", consenting)) {
		t.Error("a principal without send answered a dialog through the create route")
	}
	if denied(create("tok-drive", consenting)) {
		t.Error("a principal holding both grants was refused its own consent")
	}

	// Same bar for the other two things a create body can ask for beyond
	// starting a session, and for the same reason: one produces a keypress, the
	// other produces a session that acts without asking.
	//
	// mcpConfig joins them one step out: it names tool servers the session will
	// LAUNCH, so "may start a session" and "may start a session that also
	// starts these" are different authorities and only the second needs saying.
	for _, tc := range []struct{ name, body string }{
		{"consents", `{"runtime":"stub","cwd":"/w","consents":["folder-trust"]}`},
		// The second boot question (#211) takes the same grant: a consent to it
		// is a keypress on that question, whatever the question is.
		{"consents-external-imports", `{"runtime":"stub","cwd":"/w","consents":["external-imports"]}`},
		{"permissionMode", `{"runtime":"stub","cwd":"/w","permissionMode":"bypass"}`},
		{"mcpConfig", `{"runtime":"stub","cwd":"/w","mcpConfig":["/abs/servers.json"]}`},
		{"settings", `{"runtime":"stub","cwd":"/w","permissionMode":"bypass","settings":{"crossSessionInbound":"accept"}}`},
	} {
		if !denied(create("tok-new-"+tc.name, tc.body)) {
			t.Errorf("%s: a principal without send got it through the create route", tc.name)
		}
	}
}

// muster #441: record is a grant like any other as far as configuration goes, is
// defined once, and the one route that needs it asks for it by name.
func TestGrantRecord_IsDefinedOnceAndNamedByItsRoute(t *testing.T) {
	if !ValidGrant("record") || GrantRecord != "record" {
		t.Fatal("record is not a grant this build can configure")
	}
	n := 0
	for _, g := range Grants() {
		if g == GrantRecord {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Grants() holds record %d times, want once", n)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/machines/m/sessions/s1/record", nil)
	if g := grantForVerb(r); g != GrantRecord {
		t.Errorf("GET …/record needs %s, want record", g)
	}
	// A session that happens to be named "record" is a state read, not the route.
	r = httptest.NewRequest(http.MethodGet, "/v1/machines/m/sessions/record", nil)
	if g := grantForVerb(r); g != GrantRead {
		t.Errorf("GET …/sessions/record needs %s, want read", g)
	}
}
