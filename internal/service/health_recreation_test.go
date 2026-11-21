package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #510: GET /v1/health says how many sessions the last start left needing
// re-creation, and how many of those still wait. The count is the driver's; the
// service only carries it, per runtime, and says nothing when there is nothing to say.

// recreationStubDriver is stub.Driver plus a settable pair of counts: the smallest
// fake that exercises handleHealth's `needsRecreation` branch without standing up a
// runtime that restarts.
type recreationStubDriver struct {
	stub.Driver
	left, waiting atomic.Int64
}

func (d *recreationStubDriver) NeedingRecreation() (left, waiting int) {
	return int(d.left.Load()), int(d.waiting.Load())
}

var _ driver.RecreationReporter = (*recreationStubDriver)(nil)

// healthBody reads GET /v1/health as a raw map, so an absent key is distinguishable
// from a present empty one.
func healthRaw(t *testing.T, srv *httptest.Server) map[string]json.RawMessage {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+"/v1/health", nil))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: status %d", resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func recreationServer(t *testing.T, runtimes map[fleet.RuntimeId]driver.Driver) *httptest.Server {
	t.Helper()
	svc := New("test-machine")
	for rt, d := range runtimes {
		if err := svc.RegisterLocalDriver(rt, d); err != nil {
			t.Fatalf("RegisterLocalDriver(%s): %v", rt, err)
		}
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	return srv
}

// What the last start left is reported per runtime, and follows the driver as sessions
// are revived or closed: left stays, waiting falls.
func TestHealth_ReportsWhatTheLastStartLeftNeedingRecreation(t *testing.T) {
	d := &recreationStubDriver{Driver: stub.Driver{DeadlineMs: 200}}
	d.left.Store(3)
	d.waiting.Store(3)
	srv := recreationServer(t, map[fleet.RuntimeId]driver.Driver{"opencode": d})

	read := func() map[string]map[string]int {
		t.Helper()
		raw, ok := healthRaw(t, srv)["needsRecreation"]
		if !ok {
			t.Fatal("health has no needsRecreation although the last start left sessions waiting")
		}
		var got map[string]map[string]int
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("needsRecreation = %s: %v", raw, err)
		}
		return got
	}

	got := read()
	if want := (map[string]int{"leftAtStart": 3, "stillWaiting": 3}); len(got) != 1 || !mapsEqual(got["opencode"], want) {
		t.Fatalf("needsRecreation = %v; want only opencode: %v (a count per runtime and nothing else)", got, want)
	}

	// One revived: the start's figure stays, what still waits falls.
	d.waiting.Store(2)
	if want := (map[string]int{"leftAtStart": 3, "stillWaiting": 2}); !mapsEqual(read()["opencode"], want) {
		t.Errorf("after one revive: %v; want %v", read()["opencode"], want)
	}
	// All of them settled: still reported, so a monitor sees the entry go quiet rather
	// than vanish.
	d.waiting.Store(0)
	if want := (map[string]int{"leftAtStart": 3, "stillWaiting": 0}); !mapsEqual(read()["opencode"], want) {
		t.Errorf("after the last: %v; want %v", read()["opencode"], want)
	}
}

// A driver with nothing to count reports nothing, and a service with no such driver
// answers exactly as it did before: the key is absent, not an empty object and not a zero.
func TestHealth_OmitsNeedsRecreationWhenThereIsNothingToCount(t *testing.T) {
	quiet := &recreationStubDriver{Driver: stub.Driver{DeadlineMs: 200}} // implements it; its last start left none
	cases := map[string]map[fleet.RuntimeId]driver.Driver{
		"no driver implements it":             {"stub": &stub.Driver{DeadlineMs: 200}},
		"a driver whose last start left none": {"opencode": quiet},
	}
	for name, runtimes := range cases {
		t.Run(name, func(t *testing.T) {
			srv := recreationServer(t, runtimes)
			if raw, ok := healthRaw(t, srv)["needsRecreation"]; ok {
				t.Errorf("needsRecreation = %s; want the key absent", raw)
			}
		})
	}
}

// Only a runtime with something to report appears; a quiet one beside it does not.
func TestHealth_NeedsRecreationNamesOnlyTheRuntimesThatHaveSome(t *testing.T) {
	busy := &recreationStubDriver{Driver: stub.Driver{DeadlineMs: 200}}
	busy.left.Store(1)
	busy.waiting.Store(1)
	quiet := &recreationStubDriver{Driver: stub.Driver{DeadlineMs: 200}}
	srv := recreationServer(t, map[fleet.RuntimeId]driver.Driver{"opencode": busy, "other": quiet})

	var got map[string]map[string]int
	if err := json.Unmarshal(healthRaw(t, srv)["needsRecreation"], &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["other"]; ok || len(got) != 1 {
		t.Errorf("needsRecreation = %v; want only the runtime that left sessions waiting", got)
	}
}

// A figure that cannot be true (more waiting than the start left, or a negative one) is
// held to what can: a driver's bad day must not put an impossible number on health.
func TestHealth_NeedsRecreationNeverReportsMoreWaitingThanWereLeft(t *testing.T) {
	d := &recreationStubDriver{Driver: stub.Driver{DeadlineMs: 200}}
	d.left.Store(2)
	d.waiting.Store(5)
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("opencode", d); err != nil {
		t.Fatal(err)
	}
	if got := svc.recreationSnapshot()["opencode"]; got.LeftAtStart != 2 || got.StillWaiting != 2 {
		t.Errorf("snapshot = %+v; want waiting held at the 2 left", got)
	}
	d.waiting.Store(-1)
	if got := svc.recreationSnapshot()["opencode"]; got.StillWaiting != 0 {
		t.Errorf("snapshot = %+v; want a negative count read as 0", got)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
