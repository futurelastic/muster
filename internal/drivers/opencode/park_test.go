package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #421: a server with nothing to do is stopped, and started again by the
// next input. These tests start REAL per-session processes (the stand-in in
// fakeopencode_test.go) over a state directory, and drive the sweep by hand
// (parkIdle) with a clock they control, so a bound of ten minutes costs nothing.

const parkBound = 10 * time.Minute

// parkClock is a clock a test moves forward. It starts at the real time and never
// goes back: the driver's own deadlines are made from it and checked against the
// real one.
type parkClock struct {
	mu sync.Mutex
	t  time.Time
}

func newParkClock() *parkClock { return &parkClock{t: time.Now()} }

func (c *parkClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *parkClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// parkLog keeps the driver's notices.
type parkLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *parkLog) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *parkLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ln := range l.lines {
		if strings.Contains(ln, sub) {
			return true
		}
	}
	return false
}

func newParkDriver(t *testing.T, extra ...Option) (*Driver, *parkClock, *parkLog, string) {
	t.Helper()
	clock, sink, state := newParkClock(), &parkLog{}, t.TempDir()
	opts := append([]Option{WithIdleStop(parkBound), withClock(clock.now), WithLogf(sink.logf)}, extra...)
	d := newStateDriver(t, state, opts...)
	t.Cleanup(func() { _ = d.Shutdown() })
	return d, clock, sink, state
}

// parkSession lets the bound pass and runs one pass of the sweep.
func parkSession(t *testing.T, d *Driver, clock *parkClock, id string) *server {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok {
		t.Fatalf("session %s is not known", id)
	}
	clock.advance(parkBound + time.Minute)
	d.parkIdle()
	if got := d.phaseOf(known.srv); got != phaseParked {
		t.Fatalf("phase after the bound = %d, want parked (%d)", got, phaseParked)
	}
	return known.srv
}

func liveCount(d *Driver) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.live)
}

func sendText(t *testing.T, d *Driver, ref fleet.SessionRef, text string) (fleet.DeliveryReceipt, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return d.Send(ctx, testReq, ref, text, driver.SendOptions{Submit: true})
}

func parkState(t *testing.T, d *Driver, ref fleet.SessionRef) fleet.SessionState {
	t.Helper()
	st, err := d.State(context.Background(), testReq, ref)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return st
}

func TestPark_AnIdleServerIsParkedAfterTheBound(t *testing.T) {
	d, clock, sink, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "idler"})
	known, _ := d.wasSeen(sess.ID)
	srv, pid := known.srv, known.srv.proc.cmd.Process.Pid

	d.parkIdle()
	clock.advance(parkBound - time.Minute)
	d.parkIdle()
	if got := d.phaseOf(srv); got != phaseRunning {
		t.Fatalf("phase inside the bound = %d, want running", got)
	}
	clock.advance(2 * time.Minute)
	d.parkIdle()

	if got := d.phaseOf(srv); got != phaseParked {
		t.Fatalf("phase past the bound = %d, want parked", got)
	}
	waitGone(t, "the parked server", pid)
	if left := recordFiles(t, state); len(left) != 1 {
		t.Errorf("records after a park = %v, want the session's one", left)
	}
	if _, err := os.Stat(srv.dir); err != nil {
		t.Errorf("the session's directory after a park: %v", err)
	}
	if n := liveCount(d); n != 0 {
		t.Errorf("%d servers still tracked after the only one was parked", n)
	}
	if !sink.has("parked session " + sess.ID) {
		t.Error("the park left no notice")
	}
}

func TestPark_OffStartsNoSweepAndParksNothing(t *testing.T) {
	// The default: no bound, no goroutine, no read, whatever the time.
	state, clock := t.TempDir(), newParkClock()
	d := newStateDriver(t, state, withClock(clock.now))
	t.Cleanup(func() { _ = d.Shutdown() })
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	if d.idleStopCh != nil || d.idleDone != nil {
		t.Error("a sweep goroutine exists with no bound set")
	}
	clock.advance(48 * time.Hour)
	d.parkIdle()
	if got := d.phaseOf(known.srv); got != phaseRunning {
		t.Errorf("phase with the knob off = %d, want running", got)
	}

	// A bound with nowhere to keep a session's record is not a bound: a stopped
	// session could not be started again.
	sink := &parkLog{}
	d2, _ := newIsolatedDriver(t, WithIdleStop(parkBound), WithLogf(sink.logf))
	if d2.idleStop != 0 || d2.idleStopCh != nil {
		t.Errorf("idleStop = %v with no state directory, want it off", d2.idleStop)
	}
	if !sink.has("no state directory") {
		t.Error("the driver did not say why the bound was dropped")
	}
}

func TestPark_TheSweepGoroutineParksAndShutdownWaitsForIt(t *testing.T) {
	d, clock, _, _ := newParkDriver(t, withIdleSweep(20*time.Millisecond))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	if d.idleStopCh == nil {
		t.Fatal("no sweep goroutine with a bound and a state directory")
	}
	clock.advance(parkBound + time.Minute)
	waitFor(t, "the sweep to park the session", func() bool { return d.phaseOf(known.srv) == phaseParked })

	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.idleDone:
	default:
		t.Error("Shutdown returned while the sweep goroutine was still running")
	}
}

func TestPark_ParkedReadsIdleWithParkedEvidence(t *testing.T) {
	d, clock, _, _ := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "idler"})
	parkSession(t, d, clock, sess.ID)

	st := parkState(t, d, sess.SessionRef)
	if st.Status != fleet.StatusIdle || st.Confidence != fleet.ConfidenceObserved || !strings.HasPrefix(st.Evidence, "parked:") {
		t.Errorf("State of a parked session = %s/%s %q, want idle, observed, parked evidence", st.Status, st.Confidence, st.Evidence)
	}
	for name, filter := range map[string]driver.ListFilter{"listing": {}, "listing of one": {ID: sess.ID}} {
		col, err := d.List(context.Background(), testReq, filter)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var found bool
		for _, s := range col.Items() {
			if s.ID != sess.ID {
				continue
			}
			found = true
			if s.State.Status != fleet.StatusIdle || !strings.HasPrefix(s.State.Evidence, "parked:") {
				t.Errorf("%s reads %s %q, want idle with parked evidence — never dead or unknown", name, s.State.Status, s.State.Evidence)
			}
		}
		if !found {
			t.Errorf("%s does not list the parked session", name)
		}
	}
}

func TestPark_ReadsNeverStartAProcess(t *testing.T) {
	d, clock, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	srv := parkSession(t, d, clock, sess.ID)
	pidBefore := d.store.load(sess.ID).Pid

	for i := 0; i < 3; i++ {
		parkState(t, d, sess.SessionRef)
	}
	_ = listed(t, d)
	if _, err := d.List(context.Background(), testReq, driver.ListFilter{ID: sess.ID}); err != nil {
		t.Fatal(err)
	}
	s := subscribe(t, d, driver.SubscribeFilter{})
	if sub := s.(*subscription); func() bool { sub.mu.Lock(); defer sub.mu.Unlock(); return len(sub.attached) != 0 }() {
		t.Error("a subscription attached to a parked server")
	}
	_, err := d.Turns(context.Background(), testReq, sess.SessionRef, fleet.TurnsQuery{})
	if kindOfErr(err) != fleet.ErrorConflict {
		t.Errorf("Turns on a parked session = %v, want a conflict that says it is parked", err)
	}

	known, _ := d.wasSeen(sess.ID)
	if known.srv != srv || d.phaseOf(srv) != phaseParked {
		t.Error("a read replaced or revived the parked server")
	}
	if n := liveCount(d); n != 0 {
		t.Errorf("%d servers tracked after reads of a parked session", n)
	}
	if pid := d.store.load(sess.ID).Pid; pid != pidBefore {
		t.Errorf("the record's pid changed from %d to %d: something started a process", pidBefore, pid)
	}
	if left := recordFiles(t, state); len(left) != 1 {
		t.Errorf("records = %v", left)
	}
}

func TestPark_SendStartsTheServerAgainAndDelivers(t *testing.T) {
	d, clock, _, _ := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	dataBefore := inside(t, d, sess.ID).vars()["XDG_DATA_HOME"]
	old := parkSession(t, d, clock, sess.ID)
	oldPid := old.proc.cmd.Process.Pid

	rcpt, err := sendText(t, d, sess.SessionRef, "wake up")
	if err != nil {
		t.Fatalf("Send to a parked session: %v", err)
	}
	if rcpt.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %s, want queued", rcpt.Outcome)
	}
	known, _ := d.wasSeen(sess.ID)
	if known.srv == old || d.phaseOf(known.srv) != phaseRunning {
		t.Fatal("the session is not on a new, running server")
	}
	view := inside(t, d, sess.ID)
	if view.Pid == oldPid {
		t.Error("the same process answered after a park")
	}
	if got := view.vars()["XDG_DATA_HOME"]; got != dataBefore {
		t.Errorf("XDG_DATA_HOME = %q after the start, want the session's own %q", got, dataBefore)
	}
	raw, err := os.ReadFile(filepath.Join(dataBefore, "fake-prompts.jsonl"))
	if err != nil || !strings.Contains(string(raw), "wake up") {
		t.Errorf("the prompt did not reach the new process: %v %q", err, raw)
	}
	rec := d.store.load(sess.ID)
	if rec.Pid != view.Pid {
		t.Errorf("record pid = %d, want the new process's %d", rec.Pid, view.Pid)
	}
	if st := parkState(t, d, sess.SessionRef); strings.HasPrefix(st.Evidence, "parked:") {
		t.Errorf("State after the start still says %q", st.Evidence)
	}
}

func TestPark_InterruptAndRespondStartItToo(t *testing.T) {
	d, clock, _, _ := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	srv := parkSession(t, d, clock, sess.ID)

	if ack, err := d.Interrupt(context.Background(), testReq, sess.SessionRef); err != nil || !ack.Accepted {
		t.Fatalf("Interrupt on a parked session = %+v, %v", ack, err)
	}
	if known, _ := d.wasSeen(sess.ID); known.srv == srv || d.phaseOf(known.srv) != phaseRunning {
		t.Fatal("Interrupt did not start the session's server again")
	}

	srv = parkSession(t, d, clock, sess.ID)
	rcpt, err := d.Respond(context.Background(), testReq, sess.SessionRef, fleet.Response{Choice: 1})
	if err != nil {
		t.Fatalf("Respond on a parked session: %v", err)
	}
	if rcpt.Outcome != fleet.OutcomeRefused || !strings.Contains(rcpt.Reason, "not waiting") {
		t.Errorf("Respond = %+v, want a refusal that says it is not waiting on an ask", rcpt)
	}
	if known, _ := d.wasSeen(sess.ID); known.srv == srv || d.phaseOf(known.srv) != phaseRunning {
		t.Error("Respond did not start the session's server again")
	}
}

func TestPark_ASubscriberSeesNoCloseAcrossAParkAndAStart(t *testing.T) {
	d, clock, _, _ := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	s := subscribe(t, d, driver.SubscribeFilter{})
	sub := s.(*subscription)
	known, _ := d.wasSeen(sess.ID)
	waitFor(t, "the bus connection", func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		_, ok := sub.attached[known.srv]
		return ok
	})

	parked := parkSession(t, d, clock, sess.ID)
	waitFor(t, "the stream to let go of the parked server", func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		_, ok := sub.attached[parked]
		return !ok
	})
	// Longer than the wait that tells a gap from an exit: the stream was told the
	// session was idle, it still is, and nothing — no close, no gap — is news.
	ctx, cancel := context.WithTimeout(context.Background(), busExitWait+500*time.Millisecond)
	defer cancel()
	if ev, err := s.Next(ctx); err == nil {
		t.Fatalf("a parked session produced an event: %s %+v", ev.Kind, ev.Payload)
	} else if ctx.Err() == nil {
		t.Fatalf("the stream ended when a session was parked: %v", err)
	}

	if _, err := sendText(t, d, sess.SessionRef, "wake up"); err != nil {
		t.Fatal(err)
	}
	postTo(t, d, sess.ID, "/__test/emit", `{"type":"session.status","properties":{"sessionID":"`+sess.ID+`","status":{"type":"busy"}}}`)
	ev := next(t, s)
	if p := statePayload(t, ev); ev.Kind != fleet.EventSessionState || p.Ref.ID != sess.ID || p.State.Status != fleet.StatusWorking {
		t.Fatalf("first event after the start = %s %+v, want session.state working from the new process", ev.Kind, p)
	}
	silent(t, s, "a start is one stream of events, not a close and a creation")
}

func TestPark_ACloseOfAParkedSessionRemovesWhatItLeft(t *testing.T) {
	d, clock, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	srv := parkSession(t, d, clock, sess.ID)
	s := subscribe(t, d, driver.SubscribeFilter{})

	if ack, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil || !ack.Accepted {
		t.Fatalf("Close on a parked session = %+v, %v", ack, err)
	}
	if ev := next(t, s); ev.Kind != fleet.EventSessionClosed || statePayload(t, ev).Ref.ID != sess.ID {
		t.Fatalf("event = %s, want session.closed for the session", ev.Kind)
	}
	silent(t, s, "a close is one event")
	if left := recordFiles(t, state); len(left) != 0 {
		t.Errorf("records left after close: %v", left)
	}
	if _, err := os.Stat(srv.dir); err == nil {
		t.Error("the session's directory is still there after close")
	}
	if n := liveCount(d); n != 0 {
		t.Errorf("%d servers tracked after closing a parked session: Close started a process", n)
	}
	if _, ok := d.wasSeen(sess.ID); ok {
		t.Error("the closed session is still known")
	}
}

func TestPark_WhatIsNeverParked(t *testing.T) {
	cases := []struct {
		name string
		// setup runs after the session exists and before the bound has passed.
		setup func(t *testing.T, d *Driver, sess fleet.Session, cwd string)
	}{
		{"a turn in progress", func(t *testing.T, d *Driver, sess fleet.Session, cwd string) {
			if err := os.WriteFile(filepath.Join(cwd, ".fake-busy"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a permission ask waiting", func(t *testing.T, d *Driver, sess fleet.Session, cwd string) {
			if err := os.WriteFile(filepath.Join(cwd, ".fake-ask"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, clock, _, _ := newParkDriver(t)
			cwd := t.TempDir()
			sess := mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
			known, _ := d.wasSeen(sess.ID)
			tc.setup(t, d, sess, cwd)
			for i := 0; i < 3; i++ {
				clock.advance(parkBound + time.Minute)
				d.parkIdle()
			}
			if got := d.phaseOf(known.srv); got != phaseRunning {
				t.Fatalf("phase = %d, want running: %s", got, tc.name)
			}
		})
	}

	t.Run("a session created with caller environment", func(t *testing.T) {
		d, clock, _, _ := newParkDriver(t)
		sess := mustCreate(t, d, "k1", fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": "x"}})
		known, _ := d.wasSeen(sess.ID)
		clock.advance(parkBound + time.Minute)
		d.parkIdle()
		if got := d.phaseOf(known.srv); got != phaseRunning {
			t.Fatalf("phase = %d, want running: it could not be started again as it was created", got)
		}
	})

	t.Run("a session written to inside the bound", func(t *testing.T) {
		d, clock, _, _ := newParkDriver(t)
		sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
		known, _ := d.wasSeen(sess.ID)
		clock.advance(parkBound - time.Minute)
		if _, err := sendText(t, d, sess.SessionRef, "still here"); err != nil {
			t.Fatal(err)
		}
		clock.advance(parkBound - time.Minute) // a whole bound since the create, not since the send
		d.parkIdle()
		if got := d.phaseOf(known.srv); got != phaseRunning {
			t.Fatalf("phase = %d, want running: it was written to a bound minus a minute ago", got)
		}
		clock.advance(2 * time.Minute)
		d.parkIdle()
		if got := d.phaseOf(known.srv); got != phaseParked {
			t.Fatalf("phase = %d, want parked once a whole bound has passed since the send", got)
		}
	})

	t.Run("a turn nobody watched is waited out for a whole bound afterwards", func(t *testing.T) {
		d, clock, _, _ := newParkDriver(t)
		cwd := t.TempDir()
		sess := mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
		known, _ := d.wasSeen(sess.ID)
		busy := filepath.Join(cwd, ".fake-busy")
		if err := os.WriteFile(busy, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		clock.advance(parkBound + time.Minute)
		d.parkIdle() // busy: watched from here
		if err := os.Remove(busy); err != nil {
			t.Fatal(err)
		}
		d.parkIdle() // first seen idle: its quiet time starts now
		if got := d.phaseOf(known.srv); got != phaseRunning {
			t.Fatalf("phase = %d right after the turn ended, want running", got)
		}
		clock.advance(parkBound - time.Minute)
		d.parkIdle()
		if got := d.phaseOf(known.srv); got != phaseRunning {
			t.Fatalf("phase = %d inside the bound after the turn, want running", got)
		}
		clock.advance(2 * time.Minute)
		d.parkIdle()
		if got := d.phaseOf(known.srv); got != phaseParked {
			t.Fatalf("phase = %d a bound after the turn, want parked", got)
		}
	})
}

func TestPark_AFailedStartKeepsTheSessionParkedAndSaysWhy(t *testing.T) {
	d, clock, _, _ := newParkDriver(t)
	cwd := t.TempDir()
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	data := inside(t, d, sess.ID).vars()["XDG_DATA_HOME"]
	srv := parkSession(t, d, clock, sess.ID)

	if err := os.WriteFile(filepath.Join(cwd, ".fake-fail-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := sendText(t, d, sess.SessionRef, "lost message")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnreachable || !fe.Retryable {
		t.Fatalf("Send = %v, want a retryable unreachable error", err)
	}
	if !strings.Contains(fe.Message, "nothing was done") {
		t.Errorf("message %q does not say nothing was sent", fe.Message)
	}
	st := parkState(t, d, sess.SessionRef)
	if st.Status != fleet.StatusIdle || !strings.Contains(st.Evidence, "starting it again failed") {
		t.Errorf("State after a failed start = %s %q, want idle, parked, and why", st.Status, st.Evidence)
	}
	if known, _ := d.wasSeen(sess.ID); known.srv != srv || d.phaseOf(srv) != phaseParked {
		t.Error("the session left the parked state on a failed start")
	}
	if raw, _ := os.ReadFile(filepath.Join(data, "fake-prompts.jsonl")); strings.Contains(string(raw), "lost message") {
		t.Error("a message reached a runtime that never started")
	}

	if err := os.Remove(filepath.Join(cwd, ".fake-fail-ready")); err != nil {
		t.Fatal(err)
	}
	if _, err := sendText(t, d, sess.SessionRef, "second try"); err != nil {
		t.Fatalf("Send after the cause was removed: %v", err)
	}
	if known, _ := d.wasSeen(sess.ID); known.srv == srv || d.phaseOf(known.srv) != phaseRunning {
		t.Error("the next send did not start the session again")
	}
}

func TestPark_ASendRacingThePassIsSafe(t *testing.T) {
	d, clock, sink, _ := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	data := inside(t, d, sess.ID).vars()["XDG_DATA_HOME"]

	const sends = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clock.advance(parkBound + time.Minute)
			d.parkIdle()
		}
	}()
	for i := 0; i < sends; i++ {
		if _, err := sendText(t, d, sess.SessionRef, fmt.Sprintf("msg-%d", i)); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Send %d racing the sweep: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	// The race was real: the sweep did park the session, and a send did start it again.
	if !sink.has("parked session "+sess.ID) || !sink.has("started parked session "+sess.ID) {
		t.Error("the sweep and the sends never met: no park and start cycle happened, so this proved nothing")
	}
	known, _ := d.wasSeen(sess.ID)
	if got := d.phaseOf(known.srv); got != phaseRunning && got != phaseParked {
		t.Errorf("the session ended %d, want running or parked, never dead", got)
	}
	if n := liveCount(d); n > 1 {
		t.Errorf("%d servers tracked for one session", n)
	}
	raw, _ := os.ReadFile(filepath.Join(data, "fake-prompts.jsonl"))
	for i := 0; i < sends; i++ {
		if !strings.Contains(string(raw), fmt.Sprintf("msg-%d", i)) {
			t.Errorf("message %d never reached a runtime", i)
		}
	}
}

func TestPark_AParkedSessionSurvivesAShutdown(t *testing.T) {
	d, clock, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "sleeper"})
	parkSession(t, d, clock, sess.ID)
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if left := recordFiles(t, state); len(left) != 1 {
		t.Fatalf("records after a shutdown with a parked session = %v, want its one", left)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusIdle {
		t.Fatalf("after a restart the session reads %+v (listed %v), want idle", got.State, ok)
	}
	if _, err := sendText(t, d2, got.SessionRef, "welcome back"); err != nil {
		t.Errorf("Send after a restart: %v", err)
	}
}
