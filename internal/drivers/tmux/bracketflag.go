package tmux

import (
	"fmt"
	"strconv"
	"strings"
)

// BracketPasteFlagMajor and BracketPasteFlagMinor name the first tmux release
// that can answer `#{bracket_paste_flag}` (#333).
//
// Measured, not remembered: a pane whose occupant had sent DECSET 2004 read ""
// under 3.4, 3.5a, 3.6 and 3.6a and "1" under 3.7, 3.7a and 3.7b; tmux's own
// source adds the format variable in 3.7 and has none that exposes the pane's
// bracketed-paste mode before it. tmux prints an empty string for a format
// variable it does not know, so on an older tmux the answer is not "the pane
// asked for no bracketed paste" — it is "this tmux cannot tell".
const (
	BracketPasteFlagMajor = 3
	BracketPasteFlagMinor = 7
)

// BracketPasteFlagSince is BracketPasteFlagMajor.BracketPasteFlagMinor, the
// form an operator is told.
const BracketPasteFlagSince = "3.7"

// ParseTmuxVersion reads the output of `tmux -V` ("tmux 3.4", "tmux 3.5a").
// ok is false for anything without a leading major.minor — a development build
// ("tmux next-3.8", "tmux master") is deliberately not guessed at.
func ParseTmuxVersion(out string) (major, minor int, ok bool) {
	s := strings.TrimSpace(out)
	s = strings.TrimSpace(strings.TrimPrefix(s, "tmux"))
	dot := strings.IndexByte(s, '.')
	if dot <= 0 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(s[:dot])
	if err != nil {
		return 0, 0, false
	}
	rest := s[dot+1:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(rest[:end])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// TmuxProvidesBracketPasteFlag reports whether a tmux that printed versionOut
// for `-V` answers `#{bracket_paste_flag}`. known is false when the output is
// not a release version, in which case provides carries no information.
func TmuxProvidesBracketPasteFlag(versionOut string) (provides, known bool) {
	major, minor, ok := ParseTmuxVersion(versionOut)
	if !ok {
		return false, false
	}
	if major != BracketPasteFlagMajor {
		return major > BracketPasteFlagMajor, true
	}
	return minor >= BracketPasteFlagMinor, true
}

// tmuxVersionText is the multiplexer version this run saw, or "" before a
// world exists (the unit tests that build a harness by hand).
func (h *compatHarness) tmuxVersionText() string {
	if h.world == nil {
		return ""
	}
	return h.world.tmuxVersion
}

// bracketFlagFailure is the G2 finding for a composer whose bracketed-paste
// flag did not read "1". An empty answer is split out: it says the multiplexer
// cannot report the flag, not that the candidate declined bracketed paste, and
// the two have different remedies (#333).
func bracketFlagFailure(flag, tmuxVersion string) string {
	// A multiplexer that does provide the variable and still printed nothing
	// failed the query itself; that is not the version finding, so it keeps the
	// plain wording.
	if provides, known := TmuxProvidesBracketPasteFlag(tmuxVersion); flag != "" || (known && provides) {
		return fmt.Sprintf("#{bracket_paste_flag} reads %q at the composer, so the driver refuses to paste", flag)
	}
	have := "this tmux"
	if tmuxVersion != "" {
		have = tmuxVersion
	}
	return fmt.Sprintf("#{bracket_paste_flag} reads \"\" at the composer: %s does not provide that variable (it first exists in tmux %s), "+
		"so the driver cannot tell whether the candidate asked for bracketed paste and refuses to paste — this is the multiplexer's version, not the candidate; "+
		"run the check with tmux %s or newer (FLEET_TMUX_BIN)", have, BracketPasteFlagSince, BracketPasteFlagSince)
}

// flagUnreadableByVersion reports whether an empty flag is explained by the
// multiplexer's release: it is a version older than 3.7, which cannot answer
// (#334). A flag that read anything else, or a release that could have
// answered or is not recognised, is not.
func flagUnreadableByVersion(flag, tmuxVersion string) bool {
	if flag != "" {
		return false
	}
	provides, known := TmuxProvidesBracketPasteFlag(tmuxVersion)
	return known && !provides
}

// bracketedArrivalProof is G2's evidence that a paste was bracketed when the
// flag cannot be read: only a bracketed paste collapses into the runtime's
// marker, so a 40-line paste and a 900-byte single line that both did were
// delivered bracketed (#334). A draft that did not run is a finding too.
func bracketedArrivalProof(d40, d900 *compatDraft) []string {
	var bad []string
	for _, c := range []struct {
		name string
		d    *compatDraft
	}{{"40-line", d40}, {"900-byte", d900}} {
		if !c.d.ran {
			bad = append(bad, c.name+" draft not made, so bracketed arrival was not shown: "+c.d.skipped)
		}
	}
	if len(bad) > 0 {
		return bad
	}
	return collapseFindings(d40, d900)
}
