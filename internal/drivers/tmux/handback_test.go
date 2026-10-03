package tmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

// #240: a delivery confirmed by evidence that does not show a turn started —
// the runtime queueing the text, or the composer reading empty — used to leave
// no memory behind. When the runtime then handed the text back to the composer,
// the session sat unsent, the sender had been told `queued`, and the driver
// refused every attempt to finish it for want of a record. These tests drive
// that sequence end to end against the fake multiplexer.

const handbackText = "release is green, please merge when you are ready"

var handbackFrom = &fleet.MessageFrom{Agent: "agent-x", Machine: "entrybox"}

// handBack puts the labelled delivery back in the composer, unsent, the way a
// runtime that returned a queued message does.
func handBack(f *fakeMux, text string, from *fleet.MessageFrom) {
	f.setCapture("%1", composerHoldingRows(strings.Split(paneLabelled(text, from), "\n")))
}

func stateOf(t *testing.T, d *Driver, ref fleet.SessionRef) fleet.SessionState {
	t.Helper()
	st, err := d.State(context.Background(), testCaller, ref)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The oracle. Screen-only confirmation (no transcript is configured): the
// composer empties, the receipt is queued, then the text comes back.
func TestHandedBackQueuedDeliveryIsKnownAsOurOwnAndResumable(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	sent, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Outcome != fleet.OutcomeQueued {
		t.Fatalf("setup: outcome = %s (%s), want queued", sent.Outcome, sent.Reason)
	}
	if !strings.Contains(sent.Reason, "strandedDelivery") {
		t.Errorf("reason = %q: a confirmation that shows no turn started must say what the sender will see if the text is handed back", sent.Reason)
	}
	if got := d.counters.Snapshot()[counterStrandedProvisionalKept]; got != 1 {
		t.Fatalf("provisional_kept = %d, want 1", got)
	}

	handBack(f, handbackText, handbackFrom)

	st := stateOf(t, d, ref)
	if st.WaitingOn != fleet.WaitingUnsentInput {
		t.Fatalf("setup: waitingOn = %q (%s), want unsent-input", st.WaitingOn, st.Evidence)
	}
	if !st.StrandedDelivery {
		t.Fatal("the unsent text is a message this driver delivered and reported queued; state must say so")
	}

	// A different message is still refused, and the refusal now says whose text is there.
	other, err := d.Send(context.Background(), testCaller, ref, "something else",
		driver.SendOptions{Submit: true, From: handbackFrom})
	if err != nil {
		t.Fatal(err)
	}
	if other.Outcome != fleet.OutcomeRefused || !strings.Contains(other.Reason, "handed it back") {
		t.Fatalf("different text: outcome = %s (%s), want a refusal naming the hand-back", other.Outcome, other.Reason)
	}

	// The sender's own resume finishes it, with no digest and no screen reading.
	resumed, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume: outcome = %s (%s), want queued — the sender must be able to finish a handed-back delivery", resumed.Outcome, resumed.Reason)
	}
	if got := d.counters.Snapshot()[counterStrandedProvisionalHandedBack]; got < 1 {
		t.Errorf("provisional_handed_back = %d, want at least 1", got)
	}
}

// §2.4 still holds: a composer holding anything other than the delivered text
// is not ours, whatever the driver remembers.
func TestHandedBackProofIsTheTextNotTheSession(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	if _, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom}); err != nil {
		t.Fatal(err)
	}

	f.setCapture("%1", composerHoldingRows([]string{"a draft a person is typing"}))
	st := stateOf(t, d, ref)
	if st.WaitingOn != fleet.WaitingUnsentInput {
		t.Fatalf("setup: waitingOn = %q (%s)", st.WaitingOn, st.Evidence)
	}
	if st.StrandedDelivery {
		t.Fatal("a person's draft was reported as this driver's own delivery")
	}
	got, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("resume over a person's draft: outcome = %s (%s), want refused", got.Outcome, got.Reason)
	}
}

// With a transcript: the runtime queued the text (an enqueue — not proof a
// turn started), later returned it (a popAll), and the memory survives a
// restart of the service.
func TestHandedBackAfterAnEnqueueSurvivesARestart(t *testing.T) {
	recordRoot := t.TempDir()
	cwd := "/work/alpha"
	sessionName := "alpha💬"
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	convPath := filepath.Join(convDir, "conv-1.jsonl")
	if err := os.WriteFile(convPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": sessionName, "sessionId": "conv-1",
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	labelled := paneLabelled(handbackText, handbackFrom)
	dir := t.TempDir()
	f := twoSessions()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	build := func() *Driver {
		return New("testbox",
			withExec(appendOnSubmit(t, f.exec, convPath, mustJSONLine(t, map[string]any{
				"type": "queue-operation", "operation": "enqueue", "sessionId": "conv-1",
				"timestamp": time.Now().Format(time.RFC3339Nano), "content": labelled,
			}))),
			withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Now() }),
			WithRecordRoot(recordRoot),
			WithState(st),
		)
	}
	first := build()
	ref := fleet.SessionRef{Machine: "testbox", ID: sessionName}
	sent, err := first.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Outcome != fleet.OutcomeQueued {
		t.Fatalf("setup: outcome = %s (%s)", sent.Outcome, sent.Reason)
	}
	snap := first.counters.Snapshot()
	if snap[counterSubmitConfirmedByEnqueue] != 1 || snap[counterStrandedProvisionalKept] != 1 {
		t.Fatalf("by_enqueue = %d, provisional_kept = %d, want 1 and 1", snap[counterSubmitConfirmedByEnqueue], snap[counterStrandedProvisionalKept])
	}

	appendLine(t, convPath, `{"type":"queue-operation","operation":"popAll","sessionId":"conv-1"}`)
	handBack(f, handbackText, handbackFrom)

	second := build() // a restart: new process, same state directory
	got := stateOf(t, second, ref)
	if got.WaitingOn != fleet.WaitingUnsentInput || !got.StrandedDelivery {
		t.Fatalf("after a restart: waitingOn = %q, strandedDelivery = %v (%s)", got.WaitingOn, got.StrandedDelivery, got.Evidence)
	}

	// Had the runtime started a turn on the text, what is in the composer now is
	// the same words again — nothing says whose.
	appendLine(t, convPath, mustJSONLine(t, map[string]any{
		"type": "user", "sessionId": "conv-1",
		"message": map[string]any{"role": "user", "content": labelled},
	}))
	got = stateOf(t, second, ref)
	if got.StrandedDelivery {
		t.Fatal("the runtime ran this text after the delivery; a copy in the composer is not proven to be ours")
	}
}

// #249, re-cut after #257: the delivery-module lane keeps no provisional record,
// and that is deliberate. A `queued` receipt on this lane follows only a
// module verdict of `confirmed` (a user-origin turn the runtime accepted, the
// terminal path's turn-proven case, where #240 keeps no record either); a bare
// enqueue is answered `unknown`, never `queued`. And on a live lane a resume is
// refused before any record would be read, so a record would have no consumer.
// This test pins those three facts so that adding a record here later has to
// confront the reasons in docs/adr/240-a-queued-delivery-is-not-gone.md.
func TestModuleLaneKeepsNoProvisionalRecord(t *testing.T) {
	const text = "release is green, please merge when you are ready"

	t.Run("confirmed is a started turn, so no record", func(t *testing.T) {
		r, id := liveRig(t, modtest.Behaviour{})
		got := r.sendLive(id, text, driver.SendOptions{Submit: true})
		if got.Outcome != fleet.OutcomeQueued || got.ModuleOf() != modName {
			t.Fatalf("receipt = %+v, want queued naming the module", got)
		}
		if n := r.counter(counterStrandedProvisionalKept); n != 0 {
			t.Errorf("provisional_kept = %d, want 0: confirmed means a turn started", n)
		}

		// The runtime hands the text back; nothing remembers it as ours.
		r.mux.setCapture("%"+intToStr(r.pids), composerHoldingRows(strings.Split(paneLabelled(text, agentFrom), "\n")))
		st, err := r.d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if st.StrandedDelivery {
			t.Error("state claims a module-lane delivery as this driver's own stranded text; no record was kept")
		}

		// And resume is refused on the live lane with nothing written, so a record
		// would have had no consumer.
		sends, pastes := r.sends(), r.pastes()
		res := r.sendLive(id, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
		if res.Outcome != fleet.OutcomeRefused || !strings.Contains(res.Reason, "live") {
			t.Fatalf("resume on a live lane: receipt = %+v, want refused naming the live lane", res)
		}
		if r.sends() != sends || r.pastes() != pastes {
			t.Errorf("sends %d -> %d, pastes %d -> %d: a refused resume wrote something", sends, r.sends(), pastes, r.pastes())
		}
	})

	t.Run("a bare enqueue is unknown, never queued, and keeps no record", func(t *testing.T) {
		queued := modtest.JSON(map[string]any{"verdict": "queued", "enqueued": true, "elapsedMs": 5, "final": false})
		r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
			"confirm": {Results: []json.RawMessage{queued}, DelayMs: 60},
		}})
		ctx, cancel := short(t)
		defer cancel()
		o := driver.SendOptions{Submit: true, LiveLaneOnly: true, From: agentFrom}
		got, err := r.d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, text, o)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != fleet.OutcomeUnknown || got.ModuleOf() != modName {
			t.Fatalf("receipt = %+v, want unknown naming the module", got)
		}
		if n := r.counter(counterStrandedProvisionalKept); n != 0 {
			t.Errorf("provisional_kept = %d, want 0: the module lane reports an enqueue as unknown, not queued", n)
		}
	})
}
