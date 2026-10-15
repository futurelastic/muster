package tmux

import (
	"encoding/json"
	"strings"
	"testing"
)

// muster#324: an AskUserQuestion option's description is drawn under its label,
// and a preview pane shows the highlighted option's preview. Neither reached
// state.prompt, and neither is in the transcript until the question is answered.
// The corpus redacts option text, so these screens are written out here.

var detailScreenTop = strings.Repeat("─", 70) + "\n" +
	" ☐ Approach\n\n" +
	" Which approach should I take?\n\n"

func describedMenu(highlight string) string {
	rows := []string{
		"   1. Path A",
		"      Full rewrite, slower but clean",
		"   2. Path B",
		"      Small patch, ships today and keeps the old behaviour for callers that",
		"      have not moved yet",
		"   3. Path C",
		"   4. Type something.",
	}
	for i, r := range rows {
		if strings.HasPrefix(r, "   "+highlight+". ") {
			rows[i] = " ❯" + r[2:]
		}
	}
	return detailScreenTop + strings.Join(rows, "\n") + "\n\n" +
		strings.Repeat("─", 70) + "\n   5. Chat about this\n\n Enter to select · ↑/↓ to navigate · Esc to cancel"
}

func TestOptionDescriptionsReachThePrompt(t *testing.T) {
	p := parsePrompt(newScreen(describedMenu("1")))
	if p == nil {
		t.Fatal("not recognised as a prompt")
	}
	if len(p.Options) < 4 || p.Options[0] != "Path A" || p.Options[1] != "Path B" {
		t.Fatalf("options changed shape: %q", p.Options)
	}
	if len(p.OptionDetails) != len(p.Options) {
		t.Fatalf("optionDetails has %d entries for %d options", len(p.OptionDetails), len(p.Options))
	}
	d := p.OptionDetails
	if d[0].Label != "Path A" || d[0].Description != "Full rewrite, slower but clean" {
		t.Errorf("option 1 detail = %+v", d[0])
	}
	if want := "Small patch, ships today and keeps the old behaviour for callers that have not moved yet"; d[1].Description != want {
		t.Errorf("wrapped description = %q, want %q", d[1].Description, want)
	}
	if d[2].Description != "" {
		t.Errorf("option 3 has no description, got %q", d[2].Description)
	}
	if p.Question != "Which approach should I take?" {
		t.Errorf("question = %q — a description must not leak into it", p.Question)
	}
}

func TestADescriptionIsNotPartOfTheNonce(t *testing.T) {
	a := parsePrompt(newScreen(describedMenu("1")))
	b := parsePrompt(newScreen(describedMenu("2")))
	bare := parsePrompt(newScreen(strings.NewReplacer(
		"      Full rewrite, slower but clean\n", "",
		"      Small patch, ships today and keeps the old behaviour for callers that\n", "",
		"      have not moved yet\n", "").Replace(describedMenu("1"))))
	if a == nil || b == nil || bare == nil {
		t.Fatal("not recognised")
	}
	if a.Nonce != b.Nonce || a.Nonce != bare.Nonce {
		t.Errorf("nonce depends on the description or the highlight: %s %s %s", a.Nonce, b.Nonce, bare.Nonce)
	}
	if bare.OptionDetails != nil {
		t.Errorf("a menu with no descriptions reports optionDetails: %+v", bare.OptionDetails)
	}
	raw, _ := json.Marshal(bare)
	if strings.Contains(string(raw), "optionDetails") {
		t.Errorf("optionDetails is on the wire for a menu without any: %s", raw)
	}
}

func TestAFooterUnderTheLastOptionIsNotItsDescription(t *testing.T) {
	p := parsePrompt(newScreen(" Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel"))
	if p == nil {
		t.Fatal("not recognised")
	}
	if p.OptionDetails != nil {
		t.Errorf("optionDetails = %+v, want none", p.OptionDetails)
	}
}

func TestThePreviewOfTheHighlightedOptionIsReported(t *testing.T) {
	pane := func(sel int) string {
		box := []string{
			"┌──────────────────────────────────────────┐",
			"│ [layout A]                               │",
			"│ header | body                            │",
			"└──────────────────────────────────────────┘",
		}
		if sel == 2 {
			box[1] = "│ [layout B]                               │"
		}
		mark := func(n int) string {
			if n == sel {
				return " ❯ "
			}
			return "   "
		}
		return strings.Repeat("─", 100) + "\n ☐ Pick\n\n Which card do you want?\n\n" +
			mark(1) + "1. Option A                     " + box[0] + "\n" +
			mark(2) + "2. Option B                     " + box[1] + "\n" +
			mark(3) + "3. Option C                     " + box[2] + "\n" +
			"                                 " + box[3] + "\n\n" +
			strings.Repeat("─", 100) + "\n   Chat about this\n\n Enter to select · ↑/↓ to navigate · Esc to cancel"
	}
	p := parsePrompt(newScreen(pane(1)))
	if p == nil {
		t.Fatal("not recognised")
	}
	// The box's rows sit beside options 1..3 here, so the preview is the whole
	// pane's text; what matters is that it is attached to the highlighted option
	// and to no other.
	if len(p.OptionDetails) != len(p.Options) {
		t.Fatalf("optionDetails = %+v", p.OptionDetails)
	}
	if got := p.OptionDetails[0].Preview; !strings.Contains(got, "[layout A]") {
		t.Errorf("preview on option 1 = %q", got)
	}
	for i := 1; i < len(p.OptionDetails); i++ {
		if p.OptionDetails[i].Preview != "" {
			t.Errorf("preview on a non-highlighted option %d: %q", i+1, p.OptionDetails[i].Preview)
		}
	}
	q := parsePrompt(newScreen(pane(2)))
	if q == nil || p.Nonce != q.Nonce {
		t.Errorf("nonce moved with the preview: %v vs %v", p.Nonce, q)
	}
}
