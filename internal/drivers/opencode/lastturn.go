package opencode

import (
	"context"
	"time"

	fleet "github.com/futurelastic/muster"
)

// What a listing shows of the last turn (muster #426).
//
// A session whose newest turn ended on a provider error leaves the runtime's
// status map and reads idle. The only place the failure is written down is the
// session's own message record, and asking for it costs one request per session:
// the single-session read pays that (muster #363), a listing cannot. Measured when
// a provider ran out of credit: for about thirteen hours most of a machine's
// sessions sat on a failed turn, every listing showed them idle, and whatever
// polled the listing kept sending them work.
//
// # The record
//
// This driver therefore keeps, per session, the failure it last learned of
// (turnRecord, on knownSession) and a listing reads that. It learns of one from
// every path that already reads the message record or sees a turn end — a
// single-session read, the event bus, the read taken before a session is parked —
// and, for a listing nobody streams to, from one read of its own, made at most once
// per turn end: the record says a turn may have run since it was read (open), the
// listing finds the session idle, and it reads once and closes the record again.
// An idle session nothing has happened to costs a listing nothing.
//
// A turn may have run when this driver started one itself (Send, Create: turnStarted)
// or when a status read found the session working (sawWorking). Both open the
// record; only a read of the runtime's own reply to the turn closes it.
//
// # When a failure stops being reported
//
// The failure of the previous turn stays on the record after a Send, and only
// goes when the session is seen working or the next read replaces it. Clearing at
// the Send would show a healthy idle row for the moment between the runtime
// accepting a prompt and reporting the session working (writeSettle) — the very
// reading this exists to remove.
//
// # What this does not know
//
// The record lives in memory. After a restart it is empty and a listing omits the
// failure, as it did before, until a turn runs or a single-session read finds it.
// And a read can come early: if the runtime has not yet recorded the new turn's
// user message when a listing reads, the previous turn's reply is taken for the
// new one's; the next sighting of the session working opens the record again.
//
// A write to a record carries the generation (gen) it was read under, and one that
// finds a later generation is dropped, so a read that raced a new turn cannot put
// the old turn's answer back.

// turnReadGiveUp is how long after a record opened a listing keeps trying to read a
// session whose newest message is still not the runtime's reply, before it takes
// what it has as the answer.
const turnReadGiveUp = 30 * time.Second

// turnRecord is what is known of one session's newest turn. The zero value is
// "nothing known, nothing to read". Guarded by Driver.mu, as the rest of
// knownSession is, and written only through refreshSeen.
type turnRecord struct {
	// last is the newest finished turn's failure; nil when it succeeded or nothing
	// is known.
	last *fleet.TurnEnd
	// gen identifies the turn the record is about; it advances whenever one starts.
	// lastGen is the gen last was written under.
	gen, lastGen uint64
	// open says a turn may have run since last was read. openAt is when the record
	// opened.
	open   bool
	openAt time.Time
	// seenWorking says the runtime was seen working since the record opened.
	seenWorking bool
	// triedAt is when a listing last claimed a read, which bounds how often one is made.
	triedAt time.Time
}

// turnStarted records that this driver has just had a turn accepted: a prompt it
// sent, so the session is about to run. It also ends the interruption a restart
// left (muster #282): a new turn supersedes it.
func (d *Driver) turnStarted(id string) {
	now := d.now()
	d.refreshSeen(id, func(k *knownSession) {
		k.interrupted = nil
		t := &k.turn
		t.open, t.openAt, t.seenWorking, t.triedAt = true, now, false, time.Time{}
		t.gen++
	})
}

// sawWorking records that the runtime reported the session working: a turn is
// running, so what was kept of the last one no longer describes the newest. A
// repeat sighting of the same turn changes nothing.
func (d *Driver) sawWorking(id string) {
	now := d.now()
	d.refreshSeen(id, func(k *knownSession) {
		t := &k.turn
		if t.seenWorking {
			return
		}
		t.seenWorking, t.open, t.openAt, t.triedAt = true, true, now, time.Time{}
		t.last = nil
		t.gen++
	})
}

// turnGen is the generation a read about to start must hand back to noteTurn.
func (d *Driver) turnGen(id string) uint64 {
	k, _ := d.wasSeen(id)
	return k.turn.gen
}

// noteTurn keeps what a read of the runtime's newest message found, if it still
// describes the turn it was started for.
//
// ended says the caller knows the turn is over (the bus reported the session idle,
// the session is being parked) and so that the message read is the answer whatever
// it is. Without it a read is final only when it settled, when the record was not
// open, or when it has been open for turnReadGiveUp; otherwise the record stays
// open for a later read. A read that failed is dropped, never kept.
//
// Within one turn a read that records no failure does not remove a failure already
// kept: the bus can report a failure the message record does not show.
func (d *Driver) noteTurn(id string, gen uint64, r turnRead, ended bool) {
	if r.err != nil {
		return
	}
	now := d.now()
	d.refreshSeen(id, func(k *knownSession) {
		t := &k.turn
		if t.gen != gen {
			return
		}
		if !(r.settled || ended || !t.open || now.Sub(t.openAt) >= turnReadGiveUp) {
			return
		}
		if r.turn != nil || t.lastGen != gen || t.last == nil {
			t.last = copyTurnEnd(r.turn)
		}
		t.lastGen = gen
		t.open, t.seenWorking = false, false
	})
}

// claimTurnRead reports whether a listing should read how the session's newest
// turn ended, and if so records that it is about to, returning the generation the
// read is for. It is true at most once per writeSettle, and only for a record that
// is open and whose turn can have been recorded by now: the session was seen
// working, or the runtime has had writeSettle to record the prompt this driver sent.
func (d *Driver) claimTurnRead(id string) (uint64, bool) {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	k, ok := d.seen[id]
	if !ok {
		return 0, false
	}
	t := &k.turn
	if !t.open ||
		(!t.seenWorking && now.Before(t.openAt.Add(writeSettle))) ||
		now.Before(t.triedAt.Add(writeSettle)) {
		return 0, false
	}
	t.triedAt = now
	d.seen[id] = k
	return t.gen, true
}

// recordedTurn is how a listing reports an idle session's newest turn: the failure
// kept, else the interruption a restart left (muster #282).
func (d *Driver) recordedTurn(id string) *fleet.TurnEnd {
	k, ok := d.wasSeen(id)
	if !ok {
		return nil
	}
	if k.turn.last != nil {
		return copyTurnEnd(k.turn.last)
	}
	return copyTurnEnd(k.interrupted)
}

// readFinishedTurns is the read a listing makes of the sessions on one server whose
// turn may have ended since the record was last read: those absent from the status
// map that claimTurnRead lets through. It runs in the server's own status goroutine,
// right after the status read, so a listing gains at most one round trip and no
// goroutine outlives it. A session the read says is not settled yet is left open and
// tried again after writeSettle.
func (d *Driver) readFinishedTurns(ctx context.Context, srv *server, ids []string, statuses statusMap) {
	for _, id := range ids {
		if _, busy := statuses[id]; busy {
			continue
		}
		gen, ok := d.claimTurnRead(id)
		if !ok {
			continue
		}
		d.noteTurn(id, gen, d.turnVerdict(ctx, srv, id), false)
	}
}
