package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
)

// #411. A /quit sent to a session ends the session, and both delivery paths used
// to read the silence that followed as "the submit did not register" / "could not
// be confirmed ... NOT been sent": a receipt that says the text is sitting there
// and advises a retry, for a session that no longer exists, plus a stranded
// record kept for it. The receipt now says the session ended, with its exit
// status when it is known, and keeps nothing.

// exitOnSubmit wraps a fake so that the submit keystroke (the send-keys call
// carrying C-m) is the moment the session's process exits: the keystroke is not
// applied (the composer keeps showing the text, as it does when a runtime quits
// instead of taking a turn) and onExit decides what the multiplexer then shows.
// fail makes that keystroke report an error too, as tmux does for a pane that
// went away under it.
func exitOnSubmit(f *fakeMux, fail bool, onExit func()) execFunc {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "send-keys" {
			for _, a := range args {
				if a == "C-m" {
					onExit()
					if fail {
						return nil, errors.New("tmux: send-keys: can't find pane")
					}
					return nil, nil
				}
			}
		}
		return f.exec(ctx, name, args...)
	}
}

func (f *fakeMux) die(name string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.sessions {
		if f.sessions[i].name == name {
			f.sessions[i].dead = true
			f.sessions[i].deadStatus = status
		}
	}
}

func endedDriver(f *fakeMux, ex execFunc) *Driver {
	return New("testbox", withExec(ex), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
}

var endedRef = fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

// assertEndedReceipt is everything a caller is owed for an ended session.
func assertEndedReceipt(t *testing.T, d *Driver, got fleet.DeliveryReceipt, err error, wantInReason string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Send returned an error for a session that ended: %v", err)
	}
	if got.Outcome != fleet.OutcomeEnded {
		t.Fatalf("outcome = %q (%s), want ended", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, wantInReason) {
		t.Errorf("reason = %q, want it to say %q", got.Reason, wantInReason)
	}
	for _, bad := range []string{"resumeIfStranded", "unsent", "sitting there", "retry the same send"} {
		if strings.Contains(got.Reason, bad) {
			t.Errorf("reason advises a retry on a session that ended (%q): %s", bad, got.Reason)
		}
	}
	if rec, ok := d.strandedRecordByID(endedRef.ID); ok {
		t.Errorf("a stranded record was kept for a session that ended: %+v", rec)
	}
	if d.terminalUnconfirmed(endedRef.ID, "/quit") {
		t.Error("terminalUnconfirmed = true for a session that ended")
	}
	c := d.Counters()
	if c["delivery.tmux.ended"] != 1 || c["delivery.tmux.outcome.ended"] != 1 || c[counterSendEnded] != 1 {
		t.Errorf("counters: ended=%d outcome.ended=%d %s=%d, want 1 each (%v)",
			c["delivery.tmux.ended"], c["delivery.tmux.outcome.ended"], counterSendEnded, c[counterSendEnded], c)
	}
	if c["delivery.tmux.unclassified"] != 0 {
		t.Errorf("an ended send went unclassified: %v", c)
	}
}

// The measured case: the pane's process exits with status 0 in the confirmation
// window and remain-on-exit leaves the dead pane standing.
func TestSendToASessionThatExitsInTheWindowAnswersEndedWithTheStatus(t *testing.T) {
	f := twoSessions()
	d := endedDriver(f, exitOnSubmit(f, false, func() { f.die("alpha💬", 0) }))

	got, err := d.Send(context.Background(), testCaller, endedRef, "/quit", driver.SendOptions{Submit: true, HumanRelay: true})
	assertEndedReceipt(t, d, got, err, "exit status 0")
}

// A non-zero status is reported as it is, not flattened.
func TestEndedReceiptCarriesANonZeroStatus(t *testing.T) {
	f := twoSessions()
	d := endedDriver(f, exitOnSubmit(f, false, func() { f.die("alpha💬", 137) }))

	got, err := d.Send(context.Background(), testCaller, endedRef, "/quit", driver.SendOptions{Submit: true, HumanRelay: true})
	assertEndedReceipt(t, d, got, err, "exit status 137")
}

// Usually the pane is already gone when the failed confirmation looks: a List
// reaped it, and the service drained the exit into its history within the same
// poll. The driver's own memory of the exit is what still knows the status.
func TestEndedReceiptReadsTheStatusOfAnAlreadyReapedSession(t *testing.T) {
	f := twoSessions()
	var d *Driver
	d = endedDriver(f, exitOnSubmit(f, false, func() {
		f.dropSession("alpha💬")
		d.notePendingExit("alpha💬", fleet.SessionExit{Status: 3})
		d.DrainExits() // the service took it
	}))

	got, err := d.Send(context.Background(), testCaller, endedRef, "/quit", driver.SendOptions{Submit: true, HumanRelay: true})
	assertEndedReceipt(t, d, got, err, "exit status 3")
}

// Gone with no exit on record (the session was closed, or the multiplexer
// restarted): still ended, still no retry advice, and no status invented.
func TestEndedReceiptWithNoStatusOnRecordSaysSo(t *testing.T) {
	f := twoSessions()
	d := endedDriver(f, exitOnSubmit(f, false, func() { f.dropSession("alpha💬") }))

	got, err := d.Send(context.Background(), testCaller, endedRef, "/quit", driver.SendOptions{Submit: true, HumanRelay: true})
	assertEndedReceipt(t, d, got, err, "no exit status was recorded")
	if strings.Contains(got.Reason, "exit status 0") {
		t.Errorf("a status was invented for an exit nobody recorded: %s", got.Reason)
	}
}

// The submit keystroke itself failing because the pane went away is the same
// fact, and used to return an error and a stranded record.
func TestEndedReceiptWhenTheSubmitKeystrokeFailsBecauseThePaneIsGone(t *testing.T) {
	f := twoSessions()
	d := endedDriver(f, exitOnSubmit(f, true, func() { f.die("alpha💬", 0) }))

	got, err := d.Send(context.Background(), testCaller, endedRef, "/quit", driver.SendOptions{Submit: true, HumanRelay: true})
	assertEndedReceipt(t, d, got, err, "exit status 0")
}

// The guard on all of the above: a confirmation that fails on a session that is
// still alive stays exactly what it was — unknown, with the stranded record and
// the resume advice. Ended is for an END, not for any silence.
func TestAConfirmationThatFailsOnALiveSessionStaysUnknown(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true
	d := newTestDriver(f)

	got, err := d.Send(context.Background(), testCaller, endedRef, "an instruction", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "resumeIfStranded") {
		t.Fatalf("receipt = %+v, want the unchanged unknown with the resume advice", got)
	}
	if _, ok := d.strandedRecordByID(endedRef.ID); !ok {
		t.Error("the stranded record for a live session was not kept")
	}
	if n := d.Counters()[counterSendEnded]; n != 0 {
		t.Errorf("%s = %d for a live session", counterSendEnded, n)
	}
}

// A listing that cannot be read is not evidence of an end.
func TestAnUnreadableListingIsNotAnEnd(t *testing.T) {
	f := twoSessions()
	d := endedDriver(f, exitOnSubmit(f, false, func() { f.setFailList(true) }))

	got, err := d.Send(context.Background(), testCaller, endedRef, "an instruction", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeEnded {
		t.Fatalf("a failed read was reported as an end: %+v", got)
	}
}

// The exit memory is a short, bounded one.
func TestRecentExitMemoryIsBoundedAndExpires(t *testing.T) {
	now := time.Unix(1785760000, 0)
	d := New("testbox", withClock(func() time.Time { return now }))

	for i := 0; i < recentExitsCap+20; i++ {
		d.notePendingExit("s"+intToStr(i), fleet.SessionExit{Status: i})
		now = now.Add(time.Second)
	}
	d.DrainExits()
	d.exitedMu.Lock()
	n := len(d.recentExits)
	d.exitedMu.Unlock()
	if n > recentExitsCap {
		t.Fatalf("memory holds %d exits, cap is %d", n, recentExitsCap)
	}
	if _, ok := d.recentExit("s0"); ok {
		t.Error("the oldest exit survived past the cap")
	}
	last := "s" + intToStr(recentExitsCap+19)
	if e, ok := d.recentExit(last); !ok || e.Status != recentExitsCap+19 {
		t.Errorf("the newest exit = %+v, %v", e, ok)
	}

	now = now.Add(recentExitsTTL + time.Second)
	if _, ok := d.recentExit(last); ok {
		t.Error("an exit outlived its TTL")
	}
}

// --- the module lane --------------------------------------------------------

// A lane's confirmation that never comes because the session's process exited:
// ended, not "could not be confirmed ... NOT been sent". The lane is not
// degraded (it did nothing wrong), and no ledger entry holds the text, so a
// later send of the same text is answered by what is true — there is no such
// session — and not by a hold that points at a transcript nobody can read.
func TestModuleSend_ASessionThatEndedAnswersEndedNotUnknown(t *testing.T) {
	for name, confirm := range map[string]map[string]any{
		"silent": {"verdict": "silent", "enqueued": false, "elapsedMs": 5, "final": false},
		"queued": {"verdict": "queued", "enqueued": true, "elapsedMs": 5, "final": false},
	} {
		t.Run(name, func(t *testing.T) {
			raw := modtest.JSON(confirm)
			r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
				"confirm": {Results: []json.RawMessage{raw}, DelayMs: 60},
			}})
			r.mux.die(id, 0)

			ctx, cancel := short(t)
			defer cancel()
			got := r.sendCtx(ctx, id, "/quit", driver.SendOptions{HumanRelay: true})
			if got.Outcome != fleet.OutcomeEnded || got.ModuleOf() != modName {
				t.Fatalf("receipt = %+v, want ended naming the module", got)
			}
			if !strings.Contains(got.Reason, "exit status 0") || strings.Contains(got.Reason, "NOT been sent") {
				t.Errorf("reason = %q", got.Reason)
			}
			if v := r.view(id); !v.ClientConnected {
				t.Errorf("an ended session degraded the lane: %+v", v)
			}
			if c := r.d.Counters(); c["delivery."+modName+".ended"] != 1 || c["delivery."+modName+".unclassified"] != 0 {
				t.Errorf("counters = %v", c)
			}
			if r.sends() != 1 || r.pastes() != 0 {
				t.Errorf("module sends = %d, pastes = %d", r.sends(), r.pastes())
			}
			if r.d.terminalUnconfirmed(id, "/quit") {
				t.Error("an ended session left an unconfirmed record")
			}
		})
	}
}

// And a live session whose lane stays silent is exactly as before.
func TestModuleSend_ALiveSessionThatStaysSilentIsStillUnknown(t *testing.T) {
	silent := modtest.JSON(map[string]any{"verdict": "silent", "enqueued": false, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"confirm": {Results: []json.RawMessage{silent}, DelayMs: 60},
	}})
	ctx, cancel := short(t)
	defer cancel()
	got := r.sendCtx(ctx, id, "hello", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("receipt = %+v", got)
	}
}
