package tmux

import (
	"context"
	"fmt"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Raw key delivery for the dialogs `respond` cannot see (driver.KeySender).
//
// # The gap this fills, and the one it must not open
//
// `respond` answers a prompt the classifier RECOGNISED, by index, and refuses
// when it sees none — which is the state every full-screen dialog this driver
// does not parse leaves a session in. `send` refuses to produce a keystroke at
// all, by design. So a screen navigated with arrow keys was unreachable through
// this API, and the supervisor that met one kept a direct handle on the
// multiplexer in order to have any move at all.
//
// The danger is the mirror image of the gap. This delivers a keypress to a
// screen NOBODY CLASSIFIED, so the protections the neighbouring operations lean
// on are all absent: no prompt to check for, no nonce to compare, no option
// text to name in the receipt. Everything below is what replaces them, and each
// one is load-bearing rather than defensive:
//
//   - the caller quotes back a digest of what it read — the composer's text
//     when the composer holds unsent text, the whole screen otherwise,
//     because GET publishes exactly those two fields for exactly those two
//     cases (ComposerDigest, ScreenDigest) — and a mismatch is a refusal
//     (§5.4 — a proxy for identity is not identity, arriving here for the
//     fourth time; muster#127: this used to hash the whole screen
//     unconditionally, which a caller quoting ComposerDigest back could
//     never satisfy);
//   - a composer holding unsent text is refused outright, because `Enter` there
//     submits a human's half-typed message and `send` already refuses to touch
//     that composer for exactly this reason;
//   - a session at a prompt the classifier DID recognise is refused, because
//     `respond` can answer it with a nonce and name the option it chose, and
//     falling back to a blind arrow key would be a downgrade dressed as a
//     capability;
//   - and the delivery is confirmed by re-reading, because a key a dialog
//     swallows leaves the session exactly as stuck as before.
const (
	// keyConfirmWindow bounds how long to keep looking for the repaint that
	// says the key registered, and keyConfirmInterval how often. A dialog
	// redraws far faster than a turn produces output — this is a repaint, not
	// a round trip to anything — so the window is short and the usual answer
	// arrives on the first read.
	keyConfirmWindow   = 1 * time.Second
	keyConfirmInterval = 200 * time.Millisecond
)

// tmuxKey maps this API's closed vocabulary onto the multiplexer's key names.
//
// A map rather than passing the string through, so that the wire vocabulary and
// the substrate's vocabulary are separable and neither can quietly become the
// other. `Enter` is sent as C-m for the reason measured elsewhere in this
// driver: a prompt that swallows Enter leaves the session blocked, and C-m is
// what actually lands.
//
// `BTab` (#188) is the multiplexer's own name for Shift+Tab, and it is the one
// entry here that is not a dialog key: it cycles the runtime's permission mode
// from an IDLE composer. It rides every check Keys already makes — digest,
// recognised-prompt refusal, unsent-text refusal, composer lock, and the
// confirm-by-repaint — and is deliberately NOT caught by the arrow-key guard
// below, which exists to keep a move key off an idle composer while BTab's
// whole use is an idle composer. It is not a mode setter: `submitted` says the
// screen repainted under the key and nothing about which mode the session is now
// in. The mode is read separately, off the indicator row, and published as
// state.permissionMode (#194, permissionmode.go) — press, read that, repeat.
var tmuxKey = map[fleet.KeyName]string{
	fleet.KeyUp:     "Up",
	fleet.KeyDown:   "Down",
	fleet.KeyLeft:   "Left",
	fleet.KeyRight:  "Right",
	fleet.KeyEnter:  "C-m",
	fleet.KeyEscape: "Escape",
	fleet.KeyBTab:   "BTab",
}

// Keys delivers one raw key event to a session's screen (driver.KeySender).
func (d *Driver) Keys(ctx context.Context, req fleet.Request, ref fleet.SessionRef, key fleet.KeyName, expectDigest string) (fleet.DeliveryReceipt, error) {
	// Terminal path v2 / D4 — see Send's identical acquisition. Ahead of the
	// key-name validation below on purpose, the same way Send's own lock
	// runs ahead of its #53 guard: an unrecognised key name refuses without
	// ever touching a pane either way, so there is no cost to holding the
	// lock through it, and it keeps "acquire the lock first, always" a rule
	// with no exception to remember at the other three call sites.
	// Review fix (D4 lock ignoring the caller's deadline): Escape is the key
	// a caller reaches for to get OUT of a stuck dialog, and it is the one
	// key whose whole reason for existing is unblocking a session another
	// call has occupied for a while (confirmLandedV2/confirmSubmittedV2 can
	// each hold this session's lock for a full submitConfirmWindow). Making
	// Escape wait behind that lock would refuse the escape hatch for
	// exactly the situation it exists to escape. It still corroborates
	// against expectDigest below exactly as every other key does — skipping
	// the lock changes WHO gets to go first, not what this call is allowed
	// to assume about the screen.
	if key != fleet.KeyEscape {
		unlockComposer, lockOK := d.lockComposerOpsCtx(ctx, ref.ID)
		if !lockOK {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason:  composerBusyReason(" (Escape alone does not wait for this lock)"),
			}, nil
		}
		defer unlockComposer()
	}

	send, ok := tmuxKey[key]
	if !ok {
		// Unreachable through the HTTP surface, which validates first. Kept
		// because a driver must not send a key it was never taught: an
		// unmapped name reaching send-keys would be interpreted by the
		// multiplexer, which has a far larger vocabulary than this API does.
		return fleet.DeliveryReceipt{}, fmt.Errorf("keys: %q is not a key this driver delivers", key)
	}

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
			break
		}
	}
	if live == nil {
		return fleet.DeliveryReceipt{}, d.noSuchSession(ctx, rows, ref.ID)
	}
	if want := req.Expect.StartedAt; want != nil && !live.created.Equal(*want) {
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if live.dead {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "session process has exited"}, nil
	}

	capture, captured := captures[live.paneID]
	text := capture.text
	if !captured {
		// Not a refusal and not a success: this driver could not read the
		// screen, which is a statement about the driver rather than about the
		// session (§5.7). Pressing a key against a screen nobody could read is
		// the blind delivery this whole operation is arranged to prevent.
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"keys: could not capture this session's screen, so nothing can be corroborated")
	}

	// Which digest this call must corroborate against depends on what the
	// screen holds RIGHT NOW, decided before the expectDigest check so the
	// error messages below can already name the right field.
	//
	// composer holds unsent text -> composer scope, composerTextDigest(pending).
	// This is the SAME value GET publishes as ComposerDigest (classify.go),
	// and the SAME value Discard corroborates against (tmux.go) — chosen
	// because every key this driver could deliver into that state is refused
	// below regardless of which key was asked for, so the corroboration only
	// ever has to prove the caller saw the composer, never the rest of the
	// screen (muster#127).
	//
	// composer empty (or absent) -> screen scope, screenDigest(text), same as
	// before this change. A composer-scope digest here would be the constant
	// hash of "" no matter what a dialog says, which corroborates nothing —
	// exactly the relaxation this driver must not make. GET publishes this as
	// ScreenDigest, unconditionally.
	// muster#134: composerHoldsText requires scan == composerFound, so
	// a composerClipped screen falls into the SAME digest scope as an
	// absent composer — screenDigest(text), below. That is deliberate, not
	// an oversight: this driver cannot form a meaningful COMPOSER-scope
	// digest for a clipped composer (there is no text to hash), but the
	// SCREEN it captured is read in full regardless of the composer's own
	// legibility, so a screen-scope digest still corroborates something
	// real — "the caller saw this exact, possibly-ambiguous screen" — the
	// same property screenDigest already proves for every other composer
	// state that is not "found, holding text".
	screen := capture.screen()
	pending, scan := composerText(screen)
	composerHoldsText := scan == composerFound && strings.TrimSpace(pending) != ""

	if expectDigest == "" {
		if composerHoldsText {
			return fleet.DeliveryReceipt{}, fmt.Errorf(
				"%w: refusing to press %s on a composer the caller has not read; "+
					"supply the composerDigest from a read as ?expect=<composerDigest> "+
					"(the same field discard uses)", ErrAmbiguousTarget, key)
		}
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"%w: refusing to press %s on a screen the caller has not read; supply "+
				"the screenDigest from a read as ?expect=<screenDigest> (a query "+
				"parameter, where startedAt goes)", ErrAmbiguousTarget, key)
	}

	var before string
	if composerHoldsText {
		before = composerTextDigest(pending)
	} else {
		before = screenDigest(text)
	}
	// #180 L1: a caller may hold a composer digest read before an upgrade.
	matches := before == expectDigest
	if composerHoldsText {
		matches = composerDigestMatches(expectDigest, pending)
	}
	if !matches {
		if composerHoldsText {
			return fleet.DeliveryReceipt{}, fmt.Errorf(
				"%w: the composer changed since the caller read it (expected "+
					"composerDigest %s, found %s); a key sent now would be acting on "+
					"unsent text nobody has re-read", ErrAmbiguousTarget, expectDigest, before)
		}
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"%w: the screen changed since the caller read it (expected screenDigest %s, "+
				"found %s); a key sent by position now would be applied to a "+
				"different screen", ErrAmbiguousTarget, expectDigest, before)
	}

	// A recognised prompt has a better answer than this one. `respond` checks a
	// nonce, chooses by index, and names the option it took in the receipt;
	// arrow keys can do none of that, so accepting the fallback here would let
	// a caller silently trade all three away.
	//
	// "Recognised" means what a state read of THIS screen publishes, decided by
	// the same function with the same remembered screen history — never a
	// second, stricter reading of its own (#159). This used to ask parsePrompt
	// directly, which knows nothing of #58's corroboration hold: for a screen
	// the read was still holding at `unknown` (prompt null, no nonce), keys
	// refused because "a prompt exists" while respond had no nonce to verify
	// against. The documented contract then left a caller no verified move.
	// One classifier, one answer: the refusal fires exactly when a read would
	// have handed the caller a prompt and its nonce.
	now := d.now()
	d.mu.Lock()
	resolved, _ := classifyCaptureRemembering(capture, true, true,
		now.Sub(live.created) < startingWindow, d.memoryLocked(live.session), now)
	d.mu.Unlock()
	if resolved.Prompt != nil {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "this session is at a prompt the driver recognises; answer it " +
				"through respond, which verifies a nonce and can say which option " +
				"it chose (read the session again for the prompt and its nonce)",
		}, nil
	}

	// muster#215: the feedback-draft card over a composer row that holds
	// text. Not a prompt, so the refusal above did not fire, and the composer is
	// not readable as a whole, so the checks below would see an absent one. A key
	// sent now is appended to that text or read by the card.
	//
	// muster#217 decided what Escape does here. Over a card this driver
	// cannot read past, it is the one key accepted, in the three states it
	// measurably leaves: the key row (dismisses the card; the draft stays queued),
	// the send confirmation (returns to the key row) and the send error
	// (dismisses). Enter there could submit text nobody read, and the arrows drive
	// the runtime. Not while sending: what Escape does to a request in flight was
	// not measured. Where the card is the key row over an empty composer that can
	// be read, it is a prompt, respond answers it (dismiss is its own verb), and
	// the refusal above already sent the caller there; with a composer that reads
	// whole the card blocks nothing, and Escape falls through to the delivery
	// below with a note saying what it did.
	if c, isCard := liveFeedbackCard(screen); isCard && !c.composerFound {
		steps := key == fleet.KeyEscape && c.rowText == "" && c.state != feedbackSending
		if !steps {
			d.counters.incr(counterFeedbackCardRefusedKeys)
			reason := c.refusal()
			if c.rowText == "" && c.state != feedbackSending {
				reason += "; only Escape is accepted through keys here, and it dismisses the card " +
					"or steps back from the confirmation"
			}
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
		}
	}
	// The feedback panel replaces the composer. Escape is how a person leaves it,
	// and it decides nothing; every other key acts on a draft (Enter opens one or
	// sends it, transcript included).
	if reason, onPanel := feedbackPanelRefusal(screen); onPanel && key != fleet.KeyEscape {
		d.counters.incr(counterFeedbackCardRefusedKeys)
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
	}
	note := ""
	if key == fleet.KeyEscape {
		note = feedbackEscapeNote(screen)
	}

	// muster#134: a composer taller than this driver's capture window,
	// with no recognised prompt to route to instead (that case already
	// returned, above). Refuse rather than guess — a key sent now could
	// submit text this driver never saw, the same hazard composerHoldsText
	// guards below, just for a composer this driver could not read rather
	// than one it read and found busy.
	if scan == composerClipped {
		d.counters.incr(counterComposerClippedRefusedKeys)
		d.countClippedCause(screen)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "this composer " + composerClippedCause(screen) + ", so its " +
				"content could not be read in full; a key delivered now could submit " +
				"text nobody has seen, and discard refuses for the same reason. " +
				clippedComposerRemedy,
		}, nil
	}

	// Unsent text in the composer. `Enter` submits it, and it is not this
	// caller's to submit — the runtime redraws a composer identically whether a
	// human typed into it or a delivery stranded text there, so the only safe
	// reading is that somebody meant it.
	if composerHoldsText {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "the composer holds unsent text; a key delivered now could submit " +
				"something nobody asked to send. Clear it with discard, or send it",
		}, nil
	}

	// #180 L7: arrow keys exist for dialogs. On an idle, empty composer with
	// no dialog on screen they drive the runtime itself — Left was measured
	// opening its agent view and starting a background supervisor.
	if scan == composerFound && !awaitingSelection(screen) {
		switch key {
		case fleet.KeyUp, fleet.KeyDown, fleet.KeyLeft, fleet.KeyRight:
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "arrow keys are for answering a dialog, and this session shows an " +
					"empty composer with no dialog; on an idle composer an arrow drives the " +
					"runtime's own interface instead (Left opens its agent view) — refusing",
			}, nil
		}
	}

	if _, err := d.run(ctx, d.bin, "send-keys", "-t", live.paneID, send); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("keys: %w", err)
	}

	// Confirm by looking. A key that landed on a dialog changes it; one the
	// dialog swallowed does not. Reporting the second as submitted is how a
	// supervisor concludes it has moved a selection it has not moved.
	//
	// Read FIRST, then decide whether to wait again — the same order
	// promptCleared uses, and for the same reason: an operation whose deadline
	// has already passed should still make the one observation it came for
	// rather than reporting "could not confirm" without having tried.
	//
	// Re-read through the SAME path the first reading came from. A digest is
	// only a comparison if both sides were produced identically, and this
	// driver has two capture shapes for reasons of their own — comparing
	// across them would report "changed" for a screen that did not, which is
	// the one direction of this confirmation that must never be wrong.
	deadline := d.now().Add(keyConfirmWindow)
	for {
		if _, afterCaptures, err := d.enumerate(ctx); err == nil {
			if after, reread := afterCaptures[live.paneID]; reread && screenDigest(after.text) != before {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeSubmitted,
					Reason:  "sent " + string(key) + "; the screen changed in response" + note,
				}, nil
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(keyConfirmInterval):
			continue
		}
		break
	}

	// Honest rather than convenient. A legitimate no-op — Down at the bottom of
	// a list — reports the same way, because from outside the dialog the two
	// are the same observation, and inventing a distinction here would mean
	// claiming to know what the dialog is.
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason: "sent " + string(key) + "; the screen did not change, so the key was " +
			"either swallowed or had nothing to do" + note,
	}, nil
}
