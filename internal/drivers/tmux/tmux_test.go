package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
	"github.com/futurelastic/muster/internal/trustseed"
)

// fakeMux is a stand-in for the multiplexer binary. It records every
// invocation so tests can assert on subprocess count — the property
// driver.Driver.List's doc comment calls a "correct-looking bug" if got
// wrong — and on what never reached a command line (§5.3).
// fakeMux is shared between the test goroutine and the driver's engine
// goroutine whenever a subscription is live, so every field is guarded. An
// earlier version was not, and the race detector found it the moment a test
// mutated state while a stream was running — a fault in the harness rather
// than the driver, but one that would have made every subscription test
// quietly untrustworthy.
type fakeMux struct {
	mu         sync.Mutex
	calls      [][]string
	sessions   []fakeSession
	captures   map[string]string
	failList   bool
	failCreate bool
	// buffers/pasted make the fake behave like a terminal: text delivered
	// through the paste buffer becomes visible in a later capture. Without
	// that, send() can never confirm its own delivery and the confirmation
	// path is untestable.
	buffers map[string]string
	pasted  map[string]string
	noEcho  bool // simulate a pane that never renders what was pasted
	// swallowSubmit simulates the defect Send now checks for: the text is
	// delivered and rendered, the submit keystroke goes nowhere, and the
	// composer is left holding the line.
	swallowSubmit bool
	// composerLines models a composer holding several LOGICAL lines, so
	// send-keys can model what C-u (unix-line-discard) actually does against
	// one: clear the line the cursor sits on and leave the rest standing,
	// rather than the whole buffer at once. Only set for a pane a test put
	// there via setMultilineComposer; every other pane keeps the older
	// all-at-once behaviour below, so this changes nothing for a test that
	// never asked for it.
	composerLines map[string][]string
	// frozen models a pane where the clear keystroke never registers at
	// all — the untouched half of #32's missing branch, as distinct from
	// composerLines' partial-clear model of the damaged half.
	frozen map[string]bool
	// frozenUnixLineDiscard models muster#138's own shape: C-u (even
	// preceded by composerLineEndKey) makes no progress on this pane — as
	// if the substrate does not honour the positioning key at all — while
	// Backspace still clears a row normally. Distinct from frozen (which
	// freezes BOTH keys): this exists to prove clearComposer's no-movement
	// latch actually flips to the alternate shape rather than repeating a
	// key that just proved itself a no-op.
	frozenUnixLineDiscard map[string]bool
	// composerFloor models the shape #87 needs and neither of the above
	// two do: a composer that clears down to a certain number of lines and
	// then STOPS moving, as opposed to composerLines (always empties given
	// enough presses) or frozen (never moves even once). Only meaningful
	// alongside composerLines — see setComposerFloor.
	composerFloor map[string]int
	// keyRepaint models a DIALOG: a pane that redraws when a raw key lands on
	// it. That redraw is the only evidence Keys has that its key registered,
	// so a pane without this models the opposite case — a screen that
	// swallowed the key — and both need to be reachable from a test.
	keyRepaint map[string]bool
	// dialog models a numbered menu that COMMITS on a digit, as measured on
	// the runtime (#168): see fakeDialog. Only for panes a test armed; every
	// other pane keeps the send-keys behaviour below.
	dialog map[string]keyedScreen
	// renameNoop models a rename-session call that reports success without
	// actually moving anything — muster #97's "never reached the
	// runtime at all" hypothesis, as distinct from a real rename that later
	// gets reverted by mutating f.sessions directly (see
	// TestRenameSurvivesARuntimeRevert). Off by default: every rename this
	// fake receives moves the name, exactly as the real multiplexer does.
	renameNoop bool
	// capChunkArgWall models muster#141's real-world failure mode: the
	// multiplexer's own command channel refusing a chained invocation
	// outright once it carries more than this many args, producing NO
	// output at all (not a partial one) and a nonzero exit — "command too
	// long", not a per-target failure like a vanished pane. Zero (the
	// default) means unbounded, matching every other fakeMux behaviour: a
	// test only pays for this if it asks for it.
	capChunkArgWall int
	// heights is a pane's height, expanded into a batched capture's marker
	// the way the real multiplexer expands #{pane_height} (muster#169).
	// A pane absent here leaves the format literal, which the driver reads as
	// "height unknown" — so every test that never sets it keeps treating all
	// captured rows as visible, exactly as before.
	heights map[string]int
	// bracketPasteOff models the one pane shape terminal-path-v2's
	// pasteBracketed refuses on: an occupant that has NOT asked the
	// terminal for bracketed paste mode (`#{bracket_paste_flag}` reads 0).
	// Every pane absent here answers "1" — matching round-1's own live
	// measurement that every real Claude Code pane sampled reported
	// bracket_paste_flag=1 — so no existing fixture has to know this query
	// exists at all; only a test that specifically wants to exercise the
	// refusal sets a pane here.
	bracketPasteOff map[string]bool
	// echoOutsideFence restores this fake's original paste model: delivered
	// text is appended AFTER the whole pre-armed screen instead of being
	// rendered inside the composer's fence (renderInComposer). That is the
	// shape of text echoed by something other than the composer — a shell
	// under a leftover composer frame (#180 H2) — so a landed check must
	// never be confirmed by it.
	echoOutsideFence bool
	// pasteLog is every payload delivered through the paste buffer, in
	// order, whatever became of it on screen afterwards.
	pasteLog []string
	// collapsePastes models the runtime collapsing a paste of five or more
	// lines, or over 800 bytes, to one "[Pasted text #N +L lines]" marker
	// (bare "[Pasted text #N]" for a single long line) inside the composer.
	// N counts per pane; L is the paste's newline count, as measured.
	collapsePastes bool
	pasteSeq       map[string]int
	// currentCommand is a pane's #{pane_current_command} (#180 H2): the
	// name of its foreground process. Absent means the runtime.
	currentCommand map[string]string
}

// renderInComposer models what the runtime does with a paste: the text
// appears INSIDE the composer, between its fences, one row per logical line
// (#180). A screen with no fenced composer is returned unchanged (ok=false),
// and the caller falls back to appending — text a pane shows outside any
// composer. Existing composer text (other than the dim placeholder) is kept
// and the paste continues it, as a paste at the end of the line does.
func renderInComposer(capture, pasted string) (string, bool) {
	lines := strings.Split(capture, "\n")
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if isRule(stripEscapes(lines[i])) {
			last = i
			break
		}
	}
	if last <= 0 {
		return "", false
	}
	prompt := -1
	for i := last - 1; i >= 0; i-- {
		plain := strings.TrimSpace(stripEscapes(lines[i]))
		if isRule(plain) {
			return "", false
		}
		if strings.HasPrefix(plain, composerRuneMarker) {
			prompt = i
			break
		}
	}
	if prompt < 0 {
		return "", false
	}
	var existing []string
	if !allDim(afterMarker(lines[prompt])) {
		if first := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stripEscapes(lines[prompt])), composerRuneMarker)); first != "" {
			existing = append(existing, first)
		}
		for i := prompt + 1; i < last; i++ {
			if row := stripEscapes(lines[i]); strings.TrimSpace(row) != "" {
				existing = append(existing, row)
			}
		}
	}
	rows := strings.Split(pasted, "\n")
	if n := len(existing); n > 0 {
		existing[n-1] += rows[0]
		rows = append(existing, rows[1:]...)
	}
	out := append([]string{}, lines[:prompt]...)
	out = append(out, composerRuneMarker+" "+rows[0])
	out = append(out, rows[1:]...)
	out = append(out, lines[last:]...)
	return strings.Join(out, "\n"), true
}

// setBracketPasteOff arms the one pane shape terminal-path-v2's
// pasteBracketed refuses delivery on — see bracketPasteOff's own doc
// comment.
func (f *fakeMux) setBracketPasteOff(paneID string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bracketPasteOff == nil {
		f.bracketPasteOff = map[string]bool{}
	}
	f.bracketPasteOff[paneID] = v
}

func (f *fakeMux) setCapChunkArgWall(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capChunkArgWall = n
}

func (f *fakeMux) setRenameNoop(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renameNoop = v
}

// composerHolding renders a frame whose composer holds text — the shape of a
// pane whose submit did not register.
func composerHolding(text string) string {
	return "  transcript line\n✻ Brewed for 1m 0s\n" + rule + "\n❯ " +
		strings.TrimSpace(text) + "\n" + rule + "\n  ⏵⏵ auto mode on"
}

// composerHoldingRows is composerHolding's row-preserving sibling: lines are
// rendered one per screen row exactly as given, INCLUDING a blank ("") row
// at either end. composerHolding's own strings.TrimSpace(text) would erase
// exactly that shape — a payload ending in one or more real trailing
// newlines (muster#132) — so it cannot be used to build this fixture.
func composerHoldingRows(lines []string) string {
	var b strings.Builder
	b.WriteString("  transcript line\n✻ Brewed for 1m 0s\n")
	b.WriteString(rule)
	b.WriteString("\n❯ ")
	for i, l := range lines {
		if i == 0 {
			b.WriteString(l)
			continue
		}
		b.WriteString("\n")
		b.WriteString(l)
	}
	b.WriteString("\n")
	b.WriteString(rule)
	b.WriteString("\n  ⏵⏵ auto mode on")
	return b.String()
}

func (f *fakeMux) setCapture(paneID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures[paneID] = text
}

// setMultilineComposer puts a composer on the pane holding several logical
// lines, and arms the line-by-line C-u model for it (see composerLines).
// lines[0] is what would sit after the ❯ marker; the rest are continuation
// lines a long unsent message wraps onto below it.
func (f *fakeMux) setMultilineComposer(paneID string, lines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.composerLines == nil {
		f.composerLines = map[string][]string{}
	}
	cp := append([]string(nil), lines...)
	f.composerLines[paneID] = cp
	f.captures[paneID] = composerHoldingRows(cp)
}

// freezeComposer makes C-u a complete no-op against paneID — modelling a
// clear keystroke that never reaches the terminal at all, as opposed to
// setMultilineComposer's partial-clear model of one that reaches it and
// only gets partway.
func (f *fakeMux) freezeComposer(paneID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen == nil {
		f.frozen = map[string]bool{}
	}
	f.frozen[paneID] = true
}

// unfreezeComposer reverses freezeComposer — muster#136's own tests
// need a pane that resists the ordinary pass (to get a genuine futility
// record) and THEN responds to the sweep, which freezeComposer alone
// cannot model (it freezes every key, forever).
func (f *fakeMux) unfreezeComposer(paneID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.frozen, paneID)
}

// freezeUnixLineDiscard makes C-u (whether or not composerLineEndKey
// preceded it) a no-op against paneID, while leaving Backspace fully
// effective — muster#138's own shape, where the positioning fix does
// not help on some substrate and only the alternate key shape converges.
func (f *fakeMux) freezeUnixLineDiscard(paneID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozenUnixLineDiscard == nil {
		f.frozenUnixLineDiscard = map[string]bool{}
	}
	f.frozenUnixLineDiscard[paneID] = true
}

// setComposerFloor arms the "moved, then stalled" shape for paneID: C-u
// keeps removing lines (setMultilineComposer's usual model) until only
// floor lines remain, and then becomes a no-op — real progress that
// genuinely stops, rather than composerLines' unconditional convergence to
// empty or freezeComposer's total non-response (#87).
func (f *fakeMux) setComposerFloor(paneID string, floor int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.composerFloor == nil {
		f.composerFloor = map[string]int{}
	}
	f.composerFloor[paneID] = floor
}

func (f *fakeMux) addSession(s fakeSession, capture string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, s)
	f.captures[s.paneID] = capture
}

// paneExists reports whether paneID is still a live session's pane. Callers
// must already hold f.mu — this has no lock of its own because its only
// caller, exec's "display-message" case, is already inside one, and
// sync.Mutex is not reentrant (see the rename-session case's own note on
// the same constraint).
func (f *fakeMux) paneExists(paneID string) bool {
	for _, s := range f.sessions {
		if s.paneID == paneID {
			return true
		}
	}
	return false
}

func (f *fakeMux) dropLastSession() {
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.sessions[len(f.sessions)-1]
	f.sessions = f.sessions[:len(f.sessions)-1]
	delete(f.captures, last.paneID)
}

// dropSession removes ONE named session, wherever it sits in the slice —
// dropLastSession only ever removes whichever fixture happened to be added
// last, which is `beta` in twoSessions(), not the `alpha💬` most settle tests
// target. Used to simulate a session vanishing out from under an in-flight
// settleNewSession poll (muster #125).
func (f *fakeMux) dropSession(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.sessions {
		if s.name != name {
			continue
		}
		f.sessions = append(f.sessions[:i], f.sessions[i+1:]...)
		delete(f.captures, s.paneID)
		return
	}
}

func (f *fakeMux) setFailList(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failList = v
}

func (f *fakeMux) callsSnapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

type fakeSession struct {
	name    string
	paneID  string
	cwd     string
	pid     int
	created int64
	dead    bool
	// deadStatus is pane_dead_status (muster #235). Meaningful only
	// alongside dead: true.
	deadStatus int
	title      string
	// managed models the marker set-option Create leaves on every session
	// this driver starts (muster #236, managedSessionOption). Defaults
	// to false, matching a real session nobody ever set the option on.
	managed bool
	// launchRC models launchRemoteControlOption (muster #269): "1", "0" or ""
	// (never recorded). Emitted as the listing's tenth field.
	launchRC string
}

const testNonce = "0badc0de"

// testCaller is what a service would hand a local driver. Credential is
// empty on purpose: a local driver has no peer to present it to, and a test
// that supplied one would be asserting a behaviour this driver must not have.
var testCaller = fleet.Request{Caller: fleet.Caller{Principal: "test:unit"}}

func (f *fakeMux) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if f.buffers == nil {
		f.buffers = map[string]string{}
	}
	if f.pasted == nil {
		f.pasted = map[string]string{}
	}
	switch args[0] {
	case "load-buffer":
		// One invocation carries both halves, separated by a literal ";":
		//   load-buffer -b <name> <file> ; paste-buffer -b <name> -t <pane> -d
		var content, pane string
		for i, a := range args {
			switch a {
			case "load-buffer":
				if i+3 < len(args) {
					if raw, err := os.ReadFile(args[i+3]); err == nil {
						content = string(raw)
					}
				}
			case "paste-buffer":
				for j := i; j < len(args)-1; j++ {
					if args[j] == "-t" {
						pane = args[j+1]
					}
				}
			}
		}
		if pane != "" {
			f.pasteLog = append(f.pasteLog, content)
		}
		// A dialog that takes a paste says so itself (muster#206): the
		// runtime's free-text field receives a bracketed paste as typed text,
		// which is not what a composer does with one.
		if g, ok := f.dialog[pane].(interface{ paste(text string) }); ok && pane != "" {
			g.paste(content)
			f.captures[pane] = f.dialog[pane].screen()
			return nil, nil
		}
		if pane != "" && !f.noEcho {
			if f.collapsePastes && (strings.Count(content, "\n") >= 4 || len(content) > 800) {
				if f.pasteSeq == nil {
					f.pasteSeq = map[string]int{}
				}
				f.pasteSeq[pane]++
				if n := strings.Count(content, "\n"); n > 0 {
					content = fmt.Sprintf("[Pasted text #%d +%d lines]", f.pasteSeq[pane], n)
				} else {
					content = fmt.Sprintf("[Pasted text #%d]", f.pasteSeq[pane])
				}
			}
			f.pasted[pane] += content
		}
		return nil, nil
	case "capture-pane":
		// Single-pane capture, as used to confirm delivery.
		//
		// THE -e FLAG IS MODELLED, and it has to be. Without -e the real
		// multiplexer strips attributes, and the classifier is documented to
		// need them: the composer's placeholder is distinguished from typed
		// text by DIMNESS ALONE. A fake that returned the same bytes either way
		// made a missing -e invisible to every test, which is exactly how three
		// capture sites shipped without it.
		withEscapes := false
		for _, a := range args {
			if a == "-e" {
				withEscapes = true
			}
		}
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-t" {
				body := f.captures[args[i+1]] + "\n" + f.pasted[args[i+1]]
				if p := f.pasted[args[i+1]]; p != "" && !f.echoOutsideFence {
					if rendered, ok := renderInComposer(f.captures[args[i+1]], p); ok {
						body = rendered
					}
				}
				if !withEscapes {
					body = stripEscapes(body)
				}
				return []byte(body), nil
			}
		}
		return nil, nil
	case "list-panes":
		if f.failList {
			return nil, errors.New("multiplexer server not running")
		}
		sep := testNonce + "F"
		var b strings.Builder
		for _, s := range f.sessions {
			dead := "0"
			if s.dead {
				dead = "1"
			}
			managed := ""
			if s.managed {
				managed = "1"
			}
			b.WriteString(strings.Join([]string{
				s.name, s.paneID, s.cwd, itoa(s.pid), itoa64(s.created), dead,
				itoa(s.deadStatus), s.title, managed, s.launchRC,
			}, sep))
			b.WriteString("\n")
		}
		return []byte(b.String()), nil
	case "list-sessions":
		// Names only, as the naming resolver asks for them. Modelled rather
		// than left to the default no-op because "no sessions exist" is the
		// answer that makes every collision test pass without colliding.
		var b strings.Builder
		for _, s := range f.sessions {
			b.WriteString(s.name)
			b.WriteString("\n")
		}
		return []byte(b.String()), nil
	case "new-session":
		if f.failCreate {
			return nil, errors.New("cannot create session")
		}
		return nil, nil
	case "rename-session":
		// -t "=OLD" NEW. The "=" pin is stripped the way the real
		// multiplexer does, so a test can assert it was sent.
		//
		// No locking here: exec() already holds f.mu, and sync.Mutex is not
		// reentrant — taking it again deadlocks the whole suite, which is
		// exactly what it did.
		var from, to string
		for i := 0; i < len(args); i++ {
			if args[i] == "-t" && i+1 < len(args) {
				from = strings.TrimPrefix(args[i+1], "=")
				if i+2 < len(args) {
					to = args[i+2]
				}
			}
		}
		if f.renameNoop {
			// Reports success and moves nothing — see renameNoop's own doc
			// comment on the field.
			return nil, nil
		}
		for i := range f.sessions {
			if f.sessions[i].name == from {
				f.sessions[i].name = to
				return nil, nil
			}
		}
		return nil, errors.New("can't find session")
	case "send-keys":
		if g := f.dialog[sendKeysPane(args)]; g != nil {
			pane := sendKeysPane(args)
			if b, ok := g.(interface{ pressBurst(keys []string) }); ok && len(sentKeys(args)) > 1 {
				// A model that has measured what the runtime does with several
				// keys in ONE send-keys says so itself (muster#204).
				b.pressBurst(sentKeys(args))
			} else {
				for _, k := range sentKeys(args) {
					g.press(k)
				}
			}
			f.captures[pane] = g.screen()
			return nil, nil
		}
		// Model the two keys clearComposer's press loop can send: C-u
		// (unix-line-discard) for an ordinary row, and — muster#132 —
		// Backspace for a row that is already blank, which C-u cannot
		// touch at all (it kills to the start of the CURRENT line and
		// stops there; a blank line has nothing to kill and never crosses
		// the boundary above it). Anything else stays a no-op, as before.
		//
		// Modelling it here rather than in the test matters: clearing the pane
		// before calling Discard would have it see an empty composer and
		// return early, so the test would pass while proving nothing about the
		// keystroke.
		var pane string
		unixLineDiscard := false
		backspace := false
		newline := false
		for i := 0; i < len(args); i++ {
			if args[i] == "-t" && i+1 < len(args) {
				pane = args[i+1]
			}
			if args[i] == "C-u" {
				unixLineDiscard = true
			}
			if args[i] == "BSpace" {
				backspace = true
			}
			if args[i] == "C-m" || args[i] == "Enter" {
				newline = true
			}
		}
		if (unixLineDiscard || backspace) && pane != "" {
			if f.frozen[pane] {
				// Models a keystroke that goes nowhere: composer
				// untouched, whichever of the two keys arrived.
			} else if lines, armed := f.composerLines[pane]; armed && len(lines) > 0 {
				switch {
				case func() bool { floor, hasFloor := f.composerFloor[pane]; return hasFloor && len(lines) <= floor }():
					// #87's "moved, then stalled" shape: real progress was
					// made getting here, and now this press — like every
					// press after it — changes nothing. Unlike frozen,
					// which never moved at all.
				case lines[len(lines)-1] == "":
					// The bottom row is blank — the #132 shape a trailing
					// (or interior) real newline leaves behind. C-u has
					// nothing to kill on an empty line and is a no-op;
					// only Backspace, crossing the line boundary above it,
					// removes the row.
					if backspace {
						lines = lines[:len(lines)-1]
						f.composerLines[pane] = lines
						if len(lines) == 0 {
							delete(f.composerLines, pane)
							delete(f.pasted, pane)
							f.captures[pane] = idleFixtureFor("cleared")
						} else {
							f.captures[pane] = composerHoldingRows(lines)
						}
					}
					// unix-line-discard against a blank row: no-op,
					// matching real unix-line-discard.
				case unixLineDiscard && !f.frozenUnixLineDiscard[pane]:
					// unix-line-discard kills the line the cursor sits on, not the
					// whole buffer: one press drops the LAST row, leaving
					// every row above it exactly as it was. A composer with one
					// row therefore still clears in a single press — same as the
					// plain branch below — and only a composer with several needs
					// this pressed more than once to go empty.
					lines = lines[:len(lines)-1]
					f.composerLines[pane] = lines
					if len(lines) == 0 {
						delete(f.composerLines, pane)
						delete(f.pasted, pane)
						f.captures[pane] = idleFixtureFor("cleared")
					} else {
						f.captures[pane] = composerHoldingRows(lines)
					}
				case backspace:
					// muster#138: Backspace against a NON-blank row,
					// reached only via clearComposer's no-movement latch —
					// the alternate shape it falls back to once End+C-u has
					// already proven itself a no-op against this pane
					// (freezeUnixLineDiscard). This fake has no character-
					// level cursor model, so Backspace is treated as
					// equally capable of clearing the row it lands on as
					// unix-line-discard is — enough to prove the pass
					// converges once the latch has actually flipped shape,
					// without claiming anything about which real keystroke
					// a live terminal would need.
					lines = lines[:len(lines)-1]
					f.composerLines[pane] = lines
					if len(lines) == 0 {
						delete(f.composerLines, pane)
						delete(f.pasted, pane)
						f.captures[pane] = idleFixtureFor("cleared")
					} else {
						f.captures[pane] = composerHoldingRows(lines)
					}
				default:
					// unix-line-discard was sent but frozenUnixLineDiscard
					// made it a no-op for this pane, and no Backspace
					// accompanied it this press: models "the positioning
					// key did not help; C-u alone still cannot touch this
					// row" — muster#138's own worst case.
				}
			} else {
				delete(f.pasted, pane)
				f.captures[pane] = idleFixtureFor("cleared")
			}
		}
		// A dialog redrawing under a raw key. Only for panes a test armed, so
		// every existing test keeps the old no-op behaviour.
		if pane != "" && f.keyRepaint[pane] {
			for _, a := range args {
				switch a {
				case "Up", "Down", "Left", "Right", "Escape", "C-m", "BTab":
					// BTab repaints too: on an idle composer it rewrites the
					// mode indicator in the footer (#188).
					f.captures[pane] += "\n  selection moved by " + a
				}
			}
		}
		// Model what a SUBMIT does, which the fake previously left implicit.
		// It mattered once the driver started confirming that the submit
		// registered: with no model, the composer read empty either way and
		// the confirmation could not fail, so a test asserting on it would
		// have passed without exercising anything.
		if newline && pane != "" {
			if f.swallowSubmit {
				// The failure this models: the keystroke goes nowhere and the
				// delivered text is left sitting in the composer.
				//
				// Only overwrite f.captures when THIS call also pasted
				// something (f.pasted non-empty): that is the ordinary
				// first-attempt shape, where the paste and the submit happen
				// in the same Send call and f.pasted is where the delivered
				// text actually lives. A resume's swallowed keystroke pastes
				// nothing new — the text it is trying to submit already sits
				// in f.captures from an earlier call or from a test's own
				// setCapture — and a swallowed key changes NOTHING on
				// screen, by definition. Overwriting captures with
				// composerHolding("") here would show an EMPTY composer
				// despite the "swallow" the test asked for, which is exactly
				// backwards.
				if f.pasted[pane] != "" {
					f.captures[pane] = composerHolding(f.pasted[pane])
					// The text now lives in the screen itself; leaving it in
					// f.pasted too would render it a second time inside the
					// composer (renderInComposer).
					if !f.echoOutsideFence {
						delete(f.pasted, pane)
					}
				}
			} else {
				delete(f.pasted, pane)
				// #101: a genuinely successful submit empties the composer,
				// full stop — real tmux does not leave the PREVIOUS screen's
				// fenced text sitting there just because a test fixture wrote
				// it directly into f.captures instead of routing it through
				// f.pasted. Without this, a test that models a stranded
				// composer via setCapture/composerHolding (rather than a
				// paste that populates f.pasted) can never observe a
				// confirmed resume clearing it: the deleted f.pasted entry
				// was already empty, and f.captures was never touched, so
				// the fake would go on reporting the composer as full forever
				// regardless of whether the driver's confirmation logic is
				// even exercised. Symmetric with the C-u clear branch above,
				// which already resets to idleFixtureFor("cleared") on its
				// own success.
				f.captures[pane] = idleFixtureFor("cleared")
			}
		}
		return nil, nil
	case "display-message":
		// #180 H2: foregroundIsRuntime's own query. A live runtime pane
		// reports the runtime's process title (its version string), so
		// that is the default; a test arms currentCommand to model a
		// shell. The tty is deliberately not a real device name, so an
		// un-faked ps finds nothing there.
		if len(args) == 5 && args[4] == paneForegroundFormat {
			pane := args[3]
			cmd := "2.1.281"
			if c, ok := f.currentCommand[pane]; ok {
				cmd = c
			}
			pid := 0
			for _, s := range f.sessions {
				if s.paneID == pane {
					pid = s.pid
				}
			}
			return []byte(itoa(pid) + "|/dev/fake-tty" + strings.TrimPrefix(pane, "%") + "|" + cmd + "\n"), nil
		}
		// Terminal path v2: pasteBracketed's own standalone probe
		// (`display-message -p -t <pane> "#{bracket_paste_flag}"`) — never
		// chained with anything else, unlike every other display-message
		// shape this fake models below. See bracketPasteOff's own doc
		// comment for why the default answer is "1" rather than "0".
		if len(args) == 5 && args[4] == "#{bracket_paste_flag}" {
			pane := args[3]
			if f.bracketPasteOff[pane] {
				return []byte("0\n"), nil
			}
			return []byte("1\n"), nil
		}
		// The batched capture call: one chained invocation of
		// "display-message -p <mark> ; capture-pane ... -t <pane> ; ..." per
		// row, exactly as enumerate builds it (tmux.go's capArgs).
		//
		// This used to answer by walking f.sessions directly — one marker
		// per CURRENT session, in CURRENT order — which quietly assumed
		// nothing changes between the listing call that built this command
		// line and this one running it. #29 is exactly that assumption
		// failing: a pane can vanish in the gap. So this parses the actual
		// argv instead, the way the real multiplexer would receive it, and
		// answers per chained sub-command: a capture-pane whose target no
		// longer exists in f.sessions fails — no marker or body for that
		// pane, and the overall exit is nonzero — while every other pane in
		// the same invocation still gets read, matching capture-pane's
		// real per-target failure and the multiplexer's documented "run
		// every chained command, report failure if any of them failed"
		// behaviour.
		//
		// capChunkArgWall models the OTHER real failure mode (#141): past a
		// certain size, the multiplexer refuses the whole chained invocation
		// before running any of it — total failure, empty output, distinct
		// from the per-target "pane vanished" case below which still
		// returns everything that DID capture.
		if f.capChunkArgWall > 0 && len(args) > f.capChunkArgWall {
			return nil, errors.New("command too long")
		}
		var groups [][]string
		var cur []string
		for _, a := range args {
			if a == ";" {
				groups = append(groups, cur)
				cur = nil
				continue
			}
			cur = append(cur, a)
		}
		groups = append(groups, cur)

		var b strings.Builder
		anyVanished := false
		pendingMark := ""
		for _, g := range groups {
			if len(g) == 0 {
				continue
			}
			switch g[0] {
			case "display-message":
				target := ""
				for i, a := range g {
					if a == "-t" && i+1 < len(g) {
						target = g[i+1]
					}
				}
				for i, a := range g {
					if a == "-p" && i+1 < len(g) {
						pendingMark = g[i+1]
						if h, ok := f.heights[target]; ok {
							pendingMark = strings.ReplaceAll(pendingMark, paneHeightFormat, itoa(h))
						}
					}
				}
			case "capture-pane":
				var pane string
				withEscapes := false
				for i, a := range g {
					if a == "-t" && i+1 < len(g) {
						pane = g[i+1]
					}
					if a == "-e" {
						withEscapes = true
					}
				}
				if !f.paneExists(pane) {
					// The pane this sub-command targets is gone: real
					// capture-pane exits 1 for it and prints nothing, so
					// this block contributes neither marker nor body — and
					// the mark buffered above is discarded with it, the
					// same way the real marker is never printed if the
					// capture-pane after it never ran.
					anyVanished = true
					pendingMark = ""
					continue
				}
				if pendingMark != "" {
					b.WriteString(pendingMark + "\n")
				}
				body := f.captures[pane] + "\n" + f.pasted[pane]
				if !withEscapes {
					body = stripEscapes(body)
				}
				b.WriteString(body)
				// One row terminator after the last row, no more: real capture-pane
				// ends its output on the last row's newline and the next marker
				// follows it directly. An unconditional extra newline here was a
				// blank row no pane has, which the screen counts since #216 (a
				// prompt row on the pane's last row is only cut off by the pane's
				// edge when nothing is drawn below it). Measured against real
				// tmux: the batched capture and a direct one end the same way.
				if !strings.HasSuffix(body, "\n") {
					b.WriteString("\n")
				}
				pendingMark = ""
			}
		}
		if anyVanished {
			return []byte(b.String()), errors.New("exit status 1")
		}
		return []byte(b.String()), nil
	default:
		return nil, nil
	}
}

func itoa(i int) string     { return strings.TrimSpace(strings.Join([]string{intToStr(i)}, "")) }
func itoa64(i int64) string { return intToStr(int(i)) }
func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// newTestDriver injects no control-mode dialer, so anything that dials —
// Subscribe above all — attaches REAL multiplexer clients to whatever server
// the test machine has. Subscription tests use newSubDriver instead (#162).
// pendingOf is the composer text a fixture shows, as composerText reads it.
func pendingOf(t *testing.T, fixture string) string {
	t.Helper()
	p, scan := composerText(newScreen(fixture))
	if scan != composerFound || p == "" {
		t.Fatalf("setup: fixture holds no composer text")
	}
	return p
}

func newTestDriver(f *fakeMux) *Driver {
	return New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	)
}

func idleFixtureFor(label string) string {
	return "  transcript line for " + label + "\n✻ Brewed for 1m 0s\n" + rule + "\n❯\n" + rule + "\n  ⏵⏵ auto mode on"
}

// clippedComposerFixture models exactly what a `-S -N` capture returns for a
// composer taller than the capture window (muster#134): only the TAIL of
// the composer is in view, with no opening rule and no ❯-marked prompt row
// anywhere in the captured lines — both scrolled out above the window, same
// shape as classify_test.go's TestComposerSpanMissesAComposerTallerThanTheCaptureWindow.
func clippedComposerFixture() string {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("row %d of a paste taller than the capture window", i))
	}
	tail := lines[len(lines)-defaultCaptureLines:]
	return strings.Join(tail, "\n") + "\n" + rule
}

func twoSessions() *fakeMux {
	return &fakeMux{
		sessions: []fakeSession{
			{name: "alpha💬", paneID: "%1", cwd: "/work/alpha", pid: 100, created: 1785600000, title: "2_1_220"},
			{name: "beta", paneID: "%2", cwd: "/work/beta", pid: 200, created: 1785600001, title: "2_1_220"},
		},
		captures: map[string]string{
			"%1": idleFixtureFor("alpha"),
			"%2": fixtureUnsent,
		},
	}
}

// The headline cost property: enumerating N sessions costs a constant
// number of subprocess spawns, not one per session.
func TestListCostsConstantSpawns(t *testing.T) {
	f := twoSessions()
	// Grow to a size where a per-session implementation would be obvious.
	for i := 0; i < 40; i++ {
		id := "%" + intToStr(100+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "s" + intToStr(i), paneID: id, cwd: "/w", pid: 1000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("s" + intToStr(i))
	}
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsSnapshot()) != 2 {
		t.Errorf("List made %d subprocess calls for %d sessions; must be constant (2), "+
			"not proportional — see driver.Driver.List's contract",
			len(f.callsSnapshot()), len(f.sessions))
	}
}

// TestListSurvivesAFleetBiggerThanOneInvocationCanCarry is muster#141's
// own regression test. Before the chunking fix, a fleet large enough to
// cross the multiplexer's own command-length wall came back with EVERY
// session misclassified as a driver malfunction — not just the ones past
// the wall, because the one chained invocation carrying the whole fleet
// failed as a unit and produced no output at all. This reproduces that wall
// with a mock (capChunkArgWall) rather than a live multiplexer — the real
// wall itself is confirmed separately, against an actual server, by
// TestLiveEnumerationSurvivesTheCommandLengthWall in integration_test.go —
// and asserts the driver never lets a single oversized invocation take the
// whole fleet down with it.
func TestListSurvivesAFleetBiggerThanOneInvocationCanCarry(t *testing.T) {
	f := twoSessions()
	const n = 200 // 200 sessions ≈ 2399 args unchunked — nowhere near fitting
	// under any wall a real single invocation could survive.
	for i := 0; i < n; i++ {
		id := "%" + intToStr(300+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "s" + intToStr(i), paneID: id, cwd: "/w", pid: 2000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("s" + intToStr(i))
	}
	// Set the fake's wall ABOVE captureChunkMaxArgs (so every chunk this
	// driver actually issues still succeeds) but far BELOW what an
	// unchunked whole-fleet invocation would need — the same relationship
	// the real multiplexer had to the reported 85-session fleet.
	f.setCapChunkArgWall(captureChunkMaxArgs + 50)

	d := newTestDriver(f)
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.Items()) != n+2 {
		t.Fatalf("want %d sessions, got %d", n+2, len(got.Items()))
	}

	var unknown []string
	for _, s := range got.Items() {
		if s.State.Status == fleet.StatusUnknown {
			unknown = append(unknown, s.ID)
		}
	}
	if len(unknown) != 0 {
		t.Errorf("%d/%d sessions came back %q past the command-length wall "+
			"(chunking should have kept every chunk under it): %v",
			len(unknown), len(got.Items()), fleet.StatusUnknown, unknown)
	}

	// The wall must actually have been exercised — otherwise this test
	// would pass vacuously even without the fix, because a single
	// under-wall invocation covering everything never triggers the failure
	// mode being guarded against.
	var captureCalls int
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && c[0] == "display-message" {
			captureCalls++
			if len(c) > captureChunkMaxArgs {
				t.Errorf("one capture invocation carried %d args, over captureChunkMaxArgs (%d) — "+
					"chunking is not actually bounding invocation size", len(c), captureChunkMaxArgs)
			}
		}
	}
	if captureCalls < 2 {
		t.Fatalf("expected chunking to issue at least 2 capture invocations for a fleet this "+
			"size, got %d — the wall this test exists to guard against was never exercised",
			captureCalls)
	}
}

func TestListCarriesExactlyOneSourceAndRealStatuses(t *testing.T) {
	d := newTestDriver(twoSessions())
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources()) != 1 {
		t.Fatalf("a local answer carries exactly one SourceStatus (§9), got %d", len(got.Sources()))
	}
	if got.Sources()[0].Status != fleet.SourceOK || !got.Complete() {
		t.Errorf("healthy read should be ok+complete, got %v complete=%v",
			got.Sources()[0].Status, got.Complete())
	}
	if len(got.Items()) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got.Items()))
	}
	byID := map[string]fleet.Session{}
	for _, s := range got.Items() {
		byID[s.ID] = s
	}
	if byID["alpha💬"].State.Status != fleet.StatusIdle {
		t.Errorf("alpha: want idle, got %q", byID["alpha💬"].State.Status)
	}
	if byID["beta"].State.Status != fleet.StatusWaitingInput {
		t.Errorf("beta (unsent composer): want waiting_input, got %q", byID["beta"].State.Status)
	}
	if byID["alpha💬"].Cwd != "/work/alpha" {
		t.Errorf("emoji session name broke field parsing: cwd = %q", byID["alpha💬"].Cwd)
	}
}

// #29: a pane vanishing in the gap between listing and capturing must not
// cost every OTHER session on the machine its read — and the source that
// answered must say it read only PART of the machine, not claim a clean ok
// (nor, since it plainly answered, unreachable).
func TestListToleratesPaneVanishingMidCapture(t *testing.T) {
	f := twoSessions()
	// Reproduces the exact race #29 describes: "beta" is still there when
	// list-panes runs — its row is in the listing — but is gone by the time
	// the batched capture-pane invocation reaches it, the same way a
	// short-lived session ends in the gap on a live fleet. dropLastSession
	// after the listing call removes it from f.sessions before
	// display-message answers, so its capture-pane sub-command has no
	// target left and the fake reports exactly what real capture-pane does
	// for a gone pane: that one sub-command fails, the invocation's overall
	// exit is nonzero, and everything captured for OTHER panes is still in
	// the output.
	wrapped := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.exec(ctx, name, args...)
		if len(args) > 0 && args[0] == "list-panes" {
			f.dropLastSession()
		}
		return out, err
	}
	d := New("testbox",
		withExec(wrapped),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	)

	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("a vanished pane must not fail the whole read: %v", err)
	}

	if len(got.Items()) != 2 {
		t.Fatalf("want both sessions reported — the vanished one as unknown, "+
			"not dropped — got %d: %+v", len(got.Items()), got.Items())
	}
	byID := map[string]fleet.Session{}
	for _, s := range got.Items() {
		byID[s.ID] = s
	}
	if byID["alpha💬"].State.Status != fleet.StatusIdle {
		t.Errorf("alpha's read must survive beta vanishing, got %q", byID["alpha💬"].State.Status)
	}
	if byID["beta"].State.Status != fleet.StatusUnknown {
		t.Errorf("the vanished pane's own session should read unknown, not silently drop, got %q",
			byID["beta"].State.Status)
	}

	if len(got.Sources()) != 1 {
		t.Fatalf("a local answer carries exactly one SourceStatus (§9), got %d", len(got.Sources()))
	}
	src := got.Sources()[0]
	if src.Status == fleet.SourceUnreachable {
		t.Error("this machine answered — 1 of 2 panes read — unreachable is the wrong word")
	}
	if src.Status != fleet.SourceDegraded {
		t.Errorf("a partial read must report degraded, got %q", src.Status)
	}
	if src.Error == "" {
		t.Error("a degraded source must carry why (§9)")
	}
	if got.Complete() {
		t.Error("a source that answered partially must not report itself complete")
	}
}

// §5.7: a failed read must never render as an empty result.
func TestListFailureIsNotAnEmptyList(t *testing.T) {
	f := twoSessions()
	f.failList = true
	d := newTestDriver(f)
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("a source failure belongs in the envelope, not in err: %v", err)
	}
	if got.Complete() {
		t.Error("a failed read must not report complete")
	}
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceUnreachable {
		t.Errorf("want one unreachable source, got %+v", got.Sources())
	}
	if got.Sources()[0].Error == "" {
		t.Error("an unreachable source must carry why (§9)")
	}
}

// §2.4's refusal, which the README listed as "prose that has never run".
func TestSendRefusesWhenComposerHoldsUnsentInput(t *testing.T) {
	d := newTestDriver(twoSessions())
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("a refusal is a domain outcome, not an error: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("want refused, got %q", got.Outcome)
	}
	if got.Reason == "" {
		t.Error("a refusal must explain itself (§2.4)")
	}
}

// muster#134: before this fix, a composer taller than the capture
// window read as composerAbsent, so this guard never fired — a delivery
// would proceed to concatenate onto text this driver could not see. This
// proves the fix fails closed instead: refused, and the payload never
// reaches the multiplexer.
func TestSendRefusesDeliveryIntoAClippedComposer(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = clippedComposerFixture()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("a refusal is a domain outcome, not an error: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("want refused for a clipped composer, got %q (%s)", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "capture window") {
		t.Errorf("reason should say why this driver could not confirm it is safe to send: %q", got.Reason)
	}
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, "hello") {
				t.Errorf("payload reached a command line despite the refusal: %v", c)
			}
		}
		if len(c) > 0 && (c[0] == "send-keys" || c[0] == "paste-buffer") {
			t.Errorf("nothing may be typed or pasted into a clipped composer; saw %v", c)
		}
	}
	assertClippedRemedy(t, got.Reason)
	if n := d.Counters()[counterComposerClippedRefusedSend]; n != 1 {
		t.Errorf("%s = %d, want 1", counterComposerClippedRefusedSend, n)
	}
}

func TestSendDeliversWhenComposerIsEmpty(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("this substrate cannot confirm receipt, so queued is the honest outcome; got %q", got.Outcome)
	}
	// The payload must not appear in any argv (§5.3's rationale).
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, "hello") {
				t.Errorf("payload reached a command line: %v", c)
			}
		}
	}
}

func TestSendRefusesDeadAndMissingSessions(t *testing.T) {
	f := twoSessions()
	f.sessions[0].dead = true
	d := newTestDriver(f)
	got, _ := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "x", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("dead session: want refused, got %q", got.Outcome)
	}
	got, _ = d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "nope"}, "x", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("missing session: want refused, got %q", got.Outcome)
	}
}

// §5.4: never act destructively on an id match alone.
func TestCloseRefusesAnUncorroboratedTarget(t *testing.T) {
	d := newTestDriver(twoSessions())
	_, err := d.Close(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("closing a never-observed id must refuse (§5.4); got %v", err)
	}
}

func TestCloseRefusesWhenTheIdWasRecycled(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	// Same name, different session: the exact hazard §5.4 describes.
	f.sessions[0].created = 1785699999
	_, err := d.Close(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("a recycled id must refuse, got %v", err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("a refused close must not have killed anything")
		}
	}
}

func TestCloseProceedsWhenCorroborated(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	ack, err := d.Close(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil || !ack.Accepted {
		t.Fatalf("corroborated close should proceed: ack=%+v err=%v", ack, err)
	}
	var killed bool
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			killed = true
		}
	}
	if !killed {
		t.Error("expected a kill-session invocation")
	}
}

// §10: a repeat key returns the existing ref rather than a second session.
func TestCreateIsIdempotentPerKey(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	spec := fleet.SessionSpec{Machine: "testbox", Cwd: "/work/new", Name: "gamma"}

	first, err := d.Create(context.Background(), testCaller, "key-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	// #234: the replay is now corroborated against what is actually running.
	f.addSession(fakeSession{name: "gamma", paneID: "%gamma", cwd: "/work/new",
		pid: 900, created: 1785760000, title: "2_1_220"}, idleFixtureFor("gamma"))
	before := countCalls(f, "new-session")
	second, err := d.Create(context.Background(), testCaller, "key-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	// DeepEqual, not !=: Pins/RuntimeSurface/PromptDelivery (muster
	// #84/#85/#86) are freshly allocated pointers on every call even for
	// identical content — struct-identity != spuriously fails on two
	// independently-built Sessions describing the same thing.
	if !reflect.DeepEqual(first, second) {
		t.Errorf("same key must return the same ref: %+v vs %+v", first, second)
	}
	if countCalls(f, "new-session") != before {
		t.Error("a repeat key must not start a second session (§10)")
	}
}

// #47, point 5: Create seeds spec.Cwd's owning root before starting the
// session, so a directory under a configured root never meets the runtime's
// folder-trust question in the first place — closing the race a
// periodic-only pass leaves open for a worktree younger than the interval.
func TestCreateSeedsCwdBeforeStartingWhenTrustSeedConfigured(t *testing.T) {
	f := twoSessions()

	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	statePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(statePath, []byte(`{"projects":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "workspace")
	cwd := filepath.Join(root, "widgets")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	d := New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithTrustSeed(statePath, home, []string{root}),
	)

	if _, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{
		Machine: "testbox", Cwd: fleet.AbsolutePath(cwd), Name: "gamma",
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	entry, ok := top.Projects[cwd]
	if !ok || !entry.HasTrustDialogAccepted {
		t.Errorf("Create did not seed cwd's trust key before starting the session; projects=%+v", top.Projects)
	}

	if got := d.Counters()[trustseed.CounterGranted]; got == 0 {
		t.Error("trust-seed counters were not merged into Driver.Counters()")
	}
}

// A Driver with trust-seed unconfigured must behave exactly as it always
// has — nil is the off switch, not a code path a caller has to avoid.
func TestCreateWithoutTrustSeedConfiguredIsUnaffected(t *testing.T) {
	d := newTestDriver(twoSessions())
	if _, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{
		Machine: "testbox", Cwd: "/work/new", Name: "gamma",
	}); err != nil {
		t.Fatal(err)
	}
	if counters := d.Counters(); counters[trustseed.CounterGranted] != 0 {
		t.Errorf("unconfigured trust-seed reported a count: %v", counters)
	}
}

func TestCreateRequiresAnIdempotencyKey(t *testing.T) {
	d := newTestDriver(twoSessions())
	_, err := d.Create(context.Background(), testCaller, "", fleet.SessionSpec{Cwd: "/w"})
	if err == nil {
		t.Error("§10 makes the key required, not optional")
	}
}

// §5.3: context and prompt travel by path/buffer, never in argv.
func TestCreateKeepsPromptAndContextOutOfArgv(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	secret := "a-prompt-that-must-not-be-greppable"
	_, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{
		Machine:    "testbox",
		Cwd:        "/work/new",
		Name:       "gamma",
		Prompt:     secret,
		ContextRef: "/tmp/ctx.txt",
		Agent:      "may",
		Model:      "opus",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Errorf("prompt reached a command line (§5.3): %v", c)
			}
		}
	}
	// The context path, by contrast, is exactly what SHOULD be in argv.
	var sawContextPath bool
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if a == "/tmp/ctx.txt" {
				sawContextPath = true
			}
		}
	}
	if !sawContextPath {
		t.Error("contextRef should be passed by path (§5.3)")
	}
}

func TestCreateRejectsRelativeContextRef(t *testing.T) {
	d := newTestDriver(twoSessions())
	_, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{
		Cwd: "/w", ContextRef: "relative/path.txt",
	})
	if err == nil {
		t.Error("contextRef is an AbsolutePath; a relative one must be rejected")
	}
}

// §4.3/§5.6: the capability declaration must not overstate the substrate.
func TestCapabilitiesAreHonest(t *testing.T) {
	d := newTestDriver(twoSessions())
	c := d.Capabilities()
	if c.ObservesState {
		t.Error("every status here is screen-inferred; ObservesState must be false (§5.6)")
	}
	if c.ConfirmsDelivery {
		t.Error("receipt is not observable on this substrate; ConfirmsDelivery must be false")
	}
	if !c.SupportsResume {
		t.Error("multiplexer sessions outlive this process; SupportsResume is true")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("§4.4 requires a positive deadline: %v", err)
	}
}

// §12: reconciliation adopts and never destroys.
func TestReconcileAdoptsAndDestroysNothing(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Live.Items()) != 2 {
		t.Errorf("reconciliation must surface everything found, got %d", len(got.Live.Items()))
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("§12 rule 4 is absolute: reconciliation destroys nothing")
		}
	}
}

// §5.7 for a singular read: "looked, not there" is an answer, not an error.
// Absence is an answer — that part never changed. What changed is that there
// are TWO absences, and they were being given the same one.
//
// A session this driver watched and can no longer find is dead. An id it has
// never seen is not dead: claiming so invents a history, and tells a caller
// who mistyped an id that its work has died.
func TestStateSeparatesGoneFromNeverHere(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	// Never seen: no history to report, so this is not an answer about a
	// session at all.
	_, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "ghost"})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Fatalf("an id never observed must not be reported as dead; got err=%v", err)
	}

	// Seen, then gone: this one really is dead, and absence is the answer.
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	f.dropLastSession()
	got, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"})
	if err != nil {
		t.Fatalf("a session that was here and is gone is an answer, not an error: %v", err)
	}
	if got.Status != fleet.StatusDead {
		t.Errorf("want dead, got %q", got.Status)
	}
	if got.Confidence != fleet.ConfidenceInferred {
		t.Errorf("want inferred, got %q", got.Confidence)
	}
	// Name what ended. An id alone is recyclable (§5.4), so a caller
	// reconciling its own records needs more than the id it asked with.
	if !strings.Contains(got.Evidence, "/work/beta") {
		t.Errorf("evidence should say which session ended, got %q", got.Evidence)
	}
}

func countCalls(f *fakeMux, verb string) int {
	n := 0
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}

// §5.4's real guarantee: corroborate against what the CALLER observed.
//
// alpha was created at 1785600000 in the fixture.
func expectStarted(unix int64) fleet.Request {
	ts := time.Unix(unix, 0)
	return fleet.Request{
		Caller: fleet.Caller{Principal: "test:unit"},
		Expect: fleet.Expectation{StartedAt: &ts},
	}
}

func TestCloseWithMatchingExpectationNeedsNoPriorSighting(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	// Deliberately no List first: the caller's own observation is enough,
	// which is the point — the driver's sightings are not the authority.
	ack, err := d.Close(context.Background(), expectStarted(1785600000),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil || !ack.Accepted {
		t.Fatalf("a caller quoting the right start time should be able to close: ack=%+v err=%v", ack, err)
	}
	if countCalls(f, "kill-session") != 1 {
		t.Error("expected exactly one kill-session")
	}
}

// The test the whole envelope exists for. The driver's OWN sighting is
// current and would pass the weak check — but the caller is quoting a session
// that no longer exists at that id. Before Request.Expect this was
// unexpressible, and the destroy would have proceeded.
func TestStaleCallerExpectationRefusesEvenWhenTheDriversOwnSightingIsFresh(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	// Driver observes the session as it is now — weak check would pass.
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	// Caller, however, is talking about an earlier session at the same id.
	_, err := d.Close(context.Background(), expectStarted(1785500000),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("a stale caller expectation must refuse; got %v", err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("destroyed a session the caller did not mean — §5.4's exact failure")
		}
	}
}

// Omitting the expectation is allowed, but the caller must get the weaker
// guarantee explicitly rather than silently.
func TestCloseWithoutExpectationFallsBackAndSaysSo(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Close(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("no expectation and no prior sighting must refuse; got %v", err)
	}
	if !strings.Contains(err.Error(), "no expected start time") {
		t.Errorf("refusal should name which check it applied, got: %v", err)
	}
}

// A caller cannot quote a start time it was never given, so reads must carry it.
func TestListExposesStartedAtSoCallersCanCorroborate(t *testing.T) {
	d := newTestDriver(twoSessions())
	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.StartedAt == nil {
			t.Errorf("session %q has no StartedAt; the caller has nothing to quote back "+
				"and §5.4's strong check is unreachable", s.ID)
		}
	}
}

// The submit race, from a sibling project's measurements: pasting and
// submitting back-to-back lets the submit win, the prompt is submitted EMPTY,
// and the text lands afterwards where it sits unsent forever. Counted there at
// eight stranded operator instructions in one day.
func TestSubmitOnlyAfterTheTextIsVisible(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "do the thing", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %q, want queued", got.Outcome)
	}
	// The capture must happen between the paste and the submit.
	var pasteAt, captureAt, submitAt = -1, -1, -1
	for i, c := range f.callsSnapshot() {
		switch c[0] {
		case "load-buffer":
			pasteAt = i
		case "capture-pane":
			if captureAt < 0 && pasteAt >= 0 {
				captureAt = i
			}
		case "send-keys":
			submitAt = i
		}
	}
	if pasteAt < 0 || captureAt < 0 || submitAt < 0 {
		t.Fatalf("expected paste, capture and submit; got %d/%d/%d", pasteAt, captureAt, submitAt)
	}
	if !(pasteAt < captureAt && captureAt < submitAt) {
		t.Errorf("order was paste=%d capture=%d submit=%d; the confirmation must sit "+
			"between them or the submit can win the race", pasteAt, captureAt, submitAt)
	}
}

// `Enter` was observed being silently dropped on a pane where `C-m` submitted
// immediately — same text, seconds apart. Only one of them has been seen to
// work when the other did not.
func TestSubmitUsesControlM(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "x", driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			last := c[len(c)-1]
			if last == "Enter" {
				t.Error("submitted with Enter; C-m is the form measured to work where Enter did not")
			}
			if last != "C-m" {
				t.Errorf("submit key = %q, want C-m", last)
			}
		}
	}
}

// sentKeys returns the key names from a `send-keys` invocation — everything
// after the `-t <pane>` target. It is deliberately dumb: the assertion below is
// about the ORDER of the keys on the wire, and anything smarter would be
// re-implementing the driver instead of checking it.
func sentKeys(argv []string) []string {
	for i := 0; i < len(argv); i++ {
		if argv[i] == "-t" {
			return argv[i+2:]
		}
	}
	return argv[1:]
}

// A "printable key" is one that puts a character on screen — the property the
// measurement turns on. Named control keys (C-m, Enter, Escape, BSpace) are
// not printable; `Space` and the digits are.
func printableKey(k string) bool {
	if k == "Space" {
		return true
	}
	r := []rune(k)
	return len(r) == 1 && unicode.IsPrint(r[0])
}

func isNewlineKey(k string) bool { return k == "C-m" || k == "Enter" }

// The FIRST keystroke into an idle pane is swallowed when that keystroke is
// Enter — measured 6 times out of 6 on real sessions, where the same pane
// accepted a printable key and then submitted on the very next newline. A paste
// is not a keystroke, so after paste-buffer the submit is always the first
// keystroke: a lone newline strands the delivered text in the composer while
// the receipt reports success.
//
// So every submit must carry a printable wake key IMMEDIATELY before its
// newline, in the same invocation — a second call would race, and a gap would
// put the newline back in the first-keystroke slot.
//
// The trailing space is deliberate. Tidying it away with a `BSpace` before the
// newline would restore a non-printable key as the first post-idle keystroke,
// which is the untested case; this test fails that shape too, because BSpace is
// not printable.
func TestEverySubmitCarriesAPrintableWakeKeyBeforeTheNewline(t *testing.T) {
	prompted := func() *fakeMux {
		f := twoSessions()
		f.captures["%1"] = fixtureTrustPrompt
		return f
	}
	cases := []struct {
		name string
		mux  *fakeMux
		act  func(*Driver) error
	}{
		{
			// The post-paste submit: the site the defect was measured on.
			name: "send",
			mux:  twoSessions(),
			act: func(d *Driver) error {
				_, err := d.Send(context.Background(), testCaller,
					fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "do the thing",
					driver.SendOptions{Submit: true})
				return err
			},
		},
		{
			// "Accept the highlighted option" — the other path that used to
			// send a bare newline into a pane that has been idle by definition,
			// since it has been sitting on a question.
			name: "respond/accept-highlighted",
			mux:  prompted(),
			act: func(d *Driver) error {
				_, err := d.Respond(context.Background(), testCaller,
					fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.Response{})
				return err
			},
		},
		// respond by choice used to be a case here too, as a digit+C-m pair.
		// Since #168 it sends the digit alone and no newline at all, so it is
		// no longer a submit; respond_dialog_test.go pins that instead.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(tc.mux)
			if err := tc.act(d); err != nil {
				t.Fatal(err)
			}
			submits := 0
			for _, c := range tc.mux.callsSnapshot() {
				if c[0] != "send-keys" {
					continue
				}
				keys := sentKeys(c)
				var last string
				if len(keys) > 0 {
					last = keys[len(keys)-1]
				}
				if !isNewlineKey(last) {
					continue // not a submit — Escape, C-u and friends
				}
				submits++
				if len(keys) < 2 {
					t.Fatalf("submit sent %v — a lone newline is swallowed as the first "+
						"keystroke into an idle pane (6/6), stranding the text in the composer", keys)
				}
				if wake := keys[len(keys)-2]; !printableKey(wake) {
					t.Errorf("key before the newline is %q, which is not printable; keys were %v — "+
						"only a printable key was measured to wake the pane", wake, keys)
				}
			}
			if submits == 0 {
				t.Fatal("no submit invocation was made at all; this assertion checked nothing")
			}
		})
	}
}

// A pane that never renders the delivered text must not be submitted blind,
// and the caller must be TOLD the text is sitting there — silence is how a
// session ends up holding an instruction nobody knows about.
func TestUnrenderedTextIsReportedNotSubmitted(t *testing.T) {
	f := twoSessions()
	f.noEcho = true
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "never renders", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Errorf("outcome = %q, want unknown — delivered but not confirmed", got.Outcome)
	}
	if !strings.Contains(got.Reason, "unsent") {
		t.Errorf("reason = %q; it must say the text is sitting in the composer", got.Reason)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			t.Error("submitted without confirming the text had rendered")
		}
	}
}

// §8: "`since` is the time the status was first observed to hold, not the time
// it began." Holding it steady across reads is what makes duration meaningful.
func TestSinceHoldsWhileTheStatusDoesAndResetsWhenItChanges(t *testing.T) {
	f := twoSessions()
	clock := time.Unix(1785760000, 0)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return clock }))

	first, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Since == nil {
		t.Fatal("no since recorded")
	}
	t0 := *first.Since

	clock = clock.Add(2 * time.Hour)
	again, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Since.Equal(t0) {
		t.Errorf("since moved to %v while the status was unchanged; duration is only "+
			"meaningful if the clock does not restart on every read", *again.Since)
	}

	// A real change restarts it.
	f.setCapture("%1", fixtureWorking)
	clock = clock.Add(time.Minute)
	changed, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Since.Equal(t0) {
		t.Error("since survived a status change; it marks when THIS status began")
	}
}

// The discriminator a sibling project could only get by typing into the pane —
// available passively as duration. Text unchanged for hours is not a sentence
// somebody is still composing.
func TestLongHeldUnsentInputSaysHowLong(t *testing.T) {
	f := twoSessions()
	clock := time.Unix(1785760000, 0)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return clock }))

	// beta holds unsent input in the fixture.
	if _, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(14 * time.Hour)
	got, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != fleet.StatusWaitingInput {
		t.Fatalf("status = %q", got.Status)
	}
	if !strings.Contains(got.Evidence, "unchanged for") {
		t.Errorf("evidence = %q; a human reading this needs the age, which is the "+
			"whole difference between 'someone is typing' and 'nobody is coming back'", got.Evidence)
	}
	if age := clock.Sub(*got.Since); age < 13*time.Hour {
		t.Errorf("since implies %v; want the full holding period", age)
	}
}

// A brief hold is noise, not a story.
func TestRecentUnsentInputDoesNotShoutAboutAge(t *testing.T) {
	f := twoSessions()
	clock := time.Unix(1785760000, 0)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return clock }))
	if _, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(30 * time.Second)
	got, _ := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"})
	if strings.Contains(got.Evidence, "unchanged for") {
		t.Errorf("evidence = %q; thirty seconds is somebody typing", got.Evidence)
	}
}

// The abstraction is only complete if a client never has to know what the
// substrate is — including for the one job a supervisor's users touch
// directly, which is putting a terminal in front of a session.
func TestListCarriesAnAttachHint(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var h *fleet.AttachHint
	for _, s := range col.Items() {
		if s.ID == "alpha💬" {
			h = s.Attach
		}
	}
	if h == nil {
		t.Fatal("no attach hint; a client would have to know this is a multiplexer")
	}
	if h.Target != "alpha💬" {
		t.Errorf("target = %q, want the session's own handle", h.Target)
	}
	if len(h.Command) == 0 || len(h.ReadOnly) == 0 {
		t.Fatal("both a take-over and a watch form are needed; a client that cannot tell them apart offers the dangerous one")
	}
	// Argv, not a shell string: this id contains an emoji, and a caller
	// interpolating it into a shell is a quoting bug waiting to happen.
	last := h.Command[len(h.Command)-1]
	if last != "alpha💬" {
		t.Errorf("command should end with the verbatim id, got %q", last)
	}
	if h.Command[0] == "tmux" {
		t.Error("hint must carry the resolved binary path; a bare name fails in the non-interactive shell a supervisor runs it from")
	}
	if !h.Shared {
		t.Error("this substrate allows concurrent viewers; saying otherwise makes a supervisor warn about eviction that cannot happen")
	}
}

// A multi-line paste is not echoed: the runtime collapses it into a summary,
// so the bytes just delivered appear nowhere on screen. Matching the text then
// fails forever and every long message is reported stranded — delivered,
// honestly refused, and left sitting in the composer. Measured the first time a
// long message went to a live session.
func TestCollapsedPasteCountsAsDelivered(t *testing.T) {
	const rule = "────────────────────"
	cases := []struct {
		name    string
		painted string
		want    bool
	}{
		{"the observed form", rule + "\n❯ [Pasted text #1 +8 lines]\n" + rule, true},
		{"reworded, still counting lines", rule + "\n❯ [attached 12 lines]\n" + rule, true},
		{"a bracketed thing that is not a paste", rule + "\n❯ see [the docs] first\n" + rule, false},
		{"an ordinary typed line", rule + "\n❯ merge it\n" + rule, false},
		{"empty composer", rule + "\n❯ \n" + rule, false},
		{"a bracket somewhere else entirely", "transcript [4 lines] here\n" + rule + "\n❯ \n" + rule, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composerHoldsCollapsedPaste(tc.painted); got != tc.want {
				t.Errorf("composerHoldsCollapsedPaste = %v, want %v", got, tc.want)
			}
		})
	}
}

// captureCounter wraps a fakeMux so a test can change what the composer
// shows PARTWAY through a confirmation loop — the shape every test below
// needs, because the defect under test only shows up across two readings: a
// residue that was already there, and a change that happened after it.
func captureCounter(f *fakeMux, onNthCapture int, then func()) execFunc {
	calls := 0
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.exec(ctx, name, args...)
		if len(args) > 0 && args[0] == "capture-pane" {
			calls++
			if calls == onNthCapture {
				then()
			}
		}
		return out, err
	}
}

// Issue #37: `confirmLanded` used to accept ANY collapsed-paste marker as
// evidence that THIS delivery landed — including a marker that was sitting
// in the composer before this delivery ever pasted anything. This is the
// false positive half of that defect: attribution must come from a marker
// this delivery caused to APPEAR, never from one merely present.
//
// Reverting `confirmLanded` to `composerHoldsCollapsedPaste(painted)` (no
// `before`, no `gained`) makes this test fail: the residue alone satisfies
// that check on the very first read, before this delivery's own marker (#11)
// has appeared at all.
func TestConfirmLandedIgnoresResidueAndAttributesOnlyTheNewMarker(t *testing.T) {
	const residue = "transcript\n" + rule + "\n❯ [Pasted text #10 +12 lines]\n" + rule
	const residuePlusOurs = "transcript\n" + rule +
		"\n❯ [Pasted text #10 +12 lines][Pasted text #11 +30 lines]\n" + rule

	f := twoSessions()
	f.setCapture("%1", residue)
	d := newTestDriver(f)
	sc, _ := d.captureForClassify(context.Background(), "%1")
	before := composerMarkers(sc)
	if len(before) != 1 {
		t.Fatalf("setup: before-snapshot = %v, want exactly the one pre-existing marker", before)
	}

	// Sanity: this residue is exactly the shape the OLD check treated as
	// landing evidence, so the contrast below is not assumed.
	if !composerHoldsCollapsedPaste(residue) {
		t.Fatal("setup: residue must look like positive evidence under the old check, " +
			"or this test proves nothing about the fix")
	}

	// This delivery's own paste renders on the SECOND capture, not the first —
	// modelling the render lag confirmLanded's own doc comment describes.
	d2 := New("testbox", withExec(captureCounter(f, 2, func() {
		f.setCapture("%1", residuePlusOurs)
	})), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))

	// "q", not "x": muster#143 made confirmLanded's literal-match needle
	// the first LINE alone, which here is a single character — and "x" is a
	// substring of "text", which appears in the fixture's own collapsed-paste
	// marker text ("Pasted text #…"). That coincidence would satisfy the
	// literal-match branch on the very first read, before this delivery's own
	// marker (#11) ever appears, which is exactly the false positive this
	// test exists to rule out. "q" appears nowhere in the fixture.
	key, atCount, ok := d2.confirmLandedV2(context.Background(), "%1", strings.Repeat("q\n", 30), before, false, false)
	if !ok {
		t.Fatal("never confirmed landed, even after this delivery's own marker appeared")
	}
	want := pasteKey{index: 11, lines: 30}
	if key != want {
		t.Errorf("attributed %+v, want %+v — the NEW marker, not the pre-existing residue (#10)", key, want)
	}
	if atCount != 1 {
		t.Errorf("atCount = %d, want 1", atCount)
	}
}

// Issue #37's measured false negative: two consecutive `unknown` receipts for
// a delivery the agent had already received and was acting on, because
// `confirmSubmitted` watched only for the composer to go fully EMPTY — which
// residue that has nothing to do with this delivery can prevent forever.
//
// Reverting `confirmSubmitted` to the composer-emptied-only check makes this
// test fail: the residue marker (#10) never leaves in this fixture, so the
// composer is never empty, and the old check has no other way to notice that
// THIS delivery's own block (#11) cleared.
func TestConfirmSubmittedDetectsOurBlockLeavingDespiteResidue(t *testing.T) {
	const bothBlocks = "transcript\n" + rule +
		"\n❯ [Pasted text #10 +12 lines][Pasted text #11 +30 lines]\n" + rule
	const residueOnly = "transcript\n" + rule + "\n❯ [Pasted text #10 +12 lines]\n" + rule

	f := twoSessions()
	f.setCapture("%1", bothBlocks)

	// Sanity: the composer never reads empty in this fixture, before or
	// after — the old single-signal check could not pass here no matter how
	// long it waited.
	for _, painted := range []string{bothBlocks, residueOnly} {
		if text, scan := composerText(newScreen(painted)); scan != composerFound || text == "" {
			t.Fatalf("setup: composerText(%q) = %q, %v; want non-empty so the old "+
				"composer-emptied check is genuinely defeated by this fixture", painted, text, scan)
		}
	}

	ours := pasteKey{index: 11, lines: 30}
	d := New("testbox", withExec(captureCounter(f, 2, func() {
		f.setCapture("%1", residueOnly) // our block (#11) submitted and cleared; #10 remains
	})), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))

	// Bounded, unlike the driver's own frozen test clock: the sanity check
	// above proves the composer-emptied signal alone never resolves in this
	// fixture, so a build missing the count-dropped signal would spin this
	// loop for real wall-clock time, not fail fast. This bound is generous
	// next to the ~150ms the fix needs (one poll interval), and only exists
	// to keep a regression from hanging the suite instead of failing it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !d.confirmSubmitted(ctx, "%1", ours, 1) {
		t.Fatal("did not detect the submit — this delivery's own marker count dropped to 0, " +
			"which must be reported as submitted even though residue keeps the composer non-empty")
	}
}

// Issue #37's third bullet: attribution must fail CLOSED, not guess, when
// more than one marker grows across a single delivery — something other than
// this delivery also changed the composer in the same window, and claiming
// either key would be the unfounded confidence the whole fix removes.
func TestGainedFailsClosedWhenTwoKeysGrowAtOnce(t *testing.T) {
	before := map[pasteKey]int{{index: 1, lines: 5}: 1}
	after := map[pasteKey]int{
		{index: 1, lines: 5}: 1, // unchanged
		{index: 2, lines: 7}: 1, // grew
		{index: 3, lines: 9}: 1, // grew
	}
	if key, ok := gained(before, after); ok {
		t.Fatalf("attributed %+v while two keys grew at once; ambiguity must yield no attribution", key)
	}
}

// Rename changes the very thing callers address a session by, so it is
// corroborated exactly as Close is — and its failure is quieter than Close's:
// renaming the wrong session succeeds silently and leaves two sessions
// misnamed rather than raising anything.
func TestRenameCorroboratesLikeClose(t *testing.T) {
	ctx := context.Background()

	t.Run("with the caller's expected start time", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		started := time.Unix(1785600000, 0)
		req := testCaller
		req.Expect.StartedAt = &started

		if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		col, _ := d.List(ctx, testCaller, driver.ListFilter{})
		var names []string
		for _, s := range col.Items() {
			names = append(names, s.ID)
		}
		if !slicesContains(names, "renamed💬") || slicesContains(names, "alpha💬") {
			t.Errorf("after rename the fleet reads %v", names)
		}
	})

	t.Run("a stale expectation is refused", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		wrong := time.Unix(1700000000, 0)
		req := testCaller
		req.Expect.StartedAt = &wrong

		_, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬")
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("want an ambiguous-target refusal, got %v", err)
		}
	})

	t.Run("nothing to corroborate against is refused", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f) // never listed, so the driver has no sighting
		_, err := d.Rename(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬")
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("want a refusal when nothing corroborates, got %v", err)
		}
	})

	t.Run("a name already in use is refused", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		started := time.Unix(1785600000, 0)
		req := testCaller
		req.Expect.StartedAt = &started

		_, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "beta")
		if err == nil {
			t.Error("renaming onto a live session's name must be refused, not left to the multiplexer")
		}
	})

	t.Run("an unknown id is not found", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		_, err := d.Rename(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "ghost"}, "whatever")
		if !errors.Is(err, fleet.ErrNoSuchSession) {
			t.Errorf("want no-such-session, got %v", err)
		}
	})
}

// The driver's memory must move with the name, or the renamed session looks
// brand new: `since` resets and §12 reports it as newly adopted.
func TestRenameCarriesTheDriversMemory(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(ctx, testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	started := time.Unix(1785600000, 0)
	req := testCaller
	req.Expect.StartedAt = &started
	if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	d.mu.Lock()
	_, oldGone := d.observed["alpha💬"]
	_, newKept := d.observed["renamed💬"]
	d.mu.Unlock()
	if oldGone {
		t.Error("the old id is still remembered")
	}
	if !newKept {
		t.Error("the driver forgot the session it just renamed — since would reset and §12 would call it adopted")
	}
}

// muster #97: a rename that is accepted, reads back correct, and then
// — with no request asking for it — no longer holds. List must notice and
// put it back, whichever of the two ways it did not hold: it reached the
// runtime and was later undone by a second actor on the machine, or it
// never reached the runtime at all.
func TestRenameSurvivesARuntimeRevert(t *testing.T) {
	ctx := context.Background()
	started := time.Unix(1785600001, 0) // beta's own created time, from twoSessions
	renameBetaTo := func(t *testing.T, d *Driver, to string) {
		t.Helper()
		req := testCaller
		req.Expect.StartedAt = &started
		if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "beta"}, to); err != nil {
			t.Fatalf("rename: %v", err)
		}
	}
	revertBeta := func(f *fakeMux, from string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.sessions {
			if f.sessions[i].name == from {
				f.sessions[i].name = "beta"
			}
		}
	}

	t.Run("reached the runtime, then a second actor reverted it", func(t *testing.T) {
		dir := t.TempDir()
		f := twoSessions()
		d := stateDriver(t, f, dir)

		if _, err := d.List(ctx, testCaller, listAll()); err != nil {
			t.Fatal(err)
		}
		renameBetaTo(t, d, "beta-x")
		col, err := d.List(ctx, testCaller, listAll())
		if err != nil {
			t.Fatal(err)
		}
		if !hasSessionID(col, "beta-x") {
			t.Fatalf("rename did not read back: %v", idsOf(col))
		}

		// A second actor on the machine reverts it — entirely outside this
		// driver, directly against the fake, the way something this
		// service does not own would touch the real multiplexer.
		revertBeta(f, "beta-x")

		baseline := len(f.callsSnapshot())
		// The read that first observes the revert reports the runtime
		// truthfully (it is genuinely "beta" again at this instant) but
		// must say so rather than agree silently — #97's own defect — and
		// repairs it as a side effect, for the NEXT read to find converged.
		firstAfterRevert, err := d.List(ctx, testCaller, listAll())
		if err != nil {
			t.Fatal(err)
		}
		if !evidenceMentionsDrift(firstAfterRevert, "beta", "beta-x") {
			t.Errorf("the first read after the revert did not say the runtime disagreed with what this machine asserted")
		}
		var sawRepair bool
		for _, c := range f.callsSnapshot()[baseline:] {
			if len(c) > 2 && c[0] == "rename-session" && c[1] == "-t" && c[2] == "=beta" {
				sawRepair = true
			}
		}
		if !sawRepair {
			t.Fatal("no rename-session call was issued to put the name back")
		}

		col, err = d.List(ctx, testCaller, listAll())
		if err != nil {
			t.Fatal(err)
		}
		if !hasSessionID(col, "beta-x") || hasSessionID(col, "beta") {
			t.Fatalf("the next read did not converge onto the asserted name: %v", idsOf(col))
		}
	})

	t.Run("never reached the runtime at all", func(t *testing.T) {
		dir := t.TempDir()
		f := twoSessions()
		d := stateDriver(t, f, dir)

		if _, err := d.List(ctx, testCaller, listAll()); err != nil {
			t.Fatal(err)
		}
		f.setRenameNoop(true)
		renameBetaTo(t, d, "beta-x") // "succeeds" (nil error); nothing moved
		f.setRenameNoop(false)

		// Same shape as the other hypothesis: the first read after the
		// rename reports what is genuinely live (still "beta") and repairs
		// it as a side effect.
		if _, err := d.List(ctx, testCaller, listAll()); err != nil {
			t.Fatal(err)
		}
		col, err := d.List(ctx, testCaller, listAll())
		if err != nil {
			t.Fatal(err)
		}
		if !hasSessionID(col, "beta-x") {
			t.Fatalf("the record did not converge the runtime onto the asserted name: %v", idsOf(col))
		}
	})
}

// muster #223: a name the multiplexer would silently mangle (naming.go's
// sanitizeName) must be refused, not renamed-to-and-announced — the same
// principle as the "already in use" collision case a few lines above in
// Rename itself.
func TestRenameRefusesANameTheMultiplexerWouldMangle(t *testing.T) {
	ctx := context.Background()
	started := time.Unix(1785600000, 0)
	req := testCaller
	req.Expect.StartedAt = &started

	cases := []struct {
		name string
		to   string
	}{
		{"a period the multiplexer mangles to an underscore", "renamed.session"},
		{"a colon, the multiplexer's own target separator", "renamed:session"},
		{"a leading dash, which argv parsing downstream reads as a flag", "-renamed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := twoSessions()
			d := newTestDriver(f)
			_, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, tc.to)
			if err == nil {
				t.Fatalf("renaming to %q must be refused, not silently cleaned and announced", tc.to)
			}
			if clean := sanitizeName(tc.to); !strings.Contains(err.Error(), clean) {
				t.Errorf("refusal %q does not name the clean form %q a caller could retry with", err, clean)
			}
			// The multiplexer must not have been touched at all: the old id
			// is still live, and no session carries the requested (unclean) name.
			col, lerr := d.List(ctx, testCaller, driver.ListFilter{})
			if lerr != nil {
				t.Fatal(lerr)
			}
			if !hasSessionID(col, "alpha💬") || hasSessionID(col, tc.to) {
				t.Fatalf("a refused rename must leave the multiplexer untouched: %v", idsOf(col))
			}
		})
	}

	t.Run("already-clean names are unaffected", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed-session"); err != nil {
			t.Fatalf("rename of an already-clean name must succeed: %v", err)
		}
	})
}

// muster #223: the four per-id records d.observed's own move (above, in
// Rename) and d.mods.rekey (#185) did not already cover — a stranded
// delivery, its tombstones, the #184 cross-path ledger, and the delivery
// mark #111's `turns` is counted from — must move to the new id too, or a
// resume/lookup against the NEW id finds nothing while the OLD id's entry
// sits orphaned forever.
func TestRenameMovesPerIDStateToTheNewID(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	d := newTestDriver(f)

	const oldID, newID, cwd = "alpha💬", "renamed💬", "/work/alpha"

	// Seed a live stranded record AND a tombstone (a second, different
	// strand buries the first — draftrule_test.go's own setup shape).
	d.noteStranded(oldID, cwd, "first text", composerTextDigest("first text"))
	d.noteStranded(oldID, cwd, "second text", composerTextDigest("second text"))
	// Seed the #184 cross-path ledger.
	d.noteUnconfirmed(oldID, unconfirmedEntry{Key: "k1", Cwd: cwd})
	// Seed a delivery mark (#111's `turns` denominator).
	d.noteDelivery(oldID, cwd)

	started := time.Unix(1785600000, 0)
	req := testCaller
	req.Expect.StartedAt = &started
	if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: oldID}, newID); err != nil {
		t.Fatalf("rename: %v", err)
	}

	d.mu.Lock()
	_, strandedOldGone := d.stranded[oldID]
	strandedNew, strandedNewOK := d.stranded[newID]
	tombstonesOldGone := len(d.tombstones[oldID]) == 0
	tombstonesNew := len(d.tombstones[newID])
	_, unconfirmedOldGone := d.unconfirmed[oldID]
	unconfirmedNew := len(d.unconfirmed[newID])
	_, deliveredOldGone := d.delivered[oldID]
	deliveredNew, deliveredNewOK := d.delivered[newID]
	d.mu.Unlock()

	if strandedOldGone {
		t.Error("a stranded record is still keyed to the old id")
	}
	if !strandedNewOK || strandedNew.Text != "second text" {
		t.Errorf("the stranded record did not move to the new id: %+v (ok=%v)", strandedNew, strandedNewOK)
	}
	if !tombstonesOldGone {
		t.Error("a tombstone is still keyed to the old id")
	}
	if tombstonesNew != 1 {
		t.Errorf("tombstones under the new id = %d, want 1", tombstonesNew)
	}
	if unconfirmedOldGone {
		t.Error("a #184 ledger entry is still keyed to the old id")
	}
	if unconfirmedNew != 1 {
		t.Errorf("#184 ledger entries under the new id = %d, want 1", unconfirmedNew)
	}
	if deliveredOldGone {
		t.Error("a delivery mark is still keyed to the old id")
	}
	if !deliveredNewOK || deliveredNew.Cwd != cwd {
		t.Errorf("the delivery mark did not move to the new id: %+v (ok=%v)", deliveredNew, deliveredNewOK)
	}
}

func evidenceMentionsDrift(col fleet.Collection[fleet.Session], liveID, want string) bool {
	for _, s := range col.Items() {
		if s.ID == liveID && strings.Contains(s.State.Evidence, want) {
			return true
		}
	}
	return false
}

// A repair already proven not to hold is not attempted forever —
// discardProvenFutile's rule (this file, the composer-clear case) applied
// to identity: an unbounded loop against a second actor that keeps
// reverting a name is a rename war, not a fix.
func TestIdentityReassertStopsOnceContested(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)

	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}
	started := time.Unix(1785600001, 0)
	req := testCaller
	req.Expect.StartedAt = &started
	if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "beta-x"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	baseline := len(f.callsSnapshot())

	revert := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.sessions {
			if f.sessions[i].name == "beta-x" {
				f.sessions[i].name = "beta"
			}
		}
	}

	for i := 0; i < 5; i++ {
		revert()
		if _, err := d.List(ctx, testCaller, listAll()); err != nil {
			t.Fatal(err)
		}
	}

	var reasserts int
	for _, c := range f.callsSnapshot()[baseline:] {
		if len(c) > 2 && c[0] == "rename-session" && c[1] == "-t" && c[2] == "=beta" {
			reasserts++
		}
	}
	if reasserts != maxNameReasserts {
		t.Errorf("issued %d rename-session repairs across 5 reverts, want exactly %d (bounded)",
			reasserts, maxNameReasserts)
	}
	if got := d.counters.Snapshot()[counterIdentityContested]; got < 1 {
		t.Errorf("contested counter = %d, want at least 1", got)
	}
}

// Two live sessions cannot share a name — reassertNames must refuse a
// collision rather than let the multiplexer decide, the same rule Rename
// itself already applies at request time.
func TestReassertRefusesWhenTheNameIsTaken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)

	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}
	started := time.Unix(1785600001, 0)
	req := testCaller
	req.Expect.StartedAt = &started
	if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "beta-x"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	baseline := len(f.callsSnapshot())

	// A second actor reverts it, AND — independently — some other session
	// now occupies the very name this driver wants back.
	f.mu.Lock()
	for i := range f.sessions {
		if f.sessions[i].name == "beta-x" {
			f.sessions[i].name = "beta"
		}
	}
	f.mu.Unlock()
	f.addSession(fakeSession{name: "beta-x", paneID: "%99", cwd: "/work/other", pid: 999, created: 1785600555},
		idleFixtureFor("other"))

	col, err := d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.callsSnapshot()[baseline:] {
		if len(c) > 0 && c[0] == "rename-session" {
			t.Fatalf("a rename-session call was issued even though the wanted name is live under another session: %v", c)
		}
	}
	if got := d.counters.Snapshot()[counterIdentityContested]; got < 1 {
		t.Errorf("contested counter = %d, want at least 1", got)
	}
	// beta stays "beta" — reported as drifted, never silently merged into
	// the other session's name.
	if !hasSessionID(col, "beta") {
		t.Fatalf("beta went missing rather than staying put while contested: %v", idsOf(col))
	}
}

func hasSessionID(col fleet.Collection[fleet.Session], id string) bool {
	for _, s := range col.Items() {
		if s.ID == id {
			return true
		}
	}
	return false
}

func idsOf(col fleet.Collection[fleet.Session]) []string {
	var out []string
	for _, s := range col.Items() {
		out = append(out, s.ID)
	}
	return out
}

func sessionByID(col fleet.Collection[fleet.Session], id string) (fleet.Session, bool) {
	for _, s := range col.Items() {
		if s.ID == id {
			return s, true
		}
	}
	return fleet.Session{}, false
}

// muster #102: the same fact evidenceMentionsDrift checks for in prose
// must also be readable structurally, and the two must never disagree about
// the same read — walks the same revert scenario
// TestRenameSurvivesARuntimeRevert does, then checks IdentityAssertion at
// each step: drifted on the read that discovers it, held on the read after
// the repair, absent for a session this driver never asserted anything for.
func TestIdentityAssertionReportsDriftStructurally(t *testing.T) {
	ctx := context.Background()
	started := time.Unix(1785600001, 0) // beta's own created time, from twoSessions
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)

	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}
	req := testCaller
	req.Expect.StartedAt = &started
	if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "beta-x"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	col, err := d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	renamed, ok := sessionByID(col, "beta-x")
	if !ok {
		t.Fatalf("rename did not read back: %v", idsOf(col))
	}
	if renamed.IdentityAssertion == nil || renamed.IdentityAssertion.Drifted == nil || *renamed.IdentityAssertion.Drifted {
		t.Errorf("a freshly held rename must report IdentityAssertion.Drifted = false, got %+v", renamed.IdentityAssertion)
	}

	// alpha was never created or renamed by this driver — an adopted/foreign
	// session in every sense this record store knows. The field must be
	// absent, never Drifted:false.
	alpha, ok := sessionByID(col, "alpha💬")
	if !ok {
		t.Fatalf("alpha went missing: %v", idsOf(col))
	}
	if alpha.IdentityAssertion != nil {
		t.Errorf("a session this driver never asserted an identity for must carry no IdentityAssertion, got %+v", alpha.IdentityAssertion)
	}

	// A second actor on the machine reverts it — outside this driver
	// entirely, directly against the fake, the way something this service
	// does not own would touch the real multiplexer.
	f.mu.Lock()
	for i := range f.sessions {
		if f.sessions[i].name == "beta-x" {
			f.sessions[i].name = "beta"
		}
	}
	f.mu.Unlock()

	firstAfterRevert, err := d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	drifted, ok := sessionByID(firstAfterRevert, "beta")
	if !ok {
		t.Fatalf("beta went missing after the revert: %v", idsOf(firstAfterRevert))
	}
	ia := drifted.IdentityAssertion
	if ia == nil || ia.Drifted == nil || !*ia.Drifted {
		t.Fatalf("the first read after the revert must report Drifted = true, got %+v", ia)
	}
	if ia.Asserted != "beta-x" || ia.Carried != "beta" {
		t.Errorf("Asserted/Carried = %q/%q, want beta-x/beta", ia.Asserted, ia.Carried)
	}
	// Cross-check: the same read's prose evidence and the structural field
	// must agree — one computation, two channels (reconcile.go's
	// driftSentence).
	if !evidenceMentionsDrift(firstAfterRevert, "beta", "beta-x") {
		t.Error("the first read after the revert did not say so in state.evidence either")
	}

	// The read that discovered the drift also repaired it as a side effect
	// (reassertNames, after this response was already built) — the NEXT
	// read is where that repair shows up, never this one.
	col, err = d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	converged, ok := sessionByID(col, "beta-x")
	if !ok {
		t.Fatalf("the next read did not converge onto the asserted name: %v", idsOf(col))
	}
	if converged.IdentityAssertion == nil || converged.IdentityAssertion.Drifted == nil || *converged.IdentityAssertion.Drifted {
		t.Errorf("the read after repair must report Drifted = false (a per-read observation, not a latch), got %+v", converged.IdentityAssertion)
	}
}

// muster #102: a create's own response, and the first listing right
// after it, both report the asserted identity as unresolved — nothing has
// read the session back yet, so neither may claim the runtime carries it.
func TestIdentityAssertionUncorroboratedAtCreate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)

	ref, err := d.Create(ctx, testCaller, "key-gamma", fleet.SessionSpec{Cwd: "/work/gamma", Name: "gamma"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: ref.ID, paneID: "%50", cwd: "/work/gamma", pid: 500, created: 1785600600},
		idleFixtureFor("gamma"))

	ia := ref.IdentityAssertion
	if ia == nil {
		t.Fatal("Create's own response must carry IdentityAssertion, not leave it absent")
	}
	if ia.Drifted != nil {
		t.Errorf("Create's response must report Drifted = nil (unresolved), got %v", ia.Drifted)
	}
	if ia.Asserted != ref.ID {
		t.Errorf("Asserted = %q, want %q", ia.Asserted, ref.ID)
	}

	col, err := d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	first, ok := sessionByID(col, ref.ID)
	if !ok {
		t.Fatalf("the created session did not read back: %v", idsOf(col))
	}
	if first.IdentityAssertion == nil || first.IdentityAssertion.Drifted != nil {
		t.Errorf("the first listing right after create must still report Drifted = nil, got %+v", first.IdentityAssertion)
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// Discard destroys somebody's typing, so every refusal here is the feature.
func TestDiscardRefusesWhatItCannotCorroborate(t *testing.T) {
	ctx := context.Background()

	t.Run("blind discard is refused", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = fixtureUnsent
		d := newTestDriver(f)
		_, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "", driver.DiscardOptions{})
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("no digest should be refused, not treated as permission; got %v", err)
		}
	})

	t.Run("a stale digest is refused", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = fixtureUnsent
		d := newTestDriver(f)
		_, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "not-the-digest", driver.DiscardOptions{})
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("a changed composer must refuse — somebody may be typing; got %v", err)
		}
	})

	t.Run("an empty composer succeeds, so a retry is safe", func(t *testing.T) {
		f := twoSessions() // alpha's composer is empty
		d := newTestDriver(f)
		if _, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "anything", driver.DiscardOptions{}); err != nil {
			t.Errorf("clearing nothing destroys nothing and must not fail: %v", err)
		}
	})

	t.Run("an unknown session is not found", func(t *testing.T) {
		d := newTestDriver(twoSessions())
		_, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "ghost"}, "x", driver.DiscardOptions{})
		if !errors.Is(err, fleet.ErrNoSuchSession) {
			t.Errorf("want no-such-session, got %v", err)
		}
	})
}

// muster#134: before this fix, a composer taller than the capture
// window read as composerAbsent, so Discard's early return
// ("pending == \"\" means already clear") fired and reported success on a
// composer that, off-screen, still held real unsent text — the exact false
// negative #134 exists to close. This proves the fix: Discard must refuse,
// not accept, and it must do so WITHOUT pressing a single key — a clipped
// composer is never corroborated, so clearComposer must never run against
// it (see this function's own doc comment on discardComposerClipped).
func TestDiscardRefusesAClippedComposerRatherThanClaimingSuccess(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = clippedComposerFixture()
	d := newTestDriver(f)

	_, err := d.Discard(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, "any-digest-the-caller-happens-to-have", driver.DiscardOptions{})
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Fatalf("want ErrAmbiguousTarget for a clipped composer, got %v", err)
	}
	if !strings.Contains(err.Error(), "capture window") {
		t.Errorf("error should say why this driver could not act: %v", err)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "send-keys" {
			t.Errorf("a clipped composer must never be pressed against — nothing was "+
				"corroborated, so clearComposer must never run; saw %v", call)
		}
	}
	assertClippedRemedy(t, err.Error())
	if got := d.Counters()[counterComposerClippedRefusedDiscard]; got != 1 {
		t.Errorf("%s = %d, want 1: the refusal's rate is the evidence ADR 149's reopen condition reads", counterComposerClippedRefusedDiscard, got)
	}
	assertNoWidenedCapture(t, f)
}

// assertClippedRemedy pins muster#149's message contract for every
// clipped-composer refusal: it carries the one shared remedy, and it no longer
// tells the caller to wait for the composer to shrink — the advice that sent
// unattended callers into a retry loop that could not end.
func assertClippedRemedy(t *testing.T, msg string) {
	t.Helper()
	if !strings.Contains(msg, clippedComposerRemedy) {
		t.Errorf("refusal must carry the shared clipped-composer remedy; got %q", msg)
	}
	if strings.Contains(msg, "shrink") {
		t.Errorf("refusal still promises the composer will shrink back — that never arrives on an unattended session (#149): %q", msg)
	}
}

// assertNoWidenedCapture pins ADR 149's rejection of a wider read as a way
// out: every capture this driver issued used the configured window, never a
// full-scrollback start or a larger one reached for because the first read
// came back clipped.
func assertNoWidenedCapture(t *testing.T, f *fakeMux) {
	t.Helper()
	want := fmt.Sprintf("-%d", defaultCaptureLines)
	for _, call := range f.callsSnapshot() {
		for i, a := range call {
			if a == "-S" && (i+1 >= len(call) || call[i+1] != want) {
				t.Errorf("a capture started somewhere other than the configured window (want -S %s): %v", want, call)
			}
		}
	}
}

// muster#149: no call shape moves a clipped-composer discard. Not an
// empty expect, not a digest the caller happens to hold, and not force —
// force authorises a stronger clear once corroboration has PASSED (#136), and
// here there is nothing to corroborate against. Each attempt refuses the same
// way, presses nothing, and is counted.
func TestDiscardClippedRefusalIsIndependentOfExpectAndForce(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = clippedComposerFixture()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	attempts := []struct {
		name   string
		expect string
		opts   driver.DiscardOptions
	}{
		{"no expect", "", driver.DiscardOptions{}},
		{"some expect", "x", driver.DiscardOptions{}},
		{"expect and force", "x", driver.DiscardOptions{Force: true}},
	}
	var first string
	for _, a := range attempts {
		_, err := d.Discard(context.Background(), testCaller, ref, a.expect, a.opts)
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Fatalf("%s: want ErrAmbiguousTarget, got %v", a.name, err)
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Errorf("%s: refusal changed with the call shape, which would read as a way out:\n got %q\nwant %q", a.name, err.Error(), first)
		}
		assertClippedRemedy(t, err.Error())
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "send-keys" {
			t.Errorf("no call shape may press against a clipped composer; saw %v", call)
		}
	}
	if got := d.Counters()[counterComposerClippedRefusedDiscard]; got != int64(len(attempts)) {
		t.Errorf("%s = %d, want %d", counterComposerClippedRefusedDiscard, got, len(attempts))
	}
	assertNoWidenedCapture(t, f)
}

// The happy path: the digest the caller read is the digest that gets cleared,
// and the clear is VERIFIED rather than assumed — a keystroke that did not
// register looks exactly like one that did.
func TestDiscardClearsWhatTheCallerSaw(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	d := newTestDriver(f)

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a session holding unsent text published no composerDigest, so a caller cannot discard it safely")
	}

	if _, err := d.Discard(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{}); err != nil {
		t.Fatalf("discard with the digest the caller read: %v", err)
	}

	var sawClear bool
	for _, call := range f.callsSnapshot() {
		if len(call) >= 4 && call[0] == "send-keys" && call[len(call)-1] == "C-u" {
			sawClear = true
		}
	}
	if !sawClear {
		t.Error("no clear keystroke was sent")
	}
}

func countClears(calls [][]string) int {
	n := 0
	for _, call := range calls {
		if len(call) >= 4 && call[0] == "send-keys" && call[len(call)-1] == "C-u" {
			n++
		}
	}
	return n
}

// countBackspaces mirrors countClears for the #132 key: Backspace, sent only
// when clearComposer finds the composer's current bottom row already blank.
func countBackspaces(calls [][]string) int {
	n := 0
	for _, call := range calls {
		if len(call) >= 4 && call[0] == "send-keys" && call[len(call)-1] == "BSpace" {
			n++
		}
	}
	return n
}

// Issue #32, surviving half: the clear is a single un-repeated keystroke,
// and C-u (unix-line-discard) only ever kills the line the cursor sits on —
// never the whole buffer. Against a composer holding several logical lines
// (the shape of the 6.6 KB paste in the issue), one press cannot reach
// empty no matter how long the caller waits for confirmation.
//
// This fails against the un-repeated version of Discard: it sends exactly
// one C-u, the fake's line-by-line model drops only the last of three
// lines, and the poll loop then just watches a composer that will never go
// empty until the bound below gives up — countClears would read 1, not 3,
// and the call would return an error instead of Accepted.
func TestDiscardRepeatsTheClearForAMultiLineComposer(t *testing.T) {
	f := twoSessions()
	lines := []string{"just a moment,", "this is the second visual line,", "and this is the third."}
	f.setMultilineComposer("%2", lines)

	// Not newTestDriver's usual frozen clock: bounded()'s deadline is
	// derived from d.now(), and a frozen constant that has already fallen
	// behind the real ctx.WithTimeout below would make bounded() hand back
	// an already-expired context — see the untouched test's comment for the
	// failure that produces.
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a multi-line composer published no composerDigest")
	}

	// A hang-guard only: an un-repeated clear that never reaches empty
	// would otherwise spin this loop for the whole content-derived press
	// budget rather than fail it quickly. Generous next to the ~600ms three
	// presses need.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a %d-line composer: %v", len(lines), err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countClears(f.callsSnapshot()); got < len(lines) {
		t.Errorf("sent %d clear keystrokes for a %d-line composer; a single un-repeated "+
			"C-u only ever kills the line the cursor is on, so this must press it again "+
			"for every line still standing", got, len(lines))
	}
}

// muster#129's own acceptance criterion: a multi-line composer well
// beyond what the retired 3-second promptClearWindow allowed still clears.
// At one press per promptClearInterval (200ms), 3 seconds bought roughly 15
// presses at best, before any subprocess overhead — this composer needs 20,
// which the old flat window could not have delivered at any machine speed.
// clearComposer's press budget is now sized to the composer's own row count
// (composerVisualLines) instead, so this succeeds regardless.
func TestDiscardClearsAComposerBeyondTheOldThreeSecondBudget(t *testing.T) {
	f := twoSessions()
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %d of a paste the old 3-second window could not finish", i)
	}
	f.setMultilineComposer("%2", lines)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a multi-line composer published no composerDigest")
	}

	// A hang-guard only, generous next to the ~4s the 20 presses need.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a %d-line composer, beyond the old window's reach: %v", len(lines), err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countClears(f.callsSnapshot()); got < len(lines) {
		t.Errorf("sent %d clear keystrokes for a %d-line composer; wanted at least one per "+
			"row, which the old 3-second window could not have afforded", got, len(lines))
	}
}

// muster#132: a payload ending in real trailing newlines leaves blank
// rows behind in the composer, and C-u (unix-line-discard) is a guaranteed
// no-op against a blank row — it kills to the start of the CURRENT line and
// never crosses the boundary above it. Against the pre-fix clearComposer
// this composer never moves at all: every C-u press comes back byte-for-byte
// identical, the pass exhausts its budget having achieved nothing, and the
// residue is recorded futile (#87) — permanently wedging the composer,
// exactly the field shape #132 reports.
func TestDiscardCrossesTrailingBlankLinesWithBackspace(t *testing.T) {
	f := twoSessions()
	// The payload's own shape: real content, then two real trailing
	// newlines — composer#132's own field case.
	lines := []string{"the payload ends in two real trailing newlines", "", ""}
	f.setMultilineComposer("%2", lines)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer ending in blank rows published no composerDigest")
	}

	// A hang-guard only: an old C-u-only loop would spin its whole budget
	// without ever moving rather than fail fast, so this is generous next
	// to the handful of presses a fixed loop needs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a composer ending in two blank rows: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countBackspaces(f.callsSnapshot()); got < 2 {
		t.Errorf("sent %d Backspace presses; two blank rows each need one to cross the "+
			"line boundary C-u cannot (muster#132), got %d", got, got)
	}
	if got := countClears(f.callsSnapshot()); got < 1 {
		t.Error("no C-u was sent at all; the one real content row still needs it")
	}
}

// muster#138: composerCursorRowBlank's row-blankness proxy is only
// valid when the cursor sits at the END of the current row — an assumption
// clearComposer used to take on faith. This pins the fix for the ordinary
// non-blank case: composerLineEndKey (End) must be sent immediately before
// C-u, in the SAME send-keys call, so C-u is well-defined regardless of
// where the cursor actually started.
func TestClearComposerPositionsBeforeKillingSoCUCanWorkOnTheCursorRow(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent // single non-blank line
	d := newTestDriver(f)

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer holding unsent text published no composerDigest")
	}

	if _, err := d.Discard(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{}); err != nil {
		t.Fatalf("discard: %v", err)
	}

	var sawPositionedClear bool
	for _, call := range f.callsSnapshot() {
		if len(call) == 5 && call[0] == "send-keys" && call[1] == "-t" &&
			call[3] == composerLineEndKey && call[4] == "C-u" {
			sawPositionedClear = true
		}
		// Never plain C-u alone against a non-blank row: that is exactly
		// the pre-#138 shape whose cursor-position assumption was never
		// actually established.
		if len(call) == 4 && call[0] == "send-keys" && call[3] == "C-u" {
			t.Errorf("sent plain C-u without positioning first: %v", call)
		}
	}
	if !sawPositionedClear {
		t.Errorf("never sent %s then C-u in one call; calls: %v", composerLineEndKey, f.callsSnapshot())
	}
}

// muster#138's own measured shape: residue sitting on the composer's
// ❯-marked row itself, where composerCursorRowBlank structurally reports
// blank=false forever (its own doc comment) — so, pre-#138, clearComposer
// pressed plain C-u to budget exhaustion with zero movement. This proves
// the no-movement latch: once End+C-u has already proven itself a no-op on
// a pane (freezeUnixLineDiscard models a substrate where positioning does
// not help), the NEXT press must use the alternate shape (Backspace alone)
// rather than repeating the one that just failed — and the pass must then
// actually converge.
func TestClearComposerAlternatesAfterAPressThatMovedNothing(t *testing.T) {
	f := twoSessions()
	lines := []string{"residue sitting on the marker row with nothing blank about it"}
	f.setMultilineComposer("%2", lines)
	f.freezeUnixLineDiscard("%2") // End+C-u never works on this pane; Backspace does

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer holding unsent text published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false; the alternate shape (Backspace) should have converged " +
			"once End+C-u proved itself a no-op")
	}

	calls := f.callsSnapshot()
	var sawEndCU, sawBSpaceAfterFailedEndCU bool
	for i, call := range calls {
		if len(call) == 5 && call[0] == "send-keys" && call[3] == composerLineEndKey && call[4] == "C-u" {
			sawEndCU = true
			// The very next send-keys call must be the alternate shape,
			// not a repeat of End+C-u.
			for _, later := range calls[i+1:] {
				if later[0] != "send-keys" {
					continue
				}
				if len(later) == 4 && later[3] == "BSpace" {
					sawBSpaceAfterFailedEndCU = true
				}
				break
			}
			break
		}
	}
	if !sawEndCU {
		t.Fatalf("never sent the ordinary End+C-u shape first: %v", calls)
	}
	if !sawBSpaceAfterFailedEndCU {
		t.Errorf("did not flip to Backspace after End+C-u produced no movement: %v", calls)
	}
}

// muster#132 pinned unbroken by #138: a blank current row must still
// get plain Backspace alone, never composerLineEndKey first. Positioning to
// the end of an already-empty row buys nothing (there is nothing there to
// position after) and #138's own fix is additive to #132's, not a
// replacement of it.
func TestClearComposerStillSendsBSpaceAloneOnABlankRow(t *testing.T) {
	f := twoSessions()
	lines := []string{"real content", ""}
	f.setMultilineComposer("%2", lines)
	// withClock(time.Now), not newTestDriver's fixed clock: this composer
	// needs TWO presses to clear (the blank row, then the real-content row
	// End+C-u empties), so it crosses the select{time.After(...)} wait
	// between iterations — d.bounded's deadline is computed from d.now(),
	// and a fixed/stale clock there makes ctx already-expired by the time
	// that wait is reached, same reason every other multi-press test here
	// (TestDiscardCrossesTrailingBlankLinesWithBackspace and siblings)
	// avoids newTestDriver too.
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer ending in a blank row published no composerDigest")
	}

	if _, err := d.Discard(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{}); err != nil {
		t.Fatalf("discard: %v", err)
	}

	// The FIRST clear press is the one aimed at this fixture's blank bottom
	// row — that specific press is the #132 pin, and it must stay bare
	// Backspace, never composerLineEndKey first (positioning to the end of
	// an already-empty row has nothing to position after). A LATER press,
	// once the blank row is gone and the composer has shrunk to its own
	// ❯-marked row, correctly switches to End+C-u (#138) — that is not a
	// regression, so this only pins the first press's shape.
	var sendKeys [][]string
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "send-keys" {
			sendKeys = append(sendKeys, call)
		}
	}
	if len(sendKeys) == 0 {
		t.Fatal("no send-keys calls at all")
	}
	first := sendKeys[0]
	if len(first) != 4 || first[3] != "BSpace" {
		t.Errorf("first press = %v, want bare Backspace for the blank bottom row", first)
	}
}

// muster#134/#138 interaction: clearComposer's "cleared" check must
// additionally require composerFound, not merely got=="" — a mid-pass
// capture that comes back composerClipped (this driver's OWN capture
// running out, not the composer actually emptying) produces exactly the
// same "" text a genuinely empty composer would, and must never be read as
// success. Modelled by freezing the clear keystroke (so nothing about the
// real composer content ever changes) and swapping the pane's capture to a
// taller-than-the-window shape right after Discard's own initial read —
// so the FIRST post-press re-read inside clearComposer's loop sees the
// clipped shape instead of the unchanged original.
func TestClearComposerDoesNotReportClearedOnAClippedCapture(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = composerHoldingRows([]string{"a single unsent line"})
	f.freezeComposer("%2") // the press itself must be a genuine no-op

	execWithSwap := captureCounter(f, 2, func() {
		// Capture-pane call #1 is List's own read (used below for the
		// digest); call #2 is Discard's own initial enumerate — the read
		// `sc`/`pending` come from. Swap right after it so the FIRST
		// post-press re-read (capture-pane call #3, inside clearComposer's
		// loop) sees the clipped shape instead of the unchanged original.
		f.setCapture("%2", clippedComposerFixture())
	})
	d := New("testbox", withExec(execWithSwap), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer holding unsent text published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{}); err == nil {
		t.Fatal("want a refusal/error: a clipped mid-pass capture must never be read as " +
			"\"cleared\" just because its extracted text happens to be empty")
	}
}

// The single-trailing-newline shape — the minimal case a payload from
// `echo` or an editor ordinarily produces, as distinct from #132's own
// two-newline field case above.
func TestDiscardCrossesASingleTrailingBlankLine(t *testing.T) {
	f := twoSessions()
	lines := []string{"one real trailing newline", ""}
	f.setMultilineComposer("%2", lines)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer ending in a blank row published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a composer ending in one blank row: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countBackspaces(f.callsSnapshot()); got < 1 {
		t.Error("no Backspace was sent for a single trailing blank row (muster#132)")
	}
}

// An INTERIOR blank row — not the last row overall, but the row the cursor
// currently sits on partway through the pass — needs the identical crossing
// once the pass walks back to it. Three rows, the middle one already blank:
// the pass clears the real bottom row first (ordinary C-u), then reaches the
// blank one and must cross it with Backspace before the top row's real
// content is reachable at all.
func TestDiscardCrossesAnInteriorBlankLine(t *testing.T) {
	f := twoSessions()
	lines := []string{"top row, real content", "", "bottom row, real content"}
	f.setMultilineComposer("%2", lines)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer with an interior blank row published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a composer with an interior blank row: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countBackspaces(f.callsSnapshot()); got < 1 {
		t.Error("no Backspace was sent for an interior blank row (muster#132)")
	}
	if got := countClears(f.callsSnapshot()); got < 2 {
		t.Errorf("sent %d C-u presses; both real-content rows still need one each", got)
	}
}

// "Several" trailing blank rows, not just #132's own two — the test matrix
// #132 asks for explicitly ("one, two, several"). Each blank row needs its
// own Backspace; none of them should ever see a C-u.
func TestDiscardCrossesSeveralTrailingBlankLines(t *testing.T) {
	f := twoSessions()
	lines := []string{"real content, then a run of blank rows", "", "", "", ""}
	f.setMultilineComposer("%2", lines)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer ending in several blank rows published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a composer ending in several blank rows: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countBackspaces(f.callsSnapshot()); got < 4 {
		t.Errorf("sent %d Backspace presses; four trailing blank rows each need one "+
			"(muster#132), got %d", got, got)
	}
}

// muster#133: "a single line with no trailing newline at all" — the
// control case the rest of this matrix is a deviation FROM. No blank row
// exists anywhere, so this is a plain single-press C-u clear, and Backspace
// must never be sent — pinned explicitly rather than only implied by the
// shapes around it.
func TestDiscardClearsASingleLineWithNoTrailingNewline(t *testing.T) {
	f := twoSessions()
	f.setMultilineComposer("%2", []string{"a single line, no trailing newline at all"})

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a single-line composer published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a single line with no trailing newline: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countBackspaces(f.callsSnapshot()); got != 0 {
		t.Errorf("sent %d Backspace presses against a composer with no blank row at all; "+
			"none was ever needed", got)
	}
	if got := countClears(f.callsSnapshot()); got < 1 {
		t.Error("no C-u was sent for the one real content row")
	}
}

// muster#133 §1: "a leading blank row ahead of the only remaining
// content row" — believed safe during #132 ("it collapses into the existing
// 'blank rows read as already-empty' behaviour before the new blank-row check
// is ever consulted") but never pinned. composerCursorRowBlank only ever
// inspects the composer's BOTTOM row (see its own doc comment); a blank row
// sitting ABOVE the sole remaining content row is never the row being
// inspected, so an ordinary C-u against the real content below it is what
// clears this — and once that lands, the composer has shrunk to its own bare
// ❯-marked line, which composerText already treats as empty (see
// composerCursorRowBlank's doc comment on that one-row special case) without
// Backspace ever having to cross anything.
func TestDiscardCrossesALeadingBlankRowAheadOfTheOnlyRemainingContentRow(t *testing.T) {
	f := twoSessions()
	f.setMultilineComposer("%2", []string{"", "the only remaining content"})

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a composer with a leading blank row published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a composer with a leading blank row: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countBackspaces(f.callsSnapshot()); got != 0 {
		t.Errorf("sent %d Backspace presses; a leading blank row ahead of the sole content row "+
			"should clear via an ordinary C-u on the content row, never needing to cross the "+
			"blank row directly (muster#133 §1)", got)
	}
	if got := countClears(f.callsSnapshot()); got < 1 {
		t.Error("no C-u was sent for the one real content row")
	}
}

// muster#133 §1: "whitespace-only payload". composerText already trims
// per line (pinned directly against classify.go by
// TestWhitespaceOnlyComposerIsEmpty); this exercises the SAME shape through
// Discard end to end, closing the "believed unaffected, not exercised end to
// end" gap #132 left open. A composer holding only whitespace reads as
// already empty, so Discard's own early return fires — success, and not one
// keystroke is spent proving it.
func TestDiscardAcceptsAWhitespaceOnlyComposerWithoutTouchingThePane(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = rule + "\n❯      \n" + rule

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Discard's early return fires before expectDigest is ever consulted (an
	// already-empty composer needs no corroboration to leave alone), so no
	// real digest is required to prove this path.
	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "irrelevant", driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a whitespace-only composer: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a whitespace-only composer, which is already empty content-wise")
	}
	// Discard's own enumeration (finding the live session, reading its
	// screen) still runs — this asserts no CLEAR keystroke was ever sent,
	// not that the pane was never read at all.
	if got := countClears(f.callsSnapshot()) + countBackspaces(f.callsSnapshot()); got != 0 {
		t.Errorf("sent %d clear keystrokes against a composer already read as empty; want 0", got)
	}
}

// Every shape above models a "content row" as a single line. A real pasted
// message rarely is: a paragraph of prose wraps onto several visual rows
// before the next blank-line break, so a multi-paragraph paste — someone
// pasting an email, or several separate thoughts typed with blank lines
// between them — leaves MULTIPLE non-blank rows standing between each blank
// separator, not one. This is the shape none of the single-row fixtures
// above can stand in for: it asks whether the blank/non-blank key choice
// keeps alternating correctly across repeated multi-row-then-blank cycles,
// not just a single blank row bounded by single content rows on each side.
//
// Three paragraphs (2, 3, and 1 rows respectively), two separate blank
// separators between them — deliberately more than #132's own interior-blank
// case (one blank row, one content row on each side) and more than any
// trailing-run case (one paragraph, several blanks).
func TestDiscardCrossesMultipleParagraphBreaksInAMultiRowPaste(t *testing.T) {
	f := twoSessions()
	lines := []string{
		"first paragraph, line one",
		"first paragraph, line two",
		"",
		"second paragraph, line one",
		"second paragraph, line two",
		"second paragraph, line three",
		"",
		"third paragraph, a single line",
	}
	f.setMultilineComposer("%2", lines)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a multi-paragraph composer published no composerDigest")
	}

	// A hang-guard only, generous next to the ~8 presses (6 content rows + 2
	// separators) this needs.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a multi-paragraph composer: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false on a successful clear")
	}
	if got := countClears(f.callsSnapshot()); got < 6 {
		t.Errorf("sent %d C-u presses; six real content rows across three paragraphs each "+
			"need one, got %d", got, got)
	}
	if got := countBackspaces(f.callsSnapshot()); got < 2 {
		t.Errorf("sent %d Backspace presses; two separate paragraph-break blank rows each "+
			"need one to cross (muster#132/#133), got %d", got, got)
	}
}

// --- muster#133 follow-up: the press budget itself -------------------
//
// A separate reviewing pass over the same issue found the one area the
// matrix above never touched: `clearComposer`'s press budget
// (expectedLines + clearPressMargin, capped at maxClearPresses — #129) treats
// a blank row's Backspace and a content row's C-u as costing the identical
// one press each (ADR 132: "no third key, no changed budget shape"). Nothing
// asserted that holds when blank rows dominate, nothing covered what happens
// when the budget is not enough, and nothing checked that #132's new
// row-count progress signal cannot mask a genuine #87 stall.

// Budget fits when blank rows are the MAJORITY of the composer, not the
// minority every shape above tested. The margin (clearPressMargin = 5) is a
// fixed constant regardless of composer size, so this is "just inside the
// budget" for any row count — if a blank row ever cost more than the one
// press a content row costs, a blank-dominated composer is exactly the shape
// that would first run the margin out.
func TestDiscardClearsWithinBudgetWhenBlankRowsDominate(t *testing.T) {
	f := twoSessions()
	lines := []string{
		"real content, row 1", "", "",
		"real content, row 2", "", "",
		"real content, row 3", "",
	}
	f.setMultilineComposer("%2", lines)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a blank-dominated composer published no composerDigest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ack, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("discard of a blank-dominated composer: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false; a blank-dominated composer must still clear fully within " +
			"expectedLines+clearPressMargin, the same one-press-per-row budget any other shape gets")
	}
	if got := countClears(f.callsSnapshot()); got < 3 {
		t.Errorf("sent %d C-u presses; three real content rows each need one, got %d", got, got)
	}
	if got := countBackspaces(f.callsSnapshot()); got < 5 {
		t.Errorf("sent %d Backspace presses; five blank rows — the composer's own majority — "+
			"each need one, got %d", got, got)
	}
}

// Budget exhaustion is graceful, not silent success and not a wedge.
// maxClearPresses (120) is a hard ceiling independent of composer size — a
// composer taller than that cannot possibly clear in one pass, by
// construction, regardless of how correctly clearComposer behaves. What
// matters is what a caller is told, and whether a second attempt (with a
// freshly read digest) can still make progress — the #132 field case was
// exactly a partial clear that left NO way back; this proves this shape
// still has one.
func TestDiscardStopsGracefullyWhenTheBudgetIsNotEnough(t *testing.T) {
	f := twoSessions()
	lines := make([]string, maxClearPresses+10)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %d of a paste taller than even the capped budget", i)
	}
	f.setMultilineComposer("%2", lines)

	// Real clock: the press loop's own select waits on a real timer between
	// presses, same reason every other multi-press test here uses it.
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}

	// Generous next to the ~24s the capped 120 presses need at
	// promptClearInterval apart.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	_, err = d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a composer taller than the capped budget must not report success on the first pass")
	}
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("kind: got %v, want ErrAmbiguousTarget", err)
	}
	if !strings.Contains(err.Error(), "damaged") {
		t.Errorf("message = %q; a partial clear that changed but did not empty is the same "+
			"\"damaged\" shape as any other changed-but-nonempty residue, not a new kind of failure", err.Error())
	}
	if strings.Contains(err.Error(), "unchanged") {
		t.Errorf("message = %q; real progress was made (120 rows cleared), so this must not "+
			"read as the untouched-composer shape", err.Error())
	}
	if got := countClears(f.callsSnapshot()); got != maxClearPresses {
		t.Errorf("sent %d C-u presses, want exactly maxClearPresses (%d) — the pass must stop "+
			"AT the cap, neither short of it (still genuinely moving) nor past it (the cap not "+
			"actually enforced)", got, maxClearPresses)
	}

	// The part #132 was actually filed over: is there a way back? Read the
	// session fresh — the composer now holds only the top 10 rows the capped
	// pass never reached — and confirm a SECOND call, with the fresh digest,
	// is not refused as futile and completes the clear. A permanently wedged
	// composer would refuse this exactly the way discardProvenFutile does;
	// noteFutile is only ever recorded when a pass makes NO progress at all
	// (moved=false), and this pass moved 120 rows' worth, so it must not have
	// been recorded here.
	col2, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest2 string
	for _, s := range col2.Items() {
		if s.ID == "beta" {
			digest2 = s.State.ComposerDigest
		}
	}
	if digest2 == "" {
		t.Fatal("re-read: no composerDigest published for the remaining residue")
	}
	if digest2 == digest {
		t.Fatal("re-read: digest unchanged after 120 presses — setup did not make progress")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	ack2, err := d.Discard(ctx2, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest2, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("second discard, against the fresh digest of what the capped pass left behind: %v — "+
			"this is exactly muster#132's own wedge if it fails", err)
	}
	if !ack2.Accepted {
		t.Error("accepted = false on the second pass; the remaining 10 rows fit well inside a " +
			"fresh budget and should have cleared fully")
	}
}

// Adversarial: #132 added "the composer's own visual row count changed" as a
// SECOND progress signal alongside "the joined text changed" — either one
// resets #87's stall counter (clearComposer's own doc comment). This
// constructs a composer where genuine row-count progress (several blank rows
// actually being crossed) happens FIRST, immediately ahead of a floor of
// real content that then never moves again — checking that reaching the
// floor still accumulates stallPresses (3) and breaks early, rather than the
// earlier row-count movement's `moved=true` state letting the loop coast on
// stale "progress" to the full press budget.
func TestDiscardStallDetectionSurvivesPriorRowCountProgress(t *testing.T) {
	f := twoSessions()
	// Bottom-up press order (the fake pops from the end of this slice):
	// "row F" real content, blank, "row E" real content, blank, "row D" real
	// content — five presses of genuine progress, two of them via Backspace
	// crossing a blank row — landing exactly on the 3-line floor below. Every
	// press against the floor is then a complete no-op, on both signals at
	// once (the fake's floor check runs before its blank-row check, so
	// nothing here is blank-row-shaped once the floor is reached).
	lines := []string{
		"stuck 1", "stuck 2", "stuck 3",
		"real content D", "",
		"real content E", "",
		"real content F",
	}
	f.setMultilineComposer("%2", lines)
	f.setComposerFloor("%2", 3)

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a composer that stalls above empty must not report success, prior row-count " +
			"progress notwithstanding")
	}
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("kind: got %v, want ErrAmbiguousTarget", err)
	}
	if !strings.Contains(err.Error(), "damaged") {
		t.Errorf("message = %q; real progress was made before the stall, so this is the same "+
			"damaged shape TestDiscardStopsPressingOnceTheComposerStopsMoving already covers "+
			"for a floor with no blank rows ahead of it", err.Error())
	}

	// 5 real presses (2 of them Backspace, crossing the two blank rows) to
	// reach the floor, then exactly stallPresses (3) more once every press
	// there is a no-op on both signals — 8 total. This is the number that
	// PROVES the row-count signal did not paper over the floor: if it had,
	// the loop would instead have run the full expectedLines(8)+
	// clearPressMargin(5) = 13-press budget, because "moved" from the
	// earlier real progress never resets to false on its own.
	want := 5 + stallPresses
	if got := countClears(f.callsSnapshot()) + countBackspaces(f.callsSnapshot()); got != want {
		t.Errorf("sent %d clear keystrokes, want exactly %d (5 presses of real progress — 3 "+
			"content rows plus 2 blank-row crossings — then stallPresses more once the floor "+
			"stops moving). If this is HIGHER, the row-count signal from the earlier blank-row "+
			"crossings is masking the stall and letting the loop run past it toward the full "+
			"budget — that would be a genuine finding to file as its own issue, not a reason to "+
			"widen this assertion", got, want)
	}
}

// muster#133 §1's "three caller paths" ask: clearComposer's row-level
// key choice (this whole matrix's real subject) is shared code, already
// exhaustively covered above through Discard — muster#112's own
// extraction is what keeps Discard and replaceIfStranded from being able to
// drift apart on it. What differs PER CALLER, and was untested until now (no
// test anywhere exercised replaceIfStranded at all, blank row or not), is the
// plumbing feeding that shared function: whether a blank row inside a
// composer THIS DRIVER stranded gets crossed the same way when reached
// through Send's replaceIfStranded path instead of Discard's own endpoint.
func TestReplaceIfStrandedCrossesATrailingBlankLine(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true // the first attempt's own submit keystroke never registers
	// The REAL clock, deliberately not newTestDriver's frozen test constant:
	// clearComposer's press loop waits out promptClearInterval on a real
	// time.After between presses (see TestDiscardClearsAComposerBeyondTheOld
	// ThreeSecondBudget's identical note), and d.bounded's deadline is
	// computed from d.now() — a frozen historical clock makes that deadline
	// already past real wall-clock time, so ctx.Err() fires after the very
	// first press and the pass never gets a second one.
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	first, err := d.Send(context.Background(), testCaller, ref, "original", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: first send outcome = %q, want unknown so the text is recorded as stranded",
			first.Outcome)
	}

	// The composer now holds this driver's own stranded delivery, plus a real
	// trailing blank row — the #132 shape. composerText drops the blank row
	// from what it joins, so this still corroborates against the record's
	// digest exactly (screenDigest of the trimmed, joined text).
	f.setMultilineComposer("%1", []string{"original", ""})
	f.swallowSubmit = false

	got, err := d.Send(context.Background(), testCaller, ref, "replacement text",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("replaceIfStranded refused instead of crossing the blank row: %s", got.Reason)
	}
	if n := countBackspaces(f.callsSnapshot()); n < 1 {
		t.Error("no Backspace was sent to cross the trailing blank row reached via " +
			"replaceIfStranded (muster#132/#133) — only Discard's own endpoint was " +
			"ever proven to do this")
	}
}

// Issue #32, the branch that did not exist at all: a clear that runs out of
// time without emptying the composer covers two situations that demand
// opposite handling, and nothing told them apart. This is the untouched
// half — the keystroke never registered, so the composer is byte-for-byte
// what the caller already corroborated, and retrying with the same digest
// is safe.
//
// Against the pre-fix Discard this fails twice over: the returned error is
// a bare errors.New, not wrapped in ErrAmbiguousTarget, so it is reported
// as invalid (400, the caller's fault) rather than conflict (409); and its
// message never says the text is intact, so a caller has no way to know a
// retry is safe.
func TestDiscardReportsAnUntouchedComposerDistinctlyAndSafely(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	f.freezeComposer("%2") // the clear keystroke never reaches this pane at all

	// The real clock, deliberately not the file's usual frozen test
	// constant: the poll loop's own select waits on a REAL time.After
	// between presses regardless of d.now(), and bounded() computes its
	// outer deadline from d.now() — mixing a frozen driver clock with a
	// real-time context there produces an already-expired context the
	// moment the frozen constant is older than the wall clock, which
	// silently corrupts any test exercising more than one poll iteration.
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}

	_, err = d.Discard(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a frozen pane must not report success")
	}
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("kind: got %v, want an ErrAmbiguousTarget (conflict, not the caller's "+
			"fault) — a driver-side execution failure must not be reported as invalid", err)
	}
	if !strings.Contains(err.Error(), "unchanged") || !strings.Contains(err.Error(), "safe") {
		t.Errorf("message = %q; an untouched composer must say the text is intact and a "+
			"same-digest retry is safe, not just that it %q", err.Error(), "did not clear")
	}
}

// muster #124: Discard's own 409 should carry the same restart-
// correlation fact State/List already surface (stampSinceLocked's "age
// carried from before this service restarted"), instead of making an
// operator cross-reference a separate read by hand — which is exactly what
// #124's field report spent two follow-up comments doing.
//
// Simulated the same way TestResumeIntentSurvivesARestart does: a second
// Driver built over the same on-disk state store stands in for the process
// this service restarts as part of its own deploy. The first driver's List
// call is what persists the waiting_input status/record to that store; the
// second driver never calls List itself before Discard, so its own
// in-memory d.observed has nothing for this session and it must fall back
// to the persisted record alone — the exact path restoredWaitingInputSince
// exists for.
func TestDiscardNotesWhenTheUnsentInputStatusPredatesTheService(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	f.freezeComposer("%2") // the clear keystroke never reaches this pane at all

	newDriver := func() *Driver {
		st, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		// Real clock, deliberately: see the sibling untouched/damaged Discard
		// tests in this file for why — the clear loop's own select waits on a
		// real timer between presses regardless of d.now().
		return New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
			withClock(time.Now), WithState(st))
	}

	first := newDriver()
	col, err := first.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}

	// The restart. Nothing about the state store or the multiplexer
	// changed — only the driver process.
	second := newDriver()

	_, err = second.Discard(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a frozen pane must not report success")
	}
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("kind: got %v, want an ErrAmbiguousTarget (conflict, not the caller's "+
			"fault) — a driver-side execution failure must not be reported as invalid", err)
	}
	if !strings.Contains(err.Error(), "carried from before this service restarted") {
		t.Errorf("message = %q; a Discard failure against a session whose waiting_input "+
			"status predates this service's current process must say so, the same fact "+
			"State/List already carry in their own evidence line", err.Error())
	}
}

// Issue #113: discardIncomplete's first-occurrence ("unchanged") message must
// not read as an unconditional promise, because the very next identical call
// can land on discardProvenFutile and say the opposite. The fix is wording
// only — this test pins the bounded phrasing so it cannot regress back to an
// unqualified "safe".
func TestDiscardIncompleteFirstMessageBoundsItsOwnSafetyPromise(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	f.freezeComposer("%2")

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}

	_, err = d.Discard(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a frozen pane must not report success")
	}
	if !strings.Contains(err.Error(), "retrying once more") {
		t.Errorf("message = %q; the first-occurrence message must bound the safety "+
			"claim to a single retry, not read as an unconditional promise", err.Error())
	}
	if !strings.Contains(err.Error(), "if it is still unchanged after that retry") {
		t.Errorf("message = %q; the first-occurrence message must say what to do if "+
			"the bounded retry also fails, since the next identical call can land on "+
			"discardProvenFutile and contradict an unqualified promise", err.Error())
	}
}

// Issue #32, the branch's other half, and the worse of the two: the
// keystroke ran and did SOMETHING, but the composer that is left is neither
// what the caller saw nor empty. A caller told only "not cleared" cannot
// tell this apart from the untouched case above, and the two demand
// opposite next steps — one is safe to retry as-is, this one is not.
//
// Against the pre-fix Discard this fails the same two ways as the untouched
// test: unwrapped error (mapped to invalid, not conflict) and a message
// that never says the composer is now damaged.
//
// muster#129 retargeted HOW this state is reached, not what it asserts.
// Before #129, "damaged" was reached by outrunning promptClearWindow's flat
// 3-second clock with a composer long enough that the clock, not the
// composer, was the limiting factor — a shape #129 explicitly removed:
// clearComposer's press budget is content-derived now, so a composer merely
// LONG (as opposed to genuinely stuck) clears in full regardless of line
// count, up to maxClearPresses. What still legitimately lands a composer in
// "damaged" is #87's stall — real progress, then none — so this uses the
// same fixture shape TestDiscardStopsPressingOnceTheComposerStopsMoving
// does, with different numbers, and pins the MESSAGE this state must
// produce rather than that test's press-count assertions.
func TestDiscardReportsADamagedComposerDistinctlyAndUnsafely(t *testing.T) {
	f := twoSessions()
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d of an unsent message that never got resent", i)
	}
	f.setMultilineComposer("%2", lines)
	f.setComposerFloor("%2", 4) // real progress (10 -> 4), then genuinely stops

	// See the sibling untouched test for why this is the real clock and not
	// the file's usual frozen one.
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}

	_, err = d.Discard(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a composer that never reaches empty must not report success")
	}
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("kind: got %v, want an ErrAmbiguousTarget (conflict, not the caller's "+
			"fault) — a driver-side execution failure must not be reported as invalid", err)
	}
	if !strings.Contains(err.Error(), "damaged") {
		t.Errorf("message = %q; a partially cleared composer must say so — this is the "+
			"outcome a caller cannot tell apart from success or untouched without it", err.Error())
	}
	if strings.Contains(err.Error(), "safe") {
		t.Errorf("message = %q; a damaged composer must NOT read as safe to retry — the "+
			"digest guarding it has already moved", err.Error())
	}
	// At least one line must have actually gone, distinguishing this from
	// the untouched case: the keystroke partially ran.
	if got := countClears(f.callsSnapshot()); got == 0 {
		t.Error("no clear keystroke was even attempted")
	}
}

// #87: a clear pass that made real progress and then genuinely stopped
// must not keep pressing C-u for the rest of the 3s window — every press
// past the stall is a destructive keystroke aimed at text nobody has
// re-read, for zero further effect. Six lines, floor 3: three presses make
// real progress, then the pane holding the fixture's "moved" model stops
// moving. `stallPresses` more presses are tolerated to tell "stopped" apart
// from "one slow repaint", and no more.
func TestDiscardStopsPressingOnceTheComposerStopsMoving(t *testing.T) {
	f := twoSessions()
	lines := []string{"one", "two", "three", "four", "five", "six"}
	f.setMultilineComposer("%2", lines)
	f.setComposerFloor("%2", 3)

	// Real clock: see the sibling untouched/damaged tests for why — the
	// select between presses waits on a real timer regardless of d.now().
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a composer that stops moving above empty must not report success")
	}
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("kind: got %v, want ErrAmbiguousTarget", err)
	}
	if !strings.Contains(err.Error(), "damaged") {
		t.Errorf("message = %q; a floor above empty is the damaged shape, same as issue #32's", err.Error())
	}
	if strings.Contains(err.Error(), "safe") {
		t.Errorf("message = %q; a damaged composer must not read as safe to retry", err.Error())
	}
	got := countClears(f.callsSnapshot())
	if got < 3 {
		t.Errorf("sent %d clear keystrokes; the fixture makes 3 lines of real progress "+
			"before stalling, so fewer than that means the loop gave up before it should", got)
	}
	if got > 3+stallPresses {
		t.Errorf("sent %d clear keystrokes against a composer that stopped moving after 3 "+
			"presses; the loop should have stopped within stallPresses (%d) more instead of "+
			"spending the rest of the 3s window on presses already proven to do nothing",
			got, stallPresses)
	}
}

// #87's core liveness fix: a residue already proven — by a full, exhausted
// pass — not to move must not be told "safe to retry" again. The FIRST
// call against a frozen pane gets the existing honest message (nothing was
// destroyed, first time seeing this). The SECOND call, same digest because
// nothing moved, must not repeat that promise, and — the actual convergence
// proof, not just wording — must not press C-u again at all: pressing a
// second identical, already-exhausted pass would just be more of the same
// destructive keystrokes for the same zero effect.
func TestDiscardStopsPromisingASafeRetryOnceItHasProvedFutile(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	f.freezeComposer("%2")

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("setup: no composerDigest published")
	}
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	// context.Background(), not a short WithTimeout: this call needs its
	// whole content-derived press budget to run out, exactly like the
	// sibling ReportsAnUntouchedComposer test — a caller-supplied deadline
	// shorter than that races the internal ctx.Done() case and returns a
	// bare "context deadline exceeded" instead of exercising this path.
	_, err1 := d.Discard(context.Background(), testCaller, ref, digest, driver.DiscardOptions{})
	if err1 == nil {
		t.Fatal("a frozen pane must not report success on the first call")
	}
	if !strings.Contains(err1.Error(), "unchanged") || !strings.Contains(err1.Error(), "safe") {
		t.Errorf("first call message = %q; a genuinely first-time unmoved composer "+
			"must still read as unchanged and safe to retry", err1.Error())
	}
	before := countClears(f.callsSnapshot())
	if before == 0 {
		t.Fatal("setup: the first call never pressed C-u at all")
	}

	// This call must be refused BEFORE pressing anything, so it returns
	// immediately — a generous hang-guard here only catches a regression
	// back to the old behaviour, it is never expected to fire.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_, err2 := d.Discard(ctx2, testCaller, ref, digest, driver.DiscardOptions{})
	if err2 == nil {
		t.Fatal("a residue already proven futile must not report success either")
	}
	if !errors.Is(err2, fleet.ErrAmbiguousTarget) {
		t.Errorf("kind: got %v, want ErrAmbiguousTarget", err2)
	}
	if strings.Contains(err2.Error(), "safe") {
		t.Errorf("second call message = %q; must not promise a retry is safe/worth doing "+
			"once a full pass already proved this exact residue will not move", err2.Error())
	}
	if strings.Contains(err2.Error(), "unchanged") {
		t.Errorf("second call message = %q; must read as a genuinely different outcome, "+
			"not a repeat of the first call's wording", err2.Error())
	}
	after := countClears(f.callsSnapshot())
	if after != before {
		t.Errorf("second call sent %d new clear keystrokes; a residue already proven "+
			"futile must be refused BEFORE pressing, not re-learn the same lesson", after-before)
	}
}

// muster#136: discardProvenFutile's message used to terminate in "close
// the session (DELETE ...)" as the ONLY remedy it named. This pins that it no
// longer does — the grep-able assertion the issue itself asks for — and that
// the remedy it names now is force, not destruction.
func TestDiscardProvenFutileNamesForceNotDestruction(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	f.freezeComposer("%2")

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	if _, err := d.Discard(context.Background(), testCaller, ref, digest, driver.DiscardOptions{}); err == nil {
		t.Fatal("setup: expected the frozen pane to refuse on the first call")
	}

	_, err = d.Discard(context.Background(), testCaller, ref, digest, driver.DiscardOptions{})
	if err == nil {
		t.Fatal("a residue already proven futile must still refuse without force")
	}
	if !strings.Contains(err.Error(), "force=true") {
		t.Errorf("message = %q; must name force=true as the available remedy", err.Error())
	}
	if !strings.Contains(err.Error(), "should not be needed") {
		t.Errorf("message = %q; must say closing the session is no longer the "+
			"documented next step for a stuck composer alone", err.Error())
	}
}

// muster#136: force is the escape hatch — once the ordinary pass has
// already been proven futile against this exact residue, a retry WITH
// force must reach clearComposerSweep instead of being refused outright,
// and (given a pane that responds to Backspace at all, unlike the fully
// frozen case above) actually clear it.
func TestForceReachesTheSweepAfterFutilityIsProven(t *testing.T) {
	f := twoSessions()
	lines := []string{"residue that the ordinary pass never got a chance to move"}
	f.setMultilineComposer("%2", lines)
	f.freezeComposer("%2") // force the FIRST pass to fail and record futility

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	if _, err := d.Discard(context.Background(), testCaller, ref, digest, driver.DiscardOptions{}); err == nil {
		t.Fatal("setup: expected the frozen pane to refuse on the first call")
	}
	if _, err := d.Discard(context.Background(), testCaller, ref, digest, driver.DiscardOptions{}); err == nil {
		t.Fatal("setup: expected the SECOND call to refuse too (proven futile, no force yet)")
	}
	before := len(f.callsSnapshot())

	// The pane now responds normally — modelling "the ordinary structural
	// pass could not read this residue's shape correctly, but a blind
	// character sweep still reaches it once the caller has asked for one."
	f.unfreezeComposer("%2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := d.Discard(ctx, testCaller, ref, digest, driver.DiscardOptions{Force: true})
	if err != nil {
		t.Fatalf("force should have reached the sweep and cleared: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false; the sweep should have converged once the pane responded")
	}

	var sawSweepBackspaces, sawStructuralKeys bool
	for _, call := range f.callsSnapshot()[before:] {
		if len(call) == 0 || call[0] != "send-keys" {
			continue
		}
		for _, a := range call {
			if a == "BSpace" {
				sawSweepBackspaces = true
			}
			if a == "C-u" {
				sawStructuralKeys = true
			}
		}
	}
	if !sawSweepBackspaces {
		t.Errorf("the forced call never sent a Backspace sweep: %v", f.callsSnapshot()[before:])
	}
	if sawStructuralKeys {
		t.Errorf("the forced call sent C-u; the sweep must be a distinct, purely-Backspace "+
			"mechanism, not a re-run of the structural pass: %v", f.callsSnapshot()[before:])
	}
}

// muster#136: force is not a licence to skip corroboration. It changes
// what happens ONCE a matching digest has already been proven futile, not
// whether a digest is required at all — a caller cannot use force to
// discard blind, or to discard against a composer that has changed since
// they last read it.
func TestForceDoesNotRelaxTheDigestRequirement(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	t.Run("no digest at all", func(t *testing.T) {
		_, err := d.Discard(context.Background(), testCaller, ref, "", driver.DiscardOptions{Force: true})
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("force must not turn a blind discard into permission; got %v", err)
		}
	})

	t.Run("a stale digest", func(t *testing.T) {
		_, err := d.Discard(context.Background(), testCaller, ref, "not-the-real-digest", driver.DiscardOptions{Force: true})
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("force must not corroborate a digest the caller did not actually see; got %v", err)
		}
	})
}

// #87: a residue that moves — even to something still non-empty — is
// evidence the earlier "would not move" record no longer describes this
// composer. The next call, against the FRESH digest that produced, must
// press again rather than being blocked by a futile record made for a
// different piece of text.
func TestDiscardForgetsFutilityWhenTheComposerMoves(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	f.freezeComposer("%2")

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest1 string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest1 = s.State.ComposerDigest
		}
	}
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	// context.Background(): the frozen composer needs its whole
	// content-derived press budget to run out before reporting "unchanged"
	// — see the sibling ProvedFutile test's comment on why a short
	// WithTimeout races the internal ctx.Done() case here.
	if _, err := d.Discard(context.Background(), testCaller, ref, digest1, driver.DiscardOptions{}); err == nil {
		t.Fatal("setup: expected the frozen pane to refuse on the first call")
	}

	// The composer changes underneath — someone typed, or a later escape
	// hatch cleared part of it. Either way it is no longer the residue the
	// futile record above describes.
	f.mu.Lock()
	delete(f.frozen, "%2")
	f.mu.Unlock()
	f.setMultilineComposer("%2", []string{"a brand new unsent message"})

	col2, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest2 string
	for _, s := range col2.Items() {
		if s.ID == "beta" {
			digest2 = s.State.ComposerDigest
		}
	}
	if digest2 == "" || digest2 == digest1 {
		t.Fatalf("setup: expected a fresh digest for the fresh composer, got %q (was %q)", digest2, digest1)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	ack, err := d.Discard(ctx2, testCaller, ref, digest2, driver.DiscardOptions{})
	if err != nil {
		t.Fatalf("a fresh residue must get a fresh pass, not be blocked by a stale futile "+
			"record: %v", err)
	}
	if !ack.Accepted {
		t.Error("accepted = false clearing a single-line composer that was never frozen")
	}
}

// #87: futility is corroborated on cwd, not id alone (§5.4) — the same rule
// strandedMatches already applies. An id that gets recycled onto a
// different session's pane must not inherit a futile record made for the
// session that used to hold that id.
func TestDiscardFutilityIsCorroboratedByCwd(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	f.freezeComposer("%2")

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	// context.Background(): see the ProvedFutile test's comment — both
	// calls below need their whole content-derived press budget to run,
	// since the composer stays frozen throughout and neither is expected to
	// be blocked before pressing (the second is deliberately NOT
	// proven-futile-blocked here, that is the property under test).
	if _, err := d.Discard(context.Background(), testCaller, ref, digest, driver.DiscardOptions{}); err == nil {
		t.Fatal("setup: expected the frozen pane to refuse on the first call")
	}
	before := countClears(f.callsSnapshot())

	// Simulate the id being recycled onto a different session: same id,
	// different working directory, same (still-frozen) composer text so the
	// digest is unchanged — the one case only the cwd check can catch.
	f.mu.Lock()
	for i := range f.sessions {
		if f.sessions[i].paneID == "%2" {
			f.sessions[i].cwd = "/work/a-different-checkout"
		}
	}
	f.mu.Unlock()

	if _, err := d.Discard(context.Background(), testCaller, ref, digest, driver.DiscardOptions{}); err == nil {
		t.Fatal("a still-frozen composer must not report success")
	}
	after := countClears(f.callsSnapshot())
	if after == before {
		t.Error("a recycled id with a different cwd must get its own fresh pass, not be " +
			"blocked by a futile record made for the previous occupant of that id")
	}
}

// #3: send's own safety refusal used to be a dead end. It delivers, cannot
// confirm, and says the text is sitting there — after which a second send is
// refused by the very rule that protects the text, and nothing else submits.
func TestResumeSubmitsOnlyWhatThisDriverStranded(t *testing.T) {
	ctx := context.Background()

	t.Run("resume completes our own stranded delivery", func(t *testing.T) {
		f := twoSessions()
		f.noEcho = true // the pane never renders the paste, so confirm times out
		d := newTestDriver(f)

		r1, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"the long message", driver.SendOptions{Submit: true})
		if err != nil {
			t.Fatal(err)
		}
		if r1.Outcome != fleet.OutcomeUnknown {
			t.Fatalf("expected an unconfirmed delivery, got %s", r1.Outcome)
		}

		// The text is now visibly in the composer.
		f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ the long message\n"+rule+"\n")
		f.noEcho = false

		r2, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"the long message", driver.SendOptions{Submit: true, ResumeIfStranded: true})
		if err != nil {
			t.Fatal(err)
		}
		// #101: a confirmed resume reports the same outcome the first-attempt
		// path would for the same evidence — queued, never submitted, since
		// §4.3's ConfirmsDelivery is false on this substrate either way.
		if r2.Outcome != fleet.OutcomeQueued {
			t.Errorf("resume outcome = %s (%s), want queued", r2.Outcome, r2.Reason)
		}
	})

	// muster #135: this used to be the dead end this whole function's own
	// doc comment describes — no record backing the composer's content meant
	// an unconditional refusal regardless of either flag, "close to an hour"
	// per #135's field report once the 30-minute strandedRetention window
	// lapsed mid-retry. #135 originally folded BOTH flags' "no record" case
	// into the identical clear-and-deliver door; a later review fix (see
	// stranded_test.go's TestResumeIfStrandedRefusesWithNoStrandedRecord)
	// narrowed that to replaceIfStranded alone — resumeIfStranded's own
	// contract ("finish MY earlier delivery") has nothing to finish here and
	// now refuses instead of guessing. This subtest is updated to exercise
	// the door that still exists (replaceIfStranded), and keeps proving the
	// same safety property it always did: this driver must never submit text
	// it did not place. The FOREIGN text sitting in the composer is
	// discarded, never delivered; only the caller's own new text may reach
	// the pane. See stranded_test.go's dedicated #135 tests for the rest of
	// the new behaviour (the #87 futile guard, a partial clear reported
	// honestly, and a bare send with neither flag set still refusing with
	// the original wording).
	t.Run("replace clears foreign text and submits only what this driver placed", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = fixtureUnsent // a human's typing, nothing to do with us
		d := newTestDriver(f)

		r, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"},
			"something else entirely", driver.SendOptions{Submit: true, ReplaceIfStranded: true,
				ExpectComposerDigest: composerTextDigest(pendingOf(t, fixtureUnsent))})
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != fleet.OutcomeQueued {
			t.Errorf("outcome = %s (%s), want queued — #135: no record means nothing to resume, "+
				"so replaceIfStranded clears the composer and delivers this call's own text instead",
				r.Outcome, r.Reason)
		}
		// The ordinary delivery path this falls through to always pastes THIS
		// call's own `text` parameter — never `pending`, the composer content
		// just cleared — so there is no code path here that could resubmit
		// the foreign text; a confirmed Queued (not Unknown) is itself
		// evidence the marker-based confirmation attributed OUR paste, not a
		// leftover of the foreign one.
	})

	t.Run("resume with different text is refused", func(t *testing.T) {
		f := twoSessions()
		f.noEcho = true
		d := newTestDriver(f)
		if _, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"the original", driver.SendOptions{Submit: true}); err != nil {
			t.Fatal(err)
		}
		f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ the original\n"+rule+"\n")
		f.noEcho = false

		r, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"NOT the original", driver.SendOptions{Submit: true, ResumeIfStranded: true})
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != fleet.OutcomeRefused {
			t.Errorf("outcome = %s, want refused — resume finishes one delivery, it does not start another", r.Outcome)
		}
	})
}

// A usage limit belongs to the account and lasts days; the notice is on screen
// for seconds. Measured on a live fleet: 51 sessions, the notice visible in 2
// panes, 0 reported blocked, 48 reporting idle — the status that means "send it
// work" — while the account had four days left to run.
func TestQuotaBlockOutlivesTheNoticeThatAnnouncedIt(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	// beta shows the limit; alpha looks perfectly idle, as most panes do.
	f.captures["%2"] = "transcript\nYou've hit your weekly limit · resets Aug 10 at 12am\n" +
		rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status != fleet.StatusQuotaBlocked {
			t.Errorf("%s reported %s; every session on a refusing account is blocked",
				s.ID, s.State.Status)
		}
	}

	// The notice scrolls away, which is what actually happened.
	f.setCapture("%2", idleFixtureFor("beta"))
	col, err = d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status != fleet.StatusQuotaBlocked {
			t.Errorf("%s went back to %s once the notice scrolled away — the block is remembered, not read",
				s.ID, s.State.Status)
		}
		if s.State.Quota == nil || s.State.Quota.ResetHint == "" {
			t.Errorf("%s lost the reset hint; a caller should not scrape it from prose", s.ID)
		}
	}
}

// One working session is proof the account works. Nothing else clears the
// block — least of all the scraped reset time, which is prose the runtime may
// reword and which a supervisor next door already parsed into garbage.
func TestOneWorkingSessionClearsTheQuotaBlock(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%2"] = "transcript\nYou've hit your weekly limit\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)
	if _, err := d.List(ctx, testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	// The account recovers: a session starts working again.
	f.setCapture("%1", "transcript\n✻ Brewing… (3s · ↓ 1.2k tokens)\n"+rule+"\n❯ \n"+rule+"\n")
	f.setCapture("%2", idleFixtureFor("beta"))
	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status == fleet.StatusQuotaBlocked {
			t.Errorf("%s still blocked after a session was observed working", s.ID)
		}
	}
}

// A notice outlives the limit it announced: nobody types into a session that
// refused them, so the screen never changes. Read literally it says "blocked"
// forever — measured on a working machine as two sessions blocked by expired
// notices while two others on that account worked.
func TestWorkingSessionOverrulesAnExpiredNoticeOnAnotherScreen(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%1"] = "transcript\n✻ Brewing… (3s · ↓ 1.2k tokens)\n" + rule + "\n❯ \n" + rule + "\n"
	f.captures["%2"] = "  ⎿  You've hit your session limit · resets 3:40pm (Asia/Saigon)\n" +
		"     /upgrade to increase your usage limit.\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var sawWorking bool
	for _, s := range col.Items() {
		switch s.State.Status {
		case fleet.StatusWorking:
			sawWorking = true
		case fleet.StatusQuotaBlocked:
			t.Errorf("%s reported blocked while another session on the account works", s.ID)
		}
		if s.State.Quota != nil {
			t.Errorf("%s: an overruled notice left a quota block behind", s.ID)
		}
	}
	if !sawWorking {
		t.Fatal("fixture never produced a working session; the test proves nothing")
	}
}

// A block always carries a real since. The per-session path builds it in
// classify, which has no clock; the zero time serialises as year 1, so a
// caller asking "blocked for how long" got two millennia.
func TestQuotaBlockCarriesARealSince(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	notice := "  ⎿  You've hit your weekly limit · resets Aug 10 at 12am (Asia/Tokyo)\n" + rule + "\n❯ \n" + rule + "\n"
	f.captures["%1"] = notice
	f.captures["%2"] = notice
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		q := s.State.Quota
		if q == nil {
			t.Fatalf("%s: no quota block on a blocked session", s.ID)
		}
		if q.Since.IsZero() {
			t.Errorf("%s: since is the zero time, which claims a date in year 1", s.ID)
		}
	}
}

// unknown is not a competing truth — it is this driver saying it could not
// determine one, and an account fact is more specific than that. Left out at
// first, which showed up as four sessions flapping between unknown and
// quota_blocked across consecutive reads, on panes that redraw a counter.
func TestAccountBlockCoversSessionsItCouldNotClassify(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%1"] = "transcript\nYou've hit your weekly limit · resets Aug 10\n" + rule + "\n❯ \n" + rule + "\n"
	// A pane with no spinner and an empty composer, seen once: the driver has
	// nothing to settle it against, so it classifies unknown.
	f.captures["%2"] = "transcript\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status == fleet.StatusUnknown {
			t.Errorf("%s left unknown while the account is refusing work — "+
				"unknown is the absence of a truth, not a better one", s.ID)
		}
	}
}

// #10.a: a create must not be silently refused because the account behind
// the machine is refusing work — the caller is told and decides. This is
// the "told" half: the state read back for a session just created while the
// account is blocked must say so, not "starting", which is exactly what a
// caller reading the state embedded in Create's own 201 response sees
// first.
func TestCreateOnARefusingAccountReportsTheBlockRatherThanSwallowingIt(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	// Establish the block the way the driver actually learns of one: a
	// session shows the notice.
	f.captures["%2"] = "transcript\nYou've hit your weekly limit · resets Aug 10 at 12am\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)
	if _, err := d.List(ctx, testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	// The notice scrolls away — same setup as
	// TestQuotaBlockOutlivesTheNoticeThatAnnouncedIt — so nothing on any
	// screen says the account is blocked any more. The only surviving
	// evidence is the driver's own remembered block.
	f.setCapture("%2", idleFixtureFor("beta"))

	ref, err := d.Create(ctx, testCaller, "create-key", fleet.SessionSpec{
		Machine: "testbox", Cwd: "/work/new", Name: "gamma",
	})
	if err != nil {
		t.Fatalf("create must not refuse on a refusing account (#10): %v", err)
	}

	// The fake's "new-session" is a no-op against its own session table (see
	// its own comment on that case); model what a subsequent list-panes
	// would show for the pane that was actually just spawned: young, and
	// with nothing painted yet — exactly the pane classify sees the instant
	// Create's own HTTP handler reads it back to build a 201 body.
	f.mu.Lock()
	f.sessions = append(f.sessions, fakeSession{
		name: ref.ID, paneID: "%3", cwd: "/work/new", pid: 999, created: 1785760000,
	})
	f.captures["%3"] = "  loading...\n"
	f.mu.Unlock()

	st, err := d.State(ctx, testCaller, ref.SessionRef)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != fleet.StatusQuotaBlocked {
		t.Errorf("a session created while the account is refusing work read %q, "+
			"not quota_blocked — the fact was swallowed rather than reported", st.Status)
	}
}

// #10.b: a fleet-scope caller must be able to tell this machine's account is
// refusing work by reading Sources() alone — never by reading Items() and
// inferring it from what individual sessions say, which is unusable the
// moment a filter empties Items() and impossible for a caller that never
// asked for sessions in the first place.
func TestListSourceReportsAccountRefusalWithoutReadingSessions(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%2"] = "transcript\nYou've hit your weekly limit · resets Aug 10 at 12am\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(col.Sources()) != 1 {
		t.Fatalf("want exactly one source, got %d", len(col.Sources()))
	}
	src := col.Sources()[0]
	if src.Status != fleet.SourceOK {
		t.Errorf("the machine is reachable and answering; that must stay SourceOK "+
			"even though the account is refusing work — conflating the two is exactly "+
			"what this envelope exists to prevent, got status %q", src.Status)
	}
	if src.Quota == nil {
		t.Fatal("Sources()[0].Quota is nil — a scheduler reading only the source list " +
			"cannot tell this machine's account is refusing work without reading sessions")
	}

	// And it survives a filter that empties Items() entirely — the case
	// where nothing about the fact could be inferred from what came back.
	filtered, err := d.List(ctx, testCaller, driver.ListFilter{CwdPrefix: "/nowhere"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items()) != 0 {
		t.Fatalf("filter should have matched nothing, got %d items", len(filtered.Items()))
	}
	if filtered.Sources()[0].Quota == nil {
		t.Error("the account fact must not depend on Items() being non-empty")
	}
}

// #12.b: a session started before the local credential material changed
// must be distinguishable, by field alone, from one started after — the
// predicate a supervisor evaluates itself (startedAt < generation), with no
// account identity involved.
func TestSessionsDistinguishWhichCredentialGenerationTheyStartedUnder(t *testing.T) {
	f := twoSessions() // alpha created 1785600000, well before the rotation below
	// A third session, spawned AFTER the credential rotation modelled below —
	// the "after" half of the pair this field exists to tell apart.
	f.sessions = append(f.sessions, fakeSession{
		name: "gamma", paneID: "%3", cwd: "/work/gamma", pid: 300, created: 1785750000, title: "2_1_220",
	})
	f.captures["%3"] = idleFixtureFor("gamma")

	credPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The rotation: the store's mtime lands strictly between alpha's start
	// and gamma's.
	rotated := time.Unix(1785700000, 0)
	if err := os.Chtimes(credPath, rotated, rotated); err != nil {
		t.Fatal(err)
	}

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithCredentialPath(credPath))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]fleet.Session{}
	for _, s := range col.Items() {
		byName[s.ID] = s
	}
	alpha, ok := byName["alpha💬"]
	if !ok {
		t.Fatal("alpha missing from the listing")
	}
	gamma, ok := byName["gamma"]
	if !ok {
		t.Fatal("gamma missing from the listing")
	}

	if alpha.State.CredentialGeneration == nil || gamma.State.CredentialGeneration == nil {
		t.Fatalf("CredentialGeneration absent: alpha=%v gamma=%v",
			alpha.State.CredentialGeneration, gamma.State.CredentialGeneration)
	}
	// Both sessions read the SAME generation — one machine, one credential
	// store, one instant of read (List reads it once, not per session) —
	// only StartedAt differs between them.
	if !alpha.State.CredentialGeneration.Equal(*gamma.State.CredentialGeneration) {
		t.Errorf("alpha and gamma read different generations in the same List call: %v vs %v",
			alpha.State.CredentialGeneration, gamma.State.CredentialGeneration)
	}

	if alpha.StartedAt == nil || gamma.StartedAt == nil {
		t.Fatal("StartedAt absent")
	}
	if !alpha.StartedAt.Before(*alpha.State.CredentialGeneration) {
		t.Errorf("alpha started at %v, before the rotation at %v, but the predicate does not show it",
			*alpha.StartedAt, *alpha.State.CredentialGeneration)
	}
	if gamma.StartedAt.Before(*gamma.State.CredentialGeneration) {
		t.Errorf("gamma started at %v, after the rotation at %v, but the predicate claims otherwise",
			*gamma.StartedAt, *gamma.State.CredentialGeneration)
	}
}

// #12, §5.7 applied to this field: a driver with no credential store
// configured reports the fact absent, not a guessed "current" value.
func TestCredentialGenerationAbsentWhenUnconfigured(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f) // no WithCredentialPath
	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.CredentialGeneration != nil {
			t.Errorf("session %s: CredentialGeneration = %v, want nil with no store configured",
				s.ID, s.State.CredentialGeneration)
		}
	}
}

// #12: State() — the single-session read Create's own HTTP handler uses to
// build a 201 body (see quotaBlockedState's own comment on that path) —
// must carry CredentialGeneration too, or a caller reading a freshly
// created session's state has no way to tell which generation it answered
// under until a later List call happens to notice.
func TestStateCarriesCredentialGeneration(t *testing.T) {
	f := twoSessions()
	credPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithCredentialPath(credPath))

	st, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if st.CredentialGeneration == nil {
		t.Fatal("State() did not carry CredentialGeneration")
	}
}

// The THIRD submit site (#22), reached only through the recovery path: a
// delivery this driver could not confirm, resumed by the caller.
//
// It is the site most exposed to a dropped newline and it was the one left
// out. The pane is idle by definition — the branch only runs when a composer
// has been sitting on an unsubmitted line — so if a lone newline is ever
// swallowed anywhere, it is swallowed here.
//
// #101: it used to fail worse than the others. This path reported `submitted`,
// the strongest outcome in the enum, without verifying anything, and called
// forgetStranded immediately afterwards, discarding the driver's own record of
// the text on the strength of a keystroke nobody checked — a swallowed newline
// here turned a recoverable stranding into a permanent one while reporting
// success. It now confirms the submit the same evidence-based way the
// first-attempt path already does (composer emptying, or the attributed
// marker clearing) before reporting anything, and keeps the record if it
// cannot. TestSendReportsUnknownWhenTheSubmitDoesNotRegisterOnResume covers the
// swallowed-keystroke case directly.
//
// Reaching this branch needs a stranded record AND a busy composer. The first
// send manufactures both: a pane that never renders what was pasted cannot be
// confirmed, so the driver records the text and reports unknown.
func TestResumingAStrandedSendAlsoWakesThePane(t *testing.T) {
	f := twoSessions()
	f.noEcho = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the instruction that stranded"

	first, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: first send outcome = %q, want unknown so the text is recorded as stranded",
			first.Outcome)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			t.Fatal("setup: the unconfirmed send must not have submitted anything; " +
				"any send-keys below would then be ambiguous")
		}
	}

	// The composer now holds unsent text, which is the state the resume branch
	// requires before it will act. #109: this must be OUR OWN text (or an
	// attributable marker for it), not any unrelated non-empty composer —
	// confirmLanded now gates the resume submit (below), and a fixture that
	// does not echo the stranded text would never satisfy it.
	f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ "+text+"\n"+rule+"\n")

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume outcome = %q (%s), want queued", got.Outcome, got.Reason)
	}

	submits := 0
	for _, c := range f.callsSnapshot() {
		if c[0] != "send-keys" {
			continue
		}
		keys := sentKeys(c)
		if len(keys) == 0 || !isNewlineKey(keys[len(keys)-1]) {
			continue
		}
		submits++
		if len(keys) < 2 {
			t.Fatalf("the resume submitted with %v — a lone newline into the one pane that is "+
				"idle by definition, on the path that then DISCARDS the stranded record", keys)
		}
		if wake := keys[len(keys)-2]; !printableKey(wake) {
			t.Errorf("key before the newline is %q, not printable; keys were %v", wake, keys)
		}
	}
	if submits != 1 {
		t.Fatalf("expected exactly one submit from the resume, saw %d", submits)
	}
}

// #101. Confirmed to fail against the pre-fix code: reverting the resume
// branch to report OutcomeSubmitted unconditionally makes this test fail on
// BOTH assertions — the outcome would read submitted despite the composer
// still holding the text, and forgetStranded would already have discarded
// the record the second t.Fatal below checks for.
//
// This is the exact failure #101 was filed over: a resume's own submit
// keystroke can be swallowed exactly like any other, and the old code never
// looked before reporting the strongest outcome in the enum.
func TestSendReportsUnknownWhenTheSubmitDoesNotRegisterOnResume(t *testing.T) {
	f := twoSessions()
	f.noEcho = true // the first attempt never renders the paste, so confirm times out
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "an instruction whose resume submit will also be swallowed"

	first, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: first send outcome = %q, want unknown so the text is recorded as stranded",
			first.Outcome)
	}

	// The composer now holds the stranded text, and THIS time the resume's
	// own wake-key submit is the one that gets swallowed too. #109: this must
	// be OUR OWN text so confirmLanded's new gate (below) is satisfied and the
	// submit is actually attempted — this test is about the SUBMIT keystroke
	// being swallowed, not about attribution failing.
	f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ "+text+"\n"+rule+"\n")
	f.noEcho = false
	f.swallowSubmit = true

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeSubmitted {
		t.Fatal("a resume whose own submit keystroke was swallowed was reported as submitted " +
			"— the exact false receipt #101 was filed over")
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	if !d.strandedMatches(ref.ID, "/work/alpha", text) {
		t.Fatal("the stranded record was discarded on a submit that was never confirmed; " +
			"a third attempt now has nothing left to resume")
	}
}

// #109: a live regression measured within minutes of #101 shipping — a long
// multi-line create-time prompt reaching the agent truncated, tail only. The
// resume branch pressed the wake key and submit UNCONDITIONALLY, discarding
// confirmLanded's own `landed` result (`key, atCount, _ := d.confirmLanded(...)`)
// on the reasoning that composerText already proved something was pending, so
// there was "nothing left to land, only to submit". That reasoning has a gap:
// composerText proves the composer is non-empty, not that it holds a SETTLED,
// attributable copy of this delivery's text — a still-collapsing multi-line
// paste, or an ambiguous composer holding more than one collapsed-paste
// marker (this driver's own residue from an unrelated stranding, per #37),
// both leave confirmLanded unable to attribute anything, and the old code
// pressed Enter anyway.
//
// Confirmed to fail against the pre-fix code (reverting the `if !landed`
// early return makes this test fail on both assertions below: a send-keys
// call appears in the snapshot, and the resume reports something other than
// unknown).
func TestResumeRefusesToSubmitWhenTheComposerCannotBeAttributed(t *testing.T) {
	f := twoSessions()
	f.noEcho = true // the first attempt never renders the paste, so confirm times out
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "a long multi-line prompt that stranded on the first attempt"

	first, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: first send outcome = %q, want unknown so the text is recorded as stranded",
			first.Outcome)
	}
	f.noEcho = false

	// The composer is non-empty (composerText's own gate is satisfied — this
	// resume branch is reached at all), but it holds TWO ambiguous
	// collapsed-paste markers rather than a single one this delivery can be
	// pinned to: unrelated residue (#10) alongside a marker that could be
	// ours (#11). `gained` against confirmLanded's empty baseline sees both
	// counts rise from zero and refuses to pick one (hits != 1) — this is
	// deliberately NOT the single-marker shape "resume completes our own
	// stranded delivery" (above) exercises.
	f.setCapture("%1", "transcript\n"+rule+
		"\n❯ [Pasted text #10 +12 lines][Pasted text #11 +30 lines]\n"+rule)

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %q (%s), want unknown — nothing here can be attributed to our "+
			"delivery, so submitting would risk sending an incomplete or wrong composer",
			got.Outcome, got.Reason)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			t.Fatalf("send-keys was called (%v) — the resume must not press the composer "+
				"blind when it cannot confirm what is actually sitting in it", c)
		}
	}
	if !d.strandedMatches(ref.ID, "/work/alpha", text) {
		t.Fatal("the stranded record was discarded even though nothing was ever confirmed " +
			"or submitted; a later resume now has nothing left to finish")
	}
}

// A session that cannot receive yet must be REFUSED, not delivered to.
//
// Measured on a live fleet: delivering to a session that has not finished
// starting renders the text in the composer and drops the submit, two runs in
// three, while the receipt said "queued". Create returns as soon as the
// process is spawned, so this window is the ordinary case for anything that
// creates a session and immediately sends to it.
//
// The signal is the composer's presence, not a delay. A pane with no composer
// painted has no input widget, and the widget is drawn by the component that
// reads keys — so its absence is evidence about the input path rather than
// about elapsed time. A timing guess would pass on this machine and fail on a
// slower one, which is the bug wearing a different hat.
func TestSendRefusesASessionThatCannotReceiveYet(t *testing.T) {
	f := twoSessions()
	// A pane mid-startup: output, but no composer fenced by rules. twoSessions'
	// alpha is created well outside startingWindow relative to the fixed test
	// clock (newTestDriver), so this exercises the "old" branch — see
	// muster #64 below for why that branch's wording matters.
	f.captures["%1"] = "starting up\nloading configuration\n"
	d := newTestDriver(f)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "do the thing",
		driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q (%s), want refused — delivering here strands the text",
			got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "idle") {
		t.Errorf("reason = %q; it must tell the caller what to wait for", got.Reason)
	}
	// muster #64: an old pane with no composer painted is not
	// necessarily "still starting or is not listening" — that guess was
	// wrong for a session showing a full-screen interface (a
	// control-channel dialog, measured directly). The refusal must not
	// assert a single cause, and must point at keys() as the one surface
	// that can still reach the screen either way.
	if !strings.Contains(got.Reason, "full-screen") {
		t.Errorf("reason = %q; an old pane's refusal must not assume startup "+
			"is the only explanation (#64)", got.Reason)
	}
	if !strings.Contains(got.Reason, "keys()") {
		t.Errorf("reason = %q; must point at keys() as the surface that can still "+
			"reach a full-screen interface (#64)", got.Reason)
	}
	// And nothing may have been delivered. A refusal that pasted first would
	// leave the session holding text nobody can account for.
	for _, c := range f.callsSnapshot() {
		switch c[0] {
		case "load-buffer":
			t.Error("text was delivered despite the refusal")
		case "send-keys":
			t.Error("a keystroke was sent despite the refusal")
		}
	}
}

// The companion case: a session created moments ago, still with no
// composer painted. Here "still starting" IS the honest, established
// reading — age is the discriminator classify.go already uses for the
// identical ambiguity, and Send's refusal must agree with it rather than
// hedge about a full-screen interface a two-second-old session cannot yet
// be showing.
func TestSendRefusesAYoungSessionAsStartingNotAmbiguous(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = "starting up\nloading configuration\n"
	d := newTestDriver(f)
	// newTestDriver's clock is fixed at 1785760000; put this pane's creation
	// just inside startingWindow (90s) of it.
	for i := range f.sessions {
		if f.sessions[i].paneID == "%1" {
			f.sessions[i].created = 1785760000 - 10
		}
	}

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "do the thing",
		driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q (%s), want refused", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "young enough to still be starting") {
		t.Errorf("reason = %q; a genuinely young session should read as starting, "+
			"not hedge about a full-screen interface it cannot yet be showing (#64)", got.Reason)
	}
	if strings.Contains(got.Reason, "full-screen") {
		t.Errorf("reason = %q; the young case should not raise a possibility the "+
			"old case exists to cover (#64)", got.Reason)
	}
}

// muster #64: Respond's refusal fires whenever no structured prompt is
// recognised, which is right and common (nothing is being asked) — but a
// composer present settles which case this is. An idle, empty composer is
// definitely not a full-screen interface (that shape has no composer of its
// own), so the original wording is correct and unhedged here.
func TestRespondRefusesOrdinarilyWhenComposerIsPresent(t *testing.T) {
	f := twoSessions() // alpha: idle fixture, composer present and empty, no prompt
	d := newTestDriver(f)

	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.Response{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused", got.Outcome)
	}
	if !strings.Contains(got.Reason, "consumed by whatever it is doing") {
		t.Errorf("reason = %q; an idle composer settles that nothing is being "+
			"asked, unhedged (#64)", got.Reason)
	}
	if strings.Contains(got.Reason, "keys()") {
		t.Errorf("reason = %q; must not point at keys() when a composer proves "+
			"there is no full-screen interface to reach (#64)", got.Reason)
	}
}

// The companion case: no recognised prompt AND no composer, which is the
// shape #64 measured a real control-channel dialog producing. respond()
// cannot answer an unrecognised full-screen prompt — there is no option
// list or nonce — so the refusal must say that, and point at keys(), rather
// than assert the session is doing something else when it may be waiting on
// exactly this keypress.
func TestRespondHedgesWhenNeitherPromptNorComposerIsFound(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = "starting up\nloading configuration\n"
	d := newTestDriver(f)

	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.Response{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused", got.Outcome)
	}
	if strings.Contains(got.Reason, "consumed by whatever it is doing") {
		t.Errorf("reason = %q; must not assert the session is doing something "+
			"else — it may be waiting on exactly this keypress (#64)", got.Reason)
	}
	if !strings.Contains(got.Reason, "keys()") {
		t.Errorf("reason = %q; must point at keys() as the surface that can still "+
			"reach an unrecognised full-screen prompt (#64)", got.Reason)
	}
}

// The fail-closed half. When the submit does not register, the receipt must
// say so rather than reporting the same "queued" a working send reports.
//
// This is the case that was silent: the text renders (so the pre-submit
// confirmation is satisfied), the keystroke goes nowhere, and the caller is
// told the delivery was queued. Nothing afterwards contradicted it.
func TestSendReportsUnknownWhenTheSubmitDoesNotRegister(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "an instruction that must not vanish"

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeQueued {
		t.Fatal("a submit that went nowhere was reported as queued — this is the silent " +
			"failure the confirmation exists to end")
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %q, want unknown", got.Outcome)
	}
	if !strings.Contains(got.Reason, "resumeIfStranded") {
		t.Errorf("reason = %q; it must name the way back in", got.Reason)
	}

	// AND the driver must have recorded it, or the recovery it just advised is
	// refused for lack of a record. That was the real shape of this gap: the
	// stranded record was written only when the text failed to RENDER, so a
	// dropped submit left nothing behind and resumeIfStranded could not help.
	if !d.strandedMatches(ref.ID, "/work/alpha", text) {
		t.Fatal("no stranded record was kept, so the resume this receipt recommends " +
			"would be refused and the text is unreachable")
	}
}

// muster #131 (item 3): the same restart-correlation fact Discard's own
// 409s carry since 8ff743a (see TestDiscardNotesWhenTheUnsentInputStatusPredatesTheService)
// must reach Send's own OutcomeUnknown receipts too — a caller retrying a
// send that lands on unknown should not have to cross-reference a separate
// State() read to learn the session's waiting_input status predates this
// service's current process.
//
// Simulated the same way that Discard test is: a first driver's List()
// persists "beta"'s waiting_input status to a shared state store, standing
// in for the deploy that restarts this service; a second driver over the
// same store never calls List itself, so it must fall back to the persisted
// record the same way restoredWaitingInputSince does for Discard. By the
// time the second driver's Send runs, "beta"'s composer is idle again (the
// note fires purely off the PERSISTED record, never off what the composer
// holds right now — the same evidence restoredWaitingInputSince itself
// reads, no more).
func TestSendNotesWhenTheUnsentInputStatusPredatesTheService(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions() // "beta" (%2) starts holding unsent input; see twoSessions

	newDriver := func() *Driver {
		st, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Unix(1785760000, 0) }), WithState(st))
	}

	first := newDriver()
	if _, err := first.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	// The restart. Nothing about the state store or the multiplexer changed —
	// only the driver process. "beta"'s composer clears (someone's own
	// business, or an earlier delivery — irrelevant here) before the send
	// under test, and that send's own paste is never rendered, landing it on
	// unknown for an unrelated reason. The point under test is that the note
	// still fires, off the persisted record alone.
	f.setCapture("%2", idleFixtureFor("beta"))
	f.noEcho = true

	second := newDriver()
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}
	got, err := second.Send(context.Background(), testCaller, ref,
		"an instruction into a session that just restarted", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %q, want unknown", got.Outcome)
	}
	if !strings.Contains(got.Reason, "carried from before this service restarted") {
		t.Errorf("reason = %q; a Send landing on unknown against a session whose "+
			"waiting_input status predates this service's current process must say so, "+
			"the same fact Discard's 409s and State/List's own evidence line already carry",
			got.Reason)
	}

	// Control: "alpha💬" never held unsent input, so its persisted record
	// carries no waiting_input status and the note must not appear — proving
	// this is not unconditionally appended to every unknown outcome.
	third := newDriver()
	gotAlpha, err := third.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "an ordinary instruction",
		driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if gotAlpha.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: control outcome = %q, want unknown (noEcho is still set)", gotAlpha.Outcome)
	}
	if strings.Contains(gotAlpha.Reason, "carried from before this service restarted") {
		t.Errorf("reason = %q; must not append the restart note to a session with no "+
			"persisted waiting_input history", gotAlpha.Reason)
	}
}

// The confirmation must not fire on the composer's own placeholder. The
// runtime draws a faint hint into an EMPTY composer, and reading that as
// leftover text would report every healthy send as stranded — the same
// mistake, in the opposite direction, that produced a round of false evidence
// on this repo's tracker.
func TestSubmitConfirmationTreatsAFaintPlaceholderAsEmpty(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = "  transcript line\n✻ Brewed for 1m 0s\n" + rule +
		"\n❯ \x1b[2mtry asking about the build\x1b[0m\n" + rule + "\n  ⏵⏵ auto mode on"
	d := newTestDriver(f)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello",
		driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %q (%s); a composer holding only its dim placeholder is EMPTY, "+
			"and treating it as unsent text reports healthy sends as stranded",
			got.Outcome, got.Reason)
	}
}

// --- what happens to a new session that boots into a question ---------------

// settleHarness watches a pane the way a test needs to and the fake cannot: it
// records the payload of every delivery (the fake models a submit by EMPTYING
// the composer, so the delivered text is gone by the time an assertion runs),
// and it notes whether the trust question was answered by index.
//
// When answers is true it also moves the screen on: a fake whose pane never
// changes cannot tell "answered it" from "kept pressing keys at a screen that
// was never going to move", which is the whole point of spending a consent once.
type settleHarness struct {
	mu        sync.Mutex
	mux       *fakeMux
	delivered []string
	answered  bool
	chosen    []string
}

func newSettleHarness(t *testing.T, capture string, answers bool) (*settleHarness, *Driver) {
	t.Helper()
	h := &settleHarness{mux: twoSessions()}
	h.mux.captures["%1"] = capture
	exec := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 3 && args[0] == "load-buffer" {
			if raw, err := os.ReadFile(args[3]); err == nil {
				h.mu.Lock()
				h.delivered = append(h.delivered, string(raw))
				h.mu.Unlock()
			}
		}
		if len(args) > 0 && args[0] == "send-keys" {
			for _, a := range args[3:] {
				if a == "1" || a == "2" || a == "Escape" {
					h.mu.Lock()
					h.answered = true
					h.chosen = append(h.chosen, a)
					h.mu.Unlock()
					if answers {
						h.mux.setCapture("%1", idleFixtureFor("alpha"))
					}
				}
			}
		}
		return h.mux.exec(ctx, name, args...)
	}
	d := New("testbox", withExec(exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	return h, d
}

func (h *settleHarness) sawDelivery(text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.delivered {
		if strings.Contains(d, text) {
			return true
		}
	}
	return false
}

func (h *settleHarness) keysPressed() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.chosen...)
}

// waitForTimeout is the per-condition budget waitFor waits before giving up.
// A caller whose condition is inherently shared across several concurrent
// actors (so its real cost scales with how many there are, not a fixed
// constant) should not reuse this value as-is — see waitForDeadline.
const waitForTimeout = 15 * time.Second

// waitFor polls rather than sleeping a guessed amount: the settle loop's own
// interval is long on purpose (a startup is slow), so a fixed sleep would be
// either flaky or the slowest test in the package.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	waitForDeadline(t, why, time.Now().Add(waitForTimeout), cond)
}

// waitForDeadline is waitFor with the deadline supplied rather than assumed.
// It exists for a condition whose real budget is not the package's ordinary
// waitForTimeout — e.g. several actors doing the same wait concurrently,
// which contend for the same locks and so take longer under load the more of
// them there are (muster#221, the same class of fixed-wall-clock flake
// as #214 and #186: the bound a healthy run needs is milliseconds, but the
// bound that must never misfire under a loaded -race run is a different,
// larger number, and picking one fixed constant for both is what flaked).
func waitForDeadline(t *testing.T, why string, deadline time.Time, cond func() bool) {
	t.Helper()
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// A caller that consented at create time gets past the question, and the work
// it created the session for is what arrives on the other side.
func TestTrustCwdAnswersTheFolderQuestionAndThenDelivers(t *testing.T) {
	h, d := newSettleHarness(t, fixtureTrustPrompt, true)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{Cwd: "/work/alpha", Prompt: "do the thing", TrustCwd: true})

	waitFor(t, "the work to be delivered after the question was answered", func() bool {
		return h.sawDelivery("do the thing")
	})
	// By index, and by the index that GRANTS trust — the fixture's option 2 is
	// the one that declines, and Escape would kill the session outright.
	if got := h.keysPressed(); len(got) == 0 || got[0] != "1" {
		t.Errorf("keys pressed at the question = %v, want the granting option first", got)
	}
}

// The regression that cost a session two days of its life.
//
// Without consent the driver answers NOTHING — but it must not walk away
// either. It used to: the loop returned the moment it saw a prompt, so the
// instruction the session was created with was discarded while the modal was
// up, and a human answering the question a minute later got a ready, empty
// session with no record that any work had ever been attached to it.
func TestABlockingQuestionIsWaitedThroughRatherThanGivenUpOn(t *testing.T) {
	h, d := newSettleHarness(t, fixtureTrustPrompt, false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.settleNewSession(testCaller,
			fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			fleet.SessionSpec{Cwd: "/work/alpha", Prompt: "the work it was created for"})
	}()

	// A human answers, some time after the create returned.
	time.Sleep(100 * time.Millisecond)
	h.mux.setCapture("%1", idleFixtureFor("alpha"))

	waitFor(t, "the work to be delivered once the question cleared", func() bool {
		return h.sawDelivery("the work it was created for")
	})
	<-done

	if got := h.keysPressed(); len(got) != 0 {
		t.Errorf("the driver answered a question nobody consented to: %v", got)
	}
}

// muster #124/#125: the field measurement was of the CREATE RECORD a
// real caller reads back through List/Create's own response — not of
// settleNewSession's internal delivery log, which TestABlockingQuestionIs-
// WaitedThroughRatherThanGivenUpOn above already covers. Two machines in the
// identical situation diverged there: one resolved to `unknown` with
// evidence, the other stayed `null` forever. This proves the record itself
// converges to a real, non-`unknown` outcome when the dialog clears well
// inside the deadline — the success half of the same fix; the timeout half
// is TestPromptDeliveryAlwaysResolves's "the window closes before the
// session is ever ready" in createrecord_test.go.
func TestABlockingQuestionAnsweredEventuallyResolvesTheCreateRecord(t *testing.T) {
	h, d := newSettleHarness(t, fixtureTrustPrompt, false)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const cwd = "/work/alpha"
	const text = "the work it was created for"
	// Mimics what Create itself does (noteCreateRecord) before starting the
	// settle goroutine — see createrecord_test.go's seedRecord for the same
	// technique and its own reasoning.
	d.noteCreateRecord(ref.ID, cwd, fleet.SessionSpec{Cwd: cwd, Prompt: text})

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.settleNewSession(testCaller, ref, fleet.SessionSpec{Cwd: cwd, Prompt: text})
	}()

	// A human answers, some time after the create returned — same shape as
	// TestABlockingQuestionIsWaitedThroughRatherThanGivenUpOn, but this test
	// reads the outcome the way a real caller does: through the create
	// record, not through the test harness's own delivery log.
	time.Sleep(100 * time.Millisecond)
	h.mux.setCapture("%1", idleFixtureFor("alpha"))

	waitFor(t, "the create record to resolve", func() bool {
		rec, ok := d.createRecordFor(ref.ID, cwd)
		return ok && rec.PromptOutcome != ""
	})
	<-done

	rec, ok := d.createRecordFor(ref.ID, cwd)
	if !ok {
		t.Fatal("create record vanished")
	}
	if rec.PromptOutcome == string(fleet.OutcomeUnknown) {
		t.Errorf("PromptOutcome = unknown; the dialog cleared and the session was still "+
			"there, this should have delivered normally — got evidence %q", rec.PromptEvidence)
	}
	if rec.PromptEvidence == "" {
		t.Error("a resolved delivery must still carry evidence")
	}
	if got := h.keysPressed(); len(got) != 0 {
		t.Errorf("the driver answered a question nobody consented to: %v", got)
	}
}

// muster #125's own load-bearing requirement: a caller must be able to
// say WHY a prompt has not landed WHILE it is still waiting, not only once
// the wait ends. This reads the create record DURING the wait — before the
// dialog ever clears — and requires the live evidence to name the actual
// blocking dialog, not the generic "accepted at creation" placeholder
// promptDeliveryFor falls back to only in the instant before any diagnosis
// exists.
func TestPendingCreateRecordExplainsWhyWhileStillWaiting(t *testing.T) {
	h, d := newSettleHarness(t, fixtureTrustPrompt, false) // screen never moves on its own
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const cwd = "/work/alpha"
	const text = "the work it was created for"
	d.noteCreateRecord(ref.ID, cwd, fleet.SessionSpec{Cwd: cwd, Prompt: text})

	go d.settleNewSession(testCaller, ref, fleet.SessionSpec{Cwd: cwd, Prompt: text})

	waitFor(t, "the pending evidence to name the dialog", func() bool {
		rec, ok := d.createRecordFor(ref.ID, cwd)
		return ok && strings.Contains(rec.PromptEvidence, "folder-trust")
	})

	rec, ok := d.createRecordFor(ref.ID, cwd)
	if !ok {
		t.Fatal("create record vanished")
	}
	if rec.PromptOutcome != "" {
		t.Fatalf("PromptOutcome = %q, want still empty — the dialog never cleared in this "+
			"test, so this must still be a live diagnosis, not a verdict", rec.PromptOutcome)
	}
	if rec.PromptEvidence == "the prompt was accepted at creation and has not been "+
		"delivered yet; the session has not finished painting a composer to receive it" {
		t.Error("evidence is still the generic placeholder; the live diagnosis never landed")
	}
	if got := h.keysPressed(); len(got) != 0 {
		t.Errorf("the driver answered a question nobody consented to: %v", got)
	}
}

// muster #126: the same live diagnosis #125 proved above must also
// carry a machine-readable class — a caller must be able to tell needs a
// human keypress · composer occupied · still starting apart WITHOUT parsing
// the prose. Three fixtures exercise the three classes this driver can
// currently name; see fleet.WaitingReason's own doc for why these three.
func TestPendingCreateRecordCarriesAMachineReadableClass(t *testing.T) {
	cases := []struct {
		name    string
		capture string
		want    fleet.WaitingReason
	}{
		{"parked on a dialog", fixtureTrustPrompt, fleet.WaitingPrompt},
		{"composer holds unsent text", fixtureUnsent, fleet.WaitingUnsentInput},
		{"no composer painted yet", "  loading...\n", fleet.WaitingStarting},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, d := newSettleHarness(t, c.capture, false) // screen never moves on its own
			ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
			const cwd = "/work/alpha"
			const text = "the work it was created for"
			d.noteCreateRecord(ref.ID, cwd, fleet.SessionSpec{Cwd: cwd, Prompt: text})

			go d.settleNewSession(testCaller, ref, fleet.SessionSpec{Cwd: cwd, Prompt: text})

			waitFor(t, "the pending record to carry a class", func() bool {
				rec, ok := d.createRecordFor(ref.ID, cwd)
				return ok && rec.PromptWaitingOn != ""
			})

			rec, ok := d.createRecordFor(ref.ID, cwd)
			if !ok {
				t.Fatal("create record vanished")
			}
			if got := fleet.WaitingReason(rec.PromptWaitingOn); got != c.want {
				t.Errorf("PromptWaitingOn = %q, want %q", got, c.want)
			}
			if rec.PromptOutcome != "" {
				t.Fatalf("PromptOutcome = %q, want still empty — this fixture never "+
					"resolves on its own", rec.PromptOutcome)
			}
			if got := h.keysPressed(); len(got) != 0 {
				t.Errorf("the driver answered a question nobody consented to: %v", got)
			}
		})
	}
}

// Consent is spent once. A trust question still on screen at the next poll —
// the keypress has not repainted yet — must not be answered twice: the second
// digit lands in whatever screen replaced it.
func TestTrustConsentIsSpentOnce(t *testing.T) {
	h, d := newSettleHarness(t, fixtureTrustPrompt, false) // screen never moves

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{Cwd: "/work/alpha", TrustCwd: true})
	waitFor(t, "the question to be answered", func() bool { return len(h.keysPressed()) > 0 })

	// Several more polls go by with the same question on screen.
	time.Sleep(4 * promptPollInterval)
	if got := h.keysPressed(); len(got) != 1 {
		t.Errorf("keys pressed = %v, want exactly one; a repeated answer lands in "+
			"whatever screen replaced the question", got)
	}
}

// --- the initial prompt's own delivery, once nobody is waiting on it --------

// The regression #44 measured, exercised through the full asynchronous path
// a real create takes: a session settles, its initial prompt lands and
// renders, the submit keystroke goes nowhere, and — before this fix —
// settleNewSession's discarded receipt (`_, _ = d.Send(...)`) meant nothing
// ever tried again. `done` closing is settleNewSession returning; against the
// code as it stood, the assertion after it fails, because nothing in that
// code path ever calls Send a second time and the stranded record this
// fixture already proves Send itself keeps (noteStranded) just sits there.
//
// #101 revised this fixture: swallowSubmit used to be left permanently on,
// on the reasoning that the retry "resumes through the SAME mechanism ...
// which corroborates against this driver's own record of what it delivered
// rather than re-reading the screen for confirmation" — true for WHICH TEXT
// is ours (§F49 still holds, a collapsed paste cannot be matched byte for
// byte), but the resume path now also confirms WHETHER THE SUBMIT
// REGISTERED, the same evidence-based way the first attempt already does.
// A terminal that swallows every keystroke forever is a terminal on which
// neither attempt should ever be reported as delivered — that is now the
// correct, honest outcome, not a fixture bug. So this models #44's actual
// recovery shape instead: the pane was receptive again "seconds later,
// nothing changed but time" (deliverInitialPrompt's own doc comment) — the
// FIRST submit is swallowed, manufacturing the strand, and the retry's own
// submit succeeds normally, exactly as #44 was measured recovering by hand.
func TestSettleNewSessionRecoversFromASwallowedInitialPromptSubmit(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = idleFixtureFor("alpha") // ready immediately, no boot question
	d := New("testbox", withExec(swallowFirstSubmitOnly(f)), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the work it was created for, long enough to strand"

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.settleNewSession(testCaller, ref,
			fleet.SessionSpec{Cwd: "/work/alpha", Prompt: text})
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("settleNewSession never returned")
	}

	if d.strandedMatches(ref.ID, "/work/alpha", text) {
		t.Error("the initial prompt is still stranded after settle finished; the one " +
			"retry #44 asks for did not run")
	}

	// A retry that clears silently would hide the one number #44 says
	// matters: how often this needs to happen at all. The counter must go
	// up on a retry that SUCCEEDS, not only on one that does not.
	got := d.counters.Snapshot()
	if got[counterInitialPromptRetried] != 1 {
		t.Errorf("initial_prompt.delivery_retried = %d, want 1", got[counterInitialPromptRetried])
	}
	if got[counterInitialPromptStranded] != 0 {
		t.Errorf("initial_prompt.delivery_stranded = %d, want 0 — the retry cleared it",
			got[counterInitialPromptStranded])
	}
}

// swallowFirstSubmitOnly wraps a fakeMux so exactly the FIRST send-keys
// invocation carrying a newline (C-m or Enter) is swallowed — the composer
// renders the text but the keystroke registers nothing — and every later
// submit, including a resume's, succeeds normally. This is #44's own
// measured recovery shape: the pane was receptive again "seconds later,
// nothing changed but time", not a terminal that swallows every keystroke
// forever. #101: a fixture that leaves swallowSubmit permanently on now
// means NEITHER attempt's submit ever registers, which is a correct strand
// forever, not a recoverable one — so a test wanting to see a resume
// actually recover needs the swallow to be transient, matching the
// incident it is named for.
func swallowFirstSubmitOnly(f *fakeMux) execFunc {
	seen := false
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		isSubmit := false
		if len(args) > 0 && args[0] == "send-keys" {
			for _, a := range args {
				if a == "C-m" || a == "Enter" {
					isSubmit = true
				}
			}
		}
		f.mu.Lock()
		f.swallowSubmit = isSubmit && !seen
		f.mu.Unlock()
		if isSubmit {
			seen = true
		}
		return f.exec(ctx, name, args...)
	}
}

// promptDeliveredThenInterrupted wraps a fakeMux so its composer holds this
// driver's OWN delivered text right up through the first attempt's own
// confirmation read — the shape TestSendReportsUnknownWhenTheSubmitDoesNotRegister
// already establishes strands honestly — and only THEN goes dark, modelling
// a session that stops being able to receive input at all before the retry
// looks, rather than staying receptive the way #44's own pane did.
func promptDeliveredThenInterrupted(f *fakeMux, paneID, goesDark string) execFunc {
	swallows := 0
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.exec(ctx, name, args...)
		if len(args) > 0 && args[0] == "send-keys" {
			for _, a := range args {
				if a == "C-m" || a == "Enter" {
					swallows++
					if swallows == 1 {
						// The first attempt's own confirmation read (the very
						// next capture-pane call) must still see what it
						// actually delivered, or this fixture would be
						// testing a different failure than #44's. Mutating
						// here — after f.exec already applied the swallow —
						// changes what the SECOND attempt sees without
						// touching the first's.
						f.setCapture(paneID, goesDark)
					}
				}
			}
		}
		return out, err
	}
}

// The other half of #44's design constraint: when the one retry is not
// enough, that must be counted too, not folded silently into "eventually
// worked".
//
// resumeIfStranded corroborates against this driver's OWN record of what it
// delivered (noteStranded), not against the screen — #49 established that a
// multi-line paste collapses on screen and cannot be read back, so exact
// text matching against the pane was never the available check. What CAN
// still fail between two attempts is readiness itself: the session stops
// painting a composer at all. Modelled here as exactly that, so the retry's
// own awaitReceptive gate refuses it before resumeIfStranded is ever
// reached, and the prompt is still unsent when deliverInitialPrompt gives up.
//
// Driving deliverInitialPrompt directly, not through settleNewSession's
// polling wrapper: the wrapper's own readiness loop adds enumeration calls
// whose count is not this test's concern, and pinning the swallow to a
// specific send-keys occurrence (rather than a capture-pane index like
// TestConfirmLandedIgnoresResidueAndAttributesOnlyTheNewMarker uses) is
// stable across however many reads the readiness gate happens to take.
func TestDeliverInitialPromptCountsAStrandTheRetryCannotClear(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = idleFixtureFor("alpha")
	f.swallowSubmit = true
	d := New("testbox",
		withExec(promptDeliveredThenInterrupted(f, "%1", "starting up\nloading configuration\n")),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "an instruction that must not vanish silently"

	d.deliverInitialPrompt(context.Background(), testCaller, ref, text)

	got := d.counters.Snapshot()
	if got[counterInitialPromptRetried] != 1 {
		t.Errorf("initial_prompt.delivery_retried = %d, want 1", got[counterInitialPromptRetried])
	}
	if got[counterInitialPromptStranded] != 1 {
		t.Errorf("initial_prompt.delivery_stranded = %d, want 1 — the retry never reached "+
			"a session able to receive it", got[counterInitialPromptStranded])
	}
	// And the original record is untouched — a caller who does eventually
	// look still finds the same resumeIfStranded path #44 measured working
	// by hand, 6 times out of 6, once the session is receptive again.
	if !d.strandedMatches(ref.ID, "/work/alpha", text) {
		t.Error("the stranded record was lost; the manual recovery path #44 measured " +
			"working would now find nothing to resume")
	}
}

// --- the option a consenting caller's trust is spent on ---------------------

// Never the highlighted one, and never on a prompt this is not about.
func TestAffirmativeOptionReadsTheOptionsOnly(t *testing.T) {
	classified := func(fixture string) *fleet.SessionPrompt {
		p := parsePrompt(newScreen(fixture))
		if p == nil {
			t.Fatalf("fixture did not parse: %q", fixture)
		}
		p.Kind = classifyPromptKind(p)
		return p
	}

	trust := classified(fixtureTrustPrompt)
	got, ok := affirmativeOption(trust)
	if !ok || got != 1 {
		t.Errorf("trust prompt = (%d, %v), want (1, true)", got, ok)
	}

	// The bypass screen highlights option 1 too, and option 1 is "No, exit".
	// A trust consent must not reach it at all.
	if _, ok := affirmativeOption(classified(fixtureBypassPrompt)); ok {
		t.Error("a bypass-acceptance screen was answered by a folder-trust consent")
	}

	// Reworded so that two options match the needles: the honest answer is to
	// answer nothing and leave the question for a human.
	reworded := classified("  Quick safety check\n" +
		"❯ 1. Yes, I trust this folder\n" +
		"  2. No, I do not trust this folder\n" +
		"Enter to confirm · Esc to cancel")
	if _, ok := affirmativeOption(reworded); ok {
		t.Error("an ambiguous rewording was answered anyway; one of the two matches " +
			"means the opposite of consent")
	}
}
