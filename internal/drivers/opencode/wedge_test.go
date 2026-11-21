package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #513: a server whose connection to its own store broke answers 5xx for ever;
// the driver replaces it. These tests start REAL per-session processes (the stand-in in
// fakeopencode_test.go) over a state directory, on a clock they move: a process the
// stand-in is told to wedge fails every read of the session's store but not the status
// map, as the measured runtime did, and a process started on the same directory
// afterwards is healthy.

func newWedgeDriver(t *testing.T, extra ...Option) (*Driver, *parkClock, *parkLog, string) {
	t.Helper()
	clock, sink, state := newParkClock(), &parkLog{}, t.TempDir()
	opts := append([]Option{withClock(clock.now), WithLogf(sink.logf)}, extra...)
	d := newStateDriver(t, state, opts...)
	t.Cleanup(func() { _ = d.Shutdown() })
	return d, clock, sink, state
}

func pidOf(srv *server) int { return srv.proc.cmd.Process.Pid }

// wedgeServer makes srv's process fail the session's store from now on. "sqlite" also
// makes it log the storage failure the real runtime logs.
func wedgeServer(t *testing.T, cwd fleet.AbsolutePath, srv *server, marker string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(string(cwd), fmt.Sprintf(".fake-wedge-%d", pidOf(srv))), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
}

func healServer(t *testing.T, cwd fleet.AbsolutePath, srv *server) {
	t.Helper()
	if err := os.Remove(filepath.Join(string(cwd), fmt.Sprintf(".fake-wedge-%d", pidOf(srv)))); err != nil {
		t.Fatal(err)
	}
}

// failRead is one state read of a wedged session: the status map answers, the by-id read
// does not, and the read fails.
func failRead(t *testing.T, d *Driver, ref fleet.SessionRef) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.State(ctx, testReq, ref); err == nil {
		t.Fatal("State of a wedged session succeeded")
	}
}

// replacedBy waits for the session to be on a server other than old, and returns it.
func replacedBy(t *testing.T, d *Driver, id string, old *server) *server {
	t.Helper()
	var got *server
	waitFor(t, "the wedged server to be replaced", func() bool {
		k, ok := d.wasSeen(id)
		if ok && k.srv != nil && k.srv != old {
			got = k.srv
			return true
		}
		return false
	})
	return got
}

func neverReplaced(t *testing.T, d *Driver, id string, old *server) {
	t.Helper()
	time.Sleep(300 * time.Millisecond) // the restart is a goroutine: give a wrong one time to show
	k, _ := d.wasSeen(id)
	if k.srv != old || d.phaseOf(old) != phaseRunning || old.restarting.Load() {
		t.Fatalf("the server was replaced (or being replaced) with no wedge: phase %d", d.phaseOf(old))
	}
}

func TestWedge_ASingleFailureDoesNotRestartAndAHealthyReadEndsARun(t *testing.T) {
	d, clock, _, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)

	wedgeServer(t, known.cwd, known.srv, "")
	failRead(t, d, sess.SessionRef)
	healServer(t, known.cwd, known.srv)
	if got := parkState(t, d, sess.SessionRef); got.Status != fleet.StatusIdle {
		t.Fatalf("state after the store answered again = %s, want idle", got.Status)
	}

	// The healthy read ended the run: a failure twenty seconds later is the first of a
	// new one, not the third of the old.
	clock.advance(25 * time.Second)
	wedgeServer(t, known.cwd, known.srv, "")
	failRead(t, d, sess.SessionRef)
	neverReplaced(t, d, sess.ID, known.srv)
}

func TestWedge_ARunThatHasNotLastedTheSpanDoesNotRestart(t *testing.T) {
	d, clock, _, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)

	// Three reads failing together under one load blip.
	wedgeServer(t, known.cwd, known.srv, "")
	for i := 0; i < wedgeRun+2; i++ {
		failRead(t, d, sess.SessionRef)
		clock.advance(time.Second)
	}
	neverReplaced(t, d, sess.ID, known.srv)
}

func TestWedge_RepeatedFailuresOverTheSpanReplaceTheServer(t *testing.T) {
	d, clock, sink, state := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	old, oldPID := known.srv, pidOf(known.srv)
	wedgeServer(t, known.cwd, old, "")

	// Every one of these reads also reads the status map, which answers: that must not
	// end the run.
	failRead(t, d, sess.SessionRef)
	clock.advance(10 * time.Second)
	failRead(t, d, sess.SessionRef)
	clock.advance(11 * time.Second)
	failRead(t, d, sess.SessionRef)

	nsrv := replacedBy(t, d, sess.ID, old)
	if pidOf(nsrv) == oldPID {
		t.Fatal("the session is on the same process")
	}
	waitGone(t, "the wedged server", oldPID)
	waitForLine(t, sink, "replaced the runtime of session "+sess.ID)
	if !sink.has("restart 1 of 3") {
		t.Errorf("the notice does not say how much of the budget is used: %q", sink.lines)
	}
	if st := parkState(t, d, sess.SessionRef); st.Status != fleet.StatusIdle {
		t.Errorf("state on the new server = %s (%s), want idle", st.Status, st.Evidence)
	}
	if rcpt, err := sendText(t, d, sess.SessionRef, "carry on"); err != nil || rcpt.Outcome != fleet.OutcomeQueued {
		t.Errorf("Send on the new server = %+v, %v; want it queued", rcpt, err)
	}
	rec := d.store.load(sess.ID)
	if rec == nil || rec.Wedge == nil || len(rec.Wedge.RestartsMs) != 1 || rec.Wedge.Cause == "" {
		t.Errorf("the record does not keep the restart: %+v", rec)
	}
	if left := recordFiles(t, state); len(left) != 1 {
		t.Errorf("records after a replacement = %v, want the session's one", left)
	}
	if got, ok := d.wasSeen(sess.ID); !ok || got.name != known.name {
		t.Errorf("the session's identity changed across the replacement: %+v", got)
	}
}

func TestWedge_TheRuntimesLogLowersTheThreshold(t *testing.T) {
	d, _, sink, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	old := known.srv
	wedgeServer(t, known.cwd, old, "sqlite")

	// One failure never decides, whatever the log says.
	failRead(t, d, sess.SessionRef)
	waitFor(t, "the log line to be read", func() bool { return old.logs.ioerrCount() > 0 })
	neverReplaced(t, d, sess.ID, old)

	// The second does, with no span: the log is the evidence the span waits for.
	failRead(t, d, sess.SessionRef)
	replacedBy(t, d, sess.ID, old)
	waitForLine(t, sink, "runtime 500: sqlite disk I/O error")
}

func TestWedge_ASubscriberSeesNoCloseAcrossAReplacement(t *testing.T) {
	d, _, _, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	s := subscribe(t, d, driver.SubscribeFilter{})
	known, _ := d.wasSeen(sess.ID)
	old := known.srv
	waitFor(t, "the bus connection", func() bool {
		sub := s.(*subscription)
		sub.mu.Lock()
		defer sub.mu.Unlock()
		_, ok := sub.attached[old]
		return ok
	})
	wedgeServer(t, known.cwd, old, "sqlite")
	failRead(t, d, sess.SessionRef)
	failRead(t, d, sess.SessionRef)
	nsrv := replacedBy(t, d, sess.ID, old)

	// The stream is told what happened and never that the session closed.
	ctx, cancel := context.WithTimeout(context.Background(), busExitWait+500*time.Millisecond)
	defer cancel()
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			break
		}
		if ev.Kind == fleet.EventSessionClosed {
			t.Fatalf("a replaced server produced session.closed: %+v", ev.Payload)
		}
	}
	postTo(t, d, sess.ID, "/__test/emit", `{"type":"session.status","properties":{"sessionID":"`+sess.ID+`","status":{"type":"busy"}}}`)
	ev := next(t, s)
	if p := statePayload(t, ev); p.Ref.ID != sess.ID || p.State.Status != fleet.StatusWorking {
		t.Fatalf("event from the new process = %s %+v, want session.state working", ev.Kind, p)
	}
	_ = nsrv
}

func TestWedge_ATurnTheRuntimeHadNotFinishedIsReportedInterrupted(t *testing.T) {
	d, _, _, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	old := known.srv
	raw, _ := json.Marshal([]wireMessage{{Info: wireMessageInfo{Role: "assistant"}}})
	if err := os.WriteFile(filepath.Join(old.dir, "xdg", "data", "fake-messages.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	wedgeServer(t, known.cwd, old, "sqlite")
	failRead(t, d, sess.SessionRef)
	failRead(t, d, sess.SessionRef)
	replacedBy(t, d, sess.ID, old)

	st := parkState(t, d, sess.SessionRef)
	if st.LastTurn == nil || st.LastTurn.Outcome != "interrupted" || !st.LastTurn.Retryable ||
		!strings.Contains(st.LastTurn.Reason, "restarted") {
		t.Errorf("lastTurn = %+v; want an interrupted, retryable turn that says the runtime was restarted", st.LastTurn)
	}
}

// wedgeAndWait wedges whatever server the session is on now, with the log evidence so
// no span is needed, reads it twice, and waits for the driver to act: a new server, or
// no server at all.
func wedgeAndWait(t *testing.T, d *Driver, ref fleet.SessionRef) {
	t.Helper()
	k, _ := d.wasSeen(ref.ID)
	old := k.srv
	wedgeServer(t, k.cwd, old, "sqlite")
	failRead(t, d, ref)
	waitFor(t, "the log line to be read", func() bool { return old.logs.ioerrCount() > 0 })
	failRead(t, d, ref)
	waitFor(t, "the driver to act on the wedged server", func() bool {
		cur, _ := d.wasSeen(ref.ID)
		return cur.srv != old
	})
}

func TestWedge_TheBudgetRunsOutAndTheSessionIsListedWithTheCause(t *testing.T) {
	d, _, sink, state := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	dir := known.srv.dir

	for i := 1; i <= wedgeBudgetMax; i++ {
		wedgeAndWait(t, d, sess.SessionRef)
		if cur, _ := d.wasSeen(sess.ID); cur.srv == nil {
			t.Fatalf("gave up after %d restarts, before the budget of %d", i-1, wedgeBudgetMax)
		}
		waitForLine(t, sink, fmt.Sprintf("restart %d of %d", i, wedgeBudgetMax))
	}
	wedgeAndWait(t, d, sess.SessionRef)
	waitForLine(t, sink, "gave up replacing the runtime of session "+sess.ID)

	cur, _ := d.wasSeen(sess.ID)
	if cur.srv != nil {
		t.Fatal("the session still has a server after the budget ran out")
	}
	st := parkState(t, d, sess.SessionRef)
	if st.Status != fleet.StatusUnknown || !strings.Contains(st.Evidence, "runtime 500: sqlite disk I/O error") ||
		!strings.Contains(st.Evidence, "not replaced again") {
		t.Errorf("state = %s %q; want unknown, with the cause and that it is not replaced again", st.Status, st.Evidence)
	}
	if rec := d.store.load(sess.ID); rec == nil || rec.Wedge == nil || rec.Wedge.GaveUpAtMs == 0 || rec.RelaunchFailedAtMs == 0 {
		t.Errorf("the record does not say the driver gave up: %+v", rec)
	}
	if left, _ := d.NeedingRecreation(); left != 0 {
		t.Errorf("NeedingRecreation left = %d: a give-up is not a restart's caller-env count", left)
	}

	// The session is still the caller's to close, and closing it removes what it left.
	if ack, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil || !ack.Accepted {
		t.Fatalf("Close = %+v, %v", ack, err)
	}
	settleRemovals(t, state)
	if _, err := os.Stat(dir); err == nil {
		t.Error("the session's directory is still there after close")
	}
}

func TestWedge_TheBudgetRefillsAfterAnHour(t *testing.T) {
	d, clock, _, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	for i := 0; i < wedgeBudgetMax; i++ {
		wedgeAndWait(t, d, sess.SessionRef)
	}
	clock.advance(wedgeBudgetWindow + time.Minute)
	wedgeAndWait(t, d, sess.SessionRef)

	cur, _ := d.wasSeen(sess.ID)
	if cur.srv == nil || d.phaseOf(cur.srv) != phaseRunning {
		t.Fatalf("a wedge an hour after the last restart was not replaced: srv %v", cur.srv)
	}
	if rec := d.store.load(sess.ID); rec == nil || rec.Wedge == nil || len(rec.Wedge.RestartsMs) != 1 {
		t.Errorf("the budget still holds restarts older than the window: %+v", rec)
	}
}

func TestWedge_ACallerEnvSessionIsStoppedAndItsCallerCanReviveIt(t *testing.T) {
	d, _, sink, state := newWedgeDriver(t)
	env := map[string]string{"MODEL_KEY": reviveSecret}
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Env: env})
	known, _ := d.wasSeen(sess.ID)
	oldPID := pidOf(known.srv)
	leftBefore, waitingBefore := d.NeedingRecreation()

	wedgeAndWait(t, d, sess.SessionRef)
	waitForLine(t, sink, "needs its caller's environment")
	waitGone(t, "the wedged server", oldPID)

	st := parkState(t, d, sess.SessionRef)
	if st.Status != fleet.StatusUnknown || !strings.Contains(st.Evidence, "runtime 500: sqlite disk I/O error") ||
		!strings.Contains(st.Evidence, "caller-supplied environment") {
		t.Errorf("state = %s %q; want unknown, with the cause and what the caller can do", st.Status, st.Evidence)
	}
	if left, waiting := d.NeedingRecreation(); left != leftBefore || waiting != waitingBefore {
		t.Errorf("NeedingRecreation = %d, %d after a wedge, want %d, %d: the figure is about the start that built this driver",
			left, waiting, leftBefore, waitingBefore)
	}
	if rec := d.store.load(sess.ID); rec == nil || rec.Wedge == nil || len(rec.Wedge.RestartsMs) != 0 {
		t.Errorf("a stop that restarted nothing spent budget: %+v", rec)
	}
	assertNoSecretUnder(t, state) // the stop kept no value, as a service restart keeps none

	got, err := reviveWith(d, sess, env)
	if err != nil {
		t.Fatalf("Revive: %v", err)
	}
	if got.State.Status != fleet.StatusIdle {
		t.Errorf("revived state = %s (%s), want idle", got.State.Status, got.State.Evidence)
	}
	cur, _ := d.wasSeen(sess.ID)
	if cur.srv == nil || pidOf(cur.srv) == oldPID {
		t.Error("the revive did not put the session on a new process")
	}
}

func TestWedge_AWriteToASessionBeingReplacedIsRefusedRetryablyAndSendsNothing(t *testing.T) {
	d, _, _, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	known.srv.wedge.Store(&wedgeNote{at: time.Now(), cause: "runtime 500: sqlite disk I/O error"})

	st := parkState(t, d, sess.SessionRef)
	if st.Status != fleet.StatusUnknown || !strings.Contains(st.Evidence, "restarting") {
		t.Errorf("state during a replacement = %s %q; want unknown, restarting", st.Status, st.Evidence)
	}
	_, err := sendText(t, d, sess.SessionRef, "now")
	fe, ok := err.(*fleet.Error)
	if !ok || fe.Kind != fleet.ErrorUnreachable || !fe.Retryable {
		t.Fatalf("Send during a replacement = %v; want a retryable unreachable", err)
	}
	col, lerr := d.List(context.Background(), testReq, driver.ListFilter{})
	if lerr != nil || len(col.Items()) != 1 || col.Items()[0].State.Status != fleet.StatusUnknown {
		t.Errorf("List during a replacement = %v, %v; want the session, unknown", col, lerr)
	}
}

func TestWedge_FailuresOfAPathThatDoesNotReadTheStoreAreNotCounted(t *testing.T) {
	d, clock, _, _ := newWedgeDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)

	// A runtime whose permission list alone fails still has a working store.
	for i := 0; i < 3*wedgeRun; i++ {
		d.noteAnswer(known.srv, "/permission", 500)
		d.noteAnswer(known.srv, "/session/status", 500)
		clock.advance(wedgeSpan)
	}
	neverReplaced(t, d, sess.ID, known.srv)
}

func TestWedge_WithoutAStateDirectoryNothingIsRestarted(t *testing.T) {
	// Nothing to start it again from: the driver behaves as it did.
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	wedgeServer(t, known.cwd, known.srv, "sqlite")
	failRead(t, d, sess.SessionRef)
	waitFor(t, "the log line to be read", func() bool { return known.srv.logs.ioerrCount() > 0 })
	failRead(t, d, sess.SessionRef)
	failRead(t, d, sess.SessionRef)
	neverReplaced(t, d, sess.ID, known.srv)
}

func TestWedge_OnlyAPathThatReadsTheStoreEndsARun(t *testing.T) {
	for path, want := range map[string]bool{
		"/session":                       true,
		"/session/ses_1":                 true,
		"/session/ses_1/message?limit=1": true,
		"/session/ses_1/prompt_async":    true,
		"/session/status":                false,
		"/permission":                    false,
		"/question":                      false,
		"/config/providers":              false,
	} {
		if got := storeBacked(path); got != want {
			t.Errorf("storeBacked(%q) = %v, want %v", path, got, want)
		}
	}
}
