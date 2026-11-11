package opencode

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
)

// Finding sessions again after a restart (muster #282).
//
// New calls resume once, after the driver is built and before it is handed to
// anyone. For every record on disk it does one of three things, and every one of
// them leaves the session LISTED — a restart adopts and never silently drops
// (§7.5):
//
//   - relaunch: a process of its own, in the session's own directory and working
//     directory, with the environment rebuilt from this machine's sessionEnv files
//     (read fresh, as at create) and the sandbox profile the create asked for. A
//     by-id read then confirms the runtime found the session in its store.
//   - unknown, "needs re-creation": the create carried caller-supplied environment
//     values, which this driver never kept, or named a context file (muster #371)
//     that can no longer be read. Relaunching it with less than it was created with
//     would be a different session wearing the same id.
//   - unknown, with the reason it could not be relaunched: the sandbox can no longer
//     be enforced, a required sessionEnv file is gone, the directory is missing, the
//     runtime would not start, the runtime no longer has the session. The record and
//     the directory stay, so a later start tries again, and Close removes them. The
//     record also says the start failed (noteRelaunch, muster #446), which is the one
//     way `muster doctor`, a different process that cannot see this one's memory,
//     can count the session; the start that relaunches it takes that off.
//
// A session whose record says it was parked (muster #423) is not relaunched while
// the idle bound that parked it still holds: it is listed with the stopped server
// it had, and its first input starts it (parkedServerFor).
//
// Relaunches run concurrently, a few at a time: each waits for a server to answer.

// resumeParallel bounds how many servers are starting at once.
const resumeParallel = 4

func (d *Driver) resume(ctx context.Context) {
	items := d.store.loadAll()
	if len(items) == 0 {
		return
	}
	sem := make(chan struct{}, resumeParallel)
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(it loaded) {
			defer wg.Done()
			defer func() { <-sem }()
			d.resumeOne(ctx, it)
		}(it)
	}
	wg.Wait()
}

func (d *Driver) resumeOne(ctx context.Context, it loaded) {
	if it.rec == nil {
		// Unreadable: still a session somebody created. Its file name is its id.
		id := strings.TrimSuffix(filepath.Base(it.file), ".json")
		if safeID(id) {
			unknown := fmt.Sprintf("the stored record of this session could not be used (%v); it needs re-creation", it.err)
			d.markSeen(id, knownSession{unknown: unknown, conversation: unconfirmed(unknown)})
		}
		return
	}
	rec := it.rec
	// muster #398: the server the previous service left behind is stopped for
	// EVERY readable record, before any branch below decides whether the session
	// is relaunched. A record that is listed unknown (caller-supplied environment,
	// a context file that is gone, a runtime that would not start) has no new
	// server, but its old one is still running and still holds the session's
	// database; nothing else ever stops it, because closing the record later
	// finds no process attached.
	if reapStale(rec) {
		d.say("opencode: stopped a server left running by an earlier service (session %s, pid %d, port %d)", rec.ID, rec.Pid, rec.Port)
	}
	base := knownSession{
		cwd: fleet.AbsolutePath(rec.Cwd), name: rec.Name, agent: rec.Agent, specAgent: rec.SpecAgent, specModel: rec.SpecModel,
		startedAt: time.UnixMilli(rec.StartedAtMs),
	}
	if d.ownsDir(rec.Dir) {
		base.dir = rec.Dir
	}
	if why := rec.recreateReason(); why != "" {
		base.unknown = why
		base.conversation = unconfirmed(why)
		d.markSeen(rec.ID, base)
		return
	}
	// muster #371: the context is read again, from the path the record kept, before
	// anything is started. A session relaunched without the brief it was created
	// with would answer as a different session wearing the same id, so a file that
	// has gone or can no longer be used is a session listed unknown, not one
	// quietly relaunched with less.
	if rec.ContextRef != "" {
		base.contextRef = fleet.AbsolutePath(rec.ContextRef)
		text, err := d.readContext(base.contextRef)
		if err != nil {
			base.unknown = "the session could not be relaunched after the restart: its context file can no longer be used, " +
				"and a session is not relaunched without the context it was created with (" + describeCreateError(err) + "); it needs re-creation"
			base.conversation = unconfirmed(base.unknown)
			d.noteRelaunch(rec, true)
			d.markSeen(rec.ID, base)
			return
		}
		base.context = text
	}
	// muster #423: a session that was parked stays parked, with nothing started.
	if srv := d.parkedServerFor(rec); srv != nil {
		d.noteRelaunch(rec, false) // this start did not fail it
		base.srv = srv
		base.conversation = unconfirmed("the session was left parked, so its runtime has not been asked; " +
			"the input that starts it confirms the record")
		d.markSeen(rec.ID, base)
		d.say("opencode: session %s was parked before the restart and is left parked; its next input starts it", rec.ID)
		return
	}
	rctx, cancel := d.bounded(ctx)
	defer cancel()
	srv, interrupted, err := d.relaunch(rctx, rec)
	if err != nil {
		base.unknown = "the session could not be relaunched after the restart: " + err.Error()
		base.conversation = unconfirmed(base.unknown)
		d.noteRelaunch(rec, true)
		d.markSeen(rec.ID, base)
		return
	}
	base.srv, base.interrupted = srv, interrupted
	// relaunch read the session back by this id: the runtime still holds its record.
	base.conversation = confirmedAfterRestart(rec.ID)
	d.markSeen(rec.ID, base)
}

// relaunch starts a server for rec's session and confirms the runtime has it.
// On any failure the server is stopped and the session's directory is left alone.
//
// Two callers: resumeOne, at the service's start, and revive (park.go, muster
// #421), when an input reaches a session whose server was stopped for being idle.
// The second holds the session's lifecycle lock and ignores the interrupted turn
// this returns, because a server is only parked while it has no turn.
func (d *Driver) relaunch(ctx context.Context, rec *sessionRecord) (*server, *fleet.TurnEnd, error) {
	if err := d.checkRelaunchable(rec); err != nil {
		return nil, nil, err
	}
	// A server the previous service left running holds this database too; it was
	// stopped by resumeOne before this was called (muster #398).

	srv, err := d.startServerIn(ctx, rec.spec(), rec.Dir, rec.CacheCopy)
	if err != nil {
		return nil, nil, err
	}
	var sess wireSession
	if err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(rec.ID), nil, &sess); err != nil {
		srv.pauseForShutdown()
		if isNotFound(err) {
			return nil, nil, fmt.Errorf("the runtime restarted but no longer has this session in its store")
		}
		return nil, nil, fmt.Errorf("the runtime restarted but the session could not be read back: %w", err)
	}
	d.stampProcess(rec, srv)
	// muster #423: a process serves the session now, so the record no longer says
	// parked, in the same write that names the process.
	rec.Parked = nil
	// muster #446: and a start that worked no longer has a failed one to report.
	rec.RelaunchFailedAtMs = 0
	if err := d.store.write(rec); err != nil {
		srv.pauseForShutdown()
		return nil, nil, err
	}
	return srv, d.interruptedTurn(ctx, srv, rec.ID), nil
}

// noteRelaunch keeps rec's record saying what the last start did with the session
// (muster #446): failed marks it, and anything else takes the mark off. It writes only
// when that changes the record, so a session that fails every start is written once,
// not once per start, and a healthy session's record is never touched.
//
// A failed write is logged and nothing more. The listing already carries the session
// as unknown with its reason, which is what a start owes; the record is only what lets
// the doctor count it from disk later, and a record this driver cannot write is a
// state directory with larger problems than one uncounted session.
func (d *Driver) noteRelaunch(rec *sessionRecord, failed bool) {
	switch {
	case failed && rec.RelaunchFailedAtMs == 0:
		rec.RelaunchFailedAtMs = d.now().UnixMilli()
	case !failed && rec.RelaunchFailedAtMs != 0:
		rec.RelaunchFailedAtMs = 0
	default:
		return
	}
	if err := d.store.write(rec); err != nil {
		d.say("opencode: could not update the record of session %s after the start: %v", rec.ID, err)
	}
}

// spec is the create's request as the record kept it: what a start needs to build
// the session's environment, sandbox and permission posture again.
func (r *sessionRecord) spec() fleet.SessionSpec {
	return fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(r.Cwd), Agent: fleet.AgentId(r.SpecAgent), Marker: r.SpecMarker,
		Model:   r.SpecModel, // muster #458: a relaunch judges "is this session bound for the provider" as the create did
		Sandbox: r.Sandbox, PermissionMode: r.PermissionMode,
	}
}

// checkRelaunchable is what a start of rec's session needs to be true before any
// process exists: a posture this driver still enforces and a directory it made and
// that still holds the session's database. Shared by relaunch, which refuses on it,
// and by parkedServerFor, which will not leave parked a session that could not be
// started.
func (d *Driver) checkRelaunchable(rec *sessionRecord) error {
	if rec.PermissionMode != "" && rec.Sandbox == nil {
		// Cannot arise from a create this driver accepted; a record edited by hand
		// is refused rather than relaunched pre-approved with nothing confining it.
		return fmt.Errorf("its record asks for permission mode %q without a sandbox, which this driver never starts", rec.PermissionMode)
	}
	if rec.Sandbox != nil {
		if sup, why := d.sandboxSupportNow(); sup == nil {
			return fmt.Errorf("it was created in a sandbox and this driver can no longer enforce one (%s); "+
				"it is not relaunched unconfined", why)
		}
	}
	if !d.ownsDir(rec.Dir) {
		return fmt.Errorf("its recorded directory %q is not one this driver made", rec.Dir)
	}
	if info, err := os.Stat(rec.Dir); err != nil || !info.IsDir() {
		return fmt.Errorf("its scoped directory %q is gone, and with it the session's database", rec.Dir)
	}
	return nil
}

// parkedServerFor is the stopped server a session is listed with after a restart
// when its record says it was parked (muster #423), or nil when the session is to
// be started the ordinary way. It starts nothing and reads nothing from a runtime:
// it is the server the park left, rebuilt from what the record kept — the scoped
// directory, the sandbox and permission posture a state read reports, and the note
// that says why it is stopped — so phaseOf reads it parked and the first input
// revives it exactly as it would have before the restart.
//
// nil, and so a start, in every case where leaving the session parked would trust
// something the service cannot stand behind:
//   - the record does not say parked, or this driver is in shared mode;
//   - no idle bound is set now. Nothing would park the session again, and a parked
//     session is only ever made with the bound set (its lock is taken only then);
//   - the bound is lower than the one the park was decided under: the fact belongs
//     to a policy that has since been replaced, so the session is started and the
//     new bound decides afresh, one bound of memory later at most. A bound that is
//     the same or higher keeps it parked: it would have been parked by the new one;
//   - a start of it would fail before any process exists (checkRelaunchable, the
//     environment files, the operator's agents file, the sandbox plan). Those are
//     refused at a restart as they always were — the session is listed unknown, with
//     the reason — rather than left parked behind a first input that cannot start it.
//
// The one thing left unchecked is that the runtime still holds the session in its
// store, which only a process can say. relaunch reads it back when the first input
// starts the session, and a failure there is the retryable one a parked session
// already has.
func (d *Driver) parkedServerFor(rec *sessionRecord) *server {
	p := rec.Parked
	if p == nil || d.shared != nil || d.idleStop <= 0 || d.idleStop < time.Duration(p.BoundMs)*time.Millisecond {
		return nil
	}
	if d.checkRelaunchable(rec) != nil {
		return nil
	}
	spec := rec.spec()
	env, err := d.sessionEnvFor(spec)
	if err != nil {
		return nil
	}
	if _, err := d.operatorAgentsFor(); err != nil {
		return nil
	}
	srv := &server{dir: rec.Dir, credential: d.credentialOfRecord(spec, env)}
	if spec.PermissionMode == fleet.PermissionModeBypass {
		srv.permission = fleet.PermissionModeBypass
	}
	if spec.Sandbox != nil {
		if pc := spec.Sandbox.PackageCache; pc != nil && pc.Mode != fleet.PackageCacheShared && rec.CacheCopy == "" {
			// Planning would make the session's private cache again, here and then
			// once more by the start this falls back to: not a side effect to take twice.
			return nil
		}
		// No context: the guard above returns before any copy could be made here.
		plan, err := d.planSandboxFor(context.Background(), spec, rec.Dir, rec.CacheCopy)
		if err != nil {
			return nil
		}
		srv.sandbox = plan.state()
	}
	srv.persisted.Store(true)
	srv.touch(d.now())
	srv.park.Store(noteFromRecord(p))
	return srv
}

// interruptedTurn reports a turn the previous process was in the middle of: the
// newest message is the caller's with no answer, or an answer the runtime never
// finished. Nothing is running in a freshly started server, so either is a turn
// that stopped with the service rather than one still going. Nil when the history
// shows none, or cannot be read — absence of evidence, not a claim of a clean end.
func (d *Driver) interruptedTurn(ctx context.Context, srv *server, id string) *fleet.TurnEnd {
	var msgs []wireMessage
	if err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(id)+"/message?limit=1", nil, &msgs); err != nil || len(msgs) == 0 {
		return nil
	}
	info := msgs[0].Info
	cut := (info.Role == "user") || (info.Role == "assistant" && info.Error == nil && info.Time.Completed == 0)
	if !cut {
		return nil
	}
	return &fleet.TurnEnd{
		Outcome:   "interrupted",
		Reason:    "the service stopped while a turn was running; that turn did not finish",
		Retryable: true,
	}
}

// stampProcess records which process serves rec's session now.
func (d *Driver) stampProcess(rec *sessionRecord, srv *server) {
	rec.Bin = d.bin
	rec.Pid = 0
	if srv.proc != nil && srv.proc.cmd != nil && srv.proc.cmd.Process != nil {
		rec.Pid = srv.proc.cmd.Process.Pid
	}
	if _, port, ok := strings.Cut(strings.TrimPrefix(srv.baseURL, "http://"), ":"); ok {
		rec.Port, _ = strconv.Atoi(port)
	}
}

// describeCreateError is a create-time refusal's own words without its "create: "
// prefix, for a sentence that is about a restart rather than a create.
func describeCreateError(err error) string {
	msg := err.Error()
	if fe, ok := err.(*fleet.Error); ok {
		msg = fe.Message
	}
	return strings.TrimPrefix(msg, "create: ")
}

// requireLive refuses an operation on a session that is listed but not running.
func (d *Driver) requireLive(k knownSession) error {
	if k.srv != nil {
		return nil
	}
	return &fleet.Error{
		Kind:    fleet.ErrorConflict,
		Message: "this session is listed but not running: " + k.unknown,
		Machine: d.machine,
	}
}

// closeUnlaunched closes a session that was found on disk but is not running:
// this service started no process for it and there is no runtime to ask, so
// closing it is removing what it left — the record and the directory — and
// stopping a server the previous service left running, if the record still names
// one that matches (muster #398). The caller's expected start time is still
// checked when both sides have one.
func (d *Driver) closeUnlaunched(req fleet.Request, ref fleet.SessionRef, prior knownSession) (fleet.Ack, error) {
	if want := req.Expect.StartedAt; want != nil && !prior.startedAt.IsZero() && !prior.startedAt.Equal(*want) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q holds a session started at %s; the caller meant the one started at %s",
			fleet.ErrAmbiguousTarget, ref.ID, prior.startedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// muster #398: "there is no process to stop" was only ever true of the new
	// service. The previous one may have left a server behind, and the record
	// being removed below is the last thing that names it; stop it first, on the
	// same identity checks the start-up reap uses (the recorded binary serving the
	// recorded port). Whatever state the session is in, closing it leaves no
	// server for it.
	if rec := d.store.load(ref.ID); rec != nil && reapStale(rec) {
		d.say("opencode: stopped a server left running by an earlier service (session %s, pid %d, port %d)", rec.ID, rec.Pid, rec.Port)
	}
	if prior.dir != "" {
		// muster #405: and what the session started outside the server's process
		// group, found by the directory that is about to be removed.
		if !d.noStraySweep {
			stopped, left := stopStraysIn(prior.dir)
			d.sayStrays("still running when their session was closed", stopped, left)
		}
		if err := privfs.RemoveAll(prior.dir); err != nil {
			return fleet.Ack{}, fmt.Errorf("close: removing the session's directory: %w", err)
		}
	}
	d.forgetSeen(ref.ID) // removes the record
	d.publish(prior, fleet.SessionRef{Machine: d.machine, ID: ref.ID, Name: prior.name}, fleet.Event{
		Machine: d.machine,
		Kind:    fleet.EventSessionClosed,
		Payload: fleet.SessionStatePayload{
			Ref:   fleet.SessionRef{Machine: d.machine, ID: ref.ID, Name: prior.name},
			State: fleet.InferredState(fleet.StatusDead, "the session was closed", nil),
		},
	})
	return fleet.Ack{Accepted: true}, nil
}

// ownsDir reports whether dir is a session directory this driver made: directly
// under the session root, named as sessionDirs names them. A record is a file on
// disk and could say anything; nothing is removed or started on its word alone.
func (d *Driver) ownsDir(dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	rel, err := filepath.Rel(d.sessionRoot, filepath.Clean(dir))
	return err == nil && !strings.Contains(rel, string(filepath.Separator)) &&
		strings.HasPrefix(rel, "muster-opencode-")
}
