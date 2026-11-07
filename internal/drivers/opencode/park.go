package opencode

import (
	"context"
	"fmt"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Parking an idle server (muster #421).
//
// An opencode server that has nothing to do still holds its memory: several
// hundred megabytes for a small session, more for a long one, for as long as the
// session lives. #419 made an idle server cheap to ask; it did nothing for what
// it holds. With a bound set (WithIdleStop), a session whose server has had
// nothing to do for that long is PARKED: the server's process group is stopped
// the way the service stopping stops it, and the session keeps its record and its
// scoped directory, which is all a start needs. The next Send, Interrupt or
// Respond starts a server on the same directory again — the path a restart already
// uses (relaunch) — and delivers.
//
// # The third meaning of "the process is gone"
//
// Until now a server whose process had exited meant one thing: the session is
// dead. A parked one is not. The driver keeps knownSession.srv pointing at the
// stopped server, and server.park says why it stopped: the one fact every reader
// of "has it exited" must consult first. phaseOf is the only classifier, so no
// reader tells the three states apart on its own:
//
//   - running: ask it.
//   - parked:  idle, with evidence that says so. Answered from memory, never by
//     starting anything: a state read, a listing, a subscription and the
//     service's holder probe must not undo the park.
//   - exited:  dead, as before.
//
// # Who may park, and when
//
// Only an isolated session with a record on disk, whose record can be relaunched
// as it was created (no caller-supplied environment): a session that could not
// come back is never stopped. It is parked when, for the bound, this driver wrote
// nothing to it and its event stream said nothing about it, AND a fresh read —
// not the #419 cache, which may be thirty seconds old — shows no turn in progress,
// no permission ask and no question. A read that fails is not an answer: the
// session stays up and is looked at again.
//
// A turn nobody was watching leaves no trace in lastActive. A check that finds the
// session busy therefore marks the server watched, and a watched server is checked
// on every pass until it is first seen idle; that moment is when its quiet time
// starts, so a long unobserved turn is still waited out for a whole bound.
//
// # Across a service restart (muster #423)
//
// A park is written to the session's record (persist.go, parkedRecord) before the
// process is stopped, and the write that records the process started in its place
// clears it. At the service's start, with the bound still set and not lower than
// the one the park was decided under, resumeOne rebuilds the stopped server from
// the record instead of starting a process (parkedServerFor, resume.go): the
// session is listed idle and parked, and its first input pays the start any parked
// session pays. A record the restart cannot trust — no bound now, a lower one, a
// start that would not succeed as things are — is started the ordinary way, so no
// session is ever left parked that the service could not start again.
//
// # Locking
//
// With the bound set, every operation that writes to a session, and Close, runs
// under that session's lock (lockSession); the sweep takes it with TryLock and
// skips a session that is busy being written to. Lock order: the session lock
// first, then d.mu, d.subMu and the leaf locks below it; nothing takes a session
// lock while holding one of those. With the bound unset the lock is not taken at
// all, so a driver without the knob behaves exactly as it did.
//
// A send that races the sweep is safe whichever wins: if the park holds the lock,
// the send waits, finds the session parked and revives it; if the send holds it,
// the sweep skips, and the send has just made the session active.

// defaultIdleSweepEvery is how often the sweep looks for sessions to park. The
// bound itself is minutes, so a pass a minute is as fine as it needs to be.
const defaultIdleSweepEvery = time.Minute

// parkReadTimeout bounds the fresh reads a park decision makes, together. A server
// that does not answer inside it is not parked.
const parkReadTimeout = 5 * time.Second

// phase is what a session's server is doing, as far as this driver can tell.
type phase int

const (
	// phaseNone: the session has no server at all (listed, not running).
	phaseNone phase = iota
	// phaseRunning: the server's process is up — or, in shared mode, is not this
	// driver's to watch.
	phaseRunning
	// phaseParked: this driver stopped the server on purpose; the session is idle
	// and the next input starts it again.
	phaseParked
	// phaseExited: the process is gone and nobody stopped it for the session's sake.
	phaseExited
)

// phaseOf classifies srv. The order is the point: a parked server's process has
// exited too, and must not be read as the session's death.
func (d *Driver) phaseOf(srv *server) phase {
	switch {
	case srv == nil:
		return phaseNone
	case d.shared != nil:
		return phaseRunning
	case srv.park.Load() != nil:
		return phaseParked
	case srv.proc.exited():
		return phaseExited
	}
	return phaseRunning
}

// parkNote is what a parked server remembers of why and when it was stopped. It is
// never changed after it is stored: a failed revive stores a copy with the failure.
type parkNote struct {
	at      time.Time
	idleFor time.Duration
	// lastTurn is how the session's newest turn ended, read just before the stop, so
	// a parked session reports it without a process to ask.
	lastTurn *fleet.TurnEnd
	// reviveErr and reviveAt are set when starting the session again failed.
	reviveErr string
	reviveAt  time.Time
	// restored is set on a note rebuilt from the session's record at a service
	// restart (muster #423): the stop was an earlier service's, and the runtime has
	// not been asked anything about the session since.
	restored bool
}

// record is the part of the note that is kept on disk, under the bound that decided
// the park.
func (n *parkNote) record(bound time.Duration) *parkedRecord {
	return &parkedRecord{
		AtMs: n.at.UnixMilli(), IdleForMs: n.idleFor.Milliseconds(), BoundMs: bound.Milliseconds(),
		LastTurn: copyTurnEnd(n.lastTurn),
	}
}

// noteFromRecord is the parkNote a restart rebuilds from a record that says parked.
func noteFromRecord(p *parkedRecord) *parkNote {
	return &parkNote{
		at: time.UnixMilli(p.AtMs), idleFor: time.Duration(p.IdleForMs) * time.Millisecond,
		lastTurn: copyTurnEnd(p.LastTurn), restored: true,
	}
}

// touch records that the server's session did something now.
func (s *server) touch(at time.Time) { s.lastActive.Store(at.UnixNano()) }

// WithIdleStop sets how long a session's server may have nothing to do before this
// driver stops it to free its memory (muster #421). Zero, the default, never stops
// one. It applies only with a state directory: a session is started again from its
// record, and without one it could not be.
func WithIdleStop(d time.Duration) Option { return func(dr *Driver) { dr.idleStop = d } }

// withIdleSweep sets how often the sweep runs. Unexported: a test seam.
func withIdleSweep(every time.Duration) Option { return func(d *Driver) { d.idleSweepEvery = every } }

// parkedState is what a state read of a parked session answers: idle, observed —
// the park itself read it idle, and no process exists to have started anything
// since — with the evidence that it is parked.
func (d *Driver) parkedState(k knownSession) fleet.SessionState {
	note := k.srv.park.Load()
	ev := "parked: the runtime's process was stopped after a period with nothing to do, to free its memory; " +
		"the next input starts it again"
	if note != nil {
		ev = fmt.Sprintf("parked: the runtime's process was stopped after %s with nothing to do, to free its memory; "+
			"the next input starts it again", note.idleFor.Round(time.Second))
		if note.restored {
			ev += "; the service has restarted since and left it stopped"
		}
		if note.reviveErr != "" {
			ev += fmt.Sprintf("; starting it again failed at %s: %s", note.reviveAt.UTC().Format(time.RFC3339), note.reviveErr)
		}
	}
	st := fleet.ObservedState(fleet.StatusIdle, ev, nil)
	if note != nil {
		st.LastTurn = copyTurnEnd(note.lastTurn)
	}
	if st.LastTurn == nil {
		st.LastTurn = k.interrupted
	}
	return st
}

// parkedError is the refusal for a read that would need the process.
func (d *Driver) parkedError(what string) error {
	return &fleet.Error{
		Kind: fleet.ErrorConflict,
		Message: what + ": this session is parked — its runtime process was stopped to free memory — and a read does not start it. " +
			"Send it input to start it again",
		Machine: d.machine,
	}
}

// lifeLock is the lock of one session's lifecycle: park, revive and the writes that
// must not interleave with them.
func (d *Driver) lifeLock(id string) *sync.Mutex {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	if d.life == nil {
		d.life = map[string]*sync.Mutex{}
	}
	m := d.life[id]
	if m == nil {
		m = &sync.Mutex{}
		d.life[id] = m
	}
	return m
}

// dropLifeLock forgets a session's lock once the session is gone. Anyone still
// waiting on it finds the session gone too.
func (d *Driver) dropLifeLock(id string) {
	d.lifeMu.Lock()
	delete(d.life, id)
	d.lifeMu.Unlock()
}

// lockSession takes the session's lifecycle lock and returns its release. With no
// bound set nothing can be parked, so nothing is locked: the unlock is a no-op.
func (d *Driver) lockSession(id string) func() {
	if d.idleStop <= 0 {
		return func() {}
	}
	if _, known := d.wasSeen(id); !known {
		// Nothing to protect, and no lock to make for an id a client invented: the
		// caller finds the session unknown and says so.
		return func() {}
	}
	m := d.lifeLock(id)
	m.Lock()
	return m.Unlock
}

// liveFor is what every write to a session starts with: under the session's lock,
// it checks the session exists and is running, starts it again if it was parked,
// and records that it is active. The caller holds the lock until the write is
// done, by calling unlock; on an error there is nothing to unlock.
func (d *Driver) liveFor(ctx context.Context, id string) (knownSession, func(), error) {
	unlock := d.lockSession(id)
	k, ok := d.wasSeen(id)
	if !ok {
		unlock()
		return knownSession{}, nil, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, id)
	}
	if err := d.requireLive(k); err != nil {
		unlock()
		return knownSession{}, nil, err
	}
	if d.phaseOf(k.srv) == phaseParked {
		nk, err := d.revive(ctx, id, k)
		if err != nil {
			unlock()
			return knownSession{}, nil, err
		}
		k = nk
	}
	k.srv.touch(d.now())
	return k, unlock, nil
}

// revive starts a parked session's server again, on the directory it kept, and puts
// it in the place of the stopped one. The caller holds the session's lock.
//
// If it cannot be started the session STAYS parked — idle, with the failure in its
// evidence — and the write that needed it fails with a retryable error and nothing
// sent. A session listed unknown is only tried again at a service restart, while
// every reason a start fails here is either passing (a port taken, the runtime slow)
// or something the operator can fix without one (an agents file, a sessionEnv file),
// so the next write simply tries again.
func (d *Driver) revive(ctx context.Context, id string, k knownSession) (knownSession, error) {
	old := k.srv
	fail := func(cause string) (knownSession, error) {
		d.noteReviveFailure(old, cause)
		return knownSession{}, &fleet.Error{
			Kind:      fleet.ErrorUnreachable,
			Retryable: true,
			Message:   "this session was parked after being idle and could not be started again: " + cause + "; nothing was done",
			Machine:   d.machine,
		}
	}
	if d.store == nil {
		return fail("no state directory holds its record")
	}
	rec := d.store.load(id)
	if rec == nil {
		return fail("its record is gone")
	}
	rctx, cancel := d.bounded(ctx)
	defer cancel()
	srv, _, err := d.relaunch(rctx, rec)
	if err != nil {
		return fail(err.Error())
	}

	d.mu.Lock()
	cur, still := d.seen[id]
	closed := d.closed
	if still && !closed && cur.srv == old {
		cur.srv = srv
		cur.interrupted = nil
		if n := old.park.Load(); n != nil && n.restored {
			// relaunch read the session back by this id, which is the confirmation a
			// session left parked by a restart was waiting for (muster #423).
			cur.conversation = confirmedAfterRestart(id)
		}
		d.seen[id] = cur
	}
	d.mu.Unlock()
	switch {
	case closed:
		srv.pauseForShutdown()
		return knownSession{}, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: "this driver has been shut down", Machine: d.machine}
	case !still:
		srv.pauseForShutdown()
		return knownSession{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, id)
	case cur.srv != srv:
		// Another path replaced the server while this one started: not this call's to undo.
		srv.pauseForShutdown()
		return fail("the session's server changed while it was starting")
	}
	srv.touch(d.now())
	d.announceRevived(srv, cur, fleet.SessionRef{Machine: d.machine, ID: id, Name: cur.name})
	d.say("opencode: started parked session %s again for an incoming request", id)
	return cur, nil
}

// noteReviveFailure keeps the failure on the parked server, as a new note.
func (d *Driver) noteReviveFailure(srv *server, cause string) {
	prev := srv.park.Load()
	next := parkNote{}
	if prev != nil {
		next = *prev
	}
	next.reviveErr, next.reviveAt = cause, d.now()
	srv.park.Store(&next)
}

// announceRevived connects every open stream that follows the session to its new
// server, before anything is sent to it, so the first change of the turn that
// started it is not missed. It is announceCreated without the announcement: the
// session has existed all along, and a stream has been told it is idle throughout.
func (d *Driver) announceRevived(srv *server, info knownSession, ref fleet.SessionRef) {
	d.subMu.Lock()
	subs := make([]*subscription, 0, len(d.subs))
	for s := range d.subs {
		subs = append(subs, s)
	}
	d.subMu.Unlock()
	for _, s := range subs {
		if !s.filter.Matches(ref.ID, string(info.cwd)) {
			continue
		}
		if err := s.attach(srv); err != nil {
			// The session is fine; this stream is the one that cannot follow it, and
			// says so by ending, so its consumer reads again from a fresh baseline.
			s.fail(err)
		}
	}
}

// startIdleSweep starts the goroutine that parks idle sessions. It runs only with a
// bound and a state directory, and stops with Shutdown.
func (d *Driver) startIdleSweep() {
	if d.idleStop <= 0 || d.store == nil || d.idleStopCh != nil {
		return
	}
	every := d.idleSweepEvery
	if every <= 0 {
		every = defaultIdleSweepEvery
	}
	d.idleStopCh = make(chan struct{})
	d.idleDone = make(chan struct{})
	stop, done := d.idleStopCh, d.idleDone
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			d.mu.RLock()
			closed := d.closed
			d.mu.RUnlock()
			if closed {
				return
			}
			d.parkIdle()
		}
	}()
}

// stopIdleSweep ends the goroutine and waits for it, so no park overlaps what
// Shutdown does next. Safe to call any number of times.
func (d *Driver) stopIdleSweep() {
	if d.idleStopCh == nil {
		return
	}
	d.idleStopOnce.Do(func() { close(d.idleStopCh) })
	<-d.idleDone
}

// parkIdle is one pass: every running session that has been quiet for the bound, or
// that is being watched, is looked at, and parked if it is idle. It works from a
// snapshot, so no lock is held while it walks.
func (d *Driver) parkIdle() {
	if d.idleStop <= 0 || d.store == nil || d.shared != nil {
		return
	}
	now := d.now()
	for id, k := range d.knownIDs() {
		if d.idleStopCh != nil {
			select {
			case <-d.idleStopCh:
				return
			default:
			}
		}
		srv := k.srv
		if d.phaseOf(srv) != phaseRunning {
			continue
		}
		quiet := now.Sub(time.Unix(0, srv.lastActive.Load()))
		if quiet < d.idleStop && !srv.watch.Load() {
			continue
		}
		d.tryPark(id, srv)
	}
}

// tryPark parks one session if it is idle and has been for the bound. It reports
// whether it did. A session whose lock is held is being written to: skipped.
func (d *Driver) tryPark(id string, srv *server) bool {
	m := d.lifeLock(id)
	if !m.TryLock() {
		return false
	}
	defer m.Unlock()

	k, seen := d.wasSeen(id)
	if !seen {
		d.dropLifeLock(id) // closed since the pass took its snapshot
		return false
	}
	if k.srv != srv || d.phaseOf(srv) != phaseRunning || !srv.persisted.Load() {
		return false
	}
	rec := d.store.load(id)
	if rec == nil || rec.recreateReason() != "" {
		return false // it could not be started again as it was: never stop it
	}

	ctx, cancel := context.WithTimeout(context.Background(), parkReadTimeout)
	defer cancel()
	idle, err := d.idleNow(ctx, srv, id)
	now := d.now()
	switch {
	case err != nil:
		return false // not knowing is not idle
	case !idle:
		srv.touch(now)
		srv.watch.Store(true)
		return false
	case srv.watch.Load():
		// First seen idle after being seen busy: its quiet time starts now.
		srv.touch(now)
		srv.watch.Store(false)
		return false
	}
	quiet := now.Sub(time.Unix(0, srv.lastActive.Load()))
	if quiet < d.idleStop {
		return false
	}

	// What the parked state will report, read while there is still a process to ask.
	lastTurn := d.lastTurnFailure(ctx, srv, id)
	d.usageOf(ctx, srv, id, true)
	// A bus event may have arrived while those reads ran.
	if d.now().Sub(time.Unix(0, srv.lastActive.Load())) < d.idleStop {
		return false
	}

	note := &parkNote{at: d.now(), idleFor: quiet, lastTurn: lastTurn}
	// muster #423: the record says so before the process stops, so a restart that
	// finds the process gone finds the reason beside it. The write is the one place a
	// park can fail for want of disk: the session is parked regardless, because
	// freeing the memory is the point and a record without the fact only means the
	// next restart starts the session, as it always did.
	rec.Parked = note.record(d.idleStop)
	if err := d.store.write(rec); err != nil {
		d.say("opencode: parked session %s but could not record it, so a service restart will start it again: %v", id, err)
	}
	// Before the stop: whoever then sees the process gone also sees why.
	srv.park.Store(note)
	srv.pauseForShutdown()
	d.say("opencode: parked session %s after %s with nothing to do", id, quiet.Round(time.Second))
	return true
}

// idleNow reads, without the cache, whether session id has nothing to do on srv: it
// is not in the runtime's status map, and the runtime holds no permission ask and no
// question for it. A runtime that has no such endpoint holds none. Any other failed
// read is an error: the caller must not read it as idle.
func (d *Driver) idleNow(ctx context.Context, srv *server, id string) (bool, error) {
	var statuses statusMap
	if err := d.do(ctx, srv, "GET", "/session/status", nil, &statuses); err != nil {
		return false, err
	}
	if ws, busy := statuses[id]; busy && ws.Type != "idle" {
		return false, nil
	}
	var asks []wirePermissionAsk
	if err := d.do(ctx, srv, "GET", "/permission", nil, &asks); err != nil && !isNotFound(err) {
		return false, err
	}
	if askListedFor(asks, id) != nil {
		return false, nil
	}
	var qs []wireQuestionAsk
	if err := d.do(ctx, srv, "GET", "/question", nil, &qs); err != nil && !isNotFound(err) {
		return false, err
	}
	for i := range qs {
		if qs[i].SessionID == id {
			return false, nil
		}
	}
	return true, nil
}
