package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// A principal pinned to this machine (muster #395). The tests put an
// unrestricted principal and a pinned one, with the same grants, in front of the
// same service, which has one local runtime and one peer — each holding a
// session — so every assertion is a contrast: the pinned principal sees what
// the other does not, and the other still sees everything it always did.

// machineDriver lists one session, stamped with the machine it belongs to, and
// counts how often it was asked to list or subscribe: a peer that was never
// asked proves the service never reached for it.
type machineDriver struct {
	stub.Driver
	machine fleet.MachineId
	lists   atomic.Int32
	subs    atomic.Int32
}

func (d *machineDriver) List(_ context.Context, _ fleet.Request, _ driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	d.lists.Add(1)
	started := time.Unix(1785600000, 0)
	s := fleet.Session{
		SessionRef: fleet.SessionRef{Machine: d.machine, ID: string(d.machine) + "-s1", Name: string(d.machine) + "-s1"},
		Runtime:    "fake",
		Cwd:        fleet.AbsolutePath("/work/" + string(d.machine)),
		StartedAt:  &started,
		State:      fleet.InferredState(fleet.StatusIdle, "test", nil),
	}
	return fleet.NewCollection([]fleet.Session{s},
		[]fleet.SourceStatus{{Machine: d.machine, Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

func (d *machineDriver) Subscribe(ctx context.Context, _ fleet.Request, _ driver.SubscribeFilter) (driver.EventStream, error) {
	d.subs.Add(1)
	return nil, driver.ErrUnsupported
}

// every grant a test principal might need, so a refusal can only be the pin.
func allGrants() []Grant { return Grants() }

const (
	fleetTok  = "tok-fleet"
	pinnedTok = "tok-pinned"
)

func localOnlyService(t *testing.T) (*Service, *machineDriver, *machineDriver, *httptest.Server) {
	t.Helper()
	svc := New("testbox")
	local := &machineDriver{Driver: stub.Driver{DeadlineMs: 500}, machine: "testbox"}
	peer := &machineDriver{Driver: stub.Driver{DeadlineMs: 500}, machine: "otherbox"}
	if err := svc.RegisterLocalDriver("fake", local); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("otherbox", peer); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: []Principal{
		{Name: "wide", Token: fleetTok, Grants: allGrants()},
		{Name: "pinned", Token: pinnedTok, Grants: allGrants(), Scope: ScopeLocal},
	}}))
	t.Cleanup(srv.Close)
	return svc, local, peer, srv
}

func send(t *testing.T, srv *httptest.Server, method, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "k")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

type listing struct {
	Items []struct {
		Machine string `json:"machine"`
	} `json:"items"`
	Sources []struct {
		Machine string `json:"machine"`
	} `json:"sources"`
}

func machinesOf(t *testing.T, srv *httptest.Server, path, token string) (items, sources map[string]bool) {
	t.Helper()
	code, body := send(t, srv, http.MethodGet, path, token)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	var l listing
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("GET %s: %v: %s", path, err, body)
	}
	items, sources = map[string]bool{}, map[string]bool{}
	for _, it := range l.Items {
		items[it.Machine] = true
	}
	for _, s := range l.Sources {
		sources[s.Machine] = true
	}
	return items, sources
}

func reasonOf(t *testing.T, body []byte) fleet.ErrorEnvelope {
	t.Helper()
	var env fleet.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an error envelope: %v: %s", err, body)
	}
	return env
}

// The measured case: a client that never sets scope. The default is fleet, and
// for the pinned principal the answer is this machine's alone — with the same
// request and the same grants, the unrestricted principal still sees the peer.
func TestLocalOnly_FleetScopedListingIsAnsweredLocally(t *testing.T) {
	for _, path := range []string{"/v1/sessions", "/v1/sessions?scope=fleet"} {
		_, _, peer, srv := localOnlyService(t)

		items, sources := machinesOf(t, srv, path, fleetTok)
		if !items["otherbox"] || !sources["otherbox"] {
			t.Fatalf("%s: the unrestricted principal lost the peer (items %v, sources %v)", path, items, sources)
		}
		before := peer.lists.Load()

		items, sources = machinesOf(t, srv, path, pinnedTok)
		if items["otherbox"] || sources["otherbox"] {
			t.Errorf("%s: the pinned principal saw the peer (items %v, sources %v)", path, items, sources)
		}
		if !items["testbox"] {
			t.Errorf("%s: the pinned principal lost its own machine's session: %v", path, items)
		}
		if got := peer.lists.Load(); got != before {
			t.Errorf("%s: the peer was asked to list %d time(s) on the pinned principal's behalf", path, got-before)
		}
	}
}

func TestLocalOnly_ClosedListingIsAnsweredLocally(t *testing.T) {
	_, _, _, srv := localOnlyService(t)
	// The peer's driver cannot list closed sessions, so a fleet-scoped answer
	// says so in its sources: the unrestricted principal reads a source for the
	// peer, the pinned one reads this machine alone.
	if _, sources := machinesOf(t, srv, "/v1/sessions/closed", fleetTok); !sources["otherbox"] {
		t.Fatalf("the unrestricted principal lost the peer's source: %v", sources)
	}
	for _, path := range []string{"/v1/sessions/closed", "/v1/sessions/closed?scope=fleet"} {
		_, sources := machinesOf(t, srv, path, pinnedTok)
		if len(sources) != 1 || !sources["testbox"] {
			t.Errorf("GET %s sources = %v, want this machine alone", path, sources)
		}
	}
}

// A request that names another machine is refused whatever the grants, in every
// shape a path can take, and refused the same for a machine that does not exist
// — so the refusal teaches nothing about which peers there are.
func TestLocalOnly_APathNamingAnotherMachineIsRefused(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/machines/otherbox/sessions/s1"},
		{http.MethodGet, "/v1/machines/otherbox/sessions/s1/turns"},
		{http.MethodGet, "/v1/machines/otherbox/sessions/s1/composer"},
		{http.MethodGet, "/v1/machines/otherbox/sessions/s1/environment"},
		{http.MethodGet, "/v1/machines/otherbox/input-audit"},
		{http.MethodPost, "/v1/machines/otherbox/sessions"},
		{http.MethodPost, "/v1/machines/otherbox/sessions/s1/input"},
		{http.MethodPost, "/v1/machines/otherbox/sessions/s1/interrupt"},
		{http.MethodPost, "/v1/machines/otherbox/sessions/s1/keys"},
		{http.MethodDelete, "/v1/machines/otherbox/sessions/s1"},
		{http.MethodGet, "/v1/machines/nowhere/sessions/s1"},
		{http.MethodDelete, "/v1/machines/nowhere/sessions/s1"},
		{http.MethodGet, "/v1/whoami?machine=otherbox"},
		{http.MethodGet, "/v1/machine-list"},
		{http.MethodPut, "/v1/machine-list"},
	}
	for _, c := range cases {
		_, _, peer, srv := localOnlyService(t)

		code, body := send(t, srv, c.method, c.path, pinnedTok)
		env := reasonOf(t, body)
		if code != http.StatusUnauthorized || env.Error.Kind != fleet.ErrorUnauthorized || env.Error.Reason != fleet.ReasonLocalOnly {
			t.Errorf("%s %s = %d kind=%q reason=%q, want 401 unauthorized local-only", c.method, c.path, code, env.Error.Kind, env.Error.Reason)
		}
		if peer.lists.Load() != 0 || peer.subs.Load() != 0 {
			t.Errorf("%s %s: the peer was reached", c.method, c.path)
		}

		// Same grants, no pin: not this refusal. (What it is instead — an
		// unsupported stub, an unknown machine — is no concern of this test.)
		if code, body := send(t, srv, c.method, c.path, fleetTok); code == http.StatusUnauthorized && reasonOf(t, body).Error.Reason == fleet.ReasonLocalOnly {
			t.Errorf("%s %s: the unrestricted principal was refused as local-only", c.method, c.path)
		}
	}
}

// Naming this machine is not naming another one.
func TestLocalOnly_ThisMachineStillWorks(t *testing.T) {
	_, _, _, srv := localOnlyService(t)
	for _, path := range []string{
		"/v1/machines/testbox/sessions/s1",
		"/v1/machines/testbox/sessions/s1/turns",
		"/v1/whoami?machine=testbox",
		"/v1/whoami",
		"/v1/health",
		"/v1/runtimes",
	} {
		code, body := send(t, srv, http.MethodGet, path, pinnedTok)
		if code == http.StatusUnauthorized {
			t.Errorf("GET %s refused a pinned principal on its own machine: %s", path, body)
		}
	}
}

func TestLocalOnly_MachinesListsOnlySelfAndDialsNoOne(t *testing.T) {
	_, _, peer, srv := localOnlyService(t)

	items, _ := machinesOf(t, srv, "/v1/machines", fleetTok)
	if !items["testbox"] || !items["otherbox"] {
		t.Fatalf("the unrestricted principal should see both machines, got %v", items)
	}
	before := peer.lists.Load()

	for _, path := range []string{"/v1/machines", "/v1/machines?verify=1"} {
		items, sources := machinesOf(t, srv, path, pinnedTok)
		if len(items) != 1 || !items["testbox"] {
			t.Errorf("GET %s items = %v, want this machine alone", path, items)
		}
		if len(sources) != 1 || !sources["testbox"] {
			t.Errorf("GET %s sources = %v, want this machine alone", path, sources)
		}
	}
	if got := peer.lists.Load(); got != before {
		t.Errorf("a peer was dialled %d time(s) for a pinned principal", got-before)
	}

	var col struct {
		Items []fleet.MachineInfo `json:"items"`
	}
	_, body := send(t, srv, http.MethodGet, "/v1/machines", pinnedTok)
	if err := json.Unmarshal(body, &col); err != nil || len(col.Items) != 1 || !col.Items[0].Self {
		t.Errorf("the one item is not the self item: %s (%v)", body, err)
	}
}

func TestLocalOnly_RuntimesNamesNoPeer(t *testing.T) {
	_, _, _, srv := localOnlyService(t)
	for tok, wantPeer := range map[string]bool{fleetTok: true, pinnedTok: false} {
		code, body := send(t, srv, http.MethodGet, "/v1/runtimes", tok)
		if code != http.StatusOK {
			t.Fatalf("GET /v1/runtimes = %d: %s", code, body)
		}
		if got := strings.Contains(string(body), "otherbox"); got != wantPeer {
			t.Errorf("token %s: /v1/runtimes mentions the peer = %v, want %v: %s", tok, got, wantPeer, body)
		}
	}
}

func TestLocalOnly_WhoAmIShowsTheRestriction(t *testing.T) {
	_, _, _, srv := localOnlyService(t)
	for tok, want := range map[string]string{fleetTok: "fleet", pinnedTok: "local"} {
		code, body := send(t, srv, http.MethodGet, "/v1/whoami", tok)
		var rep fleet.GrantReport
		if code != http.StatusOK || json.Unmarshal(body, &rep) != nil {
			t.Fatalf("whoami = %d: %s", code, body)
		}
		if rep.Scope != want {
			t.Errorf("token %s: scope = %q, want %q", tok, rep.Scope, want)
		}
	}
}

// Single-token mode has no principals and so no way to be pinned; its whoami
// says fleet, and nothing about it changed.
func TestLocalOnly_SingleTokenModeIsUnchanged(t *testing.T) {
	svc := New("testbox")
	srv := httptest.NewServer(NewMux(svc, Config{Token: "solo", AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	code, body := send(t, srv, http.MethodGet, "/v1/whoami", "solo")
	var rep fleet.GrantReport
	if code != http.StatusOK || json.Unmarshal(body, &rep) != nil || rep.Scope != "fleet" {
		t.Errorf("whoami in single-token mode = %d scope %q: %s", code, rep.Scope, body)
	}
}

// The event plane: a fleet subscriber makes the hub open streams to every peer,
// so the proof that a pinned stream is local is that the peer was never asked to
// stream, for the SSE feed and the long poll alike.
func TestLocalOnly_StreamsDoNotReachForPeers(t *testing.T) {
	for _, path := range []string{"/v1/events", "/v1/sessions/watch?wait=1"} {
		_, _, peer, srv := localOnlyService(t)

		resp := getWith(t, srv, path, fleetTok)
		waitFor(t, func() bool { return peer.subs.Load() > 0 }, "the unrestricted stream never reached for the peer on "+path)
		resp.Body.Close()
	}
	for _, path := range []string{"/v1/events", "/v1/sessions/watch?wait=1"} {
		_, _, peer, srv := localOnlyService(t)

		resp := getWith(t, srv, path, pinnedTok)
		time.Sleep(300 * time.Millisecond)
		if got := peer.subs.Load(); got != 0 {
			t.Errorf("%s: a pinned principal's stream made the service stream from the peer %d time(s)", path, got)
		}
		resp.Body.Close()
	}
}

// getWith opens a request and returns once the response head has arrived.
func getWith(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error(msg)
}

// An explicit request for scope=local from either principal means what it
// always meant, and a malformed scope is a 400 for the pinned principal too.
func TestLocalOnly_ScopeValidationIsNotWeakened(t *testing.T) {
	_, _, _, srv := localOnlyService(t)
	if code, _ := send(t, srv, http.MethodGet, "/v1/sessions?scope=galaxy", pinnedTok); code != http.StatusBadRequest {
		t.Errorf("a malformed scope = %d for a pinned principal, want 400", code)
	}
	items, _ := machinesOf(t, srv, "/v1/sessions?scope=local", pinnedTok)
	if items["otherbox"] || !items["testbox"] {
		t.Errorf("scope=local for a pinned principal = %v", items)
	}
}

// Scope "fleet" written out is the default said aloud.
func TestLocalOnly_ExplicitFleetScopeIsUnrestricted(t *testing.T) {
	if (Principal{Scope: ScopeFleet}).LocalOnly() || (Principal{}).LocalOnly() {
		t.Error("only scope local pins a principal")
	}
	if !(Principal{Scope: ScopeLocal}).LocalOnly() {
		t.Error("scope local does not pin")
	}
}
