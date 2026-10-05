package tmux

import (
	"strconv"
	"strings"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
)

// This file reads the two things a question dialog paints that the rest of the
// classifier had no notion of (muster#204): a preview pane drawn beside
// the option list, and the header row above the question — a tab bar, or a
// single chip.
//
// # What was measured (one runtime build, live, on a private multiplexer)
//
// An AskUserQuestion whose options carry a preview draws the option list and a
// box side by side. The box's top border sits on the first option's row and
// its left edge stays in one column all the way down:
//
//	 ☐ Pick
//
//	 Which card do you want?
//
//	 ❯ 1. Option A                     ┌──────────────────────────────────────────┐
//	   2. Option B                     │ a mockup the agent wrote                 │
//	   3. Option C                     │                                          │
//	                                   └──────────────────────────────────────────┘
//
//	                                   Notes: press n to add notes
//
// Five properties of it decide everything below:
//
//   - A row the pane shares with an option is that option's label, then
//     padding, then the pane's cells. Read whole, the row is a label plus box
//     art, and it changes every time the highlight moves (the box shows the
//     highlighted option's preview), so the prompt's nonce never held still.
//   - The column the pane starts at is a SCREEN column, and a wide character
//     takes two of them while a capture emits it once: a Japanese label leaves
//     10 spaces before the box where an ASCII one leaves 21. So the pane is
//     found by the gap in front of it, never by a column number.
//   - A long label wraps inside the list column. Its continuation rows have no
//     number of their own and the pane's edge sits beside them like any other.
//   - The runtime clamps a pane to 24 content lines and paints a
//     `├─── ✂ ─── N lines hidden ───┤` row where it cut. The whole dialog is
//     then 27 rows of pane plus its chrome, which is more than promptScanDepth:
//     the option list falls out of the window and the blocked session read as
//     idle.
//   - The header is `←  ☐ Layout  ☐ Theme  ✔ Submit  →` on a multi-question
//     dialog and only ` ☐ Pick` on a single question. The highlighted tab is
//     painted with a background colour and no other mark, so a plain-text
//     capture cannot say which question is current; the raw one can.

const (
	// minPaneBorder is the fewest `─` a pane's top or bottom border may carry
	// for the row to count as one. A real pane is wide; this only keeps a
	// stray `┌┐` in prose from qualifying.
	minPaneBorder = 4
	// maxPaneRows bounds how far up a pane's top border is looked for. The
	// runtime clamps a pane to 24 content lines (measured), so this leaves room
	// for its borders and a divider without letting a screen walk the whole
	// scrollback.
	maxPaneRows = 40
	// paneFooterSlack is how many rows from the end of the screen the pane's
	// bottom border may sit: below it the dialog paints a blank row, a notes
	// hint, a rule, the chat row and the footer (7 rows, measured).
	paneFooterSlack = 14
	// previewHeaderRows is how many rows above a pane's top border the scan
	// window is widened to reach: the blank row, the question (which can wrap)
	// and the header chip above it.
	previewHeaderRows = 10
)

// previewPane is the row span of a validated side-by-side pane: the row that
// carries its top border and the row that carries its bottom one.
type previewPane struct{ top, bottom int }

func (p previewPane) contains(row int) bool { return row >= p.top && row <= p.bottom }

func isPaneLeftEdge(r rune) bool  { return r == '┌' || r == '│' || r == '├' || r == '└' }
func isPaneRightEdge(r rune) bool { return r == '┐' || r == '│' || r == '┤' || r == '┘' }

// splitPaneRow splits a row, escapes already stripped, into what stands to the
// left of a preview pane and the pane's own cells.
//
// The pane starts at the FIRST left-edge glyph that follows a gap of two or
// more spaces and runs to a right edge that ends the row. A label's own text
// comes before the gap, so a pane's inner content (box art of its own, say)
// can never be taken for the pane's start. prefix is returned as painted,
// spaces included, so a caller can keep the pane's column.
//
// The fragment must also be shaped like the pane's part of a row: a border row
// (`┌──┐` or `└──┘`) holds nothing but `─` between its corners, and a side row
// (`│ … │`) or divider (`├ … ┤`) is closed by its matching edge.
func splitPaneRow(line string) (prefix, frag string, ok bool) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" {
		return "", "", false
	}
	last, _ := utf8.DecodeLastRuneInString(line)
	if !isPaneRightEdge(last) {
		return "", "", false
	}
	runes := []rune(line)
	for i, r := range runes {
		if !isPaneLeftEdge(r) || i < 2 || runes[i-1] != ' ' || runes[i-2] != ' ' {
			continue
		}
		if paneFragment(runes[i:]) {
			return string(runes[:i]), string(runes[i:]), true
		}
	}
	return "", "", false
}

// paneFragment reports whether cells are a pane's part of one row.
func paneFragment(cells []rune) bool {
	if len(cells) < 2 {
		return false
	}
	l, r := cells[0], cells[len(cells)-1]
	switch {
	case l == '┌' && r == '┐', l == '└' && r == '┘':
		inner := cells[1 : len(cells)-1]
		if len(inner) < minPaneBorder {
			return false
		}
		for _, c := range inner {
			if c != ruleRune {
				return false
			}
		}
		return true
	case l == '│' && r == '│', l == '├' && r == '┤':
		return true
	}
	return false
}

// findPreviewPane looks for a pane at the bottom of a screen: the nearest
// bottom border, walked up through side rows to a top border of the same
// width, with a numbered option somewhere in between.
//
// Every one of those is required, because rows of `│` and `─` are also what a
// table or a diff in the agent's own output looks like, and a pane found in
// transcript would cut a real option list's rows. The pane is only believed
// when the box closes on both ends at one width AND sits beside an option.
func findPreviewPane(lines []string) (previewPane, bool) {
	n := len(lines)
	for b := n - 1; b >= 0 && b >= n-paneFooterSlack; b-- {
		_, frag, ok := splitPaneRow(lines[b])
		if !ok || !strings.HasPrefix(frag, "└") {
			continue
		}
		width := utf8.RuneCountInString(frag)
		for t := b - 1; t >= 0 && t >= b-maxPaneRows; t-- {
			_, f, ok := splitPaneRow(lines[t])
			if !ok {
				break
			}
			if strings.HasPrefix(f, "┌") {
				if utf8.RuneCountInString(f) != width || !optionBeside(lines, t, b) {
					break
				}
				return previewPane{top: t, bottom: b}, true
			}
			if !strings.HasPrefix(f, "│") && !strings.HasPrefix(f, "├") {
				break
			}
		}
	}
	return previewPane{}, false
}

// optionBeside reports whether any row of lines[top..bottom] carries a
// numbered option to the left of the pane.
func optionBeside(lines []string, top, bottom int) bool {
	for i := top; i <= bottom; i++ {
		prefix, _, ok := splitPaneRow(lines[i])
		if !ok {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(prefix), composerRuneMarker))
		if _, _, numbered := numberedOption(body); numbered {
			return true
		}
	}
	return false
}

// isDialogChip recognises the header a SINGLE-question dialog paints in place
// of a tab bar: one `☐ Header` (or `☒`) chip and nothing else.
func isDialogChip(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "☐ ") && !strings.HasPrefix(t, "☒ ") {
		return false
	}
	return strings.Count(t, "☐")+strings.Count(t, "☒") == 1 &&
		!strings.ContainsAny(t, "←→✔")
}

// isDialogHeader is either header shape.
func isDialogHeader(line string) bool { return isDialogTabBar(line) || isDialogChip(line) }

const submitTab = "✔ Submit"

// headerTabs lists a header's tabs as painted, arrows dropped: `☐ Layout`,
// `☒ Theme`, `✔ Submit`. A chip is one tab.
func headerTabs(plain string) []string {
	t := strings.TrimSpace(plain)
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "←"), "→"))
	var tabs []string
	for _, tab := range strings.Split(t, "  ") {
		if tab = strings.TrimSpace(tab); tab != "" {
			tabs = append(tabs, tab)
		}
	}
	return tabs
}

// tabPosition says where in a tabbed dialog the current question is. At is
// 1-based and 0 when it could not be read — the highlighted tab is the Submit
// one, no tab is highlighted, or the capture carries no escapes. Of counts the
// question tabs, Submit not among them.
type tabPosition struct{ At, Of int }

// dialogTabPosition reads a header row's tab position from the row as
// captured, escapes included.
//
// The current tab is found by WHERE the highlight sits, not by matching its
// label: two tabs can carry the same header text, and a redacted screen gives
// every tab the same placeholder.
func dialogTabPosition(raw string) tabPosition {
	visible, runs := paintedRuns(raw)
	tabs := headerTabs(visible)
	var pos tabPosition
	for _, tab := range tabs {
		if tab != submitTab {
			pos.Of++
		}
	}
	if len(runs) != 1 {
		return pos
	}
	i := tabsBefore(visible, runs[0].at)
	if i < 0 || i >= len(tabs) || tabs[i] == submitTab {
		return pos
	}
	for _, tab := range tabs[:i+1] {
		if tab != submitTab {
			pos.At++
		}
	}
	return pos
}

// dialogTabs reads a tabbed dialog's whole bar from the row as captured,
// escapes included (muster#242): one entry per question tab, Submit left out,
// and the 0-based index of the current one, or -1 when none of them is.
//
// ok is false — and the bar is not published at all — when it holds fewer than
// two question tabs, when a tab opens with neither glyph the runtime paints
// (a state read off an unrecognised shape would be a guess), or when no tab at
// all can be read as the highlighted one: with the current tab unknown the
// others' states are a guess too, because the current tab is painted ☐ like any
// pending one. A highlight on the Submit tab is a position that was read: the
// tabs come back, none of them current.
//
// The current tab is found by where the highlight sits (activeTab), never by
// its label, for the reason dialogTabPosition gives.
func dialogTabs(raw string) (tabs []fleet.PromptTab, current int, ok bool) {
	visible, _ := paintedRuns(raw)
	all := headerTabs(visible)
	at := activeTab(raw)
	if at < 0 || at >= len(all) {
		return nil, -1, false
	}
	current = -1
	for i, tab := range all {
		if tab == submitTab {
			continue
		}
		var state fleet.PromptTabState
		var header string
		switch {
		case strings.HasPrefix(tab, "☒"):
			state, header = fleet.PromptTabAnswered, strings.TrimPrefix(tab, "☒")
		case strings.HasPrefix(tab, "☐"):
			state, header = fleet.PromptTabPending, strings.TrimPrefix(tab, "☐")
		default:
			return nil, -1, false
		}
		if i == at {
			state, current = fleet.PromptTabCurrent, len(tabs)
		}
		tabs = append(tabs, fleet.PromptTab{Header: strings.TrimSpace(header), State: state})
	}
	if len(tabs) < 2 {
		return nil, -1, false
	}
	return tabs, current, true
}

// activeTab is the index, into headerTabs of the row's visible text, of the
// highlighted tab, or -1 when no single tab is.
func activeTab(raw string) int {
	visible, runs := paintedRuns(raw)
	if len(runs) != 1 {
		return -1
	}
	if i := tabsBefore(visible, runs[0].at); i < len(headerTabs(visible)) {
		return i
	}
	return -1
}

// tabsBefore counts the tabs that begin before a byte offset of the visible
// text: each one opens with its own glyph.
func tabsBefore(visible string, at int) int {
	if at > len(visible) {
		at = len(visible)
	}
	return strings.Count(visible[:at], "☐") + strings.Count(visible[:at], "☒") +
		strings.Count(visible[:at], "✔")
}

// paintedRun is one contiguous stretch of a row painted with a background or
// in reverse video: its text, trimmed, and the byte offset of the first
// visible character of the stretch in the row's visible text.
type paintedRun struct {
	text string
	at   int
}

// paintedRuns returns a row's visible text — every CSI sequence removed — and
// the runs of it painted with a background or in reverse video.
//
// The runtime marks the current tab with a background colour, and it is the
// only mark. A theme that draws the same tab in reverse video would read the
// same here, which is why this does not look for one colour.
func paintedRuns(raw string) (visible string, runs []paintedRun) {
	var vis, cur strings.Builder
	on := false
	start := 0
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			runs = append(runs, paintedRun{text: t, at: start})
		}
		cur.Reset()
	}
	for i := 0; i < len(raw); {
		if raw[i] == sgrEscape && i+1 < len(raw) && raw[i+1] == '[' {
			j := i + 2
			for j < len(raw) && (raw[j] < '@' || raw[j] > '~') {
				j++
			}
			if j >= len(raw) {
				break
			}
			if raw[j] == 'm' {
				was := on
				on = sgrHighlightAfter(raw[i+2:j], on)
				if !was && on {
					start = vis.Len()
				}
				if was && !on {
					flush()
				}
			}
			i = j + 1
			continue
		}
		vis.WriteByte(raw[i])
		if on {
			cur.WriteByte(raw[i])
		}
		i++
	}
	flush()
	return vis.String(), runs
}

// sgrHighlightAfter applies one SGR parameter list to "is a background or
// reverse video on". Extended colours (38/48 with 5;N or 2;R;G;B) carry
// arguments that must be skipped whole, or the 7 in `38;5;7` reads as reverse.
func sgrHighlightAfter(params string, on bool) bool {
	if params == "" {
		return false
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		n, err := strconv.Atoi(ps[i])
		if err != nil {
			continue
		}
		switch {
		case n == 0 || n == 27 || n == 49:
			on = false
		case n == 7 || (n >= 40 && n <= 47) || (n >= 100 && n <= 107):
			on = true
		case n == 38 || n == 48:
			if n == 48 {
				on = true
			}
			if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			} else {
				i += 2
			}
		}
	}
	return on
}

// menuShape is what answering a menu depends on beyond its options — the
// facts the classifier reads off the screen that a caller's Choice cannot
// carry, because they are how THIS runtime takes an answer, not what is asked.
type menuShape struct {
	// unnumbered: the options carry no numbers, so a digit does nothing
	// (muster#171).
	unnumbered bool
	// preview: the options are drawn beside a preview pane, and there a digit
	// only MOVES the highlight — Enter is what commits it (muster#204).
	preview bool
	// tab is where the current question sits in a tabbed dialog.
	tab tabPosition
	// headerAt is the 1-based screen row of the dialog's header (its tab bar or
	// chip), or 0 when none was found. It is what pendingAgentText measures
	// from: the header sits directly under the dialog's opening rule, so the
	// rows above that rule are the transcript the dialog was drawn under
	// (muster#266). One-based so the zero value means "no header".
	headerAt int
	// shortcuts is the key that answers each option, aligned with the options,
	// for a prompt whose keys are not its options' positions: the feedback-draft
	// card offers ["review","send","dismiss"] and answers them with 1, 2 and 0
	// (muster#215). Nil for every menu, where a digit is the option's
	// number.
	shortcuts []string
}
