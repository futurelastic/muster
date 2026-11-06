package tmux

import (
	"context"
	"fmt"
	"time"

	fleet "github.com/futurelastic/muster"
)

// A delivery whose session's process exits while the delivery is being
// confirmed (muster #411).
//
// The measured case is a /quit sent to a session: the runtime reads it, the
// process exits with status 0, and the confirmation that was waiting for a turn
// or an emptied composer never comes. Both delivery paths used to read that
// silence as "the submit did not register" or "the module could not confirm", so
// the receipt said the text was sitting there unsent and advised a retry — on a
// session that no longer exists, and after writing a stranded record for it.
//
// endedReceipt asks the one question that tells those apart, and only AFTER a
// confirmation has already failed: is the session still there? It is never asked
// on the happy path, so a delivery that confirms costs nothing extra.

const (
	// recentExitsCap and recentExitsTTL bound the driver's memory of how
	// sessions last exited. The service drains a captured exit into its history
	// within one poll — usually inside a send's own confirmation window — so by
	// the time a failed confirmation looks, the exit is no longer pending and the
	// pane is no longer listed. Only this memory still knows the status. It is
	// short on purpose: it answers "did the session I just wrote to exit, and
	// how", not "what happened to that session earlier".
	recentExitsCap = 64
	recentExitsTTL = 10 * time.Minute
)

// recentExit is one remembered exit.
type recentExit struct {
	exit  fleet.SessionExit
	noted time.Time
}

// rememberExitLocked records an exit in the bounded memory. Callers hold
// exitedMu (notePendingExit, its only caller, already does). The oldest entry
// goes first when the cap is reached, and an expired one is dropped on the way.
func (d *Driver) rememberExitLocked(id string, e fleet.SessionExit) {
	now := d.now()
	if d.recentExits == nil {
		d.recentExits = make(map[string]recentExit)
	}
	for k, v := range d.recentExits {
		if now.Sub(v.noted) > recentExitsTTL {
			delete(d.recentExits, k)
		}
	}
	for len(d.recentExits) >= recentExitsCap {
		oldest, first := "", true
		var at time.Time
		for k, v := range d.recentExits {
			if first || v.noted.Before(at) {
				oldest, at, first = k, v.noted, false
			}
		}
		delete(d.recentExits, oldest)
	}
	d.recentExits[id] = recentExit{exit: e, noted: now}
}

// recentExit returns how session id last exited, if this driver captured it
// within recentExitsTTL.
func (d *Driver) recentExit(id string) (fleet.SessionExit, bool) {
	d.exitedMu.Lock()
	defer d.exitedMu.Unlock()
	if r, ok := d.recentExits[id]; ok && d.now().Sub(r.noted) <= recentExitsTTL {
		return r.exit, true
	}
	for _, p := range d.exitedPending {
		if p.ID == id {
			return p.Exit, true
		}
	}
	return fleet.SessionExit{}, false
}

// endedReceipt reports whether session ref has ended, and if so the receipt a
// delivery to it should answer with. It reads the multiplexer once, on ctx:
//
//   - the pane is listed and dead — the process exited and remain-on-exit is
//     holding the pane; the exit status is the pane's own.
//   - the pane is not listed — a List already reaped it, or the session was
//     closed; the status is whatever the recent-exit memory holds, else the
//     receipt says none was recorded.
//   - the pane is listed and alive, or the read failed (including a ctx that has
//     run out) — not ended, as far as can be shown. The caller keeps the
//     "unknown" it was about to answer with. Never claims an end on a read that
//     did not complete.
//
// The session was alive at the top of this delivery (a dead pane is refused
// before anything is pasted), so a pane that is dead or gone now went away
// during it. The receipt says exactly that and nothing about whether the text
// was read.
func (d *Driver) endedReceipt(ctx context.Context, ref fleet.SessionRef) (fleet.DeliveryReceipt, bool) {
	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.DeliveryReceipt{}, false
	}
	var row *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			row = &rows[i]
			break
		}
	}
	var (
		status int
		known  bool
	)
	switch {
	case row != nil && !row.dead:
		return fleet.DeliveryReceipt{}, false
	case row != nil:
		status, known = row.deadStatus, true
	default:
		if e, ok := d.recentExit(ref.ID); ok {
			status, known = e.Status, true
		}
	}
	how := "(no exit status was recorded for it)"
	if known {
		how = fmt.Sprintf("(exit status %d)", status)
	}
	d.counters.incr(counterSendEnded)
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeEnded,
		Reason: "the session's process exited " + how + " while this delivery was being confirmed, so there is nothing " +
			"left to confirm it against, and no record of it was kept. Whether the text was read before the exit " +
			"cannot be established from here; the session's closed record and transcript say. It has not been sent " +
			"again, and it should not be: the session no longer exists, so a retry has nowhere to land",
	}, true
}
