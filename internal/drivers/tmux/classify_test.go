package tmux

import (
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// The fixtures below are real pane captures taken from a machine running 22
// concurrent sessions, trimmed and with content redacted but with every
// structural feature preserved verbatim — the rule widths, the composer
// fencing, the spinner forms, the exact footer strings. Invented fixtures
// would test the classifier against the author's belief about the TUI
// rather than against the TUI.

const rule = "────────────────────────────────────────────────────────────────────────────────"

// working: spinner in its running form, composer empty.
const fixtureWorking = `  ⎿  ◻ Build first working tmux driver
     ◻ Probe tmux control mode for real push events
✻ Zigzagging… (5m 57s · ↓ 21.3k tokens)
` + rule + `
❯
` + rule + `
  ▸ Opus 5 · muster                                                   /rc
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

// idle: spinner in its finished form, composer empty.
const fixtureIdle = `  Done — the userscript hides both columns and centres the timeline.
✻ Brewed for 8m 21s
` + rule + `
❯
` + rule + `
  ▸ Opus 5 · agents
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

// idle, but with a footer NOTICE below the composer that shares a running
// spinner's shape (muster#229): a single symbol, a space, a capitalised
// word, and a trailing ellipsis where the runtime truncated the notice. A
// scan that does not stop at the composer's closing rule reads this line
// before the real, finished spinner above the composer, and reports a
// session that is actually idle as `working` — a misread that does not clear
// on its own, because nothing here repaints until the session's next turn.
const fixtureIdleWithTranscriptWarning = `  Done — the userscript hides both columns and centres the timeline.
✻ Brewed for 8m 21s
` + rule + `
❯
` + rule + `
  ▸ Opus 5 · agents
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents
  ⚠ Transcript writes are failing (disk full — ENOSPC) · recent messages may …`

// waiting_input via unsent composer text: turn finished, human has typed
// but not submitted. This is the §2.4 hazard case.
const fixtureUnsent = `  Want me to fold the composer-text hazard into the skill?
✻ Worked for 2m 7s
` + rule + `
❯ yes, update the skill
` + rule + `
  ▸ Opus 5 (1M context) · Claude
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

// waiting_input via a blocking menu.
const fixtureMenu = `  3. Third option, summarised
     A sentence of explanation under the option label.
  4. Type something.
` + rule + `
  5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

// A pane whose transcript contains ❯-prefixed lines that are history, not
// live input — the TUI echoes previously-run commands this way. The live
// composer below them is empty. A classifier that matched "a line starting
// with ❯" instead of the fenced composer would read "/remote-control" as
// unsent input and refuse every send to this session forever.
const fixtureEchoedHistory = `❯ /remote-control
  ⎿  Remote Control disconnected.
❯ /remote-control
❯ /remote-control
` + rule + `
❯
` + rule + `
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

func TestClassifyDeadNeedsNoScreen(t *testing.T) {
	got := classify(fixtureWorking, false)
	if got.Status != fleet.StatusDead {
		t.Fatalf("a pane with no live process must be dead, got %q", got.Status)
	}
	if got.Confidence != fleet.ConfidenceInferred {
		t.Errorf("this driver never observes; got confidence %q", got.Confidence)
	}
}

func TestClassifyStatuses(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    fleet.Status
	}{
		{"running spinner", fixtureWorking, fleet.StatusWorking},
		{"finished spinner, empty composer", fixtureIdle, fleet.StatusIdle},
		{"finished spinner, empty composer, ENOSPC footer notice (#229)", fixtureIdleWithTranscriptWarning, fleet.StatusIdle},
		{"finished spinner, unsent composer text", fixtureUnsent, fleet.StatusWaitingInput},
		{"blocking selection menu", fixtureMenu, fleet.StatusWaitingInput},
		{"echoed history above an empty composer", fixtureEchoedHistory, fleet.StatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.fixture, true)
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q (evidence: %s)", got.Status, tc.want, got.Evidence)
			}
			if got.Confidence != fleet.ConfidenceInferred {
				t.Errorf("confidence = %q, want inferred: this driver reads a screen, "+
					"it never observes a structured status (§5.6)", got.Confidence)
			}
			if strings.TrimSpace(got.Evidence) == "" {
				t.Error("every state must carry evidence (§2.3); this one is empty")
			}
		})
	}
}

// The composer is the input to the §2.4 refusal decision, so it gets its
// own tests: a false positive here refuses legitimate sends forever, and a
// false negative corrupts a human's half-typed message.
func TestComposerTextDistinguishesLiveInputFromTranscript(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		wantText string
		wantOK   bool
	}{
		{"empty composer", fixtureIdle, "", true},
		{"unsent text", fixtureUnsent, "yes, update the skill", true},
		{"echoed history is not live input", fixtureEchoedHistory, "", true},
		{"menu has no composer", fixtureMenu, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, scan := composerText(newScreen(tc.fixture))
			ok := scan == composerFound
			if ok != tc.wantOK {
				t.Fatalf("found = %v (scan=%v), want %v", ok, scan, tc.wantOK)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
		})
	}
}

// An agent can print anything to its own transcript, including a convincing
// forgery of the TUI's chrome. The classifier reads only the tail below the
// final rule, so transcript text cannot masquerade as the live composer.
func TestForgedChromeInTranscriptDoesNotBecomeTheComposer(t *testing.T) {
	forged := rule + "\n❯ rm -rf something the human never typed\n" + rule + "\n" + fixtureIdle
	text, scan := composerText(newScreen(forged))
	if scan != composerFound {
		t.Fatalf("composer should still be found, got scan=%v", scan)
	}
	if text != "" {
		t.Errorf("transcript forgery leaked into composer text: %q", text)
	}
}

// The spinner's verb is randomised by the TUI. Any classifier that pattern
// matches on specific verbs is wrong; this test pins the property that only
// the shape of the suffix is load-bearing.
func TestSpinnerVerbIsNotLoadBearing(t *testing.T) {
	for _, verb := range []string{"Zigzagging", "Brewed", "Sautéed", "Percolating", "Noodling"} {
		running, found := spinner(newScreen("✻ " + verb + "… (1m 2s · ↓ 3.1k tokens)"))
		if !found || !running {
			t.Errorf("verb %q: running spinner not recognised (found=%v running=%v)", verb, found, running)
		}
		running, found = spinner(newScreen("✻ " + verb + " for 1m 2s"))
		if !found || running {
			t.Errorf("verb %q: finished spinner not recognised (found=%v running=%v)", verb, found, running)
		}
	}
}

// muster#229: a footer notice below the composer's closing rule must
// never win the backward scan over the real spinner line drawn above the
// composer, even though the notice matches statusLine's shape byte for byte
// (symbol, space, capitalised word, ellipsis).
func TestSpinnerIgnoresFooterNoticeBelowComposer(t *testing.T) {
	running, found := spinner(newScreen(fixtureIdleWithTranscriptWarning))
	if !found {
		t.Fatal("the finished spinner above the composer must still be found")
	}
	if running {
		t.Error("a footer notice below the composer must not be read as a running spinner")
	}
}

// A spinner line in a shape this driver does not recognise must report "no
// usable spinner" rather than defaulting to finished — a wrong "idle" for a
// session that is actually working is the exact silent failure §5.6 exists
// to prevent.
func TestUnrecognisedSpinnerShapeIsNotTreatedAsFinished(t *testing.T) {
	_, found := spinner(newScreen("✻ something in a shape from a future release"))
	if found {
		t.Error("an unparseable spinner must not be reported as a usable reading")
	}
	got := classify("✻ something in a shape from a future release\n"+rule+"\n❯ \n"+rule, true)
	if got.Status != fleet.StatusUnknown {
		t.Errorf("status = %q, want unknown when the spinner shape is unrecognised", got.Status)
	}
}

// Rules carry a centred session-name label. Width varies with the terminal.
func TestIsRuleToleratesLabelsAndWidths(t *testing.T) {
	if !isRule("──────────────── continue-exploring📋 ──") {
		t.Error("a labelled rule must still be a rule")
	}
	if !isRule(strings.Repeat("─", 200)) {
		t.Error("a wide rule must be a rule")
	}
	if isRule("  the design — as discussed — is settled") {
		t.Error("prose containing a dash is not a rule")
	}
	if isRule("") {
		t.Error("empty line is not a rule")
	}
}

// --- extreme cases -------------------------------------------------------
//
// Every one of these was either observed on a live machine or is a direct
// neighbour of one that was. The failure mode they guard against is specific:
// a composer detector that is wrong in one direction refuses every send to a
// session forever (the session looks stuck for no visible reason), and wrong
// in the other direction concatenates into a message a human was still typing.

// The one that was live. A selection menu marks its highlighted option with
// the same glyph as the composer prompt, and sits above a rule — so it is a
// composer to anything that does not require the OPENING fence too.
const fixtureMenuSelected = `  Which name should it use?

❯ 1. the-first-option (Recommended)
     An explanatory line under the option.
  2. the-second-option
     Another explanation.
  4. Type something.
` + rule + `
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

func TestSelectedMenuItemIsNotTreatedAsUnsentInput(t *testing.T) {
	text, scan := composerText(newScreen(fixtureMenuSelected))
	if scan == composerFound || text != "" {
		t.Fatalf("menu selection read as composer input (%q, scan=%v); every send to "+
			"this session would be refused forever, for text nobody typed", text, scan)
	}
	// Not merely "not found" — composerAbsent specifically. The question
	// line sitting right above the highlighted option is real, non-blank
	// content this driver DID see, which is what settles "definitely a
	// menu" rather than "could not tell" (muster#134's clipped/absent
	// split — see composerSpan's own doc comment on ranOffTop).
	if scan != composerAbsent {
		t.Errorf("scan = %v, want composerAbsent: a preceding question line was in view, "+
			"this is not an ambiguous/clipped read", scan)
	}
	// It is still blocked on a human, which is a different statement.
	if got := classify(fixtureMenuSelected, true); got.Status != fleet.StatusWaitingInput {
		t.Errorf("status = %q, want waiting_input", got.Status)
	}
}

// A long message wraps below the prompt and is still unsent. Reading only the
// first line under-reports it — the direction that corrupts a message.
func TestWrappedComposerCapturesEveryLine(t *testing.T) {
	f := rule + "\n❯ this is a long message that the human\n  wrapped onto a second line and a\n  third\n" + rule
	text, scan := composerText(newScreen(f))
	if scan != composerFound {
		t.Fatalf("fenced composer not found, scan=%v", scan)
	}
	for _, want := range []string{"long message", "second line", "third"} {
		if !strings.Contains(text, want) {
			t.Errorf("wrapped composer lost %q: got %q", want, text)
		}
	}
}

// muster#129: clearComposer sizes its press budget to how many rows a
// composer occupies on screen, not to a duration — composerVisualLines is
// where that count comes from, and it must agree with composerText about
// which screens hold a composer at all (same composerSpan underneath).
func TestComposerVisualLinesCountsOnScreenRows(t *testing.T) {
	wrapped := rule + "\n❯ this is a long message that the human\n  wrapped onto a second line and a\n  third\n" + rule
	cases := []struct {
		name      string
		fixture   string
		wantLines int
		wantOK    bool
	}{
		{"empty composer, one row", fixtureIdle, 1, true},
		{"one-line unsent text, one row", fixtureUnsent, 1, true},
		{"prompt plus two continuation rows", wrapped, 3, true},
		{"no composer at all", fixtureMenu, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, scan := composerVisualLines(newScreen(tc.fixture))
			ok := scan == composerFound
			if ok != tc.wantOK {
				t.Fatalf("found = %v (scan=%v), want %v", ok, scan, tc.wantOK)
			}
			if ok && got != tc.wantLines {
				t.Errorf("lines = %d, want %d", got, tc.wantLines)
			}
			// composerSpan is shared: whatever composerText finds a composer
			// in, this must find one in too, and vice versa — a caller that
			// already has composerText's "found" answer for this screen
			// relies on that (see this function's own doc comment).
			if _, textScan := composerText(newScreen(tc.fixture)); textScan != scan {
				t.Errorf("composerText scan=%v but composerVisualLines scan=%v for the "+
					"same screen; the two must agree, they share composerSpan", textScan, scan)
			}
		})
	}
}

// A labelled opening fence in a narrow pane leaves few rule characters. If
// that is not recognised as a rule, the composer is not fenced and unsent
// input goes undetected.
func TestNarrowPaneWithLongSessionNameStillFences(t *testing.T) {
	narrow := "── a-very-long-session-name-here ──"
	if !isRule(narrow) {
		t.Fatal("labelled fence in a narrow pane not recognised; unsent input would go undetected")
	}
	f := narrow + "\n❯ half-typed thought\n" + narrow
	text, scan := composerText(newScreen(f))
	if scan != composerFound || text != "half-typed thought" {
		t.Errorf("composer = %q scan=%v, want the typed text", text, scan)
	}
}

// ...and prose must still not be mistaken for a fence.
func TestProseIsNotAFence(t *testing.T) {
	for _, line := range []string{
		"the design — as discussed — is settled",
		"a-b-c-d-e-f-g-h",
		"",
		"   ",
		"1. option — with an em dash",
	} {
		if isRule(line) {
			t.Errorf("prose treated as a rule: %q", line)
		}
	}
}

// Whitespace-only input is not input. Refusing on it would stick a session on
// a stray space.
func TestWhitespaceOnlyComposerIsEmpty(t *testing.T) {
	f := rule + "\n❯      \n" + rule
	text, scan := composerText(newScreen(f))
	if scan != composerFound {
		t.Fatalf("composer not found, scan=%v", scan)
	}
	if text != "" {
		t.Errorf("composer = %q, want empty; a stray space must not block a session", text)
	}
}

// A pane that is not the TUI at all yields no composer — and therefore no
// refusal on composer grounds, which is correct: we know nothing about it.
func TestNonTUIPaneYieldsNoComposer(t *testing.T) {
	for _, f := range []string{
		"$ ls -la\ntotal 8\ndrwxr-xr-x  2 user  staff",
		"",
		"\n\n\n",
		"❯ a bare prompt with no fences at all",
	} {
		if _, scan := composerText(newScreen(f)); scan != composerAbsent {
			t.Errorf("claimed a composer (or an ambiguous read) in non-TUI output: %q, scan=%v", f, scan)
		}
	}
}

// A fresh session that has not painted its chrome yet must not be read as
// holding input.
func TestUnpaintedSessionHoldsNothing(t *testing.T) {
	if _, scan := composerText(newScreen("\n\n")); scan != composerAbsent {
		t.Errorf("unpainted pane reported a composer (or an ambiguous read), scan=%v", scan)
	}
	if got := classify("", true); got.Status != fleet.StatusUnknown {
		t.Errorf("status = %q, want unknown for an empty screen", got.Status)
	}
}

// --- the two ways a session stops being reachable -------------------------
//
// Both were live, and both are how a fleet loses a session to a dialog nobody
// can reach.

// A brand-new session shows a dim placeholder hint in its composer. Read as
// typed input, it refuses every send forever — to a session that has never
// been spoken to at all.
func TestDimPlaceholderIsNotTypedInput(t *testing.T) {
	// Exactly as captured from a live newly-created session.
	raw := rule + "\n\x1b[39m❯ \x1b[2mTry\x1b[0m \x1b[2m\"how\x1b[0m \x1b[2mdo\x1b[0m \x1b[2mI\x1b[0m \x1b[2mlog\x1b[0m \x1b[2man\x1b[0m \x1b[2merror?\"\x1b[0m\n" + rule
	text, scan := composerText(newScreen(raw))
	if scan != composerFound {
		t.Fatalf("composer not found, scan=%v", scan)
	}
	if text != "" {
		t.Errorf("placeholder read as pending input (%q); every send to a fresh "+
			"session would be refused for text nobody typed", text)
	}
}

// ...while real typed input is normal intensity and must still be protected.
func TestNormalIntensityInputIsStillProtected(t *testing.T) {
	raw := rule + "\n\x1b[39m❯ a half-typed thought\n" + rule
	text, scan := composerText(newScreen(raw))
	if scan != composerFound || text != "a half-typed thought" {
		t.Errorf("composer = %q scan=%v; real input must not be mistaken for a placeholder", text, scan)
	}
}

// Both menu footers block. Knowing only one classified a folder-trust prompt
// and a resume prompt as unknown, which reads as "cannot determine" rather
// than "blocked on a human" — so a supervisor waits forever.
func TestBothMenuFootersAreRecognised(t *testing.T) {
	for _, footer := range []string{
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
		"Enter to confirm · Esc to cancel",
	} {
		f := "  Some question?\n❯ 1. Yes, the recommended one\n  2. No\n" + footer
		option, blocked := selectionPrompt(newScreen(f))
		if !blocked {
			t.Errorf("footer %q not recognised as blocking", footer)
			continue
		}
		if option != "1. Yes, the recommended one" {
			t.Errorf("highlighted option = %q; a supervisor deciding whether to accept "+
				"the default needs to know what the default is", option)
		}
		if got := classify(f, true); got.Status != fleet.StatusWaitingInput {
			t.Errorf("status = %q, want waiting_input", got.Status)
		}
	}
}

// The evidence must name what is being asked, not merely that something is.
func TestBlockedStateNamesTheHighlightedOption(t *testing.T) {
	f := "  Resume?\n❯ 1. Resume from summary (recommended)\n  2. Full\nEnter to confirm · Esc to cancel"
	got := classify(f, true)
	if !strings.Contains(got.Evidence, "Resume from summary") {
		t.Errorf("evidence = %q; it should name the option a caller would be accepting", got.Evidence)
	}
}

// --- #424's requirements: enumerate, version, verify -----------------------

// The two boot prompts observed on one fleet put the SAFE option at different
// indices. A caller that accepted the highlighted default would proceed in one
// case and kill the session in the other — which is why the options must be
// enumerated rather than described.
const fixtureTrustPrompt = `  Quick safety check: Is this a project you created or one you trust?
❯ 1. Yes, I trust this folder
  2. No, continue without these permissions
Enter to confirm · Esc to cancel`

const fixtureBypassPrompt = `  By proceeding, you accept all responsibility for actions taken.
❯ 1. No, exit
  2. Yes, I accept
Enter to confirm · Esc to cancel`

func TestPromptOptionsAreEnumeratedInOrder(t *testing.T) {
	p := parsePrompt(newScreen(fixtureTrustPrompt))
	if p == nil {
		t.Fatal("no prompt parsed")
	}
	if len(p.Options) != 2 {
		t.Fatalf("options = %v, want two", p.Options)
	}
	if p.Options[0] != "Yes, I trust this folder" {
		t.Errorf("option 1 = %q", p.Options[0])
	}
	if p.Selected != 1 {
		t.Errorf("selected = %d, want 1", p.Selected)
	}
	if p.Question == "" {
		t.Error("question is empty; a caller has to show the human what is being asked")
	}
}

// The whole reason enumeration matters.
func TestSafeOptionIsAtADifferentIndexBetweenPrompts(t *testing.T) {
	trust := parsePrompt(newScreen(fixtureTrustPrompt))
	bypass := parsePrompt(newScreen(fixtureBypassPrompt))
	if trust == nil || bypass == nil {
		t.Fatal("both prompts must parse")
	}
	if trust.Selected != 1 || bypass.Selected != 1 {
		t.Fatal("fixtures both highlight option 1")
	}
	// Same highlighted index, opposite meanings.
	if trust.Options[0] == bypass.Options[0] {
		t.Fatal("fixtures drifted")
	}
	if !strings.HasPrefix(bypass.Options[0], "No, exit") {
		t.Errorf("bypass option 1 = %q; accepting the default here EXITS the session",
			bypass.Options[0])
	}
}

// A nonce must change when the question changes, and be stable when it does not.
func TestNonceTracksTheQuestion(t *testing.T) {
	a := parsePrompt(newScreen(fixtureTrustPrompt))
	again := parsePrompt(newScreen(fixtureTrustPrompt))
	b := parsePrompt(newScreen(fixtureBypassPrompt))
	if a.Nonce != again.Nonce {
		t.Error("nonce changed for an unchanged prompt; every answer would be refused")
	}
	if a.Nonce == b.Nonce {
		t.Error("two different prompts share a nonce; a stale answer would be applied " +
			"to a question the caller never saw")
	}
}

// Options are found by their numbering, not their wording, so a prompt from a
// future release enumerates without a new matcher.
func TestUnknownPromptStillEnumerates(t *testing.T) {
	f := `  Some question nobody has written a matcher for
❯ 1. First
  2. Second
  3. Third
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`
	p := parsePrompt(newScreen(f))
	if p == nil || len(p.Options) != 3 {
		t.Fatalf("parsed = %+v; enumeration must not depend on recognising the wording", p)
	}
}

// §8's `starting`: a booting session is not an unreadable one. Conflating them
// is why a spawn that never got past a boot screen "read as healthy".
func TestYoungPaneWithNoInterfaceIsStartingNotUnknown(t *testing.T) {
	booting := "  loading...\n"
	if got := classifyAged(booting, true, true); got.Status != fleet.StatusStarting {
		t.Errorf("young pane = %q, want starting", got.Status)
	}
	old := classifyAged(booting, true, false)
	if old.Status != fleet.StatusUnknown {
		t.Errorf("old pane = %q, want unknown — age is the discriminator", old.Status)
	}
	// muster #64: composer absence has (at least) two real causes — a
	// pane running the wrong thing, and a runtime showing a full-screen
	// interface with no composer of its own (a control-channel dialog,
	// measured directly). The evidence must not assert only the first as if
	// it were established; that is what let a healthy dialog read as a
	// broken pane.
	if !strings.Contains(old.Evidence, "full-screen") {
		t.Errorf("evidence %q does not mention the full-screen-interface possibility (#64)", old.Evidence)
	}
	if !strings.Contains(old.Evidence, "may not be running the expected runtime") {
		t.Errorf("evidence %q dropped the wrong-runtime possibility, which is still real", old.Evidence)
	}
}

// The pane is written by an agent that can print anything, so anything parsed
// out of it is untrusted input. Padding a slice up to a parsed index is
// unbounded allocation: one transcript line hung the live service.
func TestPromptParsingIsBoundedByHostileInput(t *testing.T) {
	// The hostile rows are transcript, so they stand above the dialog. Under a
	// footer a numbered run is believed only when it is the block directly
	// above it (muster#382): the same rows between the options and the footer
	// would be a row the dialog does not own, and the screen would read as no
	// menu. This test is about bounded work, not about where the rows sit.
	hostile := `  1000000. absurd index from the transcript
  99999999999999999999. and an overflowing one
  A question
❯ 1. Real option
  2. Another
Enter to confirm · Esc to cancel`
	done := make(chan *fleet.SessionPrompt, 1)
	go func() { done <- parsePrompt(newScreen(hostile)) }()
	select {
	case p := <-done:
		if p == nil {
			t.Fatal("prompt should still parse")
		}
		if len(p.Options) > 32 {
			t.Errorf("options = %d; a menu with more options than this is not a menu", len(p.Options))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parsing did not terminate — unbounded work driven by screen content")
	}
}

// A fourth footer, from a tool-permission dialog: neither "Enter to select"
// nor "Enter to confirm". Footer-matching was always going to lose this race —
// detection is structural first.
const fixturePermissionDialog = `⏺ Write(REMOTE_TEST.md)
 Create file
 REMOTE_TEST.md
  1 written by Alex on the other machine
 Do you want to create REMOTE_TEST.md?
❯ 1. Yes
  2. Yes, allow all edits in this directory during this session
  3. No
 Esc to cancel · Tab to amend`

func TestPermissionDialogIsDetectedWithoutAKnownFooter(t *testing.T) {
	p := parsePrompt(newScreen(fixturePermissionDialog))
	if p == nil {
		t.Fatal("tool-permission dialog not detected; a session blocked on it would " +
			"read as unreadable and nobody would answer it")
	}
	if len(p.Options) != 3 || p.Selected != 1 {
		t.Errorf("parsed %d options, selected %d; want 3 and 1", len(p.Options), p.Selected)
	}
	if got := classify(fixturePermissionDialog, true); got.Status != fleet.StatusWaitingInput {
		t.Errorf("status = %q, want waiting_input", got.Status)
	}
}

// ...and the case structure alone would miss: a menu whose highlighted option
// has scrolled above the captured window. The footer still carries it.
func TestMenuWithScrolledOffMarkerStillCounts(t *testing.T) {
	f := "  4. Type something.\n  5. Chat about this\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel"
	if _, blocked := selectionPrompt(newScreen(f)); !blocked {
		t.Error("a menu whose marker scrolled out of view must still read as blocked")
	}
}

// A numbered list in a transcript is not a question.
func TestTranscriptListIsNotAPrompt(t *testing.T) {
	f := `  Here is what I found:
  1. the first thing
  2. the second thing
  3. the third thing

` + rule + `
❯
` + rule
	if p := parsePrompt(newScreen(f)); p != nil {
		t.Errorf("transcript list read as a prompt: %+v", p.Options)
	}
}

// A runtime footer says that a dialog stands somewhere on the screen; it does not
// say that the numbered run found in the window is that dialog (muster#382). #380
// closed the instance the unnumbered reader could read. These are the dialogs it
// cannot — a wrapped label, blank rows between options, a title row — drawn under
// a transcript that holds a numbered list: the list used to come back as the
// dialog's options, and an answer by index went to a question nobody asked.
func TestFooterDoesNotLicenseANumberedRunThatIsNotTheBlockAboveIt(t *testing.T) {
	const footer = "   Enter to select · Esc to cancel"
	const transcript = "Here are the steps I would take:\n\n  1. read the config\n  2. change the port\n  3. restart the service\n\n"
	cases := map[string]string{
		"an unnumbered dialog with a wrapped label": transcript + ` Pick a deployment target

   ❯ Staging environment, which is shared by
       every team and is reset nightly
     Production environment
     Local only

` + footer,
		"an unnumbered dialog with blank rows between its options": transcript + ` Pick a deployment target

   ❯ Staging environment

     Production environment

     Local only

` + footer,
		"the list echoed with the highlight leader": "❯ 1. read the config\n  2. change the port\n  3. restart the service\n\n" + ` Pick a deployment target

   ❯ Staging environment

     Production environment

` + footer,
		"a title row between the list and the footer": transcript + " Pick a deployment target\n" + footer,
		"a run that begins under the footer":          footer + "\n\n  1. read the config\n  2. change the port\n",
	}
	for name, screen := range cases {
		t.Run(name, func(t *testing.T) {
			if p := parsePrompt(newScreen(screen)); p != nil {
				t.Fatalf("a numbered run that is not directly above the footer was read as the dialog: options = %q", p.Options)
			}
			// Not idle either: the dialog replaces the composer, and a session
			// with a footer on screen and nothing to answer is held, not sent to.
			if st := classify(screen, true); st.Status == fleet.StatusIdle || st.Prompt != nil {
				t.Fatalf("status %s prompt %+v, want neither idle nor an answerable prompt", st.Status, st.Prompt)
			}
		})
	}
}

// ...and everything that may stand between a numbered run and its footer still
// does: blank rows, a rule, a dashed divider, the options' own descriptions and a
// label that wraps. The rule above refuses a row the run does not own; it must
// not refuse the chrome a real dialog draws, because the cost of a refusal is a
// session that cannot be answered.
func TestFooterStillLicensesTheRunDirectlyAboveItWhateverChromeBetween(t *testing.T) {
	const footer = "Enter to select · ↑/↓ to navigate · Esc to cancel"
	cases := map[string]struct {
		screen  string
		options int
	}{
		"blank rows":                     {"Proceed?\n❯ 1. Yes\n  2. No\n\n\n" + footer, 2},
		"a rule":                         {"Proceed?\n❯ 1. Yes\n  2. No\n" + rule + "\n" + footer, 2},
		"a divider":                      {"Proceed?\n❯ 1. Yes\n  2. No\n╌╌╌╌╌╌╌╌\n" + footer, 2},
		"descriptions under the options": {"Proceed?\n\n❯ 1. Yes\n     Do it now\n  2. No\n     Leave it\n\n" + footer, 2},
		"a wrapped label":                {"Proceed?\n\n❯ 1. Yes, and keep going until\n     the work is done\n  2. No\n\n" + footer, 2},
		"the permission dialog":          {fixturePermissionDialog, 3},
		"a transcript list above a numbered dialog": {
			"  1. the first thing\n  2. the second thing\n\nProceed?\n❯ 1. Yes\n  2. No\n\n" + footer, 2},
		"a footer left in the history above a menu": {
			footer + "\n\nProceed?\n❯ 1. Yes\n  2. No", 2},
		"a scrolled-off marker": {"  4. Type something.\n  5. Chat about this\n" + footer, 5},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := parsePrompt(newScreen(c.screen))
			if p == nil {
				t.Fatalf("a run directly above its footer was not read as a menu:\n%s", c.screen)
			}
			if len(p.Options) != c.options {
				t.Fatalf("options = %q, want %d", p.Options, c.options)
			}
		})
	}
}

// The measured incident, #58: ordinary pane text coincidentally matched the
// structural shape parsePrompt looks for — a numbered line marked as if
// selected — with no earlier index ever seen. The loop that enumerates
// options pads a gap like that with "" to keep indices honest (see its own
// comment), and before this fix the pad was itself read back as a real,
// empty option: no real menu offers an option with no label, and that is
// exactly the tell #58 recorded (options[0] == "").
//
// The consequence measured live: `Send` refused every message to a healthy
// session because it believed — confidently, `kind` unset and no footer in
// sight — that it was looking at a menu.
func TestGappedHighlightIsNotAPrompt(t *testing.T) {
	f := "  some assistant output\n❯ 2. a line that only looks like an option\n" +
		rule + "\n❯\n" + rule
	if p := parsePrompt(newScreen(f)); p != nil {
		t.Errorf("gapped structural match read as a prompt: %+v", p.Options)
	}
	if _, blocked := selectionPrompt(newScreen(f)); blocked {
		t.Error("Send would have refused delivery to a session that is not " +
			"actually showing a menu (#58)")
	}
}

// #58's companion clause, demonstrated on a match that is NOT malformed —
// contrast TestGappedHighlightIsNotAPrompt, which is the one specific gap
// shape that wedged a session. This is the general rule #58 forced onto
// #54: a structural prompt match with no footer to corroborate it, whose
// kind classifyPromptKind does not recognise, is the classifier's own
// admission that it does not know what it is looking at — and that
// admission must survive into the verdict rather than being discarded
// before it forms, however well-formed the parse otherwise is.
//
// This is also #54's own "measurement first" ask, made concrete: a case
// where a second source — here, the screen held stable, the same
// screen-history source the other two ambiguities already fold in —
// corrects what a single read would have reported with unearned confidence.
func TestUnrecognisedStructuralPromptRequiresCorroboration(t *testing.T) {
	const screen = "  A question nobody has written a matcher for\n" +
		"❯ 1. Do the risky thing\n" +
		"  2. Do the safe thing"

	t0 := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

	// First sighting: nothing to corroborate against yet, so this is held to
	// unknown even though nothing about the parse is malformed.
	first, digest := classifyPaneRemembering(screen, true, true, false, paneMemory{}, t0)
	if first.Status != fleet.StatusUnknown {
		t.Fatalf("first sighting = %s, want unknown — unrecognised evidence "+
			"cannot corroborate itself (evidence: %s)", first.Status, first.Evidence)
	}
	if first.Prompt != nil {
		t.Error("an uncorroborated candidate must not carry the prompt it has not yet earned")
	}

	// Same screen, later: nothing has moved, which coincidentally-matching
	// transcript text would not manage.
	prior := paneMemory{known: true, digest: digest, at: t0}
	second, _ := classifyPaneRemembering(screen, true, true, false, prior, t0.Add(30*time.Second))
	if second.Status != fleet.StatusWaitingInput {
		t.Errorf("unchanged unrecognised prompt after 30s = %s, want waiting_input (%s)",
			second.Status, second.Evidence)
	}
	if second.WaitingOn != fleet.WaitingPrompt {
		t.Errorf("waitingOn = %q, want %q", second.WaitingOn, fleet.WaitingPrompt)
	}
	if second.Prompt == nil || len(second.Prompt.Options) != 2 {
		t.Errorf("corroborated prompt = %+v, want the full candidate carried through", second.Prompt)
	}

	// Too soon: within the paint grace, still unrecognised evidence.
	tooSoon, _ := classifyPaneRemembering(screen, true, true, false, prior, t0.Add(time.Second))
	if tooSoon.Status != fleet.StatusUnknown {
		t.Errorf("within the paint grace = %s, want unknown", tooSoon.Status)
	}

	// Changed screen: never promoted. A read that never repeats is exactly
	// the shape ordinary, moving transcript text produces — the same "only
	// ever narrows toward less activity" discipline resolveAmbiguity already
	// applies to the other two ambiguities.
	changed, _ := classifyPaneRemembering(screen+"\nmore output", true, true, false, prior, t0.Add(30*time.Second))
	if changed.Status != fleet.StatusUnknown {
		t.Errorf("changed screen = %s, want unknown — an unrecognised prompt is "+
			"never promoted from a screen that has since moved on", changed.Status)
	}
}

// The largest source of `unknown` in a real fleet was one screen shape: no
// spinner, composer painted and empty. It is genuinely ambiguous from a single
// capture — a fresh prompt and a turn that has not painted yet look identical
// — and it accounted for 10 of 91 sessions across two machines.
//
// A second look settles it, and these tests pin which way each case resolves.
func TestResolveAmbiguityUsesTheSecondLook(t *testing.T) {
	const idleScreen = "some transcript\n" +
		"────────────────────\n" +
		"❯ \x1b[2mTry \"fix the tests\"\x1b[0m\n" +
		"────────────────────\n"

	t0 := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)

	// First sighting: nothing to compare against, so the honest answer is
	// still unknown. This is the floor the change must not lower.
	first, digest := classifyPaneRemembering(idleScreen, true, true, false, paneMemory{}, t0)
	if first.Status != fleet.StatusUnknown {
		t.Fatalf("first sighting = %s, want unknown — one capture cannot settle this", first.Status)
	}
	if digest == "" {
		t.Fatal("a successful capture must yield a digest to compare against next time")
	}

	// Same screen, later: a turn that had just begun would have painted a
	// spinner by now.
	prior := paneMemory{known: true, digest: digest, at: t0}
	second, _ := classifyPaneRemembering(idleScreen, true, true, false, prior, t0.Add(30*time.Second))
	if second.Status != fleet.StatusIdle {
		t.Errorf("unchanged screen after 30s = %s, want idle (%s)", second.Status, second.Evidence)
	}
	if second.Confidence != fleet.ConfidenceInferred {
		t.Error("resolution is still an inference, not an observation")
	}

	// Too soon: within the paint grace, the ambiguity is real.
	tooSoon, _ := classifyPaneRemembering(idleScreen, true, true, false, prior, t0.Add(time.Second))
	if tooSoon.Status != fleet.StatusUnknown {
		t.Errorf("within the paint grace = %s, want unknown", tooSoon.Status)
	}

	// Changed screen: deliberately NOT resolved to working. Content moves for
	// reasons other than a turn, and guessing the busy direction is how a
	// caller interrupts a session that was doing nothing.
	changed, _ := classifyPaneRemembering(idleScreen+"more output\n", true, true, false, prior, t0.Add(30*time.Second))
	if changed.Status != fleet.StatusUnknown {
		t.Errorf("changed screen = %s, want unknown — resolution only ever goes toward less activity", changed.Status)
	}
}

// #76: a promotion through resolveAmbiguity used to rebuild the returned
// state via fleet.InferredState, which sets only Status, Confidence,
// Evidence and Since — silently dropping everything else the candidate
// carried. Caught landing a real corpus case (#65): a session's genuinely
// active control channel read as absent a few seconds later, once the
// no-spinner-empty-composer promotion this test drives had run. Pinned
// here at the unit level too, on a minimal fixture, so this is not the
// corpus case's job alone to guard.
func TestResolveAmbiguityCarriesTheControlChannelThroughThePromotion(t *testing.T) {
	const idleScreen = "some transcript\n" +
		"────────────────────\n" +
		"❯ \x1b[2mTry \"fix the tests\"\x1b[0m\n" +
		"────────────────────\n" +
		"  ⏵⏵ auto mode on   /rc active"

	t0 := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	first, digest := classifyPaneRemembering(idleScreen, true, true, false, paneMemory{}, t0)
	if first.ControlChannel == nil || first.ControlChannel.State != fleet.ControlChannelActive {
		t.Fatalf("setup: first sighting's own channel = %+v, want active", first.ControlChannel)
	}

	prior := paneMemory{known: true, digest: digest, at: t0}
	second, _ := classifyPaneRemembering(idleScreen, true, true, false, prior, t0.Add(30*time.Second))
	if second.Status != fleet.StatusIdle {
		t.Fatalf("setup: second look = %s, want idle — the promotion must actually fire for "+
			"this test to be testing anything", second.Status)
	}
	if second.ControlChannel == nil || second.ControlChannel.State != fleet.ControlChannelActive {
		t.Errorf("control channel = %+v after promotion to idle, want active — the same "+
			"channel the first sighting reported. A promotion that rebuilds the state from "+
			"scratch reports every channel as absent a few seconds after every sighting, "+
			"which reads exactly like a healthy session either way", second.ControlChannel)
	}
}

// Unsent text on a screen that has stopped moving is the case a sibling
// project measured at 37 of 39 panes: blocked on a human pressing enter, with
// the age as the discriminator between mid-thought and abandoned.
func TestResolveAmbiguityForStrandedInput(t *testing.T) {
	const pendingScreen = "transcript\n" +
		"────────────────────\n" +
		"❯ please refactor the parser\n" +
		"────────────────────\n"

	t0 := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	first, digest := classifyPaneRemembering(pendingScreen, true, true, false, paneMemory{}, t0)
	if first.Status != fleet.StatusUnknown {
		t.Fatalf("first sighting = %s, want unknown", first.Status)
	}
	prior := paneMemory{known: true, digest: digest, at: t0}
	second, _ := classifyPaneRemembering(pendingScreen, true, true, false, prior, t0.Add(time.Minute))
	if second.Status != fleet.StatusWaitingInput {
		t.Errorf("stable screen with unsent input = %s, want waiting_input (%s)", second.Status, second.Evidence)
	}
}

// A failed capture must not produce a digest: two failures in a row would
// otherwise compare equal and be read as a stable screen — a driver
// malfunction laundered into an observation about the session (§5.7, F5).
func TestFailedCaptureYieldsNoDigest(t *testing.T) {
	st, digest := classifyPaneRemembering("", false, true, false, paneMemory{}, time.Now())
	if st.Status != fleet.StatusUnknown {
		t.Errorf("failed capture = %s, want unknown", st.Status)
	}
	if digest != "" {
		t.Error("a failed capture must not be remembered as a screen")
	}
}

// Five animation frames were live on ONE machine at one instant. Matching a
// single glyph meant a session's status line was legible or invisible
// depending on which frame the capture caught — 16% of that machine's sessions
// read `unknown` while showing a perfectly good running spinner.
func TestStatusLineAcceptsEveryObservedGlyph(t *testing.T) {
	for _, glyph := range []string{"✻", "✽", "✢", "✶", "✳"} {
		t.Run(glyph, func(t *testing.T) {
			running, ok := statusLine(glyph + " Metamorphosing… (21m 39s · ↓ 91.4k tokens)")
			if !ok {
				t.Fatalf("%s not recognised as a status line", glyph)
			}
			if !running {
				t.Error("ellipsis form means the turn is still running")
			}
			finished, ok := statusLine(glyph + " Worked for 2m 7s")
			if !ok || finished {
				t.Errorf("%s finished form: ok=%v running=%v", glyph, ok, finished)
			}
		})
	}
}

// The glyph test had to be widened, and a wide test meets far more than the
// status line: the TUI's chrome is made of symbols. These are lines from real
// captures that must never be read as a turn status.
func TestStatusLineRejectsChrome(t *testing.T) {
	for _, line := range []string{
		"❯ ",                          // the composer, which sits BELOW the status line
		"❯ please refactor for me",    // ... and can contain the finished infix
		"⏵⏵ auto mode on (shift+tab)", // mode footer
		"▸ Opus 5 · some-repo",        // model footer
		"⎿  $ cd /some/path",          // tool output
		"────────────────────", // composer fencing
		"- a bulleted line", // ASCII punctuation: transcript prose
		"* another for good measure",
		"",
	} {
		if _, ok := statusLine(line); ok {
			t.Errorf("%q was read as a status line", line)
		}
	}
}

// The kind exists so a caller can auto-answer a question it recognises without
// matching prose itself. Its most important property is the last case: an
// unfamiliar prompt gets NO kind, so a filter on a known kind can never select
// it. Empty is not permission.
func TestClassifyPromptKind(t *testing.T) {
	cases := []struct {
		name string
		opts []string
		want fleet.PromptKind
	}{
		{"resume chooser", []string{"Resume from summary (recommended)", "Resume full session as-is", "Don't ask me again"}, fleet.PromptResumeChooser},
		{"folder trust", []string{"Yes, I trust this folder", "No, continue without these permissions"}, fleet.PromptFolderTrust},
		// Paired with its actual runtime negative, "No, exit Claude Code" — not
		// the bypass-acceptance screen's "No, exit", which sits at a different
		// offset in the binary. Classification does not care (only the
		// affirmative is read), but an invented pairing here is the same defect
		// a previous fixture in this same function was written to end.
		{"settings trust", []string{"Yes, I trust these settings", "No, exit Claude Code"}, fleet.PromptSettingsTrust},
		// THE REAL acceptance screen, verbatim from the runtime binary — and it
		// classifies as nothing. The case this replaced asserted the kind using
		// an option reading "Yes, I accept the bypass permissions mode", a
		// string that does not exist in the runtime. It passed for as long as it
		// did precisely because it was invented alongside the rule it verified.
		{"permission-mode acceptance is NOT classifiable", []string{"No, exit", "Yes, I accept"}, ""},
		{"tool permission", []string{"Yes, allow this command", "Yes, and don't ask again", "No, tell Claude what to do"}, fleet.PromptToolPermission},
		{"something never seen before", []string{"Deploy to production", "Cancel"}, ""},
		{"no options at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPromptKind(&fleet.SessionPrompt{Options: tc.opts})
			if got != tc.want {
				t.Errorf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// The prompt whose highlighted default is "No, exit" is the reason policy
// never moves into this service: a caller that answered the default here would
// kill the session it meant to rescue. The kind must not imply it is safe.
func TestUnrecognisedPromptIsNeverGivenAKind(t *testing.T) {
	p := &fleet.SessionPrompt{Options: []string{"No, exit", "Yes, proceed"}, Selected: 1}
	if k := classifyPromptKind(p); k != "" {
		t.Errorf("a prompt we do not recognise was labelled %q — a filter would then select it", k)
	}
}

// The exact prompt that was mislabelled in production: a ship decision whose
// QUESTION mentions "No auth bypass". The question is written by the agent, so
// matching it is injectable — an agent could get its own decision auto-answered
// by a client that filters on kind.
func TestAgentQuestionIsNeverGivenARuntimeKind(t *testing.T) {
	p := &fleet.SessionPrompt{
		Question: "failure counter never decays... No auth bypass, all 9 security fixes verified real, 29/29 green. How do you want me to proceed?",
		Options: []string{"Bounce back for the cheap fixes", "Merge now, file follow-ups",
			"Merge now, fix comment only", "Type something.", "Chat about this"},
		Selected: 1,
	}
	if k := classifyPromptKind(p); k != "" {
		t.Errorf("a merge decision was labelled %q — a filtering client would auto-answer it", k)
	}
}

// Needles must land in ONE option; spread across the set they are a coincidence.
func TestPromptKindDoesNotCombineAcrossOptions(t *testing.T) {
	p := &fleet.SessionPrompt{Options: []string{"Resume the deployment", "Show me the summary"}}
	if k := classifyPromptKind(p); k != "" {
		t.Errorf("words from two different options combined into %q", k)
	}
}

// A session blocked by a usage limit paints like a healthy one — empty
// composer, no spinner — and `idle` is the one status meaning "send it work".
// A supervisor picking the least-loaded session would preferentially choose
// exactly the sessions that cannot do anything.
func TestUsageLimitIsNotIdle(t *testing.T) {
	const rule = "────────────────────"
	blocked := "some transcript\n" +
		"You've hit your weekly limit · resets Thu 9:00\n" +
		rule + "\n❯ \n" + rule + "\n"

	st := classifyAged(blocked, true, false)
	if st.Status != fleet.StatusQuotaBlocked {
		t.Fatalf("status = %s, want quota_blocked (a blocked session must not read as available)", st.Status)
	}
	if !strings.Contains(st.Evidence, "usage limit") {
		t.Errorf("evidence should name the limit, got %q", st.Evidence)
	}
	if !strings.Contains(st.Evidence, "thu 9:00") && !strings.Contains(strings.ToLower(st.Evidence), "thu") {
		t.Errorf("evidence should carry the reset hint, got %q", st.Evidence)
	}
	if st.Prompt != nil {
		t.Error("nothing to answer here — a prompt must not be invented")
	}
}

// After --resume the runtime re-renders the transcript, so an OLD limit notice
// scrolls past again. Judging by anything but the live bottom reports a session
// as blocked by a limit it recovered from hours ago.
func TestHistoricalLimitNoticeIsIgnored(t *testing.T) {
	const rule = "────────────────────"
	replayed := "You've hit your weekly limit · resets Thu 9:00\n" +
		"  ⏺ ...and then the session carried on working\n" +
		rule + "\n❯ \n" + rule + "\n"

	st := classifyAged(replayed, true, false)
	if st.Status == fleet.StatusWaitingInput && strings.Contains(st.Evidence, "usage limit") {
		t.Error("a limit notice in replayed scrollback was read as a live block")
	}
}

// The screen that prompted this: a turn that died on a transient server error
// looks exactly like one that finished — error printed, spinner settled into
// its finished form, composer empty. Both are idle, and idle is the status
// that means "send it work", so the abandoned work goes unnoticed.
func TestFailedTurnIsNotedWithoutLyingAboutTheStatus(t *testing.T) {
	const rule = "────────────────────"
	errored := "  ⏺ API Error: 529 Overloaded. This is a server-side issue, usually\n" +
		"    temporary — try again in a moment. If it persists, check\n" +
		"    https://status.claude.com.\n" +
		"✻ Sautéed for 3m 24s\n" +
		rule + "\n❯ \n" + rule + "\n"

	st := classifyAged(errored, true, false)
	if st.Status != fleet.StatusIdle {
		t.Fatalf("status = %s, want idle — the session IS up and will accept input", st.Status)
	}
	if st.LastTurn == nil {
		t.Fatal("the turn died and nothing recorded it; this is the whole point")
	}
	if st.LastTurn.Outcome != "failed" {
		t.Errorf("outcome = %q, want failed", st.LastTurn.Outcome)
	}
	if !st.LastTurn.Retryable {
		t.Error("the runtime called it temporary; a supervisor may simply poke it")
	}
	if !strings.Contains(strings.ToLower(st.LastTurn.Reason), "529") {
		t.Errorf("reason should carry the runtime's own words, got %q", st.LastTurn.Reason)
	}
}

// A session that finished cleanly must NOT carry the footnote, or every idle
// session grows one and it stops meaning anything.
func TestCleanTurnHasNoFootnote(t *testing.T) {
	const rule = "────────────────────"
	clean := "  ⏺ Done — all 12 tests pass.\n✻ Brewed for 1m 0s\n" + rule + "\n❯ \n" + rule + "\n"
	st := classifyAged(clean, true, false)
	if st.LastTurn != nil {
		t.Errorf("a clean turn was marked %+v", st.LastTurn)
	}
}

// After a resume the runtime re-renders old output, so an error from hours ago
// scrolls past again. A session that errored and then carried on has that later
// work in the window, which is what pushes the stale error out of it.
func TestOldErrorFollowedByWorkIsNotTheLastTurn(t *testing.T) {
	const rule = "────────────────────"
	recovered := "  ⏺ API Error: 529 Overloaded. Temporary.\n" +
		"  ⏺ Retried and it worked.\n  ⏺ Then I did the next thing.\n" +
		"  ⏺ And another.\n  ⏺ And one more.\n  ⏺ Finished the task.\n" +
		"✻ Brewed for 2m 0s\n" + rule + "\n❯ \n" + rule + "\n"
	st := classifyAged(recovered, true, false)
	if st.LastTurn != nil {
		t.Errorf("a recovered session was reported as having failed: %+v", st.LastTurn)
	}
}

// waiting_input carries three different situations that need OPPOSITE
// handling, and only one of them has a prompt to branch on. Before this,
// telling "holds unsent text" from "out of quota" meant parsing the evidence
// prose — which §2.3 forbids, and which this project has paid for twice.
func TestWaitingInputSaysWhy(t *testing.T) {
	const rule = "────────────────────"

	held := "transcript\n✻ Brewed for 1m 0s\n" + rule + "\n❯ finish the migration\n" + rule + "\n"
	if st := classifyAged(held, true, false); st.WaitingOn != fleet.WaitingUnsentInput {
		t.Errorf("unsent text: waitingOn = %q, want %q", st.WaitingOn, fleet.WaitingUnsentInput)
	}

	// A quota block is its own status — §2.3's quota_blocked, "alive but
	// refused by its provider" — not a flavour of waiting_input. Nothing a
	// caller sends unblocks it, so it never belonged in the status that means
	// "blocked on a human".
	quota := "transcript\nYou've hit your weekly limit · resets Thu 9:00\n" + rule + "\n❯ \n" + rule + "\n"
	if st := classifyAged(quota, true, false); st.Status != fleet.StatusQuotaBlocked {
		t.Errorf("usage limit: status = %q, want %q", st.Status, fleet.StatusQuotaBlocked)
	}

	asked := "transcript\n  1. Yes, proceed\n❯ 2. No, exit\nEnter to select\n" + rule + "\n❯ \n" + rule + "\n"
	st := classifyAged(asked, true, false)
	if st.Status == fleet.StatusWaitingInput && st.WaitingOn != fleet.WaitingPrompt {
		t.Errorf("prompt: waitingOn = %q, want %q", st.WaitingOn, fleet.WaitingPrompt)
	}
}

// Every other status leaves it empty — a reason for waiting means nothing on a
// session that is not waiting, and a stray value invites branching on noise.
func TestWaitingOnIsEmptyWhenNotWaiting(t *testing.T) {
	const rule = "────────────────────"
	idle := "transcript\n✻ Brewed for 1m 0s\n" + rule + "\n❯ \n" + rule + "\n"
	if st := classifyAged(idle, true, false); st.WaitingOn != "" {
		t.Errorf("idle session carries waitingOn=%q", st.WaitingOn)
	}
}

// The runtime's welcome panel: box-drawing rows, capitalised words, and a
// "what's new" list of truncated lines ending in an ellipsis. Every condition
// the status-line test looks for, and not a status line at all. A freshly
// started session reported `working` forever because of it.
func TestWelcomePanelIsNotAStatusLine(t *testing.T) {
	const rule = "────────────────────"
	splash := "╭─── Claude Code v2.1.222 ───────────────╮\n" +
		"│                 Welcome back!          │ Tips for getting        │\n" +
		"│                                        │ Run /init to create a … │\n" +
		"│         Opus 5 · Claude Max ·          │ Fixed worktree-isolate… │\n" +
		"╰────────────────────────────────────────╯\n" +
		rule + "\n❯ this must never be sent\n" + rule + "\n"

	if running, ok := spinner(newScreen(splash)); ok {
		t.Errorf("the welcome panel was read as a status line (running=%v)", running)
	}
	st := classifyAged(splash, true, false)
	if st.Status == fleet.StatusWorking {
		t.Errorf("a session sitting at its welcome panel reported %s", st.Status)
	}

	// A first sighting is deliberately unknown here — no spinner and text in
	// the composer is genuinely ambiguous from one capture. The second look
	// settles it, and that is what a caller actually sees.
	t0 := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	_, digest := classifyPaneRemembering(splash, true, true, false, paneMemory{}, t0)
	prior := paneMemory{known: true, digest: digest, at: t0}
	second, _ := classifyPaneRemembering(splash, true, true, false, prior, t0.Add(time.Minute))
	if second.WaitingOn != fleet.WaitingUnsentInput {
		t.Errorf("second look: waitingOn = %q, want unsent-input — the composer plainly holds text",
			second.WaitingOn)
	}
}

// The digest a caller quotes back must fingerprint the same bytes discard
// compares. They briefly did not: the resolution path published the whole
// SCREEN's digest while discard hashed the composer text, so a caller doing
// everything right was refused every time.
func TestComposerDigestIsOfTheComposerNotTheScreen(t *testing.T) {
	const rule = "────────────────────"
	const pending = "this must never be sent"
	screenText := "transcript\n" + rule + "\n❯ " + pending + "\n" + rule + "\n"

	t0 := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	_, d := classifyPaneRemembering(screenText, true, true, false, paneMemory{}, t0)
	prior := paneMemory{known: true, digest: d, at: t0}
	st, _ := classifyPaneRemembering(screenText, true, true, false, prior, t0.Add(time.Minute))

	if st.ComposerDigest == "" {
		t.Fatal("no digest published, so the text cannot be discarded safely")
	}
	if want := composerTextDigest(pending); st.ComposerDigest != want {
		t.Errorf("digest = %s, want %s (the composer text, not the screen)", st.ComposerDigest, want)
	}
	if st.ComposerDigest == screenDigest(screenText) {
		t.Error("the published digest is of the whole screen — discard can never match it")
	}
}

// The real shape from a blocked machine: notice, the runtime's own
// continuation line, then a settled status line. A one-line rule saw nothing
// on a fleet that had been refusing work for days.
func TestLimitNoticeFollowedByChromeIsStillLive(t *testing.T) {
	const rule = "────────────────────"
	live := "  Ran 1 shell command\n" +
		"  ⎿  You've hit your weekly limit · resets Aug 10 at 12am (Asia/Tokyo)\n" +
		"     /usage-credits to finish what you're working on.\n" +
		"✻ Churned for 15s\n" + rule + "\n❯ \n" + rule + "\n"

	st := classifyAged(live, true, false)
	if st.Status != fleet.StatusQuotaBlocked {
		t.Errorf("status = %s, want quota_blocked — the notice is two lines up, not gone", st.Status)
	}
	if st.Quota == nil || !strings.Contains(st.Quota.ResetHint, "aug 10") {
		t.Errorf("reset hint missing or wrong: %+v", st.Quota)
	}
}

// ...but agent output after the notice still means the session carried on.
// That is the divider: chrome below it is the runtime finishing its sentence,
// a response bullet below it is work that happened afterwards.
func TestLimitNoticeFollowedByAgentOutputIsHistory(t *testing.T) {
	const rule = "────────────────────"
	replayed := "  ⎿  You've hit your weekly limit · resets Aug 10\n" +
		"⏺ Retried after the reset and it worked.\n" +
		"✻ Brewed for 2m 0s\n" + rule + "\n❯ \n" + rule + "\n"

	st := classifyAged(replayed, true, false)
	if st.Status == fleet.StatusQuotaBlocked {
		t.Error("a notice with agent output beneath it was read as a live block")
	}
}

// --- what a client is handed to RENDER -------------------------------------

// The published question must be text, not terminal bytes.
//
// This is the exact line measured on a live fleet, the runtime's hyperlink
// around "Security guide" included. Before OSC sequences were stripped, that
// hyperlink reached clients intact — inside `prompt.question`, which exists to
// be shown to a human at the moment they are deciding whether to trust a
// directory. A colour code garbles matching; this one garbles the ASK.
func TestPromptQuestionCarriesNoTerminalEscapes(t *testing.T) {
	const linked = "  Quick safety check: Is this a project you created or one you trust?\n" +
		"  These will apply without asking. Only proceed if you trust this configuration.\n" +
		"  \x1b]8;id=zaxmda;https://code.claude.com/docs/en/security\x1b\\Security guide\x1b]8;;\x1b\\\n" +
		"❯ 1. Yes, I trust this folder\n" +
		"  2. No, exit\n" +
		"Enter to confirm · Esc to cancel"

	p := parsePrompt(newScreen(linked))
	if p == nil {
		t.Fatal("no prompt parsed")
	}
	if strings.ContainsRune(p.Question, '\x1b') {
		t.Errorf("question carries escape bytes: %q", p.Question)
	}
	// The label inside the hyperlink is what the human sees, so it survives.
	if !strings.Contains(p.Question, "Security guide") {
		t.Errorf("stripping ate the visible label: %q", p.Question)
	}
	if strings.Contains(p.Question, "code.claude.com") {
		t.Errorf("the URI is part of the sequence, not of the text: %q", p.Question)
	}
}

// A sequence the capture cut in half is left alone. Consuming to end-of-line on
// an unterminated escape would eat visible text, silently, in the one function
// every other read is built on.
func TestUnterminatedEscapeDoesNotEatTheLine(t *testing.T) {
	const cut = "trailing words\x1b]8;id=x;https://example"
	if got := stripEscapes(cut); !strings.Contains(got, "trailing words") {
		t.Errorf("stripEscapes(%q) = %q", cut, got)
	}
}

// --- muster#133: the rest of #132's blank-row matrix -------------------
//
// #132 pinned four shapes with trailing/interior blank ROWS by tests. What
// follows closes shapes #132 traced through by hand and believed safe, or
// believed unaffected, but never actually exercised.

// composerCursorRowBlank trims per line before deciding blank-or-not, and
// newScreen itself already strips a trailing \r/\t/space from every line
// before that ever runs (see newScreen's own TrimRight cutset). #132 called
// this combination "believed unaffected, not exercised end to end" — this
// pins it, for the row composerCursorRowBlank actually inspects (the
// composer's own bottom row, which needs a continuation row present to be
// anything other than the always-not-blank marker row — see this function's
// own doc comment on that one-row special case).
func TestComposerCursorRowBlankIgnoresLineEndingNoise(t *testing.T) {
	cases := []struct {
		name      string
		bottomRow string
		wantBlank bool
	}{
		{"CRLF after real content", "real content\r", false},
		{"CRLF alone", "\r", true},
		{"tabs alone", "\t\t\t", true},
		{"trailing spaces alone", "   ", true},
		{"real content with trailing spaces", "real content   ", false},
		{"mixed tab and CRLF, no content", "\t \r", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := rule + "\n❯ top row\n" + tc.bottomRow + "\n" + rule
			blank, scan := composerCursorRowBlank(newScreen(f))
			if scan != composerFound {
				t.Fatalf("composer not found, scan=%v", scan)
			}
			if blank != tc.wantBlank {
				t.Errorf("blank = %v, want %v for bottom row %q", blank, tc.wantBlank, tc.bottomRow)
			}
		})
	}
}

// Wide characters — CJK and emoji — must not be misread as blank, and must
// still surface through composerText intact.
func TestComposerHandlesWideCharacters(t *testing.T) {
	f := rule + "\n❯ 日本語のテスト 🎉\n" + rule
	text, scan := composerText(newScreen(f))
	if scan != composerFound || text != "日本語のテスト 🎉" {
		t.Errorf("composer = %q scan=%v, want the wide-character text intact", text, scan)
	}
}

// A row holding ONLY a full-width (ideographic) space is, structurally, empty
// content: Go's unicode.IsSpace (which strings.TrimSpace, and so
// composerCursorRowBlank, relies on) treats U+3000 as whitespace exactly like
// an ordinary space. It should cross the same way an ordinary blank row does,
// not be mistaken for real content because the bytes are non-ASCII.
func TestComposerCursorRowBlankTreatsFullWidthSpaceAsBlank(t *testing.T) {
	f := rule + "\n❯ real content\n　　\n" + rule
	blank, scan := composerCursorRowBlank(newScreen(f))
	if scan != composerFound {
		t.Fatalf("composer not found, scan=%v", scan)
	}
	if !blank {
		t.Error("a row holding only full-width spaces was not read as blank")
	}
}

// A row that literally contains bracketed-paste markers or other raw control
// bytes — as opposed to the SGR/OSC sequences stripEscapes already strips —
// must not break span-finding, must not be misread as blank, and must still
// surface through composerText: it is unusual but real content, and needs an
// ordinary C-u like any other non-blank row.
func TestComposerSpanSurvivesControlBytesInText(t *testing.T) {
	// \x1b[200~ / \x1b[201~ are the bracketed-paste start/end markers; \x07 is
	// a bell. None is an SGR (…m) or OSC (…BEL/ST) sequence, so stripEscapes
	// does not remove them — this is what a pane would actually show if a
	// paste's own markers leaked into the rendered text.
	f := rule + "\n❯ top row\n\x1b[200~pasted\x07text\x1b[201~\n" + rule
	text, scan := composerText(newScreen(f))
	if scan != composerFound {
		t.Fatalf("composer not found, scan=%v", scan)
	}
	if !strings.Contains(text, "pasted") || !strings.Contains(text, "text") {
		t.Errorf("composer text lost content around control bytes: %q", text)
	}
	blank, blankScan := composerCursorRowBlank(newScreen(f))
	if blankScan != composerFound {
		t.Fatalf("composer not found, scan=%v", blankScan)
	}
	if blank {
		t.Error("a row holding real text plus control bytes was read as blank")
	}
}

// muster#133 §1: "a payload taller than the visible pane, i.e. exceeding
// the capture window" — flagged in the issue itself as possibly needing a fix
// rather than just a test, because captureForClassify reads only the tail
// (classifyCaptureArgs' `-S -N`, N = defaultCaptureLines = 24) of a pane, with
// no upper end bound. A composer taller than that window has its OPENING
// fence and prompt-marker row scrolled out of what gets captured.
//
// This is not hypothetical: muster#129's own field case was on the order
// of eighty on-screen rows, more than three times this window.
//
// composerSpan finds the composer by walking UP from the closing rule looking
// for the ❯-marked prompt row, and reports composerClipped if it reaches the
// top of the captured lines without finding one (composerSpan's own "opening
// rule reached with no composer between" case does not even apply here —
// there is no opening rule in view at all, just ordinary continuation rows).
// This reproduces that: a screen built from only the TAIL of a tall composer,
// the same shape a real `-S -24` capture would hand the classifier.
//
// CLOSED by muster#134: composerText/composerVisualLines now report
// composerClipped for this shape, not composerAbsent — every caller that used
// to read "not found" as "nothing to protect" (Discard's early return,
// Send's composer-guard) now fails closed instead of silently succeeding.
// See classify.go's composerScan and each call site in tmux.go/keys.go for
// how the three-valued result is actually consumed.
func TestComposerSpanMissesAComposerTallerThanTheCaptureWindow(t *testing.T) {
	// Model only what a `-S -24` capture would actually return: the last 24
	// rows of a composer with far more real content rows than that above it,
	// with NO opening rule and NO ❯-marked row anywhere in view — both
	// scrolled out above the window, exactly as they would be off a live pane.
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("row %d of a paste taller than the capture window", i))
	}
	tail := lines[len(lines)-defaultCaptureLines:]
	f := strings.Join(tail, "\n") + "\n" + rule

	text, scan := composerText(newScreen(f))
	if scan != composerClipped {
		t.Fatalf("composerText scan = %v, want composerClipped: the opening fence and "+
			"prompt row are not in view, so this must read as \"could not tell\", never "+
			"as \"found\" and never as \"definitely absent\" — the fixture is wrong, not "+
			"the code, if this ever fails to reproduce composerClipped", scan)
	}

	// The failure mode this closes: were this screen handed to Discard before
	// #134, its early return ("pending == \"\" means already clear") would
	// have fired on a composer that, off-screen, still holds 30 rows of
	// unsent text. Discard now refuses instead — see
	// TestDiscardRefusesAClippedComposerRatherThanClaimingSuccess.
	if text != "" {
		t.Errorf("text = %q, want empty (composerText's own contract when scan != composerFound)", text)
	}

	// composerVisualLines and composerCursorRowBlank share composerSpan and
	// must report the identical verdict for this same screen.
	if _, linesScan := composerVisualLines(newScreen(f)); linesScan != composerClipped {
		t.Errorf("composerVisualLines scan = %v, want composerClipped", linesScan)
	}
	if _, blankScan := composerCursorRowBlank(newScreen(f)); blankScan != composerClipped {
		t.Errorf("composerCursorRowBlank scan = %v, want composerClipped", blankScan)
	}
}

// muster#134: composerClipped must be a narrow predicate — reachable
// only by the two "ran off the top of the capture" exits inside
// composerSpan, never a catch-all for "did not find a composer". This pins
// the three-way split directly: a genuinely tall/scrolled composer is
// clipped, a real composer-less screen (or a menu with a real preceding
// line) is absent, and an ordinary complete composer is found — for the
// SAME three structural shapes composerSpan's own doc comments describe.
func TestComposerSpanTellsClippedApartFromAbsent(t *testing.T) {
	var tail []string
	for i := 0; i < defaultCaptureLines; i++ {
		tail = append(tail, fmt.Sprintf("row %d of a paste taller than the capture window", i))
	}
	clippedFixture := strings.Join(tail, "\n") + "\n" + rule

	cases := []struct {
		name    string
		fixture string
		want    composerScan
	}{
		{"tall composer, tail-only capture: ran off the top with no rule and no marker", clippedFixture, composerClipped},
		{"ordinary complete composer", fixtureIdle, composerFound},
		{"menu with a real preceding line: genuinely not a composer", fixtureMenuSelected, composerAbsent},
		{"no rule anywhere in the screen at all", "$ ls -la\ntotal 8", composerAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, scan := composerSpan(newScreen(tc.fixture)); scan != tc.want {
				t.Errorf("scan = %v, want %v", scan, tc.want)
			}
		})
	}
}

// fixtureReviewScreen is the last screen of a multi-question dialog (#159):
// the tab bar, the answers given, the runtime's own confirmation line, and a
// two-option confirm widget. It renders NO footer — the question tabs before
// it do — which is what used to leave it in #58's corroboration hold.
const fixtureReviewScreen = "  ←  ☒ First  ☒ Second  ✔ Submit  →\n" + rule + "\n" +
	"  Review your answers\n\n" +
	"   ● Which approach?\n     → The first one\n" +
	"   ● And the name?\n     → Keep it\n\n" +
	"  Ready to submit your answers?\n\n" +
	"  ❯ 1. Submit answers\n" +
	"    2. Cancel\n"

// #159 asks 2 and 3: the review screen is a prompt on EVERY read — first
// sighting, a memory too young to corroborate anything, a memory of a
// different screen — with the same options and the same nonce each time, and
// the respond path's own parse produces that nonce too. It carries no kind:
// recognised enough to report at once, never enough to automate.
func TestReviewScreenIsAFirstClassPrompt(t *testing.T) {
	t0 := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	digest := screenDigest(fixtureReviewScreen)
	memories := []paneMemory{
		{},
		{known: true, digest: digest, at: t0},
		{known: true, digest: "a-different-screen", at: t0.Add(-time.Hour)},
	}
	var nonce string
	for i := 0; i < 30; i++ {
		now := t0.Add(time.Duration(i) * 300 * time.Millisecond)
		for m, prior := range memories {
			st, _ := classifyPaneRemembering(fixtureReviewScreen, true, true, false, prior, now)
			if st.Status != fleet.StatusWaitingInput {
				t.Fatalf("read %d, memory %d: status = %s, want waiting_input (%s)", i, m, st.Status, st.Evidence)
			}
			if st.WaitingOn != fleet.WaitingPrompt {
				t.Fatalf("read %d, memory %d: waitingOn = %q, want %q", i, m, st.WaitingOn, fleet.WaitingPrompt)
			}
			p := st.Prompt
			if p == nil || len(p.Options) != 2 || p.Options[0] != "Submit answers" || p.Options[1] != "Cancel" {
				t.Fatalf("read %d, memory %d: prompt = %+v, want options [Submit answers Cancel]", i, m, p)
			}
			if p.Selected != 1 {
				t.Errorf("read %d, memory %d: selected = %d, want 1", i, m, p.Selected)
			}
			if p.Kind != "" {
				t.Errorf("read %d, memory %d: kind = %q; submitting somebody else's answers "+
					"must not be a kind a client can filter on and automate", i, m, p.Kind)
			}
			if nonce == "" {
				nonce = p.Nonce
			} else if p.Nonce != nonce {
				t.Fatalf("read %d, memory %d: nonce = %s, want the stable %s", i, m, p.Nonce, nonce)
			}
		}
	}
	if p := parsePrompt(newScreen(fixtureReviewScreen)); p == nil || p.Nonce != nonce {
		t.Errorf("respond's parse = %+v; it must verify against the nonce the read published (%s)", p, nonce)
	}
}

// The corroboration is the whole shape, not any one string of it. Each of
// these is one step away from the review screen and must still be held on a
// first sighting like any other unrecognised structural prompt (#58).
func TestReviewScreenLookalikesStayHeld(t *testing.T) {
	t0 := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"an agent's own menu with the same first two options": strings.Replace(fixtureReviewScreen,
			"    2. Cancel\n", "    2. Cancel\n    3. Type something.\n", 1),
		"the same options under a different question": strings.Replace(fixtureReviewScreen,
			"  Ready to submit your answers?\n", "  Ship it now?\n", 1),
		"the options reordered": strings.Replace(fixtureReviewScreen,
			"  ❯ 1. Submit answers\n    2. Cancel\n", "  ❯ 1. Cancel\n    2. Submit answers\n", 1),
	}
	for name, screen := range cases {
		st, _ := classifyPaneRemembering(screen, true, true, false, paneMemory{}, t0)
		if st.Status == fleet.StatusWaitingInput || st.Prompt != nil {
			t.Errorf("%s: first sighting = %s with prompt %+v; only the exact review screen is "+
				"corroborated by its chrome", name, st.Status, st.Prompt)
		}
	}
}
