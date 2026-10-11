package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/service"
)

// Tests for muster #237: a peer that has stopped answering is remembered,
// so a read stops waiting out the full bound on it.

// gatedPeer answers 200 health/list bodies while up is true and hangs while it
// is false, counting every request that reaches it.
type gatedPeer struct {
	srv  *httptest.Server
	up   atomic.Bool
	hits atomic.Int64
	// healthy makes /v1/health answer at once whatever up says: a peer that is
	// alive and merely slow on the routes a caller is waiting for (#302).
	healthy atomic.Bool
	// health counts /v1/health requests, concurrent and peak concurrent.
	health, healthNow, healthPeak atomic.Int64
}

func newGatedPeer(t *testing.T) *gatedPeer {
	t.Helper()
	g := &gatedPeer{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits.Add(1)
		// Only the routes these tests exercise hang; the capability probe an
		// answered call triggers in the background gets a fast 404, so it
		// cannot land a failure of its own in a streak under test.
		if !strings.HasPrefix(r.URL.Path, "/v1/sessions") && r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/v1/health" {
			g.health.Add(1)
			now := g.healthNow.Add(1)
			defer g.healthNow.Add(-1)
			for {
				peak := g.healthPeak.Load()
				if now <= peak || g.healthPeak.CompareAndSwap(peak, now) {
					break
				}
			}
			if g.healthy.Load() {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		if !g.up.Load() {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"sources":[{"machine":"sleepy","status":"ok","observedAt":"2026-09-30T00:00:00Z"}],"complete":true}`))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func newSleepy(t *testing.T, g *gatedPeer, k int, min, max time.Duration, opts ...Option) *Driver {
	t.Helper()
	opts = append([]Option{WithDeadline(150 * time.Millisecond), WithDownPolicy(k, min, max)}, opts...)
	d := New("sleepy", g.srv.URL, opts...)
	t.Cleanup(d.Shutdown)
	return d
}

func listOnce(t *testing.T, d *Driver) (fleet.SourceStatus, time.Duration) {
	t.Helper()
	started := time.Now()
	col, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("a failed read degrades the envelope, it does not fail the call: %v", err)
	}
	if len(col.Sources()) != 1 {
		t.Fatalf("want one source, got %+v", col.Sources())
	}
	return col.Sources()[0], time.Since(started)
}

// The oracle from the issue: a peer that never answers costs K bounded
// waits, and after that a read returns at once with that peer unreachable and
// the moment it went down named.
func TestAPeerThatNeverAnswersIsMarkedDownAndReadsStopWaiting(t *testing.T) {
	g := newGatedPeer(t)
	// Long backoff so no probe interferes with the fast-fail assertions.
	d := newSleepy(t, g, 3, time.Hour, time.Hour)

	for i := 0; i < 3; i++ {
		if src, _ := listOnce(t, d); src.Status != fleet.SourceUnreachable {
			t.Fatalf("call %d: %+v", i, src)
		}
	}
	down, since, failures := d.PeerDown()
	if !down || failures != 3 || since.IsZero() {
		t.Fatalf("after 3 failures: down=%v since=%v failures=%d", down, since, failures)
	}

	before := g.hits.Load()
	src, took := listOnce(t, d)
	if took > 50*time.Millisecond {
		t.Errorf("a read on a marked-down peer took %s; want an immediate answer", took)
	}
	if src.Status != fleet.SourceUnreachable {
		t.Errorf("status = %q, want unreachable", src.Status)
	}
	if !strings.Contains(src.Error, "marked down") || !strings.Contains(src.Error, since.UTC().Format(time.RFC3339)) {
		t.Errorf("error must say the peer is marked down and since when, got %q", src.Error)
	}
	if got := g.hits.Load(); got != before {
		t.Errorf("a marked-down peer was dialled: %d new request(s)", got-before)
	}
}

// Mutations are gated too, and fail with the same retryable unreachable.
func TestAMarkedDownPeerRefusesMutationsFastToo(t *testing.T) {
	g := newGatedPeer(t)
	d := newSleepy(t, g, 1, time.Hour, time.Hour)
	listOnce(t, d) // one failure trips K=1

	started := time.Now()
	_, err := d.Close(context.Background(), caller, fleet.SessionRef{Machine: "sleepy", ID: "x"})
	if err == nil {
		t.Fatal("want an error")
	}
	if took := time.Since(started); took > 50*time.Millisecond {
		t.Errorf("took %s", took)
	}
	fe, ok := err.(*fleet.Error)
	if !ok || fe.Kind != fleet.ErrorUnreachable || !fe.Retryable {
		t.Errorf("want a retryable unreachable, got %#v", err)
	}
}

// The background probe clears the state on the first answer, with no traffic
// from callers needed.
func TestTheProbeClearsTheStateWhenThePeerWakes(t *testing.T) {
	g := newGatedPeer(t)
	d := newSleepy(t, g, 2, 20*time.Millisecond, 40*time.Millisecond)

	listOnce(t, d)
	listOnce(t, d)
	if down, _, _ := d.PeerDown(); !down {
		t.Fatal("want down after 2 failures")
	}

	g.up.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if down, _, _ := d.PeerDown(); !down {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never cleared the down state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if src, _ := listOnce(t, d); src.Status != fleet.SourceOK {
		t.Errorf("after recovery: %+v", src)
	}
	if _, _, failures := d.PeerDown(); failures != 0 {
		t.Errorf("failure streak not reset: %d", failures)
	}
}

// A success in the middle of a streak resets it: K counts CONSECUTIVE
// failures, so a flapping peer is not marked down by a slow drip.
func TestASuccessResetsTheFailureStreak(t *testing.T) {
	g := newGatedPeer(t)
	d := newSleepy(t, g, 3, time.Hour, time.Hour)

	listOnce(t, d)
	listOnce(t, d)
	g.up.Store(true)
	listOnce(t, d) // answers
	g.up.Store(false)
	listOnce(t, d)
	listOnce(t, d)
	if down, _, f := d.PeerDown(); down || f != 2 {
		t.Fatalf("down=%v failures=%d; want a fresh streak of 2, not down", down, f)
	}
}

// A deadline the CALLER chose, shorter than this driver's own bound, is no
// evidence about the peer (#174): one impatient client must not mark a
// healthy, slow peer down for everyone. The peer here is alive — it answers
// /v1/health at once — and only the route the callers wait on is slow, so the
// driver's own check (#302) finds it up and the streak never starts.
func TestACallerShortenedDeadlineNeverMarksAPeerDown(t *testing.T) {
	g := newGatedPeer(t)
	g.healthy.Store(true)
	d := newSleepy(t, g, 2, time.Hour, time.Hour, WithDeadline(2*time.Second))

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err := d.List(ctx, caller, driver.ListFilter{})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	waitChecksIdle(t, d)
	if g.health.Load() == 0 {
		t.Fatal("caller-shortened misses never made the driver ask the peer itself")
	}
	if down, _, f := d.PeerDown(); down || f != 0 {
		t.Fatalf("caller-shortened misses counted: down=%v failures=%d", down, f)
	}
}

// A caller that hangs up is not a peer miss either.
func TestACallerThatHangsUpNeverMarksAPeerDown(t *testing.T) {
	g := newGatedPeer(t)
	g.healthy.Store(true)
	d := newSleepy(t, g, 1, time.Hour, time.Hour, WithDeadline(2*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, _ = d.List(ctx, caller, driver.ListFilter{})
	waitChecksIdle(t, d)
	if down, _, _ := d.PeerDown(); down {
		t.Fatal("a cancelled call marked the peer down")
	}
}

// waitChecksIdle waits for any miss-triggered check to finish, so an
// assertion about the final state does not race the check it provoked.
func waitChecksIdle(t *testing.T, d *Driver) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.down.mu.Lock()
		busy := d.down.checking
		d.down.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a miss-triggered check never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The defect in #302: a peer that drops every packet, and callers that each
// give up long before this driver's bound. None of those misses counts by
// itself, yet the peer must still be marked down — by the driver's own checks
// — and within a few check intervals, not never.
func TestShortTimeoutCallersAloneMarkADarkPeerDown(t *testing.T) {
	g := newGatedPeer(t) // hangs everything, /v1/health included
	d := newSleepy(t, g, 3, time.Hour, time.Hour, WithDeadline(200*time.Millisecond))

	started := time.Now()
	deadline := started.Add(5 * time.Second)
	for {
		if down, _, _ := d.PeerDown(); down {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer never marked down by short-timeout callers alone (failures=%d)", func() int { _, _, f := d.PeerDown(); return f }())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, _ = d.List(ctx, caller, driver.ListFilter{})
		cancel()
	}
	// Three checks of 200ms each, with slack: the oracle's "a few probe
	// intervals", not the K full bounds this used to take.
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("took %s to mark the peer down", took)
	}
	// Once down, the next read fails at once.
	if _, took := listOnce(t, d); took > 50*time.Millisecond {
		t.Errorf("a read after mark-down took %s", took)
	}
}

// Hang-ups are as good a reason to ask as shortened deadlines are.
func TestCallersThatHangUpAloneMarkADarkPeerDown(t *testing.T) {
	g := newGatedPeer(t)
	d := newSleepy(t, g, 2, time.Hour, time.Hour, WithDeadline(150*time.Millisecond))

	deadline := time.Now().Add(5 * time.Second)
	for {
		if down, _, _ := d.PeerDown(); down {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("hang-ups alone never marked a dark peer down")
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(10 * time.Millisecond); cancel() }()
		_, _ = d.List(ctx, caller, driver.ListFilter{})
	}
}

// One check in flight per peer, however many callers miss at once.
func TestMissTriggeredChecksAreSingleFlight(t *testing.T) {
	g := newGatedPeer(t)
	d := newSleepy(t, g, 100, time.Hour, time.Hour, WithDeadline(300*time.Millisecond))

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, _ = d.List(ctx, caller, driver.ListFilter{})
		}()
	}
	wg.Wait()
	waitChecksIdle(t, d)
	if peak := g.healthPeak.Load(); peak != 1 {
		t.Errorf("peak concurrent health checks = %d, want exactly 1", peak)
	}
}

// A peer slow enough to miss every impatient caller but not slow enough to
// miss the driver's own budget must never be marked down: the check answers,
// and an answer resets the streak.
func TestASlowPeerThatAnswersTheCheckIsNeverMarkedDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(80 * time.Millisecond): // slower than the callers, inside the check budget
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/v1/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"sources":[{"machine":"sleepy","status":"ok","observedAt":"2026-09-30T00:00:00Z"}],"complete":true}`))
	}))
	t.Cleanup(srv.Close)
	g := &gatedPeer{srv: srv}
	d := newSleepy(t, g, 2, time.Hour, time.Hour, WithDeadline(time.Second))

	for i := 0; i < 8; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, _ = d.List(ctx, caller, driver.ListFilter{})
		cancel()
		time.Sleep(100 * time.Millisecond) // let each check finish and answer
	}
	waitChecksIdle(t, d)
	if down, _, f := d.PeerDown(); down || f != 0 {
		t.Fatalf("slow-but-answering peer: down=%v failures=%d", down, f)
	}
}

// A peer that refuses the connection outright is a genuine transport failure
// and counts, so a wake-up race cannot leave the state stuck: the probe clears
// it as soon as something listens.
func TestAConnectionRefusedCountsAndClears(t *testing.T) {
	g := newGatedPeer(t)
	g.up.Store(true)
	url := g.srv.URL
	g.srv.Close()
	d := New("sleepy", url, WithDeadline(time.Second), WithDownPolicy(2, 20*time.Millisecond, 20*time.Millisecond))
	t.Cleanup(d.Shutdown)

	listOnce(t, d)
	listOnce(t, d)
	if down, _, _ := d.PeerDown(); !down {
		t.Fatal("two refused connections should mark the peer down")
	}
}

// One log line per transition — the incident this fixes logged ~30,000 lines
// for a single sleeping peer.
func TestDownStateLogsTransitionsNotEveryCall(t *testing.T) {
	buf := captureLog(t)
	g := newGatedPeer(t)
	d := newSleepy(t, g, 2, time.Hour, time.Hour)
	for i := 0; i < 20; i++ {
		listOnce(t, d)
	}
	out := buf.String()
	if n := strings.Count(out, "marked down"); n != 1 {
		t.Errorf("want exactly one 'marked down' line, got %d in:\n%s", n, out)
	}
	if n := strings.Count(out, "peer call failed"); n != 2 {
		t.Errorf("want 2 miss lines (the calls that dialled), got %d", n)
	}
}

// The issue's oracle, through the whole service path: with a peer that never
// answers, after K failures a fleet read AND GET /v1/machines return at once
// with that peer unreachable, and a scope=local read never waits on it.
func TestServiceReadsStayFastWhileAPeerSleeps(t *testing.T) {
	g := newGatedPeer(t)
	d := newSleepy(t, g, 2, time.Hour, time.Hour, WithDeadline(400*time.Millisecond))
	svc := homeService(t, "homebox", "sleepy", d)
	ctx := context.Background()
	req := fleetCaller("tok")

	// Local scope never dials the peer, before or after it is down.
	for i := 0; i < 2; i++ {
		started := time.Now()
		if _, err := svc.ListSessions(ctx, req, service.ScopeLocal, driver.ListFilter{}, 0); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(started); took > 50*time.Millisecond {
			t.Errorf("scope=local took %s", took)
		}
	}
	if got := g.hits.Load(); got != 0 {
		t.Fatalf("scope=local reached the peer (%d request(s))", got)
	}

	// K fleet reads each wait out the bound and mark the peer down.
	for i := 0; i < 2; i++ {
		col, err := svc.ListSessions(ctx, req, service.ScopeFleet, driver.ListFilter{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if col.Complete() {
			t.Fatal("a hung peer must not read complete")
		}
	}

	started := time.Now()
	col, err := svc.ListSessions(ctx, req, service.ScopeFleet, driver.ListFilter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 100*time.Millisecond {
		t.Errorf("fleet read after K failures took %s; want well under a second", took)
	}
	var sleepy fleet.SourceStatus
	for _, s := range col.Sources() {
		if s.Machine == "sleepy" {
			sleepy = s
		}
	}
	if sleepy.Status != fleet.SourceUnreachable || col.Complete() {
		t.Errorf("sleepy = %+v complete=%v; want unreachable and incomplete", sleepy, col.Complete())
	}

	started = time.Now()
	machines, err := svc.ListMachines(ctx, req, 0)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 100*time.Millisecond {
		t.Errorf("GET /v1/machines took %s while the peer is marked down", took)
	}
	for _, m := range machines.Items() {
		if m.Machine == "sleepy" && m.Status != fleet.SourceUnreachable {
			t.Errorf("machines lists the sleeping peer as %q", m.Status)
		}
	}
}
