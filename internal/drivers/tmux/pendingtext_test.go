package tmux

import (
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster#266: the runtime writes a message that ends in an open question to its
// record only when the question is answered, so the prose the question refers to
// is on the screen and nowhere else. Every fixture here is synthetic.

const pendingRule = "────────────────────────────────────────────────────────────────────────"

// dialogBelow is a single-question dialog: its opening rule, the header chip, the
// question, and two options under the footer the runtime draws.
const dialogBelow = pendingRule + "\n" +
	"☐ Pick\n\n" +
	"Which way?\n\n" +
	"❯ 1. Alpha\n" +
	"  2. Bravo\n" +
	pendingRule + "\n" +
	"  3. Chat about this\n\n" +
	"Enter to select · ↑/↓ to navigate · Esc to cancel\n"

func aboveDialogText(t *testing.T, screenText string) (string, bool, bool) {
	t.Helper()
	text, truncated, nonce, ok := pendingAgentText(newScreen(screenText))
	if ok && nonce == "" {
		t.Fatal("pending text returned with no nonce to tie it to the prompt")
	}
	return text, truncated, ok
}

func TestPendingText_AgentTextAboveAnOpenPromptIsReturned(t *testing.T) {
	screen := "❯ please decide\n\n" +
		"⏺ Two options follow.\n" +
		"  The first is quick.\n\n" +
		"  The second is thorough.\n\n" +
		dialogBelow
	text, truncated, ok := aboveDialogText(t, screen)
	if !ok {
		t.Fatal("no pending text for agent prose directly above an open dialog")
	}
	want := "Two options follow.\nThe first is quick.\n\nThe second is thorough."
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if truncated {
		t.Error("truncated set for a block whose head is on screen")
	}
	// Its nonce is the prompt's own, so a client can tie the two together.
	_, _, nonce, _ := pendingAgentText(newScreen(screen))
	if p := parsePrompt(newScreen(screen)); p == nil || p.Nonce != nonce {
		t.Errorf("nonce %q is not the open prompt's (%+v)", nonce, p)
	}
}

// The ruling's boundary: only the agent's own text. A block holding a tool
// result, a tool call, a status line or an inbound message is left out whole.
func TestPendingText_OnlyAgentTextIsReturned(t *testing.T) {
	cases := map[string]string{
		"tool call with output":        "⏺ Bash(ls -la)\n  ⎿  total 8\n     drwxr-xr-x  2 u g  64 file\n\n",
		"tool output rows after prose": "⏺ Reading it now.\n  ⎿  secret-looking-output\n     more output\n\n",
		"tool call, no output yet":     "⏺ Read(notes.txt)\n\n",
		"mcp tool call":                "⏺ server - lookup (MCP)(query: \"x\")\n\n",
		"collapsed tool summary":       "⏺ Read 3 files (ctrl+o to expand)\n\n",
		"a user message":               "❯ a human line directly above\n\n",
		"a status line":                "✻ Churned for 5s\n\n",
		"nothing above":                "",
	}
	for name, above := range cases {
		t.Run(name, func(t *testing.T) {
			if text, _, ok := aboveDialogText(t, above+dialogBelow); ok {
				t.Errorf("returned %q; this block is not the agent's own text", text)
			}
		})
	}

	// The same screen with agent text, a tool row WITH output, and the open
	// prompt: only the text nearest the dialog comes back, and none of the tool
	// call or its output above it.
	mixed := "⏺ Bash(cat secrets)\n  ⎿  hunter2\n\n⏺ Here is what I found.\n  Pick one.\n\n" + dialogBelow
	text, _, ok := aboveDialogText(t, mixed)
	if !ok || text != "Here is what I found.\nPick one." {
		t.Errorf("text = %q, ok = %v; want only the prose block", text, ok)
	}
	if strings.Contains(text, "hunter2") || strings.Contains(text, "Bash") {
		t.Errorf("tool call or output leaked: %q", text)
	}
}

func TestPendingText_NoOpenPromptOrAnUnreadableScreenReturnsNothing(t *testing.T) {
	cases := map[string]string{
		"prompt answered — only the transcript remains": "⏺ Two options follow.\n\n✻ Churned for 5s\n\n" + pendingRule + "\n❯ \n" + pendingRule + "\n",
		"empty screen": "",
		"garbage":      "\x00\x01 not a screen at all\n\n\n",
		"dialog without a header (permission dialog)": "⏺ Some prose.\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n\nEsc to cancel · Tab to amend\n",
		"header with no rule above it":                "⏺ Prose.\n\n☐ Pick\n\nWhich way?\n\n❯ 1. Alpha\n  2. Bravo\n\nEnter to select · Esc to cancel\n",
	}
	for name, screen := range cases {
		t.Run(name, func(t *testing.T) {
			if text, _, ok := aboveDialogText(t, screen); ok {
				t.Errorf("returned %q for a screen with no readable open prompt", text)
			}
		})
	}
}

func TestPendingText_HeadAboveTheWindowIsReportedCutOff(t *testing.T) {
	// The head row is not in the capture: only the wrapped rows survive, at the
	// prose indent. They are returned and flagged.
	text, truncated, ok := aboveDialogText(t, "  middle of a paragraph\n  and its end\n\n"+dialogBelow)
	if !ok || !truncated || text != "middle of a paragraph\nand its end" {
		t.Errorf("text = %q truncated = %v ok = %v; want the visible rows, flagged", text, truncated, ok)
	}
	// A headless block with a deeper row could be a tool result's continuation
	// — it cannot be told from prose, so it is left out.
	if text, _, ok := aboveDialogText(t, "  prose\n     deeper row\n\n"+dialogBelow); ok {
		t.Errorf("returned %q for a headless block with a tool-result-depth row", text)
	}
}

func TestPendingText_ALongBlockIsBounded(t *testing.T) {
	long := "⏺ " + strings.Repeat("é", fleet.MaxTurnTextBytes) + "\n\n"
	text, truncated, ok := aboveDialogText(t, long+dialogBelow)
	if !ok || !truncated || len(text) > fleet.MaxTurnTextBytes {
		t.Errorf("len = %d truncated = %v ok = %v; want a bounded, flagged text", len(text), truncated, ok)
	}
}

// End to end over the fake multiplexer: the text is there while the dialog is
// up and gone once the pane shows the answered state.
func TestDriverTurns_PendingTextWhileThePromptIsOpen(t *testing.T) {
	d, f, ref, _, conv := titleSyncSession(t)
	ctx := t.Context()
	appendLine(t, conv, assistant(t, 1, textBlock("an older turn")))

	f.captures["%1"] = "⏺ Explaining first.\n  Then asking.\n\n" + dialogBelow
	page, err := d.Turns(ctx, fleet.Request{}, ref, fleet.TurnsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(page), ","); got != "an older turn" {
		t.Errorf("turns = %s; the pending text must not be a recorded turn", got)
	}
	if page.Pending == nil {
		t.Fatal("no pending entry while the dialog is up")
	}
	if page.Pending.Text != "Explaining first.\nThen asking." || page.Pending.Source != fleet.PendingSourceScreen || page.Pending.Nonce == "" || page.Pending.ObservedAt.IsZero() {
		t.Errorf("pending = %+v", page.Pending)
	}

	// Resolved: the screen no longer holds a prompt, and neither does the page.
	f.captures["%1"] = idleFixtureFor("alpha")
	page, err = d.Turns(ctx, fleet.Request{}, ref, fleet.TurnsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Pending != nil {
		t.Errorf("pending = %+v after the prompt resolved", page.Pending)
	}
}
