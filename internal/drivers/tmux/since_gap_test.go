package tmux

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/state"
)

// gapRig is a driver over a fake multiplexer whose batched capture can be made
// to return nothing, with a clock the test moves (#278).
type gapRig struct {
	d *Driver
	f *fakeMux

	mu      sync.Mutex
	now     time.Time
	failCap bool
}

func newGapRig(t *testing.T, n int) *gapRig {
	t.Helper()
	f := &fakeMux{captures: map[string]string{}}
	for i := 0; i < n; i++ {
		name := "s" + itoa(i)
		pane := "%" + itoa(10+i)
		f.sessions = append(f.sessions, fakeSession{
			name: name, paneID: pane, cwd: "/work/" + name, pid: 100 + i, created: int64(1785600000 + i),
			title: "2_1_220",
		})
		f.captures[pane] = idleFixtureFor(name)
	}
	r := &gapRig{f: f, now: time.Unix(1785760000, 0)}
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exec := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		r.mu.Lock()
		fail := r.failCap
		r.mu.Unlock()
		// The batched capture is the only invocation that chains commands.
		if fail && len(args) > 0 && args[0] == "display-message" {
			return nil, context.Canceled
		}
		return f.exec(ctx, name, args...)
	}
	r.d = New("testbox", withExec(exec), WithState(st),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }))
	return r
}

func (r *gapRig) advance(by time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(by)
	r.mu.Unlock()
}

func (r *gapRig) setFail(v bool) {
	r.mu.Lock()
	r.failCap = v
	r.mu.Unlock()
}

func (r *gapRig) list(t *testing.T) map[string]fleet.SessionState {
	t.Helper()
	got, err := r.d.List(context.Background(), testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]fleet.SessionState{}
	for _, s := range got.Items() {
		out[s.ID] = s.State
	}
	return out
}

// N sessions idle since T, one chunk-wide malfunction read, one good read:
// every since is still T (#278).
func TestSinceSurvivesAChunkWideMalfunctionRead(t *testing.T) {
	const n = 12
	r := newGapRig(t, n)

	// Two good reads, so the idle status is settled and not a first sighting.
	r.list(t)
	r.advance(time.Minute)
	before := r.list(t)
	for id, st := range before {
		if st.Status != fleet.StatusIdle {
			t.Fatalf("%s: setup read %q, want idle", id, st.Status)
		}
	}

	r.advance(time.Hour)
	r.setFail(true)
	during := r.list(t)
	for id, st := range during {
		if st.Status != fleet.StatusUnknown {
			t.Fatalf("%s: a failed capture must still read unknown, got %q", id, st.Status)
		}
		if !strings.Contains(st.Evidence, "malfunction") {
			t.Errorf("%s: the malfunction is no longer said aloud: %q", id, st.Evidence)
		}
	}

	r.advance(time.Hour)
	r.setFail(false)
	after := r.list(t)
	if len(after) != n {
		t.Fatalf("listed %d sessions, want %d", len(after), n)
	}
	for id, st := range after {
		if st.Status != fleet.StatusIdle {
			t.Fatalf("%s: read %q after recovery, want idle", id, st.Status)
		}
		if !st.Since.Equal(*before[id].Since) {
			t.Errorf("%s: since moved from %s to %s across one failed read; an idle "+
				"session must not look just-active", id, before[id].Since, st.Since)
		}
	}
}

// The unknown during the gap has one start, not a new one per poll; and a
// status that really changed across the gap still restarts since.
func TestGapIsOneStretchAndARealChangeStillRestartsSince(t *testing.T) {
	r := newGapRig(t, 2)
	r.list(t)
	r.advance(time.Minute)
	idleSince := *r.list(t)["s0"].Since

	r.setFail(true)
	r.advance(time.Minute)
	first := r.list(t)["s0"]
	r.advance(time.Minute)
	second := r.list(t)["s0"]
	if first.Since == nil || second.Since == nil || !first.Since.Equal(*second.Since) {
		t.Errorf("two consecutive failed reads gave since %v then %v; one gap is one start",
			first.Since, second.Since)
	}
	if first.Since.Equal(idleSince) {
		t.Errorf("unknown must not borrow the idle since")
	}

	// The pane really changed while nobody could look.
	r.f.captures["%10"] = fixtureUnsent
	r.setFail(false)
	r.advance(time.Minute)
	got := r.list(t)["s0"]
	if got.Status == fleet.StatusIdle {
		t.Fatalf("setup: s0 should no longer read idle")
	}
	if got.Since.Equal(idleSince) {
		t.Errorf("a different status after the gap kept the old since %s", idleSince)
	}
	// ... and the untouched session still carries its original since.
	if s1 := r.list(t)["s1"]; !s1.Since.Equal(idleSince) && s1.Since.Before(idleSince) {
		t.Errorf("s1 since %s is earlier than any observation", s1.Since)
	}
}

// State reads go through the same stamp as List.
func TestStateSinceSurvivesAMalfunctionRead(t *testing.T) {
	r := newGapRig(t, 3)
	r.list(t)
	r.advance(time.Minute)
	ref := fleet.SessionRef{ID: "s1", Name: "s1", Machine: "testbox"}
	before, err := r.d.State(context.Background(), testCaller, ref)
	if err != nil {
		t.Fatal(err)
	}

	r.setFail(true)
	r.advance(time.Hour)
	if st, err := r.d.State(context.Background(), testCaller, ref); err != nil || st.Status != fleet.StatusUnknown {
		t.Fatalf("State during the gap = %v, %v; want unknown", st.Status, err)
	}
	r.setFail(false)
	r.advance(time.Hour)
	after, err := r.d.State(context.Background(), testCaller, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Since.Equal(*before.Since) {
		t.Errorf("State since moved from %s to %s across a failed read", before.Since, after.Since)
	}
}

// A pane that was not read last time is not known idle: the Send lane's
// idle-before check must not take the stale kept status at its word.
func TestIdleBeforeSendDoesNotTrustAGap(t *testing.T) {
	r := newGapRig(t, 1)
	r.list(t)
	r.advance(time.Minute)
	r.list(t)
	if idle, _ := r.d.idleBeforeSend("s0"); !idle {
		t.Fatal("setup: s0 should be known idle")
	}
	r.setFail(true)
	r.list(t)
	if idle, _ := r.d.idleBeforeSend("s0"); idle {
		t.Error("a pane whose last read failed was reported idle from a stale observation")
	}
	r.setFail(false)
	r.list(t)
	if idle, _ := r.d.idleBeforeSend("s0"); !idle {
		t.Error("idle was not restored by a good read")
	}
}
