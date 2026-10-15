package tmux

import (
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster#323: a tool-permission dialog reported only its last three rows. The
// tool's title and the command being approved sit above a dashed divider and
// were cut off, and the divider itself leaked into the question. The dialog is
// reconstructed here from the rows measured on a live session (the corpus
// redacts option text, so it cannot carry this one).

func permissionDialog(command, highlight string) string {
	rows := []string{
		"  ● Running the command.",
		"",
		strings.Repeat("─", 60),
		" Bash command",
		"",
		"   " + command,
		"   Create a file and report back",
		"",
		strings.Repeat("╌", 60),
		" This command requires approval",
		"",
		" Do you want to proceed?",
		"   1. Yes",
		"   2. Yes, and don’t ask again for: touch",
		"   3. Yes, and switch to auto mode · auto mode handles these prompts for you",
		"   4. No",
		"",
		" Esc to cancel · Tab to amend",
	}
	for i, r := range rows {
		if strings.HasPrefix(r, "   "+highlight+". ") {
			rows[i] = " ❯" + r[2:]
		}
	}
	return strings.Join(rows, "\n")
}

func TestAToolPermissionDialogReportsWhatIsBeingApproved(t *testing.T) {
	p := parsePrompt(newScreen(permissionDialog("touch /tmp/probe && echo done", "1")))
	if p == nil {
		t.Fatal("the dialog was not recognised as a prompt")
	}
	for _, want := range []string{"Bash command", "touch /tmp/probe && echo done", "This command requires approval", "Do you want to proceed?"} {
		if !strings.Contains(p.Question, want) {
			t.Errorf("question %q lost %q", p.Question, want)
		}
	}
	if strings.ContainsRune(p.Question, dividerRune) {
		t.Errorf("question %q carries the divider", p.Question)
	}
	if strings.Contains(p.Question, "Running the command") {
		t.Errorf("question %q carries the transcript above the dialog", p.Question)
	}
	if len(p.Options) != 4 || p.Selected != 1 {
		t.Errorf("options = %q selected = %d", p.Options, p.Selected)
	}
	if k := classifyPromptKind(p); k != fleet.PromptToolPermission {
		t.Errorf("kind = %q, want %q", k, fleet.PromptToolPermission)
	}
}

// Two different commands behind the same leading word are two different asks.
// They used to share a question, options and so a nonce.
func TestToolPermissionsForDifferentCommandsHaveDistinctNonces(t *testing.T) {
	a := parsePrompt(newScreen(permissionDialog("touch /tmp/a", "1")))
	b := parsePrompt(newScreen(permissionDialog("touch /tmp/b", "1")))
	if a == nil || b == nil {
		t.Fatal("not recognised")
	}
	if a.Nonce == b.Nonce {
		t.Errorf("two commands share nonce %s", a.Nonce)
	}
}

// Moving the highlight is not a new question.
func TestMovingTheHighlightKeepsAToolPermissionNonce(t *testing.T) {
	a := parsePrompt(newScreen(permissionDialog("touch /tmp/a", "1")))
	b := parsePrompt(newScreen(permissionDialog("touch /tmp/a", "3")))
	if a == nil || b == nil {
		t.Fatal("not recognised")
	}
	if a.Nonce != b.Nonce {
		t.Errorf("nonce moved with the highlight: %s vs %s", a.Nonce, b.Nonce)
	}
	if b.Selected != 3 {
		t.Errorf("selected = %d, want 3", b.Selected)
	}
}

// A window that starts inside a tall command still reads from the dialog's
// opening rule, not from wherever the window happened to begin.
func TestATallCommandIsReadWholeFromItsOpeningRule(t *testing.T) {
	var cmd []string
	for i := 0; i < 20; i++ {
		cmd = append(cmd, "   echo step-"+string(rune('a'+i)))
	}
	screen := strings.Replace(permissionDialog("PLACEHOLDER", "1"), "   PLACEHOLDER", strings.Join(cmd, "\n"), 1)
	p := parsePrompt(newScreen(screen))
	if p == nil {
		t.Fatal("not recognised")
	}
	for _, want := range []string{"Bash command", "echo step-a", "echo step-t", "Do you want to proceed?"} {
		if !strings.Contains(p.Question, want) {
			t.Errorf("question lost %q: %q", want, p.Question)
		}
	}
}

// A dashed row with no dialog around it is not a rule: it must not fence a
// composer or bound a question. The question of an ordinary menu is unchanged.
func TestADashedRowInTheTranscriptIsNotARule(t *testing.T) {
	if isRule(strings.Repeat("╌", 40)) {
		t.Error("isRule accepts the dashed divider")
	}
	if !isDialogDivider(strings.Repeat("╌", 40)) || isDialogDivider("╌╌ x ╌╌") || isDialogDivider("╌╌") {
		t.Error("isDialogDivider misreads a divider")
	}
}

func TestCurlyApostropheDoesNotHideTheKind(t *testing.T) {
	p := &fleet.SessionPrompt{Options: []string{"Yes", "Yes, and don’t ask again for: ls", "No"}}
	if k := classifyPromptKind(p); k != fleet.PromptToolPermission {
		t.Errorf("kind = %q", k)
	}
}
