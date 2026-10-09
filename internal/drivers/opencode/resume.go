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
//     values, which this driver never kept. Relaunching it with less than it was
//     created with would be a different session wearing the same id.
//   - unknown, with the reason it could not be relaunched: the sandbox can no longer
//     be enforced, a required sessionEnv file is gone, the directory is missing, the
//     runtime would not start, the runtime no longer has the session. The record and
//     the directory stay, so a later start tries again, and Close removes them.
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
			d.markSeen(id, knownSession{
				unknown: fmt.Sprintf("the stored record of this session could not be used (%v); it needs re-creation", it.err),
			})
		}
		return
	}
	rec := it.rec
	base := knownSession{
		cwd: fleet.AbsolutePath(rec.Cwd), name: rec.Name, agent: rec.Agent,
		startedAt: time.UnixMilli(rec.StartedAtMs),
	}
	if d.ownsDir(rec.Dir) {
		base.dir = rec.Dir
	}
	if why := rec.recreateReason(); why != "" {
		base.unknown = why
		d.markSeen(rec.ID, base)
		return
	}
	rctx, cancel := d.bounded(ctx)
	defer cancel()
	srv, interrupted, err := d.relaunch(rctx, rec)
	if err != nil {
		base.unknown = "the session could not be relaunched after the restart: " + err.Error()
		d.markSeen(rec.ID, base)
		return
	}
	base.srv, base.interrupted = srv, interrupted
	d.markSeen(rec.ID, base)
}

// relaunch starts a server for rec's session and confirms the runtime has it.
// On any failure the server is stopped and the session's directory is left alone.
func (d *Driver) relaunch(ctx context.Context, rec *sessionRecord) (*server, *fleet.TurnEnd, error) {
	if rec.Sandbox != nil && d.sandbox == nil {
		return nil, nil, fmt.Errorf("it was created in a sandbox and this driver can no longer enforce one (%s); "+
			"it is not relaunched unconfined", d.sandboxWhy)
	}
	if !d.ownsDir(rec.Dir) {
		return nil, nil, fmt.Errorf("its recorded directory %q is not one this driver made", rec.Dir)
	}
	if info, err := os.Stat(rec.Dir); err != nil || !info.IsDir() {
		return nil, nil, fmt.Errorf("its scoped directory %q is gone, and with it the session's database", rec.Dir)
	}
	// A server the previous service left running holds this database too.
	reapStale(rec)

	spec := fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(rec.Cwd), Agent: fleet.AgentId(rec.SpecAgent), Marker: rec.SpecMarker,
		Sandbox: rec.Sandbox,
	}
	srv, err := d.startServerIn(ctx, spec, rec.Dir, rec.CacheCopy)
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
	if err := d.store.write(rec); err != nil {
		srv.pauseForShutdown()
		return nil, nil, err
	}
	return srv, d.interruptedTurn(ctx, srv, rec.ID), nil
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

// clearInterrupted forgets that a restart cut a session's turn short, once a new
// turn has been sent.
func (d *Driver) clearInterrupted(id string) {
	d.mu.Lock()
	if k, ok := d.seen[id]; ok && k.interrupted != nil {
		k.interrupted = nil
		d.seen[id] = k
	}
	d.mu.Unlock()
}

// closeUnlaunched closes a session that was found on disk but is not running:
// there is no process to stop and no runtime to ask, so closing it is removing
// what it left — the record and the directory. The caller's expected start time
// is still checked when both sides have one.
func (d *Driver) closeUnlaunched(req fleet.Request, ref fleet.SessionRef, prior knownSession) (fleet.Ack, error) {
	if want := req.Expect.StartedAt; want != nil && !prior.startedAt.IsZero() && !prior.startedAt.Equal(*want) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q holds a session started at %s; the caller meant the one started at %s",
			fleet.ErrAmbiguousTarget, ref.ID, prior.startedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if prior.dir != "" {
		if err := os.RemoveAll(prior.dir); err != nil {
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
