package tmux

import (
	"strings"
	"testing"
)

// #333: the versions here are the ones measured with a pane that had sent
// DECSET 2004 — empty under 3.4, 3.5a, 3.6 and 3.6a, "1" from 3.7.
func TestTmuxProvidesBracketPasteFlag(t *testing.T) {
	for _, tc := range []struct {
		in             string
		provides, know bool
	}{
		{"tmux 3.4", false, true},
		{"tmux 3.5a", false, true},
		{"tmux 3.6", false, true},
		{"tmux 3.6a\n", false, true},
		{"tmux 3.7", true, true},
		{"tmux 3.7c", true, true},
		{"tmux 3.10", true, true},
		{"tmux 4.0", true, true},
		{"tmux 2.9a", false, true},
		{"tmux 1.8", false, true},
		{"tmux next-3.8", false, false},
		{"tmux master", false, false},
		{"", false, false},
		{"command not found", false, false},
	} {
		provides, known := TmuxProvidesBracketPasteFlag(tc.in)
		if provides != tc.provides || known != tc.know {
			t.Errorf("TmuxProvidesBracketPasteFlag(%q) = (%v, %v), want (%v, %v)", tc.in, provides, known, tc.provides, tc.know)
		}
	}
}

// G2 must say the multiplexer is the cause when the flag reads empty on a tmux
// that cannot answer it, and must not when the multiplexer could have answered.
func TestBracketFlagFailureSeparatesTheMultiplexerFromTheCandidate(t *testing.T) {
	old := bracketFlagFailure("", "tmux 3.4")
	for _, want := range []string{"tmux 3.4", "does not provide", "3.7", "multiplexer's version"} {
		if !strings.Contains(old, want) {
			t.Errorf("empty flag on tmux 3.4: %q lacks %q", old, want)
		}
	}
	for name, got := range map[string]string{
		"declined":              bracketFlagFailure("0", "tmux 3.4"),
		"empty on a new tmux":   bracketFlagFailure("", "tmux 3.7a"),
		"empty, version unread": bracketFlagFailure("", ""),
	} {
		if name == "empty, version unread" {
			if !strings.Contains(got, "this tmux") {
				t.Errorf("%s: %q should still name the multiplexer", name, got)
			}
			continue
		}
		if strings.Contains(got, "multiplexer's version") {
			t.Errorf("%s: %q blames the multiplexer's version", name, got)
		}
	}
}
