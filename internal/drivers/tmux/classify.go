package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
)

// This file holds everything that reads a terminal screen and guesses what
// the agent behind it is doing. It is deliberately the only such place in
// the driver: §5.1 says the interface expresses questions ("state") and
// never mechanisms ("readScreen"), so the mechanism is quarantined here
// rather than smeared across the operations.
//
// # Why almost nothing here is certain
//
// Every function below returns fleet.ConfidenceInferred, never
// ConfidenceObserved, and that is not modesty — it is measured. Claude
// Code's TUI signals that a turn is in progress with a spinner line whose
// verb is drawn at random from a large set ("Zigzagging", "Brewed",
// "Sautéed", "Worked"), and distinguishes running from finished by that
// verb's grammatical tense plus the shape of its suffix:
//
//	✻ Zigzagging… (5m 57s · ↓ 21.3k tokens)   <- running
//	✻ Worked for 2m 7s                         <- finished
//
// A driver keying on that is keying on the tense of a randomly chosen
// English word in a UI with no compatibility contract. It works today. It
// is one release note away from being wrong, and — this is the part that
// matters — wrong *silently*, because a missing spinner reads exactly like
// a finished turn.
//
// So the classifier is built to fail toward fleet.StatusUnknown rather than
// toward a plausible answer (§5.6, "degrade, never emulate"). A caller that
// sees unknown can go look; a caller that sees a confident "idle" that is
// actually "working" cannot. §2.3 makes unknown a first-class answer for
// exactly this situation, and this driver is the reason to believe that was
// the right call.
//
// # The signals that ARE structural
//
// Three do not depend on prose, and they carry the weight:
//
//   - the composer box — the "❯ " line fenced between two horizontal rules —
//     is a layout feature, not a message. Text in it is input a human typed
//     and has not submitted, which is the §2.4 refusal case.
//   - the selection footer ("Enter to select · Tab/Arrow keys to navigate")
//     is emitted by a menu widget and means the session is blocked on a
//     human choosing, which is unambiguously waiting_input.
//   - process liveness comes from the OS, not the screen.

// SGR 2 is "dim/faint". The TUI renders the composer's PLACEHOLDER hint dim
// and real typed input at normal intensity, which is the only reliable way to
// tell them apart.
//
// Matching the hint's words instead would repeat the spinner-verb mistake:
// prose in an interface with no compatibility contract. Intensity is a
// rendering attribute the TUI must use for the hint to look like a hint.
const (
	sgrDimOn  = "\x1b[2m"
	sgrReset  = "\x1b[0m"
	sgrEscape = '\x1b'
	// oscBEL is the older of the two OSC terminators. Both are in use on this
	// substrate, so both are recognised — see stripEscapes.
	oscBEL = '\x07'
)

const (
	// composerRuneMarker begins the composer (input) line of the TUI.
	composerRuneMarker = "❯"
	// ruleRune is the box-drawing character the TUI fences the composer
	// with. Matching a run of them, rather than an exact width, keeps this
	// independent of terminal width.
	ruleRune = '─'
	// promptScanDepth is how far up from the bottom a prompt may reach. A
	// numbered list further up is transcript, not a question.
	promptScanDepth = 24

	// maxPromptOptions bounds how many options a prompt may enumerate. See
	// parsePrompt: the index comes from the screen, and the screen is
	// attacker-influenced in the general case.
	maxPromptOptions = 32

	// unanchoredQuestionRows is how many rows above its options a question keeps
	// when nothing says where it starts. Without the dialog's header the rows
	// above the options can be the transcript the dialog was drawn under, and
	// only the last few are its ask.
	unanchoredQuestionRows = 3
	// maxQuestionRows bounds a question whose start IS known — the dialog's
	// header was found, so every row between it and the options is the
	// question (muster#220). The screen bounds it already, but a pane an
	// agent writes to can be as tall as the agent likes, and this text is
	// reported to every reader of the session's state, digested into its nonce
	// and quoted in refusals. A longer question keeps the rows nearest the
	// options, where its ask sits.
	maxQuestionRows = 32

	// Menu footers. There is more than one, which cost a real incident: the
	// detector knew only "Enter to select", so a folder-trust prompt and a
	// session-resume prompt — both saying "Enter to confirm" — classified as
	// unknown instead of waiting_input. A supervisor cannot tell "blocked on
	// a question" from "I cannot read this screen", so it waits forever on
	// something that will never move by itself.
	selectFooter  = "Enter to select"
	confirmFooter = "Enter to confirm"
	// A tool-permission dialog uses neither of the above. Four footers on one
	// runtime is why detection is structural first and footer second.
	amendFooter = "Tab to amend"
	// spinnerRune is ONE of the glyphs that prefix the status line — see
	// hasSpinnerGlyph for why matching this exact character was a bug. Kept
	// as a fixture anchor for tests, not used for detection.
	spinnerRune = "✻"
	// runningSuffixMarker distinguishes a running spinner ("… (5m 57s · ↓
	// 21.3k tokens)") from a finished one ("for 2m 7s"). The ellipsis is
	// a single rune in the TUI's output, not three periods.
	runningSuffixMarker = "…"
	// finishedInfix is the finished spinner's shape: "<Verb> for <duration>".
	finishedInfix = " for "
	// responseBullet marks a line the AGENT produced. Chrome does not carry
	// it, which makes it the divider between "the runtime said this just now"
	// and "the agent has since carried on".
	responseBullet = "⏺"
)

// screen is a pane's captured text, split into non-empty trailing lines
// with trailing whitespace removed. The classifier only ever looks at the
// tail: scrollback above the composer is transcript, and transcript is
// whatever the agent chose to print — including, potentially, text designed
// to look like the TUI's own chrome.
type screen struct {
	lines []string
	// raw keeps the escape sequences, because one signal cannot be read
	// from the text alone: the composer's placeholder is rendered dim.
	raw []string
	// visibleTop is the index of the first row of the VISIBLE pane. Rows
	// above it came from the `-S -N` history margin: frozen scrollback, which
	// a runtime redrawing its composer in place cannot update, and which on a
	// runtime using the alternate screen predates that screen altogether
	// (muster#169). Zero means the pane height was not known when the
	// capture was taken, and every row is treated as it always was.
	visibleTop int
	// blankBelow is how many blank rows the capture ended with, dropped from
	// lines above. capture-pane pads to the pane's height, so a capture whose
	// last non-blank row is ALSO its last row (blankBelow 0) has that row on the
	// pane's bottom edge: nothing more can be drawn below it because there is
	// no row below it (muster#216). A screen with blank rows under its
	// last content is a different fact — the runtime had room and painted
	// nothing there — and is never read as a row cut off by the pane.
	blankBelow int
}

func newScreen(raw string) screen {
	return newScreenVisible(raw, 0)
}

// newScreenVisible is newScreen for a capture whose pane height is known.
// capture-pane prints one newline-terminated row per pane row, history margin
// first, so the visible pane is the last paneHeight rows of the output. That
// boundary is counted BEFORE trailing blank rows are dropped, because dropping
// them only shortens the tail and never renumbers a row above it.
func newScreenVisible(raw string, paneHeight int) screen {
	s := splitScreen(raw)
	if paneHeight > 0 {
		rows := strings.Count(raw, "\n")
		if raw != "" && !strings.HasSuffix(raw, "\n") {
			rows++
		}
		if rows > paneHeight {
			s.visibleTop = rows - paneHeight
		}
	}
	return s
}

func splitScreen(raw string) screen {
	all := strings.Split(raw, "\n")
	out := make([]string, 0, len(all))
	raws := make([]string, 0, len(all))
	for _, l := range all {
		raws = append(raws, l)
		out = append(out, strings.TrimRight(stripEscapes(l), " \t\r"))
	}
	// Drop trailing blank lines: capture-pane pads to the pane height.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
		raws = raws[:len(raws)-1]
	}
	// The trailing newline of a capture ends its last row; it is not a row.
	dropped := len(all) - len(out)
	if strings.HasSuffix(raw, "\n") {
		dropped--
	}
	return screen{lines: out, raw: raws, blankBelow: max(dropped, 0)}
}

// stripEscapes removes the ANSI sequences a pane carries so text matching works
// on what a human sees rather than on how it is painted.
//
// Two families, because both were found on one screen:
//
//   - CSI (`ESC [ … m/K/H`) — colour and cursor movement. Matching without
//     removing these was the original reason this function exists.
//
//   - OSC (`ESC ] … BEL` or `ESC ] … ESC \`) — chiefly the hyperlink the
//     runtime wraps around link text. This one was NOT removed, and unlike a
//     colour code it does not merely disturb matching: the surviving bytes are
//     PUBLISHED. Measured on a live fleet, a folder-trust prompt's question
//     reached a client as
//
//     …only proceed if you trust this configuration. \x1b]8;id=…;https://…\x1b\\Security guide\x1b]8;;\x1b\\
//
//     A client rendering that shows escape debris to a human at exactly the
//     moment it is asking them to make a security decision.
//
// Both are consumed only when TERMINATED. A capture can cut a sequence at the
// right edge of the pane, and a scanner that swallowed to end-of-line on an
// unterminated one would eat visible text — silently, and the text it would eat
// is the text being classified.
func stripEscapes(s string) string {
	if !strings.ContainsRune(s, sgrEscape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == sgrEscape && i+1 < len(s) {
			switch s[i+1] {
			case '[':
				j := i + 2
				for j < len(s) && s[j] != 'm' && s[j] != 'K' && s[j] != 'H' {
					j++
				}
				if j < len(s) {
					i = j + 1
					continue
				}
			case ']':
				// OSC ends at BEL, or at ST (`ESC \`).
				if j, ok := oscEnd(s, i+2); ok {
					i = j
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// oscEnd finds the index just past an OSC terminator, or reports that the
// sequence is unterminated in this fragment.
func oscEnd(s string, from int) (int, bool) {
	for j := from; j < len(s); j++ {
		if s[j] == oscBEL {
			return j + 1, true
		}
		if s[j] == sgrEscape && j+1 < len(s) && s[j+1] == '\\' {
			return j + 2, true
		}
	}
	return 0, false
}

// allDim reports whether every visible character of a raw fragment is
// rendered dim — the signature of a placeholder rather than typed input.
//
// The caller passes the text AFTER the prompt marker, not the whole line: the
// marker is painted at normal intensity even when the hint beside it is dim,
// so including it makes every placeholder look partly real. That detail cost a
// test, and would have cost the fix.
//
// Returns false for a fragment with no escape sequences: plain text is normal
// intensity, which is what real input looks like.
func allDim(raw string) bool {
	if !strings.Contains(raw, sgrDimOn) {
		return false
	}
	var visible, dim int
	depth := 0
	rs := []rune(raw)
	for i := 0; i < len(rs); {
		if rs[i] == sgrEscape && i+1 < len(rs) && rs[i+1] == '[' {
			j := i + 2
			for j < len(rs) && rs[j] != 'm' {
				j++
			}
			if j < len(rs) {
				switch string(rs[i : j+1]) {
				case sgrDimOn:
					depth++
				case sgrReset:
					depth = 0
				}
				i = j + 1
				continue
			}
		}
		if !unicode.IsSpace(rs[i]) {
			visible++
			if depth > 0 {
				dim++
			}
		}
		i++
	}
	return visible > 0 && dim == visible
}

// afterMarker returns the raw fragment following the composer prompt glyph,
// escapes intact.
func afterMarker(raw string) string {
	if i := strings.Index(raw, composerRuneMarker); i >= 0 {
		return raw[i+len(composerRuneMarker):]
	}
	return raw
}

// isRule reports whether a line is one of the TUI's horizontal rules. The
// rule may carry a centred label (the session name), so this checks that
// the line is made predominantly of the box-drawing rune rather than
// requiring it to be uniform.
func isRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	rules := 0
	for _, r := range trimmed {
		if r == ruleRune {
			rules++
		}
	}
	// A small absolute count, and nothing proportional.
	//
	// Both stricter rules were tried and both failed on real screens. An
	// absolute width (eight) missed the ONE fence that carries content: the
	// opening rule is labelled with the session name, so a long name in a
	// narrow pane leaves few rule characters. Requiring rule characters to
	// outnumber the label failed for the same reason — the label is often
	// longer than the dashes around it.
	//
	// Failing to recognise a fence is the damaging direction. It means the
	// composer is not fenced, so unsent input goes undetected and gets
	// concatenated into a message a human was still typing — invisible when
	// it happens. Over-recognising merely refuses a send, which is visible
	// and recoverable.
	//
	// This character is box-drawing (U+2500); prose does not contain it, and
	// an em dash is a different rune entirely. Transcript tables do contain
	// it, which is harmless: the composer is anchored from the bottom of the
	// screen, and a table is always above it.
	return rules >= 3
}

// composerScan is composerSpan's three-valued verdict. A caller must never
// collapse composerClipped into either of the other two: only composerFound
// and composerAbsent license any inference about what the composer holds
// (muster#134).
type composerScan int

const (
	// composerFound: prompt and last are meaningful — a fenced composer was
	// located in full.
	composerFound composerScan = iota
	// composerAbsent: the screen structurally has no composer. Distinct from
	// composerClipped below — this is a POSITIVE finding (a rule with no
	// marker between it and another rule, or a real non-rule line sitting
	// where an opening fence would have to be), not merely "search ran out
	// of rows".
	composerAbsent
	// composerClipped: the search ran off the TOP of the captured lines
	// before it could rule the composer either in or out — the capture
	// ended above wherever this composer's opening fence and/or ❯-marked
	// row actually are. This is a statement about what this driver could
	// read, not about the session: a composer may be sitting there, taller
	// than captureForClassify's window (muster#134's field case: ~80
	// on-screen rows against a ~24-row scrollback margin). Callers must
	// treat this as "cannot tell", never as "empty" and never as "found
	// holding nothing" — see composerText's own contract.
	composerClipped
)

// composerSpan locates a composer's structural boundaries in s: prompt is
// the index of its ❯-marked line, last is the index of its closing rule.
// Both are meaningless unless scan is composerFound — this walks the
// identical rule/marker search composerText always has, extracted so a
// caller that needs the composer's SHAPE (composerVisualLines) is not
// forced to re-implement the search rather than just its result.
//
// The composer is identified structurally: a line beginning with the prompt
// marker that sits between two horizontal rules. Finding it by structure
// rather than by "the line starts with ❯" matters, because the transcript
// above also contains ❯-prefixed lines — they are how the TUI echoes
// commands the human already ran (see the /remote-control fixture). Those
// are history. Only the fenced one is live input.
func composerSpan(s screen) (prompt, last int, scan composerScan) {
	// The pane's bottom edge cutting the composer's closing rule off. Asked
	// first, because the walk below cannot answer it: it takes the LAST rule on
	// screen for the closing fence, and here that rule is the OPENING one, so it
	// walks up out of the composer into whatever sits above it (muster#216).
	if bottomCutComposer(s) {
		return 0, 0, composerClipped
	}

	// Walk back to the closing rule.
	last = -1
	for i := len(s.lines) - 1; i >= 0; i-- {
		if isRule(s.lines[i]) {
			last = i
			break
		}
	}
	if last <= 0 {
		// No rule at all, or a rule sitting in the very first row with
		// nothing above it to be a composer's interior. Not classified as
		// clipped (muster#134 draws that line at the two walks below,
		// which at least have a rule to walk up FROM) — a screen with no
		// rule in view at all is the ordinary "not the TUI" shape most
		// captures are, and treating every one of those as clipped would
		// make the clipped predicate a catch-all, exactly what it must not
		// be (see composerClipped's own doc comment).
		return 0, 0, composerAbsent
	}

	// Find a prompt-marked line above it, stopping at another rule.
	prompt = -1
	for i := last - 1; i >= 0; i-- {
		if isRule(s.lines[i]) {
			return 0, 0, composerAbsent // opening rule reached with no composer between
		}
		if strings.HasPrefix(strings.TrimSpace(s.lines[i]), composerRuneMarker) {
			prompt = i
			break
		}
	}
	if prompt < 0 {
		// Ran off the top of the CAPTURE without hitting either a rule
		// (which would have meant composerAbsent, above — this driver
		// looked and found no composer between the two rules) or a ❯-marked
		// row. Those are different findings: this one never got to look,
		// because the capture ended first. muster#134's own fixture is
		// exactly this shape — a tall composer's tail, no opening fence and
		// no prompt row anywhere in the captured lines.
		return 0, 0, composerClipped
	}

	// THE COMPOSER MUST BE FENCED, and this check is the whole reason this
	// function is not two lines long.
	//
	// The prompt glyph is not unique to the composer: the TUI marks the
	// SELECTED ITEM of a menu with the same character. A menu therefore
	// presents a ❯-prefixed line above a rule, which is indistinguishable
	// from a composer unless the opening fence is also required.
	//
	// Observed on a live session: a selection menu whose highlighted option
	// was read as pending input. Every send to that session would have been
	// refused, forever, with a reason naming input a human never typed —
	// a session stopped by text that was never there.
	//
	// So: the first non-blank line above the prompt must be a rule. A menu's
	// preceding line is menu text and fails this; a real composer's is its
	// opening fence.
	fenced := false
	// ranOffTop tracks whether the walk below ever found a non-blank line to
	// judge at all. If every row above the prompt down to the top of the
	// capture is blank, this driver never got to see whether an opening
	// fence was there — clipped, the same reasoning as the walk above — as
	// opposed to seeing a real, non-rule line and correctly ruling the
	// composer out (fixtureMenuSelected's own shape: a question line sits
	// right above the highlighted option, settling it as composerAbsent).
	ranOffTop := true
	fence := -1
	for i := prompt - 1; i >= 0; i-- {
		if strings.TrimSpace(s.lines[i]) == "" {
			continue
		}
		fenced = isRule(s.lines[i])
		fence = i
		ranOffTop = false
		break
	}
	if !fenced {
		if ranOffTop {
			return 0, 0, composerClipped
		}
		return 0, 0, composerAbsent
	}
	// A composer is only read from the VISIBLE pane (muster#169). When
	// its opening fence sits in the history margin above the pane, the rows
	// that settled "this is a fenced composer" are scrollback. They may be an
	// older frame of this composer, or on an alternate-screen runtime,
	// output from before the runtime drew anything. Nothing here can tell
	// which, so this is the same "cannot tell" as a fence above the capture
	// altogether — clipped, never found, and no digest is published for it
	// (docs/adr/169-a-composer-is-read-from-the-visible-pane.md). A fence
	// inside the pane leaves the prompt row and every text row inside it too,
	// since both sit below the fence.
	if fence < s.visibleTop {
		return 0, 0, composerClipped
	}
	return prompt, last, composerFound
}

// bottomCutComposer reports whether the screen ends on a composer's opening
// rule and its prompt row, with the pane's bottom edge where the closing rule
// and the mode row would be (muster#216): a ❯ row that is the LAST row of
// the pane, opened by a rule, with no rule below it because there is no row
// below it.
//
// Anything tall enough above the composer does this on the 24-row pane a
// created session gets. The feedback-draft card is the measured case (#215),
// about seven rows, and any other notice of that height reproduces it.
//
// It is clipped, not absent: the composer is there, and its bottom is not
// visible. "Absent" is a positive finding that nothing is there to protect
// (composerText's contract), and reading it here let a discard answer "already
// clear" for a row it could not read.
//
// Three things keep the shape narrow, because it changes what send, keys and
// discard decide for every session:
//
//   - The row must be the last row of the CAPTURE, not merely the last
//     non-blank line. capture-pane pads to the pane's height, so this is the
//     pane's bottom edge; blank rows under the prompt row are the runtime
//     having had room and painted nothing there, which a bottom edge did not
//     cause and this does not claim.
//   - The row above must be a plain rule. A box's bottom border (`╰───╯`, the
//     card's own) has rule runes in it and is what the old walk mistook for the
//     opening fence.
//   - The row must not be a numbered option. A menu's highlighted option is
//     drawn with the same ❯; menus are read as prompts first by every verb, and
//     nothing here changes how a menu reads.
func bottomCutComposer(s screen) bool {
	n := len(s.lines)
	if n < 2 || s.blankBelow != 0 {
		return false
	}
	row := strings.TrimSpace(s.lines[n-1])
	if !strings.HasPrefix(row, composerRuneMarker) {
		return false
	}
	if _, _, numbered := numberedOption(strings.TrimSpace(strings.TrimPrefix(row, composerRuneMarker))); numbered {
		return false
	}
	fence := firstNonBlankAbove(s.lines, n-1)
	if fence < 0 || !isRule(s.lines[fence]) {
		return false
	}
	return !strings.ContainsAny(s.lines[fence], "╭╮╰╯│")
}

// composerClippedCause names, for a refusal or a state read's evidence, why a
// composer read clipped: a clause that follows "the composer". The two causes
// need opposite words. A composer taller than the capture window is one whose
// top is out of reach; one cut off by the pane's bottom edge has its top in
// plain view and is missing its bottom, and a caller told the first about the
// second would look for a paste that is not there (muster#216).
func composerClippedCause(s screen) string {
	if bottomCutComposer(s) {
		return "is cut off by the bottom edge of the pane (its opening rule and prompt row are " +
			"the last rows shown, its closing rule is not drawn)"
	}
	return "is taller than this driver's capture window or reaches above the visible pane"
}

// clippedOnlyAboveVisiblePane reports whether s reads composerClipped solely
// because of #169's visible-pane rule: without the pane boundary, the same
// rows would read composerFound. A refusal site counts this separately, so
// the rate at which the rule refuses a composer the driver used to read is on
// record (the check #169 asked for before deciding).
func clippedOnlyAboveVisiblePane(s screen) bool {
	if s.visibleTop == 0 {
		return false
	}
	if _, _, scan := composerSpan(s); scan != composerClipped {
		return false
	}
	whole := s
	whole.visibleTop = 0
	_, _, scan := composerSpan(whole)
	return scan == composerFound
}

// composerText returns the text a human has typed into the input box but
// not submitted, and one of three verdicts about whether a composer is
// there to hold it (muster#134).
//
// # The contract every caller must honour
//
// text is only ever meaningful when scan is composerFound — "" otherwise.
// composerAbsent is a POSITIVE finding: this driver looked at the whole
// structure a composer would occupy and confirmed there is none. Callers
// may treat that exactly as before — "nothing to protect, nothing to
// refuse against". composerClipped is NOT that: it means this driver's own
// capture ended before the search could be completed, so "" here does NOT
// mean the composer is empty, and it does NOT mean there is no composer —
// it means this driver could not tell. A caller that folds composerClipped
// into "not found" reproduces #134's own false negative one level up:
// Discard would report a residue-holding composer already clear, and
// Send's busy-guard would never fire, both exactly as if a human's unsent
// text were not there.
func composerText(s screen) (string, composerScan) {
	prompt, last, scan := composerSpan(s)
	if scan != composerFound {
		return "", scan
	}

	// A wholly dim composer is the TUI's placeholder hint, not something a
	// human typed. Treating it as pending input refuses every send to a
	// FRESH session, forever — a session the supervisor can never speak to,
	// stuck for text nobody wrote. Observed live on a newly created session.
	if prompt < len(s.raw) && allDim(afterMarker(s.raw[prompt])) {
		return "", composerFound
	}

	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.lines[prompt]), composerRuneMarker))
	// Continuation lines: a long message wraps below the prompt and is still
	// unsent input. Reading only the first line would under-report it, and
	// under-reporting is the direction that corrupts a human's message.
	for i := prompt + 1; i < last; i++ {
		if i < len(s.raw) && allDim(s.raw[i]) {
			continue
		}
		if seg := strings.TrimSpace(s.lines[i]); seg != "" {
			text += " " + seg
		}
	}
	return strings.TrimSpace(text), composerFound
}

// composerVisualLines reports how many rows a composer's fenced box
// occupies on screen right now — prompt through last-1, one row for its
// first line plus one per continuation row below it — which is exactly how
// many C-u presses a clear pass should expect to need (muster#129):
// C-u (unix-line-discard) empties the ONE row the cursor sits on and leaves
// every row above it standing, and it is the on-screen row that matters,
// not the underlying logical line.
//
// # Why visual, not logical — measured, not assumed (#129's own instruction)
//
// composerText already collapses every continuation row into pending with a
// single joining space (see above), so pending's own text carries no line
// boundary a caller could count even if it wanted to. This counts the
// SCREEN's rows directly instead — which settles "visual vs logical" as a
// side effect, because a captured pane's rows are already wrapped at
// whatever width tmux rendered them at; there is no separate "logical" view
// to recover from a capture even in principle.
//
// That this is the right quantity, not merely the available one, has a real
// field measurement behind it: muster#32 recorded a composer holding
// 209 characters — continuous typed text, no line break the human put there
// — that rendered as four on-screen rows and needed four C-u presses to
// empty, corrupting on the third when the un-repeated fix of the day only
// sent one. A LOGICAL count would have read that same 209 characters as one
// line and predicted a single press was enough, which #32 is the record of
// disproving. #129's own sizing case (roughly 6.6 KB pasted, on the order of
// eighty rows at a typical pane width) is consistent with the same reading:
// a paste that size has few if any real line breaks in it, so eighty
// presses is a wrap count, not a paragraph count.
//
// scan reports exactly what composerText's own scan would report for this
// same screen — both walk composerSpan's identical structure — so a caller
// already holding a true composerFound result from composerText can treat
// this as reporting the same verdict without a separate check. A caller
// that has NOT already established composerFound must still branch on scan
// itself: 0 here means either "no composer" or "could not tell" (#134), and
// the two demand opposite handling — see composerText's own doc comment.
func composerVisualLines(s screen) (int, composerScan) {
	prompt, last, scan := composerSpan(s)
	if scan != composerFound {
		return 0, scan
	}
	return last - prompt, composerFound
}

// composerCursorRowBlank reports whether the composer's BOTTOM content row —
// the row directly above the closing rule, where typing (and so the cursor)
// is assumed to sit — is empty right now (muster#132).
//
// # Why this has to be checked at all
//
// C-u is readline's unix-line-discard: kill from the cursor back to the
// START OF THE CURRENT LINE, nothing more. When that line already has
// nothing on it — the shape a payload ending in one or more real trailing
// newlines leaves behind, one blank row per newline — there is nothing to
// kill and the keystroke is a complete no-op. It does not cross the line
// boundary above it, because unix-line-discard was never defined to. A
// clear pass that only ever sends C-u is therefore stuck forever the moment
// it reaches such a row: every further press reports the identical capture,
// which reads as "genuinely stuck" (#87's futility signal) even though nothing
// about the surrounding text resists clearing at all.
//
// clearComposer uses this, each iteration, to choose the key that can
// actually make progress: BSpace crosses a line boundary — it deletes the
// one character behind the cursor, which on an empty row is the newline
// itself, merging the blank row into the end of the row above it — where
// C-u cannot.
//
// # Why the bottom row, not composerVisualLines' whole span
//
// Clearing walks backward one row at a time from wherever the cursor
// currently is, and every caller of this function has already reduced
// "where might the cursor be" to "the last row still standing" — the same
// row composerVisualLines' own count decreases from as rows disappear (see
// clearComposer). Checking any other row would be asking about text the
// cursor is not, at this moment, sitting on.
//
// scan is composerFound only in the cases composerSpan itself locates a
// composer in full; otherwise blank is meaningless and the caller gets the
// same composerAbsent/composerClipped split composerText does — a clipped
// composer's cursor row is exactly as unreadable as its text (muster
// #134), which #138's clearComposer must treat as "cannot assume blank",
// never as "assume blank". A composer that has shrunk to (or never had more
// than) its own ❯-marked line reports blank=false, never true: that line
// always carries the marker glyph as visible content, so it can never be
// the empty-row shape this exists to detect — and if the text after the
// marker is itself empty, composerText already reports the whole composer
// empty, which the clear loop treats as done before this is ever consulted.
func composerCursorRowBlank(s screen) (blank bool, scan composerScan) {
	prompt, last, scan := composerSpan(s)
	if scan != composerFound {
		return false, scan
	}
	row := last - 1
	if row <= prompt || row >= len(s.lines) {
		return false, composerFound
	}
	return strings.TrimSpace(s.lines[row]) == "", composerFound
}

// awaitingSelection reports whether the TUI is showing a menu that blocks
// on a human keypress.
func awaitingSelection(s screen) bool {
	_, blocked := selectionPrompt(s)
	return blocked
}

// parsePrompt extracts the full question a session is blocked on: every
// option in order, which one is highlighted, and a nonce over the whole thing.
//
// Options are recognised by their leading "N." rather than by any wording, so
// a new prompt from a future release is enumerated without a new matcher —
// which is the failure mode a sibling project named explicitly: "chasing them
// individually means a new matcher every time the CLI adds a screen". A menu
// that paints no numbers at all is recognised by its shape instead — see
// unnumberedMenu (muster#171).
func parsePrompt(s screen) *fleet.SessionPrompt {
	p, _ := parsePromptMenu(s)
	return p
}

// parsePromptMenu is parsePrompt plus the one fact about a menu's shape that
// ANSWERING it depends on: whether its options carry numbers.
//
// muster#171 measured the difference on the runtime. On a numbered menu
// a digit commits the answer (#168). On the unnumbered folder-trust menu a
// digit does nothing at all — the screen stayed byte-identical — and only
// arrow keys move the highlight, which Enter then confirms. A caller's
// Choice is the same 1-based index on both, but the keys that deliver it are
// not, so Respond has to know which kind of menu it is answering. The flag
// stays out of fleet.SessionPrompt: it is how this substrate delivers an
// answer, not something a caller answers differently.
func parsePromptMenu(s screen) (*fleet.SessionPrompt, bool) {
	p, shape := parsePromptShape(s)
	return p, shape.unnumbered
}

// parsePromptShape is parsePromptMenu with every fact about the menu's shape
// that answering it depends on (see menuShape), not only whether its options
// carry numbers. A menu drawn beside a preview pane is the second such fact
// (muster#204): its options are read from the list column alone, and a
// caller has to know a pane is there because a digit does not commit on it.
//
// A menu is read first. When there is none, the runtime's feedback-draft card
// is the other thing this reads (muster#215): a notice, not a menu, so it
// has no numbered options and no highlight, and it is a prompt only while it is
// what stands between a caller and the composer — see feedbackCard.answerable.
func parsePromptShape(s screen) (p *fleet.SessionPrompt, shape menuShape) {
	if p, shape = parseMenuShape(s); p != nil {
		return p, shape
	}
	if c, ok := liveFeedbackCard(s); ok && c.answerable() {
		return c.sessionPrompt(), menuShape{shortcuts: feedbackCardShortcuts()}
	}
	return nil, menuShape{}
}

// parseMenuShape is the numbered, unnumbered and preview-pane menus'
// recogniser: everything parsePromptShape read before the feedback-draft card.
func parseMenuShape(s screen) (p *fleet.SessionPrompt, shape menuShape) {
	// Detection is STRUCTURAL, not footer-based.
	//
	// Four footers have been seen on one runtime — "Enter to select · Tab/Arrow
	// keys to navigate", "Enter to confirm · Esc to cancel", and a tool-permission
	// dialog reading "Esc to cancel · Tab to amend" — and matching them meant a
	// new matcher for every screen the runtime adds, which is how this class of
	// stall stays permanently one release behind.
	//
	// What every one of them has is the thing being asked: a run of numbered
	// options near the bottom of the screen with exactly one marked as
	// highlighted. That is the question, and it is what a caller has to answer.
	p = &fleet.SessionPrompt{}
	footer := false
	tabBar := false
	var question []string
	from := 0
	if len(s.lines) > promptScanDepth {
		from = len(s.lines) - promptScanDepth
	}

	// A preview pane is read first, because it decides both where the window
	// starts and what a row means (muster#204). It can be taller than the
	// window — the runtime clamps one to 24 content lines, which with its
	// borders and the dialog's chrome is more rows than promptScanDepth — and
	// then option 1, on the pane's top row, falls out of it and a session
	// blocked on the dialog reads as idle. So the window is widened to the
	// pane's top, and a few rows above it for the question and header.
	pane, hasPane := findPreviewPane(s.lines)
	if hasPane {
		shape.preview = true
		if want := pane.top - previewHeaderRows; want < from {
			from = max(want, 0)
		}
	} else {
		from = dialogTop(s.lines, from)
	}
	// header is the dialog's tab bar or chip, and headerRow its row.
	header, headerRow := "", -1
	// afterRule: the previous row read was a rule. A single-question header is
	// a lone chip, which is only believed directly under the dialog's opening
	// rule — a checklist item in the agent's own prose looks the same.
	afterRule := false

	// Review fix (#180 review): mask OUT the active composer's own
	// fenced rows before scanning for a menu. numberedOption/selected below
	// have no way to tell a genuine menu option apart from a MULTI-LINE
	// composer message that merely starts with "1." — Claude Code echoes a
	// composer's continuation rows with the same ❯ leader a menu's
	// highlighted option uses (composerText's own doc comment: the row after
	// the prompt wraps below it), so "1. merge PR 12 once CI is green" /
	// "2. then deploy staging" typed into a live composer parsed as a
	// two-option menu with option 1 "selected", and confirmLandedV2's own
	// dialog-race gate (which calls this via awaitingSelection) refused the
	// send outright — the exact numbered-message wedge D2 was supposed to
	// have closed.
	//
	// composerSpan is the right test for "is this really a composer" rather
	// than "is this really a menu", because on an ACTUAL selection menu
	// composerSpan itself already reads composerAbsent (composerSpan's own
	// doc comment: the fenced-composer shape a menu occupies that SAME
	// visual position instead of, per muster#58's original finding) —
	// masking only ever removes rows a real composer owns, never a real
	// menu's.
	promptRow, lastRow, composerScan := composerSpan(s)
	// Not while a preview pane is on screen (muster#204). A pane's bottom
	// border is a rule to composerSpan, so a highlighted option BELOW a short
	// pane sits between two "rules" and reads as a fenced composer — and
	// masking it made the last option, and the highlight, vanish. A composer
	// is not drawn under a dialog and its pane, so there is nothing to mask.
	maskComposer := composerScan == composerFound && !hasPane

	for i, raw := range s.lines[from:] {
		idx := from + i
		if maskComposer && idx >= promptRow && idx <= lastRow {
			continue
		}
		line := strings.TrimSpace(raw)
		if hasPane && pane.contains(idx) {
			// Inside the pane's span a row is the list column and the pane's
			// cells side by side. Only the list column is read: the label is
			// the text to the left of the pane, never the box art after it, and
			// a pane row with nothing to its left carries no option at all.
			prefix, _, _ := splitPaneRow(raw)
			line = strings.TrimSpace(prefix)
			if line == "" {
				continue
			}
			if _, _, numbered := numberedOption(strings.TrimSpace(strings.TrimPrefix(line, composerRuneMarker))); !numbered && len(p.Options) > 0 {
				// A long label wraps inside its column, and the continuation
				// has no number of its own: it is the same label, not a row to
				// drop (which would report the label cut short).
				p.Options[len(p.Options)-1] += " " + line
				continue
			}
		}
		if line == "" {
			continue
		}
		wasRule := afterRule
		afterRule = isRule(line)
		selected := strings.HasPrefix(line, composerRuneMarker)
		body := strings.TrimSpace(strings.TrimPrefix(line, composerRuneMarker))
		if n, text, ok := numberedOption(body); ok {
			if n > maxPromptOptions {
				// Bounded on purpose. This parses a pane whose contents are
				// written by an agent that can print anything, and padding
				// a slice up to a parsed index is unbounded allocation
				// driven by untrusted input: one transcript line reading
				// "1000000. x" hung the service until it was killed.
				//
				// A menu with more options than this is not a menu.
				continue
			}
			for len(p.Options) < n-1 {
				// A gap means a line did not parse; keep indices honest
				// rather than silently renumbering what a caller will
				// submit by index.
				p.Options = append(p.Options, "")
			}
			if len(p.Options) == n-1 {
				p.Options = append(p.Options, text)
			} else if n-1 < len(p.Options) {
				p.Options[n-1] = text
			}
			if selected {
				p.Selected = n
			}
			continue
		}
		if strings.Contains(line, selectFooter) || strings.Contains(line, confirmFooter) ||
			strings.Contains(line, amendFooter) {
			footer = true
			continue
		}
		if len(p.Options) == 0 && (isDialogTabBar(line) || (wasRule && isDialogChip(line))) {
			// The dialog's header. Everything above it is the transcript the
			// dialog was drawn under, and the header itself is not the
			// question, so neither belongs in Question (muster#204: it
			// read "…tail of the agent's prose ← ☐ Layout ☐ Theme ✔ Submit →
			// Which layout do you prefer?"). The header is not lost: it is
			// part of the nonce, below.
			tabBar = tabBar || isDialogTabBar(line)
			header, headerRow = line, idx
			question = nil
			continue
		}
		if len(p.Options) == 0 && !isRule(line) {
			if row := questionRow(line); row != "" {
				question = append(question, row)
			}
		}
	}
	// Structure OR footer — neither alone is sufficient.
	//
	// Structure (two or more options with one highlighted) catches dialogs
	// whose footer nobody has seen before, which is the case that kept
	// costing a new matcher per release. It ALSO requires the run to have no
	// gap (see optionsAreContiguous) — a measured incident (#58) found the
	// gap-fill padding this loop uses for honesty (see the comment above)
	// doubling as a false-positive tell when nothing legitimate produced the
	// gap: ordinary transcript text happened to contain a highlighted-looking
	// line at index 2 with nothing at index 1, and the empty pad at index 1
	// was reported as a real option. No real menu ships an option with no
	// label, so a gap here means the "menu" is not one.
	//
	// Footer catches the case structure misses: a long menu whose highlighted
	// option has scrolled above the captured window. That happens on real
	// panes, and requiring the marker would classify a genuinely blocked
	// session as merely unreadable. It gaps in exactly the same shape and is
	// deliberately NOT held to the contiguity rule above — see
	// TestMenuWithScrolledOffMarkerStillCounts — because the footer is
	// itself the corroboration this branch has instead.
	if len(p.Options) == 0 && footer {
		// No numbered option anywhere in the window, but a runtime footer
		// is. That is the one situation an unnumbered menu can be in, and
		// the footer is what licenses looking for one (muster#171).
		if menu := unnumberedMenu(s.lines[from:]); menu != nil {
			menu.Nonce = promptNonce(menu)
			return menu, menuShape{unnumbered: true}
		}
		return nil, menuShape{}
	}
	structural := len(p.Options) >= 2 && p.Selected > 0 && optionsAreContiguous(p.Options)
	if !structural && !(footer && len(p.Options) >= 1) {
		return nil, menuShape{}
	}
	// Where the question starts decides how much of it is one.
	//
	// With no header found, the rows above the options can be the transcript the
	// dialog was drawn under: the last couple are the ask, everything above is
	// not. With the header found, the loop above dropped everything before it, so
	// every row between the header and the options IS the question, and a bound
	// of three only cut a wrapped one off at the front (muster#220). It is
	// kept whole, up to maxQuestionRows — and then from the options' side.
	//
	// Either way the question is a function of the screen, not of the history the
	// capture carries above it: whatever the window reads above the header is
	// dropped at the header, and a window that starts inside a tall dialog is
	// widened to its opening rule and no further (dialogTop). So two reads of one
	// screen report one question and one nonce. A capture that itself cuts the
	// dialog off above its header finds none, and reads the last few rows as it
	// always did.
	keep := unanchoredQuestionRows
	if headerRow >= 0 {
		keep = maxQuestionRows
	}
	if n := len(question); n > keep {
		question = question[n-keep:]
	}
	p.Question = strings.Join(question, " ")
	if headerRow >= 0 {
		shape.headerAt = headerRow + 1
		rawHeader := header // no escapes to read a highlight from: the position stays unread
		if headerRow < len(s.raw) {
			rawHeader = s.raw[headerRow]
		}
		shape.tab = dialogTabPosition(rawHeader)
		if isDialogTabBar(header) {
			if tabs, cur, ok := dialogTabs(rawHeader); ok {
				p.Tabs = tabs
				if cur >= 0 {
					p.Tab = &cur
				}
			}
		}
		p.Nonce = promptNonceWithHeader(p, header, shape.tab)
	} else {
		p.Nonce = promptNonce(p)
	}
	p.MultiSelect = tabBar && multiSelectBoxes(p) > 0
	p.FreeText = freeTextOffered(p, shape)
	return p, shape
}

// questionRail is the rule the runtime draws down the left edge of a question
// too long for one row (muster#219): every row of the wrapped question is
// painted `│ <text>`, and a row between paragraphs is the rail alone.
const questionRail = "│"

// questionRow reads one row of a question, without the rail a wrapped question
// is drawn with. "" says the row held nothing but the rail, and is no row of the
// question at all. Only a rail at the row's start is set aside: the row's own
// words are the caller's, and a `│` inside them is theirs.
func questionRow(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), questionRail))
}

// dialogTop returns the row a menu is read from: from, the top of the fixed
// window, unless that window starts inside the dialog, and then the row under
// the dialog's opening rule (muster#219).
//
// A dialog is as tall as its question and its option descriptions make it — a
// question that wraps over several rows, a description under every option —
// and the window is a fixed number of rows. When it starts below the dialog's
// header the multi-select question loses the tab bar that corroborates it and
// reads as an ordinary menu whose boxes nobody may tick; when it starts below
// the first option the run has a gap and is not read as a menu at all.
//
// A dialog opens with a rule, so a window with one above its first option
// reaches the top already, and is left as it is. A window with none starts
// inside the dialog: the opening rule is the nearest one above it, because the
// rule that divides the escape rows off is below the options. Nothing widens
// unless an option or a runtime footer is there to be read, and never past the
// nearest rule — the capture is the pane plus a margin of history, so that is
// bounded by what is on screen — which is what keeps the prose above a dialog,
// numbered lists included, out of the options.
func dialogTop(lines []string, from int) int {
	if from <= 0 {
		return from
	}
	opens := false
	for _, raw := range lines[from:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if isRule(line) {
			return from
		}
		if _, _, numbered := numberedOption(strings.TrimSpace(strings.TrimPrefix(line, composerRuneMarker))); numbered ||
			strings.Contains(line, selectFooter) || strings.Contains(line, confirmFooter) || strings.Contains(line, amendFooter) {
			opens = true
			break
		}
	}
	if !opens {
		return from
	}
	for i := from - 1; i >= 0; i-- {
		if isRule(strings.TrimSpace(lines[i])) {
			return i + 1
		}
	}
	return from
}

// The checkbox glyphs a multi-select question paints in front of each option
// label (muster#176), measured live on one runtime build: a clear box and
// a ticked one. They stay in the option text — that text is what the nonce
// digests, so a changed tick is a changed prompt, which is exactly what makes
// an answer to a stale tick state refusable.
const (
	checkboxClear  = "[ ] "
	checkboxTicked = "[✔] "
)

// checkboxLabel splits an option into its label and its checkbox, if it has
// one.
func checkboxLabel(o string) (label string, ticked, box bool) {
	switch {
	case strings.HasPrefix(o, checkboxClear):
		return strings.TrimPrefix(o, checkboxClear), false, true
	case strings.HasPrefix(o, checkboxTicked):
		return strings.TrimPrefix(o, checkboxTicked), true, true
	}
	return o, false, false
}

// isEscapeAffordance reports whether an option label, whole, is one of the
// two rows the runtime appends to every agent-asked question: the free-text
// row and the chat row. Whole-label, not prefix: an agent's own option that
// merely begins with the words is still the agent's option.
func isEscapeAffordance(label string) bool {
	return isFreeTextLabel(label) || isChatLabel(label)
}

// isFreeTextLabel reports whether an option label, whole, is the runtime's
// free-text row as it reads BEFORE anyone types into it.
//
// Once text is typed the row's label becomes that text (measured, colab-
// fleet#206), so this recognises the placeholder and nothing else — which is
// why a row that already holds text is not found again, and why a driver that
// needs the row's position after typing has to carry the index it found first.
func isFreeTextLabel(label string) bool {
	return normalisedAffordance(label) == "type something"
}

func isChatLabel(label string) bool {
	return normalisedAffordance(label) == "chat about this"
}

func normalisedAffordance(label string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(label)), ".")
}

// freeTextRow returns the 1-based index of a prompt's free-text row, or 0 when
// no option reads as the placeholder. The checkbox a multi-select question
// paints in front of it is set aside first.
func freeTextRow(p *fleet.SessionPrompt) int {
	if p == nil {
		return 0
	}
	for i, o := range p.Options {
		label, _, _ := checkboxLabel(o)
		if isFreeTextLabel(label) {
			return i + 1
		}
	}
	return 0
}

// freeTextOffered decides SessionPrompt.FreeText: whether a driver has the
// measured key sequence for answering this prompt through its free-text row
// (muster#206).
//
// Every condition is required, and each one names a layout the sequence was
// NOT measured on — the answer to those is "not offered", never a guess:
//
//   - the row is there, and reads as the placeholder;
//   - the menu is numbered (an unnumbered menu is a trust dialog, which has no
//     such row) and has no preview pane (measured to have none, so a row that
//     did appear there would be an unmeasured layout);
//   - on a plain question no option carries a checkbox. A boxed list the
//     driver does not recognise as multi-select (no tab bar) is a checklist an
//     agent painted into its own prose, and on a real checkbox a digit
//     TOGGLES rather than moving to the row;
//   - on a multi-select question the row is the one directly after the
//     boxes, which is where multiSelectBoxes already requires the escape rows
//     to start.
func freeTextOffered(p *fleet.SessionPrompt, shape menuShape) bool {
	if p == nil || shape.unnumbered || shape.preview {
		return false
	}
	idx := freeTextRow(p)
	if idx == 0 || !optionsAreContiguous(p.Options) {
		return false
	}
	boxed := false
	for _, o := range p.Options {
		if _, _, box := checkboxLabel(o); box {
			boxed = true
			break
		}
	}
	if !boxed {
		return true
	}
	return p.MultiSelect && idx == multiSelectBoxes(p)+1
}

// isDialogTabBar recognises the tab row above a question in the runtime's
// question dialog: `←  ☐ <header>  ✔ Submit  →`, one `☐`/`☒` per question.
// Measured on a single-question multi-select dialog and a two-question one
// (muster#176); the same row #168 modelled for tabbed dialogs.
func isDialogTabBar(line string) bool {
	return strings.HasPrefix(line, "←") && strings.HasSuffix(line, "→") &&
		strings.Contains(line, "✔ Submit")
}

// multiSelectBoxes returns how many of a parsed prompt's options are the
// question's own checkboxes — the options a Response.Choices may name — or 0
// when the prompt is not recognised as multi-select at all.
//
// # What was measured (muster#176, one runtime build, live)
//
//	←  ☒ Fruit  ✔ Submit  →
//	Which fruits do you like?
//	❯ 1. [✔] Apple
//	  2. [ ] Banana
//	  3. [✔] Cherry
//	  4. [ ] Type something
//	     Submit
//	────────────────
//	  5. Chat about this
//	Enter to select · ↑/↓ to navigate · Esc to cancel
//
// The free-text row carries a box too, and is NOT a choice: selecting it
// opens a text field. So the answerable run is the leading boxed options up
// to the first escape affordance, and everything after that run must be one
// of the runtime's two escape rows. The unnumbered "Submit" row (or "Next",
// inside a multi-question dialog) is not an option at all and never parses
// as one.
//
// Every condition is required, and failing one yields 0 — "not recognised",
// never a guess. The tab-bar requirement is checked by the caller, from the
// raw lines: it is what keeps an agent's own `[ ] todo` checklist, painted
// into a transcript above an ordinary menu, from reading as a multi-select
// question.
func multiSelectBoxes(p *fleet.SessionPrompt) int {
	if p == nil || !optionsAreContiguous(p.Options) {
		return 0
	}
	boxes := 0
	for _, o := range p.Options {
		label, _, box := checkboxLabel(o)
		if !box || isEscapeAffordance(label) {
			break
		}
		boxes++
	}
	if boxes == 0 || boxes == len(p.Options) {
		// No boxes, or no escape row after them: not the measured shape.
		return 0
	}
	for _, o := range p.Options[boxes:] {
		label, _, _ := checkboxLabel(o)
		if !isEscapeAffordance(label) {
			return 0
		}
	}
	return boxes
}

// promptTicks reports which of a multi-select question's first boxes options
// are ticked, 0-based.
func promptTicks(p *fleet.SessionPrompt, boxes int) []bool {
	out := make([]bool, boxes)
	for i := 0; i < boxes && i < len(p.Options); i++ {
		_, out[i], _ = checkboxLabel(p.Options[i])
	}
	return out
}

// unnumberedMenu recognises a select menu whose options carry no numbers
// (muster#171). Measured on the runtime's folder-trust question in a
// directory it had never seen:
//
//	Security guide
//
//	❯ No, exit
//	  Yes, I trust this folder
//
//	Enter to confirm · Esc to cancel
//
// Without numbers, the only structure left is layout, and every piece of it
// is required, because each one alone is something ordinary transcript can
// paint:
//
//   - a runtime footer, with only blank rows between it and the options —
//     the corroboration the numbered path gets from its digits;
//   - a single block of two or more non-blank rows directly above it, ended
//     by a blank row or a rule;
//   - exactly one row in that block carrying the highlight marker;
//   - every other row indented to exactly the column where the marked row's
//     text starts, so the option labels line up.
//
// Anything that fails one of these is not read as a menu at all. The cost of
// a wrapped option label, or a layout this has not seen, is the pre-#171
// behaviour (no prompt recognised), never a guessed option list: Respond
// answers by index, and an index into a misread list is an answer to a
// question nobody asked.
func unnumberedMenu(lines []string) *fleet.SessionPrompt {
	footer := -1
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.Contains(line, selectFooter) || strings.Contains(line, confirmFooter) ||
			strings.Contains(line, amendFooter) {
			footer = i
			break
		}
	}
	if footer < 0 {
		return nil
	}
	end := footer - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end < 0 {
		return nil
	}
	start := end
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" && !isRule(lines[start-1]) {
		start--
	}
	block := lines[start : end+1]
	if len(block) < 2 || len(block) > maxPromptOptions {
		return nil
	}

	marker, textCol := -1, -1
	for i, raw := range block {
		rest := strings.TrimLeft(raw, " ")
		if !strings.HasPrefix(rest, composerRuneMarker) {
			continue
		}
		if marker >= 0 {
			return nil // two highlights is not one menu
		}
		after := strings.TrimPrefix(rest, composerRuneMarker)
		gap := len(after) - len(strings.TrimLeft(after, " "))
		if gap == 0 {
			return nil
		}
		marker = i
		// Columns, not bytes: the marker is one cell wide and three bytes long.
		textCol = (len(raw) - len(rest)) + 1 + gap
	}
	if marker < 0 {
		return nil
	}

	p := &fleet.SessionPrompt{Selected: marker + 1}
	for i, raw := range block {
		if isRule(raw) {
			return nil
		}
		text := strings.TrimSpace(raw)
		if i == marker {
			text = strings.TrimSpace(strings.TrimPrefix(text, composerRuneMarker))
		} else if len(raw)-len(strings.TrimLeft(raw, " ")) != textCol {
			return nil
		}
		if text == "" {
			return nil
		}
		if _, _, numbered := numberedOption(text); numbered {
			return nil // parsePromptMenu's numbered path owns this shape
		}
		p.Options = append(p.Options, text)
	}

	// The question: the last few non-blank rows above the block, stopping at
	// a rule — the same "last couple of lines" parsePrompt keeps.
	var question []string
	for i := start - 1; i >= 0 && len(question) < 3; i-- {
		if isRule(lines[i]) {
			break
		}
		if line := strings.TrimSpace(lines[i]); line != "" {
			question = append([]string{line}, question...)
		}
	}
	p.Question = strings.Join(question, " ")
	return p
}

// numberedOption parses "1. Some option" into its index and text.
func numberedOption(line string) (int, string, bool) {
	i := 0
	// At most four digits: a longer run is not an option number, and
	// accumulating it would overflow before it was ever rejected.
	for i < len(line) && i < 4 && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != '.' {
		return 0, "", false
	}
	n := 0
	for _, c := range line[:i] {
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return 0, "", false
	}
	return n, strings.TrimSpace(line[i+1:]), true
}

// optionsAreContiguous reports whether a structural match's option run has
// no gap — see parsePrompt's own comment on why a gap is a stronger tell
// than any wording could be: the padding that keeps indices honest for a
// genuinely scrolled-off menu (the footer branch) is the same padding a
// coincidental structural false-positive produces, and a real menu never
// prints an option with no label.
func optionsAreContiguous(opts []string) bool {
	for _, o := range opts {
		if o == "" {
			return false
		}
	}
	return true
}

// promptFooterPresent reports whether one of the runtime's own menu footers
// is visible in the same window parsePrompt scans.
//
// A second, minimal scan rather than a second return value threaded through
// parsePrompt's dozen call sites: this is used in exactly one place — see
// classifyAgedDetail's companion-clause check below — to decide how much
// corroboration an accepted structural prompt already has, and that call
// site is the only one that needs the footer fact on its own, separate from
// whatever kind of prompt it turned out to be.
func promptFooterPresent(s screen) bool {
	from := 0
	if len(s.lines) > promptScanDepth {
		from = len(s.lines) - promptScanDepth
	}
	for _, raw := range s.lines[from:] {
		line := strings.TrimSpace(raw)
		if strings.Contains(line, selectFooter) || strings.Contains(line, confirmFooter) ||
			strings.Contains(line, amendFooter) {
			return true
		}
	}
	return false
}

// reviewQuestion and reviewOptions are the fixed chrome of the last screen of
// every multi-question dialog: after the tabbed questions, the runtime shows
// the answers given and asks for a final confirmation (#159). Both strings
// were read out of the runtime's own binary, where they are literals — the
// confirm and cancel labels of one confirm widget, and the line rendered
// directly above it — not sampled from a capture.
const reviewQuestion = "Ready to submit your answers?"

var reviewOptions = []string{"Submit answers", "Cancel"}

// reviewScreenPrompt reports whether a parsed prompt is a multi-question
// dialog's review screen.
//
// # Why this is corroboration, not a kind
//
// That screen renders no footer — the question tabs before it do, which is
// why they classified immediately — so without this it fell into #58's
// uncorroborated-structural hold, and it is the last step of EVERY
// multi-question dialog. What corroborates it is the same class of evidence
// a footer is: fixed runtime vocabulary nothing about the session varies.
// All three must hold: exactly these two options in this order, and the
// question ending on the runtime's own confirmation line, which sits
// immediately above them and so is always inside the scanned window.
//
// It deliberately does NOT get a PromptKind. A kind is what a client filters
// on before auto-answering, and "submit the answers somebody else chose" is
// not a question a client should be answering unattended. Recognised enough
// to report at once; not recognised enough to automate.
//
// An agent's OWN question cannot reach this shape: the runtime appends its
// free-text and chat affordances to every agent-asked menu (see
// classifyPromptKind), so such a menu never has exactly two options.
func reviewScreenPrompt(p *fleet.SessionPrompt) bool {
	if p == nil || len(p.Options) != len(reviewOptions) {
		return false
	}
	for i, o := range reviewOptions {
		if p.Options[i] != o {
			return false
		}
	}
	return p.Question == reviewQuestion || strings.HasSuffix(p.Question, " "+reviewQuestion)
}

// promptNonce is a digest of what is being asked. It changes whenever the
// question or the options change, which is what makes a stale answer
// detectable rather than silently applied to a different menu.
// usageLimit reports whether the LIVE BOTTOM of the screen says this session is
// blocked by a usage limit, and any reset time it states.
//
// # Why the live bottom, and not the screen
//
// After a resume the runtime re-renders the transcript, so an OLD limit notice
// scrolls past again. A matcher that searched the whole capture would report a
// session blocked by a limit it hit yesterday and has long since recovered
// from — an operational runbook for this fleet records exactly that trap
// ("judge by the live bottom of the pane"). So this looks only below the last
// composer fence, which is the region the runtime redraws.
//
// # Why prose is matched here at all
//
// Reluctantly, and bounded the same way classifyPromptKind is: there is no
// structural signal for "the account ran out of quota" — no spinner, no menu,
// no dialog chrome. The screen simply says so in words. Matched loosely on the
// two shapes seen in the wild, and failing to "no limit" rather than to a
// guess.
func usageLimit(s screen) (resetHint string, blocked bool) {
	// The live region is the last few transcript lines ABOVE the composer, not
	// below it: the composer and its fences are the bottom of the screen, and
	// the runtime prints its notices just before them.
	end := len(s.lines)
	for i := len(s.lines) - 1; i >= 0 && i >= len(s.lines)-promptScanDepth; i-- {
		if strings.Contains(s.lines[i], composerRuneMarker) {
			// walk up past the opening fence
			end = i
			for end > 0 && isRule(s.lines[end-1]) {
				end--
			}
			break
		}
	}
	// muster#215: a feedback-draft card sits between the agent's output and
	// the composer, and its rows are the agent's own draft — prose about whatever
	// went wrong, which can carry the very words this reads as a runtime notice.
	// The live region ends above the card.
	if c, ok := liveFeedbackCard(s); ok && c.top < end {
		end = c.top
	}
	// The notice must be the last thing the AGENT printed — not literally the
	// last line, which was too strict and missed the real shape.
	//
	// Measured on a blocked machine: the runtime prints the notice, then its
	// own continuation line, then settles its status line. So the notice is
	// never last, and a one-line rule saw nothing on a fleet that had been
	// refusing work for days. The identical shape was already recorded for a
	// failed turn — error first, status line after — and this function was
	// written before that lesson was applied here.
	//
	// What still separates a live block from a replayed one is what comes
	// AFTER: a session that carried on has agent output below the notice, and
	// agent output is marked by the runtime's own response bullet. Chrome —
	// the status line, continuation lines, blanks — is not. So scan a window,
	// and disqualify the notice if the agent said anything after it.
	const liveTail = 8
	var found bool
	var hint string
	seen := 0
	for i := end - 1; i >= 0 && seen < liveTail; i-- {
		line := strings.ToLower(strings.TrimSpace(s.lines[i]))
		if line == "" || isRule(s.lines[i]) {
			continue
		}
		seen++
		// The runtime's response bullet means the agent produced output after
		// whatever is above it — so anything found from here up is history.
		if strings.HasPrefix(line, responseBullet) {
			return "", false
		}
		hit := (strings.Contains(line, "limit") &&
			(strings.Contains(line, "hit your") || strings.Contains(line, "reached") ||
				strings.Contains(line, "usage"))) ||
			strings.Contains(line, "/usage-credits")
		if !hit {
			continue
		}
		// A reset time when the screen offers one — the number an operator
		// actually needs, and the difference between "wait" and "switch".
		//
		// Keep scanning after a hit that carries no time. The notice spans
		// more than one line ("…weekly limit · resets Aug 10" then
		// "/usage-credits to finish…"), and scanning upward meets the
		// continuation FIRST — so returning on the first hit found the block
		// and threw the reset time away, which is the one detail an operator
		// wants.
		found = true
		if hint != "" {
			continue
		}
		if h, ok := resetHintIn(line); ok {
			hint = h
		}
	}
	return hint, found
}

// resetHintIn looks for the runtime's own words about when a limit lifts, in
// one already-lowercased line of prose.
//
// Factored out so the same rule applies whether the prose comes from a live
// screen line (usageLimit) or from the runtime's own durable record
// (latestAPIError, #56) — both are the runtime's words, and a caller must not
// be able to tell which source produced a given hint from the rule that
// extracted it.
func resetHintIn(line string) (hint string, ok bool) {
	for _, marker := range []string{"resets ", "try again ", "available again "} {
		if k := strings.Index(line, marker); k >= 0 {
			rest := strings.TrimSpace(line[k+len(marker):])
			if len(rest) > 40 {
				rest = rest[:40]
			}
			return rest, true
		}
	}
	return "", false
}

// retryableWords reports whether already-lowercased prose says a failure is
// temporary, the same test lastTurnFailed and latestAPIError (#56) both make
// — from the runtime's own words, never from a status code decided here (see
// lastTurnFailed's own comment for why that distinction matters).
func retryableWords(lower string) bool {
	return strings.Contains(lower, "temporary") || strings.Contains(lower, "try again")
}

// trimToSentence keeps prose to roughly one sentence: the rest is usually a
// support URL or advice a human does not need repeated in every listing.
// Shared by lastTurnFailed (screen) and latestAPIError (record, #56) so a
// caller sees the same shape of Reason regardless of source.
func trimToSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	if cut := strings.IndexAny(s, ".\n"); cut > 0 {
		s = s[:cut]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// lastTurnFailed reports whether the live region shows the most recent turn
// ending in an error, and what the runtime said about it.
//
// # Why a window here, and exactly one line for the usage limit
//
// usageLimit demands the notice be the LAST live line, because a limit notice
// with work beneath it is history. This one cannot: the runtime prints the
// error and THEN settles its status line, so the error is never last. The
// window is a few lines, and the same replay hazard is handled differently —
// a session that errored and then carried on has that later work in the
// window, pushing the error out of it.
//
// # What "retryable" means, and where it comes from
//
// From the screen, not from us. The runtime says "usually temporary — try
// again in a moment" when it believes the failure is transient, and that
// sentence is the difference between a supervisor poking the session and a
// human being called. Inferring it from a status code instead would mean
// deciding, here, which of somebody else's error codes are worth retrying.
func lastTurnFailed(s screen) (*fleet.TurnEnd, bool) {
	end := len(s.lines)
	for i := len(s.lines) - 1; i >= 0 && i >= len(s.lines)-promptScanDepth; i-- {
		if strings.Contains(s.lines[i], composerRuneMarker) {
			end = i
			for end > 0 && isRule(s.lines[end-1]) {
				end--
			}
			break
		}
	}
	// muster#215: a feedback-draft card sits between the agent's output and
	// the composer, and its rows are the agent's own draft — prose about whatever
	// went wrong, which can carry the very words this reads as a runtime notice.
	// The live region ends above the card.
	if c, ok := liveFeedbackCard(s); ok && c.top < end {
		end = c.top
	}
	const window = 6
	var seen []string
	for i := end - 1; i >= 0 && len(seen) < window; i-- {
		line := strings.TrimSpace(s.lines[i])
		if line == "" || isRule(s.lines[i]) {
			continue
		}
		seen = append([]string{line}, seen...)
	}
	joined := strings.Join(seen, " ")
	lower := strings.ToLower(joined)

	// Matched on the runtime's error banner rather than on any status code:
	// the banner is what the runtime prints when a turn dies, and the code is
	// an implementation detail that has already changed shape once.
	marker := "api error"
	k := strings.Index(lower, marker)
	if k < 0 {
		return nil, false
	}
	reason := trimToSentence(joined[k:], 120)
	return &fleet.TurnEnd{
		Outcome:   "failed",
		Reason:    reason,
		Retryable: retryableWords(lower),
	}, true
}

// classifyPromptKind recognises which question a prompt is asking (§2.7's
// Kind), or returns empty when it does not recognise it.
//
// # This is prose matching, and that is the point
//
// Everything else in this file avoids matching the runtime's words. This
// function does the opposite deliberately, for one reason: the alternative is
// not "no prose matching" — it is prose matching in every client that wants to
// answer a prompt safely, which is the same fragility multiplied. Quarantining
// it here means it fails to EMPTY, and one place needs fixing when the runtime
// rewords a screen.
//
// # It reads the OPTIONS only. Never the question.
//
// This rule was learned in production, twenty minutes after the first version
// shipped. A ship-decision prompt was labelled bypass-permissions because the
// AGENT had written "No auth bypass" in its question text — and a client
// filtering on that kind would have auto-answered somebody's merge decision.
//
// The question is written by the agent. The options of a RUNTIME dialog are
// fixed strings the runtime emits. Matching agent-authored text is not merely
// fragile, it is injectable: an agent that writes "resume from summary" in its
// own question could have its decision auto-answered by a client that trusts
// the kind. Only the runtime's own option text is eligible.
//
// # And a question the agent asked is never a runtime dialog
//
// When an agent asks its own question, the runtime appends affordances no
// runtime dialog has — an escape hatch to type freely, or to chat instead.
// Their presence is a structural tell that this prompt belongs to the agent,
// and it disqualifies classification outright rather than being weighed.
func classifyPromptKind(p *fleet.SessionPrompt) fleet.PromptKind {
	if p == nil || len(p.Options) == 0 {
		return ""
	}
	// A kind the parser established from STRUCTURE stands (muster#215): the
	// feedback-draft card is recognised by its box and its key row, not by its
	// options, whose words ("review", "send", "dismiss") are ordinary and would
	// match anything. Nothing below runs on option text alone for this kind.
	if p.Kind != "" {
		return p.Kind
	}
	lower := make([]string, 0, len(p.Options))
	for _, o := range p.Options {
		// A multi-select question paints a checkbox in front of every label,
		// the free-text row's included (muster#176). Stripped here so
		// the agent-question guard below still sees "type something" at the
		// start of that row — with the box left on, the guard missed, and an
		// agent's checkbox option reading "allow this" could be labelled a
		// tool permission.
		label, _, _ := checkboxLabel(o)
		lower = append(lower, strings.ToLower(label))
	}
	// Agent-authored question → not a runtime dialog, whatever it resembles.
	for _, o := range lower {
		if strings.HasPrefix(o, "type something") || strings.HasPrefix(o, "chat about") {
			return ""
		}
	}
	// All needles must appear in ONE option. Spreading them across the set
	// is how unrelated words combine into a false match.
	hasOption := func(needles ...string) bool {
		for _, o := range lower {
			all := true
			for _, n := range needles {
				if !strings.Contains(o, n) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
		return false
	}
	switch {
	case hasOption("resume", "summary"):
		return fleet.PromptResumeChooser
	case hasOption("trust", "folder"):
		return fleet.PromptFolderTrust
	case hasOption("trust", "settings"):
		return fleet.PromptSettingsTrust
	case hasOption("allow", "external", "imports") && hasOption("disable", "external", "imports"):
		// Both halves, each in ONE option: the runtime offers this question as
		// an accept and a decline of the same thing, and a screen that carries
		// only one of them is a screen this rule has not been measured on. It
		// fails to empty rather than to a kind a consent would then answer
		// (muster #211).
		return fleet.PromptExternalImports
	case hasOption("don't ask again"), hasOption("allow this"):
		return fleet.PromptToolPermission
	}
	// Nothing here matches the permission-mode ACCEPTANCE screen, and that is a
	// finding rather than an omission.
	//
	// A rule for it existed and could never fire. It required "bypass" and
	// "permissions" to appear in one option, and the runtime's binary — read
	// directly, rather than sampled from a capture — contains no such option.
	// The complete set of boot-screen options it ships is:
	//
	//	Yes, I trust this folder        No, continue without these permissions
	//	Yes, I trust these settings     No, exit Claude Code
	//	Yes, I accept                   No, exit
	//	Yes, allow external imports     No, disable external imports
	//
	// (The last pair was added when the second boot question, about a directory's
	// instruction files importing a file from outside it, was measured on a
	// session that never got past it — muster #211. Unlike the rows above
	// it IS classified, because both of its options carry its identifying words.)
	//
	// (Read from one installed build; this repository's README pins the span
	// it is tested against, and that build sits outside it. A fact read from one
	// build is evidence about that build, not a guarantee across the span.)
	//
	// The words that identify that screen — "Bypass Permissions mode" — are in
	// its QUESTION. Its options are generic, and exactly one other screen's
	// negative opens with the identical words — "No, exit Claude Code" — so a
	// rule keyed on that phrase would not even isolate this screen alone.
	//
	// So it cannot be classified here without reading the question, and the
	// question is exactly what this function must not read: it is written by the
	// agent, and a ship decision was once labelled with this very kind because
	// the AGENT had typed "No auth bypass" into its own prompt. A rule that
	// matched a generic "accept" would be worse still — it would put a kind on
	// screens this driver has never seen, and a client filtering on kind before
	// auto-answering would then answer them.
	//
	// The consent path reaches that screen a different way, and the difference
	// is the whole point: the driver knows it started the session in that mode,
	// so it identifies the screen by what it DID rather than by what the screen
	// says. See consentableKinds and settleNewSession. Provenance is available
	// there and is not available here, which is why the answer differs by
	// caller rather than by wording.
	return ""
}

func promptNonce(p *fleet.SessionPrompt) string {
	h := sha256.New()
	h.Write([]byte(p.Question))
	for _, o := range p.Options {
		h.Write([]byte{0})
		h.Write([]byte(o))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// promptNonceWithHeader is promptNonce for a prompt under a dialog header (a
// tab bar or a chip), with the header and the current tab folded in.
//
// The header used to reach the nonce by accident: it sat inside Question, so
// answering one tab flipped its ☐ to ☒ and the next tab's nonce differed even
// when two tabs asked the same thing (muster#168's gotcha). Question no
// longer carries it (#204), so it goes in on purpose — and with the current
// tab, which the plain text cannot show: two tabs with the same question, the
// same options and neither answered read alike, and an answer meant for one
// must not land on the other.
func promptNonceWithHeader(p *fleet.SessionPrompt, header string, tab tabPosition) string {
	h := sha256.New()
	h.Write([]byte(p.Question))
	for _, o := range p.Options {
		h.Write([]byte{0})
		h.Write([]byte(o))
	}
	h.Write([]byte{0, 1})
	h.Write([]byte(strings.TrimSpace(header)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(tab.At)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// selectionPrompt reports whether a blocking prompt is on screen and, if so,
// what it is asking — the highlighted option, so a supervisor reading the
// state knows what it would be agreeing to.
//
// Both footers are matched. Knowing only one was a real incident: a
// folder-trust prompt and a session-resume prompt both say "Enter to confirm",
// and both classified as unknown, which reads as "cannot determine" rather
// than "blocked on a human".
func selectionPrompt(s screen) (string, bool) {
	if p := parsePrompt(s); p != nil {
		if p.Selected > 0 && p.Selected <= len(p.Options) {
			return strconv.Itoa(p.Selected) + ". " + p.Options[p.Selected-1], true
		}
		return "", true
	}
	return "", false
}

// awaitingSelection reports whether the TUI is showing a menu that blocks
// on a human keypress.

// spinner reports the most recent status line and whether it indicates a
// turn still in progress. Second return is false when no spinner line was
// found at all, which is a different fact from "found, and it was
// finished".
func spinner(s screen) (running bool, found bool) {
	// Never scan below the composer's own closing rule, when one is found.
	//
	// muster#229: a footer NOTICE the runtime paints under the composer
	// — "⚠ Transcript writes are failing (disk full — ENOSPC) · recent
	// messages may …" — has exactly the shape statusLine matches: a single
	// non-ASCII symbol, a space, a capitalised word, and (because the
	// runtime truncated the notice with an ellipsis) the running tense's own
	// marker. A backward scan that does not stop at the composer reads that
	// notice before it ever reaches the real, finished spinner line drawn
	// ABOVE the composer — and unlike a spinner, a footer notice does not
	// clear until the runtime next redraws the whole screen, so the false
	// "working" this produces can outlive the condition that caused it by
	// hours (measured: 1-2h after the disk that triggered it was freed).
	//
	// The turn-status line is transcript, and the TUI never draws it beneath
	// the composer — only chrome (the mode indicator, this notice, keyboard
	// hints) lives there. So bounding the scan at the composer's closing
	// rule is a structural fact about the layout, not a rule about this one
	// notice's wording: it holds for any footer content this driver has not
	// been told about yet, not only the ENOSPC case that found it.
	//
	// Only when a composer is actually FOUND: composerAbsent and
	// composerClipped mean this driver does not know where the composer
	// (and so the boundary) is, and the scan falls back to the whole screen
	// exactly as it always has for those.
	top := len(s.lines) - 1
	if _, last, scan := composerSpan(s); scan == composerFound {
		top = last
	}
	// Scan upward for the LAST line that is a status line in one of its two
	// shapes, rather than stopping at the first line that merely begins with
	// a symbol.
	//
	// The earlier version did stop at the first, which was safe only because
	// it matched exactly one glyph. Widening the glyph test (see
	// hasSpinnerGlyph) immediately broke it: the composer's own `❯` is below
	// the status line and begins with a symbol too, so every screen "found" a
	// spinner line in neither shape and gave up. Chrome is full of symbols —
	// `❯`, `⏵⏵`, `▸`, `⎿` — and a matcher loose enough to survive an
	// animation frame must not treat the first symbol it meets as decisive.
	for i := top; i >= 0; i-- {
		if running, ok := statusLine(strings.TrimSpace(s.lines[i])); ok {
			return running, true
		}
	}
	return false, false
}

// statusLine reports whether a line is the TUI's turn-status line, and whether
// that status is running.
//
// Three conditions, all structural, none of them a particular character:
//
//   - it begins with a single non-ASCII symbol followed by a space — the
//     status line's shape, and the part that varies (five animation frames
//     were live at once on one machine);
//   - the next word is capitalised, which is what the TUI's verb always is and
//     what tool-output lines below the transcript generally are not;
//   - it carries one of the two tense markers, `…` for running and " for " for
//     finished.
//
// The verb itself is deliberately not matched. It is drawn at random from a
// large set, and enumerating it would be the same mistake as enumerating
// footers (F37) or glyphs (F42).
func statusLine(line string) (running bool, ok bool) {
	if !hasSpinnerGlyph(line) {
		return false, false
	}
	_, size := utf8.DecodeRuneInString(line)
	rest := strings.TrimLeft(line[size:], " ")
	first, _ := utf8.DecodeRuneInString(rest)
	if !unicode.IsUpper(first) {
		return false, false
	}
	switch {
	case strings.Contains(rest, runningSuffixMarker):
		return true, true
	case strings.Contains(rest, finishedInfix):
		return false, true
	}
	return false, false
}

// hasSpinnerGlyph reports whether a line begins with the status line's leading
// symbol.
//
// # Why this is a shape and not a character
//
// It was one character — `✻` — and that was wrong in a way nothing reported.
// The glyph is an ANIMATION FRAME: a live fleet was using five of them
// (`✻ ✽ ✢ ✶ ✳`) at the same instant, so a session's status line was
// legible or invisible depending on which frame the capture happened to catch.
//
// The consequence was not a cosmetic miss. A session 21 minutes into a turn,
// with a perfectly good running spinner on screen, fell through to "no spinner
// line" and was reported `unknown` — 16% of one machine's sessions, entirely
// at random, refreshing every few hundred milliseconds.
//
// F37 said it about footers and it is true here: **match what the line is FOR,
// not how it is decorated.** What the line is for is announcing a turn, and
// the parts that carry that meaning are the ellipsis and the "for <duration>"
// — both already matched below. The leading glyph only has to be recognised as
// "a symbol, not text", which is a property of the character class rather than
// of any particular character.
func hasSpinnerGlyph(line string) bool {
	if line == "" {
		return false
	}
	r, size := utf8.DecodeRuneInString(line)
	if r == utf8.RuneError {
		return false
	}
	// Must be a symbol or punctuation glyph, not a letter or digit: this is
	// the part that keeps ordinary transcript prose from matching.
	if !unicode.IsSymbol(r) && !unicode.IsPunct(r) {
		return false
	}
	// ASCII punctuation is excluded deliberately. Transcript lines routinely
	// begin with `-`, `*`, `>`, `#` and `|`, and admitting those would make
	// every bulleted list a candidate status line.
	if r < utf8.RuneSelf {
		return false
	}
	// Box drawing and block elements are chrome, never a status glyph.
	//
	// Found on a live session: the runtime's welcome screen draws a panel
	// whose rows begin with `│`, and its "what's new" list is full of
	// truncated lines ending in `…`. That is a non-ASCII symbol, a space, a
	// capitalised word and an ellipsis — every condition below — so the splash
	// screen classified as a RUNNING SPINNER, and a freshly started session
	// reported `working` forever while it sat idle at its own welcome panel.
	//
	// This is the cost of loosening the glyph test (F42) arriving in a second
	// place. The first was the composer's own `❯`, caught by tests; this one
	// needed a live session, because no fixture contained a splash screen.
	if (r >= 0x2500 && r <= 0x259F) || (r >= 0x25A0 && r <= 0x25FF) {
		return false
	}
	// A single glyph followed by a space, which is the status line's shape.
	// Box-drawing rules and other chrome fail this because they run.
	rest := line[size:]
	return strings.HasPrefix(rest, " ")
}

// classifyPane is what callers use. It separates "the driver failed to read
// this pane" from "the driver read this pane and it was empty" — two facts
// that a single classify(text, alive) signature collapses into one
// indistinguishable `unknown`.
//
// That collapse is not hypothetical. A marker-corruption bug in the batched
// enumeration once caused every capture to be misfiled, so every session
// received an empty string, and every session classified as `unknown` with
// the evidence "pane captured empty". The driver reported a plausible fleet
// view, returned no error, and passed every unit test — because a driver
// that cannot read any screen at all is, at this signature, shaped exactly
// like a fleet of sessions that happen to be unreadable.
//
// This is §5.7 ("absence and failure are different answers") applied one
// level below where the spec states it. The spec makes the distinction for
// plural responses across machines; the same confusion is available inside
// a single driver, between a pane it failed to read and a pane with nothing
// in it, and it is just as capable of producing a confident wrong answer.
func classifyPaneAged(raw string, captured, alive, young bool) fleet.SessionState {
	st, _ := classifyPaneRemembering(raw, captured, alive, young, paneMemory{}, time.Time{})
	return st
}

// classifyPaneRemembering classifies a pane using what the driver saw of it
// last time, and returns the digest to remember for next time.
//
// A first sighting (prior.known == false) behaves exactly as before — which is
// the honest floor, since one capture genuinely cannot settle the ambiguity
// resolveAmbiguity settles.
func classifyPaneRemembering(raw string, captured, alive, young bool, prior paneMemory, now time.Time) (fleet.SessionState, string) {
	return classifyCaptureRemembering(paneCapture{text: raw}, captured, alive, young, prior, now)
}

// classifyCaptureRemembering is classifyPaneRemembering for a capture that
// carries its pane height, so the composer is read from the visible pane only
// (muster#169). The digest stays a digest of the whole capture: it
// fingerprints what was read, and the pane boundary changes nothing about that.
func classifyCaptureRemembering(c paneCapture, captured, alive, young bool, prior paneMemory, now time.Time) (fleet.SessionState, string) {
	raw := c.text
	if !captured {
		// No digest is returned for a failed capture: remembering the
		// fingerprint of a screen we did not read would make the next
		// comparison agree with nothing, or worse, agree with an earlier
		// failure and call two failures a stable screen.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"driver failed to capture this pane's screen; this is a driver "+
				"malfunction, not an observation about the session"), ""
	}
	st, amb := classifyAgedDetailVisible(raw, c.height, alive, young)
	digest := screenDigest(raw)
	return resolveAmbiguity(st, amb, prior, digest, now), digest
}

func classifyPane(raw string, captured, alive bool) fleet.SessionState {
	if !captured {
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"driver failed to capture this pane's screen; this is a driver "+
				"malfunction, not an observation about the session")
	}
	return classify(raw, alive)
}

// classify infers a session's state from its pane text and process
// liveness.
//
// alive is the only input here that is not a guess; it comes from the
// process table. Everything else is screen-reading, and the returned
// SessionState says so.
func classify(raw string, alive bool) fleet.SessionState {
	return classifyAged(raw, alive, false)
}

// ambiguity names a classification that a single screen cannot settle but a
// second look at the same screen can.
//
// # Why this is a return value rather than a comment
//
// Two branches below refuse to commit because "no spinner" has two readings: a
// session sitting at a fresh prompt, and a turn that began so recently it has
// not painted one yet. From one capture those are identical, and guessing
// either way is how a stalled session reads as busy or a busy one gets
// interrupted.
//
// From two captures they are not identical at all. A turn that had just begun
// paints within a second; a screen that is byte-for-byte unchanged seconds
// later was not mid-anything. That evidence exists in the driver's memory
// already — §8's `since` is stamped from it — so the resolution is passive,
// costs no extra capture, and never touches the session (the same reasoning
// as F34).
//
// The alternative was string-matching the evidence text at the call site,
// which makes a prose field load-bearing. Naming the ambiguity keeps the
// question in the type.
//
// # This driver's fold, named (#54)
//
// Every case below is this classifier folding together the same two named
// facts, and it is worth saying so once rather than leaving it to be
// noticed. `classifyPaneRemembering`'s five positional inputs are, in fold
// terms, two SOURCES:
//
//   - the screen — this read's own capture, timestamped `now`. Everything
//     `classifyAgedDetail` computes (spinner, composer, prompt, usage limit,
//     turn outcome) comes from here, and it comes from here ALONE: this
//     driver never observes anything else (classify.go's package comment).
//   - the screen's own history — `prior` (`paneMemory`: a digest and the
//     time it was taken), this same pane's previous read. Not a second
//     signal in the sense #52 means it (a second driver, a runtime's own
//     record) — it is TIME applied to the one signal this driver has. That
//     is why every resolution below can only narrow toward less activity,
//     never invent a new fact the screen itself never carried.
//
// `ambNoSpinnerEmpty` and `ambNoSpinnerPending` are the screen reporting
// itself unable to decide, resolved by asking the history source whether
// the screen has held still long enough that "a turn just began" stops being
// plausible. `ambUnrecognisedPrompt`, added for #58, is the same two sources
// used for a different question: not "can the screen decide", but "has the
// screen's own candidate answer earned the confidence it was built with" —
// see resolveUnrecognisedPrompt.
//
// # The screen is upgrade-only — stated once, made explicit
//
// #54's rule, and every branch of resolveAmbiguity obeys it: a resolution
// may only ever move a classification toward a MORE SPECIFIC state working
// FROM less activity, never invent activity from silence — `ambNoSpinnerPending`
// promotes toward `waiting_input`, never toward `working`; a screen that
// CHANGED between observations is left unresolved rather than called
// `working`, because content moves for reasons other than a turn (§5.6,
// "degrade, never emulate"). `ambUnrecognisedPrompt` does not violate this:
// upgrade-only says nothing about withholding a promotion the screen
// reports it cannot itself corroborate, which is a second and independent
// rule (#58's companion clause) — see resolveUnrecognisedPrompt's own
// comment for why "upgrade-only" alone would not have prevented that
// incident.
type ambiguity int

const (
	ambNone ambiguity = iota
	// ambNoSpinnerEmpty: no spinner, composer painted and empty.
	ambNoSpinnerEmpty
	// ambNoSpinnerPending: no spinner, composer holds unsent text.
	ambNoSpinnerPending
	// ambUnrecognisedPrompt: a structural prompt match with no footer to
	// corroborate it, and a kind classifyPromptKind does not recognise —
	// exactly the shape that produced #58. See resolveUnrecognisedPrompt.
	ambUnrecognisedPrompt
)

// spinnerPaintGrace is how long a turn is allowed to have started without
// having painted a spinner. Generous by an order of magnitude: the cost of
// waiting is a slightly later answer, while the cost of being wrong is
// reporting a working session as idle.
const spinnerPaintGrace = 2 * time.Second

// screenDigest fingerprints a captured screen so two observations can be
// compared without keeping the text.
//
// Keeping the text would be the obvious implementation and is the wrong one:
// pane content is somebody's actual work, and a driver that retains it has
// turned a state cache into a transcript store.
func screenDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// resolveAmbiguity settles a classification using what the same pane looked
// like last time, or leaves it unknown when there is nothing to settle it
// with.
//
// It only ever resolves toward *less* activity, and only on evidence of
// stability. A screen that CHANGED between observations is deliberately left
// unknown rather than called working: content moves for reasons other than a
// turn (a redraw, a resize, a notification), and §5.6 says degrade rather than
// emulate. One direction here has evidence behind it; the other would be a
// guess wearing a status.
func resolveAmbiguity(st fleet.SessionState, amb ambiguity, prior paneMemory, digest string, now time.Time) fleet.SessionState {
	// ambUnrecognisedPrompt inverts the shape every other case here uses: st
	// already IS the confident candidate — see classifyAgedDetail — and what
	// this decides is whether it has EARNED that confidence yet, not what it
	// should become. Handled first and separately because the early return
	// just below assumes the opposite default (an unresolved st, returned
	// as-is when there is nothing to promote it with), which is exactly
	// backwards for a candidate that must be held DOWN, not left AS IS,
	// absent corroboration.
	if amb == ambUnrecognisedPrompt {
		return resolveUnrecognisedPrompt(st, prior, digest, now)
	}
	if amb == ambNone || !prior.known || prior.digest != digest {
		return st
	}
	stable := now.Sub(prior.at)
	if stable < spinnerPaintGrace {
		return st
	}
	age := stable.Round(time.Second).String()
	// Both branches below MUTATE the candidate st and return it, rather than
	// building a fresh SessionState the way this code used to (muster
	// #76). InferredState only ever set Status, Confidence, Evidence and
	// Since — every other field the candidate carried (ControlChannel, most
	// concretely: measured landing a real corpus case, a session's genuinely
	// active channel read as absent a few seconds later, once this exact
	// promotion ran) was silently dropped, because nothing here re-attached
	// it and nobody had reason to notice: the promotion is upgrade-only, so
	// the session it fires on always looks healthy either way.
	//
	// Mutating st is the fix that cannot repeat the mistake: it carries
	// forward whatever the candidate had, known fields and any future one
	// alike, the same way resolveUnrecognisedPrompt's own corroborated
	// branch already returns st unchanged rather than rebuilding it. The
	// two fields set below are exactly the ones this promotion is actually
	// deciding; everything else on st is left as classifyAgedDetail built
	// it.
	//
	// Checked deliberately rather than assumed: Prompt is untouched here
	// because selectionPrompt's branch (above) returns before this switch is
	// ever reached, so it is always nil on a candidate carrying either of
	// these two ambiguities. ScreenDigest, Quota, LastTurn and
	// CredentialGeneration are stamped by callers in tmux.go AFTER this
	// function returns, so they were never at risk from this rebuild either
	// way — only ControlChannel (set inside classifyAgedDetail, before this
	// runs) was actually being lost.
	switch amb {
	case ambNoSpinnerEmpty:
		st.Status = fleet.StatusIdle
		st.Confidence = fleet.ConfidenceInferred
		st.Evidence = "no spinner, and the screen is unchanged after " + age +
			" — a turn that had just begun would have painted one by now"
		return st
	case ambNoSpinnerPending:
		// Unsent text on a screen that has stopped moving is the same
		// situation as the finished-turn case above: blocked on a human
		// pressing enter. §8's `since` then carries the age, which is the
		// discriminator between an operator mid-thought and a pane nobody
		// is coming back to.
		st.Status = fleet.StatusWaitingInput
		st.Confidence = fleet.ConfidenceInferred
		st.Evidence = "composer holds unsent input; screen unchanged after " + age
		st.WaitingOn = fleet.WaitingUnsentInput
		// ComposerDigest is NOT reassigned here (the prior version copied it
		// from itself under a different name): the candidate already
		// carries the value classifyAgedDetail computed from the composer
		// text — NOT the screen digest this function was handed, which
		// fingerprints something else and is not what discard compares
		// against.
		return st
	}
	return st
}

// resolveUnrecognisedPrompt implements #58's companion clause to #54's rule:
// a promotion made on evidence the source itself reports as unrecognised is
// not a promotion, it is unknown — until something corroborates it.
//
// # Why "the screen is upgrade-only" did not already prevent the incident
//
// The promotion #58 measured was working (or idle) -> waiting_input, which
// is precisely the direction resolveAmbiguity's upgrade-only rule permits.
// What was missing was not a direction check but a SECOND, independent
// question: did the source that produced this candidate recognise what it
// saw. `classifyPromptKind` returning empty is that source's own admission
// that it did not — see its package-level doc: "fails to empty, never to a
// guess" — and classifyAgedDetail was discarding that admission before the
// verdict formed, exactly as #58 named it.
//
// # The corroboration is the fold's EXISTING second source, reused
//
// st already carries the full candidate — Prompt, WaitingOn, evidence — built
// as if trusted. This does not rebuild it: it is either released unchanged
// (corroborated) or replaced with an honest `unknown` (not yet). The
// corroboration reuses screen-history, the same source ambNoSpinnerEmpty and
// ambNoSpinnerPending already fold in: the same screen, held past
// spinnerPaintGrace. A false read built from ordinary transcript text moving
// past does not sit still that long — the text was never the same "menu"
// twice, because it was never a menu. A real dialog this classifier has not
// been taught a kind for yet does sit still, because it is genuinely
// blocking on a human. That asymmetry is what makes the wait a real test and
// not merely a delay.
//
// First sighting (prior.known == false) takes the same honest floor every
// other ambiguity here takes: one capture cannot corroborate itself.
func resolveUnrecognisedPrompt(st fleet.SessionState, prior paneMemory, digest string, now time.Time) fleet.SessionState {
	if prior.known && prior.digest == digest && now.Sub(prior.at) >= spinnerPaintGrace {
		return st
	}
	return fleet.UnknownState(fleet.ConfidenceInferred,
		"screen shows what looks like a selection prompt, but its kind was not "+
			"recognised and no runtime footer corroborates it; treated as "+
			"unrecognised evidence (#58) rather than a confident prompt until it "+
			"is seen unchanged")
}

// paneMemory is the screen-history fold source (#54): what the driver
// remembers about one pane from its previous observation — a digest and
// when it was taken, never the text itself (see screenDigest). Empty
// (known == false) is the first sighting, and every resolution above treats
// that the same way: nothing to corroborate against yet.
type paneMemory struct {
	known  bool
	digest string
	at     time.Time
}

// classifyAged adds whether the session is young enough to plausibly still be
// starting — see the default branch for why that distinction matters.
func classifyAged(raw string, alive, young bool) fleet.SessionState {
	st, _ := classifyAgedDetail(raw, alive, young)
	return st
}

func classifyAgedDetail(raw string, alive, young bool) (st fleet.SessionState, amb ambiguity) {
	return classifyAgedDetailVisible(raw, 0, alive, young)
}

// classifyAgedDetailVisible is classifyAgedDetail with the capture's pane
// height, 0 when unknown (muster#169; see screen.visibleTop).
func classifyAgedDetailVisible(raw string, paneHeight int, alive, young bool) (st fleet.SessionState, amb ambiguity) {
	if !alive {
		// §8: dead is terminal. This is the one status this driver can
		// state without reading a screen, and still it is inferred: the
		// process being gone is observed, but "this session is dead"
		// infers that the process was the session.
		return fleet.InferredState(fleet.StatusDead, "pane process not present in process table", nil), ambNone
	}

	s := newScreenVisible(raw, paneHeight)

	// Stamped on every path below rather than at each return.
	//
	// The control channel is orthogonal to what the session is DOING: it is
	// true of a working session, an idle one, one blocked on a prompt and one
	// blocked by a usage limit alike, and every one of those leaves this
	// function by a different return. Threading it through each of them is how
	// a branch added later quietly stops reporting it — and a field that
	// silently stops being reported reads exactly like a healthy channel, which
	// is the failure it exists to end.
	//
	// The dead branch above returns before this is armed, deliberately: a pane
	// whose process is gone has no runtime left to be describing itself.
	//
	// The permission mode (#194) is stamped in the same place for the same
	// reason: it is a fact about the runtime's indicator row, true of a working
	// session, an idle one and one blocked on a prompt alike, and a branch added
	// later that returned around it would make the field read as "not observed"
	// exactly when a client cycling toward a mode most needs an answer.
	// Warnings (muster#230) is stamped in this same deferred block for
	// the identical reason ControlChannel and PermissionMode are: a footer
	// notice is true of a working session, an idle one, and one blocked on a
	// prompt alike, and a branch added later that returned around this would
	// make the field read as "nothing to report" exactly when a caller most
	// needs the answer.
	defer func() {
		st.ControlChannel = controlChannelOf(s)
		st.PermissionMode = permissionModeOf(s)
		st.Warnings = warningsOf(s)
	}()

	if len(s.lines) == 0 {
		return fleet.UnknownState(fleet.ConfidenceInferred, "pane captured empty"), ambNone
	}

	if option, blocked := selectionPrompt(s); blocked {
		evidence := "blocked on a prompt awaiting a keypress"
		if option != "" {
			// Name what it is asking. A supervisor deciding whether to
			// accept the default needs to know what the default IS.
			evidence += "; highlighted option: " + option
		}
		st := fleet.InferredState(fleet.StatusWaitingInput, evidence, nil)
		st.WaitingOn = fleet.WaitingPrompt
		st.Prompt = parsePrompt(s)
		if st.Prompt != nil {
			st.Prompt.Kind = classifyPromptKind(st.Prompt)
		}
		if st.Prompt != nil && st.Prompt.Kind == fleet.PromptFeedbackReview {
			// muster#215: the card is not a menu awaiting a keypress in the
			// sense every other prompt is, and saying so would be the wrong
			// thing to hand a person. Name what it is and why it is reported.
			st.Evidence = "the runtime is showing its feedback-draft card over the composer, which " +
				"pushes the composer's closing rule off the bottom of this pane: nothing can be " +
				"delivered until a person answers the card (review, send or dismiss) or the pane " +
				"is made taller"
		}

		// Companion clause, #58: a structural-only match (no footer) whose
		// kind classifyPromptKind also would not name is exactly the shape
		// that wedged a healthy session — the classifier's own admission
		// that it could not classify what it saw, reported as a confident
		// answer anyway. That admission must survive into the verdict
		// rather than being discarded here, so this is not reported at full
		// confidence on a single read; see resolveUnrecognisedPrompt.
		//
		// A footer-corroborated match, or one classifyPromptKind DID name,
		// is unaffected — the footer, or the recognised kind, is already
		// independent corroboration and this driver has trusted either one
		// immediately since before #58.
		if st.Prompt != nil && st.Prompt.Kind == "" && !promptFooterPresent(s) &&
			!reviewScreenPrompt(st.Prompt) {
			return st, ambUnrecognisedPrompt
		}
		return st, ambNone
	}

	// Checked before anything that could resolve to idle. A session blocked by
	// a usage limit paints exactly like a healthy one waiting for work — empty
	// composer, no spinner — and `idle` is the single status that means "send
	// it work", so this is the one misreading that actively causes harm.
	if hint, blocked := usageLimit(s); blocked {
		evidence := "blocked by a usage limit"
		if hint != "" {
			evidence += "; resets " + hint
		}
		// waiting_input, deliberately: the session is blocked and only a human
		// unblocks it (wait it out, or switch accounts). Prompt stays nil —
		// there is nothing to answer, and §2.7 is optional for exactly this
		// reason.
		// quota_blocked, which §2.3 has defined since the first commit as
		// "alive but refused by its provider" and §8 gives transitions for.
		// It was reported as waiting_input for several hours because nobody
		// checked the enum — see F52. waiting_input was always slightly false
		// here: the session is not blocked on a HUMAN, it is blocked on a
		// clock or an account, and no answer from a caller unblocks it.
		st := fleet.InferredState(fleet.StatusQuotaBlocked, evidence, nil)
		st.Quota = &fleet.QuotaBlock{ResetHint: hint}
		return st, ambNone
	}

	// muster#215: the feedback-draft card over a composer this driver cannot
	// read as a whole, and not the answerable key row: text on its ❯ row that a
	// key would be appended to, or (muster#217) one of the card's other
	// states — the send confirmation, the in-flight line, the send error — whose
	// wrapped text takes rows the key row does not, so that even the ❯ row is
	// off the bottom. None is a prompt (a person answers them at the terminal),
	// and none is idle: the finished-spinner branch below would say "composer
	// empty" for a composer this driver did not find, which is the false idle the
	// card produced.
	if c, ok := liveFeedbackCard(s); ok && !c.composerFound && !c.answerable() {
		return fleet.UnknownState(fleet.ConfidenceInferred, c.evidence()), ambNone
	}

	// muster#217: the runtime's feedback panel replaces the composer. The
	// spinner line above it still reads as a finished turn, and the branch below
	// would call the screen idle with an empty composer that is not there.
	if _, ok := liveFeedbackPanel(s); ok {
		return fleet.UnknownState(fleet.ConfidenceInferred, feedbackPanelEvidence), ambNone
	}

	running, foundSpinner := spinner(s)

	// Unsent composer text is checked after the spinner, because a running
	// turn with queued input is still working — the queued text is a send
	// hazard (§2.4), not a state.
	pending, composerScanResult := composerText(s)
	hasComposer := composerScanResult == composerFound
	// clipped (muster#134): a composer is structurally there, this
	// driver just could not read it in full. Handled as its own branch
	// below, ahead of every case that would otherwise assert "composer
	// empty" from hasComposer being false — clipped must never be read as
	// empty, and must never be read as absent (see composerText's contract).
	clipped := composerScanResult == composerClipped

	switch {
	case foundSpinner && running:
		return fleet.InferredState(fleet.StatusWorking, "spinner line in running form", nil), ambNone

	case foundSpinner && !running && hasComposer && pending != "":
		// Turn finished, and a human has typed something they have not
		// sent. The session is not working and not merely idle: it is
		// holding input. waiting_input is the honest §2.3 member — the
		// session is blocked on a human (to press enter).
		held := fleet.InferredState(fleet.StatusWaitingInput,
			"turn finished; composer holds unsent input", nil)
		held.WaitingOn = fleet.WaitingUnsentInput
		held.ComposerDigest = composerTextDigest(pending)
		return held, ambNone

	case foundSpinner && !running && clipped:
		// muster#134: the turn finished, but this driver's own capture
		// ended above the composer's opening fence — it genuinely cannot
		// tell whether a human left unsent text sitting there. The
		// composer-empty branch just below must not fire for this screen:
		// asserting "empty" here is the exact false negative that lets
		// Discard report an untouched composer already clear.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"turn finished; composer "+composerClippedCause(s)+", cannot confirm it is empty"), ambNone

	case foundSpinner && !running && !hasComposer:
		// muster#216: the turn finished and no composer was found. The
		// branch below says "composer empty", which is a claim about a composer
		// and this screen has none this driver read — it used to fall through to
		// it, so a screen the send gate refuses ("no composer has been painted")
		// read `idle`, the one status that means "send it work". Nothing on
		// screen says the session will take input, so nothing says idle.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"turn finished; no composer found in the pane, cannot confirm the session will "+
				"take input (a full-screen interface has none of its own to paint)"), ambNone

	case foundSpinner && !running:
		// Idle is the honest status: the session is up and will take input.
		// But a turn that DIED here looks identical to one that finished, and
		// collapsing those two is how abandoned work goes unnoticed — so the
		// state carries a footnote when the screen says the last turn failed.
		st := fleet.InferredState(fleet.StatusIdle, "spinner line in finished form; composer empty", nil)
		if turn, failed := lastTurnFailed(s); failed {
			st.LastTurn = turn
			st.Evidence = "turn ended in an error; session is up and will accept input"
		}
		return st, ambNone

	case !foundSpinner && clipped:
		// Same reasoning as the spinner-finished clipped case above, for the
		// no-spinner branches below: none of them may assert composer-empty
		// or composer-absent for a screen this driver could not fully read.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no spinner line; composer "+composerClippedCause(s)+", cannot confirm it is empty"), ambNone

	case !foundSpinner && hasComposer && pending == "" && young:
		// A young session with a painted composer, no spinner and nothing
		// typed has not had a turn yet — so the "a turn may have just
		// begun" ambiguity below cannot apply to it. It is up and waiting
		// for work.
		//
		// This matters beyond tidiness: a caller asking "did this spawn
		// actually produce a running agent" needs an answer, and `unknown`
		// for a session whose interface is visibly painted is the reading
		// that let a dead spawn look the same as a healthy one.
		return fleet.InferredState(fleet.StatusIdle,
			"interface painted, composer empty, no turn yet", nil), ambNone

	case !foundSpinner && hasComposer && pending == "":
		// No spinner at all and an empty composer: most likely a session
		// sitting at a fresh prompt. "Most likely" is not good enough to
		// claim idle over working — a turn that has just begun may not
		// have painted a spinner yet.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no spinner line; composer present and empty"), ambNoSpinnerEmpty

	case !foundSpinner && hasComposer && pending != "":
		pendingState := fleet.UnknownState(fleet.ConfidenceInferred,
			"no spinner line; composer holds unsent input")
		// Computed HERE, where the composer text exists. The resolution path
		// below has only the screen digest, and publishing that instead made
		// the field useless: a caller quoted back a fingerprint of the whole
		// screen while discard compared the text, so a correct call could
		// never match.
		pendingState.ComposerDigest = composerTextDigest(pending)
		return pendingState, ambNoSpinnerPending

	default:
		// No composer found. Two very different situations share this
		// shape, and a caller needs them apart: a runtime still painting
		// its interface, and a pane that is not running the runtime at all.
		//
		// §8 has `starting` for the first and §2.3 has `unknown` for the
		// second, and returning `unknown` for both was why a spawn that
		// never got past a boot screen "read as healthy" — nothing could
		// distinguish "not up yet" from "cannot tell".
		//
		// Age is the discriminator, and it is honest: young means plausibly
		// still booting, old means something else is going on. The caller
		// gets the distinction; the confidence stays inferred.
		if young {
			return fleet.InferredState(fleet.StatusStarting,
				"no TUI composer yet; session is young enough to still be starting", nil), ambNone
		}
		// muster #64: "pane may not be running the expected runtime" names
		// only one of the two situations this shape actually covers, and picks
		// the one that makes the pane sound broken. The other, measured
		// directly: the runtime takes the whole screen for a full-screen
		// interface — a control-channel dialog, for one — which has no
		// composer of its own to paint and looks identical to a pane running
		// the wrong thing from here. Composer absence is the only fact this
		// branch has established; naming just one cause as if it were the
		// finding is what #64 measured going wrong.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no TUI composer found in pane; this may be a full-screen interface "+
				"with no composer of its own (a dialog, for one), or the pane may "+
				"not be running the expected runtime — composer absence alone "+
				"cannot tell those apart"), ambNone
	}
}
