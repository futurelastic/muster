package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #391: a session that reads `working` and whose screen has not changed
// for the stall bound reads as stalled, a send to it is answered `stalled`
// with nothing written, and a repaint clears it.

const stallBound = 30 * time.Minute

// stallRig is one `working` session whose screen a test can freeze or repaint,
// read through a clock the test advances.
type stallRig struct {
	t     *testing.T
	f     *fakeMux
	d     *Driver
	clock time.Time
	ref   fleet.SessionRef
}

func newStallRig(t *testing.T, opts ...Option) *stallRig {
	t.Helper()
	f := twoSessions()
	f.setCapture("%1", fixtureWorking)
	r := &stallRig{t: t, f: f, clock: time.Unix(1785760000, 0), ref: fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}}
	base := []Option{
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return r.clock }),
	}
	r.d = New("testbox", append(base, opts...)...)
	return r
}

func (r *stallRig) state() fleet.SessionState {
	r.t.Helper()
	st, err := r.d.State(context.Background(), testCaller, r.ref)
	if err != nil {
		r.t.Fatalf("State: %v", err)
	}
	return st
}

func (r *stallRig) advance(by time.Duration) { r.clock = r.clock.Add(by) }

// pasted reports whether anything was written to a pane.
func (r *stallRig) pasted() bool {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	for _, c := range r.f.calls {
		for _, a := range c {
			if a == "paste-buffer" || a == "load-buffer" {
				return true
			}
		}
	}
	return false
}

func TestAFrozenWorkingScreenReadsStalledOnceTheBoundHasPassed(t *testing.T) {
	r := newStallRig(t)
	first := r.state()
	if first.Status != fleet.StatusWorking {
		t.Fatalf("setup: status = %s (%s), want working", first.Status, first.Evidence)
	}
	if first.Stalled != nil {
		t.Fatalf("a first sighting reads stalled: %+v", first.Stalled)
	}
	started := r.clock

	r.advance(stallBound - time.Second)
	if st := r.state(); st.Stalled != nil {
		t.Errorf("one second inside the bound reads stalled: %+v", st.Stalled)
	}

	r.advance(time.Second)
	st := r.state()
	if st.Status != fleet.StatusWorking {
		t.Errorf("status = %s; the stall is a footnote, and the screen still says working", st.Status)
	}
	if st.Stalled == nil {
		t.Fatal("a screen that held still for the whole bound does not read stalled")
	}
	if !st.Stalled.Since.Equal(started) {
		t.Errorf("stalled.since = %v, want the start of the quiet stretch %v", st.Stalled.Since, started)
	}
	if st.Stalled.Reason == "" {
		t.Error("stalled.reason is empty")
	}

	// A verdict that continues keeps its since: a mirror must hear it once.
	r.advance(43 * time.Hour)
	again := r.state()
	if again.Stalled == nil || !again.Stalled.Since.Equal(started) {
		t.Errorf("a continuing stall moved its since: %+v, want %v", again.Stalled, started)
	}
	if st.MateriallyDiffers(again) {
		t.Error("a continuing stall is a material change; every poll would emit an event")
	}
}

func TestARepaintClearsAStall(t *testing.T) {
	r := newStallRig(t)
	r.state()
	r.advance(2 * stallBound)
	if r.state().Stalled == nil {
		t.Fatal("setup: not stalled")
	}

	// The runtime draws again: the elapsed counter moved.
	r.f.setCapture("%1", strings.Replace(fixtureWorking, "5m 57s", "5m 58s", 1))
	r.advance(time.Second)
	st := r.state()
	if st.Stalled != nil {
		t.Fatalf("a repainted screen still reads stalled: %+v", st.Stalled)
	}
	if st.Status != fleet.StatusWorking {
		t.Errorf("status = %s, want working", st.Status)
	}

	// And it takes a whole new bound of stillness to read stalled again.
	r.advance(stallBound - time.Second)
	if r.state().Stalled != nil {
		t.Error("the new quiet stretch was judged against the old one")
	}
	r.advance(time.Second)
	if r.state().Stalled == nil {
		t.Error("the new quiet stretch never reads stalled")
	}
}

func TestOnlyAWorkingSessionCanBeStalled(t *testing.T) {
	r := newStallRig(t)
	r.f.setCapture("%1", fixtureIdle)
	r.state()
	r.advance(48 * time.Hour)
	st := r.state()
	if st.Status != fleet.StatusIdle {
		t.Fatalf("setup: status = %s", st.Status)
	}
	if st.Stalled != nil {
		t.Errorf("an idle session reads stalled: %+v — an idle screen that holds still is the ordinary case", st.Stalled)
	}
}

func TestAStatusThatBeganLaterIsNotQuietBeforeItBegan(t *testing.T) {
	r := newStallRig(t)
	// An idle screen, long unchanged...
	r.f.setCapture("%1", fixtureIdle)
	r.state()
	r.advance(10 * time.Hour)
	// ...then the session starts a turn.
	r.f.setCapture("%1", fixtureWorking)
	st := r.state()
	if st.Status != fleet.StatusWorking || st.Stalled != nil {
		t.Fatalf("a turn that just began: status %s, stalled %+v", st.Status, st.Stalled)
	}
	r.advance(stallBound - time.Second)
	if r.state().Stalled != nil {
		t.Error("the idle hours before the turn counted toward the stall")
	}
}

func TestListCarriesTheSameVerdictAsState(t *testing.T) {
	r := newStallRig(t)
	r.state()
	r.advance(stallBound)
	col, err := r.d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range col.Items() {
		switch s.ID {
		case "alpha💬":
			found = true
			if s.State.Stalled == nil {
				t.Errorf("List does not flag the stalled session: %+v", s.State)
			}
		case "beta":
			if s.State.Stalled != nil {
				t.Errorf("List flags a session that is not working: %+v", s.State.Stalled)
			}
		}
	}
	if !found {
		t.Fatal("setup: alpha not listed")
	}
}

func TestTheStallBoundCanBeChangedOrSwitchedOff(t *testing.T) {
	t.Run("shorter bound", func(t *testing.T) {
		r := newStallRig(t, WithStallAfter(time.Minute))
		r.state()
		r.advance(time.Minute)
		if r.state().Stalled == nil {
			t.Error("a one-minute bound does not flag a minute of stillness")
		}
	})
	t.Run("off", func(t *testing.T) {
		r := newStallRig(t, WithStallAfter(0))
		r.state()
		r.advance(43 * time.Hour)
		if st := r.state(); st.Stalled != nil {
			t.Errorf("a driver with the judgement off flags a session: %+v", st.Stalled)
		}
		if r.d.Capabilities().ObservesStall {
			t.Error("observesStall is declared on a driver that does not look")
		}
		if got, handled := r.d.stalledOnSend(context.Background(), r.ref); handled {
			t.Errorf("send gate fired with the judgement off: %+v", got)
		}
	})
	t.Run("negative is ignored", func(t *testing.T) {
		r := newStallRig(t, WithStallAfter(-time.Second))
		if r.d.stallAfter != defaultStallAfter {
			t.Errorf("stallAfter = %v, want the default %v", r.d.stallAfter, defaultStallAfter)
		}
	})
	t.Run("declared by default", func(t *testing.T) {
		if !newStallRig(t).d.Capabilities().ObservesStall {
			t.Error("observesStall not declared on a driver that looks")
		}
	})
}

func TestAFailedCaptureIsNotStillness(t *testing.T) {
	r := newStallRig(t)
	r.state()
	r.advance(2 * stallBound)
	// No capture at all for the pane: the read observed nothing.
	r.f.mu.Lock()
	delete(r.f.captures, "%1")
	r.f.mu.Unlock()
	if st := r.state(); st.Stalled != nil {
		t.Errorf("a read that saw no screen reads stalled: %+v", st.Stalled)
	}
}

func TestASendToAStalledSessionIsAnsweredStalledAndWritesNothing(t *testing.T) {
	r := newStallRig(t)
	r.state()
	r.advance(43 * time.Hour)

	for _, tc := range []struct {
		name string
		opts driver.SendOptions
	}{
		{"auto", driver.SendOptions{Submit: true, Route: fleet.RouteAuto}},
		{"terminal", driver.SendOptions{Submit: true, Route: fleet.RouteTerminal}},
		{"not submitted", driver.SendOptions{Submit: false}},
		{"resume", driver.SendOptions{Submit: true, ResumeIfStranded: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := r.d.Send(context.Background(), testCaller, r.ref, "are you there?", tc.opts)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if rc.Outcome != fleet.OutcomeStalled {
				t.Fatalf("outcome = %s (%s), want stalled", rc.Outcome, rc.Reason)
			}
			if !strings.Contains(rc.Reason, "Nothing was written") {
				t.Errorf("reason does not say nothing was written: %q", rc.Reason)
			}
		})
	}
	if r.pasted() {
		t.Error("text reached the pane of a stalled session")
	}
	c := r.d.Counters()
	if got := c["delivery.tmux.outcome.stalled"]; got != 4 {
		t.Errorf("counter delivery.tmux.outcome.stalled = %d, want the 4 sends above", got)
	}
	if got := c["delivery.tmux.stalled"]; got != 4 {
		t.Errorf("counter delivery.tmux.stalled = %d, want the 4 sends above", got)
	}
}

func TestASendIsNotRefusedWhenTheVerdictIsStaleOrNotYetReached(t *testing.T) {
	send := func(r *stallRig) fleet.DeliveryReceipt {
		t.Helper()
		rc, err := r.d.Send(context.Background(), testCaller, r.ref, "hello", driver.SendOptions{Submit: true})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		return rc
	}

	t.Run("inside the bound", func(t *testing.T) {
		r := newStallRig(t)
		r.state()
		r.advance(stallBound - time.Second)
		if rc := send(r); rc.Outcome == fleet.OutcomeStalled {
			t.Errorf("refused inside the bound: %s", rc.Reason)
		}
	})
	t.Run("never read", func(t *testing.T) {
		// The service holds no memory of this session, so there is no quiet
		// stretch to measure — the send is judged by everything else, as before.
		r := newStallRig(t)
		r.advance(43 * time.Hour)
		if rc := send(r); rc.Outcome == fleet.OutcomeStalled {
			t.Errorf("refused a session nobody has read: %s", rc.Reason)
		}
	})
	t.Run("the screen moved since the last read", func(t *testing.T) {
		// Last read says stalled; the pane has been repainted since. The verdict
		// is as of the last read, and a send must be judged on the session as it
		// is now.
		r := newStallRig(t)
		r.state()
		r.advance(43 * time.Hour)
		r.f.setCapture("%1", strings.Replace(fixtureWorking, "5m 57s", "6m 01s", 1))
		if rc := send(r); rc.Outcome == fleet.OutcomeStalled {
			t.Errorf("refused a session whose screen has moved: %s", rc.Reason)
		}
	})
	t.Run("not working", func(t *testing.T) {
		r := newStallRig(t)
		r.f.setCapture("%1", fixtureIdle)
		r.state()
		r.advance(43 * time.Hour)
		if rc := send(r); rc.Outcome == fleet.OutcomeStalled {
			t.Errorf("refused an idle session: %s", rc.Reason)
		}
	})
}
