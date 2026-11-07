package opencode

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #423: a session parked before a service restart is listed parked after it,
// with no process started, instead of coming back running for one more bound.

// parkedRecordOf reads the record the driver left for id, as the next service will.
func parkedRecordOf(t *testing.T, state, id string) *sessionRecord {
	t.Helper()
	dir, _ := stateLayout(state)
	rec := (&recordStore{dir: dir}).load(id)
	if rec == nil {
		t.Fatalf("no readable record for %s under %s", id, state)
	}
	return rec
}

// restartWith is the service starting again over state, with the idle bound given
// (zero for none), the way a deploy does.
func restartWith(t *testing.T, state string, bound time.Duration, extra ...Option) (*Driver, *parkLog) {
	t.Helper()
	sink := &parkLog{}
	opts := append([]Option{WithLogf(sink.logf)}, extra...)
	if bound > 0 {
		opts = append(opts, WithIdleStop(bound))
	}
	d := newStateDriver(t, state, opts...)
	t.Cleanup(func() { _ = d.Shutdown() })
	return d, sink
}

// parkedThenStopped creates a session, parks it, and stops the service cleanly.
func parkedThenStopped(t *testing.T, spec fleet.SessionSpec) (fleet.Session, string) {
	t.Helper()
	d, clock, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", spec)
	parkSession(t, d, clock, sess.ID)
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	return sess, state
}

func TestParkRestart_ThePointOfTheParkIsWrittenToTheRecord(t *testing.T) {
	d, clock, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "idler"})
	if rec := parkedRecordOf(t, state, sess.ID); rec.Parked != nil {
		t.Fatalf("a running session's record says parked: %+v", rec.Parked)
	}
	parkSession(t, d, clock, sess.ID)

	rec := parkedRecordOf(t, state, sess.ID)
	if rec.Parked == nil {
		t.Fatal("the record of a parked session does not say so")
	}
	if rec.Version != recordVersion || recordVersion != 1 {
		t.Errorf("record version = %d (current %d), want 1: the new field is optional, not a format change", rec.Version, recordVersion)
	}
	if got := time.Duration(rec.Parked.BoundMs) * time.Millisecond; got != parkBound {
		t.Errorf("recorded bound = %s, want %s", got, parkBound)
	}
	if got := time.Duration(rec.Parked.IdleForMs) * time.Millisecond; got < parkBound {
		t.Errorf("recorded idle time = %s, want at least the bound %s", got, parkBound)
	}
	if rec.Parked.AtMs == 0 {
		t.Error("the record does not say when the session was parked")
	}
}

func TestParkRestart_AParkedSessionIsLeftParkedAndNothingIsStarted(t *testing.T) {
	sess, state := parkedThenStopped(t, fleet.SessionSpec{Name: "sleeper", Agent: "build"})

	d2, sink := restartWith(t, state, parkBound)
	known, ok := d2.wasSeen(sess.ID)
	if !ok {
		t.Fatal("the session is not known after the restart")
	}
	if got := d2.phaseOf(known.srv); got != phaseParked {
		t.Fatalf("phase after the restart = %d, want parked (%d)", got, phaseParked)
	}
	if known.srv.proc != nil {
		t.Error("a process was started for a session that was parked")
	}
	if n := liveCount(d2); n != 0 {
		t.Errorf("%d servers tracked after the restart, want none: nothing should have been started", n)
	}
	if !sink.has("left parked") {
		t.Error("the restart left no notice that it left the session parked")
	}

	st := parkState(t, d2, sess.SessionRef)
	if st.Status != fleet.StatusIdle || st.Confidence != fleet.ConfidenceObserved || !strings.HasPrefix(st.Evidence, "parked:") {
		t.Errorf("State = %s/%s %q, want idle, observed, parked evidence", st.Status, st.Confidence, st.Evidence)
	}
	if !strings.Contains(st.Evidence, "restarted") {
		t.Errorf("evidence %q does not say the service restarted and left it stopped", st.Evidence)
	}
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusIdle || !strings.HasPrefix(got.State.Evidence, "parked:") {
		t.Errorf("listing reads %+v (listed %v), want idle with parked evidence", got.State, ok)
	}
	if got.Name != "sleeper" || string(got.Agent) != "build" {
		t.Errorf("listed as %q / %q, want the name and agent it was created with", got.Name, got.Agent)
	}
	// Reads left it as it was, and the record still says why.
	if d2.phaseOf(known.srv) != phaseParked || liveCount(d2) != 0 {
		t.Error("a read started the session")
	}
	if rec := parkedRecordOf(t, state, sess.ID); rec.Parked == nil {
		t.Error("the record stopped saying parked although nothing started the session")
	}
	if got.Conversation == nil || got.Conversation.Known {
		t.Errorf("conversation = %+v, want unconfirmed while the runtime has not been asked since the restart", got.Conversation)
	}
}

func TestParkRestart_TheFirstInputStartsItAndClearsTheFact(t *testing.T) {
	sess, state := parkedThenStopped(t, fleet.SessionSpec{Name: "sleeper"})
	d2, sink := restartWith(t, state, parkBound)

	if _, err := sendText(t, d2, sess.SessionRef, "welcome back"); err != nil {
		t.Fatalf("Send to a session left parked: %v", err)
	}
	known, _ := d2.wasSeen(sess.ID)
	if got := d2.phaseOf(known.srv); got != phaseRunning {
		t.Fatalf("phase after the input = %d, want running", got)
	}
	if n := liveCount(d2); n != 1 {
		t.Errorf("%d servers tracked after the input, want the one that was started", n)
	}
	if !sink.has("started parked session " + sess.ID) {
		t.Error("the start left no notice")
	}
	if rec := parkedRecordOf(t, state, sess.ID); rec.Parked != nil {
		t.Errorf("the record still says parked after a process was started: %+v", rec.Parked)
	}
	if got := known.conversationRef(); got == nil || !got.Known || got.ID != sess.ID {
		t.Errorf("conversation after the start = %+v, want the session's id, confirmed by the read the start made", got)
	}
	st := parkState(t, d2, sess.SessionRef)
	if strings.HasPrefix(st.Evidence, "parked:") {
		t.Errorf("a running session still reads parked: %q", st.Evidence)
	}
}

func TestParkRestart_AStartedSessionIsWrittenParkedAgainByTheNextPark(t *testing.T) {
	d, clock, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "cycler"})
	parkSession(t, d, clock, sess.ID)
	if _, err := sendText(t, d, sess.SessionRef, "wake"); err != nil {
		t.Fatal(err)
	}
	if rec := parkedRecordOf(t, state, sess.ID); rec.Parked != nil {
		t.Fatalf("the record says parked while a process serves the session: %+v", rec.Parked)
	}
	parkSession(t, d, clock, sess.ID)
	if rec := parkedRecordOf(t, state, sess.ID); rec.Parked == nil {
		t.Error("the second park was not written to the record")
	}
}

func TestParkRestart_WhenTheBoundNoLongerHoldsTheSessionIsStarted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bound      time.Duration
		wantParked bool
	}{
		{"the same bound keeps it parked", parkBound, true},
		{"a higher bound keeps it parked", 3 * parkBound, true},
		{"a lower bound starts it", parkBound / 2, false},
		{"no bound starts it, or nothing would ever park or revive it under a lock", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, state := parkedThenStopped(t, fleet.SessionSpec{Name: "sleeper"})
			d2, _ := restartWith(t, state, tc.bound)

			known, ok := d2.wasSeen(sess.ID)
			if !ok {
				t.Fatal("the session is not known after the restart")
			}
			parked := d2.phaseOf(known.srv) == phaseParked
			if parked != tc.wantParked {
				t.Fatalf("parked after the restart = %v, want %v", parked, tc.wantParked)
			}
			if !tc.wantParked {
				if got := d2.phaseOf(known.srv); got != phaseRunning {
					t.Fatalf("phase = %d, want running: a session is never left stranded", got)
				}
				if rec := parkedRecordOf(t, state, sess.ID); rec.Parked != nil {
					t.Errorf("the record still says parked after the session was started: %+v", rec.Parked)
				}
			}
			// Either way the session takes input.
			if _, err := sendText(t, d2, sess.SessionRef, "hello"); err != nil {
				t.Errorf("Send: %v", err)
			}
		})
	}
}

func TestParkRestart_ARecordFromBeforeTheFieldExistedIsStarted(t *testing.T) {
	d, _, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "old"})
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(recordPath(state, sess.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "parked") {
		t.Errorf("a record of a session that was never parked mentions it: %s", raw)
	}

	d2, _ := restartWith(t, state, parkBound)
	known, _ := d2.wasSeen(sess.ID)
	if got := d2.phaseOf(known.srv); got != phaseRunning {
		t.Errorf("phase = %d, want running: with nothing recorded the session starts, as before", got)
	}
}

func recordPath(state, id string) string {
	dir, _ := stateLayout(state)
	return filepath.Join(dir, id+".json")
}

func TestParkRestart_ASessionThatCouldNotBeStartedIsNotLeftParkedBehindAnInputThatCannot(t *testing.T) {
	sess, state := parkedThenStopped(t, fleet.SessionSpec{Name: "orphaned"})
	rec := parkedRecordOf(t, state, sess.ID)
	if err := os.RemoveAll(rec.Dir); err != nil {
		t.Fatal(err)
	}

	d2, _ := restartWith(t, state, parkBound)
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatal("the session is not listed")
	}
	if got.State.Status != fleet.StatusUnknown || !strings.Contains(got.State.Evidence, "gone") {
		t.Errorf("a session whose directory is gone reads %s %q, want unknown with the reason, exactly as a restart always reported it",
			got.State.Status, got.State.Evidence)
	}
	if liveCount(d2) != 0 {
		t.Error("a server was started for a session with no directory")
	}
}

func TestParkRestart_ACloseOfASessionLeftParkedRemovesWhatItLeft(t *testing.T) {
	sess, state := parkedThenStopped(t, fleet.SessionSpec{Name: "closing"})
	rec := parkedRecordOf(t, state, sess.ID)
	d2, _ := restartWith(t, state, parkBound)

	if _, err := d2.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if left := recordFiles(t, state); len(left) != 0 {
		t.Errorf("records left after the close: %v", left)
	}
	if _, err := os.Stat(rec.Dir); err == nil {
		t.Error("the session's directory is still there after the close")
	}
	if _, ok := listed(t, d2)[sess.ID]; ok {
		t.Error("a closed session is still listed")
	}
}

func TestParkRestart_WhatTheParkKnewOfTheLastTurnSurvivesTheRestart(t *testing.T) {
	sess, state := parkedThenStopped(t, fleet.SessionSpec{Name: "failed-last"})
	dir, _ := stateLayout(state)
	store := &recordStore{dir: dir}
	rec := store.load(sess.ID)
	rec.Parked.LastTurn = &fleet.TurnEnd{Outcome: "failed", Reason: "the provider said the key is out of quota", Retryable: true}
	if err := store.write(rec); err != nil {
		t.Fatal(err)
	}

	d2, _ := restartWith(t, state, parkBound)
	st := parkState(t, d2, sess.SessionRef)
	if st.LastTurn == nil || st.LastTurn.Outcome != "failed" || !st.LastTurn.Retryable ||
		!strings.Contains(st.LastTurn.Reason, "quota") {
		t.Errorf("LastTurn after the restart = %+v, want the failure the park recorded", st.LastTurn)
	}
}

func TestParkRestart_TheSandboxAParkedSessionReportsIsTheOneItHadBefore(t *testing.T) {
	d, clock, _, state := newParkDriver(t)
	requireSandboxHost(t, d)
	sess, _ := sandboxedSession(t, d, "k-sbx", fleet.SandboxSpec{Network: fleet.SandboxNetworkClosed})
	before := parkState(t, d, sess.SessionRef)
	if before.Sandbox == nil {
		t.Fatal("a sandboxed session reports no sandbox before the park")
	}
	parkSession(t, d, clock, sess.ID)
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2, _ := restartWith(t, state, parkBound)
	known, _ := d2.wasSeen(sess.ID)
	if got := d2.phaseOf(known.srv); got != phaseParked {
		t.Fatalf("phase after the restart = %d, want parked", got)
	}
	after := parkState(t, d2, sess.SessionRef)
	if !reflect.DeepEqual(after.Sandbox, before.Sandbox) {
		t.Errorf("sandbox after the restart = %+v, want %+v: a session left parked still states how it is confined", after.Sandbox, before.Sandbox)
	}
}

func TestParkRestart_AParkThatCannotBeRecordedStillParks(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop this user from writing")
	}
	d, clock, sink, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "diskfull"})
	dir, _ := stateLayout(state)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	parkSession(t, d, clock, sess.ID) // fails the test if the session is not parked
	if !sink.has("could not record it") {
		t.Error("a park whose record could not be written said nothing about it")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if rec := parkedRecordOf(t, state, sess.ID); rec.Parked != nil {
		t.Errorf("the record says parked although the write failed: %+v", rec.Parked)
	}
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// Nothing recorded, so the next service starts it, as every service did.
	d2, _ := restartWith(t, state, parkBound)
	known, _ := d2.wasSeen(sess.ID)
	if got := d2.phaseOf(known.srv); got != phaseRunning {
		t.Errorf("phase after a restart with no recorded park = %d, want running", got)
	}
}
