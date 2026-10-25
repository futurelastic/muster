package tmux

import (
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster#358: a person's own message written as a numbered list is echoed in
// the scrollback with the same ❯ leader a menu's highlighted option uses. That
// is a run of contiguous numbers with one marked, which is the whole of the
// structural test, so a session mid-turn read as blocked on a prompt, and a
// client that trusted it answered a card that was never there.
//
// The rule these tests pin: numbered rows with a live fenced composer BELOW them
// are transcript. A selection menu replaces the composer, so it never has one
// under its options.
//
// The corpus holds the same two shapes redacted (numbered-list-echo-*); these
// read as plain text, so a failure names the rule rather than a placeholder.

const echoedList = "❯ 1. two years\n  2. not yet\n  3. wildcard"

// composerBlock is an empty fenced composer and the mode row under it.
const composerBlock = rule + "\n❯\n" + rule + "\n  ⏵⏵ auto mode on (shift+tab to cycle)"

func echoScreen(below string) string {
	return "⏺ Two quick questions before I start on the schedule.\n\n" +
		"✻ Brewed for 5s · done 11:37 PM\n\n" +
		echoedList + "\n" + below + "\n\n" + composerBlock
}

func TestEchoedNumberedListIsNotAPrompt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		below  string
		status fleet.Status
	}{
		{"over a running turn", "\n✻ Thinking… (3s · esc to interrupt)", fleet.StatusWorking},
		{"after an interrupted turn", "  ⎿  Interrupted · What should Claude do instead?", fleet.StatusIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			screen := echoScreen(tc.below)
			if p := parsePrompt(newScreen(screen)); p != nil {
				t.Fatalf("the person's own echoed list was read as a menu: options %q, question %q",
					p.Options, p.Question)
			}
			if option, blocked := selectionPrompt(newScreen(screen)); blocked {
				t.Fatalf("selectionPrompt reports a prompt (%q) on a screen with no menu", option)
			}

			// Both the first look and a look well past the paint grace: the
			// unrecognised-prompt hold promotes a screen that sits unchanged, and
			// an interrupted turn sits unchanged by definition.
			t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
			first, digest := classifyPaneRemembering(screen, true, true, false, paneMemory{}, t0)
			prior := paneMemory{known: true, digest: digest, at: t0}
			later, _ := classifyPaneRemembering(screen, true, true, false, prior, t0.Add(30*time.Second))
			for when, st := range map[string]fleet.SessionState{"first look": first, "after 30s": later} {
				if st.Status != tc.status {
					t.Errorf("%s: status = %s, want %s (evidence: %s)", when, st.Status, tc.status, st.Evidence)
				}
				if st.Prompt != nil {
					t.Errorf("%s: carries a prompt it does not have: %+v", when, st.Prompt)
				}
			}
		})
	}
}

// The same list typed into the live composer (several rows, each continuation
// echoed with the ❯ leader) was already masked out of the scan (#180); the new
// check must not change that, and must not mistake the composer's own rows for
// a composer standing below the options.
func TestNumberedListInsideTheComposerIsStillNotAPrompt(t *testing.T) {
	screen := "⏺ Two quick questions.\n\n" + rule + "\n" + echoedList + "\n" + rule
	if p := parsePrompt(newScreen(screen)); p != nil {
		t.Fatalf("a numbered message typed into the composer was read as a menu: %+v", p.Options)
	}
}

// A real selector keeps classifying as one. Its options are the last thing on
// screen, with the dialog's own footer under them and no composer.
func TestSelectorWithoutAComposerIsStillAPrompt(t *testing.T) {
	screen := "✻ Brewed for 5s · done 11:37 PM\n\n" + rule + "\n" +
		" How long have you worked here?\n\n" +
		echoedList + "\n\n" +
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel"
	p := parsePrompt(newScreen(screen))
	if p == nil {
		t.Fatal("a selection menu with its footer was not read as one")
	}
	if got := strings.Join(p.Options, "|"); got != "two years|not yet|wildcard" {
		t.Errorf("options = %q", got)
	}
	if p.Selected != 1 {
		t.Errorf("selected = %d, want 1", p.Selected)
	}
}

// The dialog's footer is the corroboration an echo cannot carry, so it wins
// over a composer standing below. No such screen has been measured; this pins
// that the check yields to the footer rather than the other way round, which is
// the safer direction for a client waiting on a real dialog.
func TestAFooterOutranksAComposerBelowTheOptions(t *testing.T) {
	screen := "✻ Brewed for 5s · done 11:37 PM\n\n" + rule + "\n" +
		" How long have you worked here?\n\n" +
		echoedList + "\n\n" +
		"Enter to select · Esc to cancel\n\n" + composerBlock
	if p := parsePrompt(newScreen(screen)); p == nil {
		t.Fatal("a footer-corroborated menu was dropped because a composer stood below it")
	}
}
