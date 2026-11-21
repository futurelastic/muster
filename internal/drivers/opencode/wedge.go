package opencode

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Replacing a server that answers 5xx for ever (muster #513).
//
// A runtime's database connection can break under a load spike and never mend: one
// storage I/O error, then every read of the session and every prompt answers 500 for
// hours, while the files under it are healthy and a fresh process on the same
// directory works at once. Until now the driver reported such a session red and left
// it, and the work stopped until a person killed and re-created it.
//
// The driver now notices, stops that server, starts another on the same directory and
// reattaches the session, a bounded number of times, and says so.
//
// # What counts as wedged
//
// Every answer a server gives this driver passes through do (client.go), so that is
// where it is counted (noteAnswer), per server — in the isolated mode one server is
// one session — and the same way whoever asked: a state read, a listing's poller, a
// send, a turns read.
//
//   - Only a path that reads or writes the session's STORE is evidence (storeBacked).
//     A 5xx on one adds to a run of failures; a healthy answer from one ends the run.
//     An answer from any other path — the status map, the permission and question
//     lists, the provider catalog, all kept in memory or in configuration — neither
//     adds to a run nor ends it. This is the point of the distinction, in both
//     directions: the measured failure answered every read of the session and every
//     prompt with 500 while the endpoints that never touch the store went on
//     answering, so a poller that reads the status map between two failed reads would
//     otherwise reset the run for ever and the server would never be seen to be
//     broken; and a runtime whose permission list alone fails has a working store, and
//     replacing it would trade a failure it can live with for a restart it cannot.
//   - A transport failure, a refusal (4xx) of a store path counted as healthy: the
//     store answered. A request that never reached the server, or whose context ended,
//     says nothing about it and is not seen here at all.
//   - The run decides when it is wedgeRun long AND has lasted wedgeSpan, on the
//     driver's clock: three reads that fail together under one load blip are not a
//     wedge, three that keep failing for twenty seconds are. If the runtime's own log
//     has named a storage I/O failure since the store last answered, the run needs only
//     wedgeFastRun and no span: the log is the evidence the span waits for. One failed
//     answer never decides anything.
//   - A server whose failures are the moved-directory cause (muster #474) is not
//     wedged: replacing it would not help and that cause is named already.
//
// # What a restart does
//
// It is its own goroutine under the session's lifecycle lock (lockLifecycle), never
// inside the request that tripped it: that request may be a write holding the same
// lock, and a read must not be the thing that starts a process. The server's wedge
// note is stored BEFORE the process is stopped — whoever sees the process gone also
// sees why, as with a park — and phaseOf reads it as phaseWedged, so no reader calls
// the session dead for a stop this driver made.
//
//   - Restart: the record names the restart first (the budget survives a service
//     restart), the process stops, relaunch starts another on the same directory and
//     confirms the runtime holds the session, and the new server takes the old one's
//     place and every open stream.
//   - A turn in flight: the request that met the 500 already returned a retryable
//     error and is never sent again — the runtime may have stored it, and a second
//     delivery would be a duplicate. A turn the runtime had not finished is reported
//     interrupted, as after a service restart. Nothing is held in a queue here: what
//     the runtime accepted is in its store, which the new process reads.
//   - A session created with caller-supplied environment cannot be started again by
//     this driver, which never kept the values (muster #282). Its server is stopped
//     and it is listed unknown with the cause; its caller revives it by handing the
//     values over again (revive.go, muster #427), the one thing a revive clears here.
//     It spends no budget: nothing was restarted.
//   - A budget of wedgeBudgetMax restarts within wedgeBudgetWindow. A session that
//     wedges again after that is stopped and listed unknown, with the cause and what
//     the budget was, rather than restarted for ever on a fault the new process shares.
//     A restart of the service tries it once more. A restart that fails leaves it
//     listed unknown with the failure, like a relaunch at a service start, and counts
//     in the same figure (noteRelaunch).
//
// # What it says
//
// One line in the driver's log per restart or give-up, carrying the session id, the
// cause and how much of the budget is used; a session.state event; the same text in
// the listing; and the record's wedge field. The cause names the failure and nothing
// the runtime wrote: the log is read only for whether such a line exists.
const (
	wedgeRun          = 3
	wedgeFastRun      = 2
	wedgeSpan         = 20 * time.Second
	wedgeBudgetMax    = 3
	wedgeBudgetWindow = time.Hour
)

// wedgeNote is what a server remembers of why it is being replaced. It is never
// changed after it is stored.
type wedgeNote struct {
	at    time.Time
	cause string
}

// wedgeRecord is the part of this that outlives the service: when the driver
// restarted the session's server (so the budget is not forgotten by a restart), and
// when it last gave up, with the cause. Times only, in milliseconds.
type wedgeRecord struct {
	RestartsMs []int64 `json:"restartsMs,omitempty"`
	Cause      string  `json:"cause,omitempty"`
	GaveUpAtMs int64   `json:"gaveUpAtMs,omitempty"`
}

// wedgeHealth is the run of failures one server is judged by.
type wedgeHealth struct {
	mu    sync.Mutex
	run   int
	first time.Time
	// ioerrBase is how many I/O failure lines the log had when the store last answered.
	ioerrBase int64
}

// healthy ends the run: the store answered.
func (h *wedgeHealth) healthy(ioerr int64) {
	h.mu.Lock()
	h.run, h.first, h.ioerrBase = 0, time.Time{}, ioerr
	h.mu.Unlock()
}

// failed adds one 5xx at now and reports the run, how long it has lasted, and whether
// the log named a storage I/O failure since the store last answered.
func (h *wedgeHealth) failed(now time.Time, ioerr int64) (run int, span time.Duration, io bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.run == 0 {
		h.first = now
	}
	h.run++
	return h.run, now.Sub(h.first), ioerr > h.ioerrBase
}

// storeBacked reports whether a request to path reads or writes the session's store,
// which is what a broken connection to it fails. The status map is kept in memory.
func storeBacked(path string) bool {
	p := pathOnly(path)
	return strings.HasPrefix(p, "/session") && p != "/session/status"
}

// wedgeCause is the cause as it is worded in a log line, an event and a listing: the
// runtime's storage failing, when its log said so, and otherwise the plain fact.
func wedgeCause(io bool, run int, span time.Duration) string {
	if io {
		return "runtime 500: sqlite disk I/O error"
	}
	return fmt.Sprintf("runtime 5xx: %d requests in a row failed over %s", run, span.Round(time.Second))
}

// noteAnswer is called with every answer a server gives. It is cheap for every one
// that is not the tipping 5xx of a store path, and starts at most one restart per
// server.
func (d *Driver) noteAnswer(srv *server, path string, status int) {
	if srv == nil || d.shared != nil || d.store == nil {
		return // nothing to restart it from: the session's record is what a start needs
	}
	if !storeBacked(path) {
		return // evidence about the store only: see "What counts as wedged"
	}
	if status < 500 {
		srv.health.healthy(srv.logs.ioerrCount())
		return
	}
	if !srv.persisted.Load() || srv.stopping.Load() || srv.wedge.Load() != nil || srv.park.Load() != nil || srv.restarting.Load() {
		return // not yet a session, or already being stopped, or already being replaced
	}
	run, span, io := srv.health.failed(d.now(), srv.logs.ioerrCount())
	if !(run >= wedgeRun && span >= wedgeSpan) && !(io && run >= wedgeFastRun) {
		return
	}
	if d.cwdMoved(srv) != "" {
		return // muster #474 names that cause, and a new process would fail the same way
	}
	if !srv.restarting.CompareAndSwap(false, true) {
		return
	}
	d.mu.RLock()
	closed := d.closed
	if !closed {
		d.wedgeWG.Add(1) // before Shutdown can set closed, so its Wait never races this Add
	}
	d.mu.RUnlock()
	if closed {
		return
	}
	cause := wedgeCause(io, run, span)
	go func() {
		defer d.wedgeWG.Done()
		d.restartWedged(srv, cause)
	}()
}

// sessionOf is the session srv serves: in the isolated mode one server is one session.
func (d *Driver) sessionOf(srv *server) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for id, k := range d.seen {
		if k.srv == srv {
			return id, true
		}
	}
	return "", false
}

// recentRestarts is the restarts that still count against the budget at now.
func recentRestarts(ms []int64, now time.Time) []int64 {
	cut := now.Add(-wedgeBudgetWindow).UnixMilli()
	var out []int64
	for _, m := range ms {
		if m > cut {
			out = append(out, m)
		}
	}
	return out
}

// restartWedged replaces srv, as described above. It is the only writer of a wedge
// note and ends every path with the session either on a new server or listed unknown.
func (d *Driver) restartWedged(srv *server, cause string) {
	id, ok := d.sessionOf(srv)
	if !ok {
		return
	}
	unlock := d.lockLifecycle(id)
	defer unlock()

	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	k, seen := d.wasSeen(id)
	// A close that held the lock first forgot the session; a park or a revive replaced
	// the server. Nothing here is then this call's to do.
	if closed || !seen || k.srv != srv || srv.stopping.Load() || srv.park.Load() != nil {
		return
	}
	rec := d.store.load(id)
	if rec == nil {
		return // no record to start it from: the reads keep failing as they did, and say so
	}
	now := d.now()
	ref := fleet.SessionRef{Machine: d.machine, ID: id, Name: k.name}
	wr := rec.Wedge
	if wr == nil {
		wr = &wedgeRecord{}
	}
	wr.RestartsMs = recentRestarts(wr.RestartsMs, now)
	wr.Cause = cause
	rec.Wedge = wr

	// Before the stop: whoever then sees the process gone also sees why.
	srv.wedge.Store(&wedgeNote{at: now, cause: cause})

	switch {
	case rec.recreateReason() != "":
		// Not restarted, so nothing is spent from the budget.
		srv.pauseForShutdown()
		d.writeWedgeRecord(rec)
		msg := cause + "; its runtime process was stopped and this driver cannot start it again without the " +
			"caller-supplied environment the session was created with; supply that environment again to " +
			"revive it, or close the session"
		d.listWedged(id, srv, ref, recreateWedgedCallerEnv, msg)
		d.say("opencode: stopped the runtime of session %s (%s); it needs its caller's environment to be started again", id, cause)
		return
	case len(wr.RestartsMs) >= wedgeBudgetMax:
		srv.pauseForShutdown()
		wr.GaveUpAtMs = now.UnixMilli()
		d.writeWedgeRecord(rec)
		d.noteRelaunch(rec, true)
		msg := fmt.Sprintf("%s; its runtime process was replaced %d times within %s and is not replaced again; "+
			"close the session and create it again (a service restart tries once more)",
			cause, len(wr.RestartsMs), wedgeBudgetWindow)
		d.listWedged(id, srv, ref, recreateRelaunchFailed, msg)
		d.say("opencode: gave up replacing the runtime of session %s (%s): %d restarts within %s",
			id, cause, len(wr.RestartsMs), wedgeBudgetWindow)
		return
	}

	// The restart is on the record before it happens, so a crash in the middle still
	// counts against the budget. relaunch writes the record again with the new process.
	wr.RestartsMs = append(wr.RestartsMs, now.UnixMilli())
	wr.GaveUpAtMs = 0
	srv.pauseForShutdown()
	rctx, cancel := d.bounded(context.Background())
	nsrv, interrupted, err := d.relaunch(rctx, rec)
	cancel()
	if err != nil {
		d.writeWedgeRecord(rec)
		d.noteRelaunch(rec, true)
		d.listWedged(id, srv, ref, recreateRelaunchFailed, cause+"; replacing its runtime process failed: "+err.Error())
		d.say("opencode: could not replace the runtime of session %s (%s): %v", id, cause, err)
		return
	}
	if interrupted != nil {
		cut := *interrupted
		cut.Reason = "the runtime was restarted after it failed every request (" + cause + "); that turn did not finish"
		interrupted = &cut
	}

	d.mu.Lock()
	cur, still := d.seen[id]
	closed = d.closed
	if still && !closed && cur.srv == srv {
		cur.srv = nsrv
		cur.interrupted = interrupted
		d.seen[id] = cur
	}
	d.mu.Unlock()
	switch {
	case closed:
		nsrv.pauseForShutdown()
		return
	case !still:
		nsrv.pauseForShutdown()
		return
	case cur.srv != nsrv:
		nsrv.pauseForShutdown()
		return
	}
	nsrv.touch(d.now())
	d.announceRevived(nsrv, cur, ref)
	st := classify(false, wireStatus{})
	st.LastTurn = interrupted
	st.Sandbox, st.PermissionMode = nsrv.sandbox, nsrv.permission
	d.publish(cur, ref, fleet.Event{
		Machine: d.machine,
		Kind:    fleet.EventSessionState,
		Payload: fleet.SessionStatePayload{Ref: ref, State: st},
	})
	d.say("opencode: replaced the runtime of session %s (%s); restart %d of %d within %s",
		id, cause, len(wr.RestartsMs), wedgeBudgetMax, wedgeBudgetWindow)
}

// writeWedgeRecord keeps the record with what this decided. A failed write is logged
// and nothing more: the listing and the log already say it, and a record this driver
// cannot write is a state directory with larger problems.
func (d *Driver) writeWedgeRecord(rec *sessionRecord) {
	if err := d.store.write(rec); err != nil {
		d.say("opencode: could not record what happened to the runtime of session %s: %v", rec.ID, err)
	}
}

// listWedged puts a session whose server this driver stopped in the place of a session
// with no server: listed unknown with msg, which is the shape close, list, state and a
// revive already handle, and announced to every stream that follows it.
func (d *Driver) listWedged(id string, srv *server, ref fleet.SessionRef, cause recreateCause, msg string) {
	d.mu.Lock()
	cur, still := d.seen[id]
	if still && !d.closed && cur.srv == srv {
		cur.srv = nil
		cur.unknown = msg
		cur.recreate = cause
		cur.interrupted = nil
		if srv.dir != "" {
			cur.dir = srv.dir
		}
		d.seen[id] = cur
	} else {
		still = false
	}
	d.mu.Unlock()
	if !still {
		return
	}
	d.publish(cur, ref, fleet.Event{
		Machine: d.machine,
		Kind:    fleet.EventSessionState,
		Payload: fleet.SessionStatePayload{Ref: ref, State: fleet.UnknownState(fleet.ConfidenceObserved, msg)},
	})
}

// stoppedOnPurpose reports whether this driver stopped (or is stopping) the server
// for the session's sake, so that the process being gone is neither a death nor a
// closed session: a park, or a replacement of a wedged server.
func (s *server) stoppedOnPurpose() bool {
	return s.park.Load() != nil || s.wedge.Load() != nil
}

// wedgedState is what a read of a session being replaced answers: unknown, with why.
// Never idle: nothing was asked of the runtime (§5.7).
func (d *Driver) wedgedState(srv *server) fleet.SessionState {
	why := "the runtime failed every request"
	if n := srv.wedge.Load(); n != nil {
		why = n.cause
	}
	return fleet.UnknownState(fleet.ConfidenceObserved,
		"restarting: "+why+"; this driver is replacing its runtime process, and the session and its conversation are kept")
}

// wedgedError is the refusal for a write or a read that reaches a session while its
// runtime is being replaced: retryable, and nothing was sent.
func (d *Driver) wedgedError(what string) error {
	return &fleet.Error{
		Kind:      fleet.ErrorUnreachable,
		Retryable: true,
		Message:   what + ": its runtime failed every request and is being replaced; nothing was done, and the request can be repeated",
		Machine:   d.machine,
	}
}
