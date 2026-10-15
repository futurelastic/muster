package tmux

import (
	"context"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster#322: state.prompt reported a question and options that were not the
// modal on screen. The parser took its question from the rows above the FIRST
// numbered row in the window and merged every numbered row after it by index,
// so a numbered run above the live dialog — an agent's own list, an answered
// dialog still in the history margin — supplied the question and the leading
// options, and the live menu only overwrote them where it happened to be
// longer. Sessions whose transcripts carried the same text then reported the
// same question and the same nonce while each was held on a different dialog.
//
// The rule these tests pin: the prompt is the LAST menu on screen. A numbered
// row that restarts at 1 after options were read begins a new menu, and
// nothing of the earlier run — its question, its options, its header —
// survives into it.

const staleList = "  Here is what I will do:\n" +
	"  1. Create the issue for the stale prompt\n" +
	"  2. Wait for the review\n" +
	"  3. Merge once CI is green\n" +
	"  4. Tear the worktree down\n"

const liveDialog = "  Do you want to proceed?\n" +
	"  ❯ 1. Yes\n" +
	"    2. Yes, and don't ask again for this command\n" +
	"    3. No\n\n" +
	"  Esc to cancel · Tab to amend"

func TestALiveDialogIsNotMergedWithANumberedListAboveIt(t *testing.T) {
	p := parsePrompt(newScreen(staleList + "\n" + liveDialog))
	if p == nil {
		t.Fatal("the live dialog was not recognised as a prompt")
	}
	if p.Question != "Do you want to proceed?" {
		t.Errorf("question = %q, want the live dialog's own", p.Question)
	}
	want := []string{"Yes", "Yes, and don't ask again for this command", "No"}
	if len(p.Options) != len(want) {
		t.Fatalf("options = %q, want %q", p.Options, want)
	}
	for i := range want {
		if p.Options[i] != want[i] {
			t.Errorf("option %d = %q, want %q", i+1, p.Options[i], want[i])
		}
	}
	if p.Selected != 1 {
		t.Errorf("selected = %d, want 1", p.Selected)
	}
}

// The same dialog under different history is one prompt, and so one nonce: a
// reader must not see the list above it in what it is told is being asked.
func TestTheNonceOfALiveDialogDoesNotDependOnAListAboveIt(t *testing.T) {
	bare := parsePrompt(newScreen(liveDialog))
	over := parsePrompt(newScreen(staleList + "\n" + liveDialog))
	if bare == nil || over == nil {
		t.Fatalf("prompts = %+v, %+v; want both recognised", bare, over)
	}
	if bare.Nonce != over.Nonce {
		t.Errorf("nonce differs with a list above the dialog: %s vs %s", bare.Nonce, over.Nonce)
	}
}

// Two sessions held on different dialogs must not read as one question. They
// share a nonce only when they share both the question and every option — the
// nonce is a digest of what is asked, by design.
func TestDifferentDialogsUnderTheSameHistoryHaveDistinctNonces(t *testing.T) {
	a := parsePrompt(newScreen(staleList + "\n" + liveDialog))
	other := "  Allow this edit to src/main.go?\n" +
		"  ❯ 1. Yes\n" +
		"    2. Yes, allow all edits this session\n" +
		"    3. No\n\n" +
		"  Esc to cancel · Tab to amend"
	b := parsePrompt(newScreen(staleList + "\n" + other))
	if a == nil || b == nil {
		t.Fatalf("prompts = %+v, %+v; want both recognised", a, b)
	}
	if a.Nonce == b.Nonce {
		t.Errorf("two different dialogs share nonce %s", a.Nonce)
	}
	if b.Question != "Allow this edit to src/main.go?" {
		t.Errorf("question = %q, want the live dialog's own", b.Question)
	}
}

// A list ABOVE a menu that has a footer and no highlight is still ignored once
// a later menu is on screen; and a menu on its own, or one whose options carry
// descriptions between them, must read exactly as it did.
func TestASingleMenuStillReadsWhole(t *testing.T) {
	p := parsePrompt(newScreen(liveDialog))
	if p == nil {
		t.Fatal("not recognised as a prompt at all")
	}
	if len(p.Options) != 3 || p.Selected != 1 || p.Question != "Do you want to proceed?" {
		t.Errorf("prompt = %+v", p)
	}
}

// A nonce that names no live prompt is refused with a reason, never answered
// submitted. The nonce quoted here is the one the screen reported before the
// session moved on to a different dialog (muster#322's third case).
func TestRespondWithAStaleNonceIsRefusedNotSubmitted(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = staleList + "\n" + liveDialog
	d := newTestDriver(f)
	quoted := parsePrompt(newScreen(f.captures["%1"]))
	if quoted == nil {
		t.Fatal("fixture is not a recognised prompt")
	}

	// The session moves on: a different dialog replaces the one that was quoted.
	f.captures["%1"] = staleList + "\n" +
		"  Allow this edit to src/main.go?\n" +
		"  ❯ 1. Yes\n    2. Yes, allow all edits this session\n    3. No\n\n" +
		"  Esc to cancel · Tab to amend"

	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.Response{Choice: 1, Nonce: quoted.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("respond = %q (%s), want refused", got.Outcome, got.Reason)
	}
	if got.Reason == "" {
		t.Errorf("a refusal carries a reason")
	}
	if len(sendCalls(f)) != 0 {
		t.Errorf("keys were sent for a stale nonce: %v", sendCalls(f))
	}
}
