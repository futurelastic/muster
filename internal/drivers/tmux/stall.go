package tmux

import (
	"context"
	"fmt"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
)

// A session that reads `working` and whose screen has not changed (muster
// #391).
//
// # What was measured
//
// A session reported `working` for about 43 hours after its last turn. Five
// sends to it were each accepted and answered `queued`, and no turn ever
// started. `queued` is the answer that says "it will be handled", and `working`
// is the status that says "it will get to it" — so the caller was told the
// right things about a session that could never act on them, and nothing in the
// API separated it from one in the middle of a long turn.
//
// # The signal, and why it is only this one
//
// A running turn cannot hold still. The spinner animates and its elapsed
// counter ticks, so a live screen differs from one second to the next, and a
// turn that completes repaints the screen. A pane that is byte-for-byte the
// same for the whole bound, in a session that read `working` the whole time,
// is a runtime that stopped drawing. The observation already carries exactly
// that fact — digestSince, "when this pane was first seen showing this digest"
// (#159) — so the verdict is a comparison, not a new probe.
//
// What it deliberately is not: a guess at how long a turn should take (a
// healthy turn can run for hours and never trips this), or a read of the
// runtime's transcript (a long tool call writes nothing for minutes and the
// session is fine). Both would trade a missed hang for a false verdict, and a
// false verdict refuses a send to a session that was working.
//
// # Why a refused send names the stall instead of being queued
//
// Pasting into a runtime that has stopped reading puts text in a composer
// nobody is looking at; the receipt then says `queued` about a message that
// will sit there. The honest answer is the one a caller can act on: nothing was
// written, the session is stalled, recover it.
//
// # Limits
//
// The quiet clock is this process's own memory. Restarting the service starts
// it again, so a session that was already hung reads stalled once the bound has
// elapsed since the restart — late, never early. The verdict also needs a
// reader: the clock starts at the first read that finds the screen, so a
// session no one has read since the service started has no quiet stretch to
// measure yet, and a send to it proceeds as before.

// quietSince is the start of the stretch for which a `working` session's
// screen has held still, as far as this driver has watched. A status that began
// after the screen last changed cannot have been quiet before it began, so the
// later of the two bounds the stretch.
func quietSince(digestSince, statusSince time.Time) time.Time {
	if statusSince.After(digestSince) {
		return statusSince
	}
	return digestSince
}

// stallVerdict decides whether a session just read is stalled. digest is the
// fingerprint of the screen st was read from (empty when the capture failed),
// and digestSince when this pane was first seen showing it.
//
// Nil in every case but one: the judgement is on, the session reads `working`,
// the read saw a screen, and that screen has held still for the whole bound.
// A read that saw no screen is "no observation" (#278), never evidence of
// stillness.
func (d *Driver) stallVerdict(st fleet.SessionState, digest string, digestSince, now time.Time) *fleet.Stall {
	if d.stallAfter <= 0 || st.Status != fleet.StatusWorking || digest == "" || st.Since == nil {
		return nil
	}
	since := quietSince(digestSince, *st.Since)
	quiet := now.Sub(since)
	if quiet < d.stallAfter {
		return nil
	}
	return &fleet.Stall{Since: since, Reason: stallReason(quiet, d.stallAfter)}
}

// stallReason is the prose half of a Stall: what was seen and what the bound
// was, for a human or a log. No caller branches on it.
func stallReason(quiet, bound time.Duration) string {
	return fmt.Sprintf("the session reads working and its screen has not changed for %s "+
		"(the bound is %s); a running turn repaints every second, so the runtime has stopped drawing",
		quiet.Round(time.Second), bound)
}

// stalledOnSend reports whether a send to ref should be answered `stalled`, and
// the receipt to answer with. It is Send's gate, run before anything is written
// and before any route is chosen: a stalled runtime reads no input on any of
// them.
//
// It is built to cost nothing for the ordinary send. The first test is this
// driver's memory of the session — last seen `working` with a screen that has
// held still for the bound — and only a session that already looks stalled pays
// for the second, a fresh read of that one pane. The second is what makes the
// answer true now rather than as of the last time somebody looked: the verdict
// stands only if the pane still shows the very screen the memory holds. A
// different screen is a session that moved, and the send proceeds untouched.
//
// An error from the fresh read is not an answer. The gate steps aside rather
// than refuse on evidence it could not gather, and the delivery path that
// follows meets the same failure with its own handling.
func (d *Driver) stalledOnSend(ctx context.Context, ref fleet.SessionRef) (fleet.DeliveryReceipt, bool) {
	if d.stallAfter <= 0 {
		return fleet.DeliveryReceipt{}, false
	}
	now := d.now()
	d.mu.Lock()
	o, ok := d.observed[ref.ID]
	d.mu.Unlock()
	if !ok || o.status != fleet.StatusWorking || o.digest == "" || o.digestSince.IsZero() {
		return fleet.DeliveryReceipt{}, false
	}
	since := quietSince(o.digestSince, o.statusSince)
	if now.Sub(since) < d.stallAfter {
		return fleet.DeliveryReceipt{}, false
	}

	bounded, cancel := d.bounded(ctx)
	defer cancel()
	rows, captures, err := d.enumerate(bounded)
	if err != nil {
		return fleet.DeliveryReceipt{}, false
	}
	for _, r := range rows {
		if r.session != ref.ID {
			continue
		}
		c, captured := captures[r.paneID]
		if r.dead || !captured || screenDigest(c.text) != o.digest {
			return fleet.DeliveryReceipt{}, false
		}
		quiet := now.Sub(since)
		return d.observeEarly(delivery.Stalled, fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeStalled,
			Reason: stallReason(quiet, d.stallAfter) + ". Nothing was written: the runtime would not " +
				"have read it, and a queued answer would have promised otherwise. Recover the session " +
				"(interrupt it, or close it and resume its conversation) instead of sending again",
		}), true
	}
	return fleet.DeliveryReceipt{}, false
}
