package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/service"
)

// muster #510: GET /v1/health says how many sessions the last start left needing
// re-creation, and how many of those still wait.

// The issue's oracle, over the wire against the real driver: create sessions with
// caller env, restart the service, health reports N left; revive one, it reports
// N-1 still waiting; close another, it reports N-2. A session created without caller
// env is relaunched by the restart and is in neither count, and nothing on the
// response names a session or a value.
func TestHealth_ReportsTheSessionsARestartLeftAndHowManyStillWait(t *testing.T) {
	state := t.TempDir()
	env := map[string]string{"MODEL_KEY": reviveSecret}

	d1 := newStateDriver(t, state)
	a := mustCreate(t, d1, "k-a", fleet.SessionSpec{Name: "needs-env-a", Env: env})
	b := mustCreate(t, d1, "k-b", fleet.SessionSpec{Name: "needs-env-b", Env: env})
	plain := mustCreate(t, d1, "k-plain", fleet.SessionSpec{Name: "plain"})
	// Before any restart there is nothing a start left.
	if left, waiting := d1.NeedingRecreation(); left != 0 || waiting != 0 {
		t.Fatalf("NeedingRecreation on a driver that was never restarted = (%d, %d); want (0, 0)", left, waiting)
	}
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	if k, _ := d2.wasSeen(plain.ID); k.srv == nil {
		t.Fatalf("the session created without caller env was not relaunched: %+v", k)
	}

	do := serveFor(t, d2)
	health := func() (map[string]int, string) {
		t.Helper()
		code, raw := do("GET", "/v1/health", "")
		if code != http.StatusOK {
			t.Fatalf("health: %d %s", code, raw)
		}
		var body struct {
			NeedsRecreation map[string]map[string]int `json:"needsRecreation"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		return body.NeedsRecreation[string(DefaultRuntime)], raw
	}
	one := func(id string) string { return "/v1/machines/test-machine/sessions/" + id }

	// The restart left two.
	got, raw := health()
	if got["leftAtStart"] != 2 || got["stillWaiting"] != 2 || len(got) != 2 {
		t.Fatalf("after the restart: needsRecreation[%s] = %v; want leftAtStart 2, stillWaiting 2 and no other field\n%s", DefaultRuntime, got, raw)
	}
	for _, leak := range []string{a.ID, b.ID, plain.ID, "needs-env", "MODEL_KEY", reviveSecret} {
		if strings.Contains(raw, leak) {
			t.Errorf("health names %q; it is a count and nothing that identifies a session or a value", leak)
		}
	}

	// One revived: N-1 still waiting, the start's own figure unchanged.
	if code, body := do("POST", one(a.ID)+"/revive", `{"env":{"MODEL_KEY":"`+reviveSecret+`"}}`); code != http.StatusOK {
		t.Fatalf("revive: %d %s", code, body)
	}
	if got, raw := health(); got["leftAtStart"] != 2 || got["stillWaiting"] != 1 {
		t.Errorf("after one revive: %v; want leftAtStart 2, stillWaiting 1\n%s", got, raw)
	}

	// One closed instead: it is not waiting any longer either.
	if code, body := do("DELETE", one(b.ID), ""); code != http.StatusAccepted {
		t.Fatalf("close: %d %s; want 202", code, body)
	}
	// A close is accepted and carried out behind the answer, so the count is read until
	// it settles rather than once.
	deadline := time.Now().Add(10 * time.Second)
	got, raw = health()
	for got["stillWaiting"] != 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		got, raw = health()
	}
	if got["leftAtStart"] != 2 || got["stillWaiting"] != 0 {
		t.Errorf("after the other is closed: %v; want leftAtStart 2, stillWaiting 0 (still reported, so a monitor sees it go quiet)\n%s", got, raw)
	}
}

// The driver's own pair, without the wire: the figure the restart log line carries is
// the figure kept, and a revive and a close each take a session out of what waits.
func TestNeedingRecreation_FollowsReviveAndClose(t *testing.T) {
	state := t.TempDir()
	env := map[string]string{"MODEL_KEY": reviveSecret}
	d2, sess := leftUnknown(t, state, fleet.SessionSpec{Env: env}, nil)
	if left, waiting := d2.NeedingRecreation(); left != 1 || waiting != 1 {
		t.Fatalf("after the restart: (%d, %d); want (1, 1)", left, waiting)
	}
	if _, err := reviveWith(d2, sess, env); err != nil {
		t.Fatalf("Revive: %v", err)
	}
	if left, waiting := d2.NeedingRecreation(); left != 1 || waiting != 0 {
		t.Errorf("after the revive: (%d, %d); want (1, 0)", left, waiting)
	}

	// A session that is unknown for another cause is not counted: supplying the
	// environment would not bring it back, so it is not a session still waiting for it.
	d3 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d3.Shutdown() })
	if left, waiting := d3.NeedingRecreation(); left != 1 || waiting != 1 {
		t.Fatalf("after the second restart: (%d, %d); want (1, 1): the revive did not persist the environment, so it is left again", left, waiting)
	}
	d3.markSeen(sess.ID, knownSession{unknown: "its relaunch failed", recreate: recreateRelaunchFailed})
	if left, waiting := d3.NeedingRecreation(); left != 1 || waiting != 0 {
		t.Errorf("a session unknown for another cause: (%d, %d); want left 1 (what the start said) and waiting 0", left, waiting)
	}
}

// A start that left nothing says nothing: the key is absent from health, not an empty
// object and not a zero, so a machine that never had the problem answers as it always did.
func TestHealth_OmitsNeedsRecreationWhenTheStartLeftNothing(t *testing.T) {
	d := newStateDriver(t, t.TempDir())
	t.Cleanup(func() { _ = d.Shutdown() })
	code, raw := serveFor(t, d)("GET", "/v1/health", "")
	if code != http.StatusOK {
		t.Fatalf("health: %d %s", code, raw)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if v, ok := body["needsRecreation"]; ok {
		t.Errorf("needsRecreation = %s; want the key absent", v)
	}
}

// serveFor puts a real service in front of d and returns a caller for it, with the
// grants these tests use: read, create (a revive needs it) and close.
func serveFor(t *testing.T, d *Driver) func(method, path, body string) (int, string) {
	t.Helper()
	svc := service.New("test-machine")
	if err := svc.RegisterLocalDriver(DefaultRuntime, d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(service.NewMux(svc, service.Config{Principals: []service.Principal{
		{Name: "ops", Token: "tok-ops", Grants: []service.Grant{service.GrantRead, service.GrantCreate, service.GrantClose}},
	}}))
	t.Cleanup(srv.Close)
	return func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
}
