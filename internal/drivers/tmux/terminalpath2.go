package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// errBracketPasteUnavailable wraps every refusal pasteBracketed returns, so a
// caller (Send) can tell "this pane cannot take a safe bracketed delivery"
// apart from an ordinary multiplexer failure with errors.Is, the same shape
// this package's other sentinel errors already give their callers.
var errBracketPasteUnavailable = errors.New("tmux: bracketed paste is not available on this pane")

// Terminal path v2 — muster round-1 research answered here (see the
// scratch research this prototype was built against; every item below is
// keyed to that research's own D-numbering so a reviewer can trace fix to
// defect without re-deriving it):
//
//	D1  landed-check false negative on Claude Code's own word-wrap  -> confirmLandedV2 (this file)
//	D2  non-bracketed paste, CR-converted, swallows/splits          -> pasteBracketed (this file)
//	D3  long single-line pastes collapse/drop bytes                 -> pasteBracketed uses -p -r (no CR conversion, no chunk boundary this driver introduces)
//	D4  no per-session serialisation                                -> terminalpath2_lock.go
//	D5  stranded-record lifecycle gaps                               -> terminalpath2_stranded.go
//	D6  screen-based confirmation is the weak link                  -> terminalpath2_transcript.go
//	D7  routing (force the terminal path)                           -> Route (driver.SendOptions; #184), inbox.go's inboxEligible
//
// This file holds the two D1/D2 fixes: bracketed, literal-newline delivery
// (replacing the CR-converting, non-bracketed paste-buffer call D2 names),
// and a composer-region-scoped, wrap-tolerant landed confirmation (replacing
// the raw-capture 24-byte-tail substring match D1 names).

// sanitizeForBracketedPaste strips bytes that could otherwise let the
// PAYLOAD end the bracket-paste sequence early — an embedded literal
// ESC[201~ (the bracketed-paste END marker) in a message this driver pastes
// would, if delivered unmodified, close the bracket ahead of schedule and
// leave the remainder of the message interpreted as ordinary (unbracketed)
// terminal input instead of pasted text. That is an injection: the payload
// would be deciding, mid-delivery, that the rest of itself is no longer a
// paste.
//
// ESC (0x1B) is dropped outright rather than merely refused-on-detection,
// because a caller has no legitimate reason to deliver a raw escape byte
// through this path — every documented reason to affect the runtime by key
// name goes through Keys (fleet.KeyName), never through Send's text.
//
// Every other C0 control byte (0x00-0x1F) is dropped too, except the two
// this driver's own delivery shape depends on: \n (the payload's own line
// breaks — the entire point of asking tmux for literal newlines with -r
// instead of the old CR-converting default) and \t (an ordinary character a
// human or an agent may legitimately type). \r is deliberately NOT kept: it
// is not content, and letting it through would reintroduce exactly the
// "did the multiplexer just turn a payload byte into an Enter-equivalent"
// ambiguity this whole change exists to remove — see -r's own semantics
// (tmux(1): "-r causes tmux to send \r rather than \n to the pane"), which
// is why this driver asks for -r AND never emits a \r of its own into the
// buffer it hands tmux.
//
// DEL (0x7F) is dropped for the same reason as the other C0 controls: it is
// a control code by convention even though it sits outside the 0x00-0x1F
// block.
func sanitizeForBracketedPaste(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' || r >= 0x7f && r <= 0x9f }) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r >= 0x7f && r <= 0x9f:
			// DEL, and the C1 controls (#180 M7): U+009B is the 8-bit form
			// of CSI, so "\u009b201~" is the paste-END sequence spelled
			// another way — measured to pass tmux into a bracketed paste
			// unchanged — and U+009D/U+0090 open OSC/DCS strings.
			continue
		case r < 0x20:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// bracketPasteFlag asks tmux whether the target pane's current occupant has
// asked the terminal for bracketed-paste mode (`#{bracket_paste_flag}`,
// tmux(1)) — the flag every terminal application that wants literal pastes
// distinguished from typed input sets, and the flag D2's non-bracketed
// `paste-buffer` call ignored entirely.
//
// The answer has three shapes, and a caller must keep them apart:
//
//	"1"  the occupant asked for bracketed paste
//	"0"  it did not
//	""   this tmux does not know the variable (it first appears in 3.7, #333)
//
// ok is false only when the query itself could not be answered (the
// multiplexer call failed) — never a stand-in for "flag is 0" or for "". An
// unanswerable query is "cannot confirm safety", which fails closed the same
// as an answered "0" does (see pasteBracketed), but for a different,
// worth-counting reason.
func (d *Driver) bracketPasteFlag(ctx context.Context, paneID string) (flag string, ok bool) {
	out, err := d.run(ctx, d.bin, "display-message", "-p", "-t", paneID, "#{bracket_paste_flag}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// tmuxVersionText is `tmux -V` for the refusal that has to name it, or "" when
// the multiplexer will not say. Only the refusal path asks, so a delivery that
// goes through pays nothing for it.
func (d *Driver) tmuxVersionText(ctx context.Context) string {
	out, err := d.run(ctx, d.bin, "-V")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// pasteBracketed is terminal-path-v2's replacement for the plain
// `paste-buffer -d` call D2 names: bracketed (`-p`) so the occupant receives
// the payload as one paste rather than as a stream of key-equivalent bytes,
// and literal-newline (`-r`) so an embedded "\n" in a multi-line message
// arrives as "\n", not as the CR D2 measured being read as Enter mid-payload
// (9/9 labelled sends swallowing their own first line; 27/27 short
// multi-line payloads splitting/early-submitting).
//
// # Why this refuses rather than falling back to the old paste on flag=0
//
// The task that asked for this file was explicit: falling back silently
// reintroduces D2 exactly on the pane where this driver could not first
// confirm the runtime asked for bracketed mode — the one case the fallback
// would be exercised on is the one case nobody could have tested it against,
// because #{bracket_paste_flag} said no. Round-1's own measurement is that
// every live Claude Code pane sampled reported bracket_paste_flag=1, so this
// refusal is expected to fire on live traffic approximately never; it exists
// for a pane whose occupant never asked for bracketed mode — a case where
// guessing which delivery shape is safe is exactly the guess this driver's
// own §2.4/§5.6 discipline (fail closed, never emulate) refuses elsewhere.
//
// It is NOT protection against a shell: an interactive shell turns bracketed
// paste on at its own prompt (measured, #180 H2), so the flag reads 1 there
// too. What stops a paste reaching a shell is foregroundIsRuntime
// (foreground.go), checked before this is ever called.
//
// The alternative considered and rejected: keep the old call as a fallback
// gated on flag=0. Rejected because nothing in this codebase, before this
// change, measured what flag=0 actually looks like on a real Claude Code
// pane (round-1 measured only flag=1), so a fallback branch would be
// deploying UNMEASURED behaviour disguised as graceful degradation — the
// same shape of mistake §5.6's "degrade, never emulate" already names.
//
// # A multiplexer older than 3.7 (#334)
//
// Before 3.7 the flag cannot be read at all, so "refuse unless it reads 1"
// would refuse every send on a distribution's tmux. There the query is skipped
// in effect (it reads empty and the release is known to predate the variable)
// and the same `paste-buffer -p -r` is issued: tmux itself brackets exactly
// when the pane asked, so a pane that did is delivered to as on 3.7, and one
// that did not receives the payload's newlines as plain input — which the
// read-back in Send then has to catch before Enter, as it must anyway.
func (d *Driver) pasteBracketed(ctx context.Context, paneID, text string) error {
	flag, ok := d.bracketPasteFlag(ctx, paneID)
	if !ok {
		d.counters.incr(counterBracketPasteFlagUnknown)
		return fmt.Errorf("%w: could not confirm this pane's occupant has bracketed paste mode "+
			"enabled (the #{bracket_paste_flag} query itself failed) — refusing to guess between "+
			"a bracketed delivery and the older CR-converting one; see pasteBracketed's own doc "+
			"comment for why there is no fallback", errBracketPasteUnavailable)
	}
	if flag == "" {
		// An empty answer is how tmux says it does not know the variable. It
		// is not the "0" of a pane that declined bracketed paste, so what to do
		// depends on which multiplexer printed it (#333, #334).
		have := d.tmuxVersionText(ctx)
		if provides, known := TmuxProvidesBracketPasteFlag(have); known && !provides {
			// A release older than 3.7 cannot answer, whatever the pane did, and
			// its `paste-buffer -p` brackets only when the pane asked (cmd-paste-
			// buffer.c, 3.4; measured here on 3.4 and 3.6a). So the delivery is
			// the same -p -r call with nothing read first. Both arrivals were
			// measured against the real runtime: bracketed, a long paste collapses
			// to one marker; unbracketed, it lands as raw rows and nothing submits
			// it by itself (Enter is pressed only after Send's landed check). That
			// check is not claimed to tell the two apart; the compat check G2 is
			// what proves a build bracketed on this tmux. See docs/compat.md.
			d.counters.incr(counterBracketPasteUnreadablePre37)
		} else {
			// A 3.7+ tmux that printed nothing, or a build whose version this
			// driver will not guess at: no delivery shape was measured against it.
			d.counters.incr(counterBracketPasteFlagUnsupported)
			if have == "" {
				have = "this tmux"
			}
			return fmt.Errorf("%w: %s did not report #{bracket_paste_flag} (it reads empty; the variable first "+
				"exists in tmux %s), so this driver cannot confirm the pane's occupant asked for bracketed paste "+
				"and refuses rather than falling back to the CR-converting paste this change replaces — use "+
				"tmux %s or newer and point FLEET_TMUX_BIN at it (muster doctor names this as runtime.tmux-version)",
				errBracketPasteUnavailable, have, BracketPasteFlagSince, BracketPasteFlagSince)
		}
	} else if flag != "1" {
		d.counters.incr(counterBracketPasteFlagOff)
		return fmt.Errorf("%w: this pane's occupant has not asked the terminal for bracketed "+
			"paste mode (#{bracket_paste_flag} reads 0) — delivering literal newlines un-bracketed "+
			"would be read as Enter keystrokes mid-message (muster round-1 D2); refusing "+
			"rather than falling back to the CR-converting paste this change replaces", errBracketPasteUnavailable)
	}

	sanitized := sanitizeForBracketedPaste(text)

	// load-buffer reads from stdin when given "-"; this driver writes to a
	// temp file instead so the payload never traverses argv (unchanged
	// rationale from the original Send: §5.3, "a shared namespace is a
	// shared namespace").
	f, err := os.CreateTemp("", "fleet-send-*")
	if err != nil {
		return fmt.Errorf("send: staging payload: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(sanitized); err != nil {
		f.Close()
		return fmt.Errorf("send: staging payload: %w", err)
	}
	f.Close()

	bufName := "fleet-" + d.nonce()
	args := []string{
		"load-buffer", "-b", bufName, f.Name(), ";",
		// -p: bracket the payload so the occupant's own bracketed-paste
		// handling receives it as one paste. -r: send "\n" as "\n" rather
		// than translating it to "\r" (tmux's default) — the literal-newline
		// half of this change; see this function's own doc comment.
		"paste-buffer", "-p", "-r", "-b", bufName, "-t", paneID, "-d",
	}
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return fmt.Errorf("send: delivering: %w", err)
	}
	return nil
}

// composerSuffixMinBytes is how long a tail must be before a composer showing
// only the tail of what was sent counts as this delivery (the runtime scrolls
// its composer inside itself on a tall paste). Short tails are coincidences:
// a trailing "." or a common closing word.
const composerSuffixMinBytes = 24

// composerRegionMatch reports whether the composer on sc renders text
// (composerMatchesText), with the evidence phrase a receipt carries.
func composerRegionMatch(sc screen, text string, allowSuffix bool) (matched bool, evidence string) {
	segs, scan := composerSegments(sc)
	if scan != composerFound || len(segs) == 0 {
		return false, ""
	}
	if segmentsMatchText(segs, text, false) {
		return true, "composer region renders the sent text exactly (row joins and composing form aside)"
	}
	if allowSuffix && segmentsMatchText(segs, text, true) {
		return true, "composer region renders a substantial suffix (tail) of the sent text — the runtime's own composer scrolled to show only the end of a tall paste"
	}
	return false, ""
}

// landedBy names the evidence a landed check rested on.
type landedBy int

const (
	landedNone landedBy = iota
	// landedByRegion: the composer's own rows render the text.
	landedByRegion
	// landedByMarker: exactly one collapsed-paste marker appeared inside the
	// composer since the pre-paste snapshot.
	landedByMarker
	// landedByClippedTail: a fresh send's composer grew past the visible top
	// and its visible rows render a substantial tail of the text (#149).
	landedByClippedTail
	// landedByNeedle: the last 24 bytes of the text's last line appear inside
	// the composer's own rows — the fallback for a composer that renders the
	// text in a shape the row model does not (#143, #145).
	landedByNeedle
	// landedByOwnMarker: a resume found the composer holding exactly the
	// collapsed-paste marker this driver's own record says landed the text
	// (#180 H1).
	landedByOwnMarker
)

// landCheck is what a landed check may accept.
type landCheck struct {
	// before is the composer's marker counts from a capture taken just before
	// this delivery's own paste. Only a fresh send has one.
	before map[pasteKey]int
	// strict is a resume: the question is "is the text this driver stranded
	// earlier sitting there, complete, right now". Every signal that could be
	// satisfied by something other than that text is off — the needle, the
	// marker diff, the clipped tail and the suffix rule.
	strict bool
	// digestVerified (strict only): the composer's content is already proven
	// to be what this driver recorded at strand time, so a tail render of
	// the driver's own text may be accepted again.
	digestVerified bool
	// ownMarker (strict only): the marker the stranded record says landed
	// the text. A composer holding exactly that marker and nothing else is
	// the driver's own collapsed paste (#180 H1).
	ownMarker   pasteKey
	ownMarkerOK bool
	// sessionID lets the check notice a respond waiting for this
	// session's lock (#180 M4) and stop, not landed, so the respond gets
	// the lock at once.
	sessionID string
}

// landing is a landed check's answer.
type landing struct {
	ok      bool
	by      landedBy
	key     pasteKey
	atCount int
	// dialog: the check stopped because a selection menu appeared. Nothing
	// will press Enter into it; the caller strands the delivery and says so.
	dialog bool
	// preempted: the check stopped because a respond is waiting for this
	// session's lock (#180 M4).
	preempted bool
}

// confirmLandedV2 is the test-facing form of landV2 for a fresh send
// (strict=false) or a resume (strict=true).
func (d *Driver) confirmLandedV2(ctx context.Context, paneID, text string, before map[pasteKey]int, strict bool, digestVerified bool) (pasteKey, int, bool) {
	l := d.landV2(ctx, paneID, text, landCheck{before: before, strict: strict, digestVerified: digestVerified})
	return l.key, l.atCount, l.ok
}

// landV2 decides whether text has landed in the composer, polling until
// submitConfirmWindow runs out.
//
// Every signal reads the composer's own rows and nothing else (#180): the
// fenced region when the composer is found, the visible rows above the
// closing rule when it is clipped. Transcript above the composer is whatever
// the agent printed; text below the closing rule belongs to something else —
// the measured case is a shell drawing under a composer frame the runtime
// left behind when it exited, echoing the message it was about to run as a
// command (#180 H2). Marker counts come from the same capture shape before
// and after the paste, so a marker in the history margin can never read as
// gained (#180 L2).
//
// A capture showing a selection menu ends the check at once, not landed
// (#180 M4): nothing will press Enter into a dialog, and holding the
// session's lock through the whole window would keep a respond to that very
// dialog waiting.
func (d *Driver) landV2(ctx context.Context, paneID, text string, c landCheck) landing {
	if strings.TrimSpace(text) == "" {
		return landing{ok: true}
	}
	needle, allowSuffix := landArgs(text, c)

	deadline := d.now().Add(submitConfirmWindow)
	for {
		if d.preempted(c.sessionID) {
			d.counters.incr(counterLandConfirmPreempted)
			return landing{preempted: true}
		}
		if sc, capOK := d.captureForClassify(ctx, paneID); capOK {
			if awaitingSelection(sc) {
				d.counters.incr(counterSendRefusedDialogRace)
				return landing{dialog: true}
			}
			if l, ok := d.landedOn(sc, text, needle, allowSuffix, c); ok {
				return l
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			d.counters.incr(counterLandConfirmTimeout)
			return landing{}
		}
		select {
		case <-ctx.Done():
			d.counters.incr(counterLandConfirmTimeout)
			return landing{}
		case <-time.After(submitConfirmInterval):
		}
	}
}

// landArgs derives the tail needle (the last line's last 24 bytes) and
// whether the suffix rule is allowed for a check.
func landArgs(text string, c landCheck) (needle string, allowSuffix bool) {
	needle = strings.TrimSpace(text)
	if idx := strings.LastIndexByte(needle, '\n'); idx >= 0 {
		needle = needle[idx+1:]
	}
	if len(needle) > 24 {
		needle = needle[len(needle)-24:]
	}
	return needle, !c.strict || c.digestVerified
}

// preSubmit is the verdict of the last look before Enter.
type preSubmit int

const (
	preSubmitOK preSubmit = iota
	// preSubmitDialog: a selection menu is showing; Enter would answer it.
	preSubmitDialog
	// preSubmitNotHeld: no composer is positively showing this delivery —
	// a half-painted dialog, a screen with no composer, a composer showing
	// something else. Enter would go somewhere this driver cannot name.
	preSubmitNotHeld
	// preSubmitNotRuntime: the pane's foreground process is not the
	// runtime (foreground.go) — a shell, say, under a composer frame the
	// runtime left behind. Enter would run the text as a command.
	preSubmitNotRuntime
	// preSubmitPreempted: a respond is waiting for this session's lock
	// (#180 M4) — most likely to answer a dialog; Enter is not pressed.
	preSubmitPreempted
)

// preSubmitCheck is the last look before the submit keystroke (#180 M6):
// one fresh capture must POSITIVELY show a composer holding this delivery,
// by the same evidence the landed check accepted — not merely fail to show a
// recognised menu. Anything else refuses and the stranded record is kept.
func (d *Driver) preSubmitCheck(ctx context.Context, target *paneRow, text string, c landCheck) preSubmit {
	if d.preempted(c.sessionID) {
		d.counters.incr(counterLandConfirmPreempted)
		return preSubmitPreempted
	}
	if ok, _ := d.foregroundIsRuntime(ctx, target); !ok {
		return preSubmitNotRuntime
	}
	sc, ok := d.captureForClassify(ctx, target.paneID)
	if !ok {
		return preSubmitNotHeld
	}
	if awaitingSelection(sc) {
		d.counters.incr(counterSendRefusedDialogRacePreSubmit)
		return preSubmitDialog
	}
	needle, allowSuffix := landArgs(text, c)
	if _, held := d.landedOn(sc, text, needle, allowSuffix, c); !held {
		d.counters.incr(counterSendRefusedNotHeldPreSubmit)
		return preSubmitNotHeld
	}
	return preSubmitOK
}

// preSubmitReason words a refusal of the last look before Enter. resumed says
// whether this was a resume of an earlier strand.
func preSubmitReason(v preSubmit, resumed bool) string {
	lead := "text landed in the composer and was attributed to this delivery, but "
	tail := " It is sitting there unsent; retry the same send with resumeIfStranded to submit it"
	if resumed {
		lead = "resumed a delivery this driver had stranded earlier and confirmed it landed, but "
		tail = " The record is kept — retry the same send with resumeIfStranded again"
	}
	switch v {
	case preSubmitDialog:
		return lead + "a selection menu appeared on this pane in the moment before submit would " +
			"have been pressed; pressing Enter now would answer that menu instead of submitting " +
			"this message, so it was not pressed." + tail
	case preSubmitPreempted:
		return lead + "a respond to this session arrived in the moment before submit and was " +
			"given priority, so submit was not pressed." + tail
	case preSubmitNotRuntime:
		return lead + "in the moment before submit this pane's foreground process was no longer " +
			"the agent runtime; pressing Enter could run the text as a command, so it was not " +
			"pressed." + tail
	}
	return lead + "in the moment before submit the composer no longer positively showed this " +
		"delivery — something else was painting the screen; pressing Enter into a screen this " +
		"driver cannot read could answer a dialog, so it was not pressed." + tail
}

// markerLinesFit reports whether a collapsed-paste marker's "+L lines" fits
// text: L is the number of newlines in the text (measured on the runtime),
// with or without trailing ones.
func markerLinesFit(lines int, text string) bool {
	return lines == strings.Count(text, "\n") || lines == strings.Count(strings.TrimRight(text, "\n"), "\n")
}

// landedOn is one landed check against one capture.
func (d *Driver) landedOn(sc screen, text, needle string, allowSuffix bool, c landCheck) (landing, bool) {
	if matched, _ := composerRegionMatch(sc, text, allowSuffix); matched {
		d.counters.incr(counterLandConfirmByComposerMatch)
		return landing{ok: true, by: landedByRegion}, true
	}
	rows, scan := composerRegion(sc)
	if c.strict {
		// #180 H1: the runtime collapses a long or many-line paste to one
		// "[Pasted text #N +L lines]" marker, which no text comparison can
		// read back. A resume accepts a composer holding exactly one such
		// marker, and nothing else, when it is provably this driver's own:
		// the marker the record says the paste landed as, or any marker when
		// the composer's digest already matches the record. Its "+L lines"
		// must fit the text either way.
		if scan == composerFound && (c.ownMarkerOK || c.digestVerified) {
			if segs, _ := composerSegments(sc); len(segs) == 1 {
				if _, isMarker := parsePastedTextMarker(segs[0]); isMarker {
					for key, n := range markerCounts(composerRuneMarker + " " + segs[0]) {
						if n == 1 && markerLinesFit(key.lines, text) && (c.digestVerified || key == c.ownMarker) {
							d.counters.incr(counterLandConfirmByOwnMarker)
							return landing{ok: true, by: landedByOwnMarker, key: key, atCount: 1}, true
						}
					}
				}
			}
		}
		return landing{}, false
	}
	switch scan {
	case composerFound:
		if strings.Contains(strings.Join(rows, "\n"), needle) {
			d.counters.incr(counterLandConfirmByLegacyNeedle)
			return landing{ok: true, by: landedByNeedle}, true
		}
	case composerClipped:
		var segs []string
		for _, r := range rows {
			if seg := collapseSpace(r); seg != "" {
				segs = append(segs, seg)
			}
		}
		if segmentsMatchText(segs, text, true) {
			d.counters.incr(counterLandConfirmByClippedTail)
			return landing{ok: true, by: landedByClippedTail}, true
		}
	}
	after := composerMarkers(sc)
	if key, ok := gained(c.before, after); ok {
		d.counters.incr(counterLandConfirmByMarker)
		return landing{ok: true, by: landedByMarker, key: key, atCount: after[key]}, true
	}
	return landing{}, false
}
