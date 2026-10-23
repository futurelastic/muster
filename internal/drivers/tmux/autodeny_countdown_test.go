package tmux

import (
	"strings"
	"testing"
	"time"
)

// muster#353: recent runtime versions add an unattended-session countdown to a
// tool-permission dialog. It sat in the question and so in the nonce, which
// then moved every second and refused every /respond.

func countdownDialog(countdown string) string {
	d := permissionDialog("touch /tmp/probe && echo done", "1")
	if countdown == "" {
		return d
	}
	row := " ⚠ Claude Code will automatically deny this request in " + countdown +
		", to avoid blocking progress on an unattended session"
	return strings.Replace(d, " Do you want to proceed?", row+"\n Do you want to proceed?", 1)
}

func freezeClock(t *testing.T, at time.Time) {
	t.Helper()
	old := promptNow
	promptNow = func() time.Time { return at }
	t.Cleanup(func() { promptNow = old })
}

func TestTheAutoDenyCountdownDoesNotMoveTheNonceOrTheQuestion(t *testing.T) {
	a := parsePrompt(newScreen(countdownDialog("1:13")))
	b := parsePrompt(newScreen(countdownDialog("1:11")))
	plain := parsePrompt(newScreen(countdownDialog("")))
	if a == nil || b == nil || plain == nil {
		t.Fatal("the dialog was not recognised as a prompt")
	}
	if a.Nonce != b.Nonce {
		t.Errorf("nonce moved with the countdown: %s vs %s", a.Nonce, b.Nonce)
	}
	if a.Question != b.Question {
		t.Errorf("question moved with the countdown: %q vs %q", a.Question, b.Question)
	}
	if a.Nonce != plain.Nonce || a.Question != plain.Question {
		t.Errorf("a dialog with a countdown differs from the same one without: %q/%s vs %q/%s",
			a.Question, a.Nonce, plain.Question, plain.Nonce)
	}
	if strings.Contains(a.Question, "automatically deny") {
		t.Errorf("question %q carries the countdown", a.Question)
	}
	if !strings.Contains(a.Question, "This command requires approval Do you want to proceed?") {
		t.Errorf("question %q lost the rows around the countdown", a.Question)
	}
	if len(a.Options) != 4 || a.Selected != 1 {
		t.Errorf("options = %q selected = %d", a.Options, a.Selected)
	}
}

func TestTheAutoDenyDeadlineIsReportedAndOnlyWhenShown(t *testing.T) {
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	freezeClock(t, now)
	p := parsePrompt(newScreen(countdownDialog("1:13")))
	if p == nil || p.AutoDenyAt == nil {
		t.Fatalf("no deadline reported: %+v", p)
	}
	if want := now.Add(73 * time.Second); !p.AutoDenyAt.Equal(want) {
		t.Errorf("autoDenyAt = %s, want %s", p.AutoDenyAt, want)
	}
	if plain := parsePrompt(newScreen(countdownDialog(""))); plain.AutoDenyAt != nil {
		t.Errorf("a dialog with no countdown reports a deadline: %s", plain.AutoDenyAt)
	}
	if s := parsePrompt(newScreen(countdownDialog("0:05"))); s.AutoDenyAt == nil || !s.AutoDenyAt.Equal(now.Add(5*time.Second)) {
		t.Errorf("0:05 read as %v", s.AutoDenyAt)
	}
}

// A dialog that differs in what it asks is still a different dialog.
func TestADifferentDialogUnderACountdownHasADifferentNonce(t *testing.T) {
	a := parsePrompt(newScreen(countdownDialog("1:13")))
	other := strings.Replace(countdownDialog("1:13"), "   4. No", "   4. No, and tell Claude why", 1)
	b := parsePrompt(newScreen(other))
	if a == nil || b == nil {
		t.Fatal("not recognised")
	}
	if a.Nonce == b.Nonce {
		t.Errorf("different options share nonce %s", a.Nonce)
	}
	c := parsePrompt(newScreen(strings.Replace(countdownDialog("1:13"), "touch /tmp/probe", "touch /tmp/other", 1)))
	if c.Nonce == a.Nonce {
		t.Errorf("a different command shares nonce %s", a.Nonce)
	}
}

func TestTheCountdownIsMatchedLooselyAroundItsAnchor(t *testing.T) {
	for _, row := range []string{
		"⚠ Claude Code will automatically deny this request in 1:13, to avoid blocking progress",
		"│ Claude will automatically deny this request in 12:05 │",
		"AUTOMATICALLY DENY THIS REQUEST IN 0:09",
	} {
		if _, ok := autoDenyCountdown(row); !ok {
			t.Errorf("%q not read as a countdown", row)
		}
	}
	for _, row := range []string{"Do you want to proceed?", "automatically deny this request", "deny this request in 1:13"} {
		if _, ok := autoDenyCountdown(row); ok {
			t.Errorf("%q read as a countdown", row)
		}
	}
}
