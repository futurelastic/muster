package opencode

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Bringing a session a restart left unknown back (muster #427).
//
// A session created with caller-supplied environment is listed `unknown` after a
// restart, "needs re-creation", and until now its caller's only verb was a close:
// the values were never kept (muster #282), and relaunching it with less would be a
// different session wearing the same id. The conversation itself is intact, in the
// runtime's own store in the session's directory, and a supervisor that passes
// per-session credentials lost every running session on every upgrade.
//
// Revive is the caller handing the values back. Nothing is persisted: the
// environment goes to ONE relaunch, through the same start a create uses and so with
// every refusal that start makes, and the session's record changes only by naming
// the new process. The next restart lists the session `unknown` again, which is
// correct: the service still holds no secret.
//
// # What it refuses, and why each is a refusal and not a try
//
//   - A session that has a server. Nothing is wrong with it; a second start would
//     put two processes on one database.
//   - A session listed unknown for any other cause. Its relaunch failed for a reason
//     a later start may clear, its record cannot be read, or its context file is
//     gone: none of those is the environment, supplying it again changes nothing,
//     and the next start already tries again where trying can help.
//   - An environment that does not name exactly the variables the create named. Fewer
//     is the session relaunched with less than it had; more is a different session.
//     Values cannot be checked, and are the caller's word as they were at create.
//   - A bypass-posture session, for a caller without the `send` grant that a create
//     asking for that posture needs: a create-only caller must not be able to start
//     what only a sender could have created.
//
// # Concurrency
//
// The whole call runs under the session's lifecycle lock (lockLifecycle): a second
// revive finds the session running and is refused, and a close waits, so it cannot
// remove the directory a server is being started in. A close that wins the lock
// first forgets the session, and the revive that was waiting finds it gone.

// recreateCause is why a session with no server has none, as a value. The text
// the listing carries (knownSession.unknown) is for a person and may change; a
// revive keys on this.
type recreateCause int

const (
	// recreateNone is a session with a server, or one nothing has classified.
	recreateNone recreateCause = iota
	// recreateCallerEnv is a session whose create carried caller-supplied
	// environment: the one cause a revive can clear.
	recreateCallerEnv
	// recreateContextGone is a session whose context file can no longer be used.
	recreateContextGone
	// recreateUnreadable is a session whose record could not be read.
	recreateUnreadable
	// recreateRelaunchFailed is a session whose relaunch failed at the last start.
	recreateRelaunchFailed
	// recreateWedgedCallerEnv is a session whose runtime this driver stopped because
	// it answered every read of the session's store with a 5xx (muster #513), and
	// whose create carried caller-supplied environment, so the driver could not start
	// it again by itself. The second cause a revive clears, and not one a restart
	// counts: it is not in NeedingRecreation's figure, which is about the start that
	// built this driver.
	recreateWedgedCallerEnv
)

// NeedingRecreation implements driver.RecreationReporter (muster #510): how many
// sessions the start that built this driver left listed unknown for want of their
// callers' environment, and how many of those still are.
//
// left is the figure resume logged at that start and never changes. waiting is read
// live from what the driver knows, and counts the sessions whose cause is the one a
// revive clears: a revive takes a session out of it (revive.go sets its cause to
// none), and so does a close (the session is forgotten). It cannot exceed left,
// because resume is the only place that cause is set, and no session created since
// can be in it: a create starts a server, and a session with a server is never
// counted. A session whose relaunch failed for another reason is in neither count:
// supplying the environment would not bring it back, and its listing carries the
// reason.
//
// A count and nothing else: the figure names no session, no directory, and no
// variable, so it is as safe on a public endpoint as the restart log line it repeats.
func (d *Driver) NeedingRecreation() (left, waiting int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, k := range d.seen {
		if k.recreate == recreateCallerEnv {
			waiting++
		}
	}
	return d.restartLeft, waiting
}

var _ driver.RecreationReporter = (*Driver)(nil)

// Revive implements driver.Reviver.
func (d *Driver) Revive(ctx context.Context, req fleet.Request, ref fleet.SessionRef, spec driver.ReviveSpec) (fleet.Session, error) {
	if d.shared != nil || d.store == nil {
		return fleet.Session{}, &fleet.Error{
			Kind: fleet.ErrorUnsupported, Machine: d.machine,
			Message: "revive: this driver keeps no record of its sessions to relaunch one from (it has no state directory, or shares one server)",
		}
	}
	if len(spec.Env) == 0 {
		return fleet.Session{}, d.reviveInvalid("env is required: a session is not relaunched with less environment than it was created with")
	}

	unlock := d.lockLifecycle(ref.ID)
	defer unlock()
	known, ok := d.wasSeen(ref.ID)
	if !ok {
		return fleet.Session{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if want := req.Expect.StartedAt; want != nil && !known.startedAt.Equal(*want) {
		return fleet.Session{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			fleet.ErrAmbiguousTarget, ref.ID, known.startedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	switch {
	case known.srv != nil:
		return fleet.Session{}, d.notRevivable("the session is running; there is nothing to bring back")
	case known.recreate != recreateCallerEnv && known.recreate != recreateWedgedCallerEnv:
		why := known.unknown
		if why == "" {
			why = "it is not listed as needing its caller's environment"
		}
		return fleet.Session{}, d.notRevivable("supplying the environment would not bring this session back: %s", why)
	}
	rec := d.store.load(ref.ID)
	if rec == nil {
		return fleet.Session{}, d.notRevivable("the session's record is gone or can no longer be read")
	}
	if len(rec.EnvNames) == 0 {
		return fleet.Session{}, d.notRevivable("the session's record names no caller-supplied environment")
	}

	// The names, before anything is built from the values.
	folded, err := normalizeEnvNames(spec.Env, envKey)
	if err != nil {
		return fleet.Session{}, d.reviveInvalid("%s", strings.TrimPrefix(err.Error(), "env: ")) // names only
	}
	if err := sameEnvNames(rec.EnvNames, folded); err != nil {
		return fleet.Session{}, d.reviveInvalid("%s", err.Error())
	}

	// The context: a check, never a change. The recorded file is read again (muster
	// #371) so the revived session carries the brief it was created with.
	recorded := fleet.AbsolutePath(rec.ContextRef)
	switch {
	case spec.ContextRef != "" && recorded == "":
		return fleet.Session{}, d.reviveInvalid("contextRef is set but this session was created without one")
	case spec.ContextRef != "" && spec.ContextRef != recorded:
		return fleet.Session{}, d.reviveInvalid("contextRef does not name the file this session was created with")
	}
	contextText, err := d.readContext(recorded)
	if err != nil {
		return fleet.Session{}, d.asRevive(err)
	}
	if rec.PermissionMode == fleet.PermissionModeBypass && !spec.MayBypass {
		return fleet.Session{}, &fleet.Error{
			Kind: fleet.ErrorUnauthorized, Machine: d.machine,
			Message: "revive: this session was created in the permission-bypass posture, which a caller needs the send grant to start",
		}
	}
	if err := d.checkRelaunchable(rec); err != nil {
		var gone *sandboxGoneError
		if errors.As(err, &gone) {
			return fleet.Session{}, &fleet.Error{Kind: fleet.ErrorUnsupported, Machine: d.machine, Message: "revive: " + err.Error()}
		}
		return fleet.Session{}, d.notRevivable("%s", err.Error())
	}

	rctx, cancel := d.bounded(ctx)
	srv, interrupted, err := d.relaunchWith(rctx, rec, folded)
	cancel()
	if err != nil {
		return fleet.Session{}, d.reviveRelaunchError(err)
	}

	d.mu.Lock()
	cur, still := d.seen[ref.ID]
	closed := d.closed
	if still && !closed && cur.srv == nil && (cur.recreate == recreateCallerEnv || cur.recreate == recreateWedgedCallerEnv) {
		cur.srv = srv
		cur.unknown = ""
		cur.recreate = recreateNone
		cur.interrupted = interrupted
		cur.contextRef, cur.context = recorded, contextText
		cur.conversation = confirmedAfterRevive(ref.ID)
		d.seen[ref.ID] = cur
	}
	d.mu.Unlock()
	switch {
	case closed:
		srv.pauseForShutdown()
		return fleet.Session{}, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: "revive: this driver has been shut down", Machine: d.machine}
	case !still:
		// Forgotten while this call held the session's lock: something that does not
		// take it (a read the runtime answered 404 to) removed it. The server is for a
		// session that is gone.
		srv.pauseForShutdown()
		return fleet.Session{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	case cur.srv != srv:
		srv.pauseForShutdown()
		return fleet.Session{}, d.notRevivable("the session changed while it was being revived")
	}

	ref.Name = cur.name
	d.announceRevived(srv, cur, ref)
	st := classify(false, wireStatus{})
	st.LastTurn = interrupted
	st.Sandbox, st.PermissionMode = srv.sandbox, srv.permission
	d.publish(cur, ref, fleet.Event{
		Machine: d.machine,
		Kind:    fleet.EventSessionState,
		Payload: fleet.SessionStatePayload{Ref: ref, State: st},
	})
	// A count and never a name or a value: the log is not a place for the environment.
	d.say("opencode: revived session %s with caller-supplied environment (%d variables)", ref.ID, len(folded))

	lctx, lcancel := d.bounded(ctx)
	defer lcancel()
	col, err := d.List(lctx, req, driver.ListFilter{ID: ref.ID})
	if err == nil {
		for _, s := range col.Items() {
			if s.ID == ref.ID {
				return s, nil
			}
		}
	}
	// The session is revived and the read that describes it failed: say what is known
	// without it rather than report a failure for a success.
	started := cur.startedAt
	return fleet.Session{
		SessionRef: ref, StartedAt: &started, Runtime: d.runtime, Cwd: cur.cwd, Agent: fleet.AgentId(cur.agent),
		State: st, Conversation: cur.conversationRef(),
	}, nil
}

// sameEnvNames refuses an env whose names are not exactly the ones the create
// named. Both sides are folded, so a platform that ignores case compares as it
// does. The message carries names and never values.
func sameEnvNames(recorded []string, supplied map[string]string) error {
	want := map[string]bool{}
	for _, n := range recorded {
		want[envKey(n)] = true
	}
	var missing, extra []string
	for n := range want {
		if _, ok := supplied[n]; !ok {
			missing = append(missing, n)
		}
	}
	for n := range supplied {
		if !want[n] {
			extra = append(extra, n)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	msg := "env must name exactly the variables the session was created with"
	if len(missing) > 0 {
		msg += "; missing: " + strings.Join(missing, ", ")
	}
	if len(extra) > 0 {
		msg += "; not in the create: " + strings.Join(extra, ", ")
	}
	return errors.New(msg)
}

func (d *Driver) notRevivable(format string, args ...any) error {
	return &fleet.Error{
		Kind: fleet.ErrorConflict, Reason: fleet.ReasonNotRevivable, Machine: d.machine,
		Message: "revive: " + fmt.Sprintf(format, args...),
	}
}

func (d *Driver) reviveInvalid(format string, args ...any) error {
	return &fleet.Error{Kind: fleet.ErrorInvalid, Machine: d.machine, Message: "revive: " + fmt.Sprintf(format, args...)}
}

// asRevive says a create-time refusal in a revive's words: the start and the context
// read are the create's own code, and their messages begin "create: ".
func (d *Driver) asRevive(err error) error {
	var fe *fleet.Error
	if errors.As(err, &fe) {
		cp := *fe
		cp.Message = "revive: " + strings.TrimPrefix(cp.Message, "create: ")
		return &cp
	}
	return err
}

// reviveRelaunchError is what a failed relaunch becomes. A typed refusal keeps its
// kind; the runtime not holding the session is not something asking again changes;
// the context ending is the caller's to retry; anything else (the runtime started
// and could not be read back, the record could not be written) is the machine
// failing now, which is retryable and never the 400 an untyped error would become.
func (d *Driver) reviveRelaunchError(err error) error {
	switch {
	case errors.Is(err, errRuntimeLostSession):
		return d.notRevivable("%s", err.Error())
	case isCtxErr(err):
		return err
	}
	var step *driver.StepTimeout
	if errors.As(err, &step) {
		return err
	}
	var fe *fleet.Error
	if errors.As(err, &fe) {
		return d.asRevive(err)
	}
	return &fleet.Error{Kind: fleet.ErrorUnreachable, Retryable: true, Machine: d.machine, Message: "revive: " + err.Error()}
}

var _ driver.Reviver = (*Driver)(nil)
