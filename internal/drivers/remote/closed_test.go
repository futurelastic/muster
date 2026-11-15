package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/service"
)

// closableDriver lists a fixed set of sessions and forgets one on Close —
// the least a peer needs to hold a closed-session record (muster #179).
type closableDriver struct {
	stub.Driver
	mu       sync.Mutex
	sessions map[string]fleet.Session
}

func (d *closableDriver) List(ctx context.Context, req fleet.Request, f driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	items := make([]fleet.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		items = append(items, s)
	}
	return fleet.NewCollection(items, []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

func (d *closableDriver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sessions[ref.ID]; ok {
		return s.State, nil
	}
	return fleet.SessionState{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
}

func (d *closableDriver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sessions, ref.ID)
	return fleet.Ack{Accepted: true}, nil
}

// A peer's closed sessions reach a fleet-scoped read through the remote
// driver, and the envelope is complete.
func TestFederatedClosedSessionsArriveFromThePeer(t *testing.T) {
	const token = "federation-token"
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := &closableDriver{Driver: stub.Driver{DeadlineMs: 2000}, sessions: map[string]fleet.Session{
		"work": {SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "work"}, Cwd: "/w", StartedAt: &started,
			State: fleet.ObservedState(fleet.StatusIdle, "fixture", nil)},
	}}
	base := peerService(t, "peerbox", "fake", d, token, true)
	rd := New("peerbox", base, WithDeadline(2*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)

	if _, err := rd.Close(context.Background(), fleetCaller(token), fleet.SessionRef{Machine: "peerbox", ID: "work"}); err != nil {
		t.Fatalf("closing on the peer: %v", err)
	}

	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete() {
		t.Errorf("both sources answered; want complete, sources=%+v", got.Sources())
	}
	if len(got.Items()) != 1 {
		t.Fatalf("want the peer's one closed session, got %+v", got.Items())
	}
	c := got.Items()[0]
	if c.Machine != "peerbox" || c.ID != "work" || c.ClosedBy != fleet.ClosedByClose {
		t.Errorf("wrong record: %+v", c)
	}

	// scope=local never reaches the peer (§13.1).
	local, _ := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeLocal, time.Time{}, 0)
	if len(local.Items()) != 0 || len(local.Sources()) != 1 {
		t.Errorf("a local read reached the peer: items=%+v sources=%+v", local.Items(), local.Sources())
	}
}

// A peer on a build that predates the route answers with a bare 404. That is
// a source that could not answer — never a peer with nothing closed.
func TestFederatedClosedSessionsFromAnOlderPeerAreNotEmpty(t *testing.T) {
	const token = "federation-token"
	old := httptest.NewServer(http.NewServeMux()) // every route: bare 404
	t.Cleanup(old.Close)
	rd := New("peerbox", old.URL, WithDeadline(2*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)

	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete() {
		t.Error("an older peer must not produce a complete envelope")
	}
	for _, src := range got.Sources() {
		if src.Machine == "peerbox" {
			if src.Status != fleet.SourceDegraded || src.Error == "" {
				t.Errorf("want a degraded source with a reason, got %+v", src)
			}
			return
		}
	}
	t.Fatalf("the peer contributed no SourceStatus; sources=%+v", got.Sources())
}

// usageClosableDriver is closableDriver that also keeps a figure for a session
// it removes, as a driver capable of it does (muster #485).
type usageClosableDriver struct {
	closableDriver
	kept []driver.CapturedUsage
}

func (d *usageClosableDriver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	d.mu.Lock()
	d.kept = append(d.kept, driver.CapturedUsage{ID: ref.ID, Usage: fleet.ClosedUsage{
		Input: 311, Output: 42, Provider: "p", Model: "m", Source: fleet.ConfidenceObserved,
		AsOf: time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC),
	}})
	d.mu.Unlock()
	return d.closableDriver.Close(ctx, req, ref)
}

func (d *usageClosableDriver) DrainFinalUsage() []driver.CapturedUsage {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.kept
	d.kept = nil
	return out
}

// The final figure a peer kept travels in its closed record: a fleet-scoped read
// returns it, with nothing in the remote driver that had to learn the field.
func TestFederatedClosedSessionsCarryThePeersFinalUsage(t *testing.T) {
	const token = "federation-token"
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := &usageClosableDriver{closableDriver: closableDriver{Driver: stub.Driver{DeadlineMs: 2000}, sessions: map[string]fleet.Session{
		"work": {SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "work"}, Cwd: "/w", StartedAt: &started,
			State: fleet.ObservedState(fleet.StatusIdle, "fixture", nil)},
	}}}
	base := peerService(t, "peerbox", "fake", d, token, true)
	rd := New("peerbox", base, WithDeadline(2*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)

	if _, err := rd.Close(context.Background(), fleetCaller(token), fleet.SessionRef{Machine: "peerbox", ID: "work"}); err != nil {
		t.Fatalf("closing on the peer: %v", err)
	}
	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items()) != 1 {
		t.Fatalf("want the peer's one closed session, got %+v", got.Items())
	}
	u := got.Items()[0].Usage
	if u == nil || u.Input != 311 || u.Output != 42 || u.Provider != "p" || u.Model != "m" ||
		!u.AsOf.Equal(time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC)) {
		t.Fatalf("usage = %+v, want the figure the peer kept", u)
	}
}
