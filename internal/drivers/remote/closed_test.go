package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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

	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet, driver.ClosedQuery{}, 0)
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
	local, _ := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeLocal, driver.ClosedQuery{}, 0)
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

	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet, driver.ClosedQuery{}, 0)
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
	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet, driver.ClosedQuery{}, 0)
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

// recordingClosedPeer serves GET /v1/sessions/closed from a fixed list, newest
// first, and remembers every query it was sent. With honor set it applies
// before and limit as a current build does; without, it answers with
// everything it has, as a build that predates them does (muster #482).
type recordingClosedPeer struct {
	mu      sync.Mutex
	queries []url.Values
}

func (p *recordingClosedPeer) serve(t *testing.T, rows []fleet.ClosedSession, honor bool) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/closed", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.mu.Lock()
		p.queries = append(p.queries, q)
		p.mu.Unlock()
		out := rows
		if honor {
			if raw := q.Get("before"); raw != "" {
				b, err := time.Parse(time.RFC3339Nano, raw)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				var kept []fleet.ClosedSession
				for _, c := range out {
					if c.ClosedAt.Before(b) {
						kept = append(kept, c)
					}
				}
				out = kept
			}
			if raw := q.Get("limit"); raw != "" {
				n, _ := strconv.Atoi(raw)
				if n < len(out) {
					out = out[:n]
				}
			}
		}
		col, err := fleet.NewCollection(out, []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}})
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(col)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func peerClosedRows() []fleet.ClosedSession {
	base := time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)
	row := func(id string, sec int) fleet.ClosedSession {
		return fleet.ClosedSession{
			SessionRef: fleet.SessionRef{Machine: "peerbox", ID: id},
			ClosedAt:   base.Add(time.Duration(sec) * time.Second), ClosedBy: fleet.ClosedByClose,
		}
	}
	return []fleet.ClosedSession{row("p5", 50), row("p4", 40), row("p3", 30), row("p2", 20), row("p1", 10)}
}

func closedIDs(col fleet.Collection[fleet.ClosedSession]) string {
	var ids []string
	for _, c := range col.Items() {
		ids = append(ids, c.ID)
	}
	return strings.Join(ids, ",")
}

// limit and before reach the peer, and before keeps its nanoseconds: the cursor
// is the closedAt of a row as the API printed it, and a second-resolution copy
// would name an earlier instant and skip the rows between.
func TestFederatedClosedSessionsAskThePeerForOnePage(t *testing.T) {
	const token = "federation-token"
	rows := peerClosedRows()
	var peer recordingClosedPeer
	rd := New("peerbox", peer.serve(t, rows, true), WithDeadline(2*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)

	before := rows[1].ClosedAt // p4: strictly older than it means p3, p2, p1
	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet,
		driver.ClosedQuery{Before: before, Limit: 2}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if closedIDs(got) != "p3,p2" {
		t.Errorf("page = %q, want p3,p2", closedIDs(got))
	}
	if !got.Complete() {
		t.Errorf("both sources answered; want complete, sources=%+v", got.Sources())
	}
	if len(peer.queries) != 1 {
		t.Fatalf("the peer was asked %d times", len(peer.queries))
	}
	q := peer.queries[0]
	if q.Get("scope") != "local" || q.Get("limit") != "2" {
		t.Errorf("peer query %v: want scope=local and limit=2", q)
	}
	if want := before.UTC().Format(time.RFC3339Nano); q.Get("before") != want {
		t.Errorf("peer was sent before=%q, want %q (nanoseconds kept)", q.Get("before"), want)
	}

	// A read naming neither asks for neither.
	peer.queries = nil
	if _, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet, driver.ClosedQuery{}, 0); err != nil {
		t.Fatal(err)
	}
	if q := peer.queries[0]; q.Has("limit") || q.Has("before") {
		t.Errorf("an unbounded read sent %v", q)
	}
}

// A peer on a build that predates the parameters ignores them and answers with
// everything. The service cuts what comes back, so the caller gets the same
// page either way; only the peer's read is longer.
func TestFederatedClosedSessionsFromAPeerThatIgnoresTheBound(t *testing.T) {
	const token = "federation-token"
	rows := peerClosedRows()
	var peer recordingClosedPeer
	rd := New("peerbox", peer.serve(t, rows, false), WithDeadline(2*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)

	got, err := svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet,
		driver.ClosedQuery{Limit: 2}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if closedIDs(got) != "p5,p4" {
		t.Errorf("first page = %q, want p5,p4 (cut here though the peer sent all five)", closedIDs(got))
	}
	got, err = svcA.ListClosedSessions(context.Background(), fleetCaller(token), service.ScopeFleet,
		driver.ClosedQuery{Before: rows[1].ClosedAt, Limit: 2}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if closedIDs(got) != "p3,p2" {
		t.Errorf("second page = %q, want p3,p2", closedIDs(got))
	}
	if !got.Complete() {
		t.Errorf("an old peer that answers is still ok; sources=%+v", got.Sources())
	}
}
