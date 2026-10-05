package tmux

import (
	"regexp"
	"strings"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
)

// The agent's text above an open prompt (muster #266).
//
// # The gap this closes
//
// The runtime writes an assistant message to its record only when the message is
// finished, and a message that ends in an open question is not finished until
// the question is answered. While the dialog is up, the record holds none of the
// message, so a client reading Turns shows the question without the prose it
// refers to. The prose is on the screen, immediately above the dialog.
//
// # What is read, and what is refused
//
// The dialog is anchored from its HEADER (the tab bar or the chip), the one row
// parseMenuShape already corroborates, and the header sits directly under the
// dialog's opening rule. Above that rule is the transcript the dialog was drawn
// under. The block read is the run of rows ending there, from the response
// bullet that heads it.
//
// Every refusal is "absent", never a guess — the same boundary Turn keeps:
//
//   - no open prompt, an unnumbered or preview-pane menu, a prompt with no
//     header (a tool-permission dialog's rows above are transcript with no
//     anchor), or a header not directly under a rule;
//   - a row in the block that is not the bullet, a blank, or an indented
//     continuation: a status line, a composer, a user message, a rule;
//   - a tool-result row anywhere in the block, or a head that has the shape of a
//     tool call or a collapsed tool summary;
//   - a block whose head ran above the captured window, unless every visible row
//     is indented exactly as a wrapped prose row is (see below).
//
// # The indent
//
// The runtime draws agent prose as `⏺ <first row>` and every continuation row
// two columns in. A tool result's continuation rows sit deeper (`  ⎿  …` and then
// five columns), so when the head is NOT visible the only continuation rows
// trusted are the ones at exactly two columns. With the head visible a deeper
// row is the agent's own (a code block, a nested list) and is kept.
const (
	// pendingContinuationIndent is how far in the runtime draws every row of a
	// text block after the one carrying the bullet.
	pendingContinuationIndent = 2

	// toolResultMarker leads the first row of a tool's output.
	toolResultMarker = "⎿"
)

// toolCallHead matches `Name(arguments)`, the way a tool call is drawn behind
// the same bullet agent text is. A prose sentence that happens to start the same
// way is left out too, which is the side of the line this is allowed to be wrong
// on.
var toolCallHead = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*\(`)

// pendingAgentText returns the agent's own text above the prompt open on this
// screen, the truncation flag, and the prompt's nonce. ok is false for every
// case described above, which includes "no prompt is open".
func pendingAgentText(s screen) (text string, truncated bool, nonce string, ok bool) {
	p, shape := parseMenuShape(s)
	if p == nil || p.Nonce == "" || shape.headerAt == 0 || shape.unnumbered || shape.preview || shape.shortcuts != nil {
		return "", false, "", false
	}
	i := shape.headerAt - 2 // the row above the header
	for i >= 0 && blankRow(s.lines[i]) {
		i--
	}
	if i < 0 || !isRule(s.lines[i]) {
		return "", false, "", false
	}
	i-- // above the opening rule
	for i >= 0 && blankRow(s.lines[i]) {
		i--
	}

	var rows []string // bottom-up
	head := -1
	for ; i >= 0; i-- {
		line := s.lines[i]
		if blankRow(line) {
			rows = append(rows, "")
			continue
		}
		if strings.HasPrefix(line, responseBullet) {
			head = i
			break
		}
		if indentOf(line) < pendingContinuationIndent || strings.HasPrefix(strings.TrimSpace(line), toolResultMarker) {
			return "", false, "", false
		}
		rows = append(rows, line)
	}

	var first string
	if head >= 0 {
		first = strings.TrimSpace(strings.TrimPrefix(s.lines[head], responseBullet))
		if first == "" || toolCallHead.MatchString(first) ||
			strings.Contains(first, "(MCP)") || strings.Contains(first, "ctrl+o") {
			return "", false, "", false
		}
	} else {
		// The head is above the window. Trust only rows indented the way a
		// wrapped prose row is, and say the block is cut off.
		for _, r := range rows {
			if r != "" && indentOf(r) != pendingContinuationIndent {
				return "", false, "", false
			}
		}
		truncated = true
	}

	out := make([]string, 0, len(rows)+1)
	if head >= 0 {
		out = append(out, first)
	}
	for j := len(rows) - 1; j >= 0; j-- {
		r := rows[j]
		if r != "" {
			r = r[min(indentOf(r), pendingContinuationIndent):]
		}
		out = append(out, r)
	}
	text = strings.TrimSpace(strings.Join(out, "\n"))
	if text == "" {
		return "", false, "", false
	}
	if len(text) > fleet.MaxTurnTextBytes {
		cut := fleet.MaxTurnTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text, truncated = text[:cut], true
	}
	return text, truncated, p.Nonce, true
}

func blankRow(line string) bool { return strings.TrimSpace(line) == "" }

// indentOf is the number of leading spaces on a row.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}
