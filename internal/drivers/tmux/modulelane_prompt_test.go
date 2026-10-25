package tmux

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
)

// #368: the create-time prompt follows a live delivery lane, and every send says
// which path it took and why. The pin to the terminal that #184 gave the launch
// prompt is gone for the first attempt; what it protected — a retry that cannot
// deliver the text twice — is kept by the module ledger and by a retry that stays
// on the terminal.

// seedPromptRecord is what Create itself does for a session launched with a
// prompt, so a test can drive deliverInitialPrompt and read its outcome.
func (r *modRig) seedPromptRecord(id, cwd, prompt string) {
	r.t.Helper()
	r.d.noteCreateRecord(id, cwd, fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Prompt: prompt})
}

func (r *modRig) promptRecord(id, cwd string) createRecord {
	r.t.Helper()
	rec, ok := r.d.createRecordFor(id, cwd)
	if !ok {
		r.t.Fatalf("no create record for %s", id)
	}
	return rec
}

// The point of the change: a lane that is live when the prompt is sent carries
// it, the terminal is not touched, and the log line says so by session.
func TestInitialPrompt_LiveLaneCarriesIt(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	const prompt = "start on the issue"
	r.seedPromptRecord(id, "/work/lane1", prompt)
	logs := captureLog(t)

	r.d.deliverInitialPrompt(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, prompt)

	if r.sends() != 1 || r.pastes() != 0 {
		t.Fatalf("module sends = %d, pastes = %d; want the lane to carry it (1 and 0)", r.sends(), r.pastes())
	}
	if got := r.lastSendText(); got != prompt {
		t.Errorf("the lane was given %q, want the prompt unlabelled", got)
	}
	if r.counter("route.decided.auto.module") != 1 {
		t.Errorf("route counters: %v", r.d.Counters())
	}
	rec := r.promptRecord(id, "/work/lane1")
	if rec.PromptOutcome != string(fleet.OutcomeQueued) {
		t.Errorf("PromptOutcome = %q, want queued (the lane confirmed the runtime took it)", rec.PromptOutcome)
	}
	want := "tmux: send session=" + id + " machine=testbox requested=auto route=module/" + modName + " outcome=queued fallback=false"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("the log does not carry %q:\n%s", want, logs.String())
	}
	if strings.Contains(logs.String(), prompt) {
		t.Errorf("the log carries the message text:\n%s", logs.String())
	}
}

// A lane that is not up at send time is the only reason the terminal carries the
// prompt, and the log names the session and the lane's own words for it.
func TestInitialPrompt_LaneNotUpFallsBackToTheTerminalAndSaysWhy(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	const prompt = "start on the issue"
	r.seedPromptRecord(id, "/work/lane1", prompt)
	r.d.mods.degrade(id, "the runtime has not connected yet", false)
	logs := captureLog(t)

	r.d.deliverInitialPrompt(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, prompt)

	if r.sends() != 0 || r.pastes() != 1 {
		t.Fatalf("module sends = %d, pastes = %d; want the terminal to carry it (0 and 1)", r.sends(), r.pastes())
	}
	if r.modCounter("fallback_to_builtin") != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	line := firstLineContaining(logs.String(), "tmux: send session="+id)
	if line == "" {
		t.Fatalf("no per-send line names the session:\n%s", logs.String())
	}
	for _, want := range []string{"requested=auto", "route=terminal", "fallback=true", "declined and wrote nothing", "the runtime has not connected yet"} {
		if !strings.Contains(line, want) {
			t.Errorf("the fallback line lacks %q:\n%s", want, line)
		}
	}
	if rec := r.promptRecord(id, "/work/lane1"); rec.PromptOutcome == string(fleet.OutcomeUnknown) {
		t.Errorf("PromptOutcome = unknown on a prompt the terminal delivered: %+v", rec)
	}
}

// The lane wrote the prompt and could not confirm it. Nothing is in the composer
// to resume and the ledger holds the text, so there is no retry: not on the lane,
// not on the terminal. The prompt resolves unknown with the lane's own reason.
func TestInitialPrompt_LaneUnknownIsNeverRetried(t *testing.T) {
	silent := modtest.JSON(map[string]any{"verdict": "silent", "enqueued": false, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"confirm": {Results: []json.RawMessage{silent}, DelayMs: 60},
	}})
	const prompt = "the one that must not be typed a second time"
	r.seedPromptRecord(id, "/work/lane1", prompt)
	ctx, cancel := short(t)
	defer cancel()
	logs := captureLog(t)

	r.d.deliverInitialPrompt(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, prompt)

	if r.sends() != 1 || r.pastes() != 0 {
		t.Fatalf("module sends = %d, pastes = %d; want one write and nothing typed (1 and 0)", r.sends(), r.pastes())
	}
	if r.counter(counterInitialPromptRetried) != 0 || r.counter(counterInitialPromptStranded) != 0 {
		t.Errorf("a lane unknown was retried or called stranded: %v", r.d.Counters())
	}
	if r.counter(counterInitialPromptLaneUnconfirmed) != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	rec := r.promptRecord(id, "/work/lane1")
	if rec.PromptOutcome != string(fleet.OutcomeUnknown) || !strings.Contains(rec.PromptEvidence, "NOT been sent again") {
		t.Errorf("record = %+v, want unknown carrying the lane's reason", rec)
	}
	if !strings.Contains(logs.String(), "initial prompt written to delivery module") {
		t.Errorf("no log line for the unconfirmed prompt:\n%s", logs.String())
	}
	// And the retry this function declined to make would have been held anyway:
	// the ledger answers the same text from the same sender without writing, so
	// skipping it loses nothing and states the answer once.
	again, err := r.d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, prompt,
		driver.SendOptions{Submit: true, ResumeIfStranded: true, Route: fleet.RouteTerminal})
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != fleet.OutcomeUnknown || again.ModuleOf() != modName || r.sends() != 1 || r.pastes() != 0 {
		t.Errorf("the retry wrote again: receipt = %+v, sends = %d, pastes = %d", again, r.sends(), r.pastes())
	}
}

// The terminal's own strand is still retried once, and the retry stays on the
// terminal: it is a resume, so no lane is offered it even if the lane has come
// back since, and it can only finish what the terminal's stranded record holds.
func TestInitialPrompt_TerminalStrandRetryStaysOnTheTerminal(t *testing.T) {
	mux := twoSessions()
	r := newModRig(t, rigOptions{mux: mux, driverOpts: []Option{withExec(swallowFirstSubmitOnly(mux))}})
	sess := r.create("lane1", nil)
	r.waitLive(sess.ID)
	const prompt = "a launch prompt long enough to strand on a busy machine"
	r.seedPromptRecord(sess.ID, "/work/lane1", prompt)
	r.d.mods.degrade(sess.ID, "the runtime has not connected yet", false)
	logs := captureLog(t)

	r.d.deliverInitialPrompt(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: sess.ID}, prompt)

	if r.sends() != 0 {
		t.Errorf("the lane saw %d sends; neither attempt may reach it", r.sends())
	}
	if r.counter(counterInitialPromptRetried) != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	if r.counter("route.decided.auto.terminal") != 1 || r.counter("route.decided.terminal.terminal") != 1 {
		t.Errorf("want the first attempt as auto and the retry pinned to terminal: %v", r.d.Counters())
	}
	rec := r.promptRecord(sess.ID, "/work/lane1")
	if rec.PromptOutcome != string(fleet.OutcomeQueued) || !strings.Contains(rec.PromptEvidence, "second attempt") {
		t.Errorf("record = %+v, want queued on the second attempt", rec)
	}
	if n := strings.Count(logs.String(), "tmux: send session="+sess.ID); n != 2 {
		t.Errorf("%d per-send lines for two sends:\n%s", n, logs.String())
	}
}

// Every send is logged against its session: a path that never had a lane, a
// shape no lane can take, and a refusal that took no path at all.
func TestLogSend_NamesTheSessionTheRouteAndTheReason(t *testing.T) {
	d := newTestDriver(twoSessions())
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	logs := captureLog(t)

	if _, err := d.Send(context.Background(), testCaller, ref, "hello", driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Send(context.Background(), testCaller, ref, "!id", driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var sends []string
	for _, l := range lines {
		if strings.HasPrefix(l, "tmux: send session=") {
			sends = append(sends, l)
		}
	}
	if len(sends) != 2 {
		t.Fatalf("want 2 per-send lines, got %d:\n%s", len(sends), logs.String())
	}
	for _, want := range []string{"session=alpha💬", "machine=testbox", "requested=auto", "route=terminal", "outcome=queued", "fallback=false", "no delivery module is enabled"} {
		if !strings.Contains(sends[0], want) {
			t.Errorf("first line lacks %q:\n%s", want, sends[0])
		}
	}
	for _, want := range []string{"session=alpha💬", "route=none", "outcome=refused", "no path was taken", "detail="} {
		if !strings.Contains(sends[1], want) {
			t.Errorf("refusal line lacks %q:\n%s", want, sends[1])
		}
	}
	if strings.Contains(logs.String(), "hello") {
		t.Errorf("the log carries message text:\n%s", logs.String())
	}
}

func firstLineContaining(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}
