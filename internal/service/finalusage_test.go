package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

// muster #485: the figure a driver kept as it removed a session lands in the
// closed-session record, whichever of the three ways the service learns the end.

// usageDriver is labelDriver plus the capability: it keeps a figure for a session
// it removes, and hands it over on the next drain.
type usageDriver struct {
	*labelDriver
	fmu     sync.Mutex
	pending []driver.CapturedUsage
	onClose map[string]fleet.ClosedUsage
}

func newUsageDriver() *usageDriver {
	return &usageDriver{labelDriver: newLabelDriver(), onClose: map[string]fleet.ClosedUsage{}}
}

func (d *usageDriver) keep(id string, u fleet.ClosedUsage) {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	d.pending = append(d.pending, driver.CapturedUsage{ID: id, Usage: u})
}

// Close removes the session, and keeps its figure the way a driver would as it does.
func (d *usageDriver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	d.fmu.Lock()
	if u, ok := d.onClose[ref.ID]; ok {
		d.pending = append(d.pending, driver.CapturedUsage{ID: ref.ID, Usage: u})
	}
	d.fmu.Unlock()
	return d.labelDriver.Close(ctx, req, ref)
}

func (d *usageDriver) DrainFinalUsage() []driver.CapturedUsage {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	out := d.pending
	d.pending = nil
	return out
}

func usageServer(t *testing.T, svc *Service, d driver.Driver) *httptest.Server {
	t.Helper()
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, tokenCfg()))
	t.Cleanup(srv.Close)
	return srv
}

func someUsage(input int64) fleet.ClosedUsage {
	cost := 0.5
	return fleet.ClosedUsage{
		Input: input, Output: 7, CacheRead: 11, CacheWrite: 13, Cost: &cost,
		Provider: "p", Model: "m", Source: fleet.ConfidenceObserved,
		AsOf: time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC),
	}
}

func wantUsage(t *testing.T, got *fleet.ClosedUsage, input int64) {
	t.Helper()
	if got == nil {
		t.Fatal("the closed record carries no usage")
	}
	if got.Input != input || got.Output != 7 || got.CacheRead != 11 || got.CacheWrite != 13 ||
		got.Cost == nil || *got.Cost != 0.5 || got.Provider != "p" || got.Model != "m" ||
		got.Source != fleet.ConfidenceObserved || !got.AsOf.Equal(someUsage(0).AsOf) {
		t.Errorf("usage = %+v, want the figures the driver kept (input %d)", got, input)
	}
}

func newSession(t *testing.T, srv *httptest.Server) fleet.Session {
	t.Helper()
	code, raw := create(t, srv, testToken, nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	return decodeSession(t, raw)
}

// The issue's oracle: a session closed through the service is returned with its
// final figures after its directory is gone — here, after the driver forgot
// everything — and a restart of the service does not lose them.
func TestClosedUsage_ACloseKeepsTheFiguresAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewWithState("test-machine", st)
	if err != nil {
		t.Fatal(err)
	}
	d := newUsageDriver()
	srv := usageServer(t, svc, d)
	sess := newSession(t, srv)
	d.onClose[sess.ID] = someUsage(311)

	closeSession(t, srv, sess.ID)

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 {
		t.Fatalf("want one closed session, got %s", body)
	}
	wantUsage(t, col.Items()[0].Usage, 311)
	for _, key := range []string{`"usage"`, `"input"`, `"cacheRead"`, `"cacheWrite"`, `"cost"`, `"provider"`, `"model"`, `"asOf"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("the wire form has no %s: %s", key, body)
		}
	}

	// Restart: a fresh service over the same state directory, a driver that
	// remembers nothing.
	st2, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc2, err := NewWithState("test-machine", st2)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := usageServer(t, svc2, newUsageDriver())
	col2, body2 := listClosed(t, srv2, "")
	if len(col2.Items()) != 1 {
		t.Fatalf("after a restart want one closed session, got %s", body2)
	}
	wantUsage(t, col2.Items()[0].Usage, 311)
}

// A session the runtime removed on its own is learned about by a complete
// listing; its figure, drained in the same pass, is on the absent record.
func TestClosedUsage_AnAbsentSessionCarriesWhatItsDriverKept(t *testing.T) {
	d := newUsageDriver()
	srv := usageServer(t, New("test-machine"), d)
	sess := newSession(t, srv)
	listLocal(t, srv, "")

	d.vanish(sess.ID)
	d.keep(sess.ID, someUsage(5))
	listLocal(t, srv, "")

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 || col.Items()[0].ClosedBy != fleet.ClosedByAbsent {
		t.Fatalf("want one absent record, got %s", body)
	}
	wantUsage(t, col.Items()[0].Usage, 5)
}

// An exit tombstone takes it the same way.
func TestClosedUsage_AnExitCarriesWhatItsDriverKept(t *testing.T) {
	d := newUsageDriver()
	srv := usageServer(t, New("test-machine"), d)
	sess := newSession(t, srv)
	listLocal(t, srv, "")

	d.exitVia(sess.ID, 3, "")
	d.keep(sess.ID, someUsage(9))
	listLocal(t, srv, "")

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 || col.Items()[0].ClosedBy != fleet.ClosedByExit {
		t.Fatalf("want one exit record, got %s", body)
	}
	wantUsage(t, col.Items()[0].Usage, 9)
}

// Everything drained at a close is kept, not only the closed session's: another
// session the runtime removed a moment before has its figure waiting, and its own
// tombstone comes with the next listing.
func TestClosedUsage_ACloseDoesNotDropAnotherSessionsFigure(t *testing.T) {
	d := newUsageDriver()
	srv := usageServer(t, New("test-machine"), d)
	a := newSession(t, srv)
	b := newSession(t, srv)
	listLocal(t, srv, "")

	d.vanish(b.ID)
	d.keep(b.ID, someUsage(22))
	d.onClose[a.ID] = someUsage(11)
	closeSession(t, srv, a.ID) // drains both
	listLocal(t, srv, "")      // learns b is gone

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 2 {
		t.Fatalf("want two closed sessions, got %s", body)
	}
	for _, c := range col.Items() {
		switch c.ID {
		case a.ID:
			wantUsage(t, c.Usage, 11)
		case b.ID:
			wantUsage(t, c.Usage, 22)
		default:
			t.Errorf("unexpected closed session %q", c.ID)
		}
	}
}

// A driver that keeps nothing leaves the record exactly as it was before this
// existed: no key, not an empty one.
func TestClosedUsage_ADriverWithoutTheCapabilityLeavesNoKey(t *testing.T) {
	d := newLabelDriver()
	srv := usageServer(t, New("test-machine"), struct{ driver.Driver }{d})
	sess := newSession(t, srv)
	closeSession(t, srv, sess.ID)

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 {
		t.Fatalf("want one closed session, got %s", body)
	}
	if col.Items()[0].Usage != nil || strings.Contains(string(body), `"usage"`) {
		t.Errorf("a record with no figure carries a usage key: %s", body)
	}
}

// A session that reported nothing is simply not in the drain: its record has no key.
func TestClosedUsage_ASessionWithNoFigureHasNoKey(t *testing.T) {
	d := newUsageDriver()
	srv := usageServer(t, New("test-machine"), d)
	sess := newSession(t, srv)
	closeSession(t, srv, sess.ID) // nothing in onClose

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 || col.Items()[0].Usage != nil || strings.Contains(string(body), `"usage"`) {
		t.Errorf("want one record with no usage key, got %s", body)
	}
}

// A figure whose tombstone never comes is not kept for ever.
func TestClosedUsage_AFigureNoTombstoneTakesIsDropped(t *testing.T) {
	h := newHistoryStore("m")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	h.stashFinal("rt", []driver.CapturedUsage{{ID: "old", Usage: someUsage(1)}})
	now = now.Add(pendingFinalTTL + time.Minute)
	h.stashFinal("rt", []driver.CapturedUsage{{ID: "new", Usage: someUsage(2)}})

	h.closedByRequest("rt", "old")
	h.closedByRequest("rt", "new")
	got := h.list(time.Time{})
	if len(got) != 2 {
		t.Fatalf("want two records, got %+v", got)
	}
	for _, c := range got {
		switch c.ID {
		case "old":
			if c.Usage != nil {
				t.Errorf("a figure older than %v was still attached: %+v", pendingFinalTTL, c.Usage)
			}
		case "new":
			wantUsage(t, c.Usage, 2)
		}
	}
	if len(h.pending) != 0 {
		t.Errorf("%d figures left waiting after both tombstones", len(h.pending))
	}
}
