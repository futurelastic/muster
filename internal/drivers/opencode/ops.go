package opencode

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Create starts a session (§3), honouring the caller's idempotency key
// (§10) with an in-memory table — see the package doc's SupportsResume
// note for why this driver does not attempt to survive a restart.
//
// In the production (isolated) mode a create STARTS A SERVER: it builds the
// session's environment (env.go), makes its scoped directory, execs a fresh
// `opencode serve` in a process group of its own, waits for it to answer, and
// only then creates the session on it — opencode assigns the session id, so
// the id cannot be known before the process exists. A failure at any of those
// steps tears the server and its directory down again and stores no idempotency
// entry, so a retry starts clean. Because that takes seconds rather than
// milliseconds, two concurrent creates with the SAME key must not each start a
// server: the second waits for the first and then replays its result (§10).
//
// Agent, Model and Effort are §2.1's "hints, not guarantees": this driver
// genuinely honours Agent and Model (opencode's own create body carries
// both) and REFUSES rather than silently drops Effort, ContextRef,
// McpConfig, Settings (muster #247), PermissionMode, Resume and ConversationId (muster #224) —
// none of which this substrate has an honest mechanism for. Env is honoured
// (muster #280) by building the session's environment; it is refused only in
// shared mode, where one server serves every session and has no way to give
// one a variable another does not see — and there IsolateEnvironment is
// refused too. TrustCwd and
// Consents are left as no-ops: a substrate with no such boot-time question
// honours them by having nothing to do (session.go's own rule for a HINT),
// and opencode's session.create is a plain REST call with no interactive
// dialog to pre-answer. RemoteControl is likewise a no-op — every opencode session
// is reachable over this driver's HTTP API, so there is no "local-only"
// mode either to grant or to refuse.
func (d *Driver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	if key == "" {
		return fleet.Session{}, &fleet.Error{
			Kind: fleet.ErrorInvalid, Message: "create: idempotency key is required (§10)", Machine: d.machine,
		}
	}
	// One create per key at a time: a repeat of a completed key replays it, a
	// repeat of an IN-FLIGHT key waits for it and then replays or, if it
	// failed, takes over.
	for {
		if ref, ok := d.idemLookup(key); ok {
			// A repeat of an already-completed create: report the same applied
			// facts the original create observed and cached (markSeen), not a
			// fresh echo of this call's own spec — muster #84 is exactly
			// the failure of reporting a request as if it were a fact.
			info, _ := d.wasSeen(ref.ID)
			return fleet.Session{
				SessionRef: ref, Cwd: spec.Cwd, Agent: fleet.AgentId(info.agent),
				Pins: pinOutcomeForAgent(spec, info.agent),
			}, nil
		}
		wait, leader := d.idemBegin(key)
		if leader {
			defer d.idemEnd(key)
			break
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return fleet.Session{}, ctx.Err()
		}
	}
	if spec.Cwd == "" {
		return fleet.Session{}, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: cwd is required", Machine: d.machine}
	}
	refused := map[string]bool{
		"effort":     spec.Effort != "",
		"contextRef": spec.ContextRef != "",
		"mcpConfig":  len(spec.McpConfig) > 0,
		// muster #247: launch-time CLI settings have no meaning for a runtime
		// with no CLI to hand them to.
		"settings":       len(spec.Settings) > 0,
		"permissionMode": spec.PermissionMode != "",
		"resume":         spec.Resume != "",
		// muster #224: opencode's session.create has no equivalent of
		// Claude Code's --session-id — this substrate assigns its own id and
		// this driver has no honest way to make it start under a caller's
		// instead. Refused for the same reason as resume, three lines above.
		"conversationId": spec.ConversationId != "",
	}
	if d.shared != nil {
		// muster #280: one server for every session has no way to give one
		// session a variable another does not see, nor to isolate anything.
		refused["env"] = len(spec.Env) > 0
		refused["isolateEnvironment"] = spec.IsolateEnvironment
	}
	if spec.Sandbox != nil && d.sandbox == nil {
		// muster #281: refuse, never degrade. The reason is the capability's own,
		// so a caller reading it learns what is missing rather than that "it failed".
		return fleet.Session{}, &fleet.Error{
			Kind:    fleet.ErrorUnsupported,
			Message: "create: this driver cannot enforce a sandbox: " + d.sandboxWhy + " (refusing rather than start the session unconfined)",
			Machine: d.machine,
		}
	}
	for name, set := range refused {
		if set {
			return fleet.Session{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("create: this driver has no honest way to honour %q (§2.1: refuse rather than drop a hint silently)", name),
				Machine: d.machine,
			}
		}
	}

	body := map[string]any{}
	if spec.Name != "" {
		body["title"] = spec.Name
	}
	if spec.Agent != "" {
		body["agent"] = string(spec.Agent)
	}
	if spec.Model != "" {
		providerID, modelID, ok := strings.Cut(spec.Model, "/")
		if !ok || providerID == "" || modelID == "" {
			return fleet.Session{}, &fleet.Error{
				Kind:    fleet.ErrorInvalid,
				Message: `create: model must be "provider/model" (opencode's own -m flag convention)`,
				Machine: d.machine,
			}
		}
		body["model"] = map[string]string{"providerID": providerID, "id": modelID}
	}

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	// Names only, taken before anything can touch the map (muster #282).
	callerEnvNames := envNames(spec.Env)

	srv := d.shared
	if srv == nil {
		started, err := d.startSessionServer(ctx, spec)
		if err != nil {
			return fleet.Session{}, err
		}
		srv = started
	}

	var sess wireSession
	path := "/session?directory=" + url.QueryEscape(string(spec.Cwd))
	if err := d.do(ctx, srv, "POST", path, body, &sess); err != nil {
		d.discardServer(srv)
		return fleet.Session{}, err
	}
	if _, dup := d.wasSeen(sess.ID); dup && d.shared == nil {
		// A fresh process was handed an id this driver already routes to
		// another one. Cannot happen with opencode's ids; refusing is safer
		// than letting two sessions share a key in the routing table.
		d.discardServer(srv)
		return fleet.Session{}, fmt.Errorf("create: the runtime returned session id %q, which another session already holds", sess.ID)
	}
	startedAt := time.UnixMilli(sess.Time.Created)
	ref := fleet.SessionRef{Machine: d.machine, ID: sess.ID, Name: sess.Title}
	cwd := fleet.AbsolutePath(sess.Directory)
	if cwd == "" {
		cwd = spec.Cwd
	}
	created := knownSession{srv: srv, cwd: cwd, name: sess.Title, agent: sess.Agent, startedAt: startedAt}
	if d.store != nil && d.shared == nil {
		// Before the session is announced or handed an id: a session the caller
		// can see but a restart cannot find would make SupportsResume a lie.
		rec := &sessionRecord{
			ID: sess.ID, Cwd: string(cwd), Name: sess.Title, Agent: sess.Agent,
			SpecAgent: string(spec.Agent), SpecMarker: spec.Marker,
			StartedAtMs: sess.Time.Created, Dir: srv.dir, EnvNames: callerEnvNames,
			Sandbox: spec.Sandbox,
		}
		if srv.sandbox != nil && srv.sandbox.PackageCache != nil {
			rec.CacheCopy = srv.sandbox.PackageCache.Copy
		}
		d.stampProcess(rec, srv)
		if err := d.store.write(rec); err != nil {
			d.discardServer(srv)
			return fleet.Session{}, err
		}
		srv.persisted.Store(true)
		created.dir = srv.dir
	}
	d.markSeen(sess.ID, created)
	// Before the opening prompt: a subscription connects to this session's bus
	// now, so the prompt's first change is on the stream (muster #284).
	d.announceCreated(srv, created, ref)

	if spec.Prompt != "" {
		if err := d.sendPrompt(ctx, srv, sess.ID, spec.Prompt); err != nil {
			// The session exists but could not be started. Reported as a
			// failure of THIS create — a caller retrying with the same
			// idempotency key gets the session back (see below) and may
			// resend the prompt via Send.
			d.idemStore(key, ref)
			return fleet.Session{}, fmt.Errorf("create: session %s was started but its opening prompt failed: %w", sess.ID, err)
		}
	}
	d.idemStore(key, ref)
	return fleet.Session{
		SessionRef: ref, Cwd: spec.Cwd, Agent: fleet.AgentId(sess.Agent),
		Pins: pinOutcomeForAgent(spec, sess.Agent),
	}, nil
}

// startSessionServer starts the one server a session will live on: resolves
// the session's environment (refusing before any side effect), makes its
// scoped directory, and execs the server. The directory is removed again if
// the server does not come up.
func (d *Driver) startSessionServer(ctx context.Context, spec fleet.SessionSpec) (*server, error) {
	return d.startServerIn(ctx, spec, "", "")
}

// startServerIn is startSessionServer for a session that may already have its
// directory (muster #282): a relaunch passes the directory the previous process
// used, so the runtime finds its database there, and never removes it on failure
// — a session that could not be relaunched keeps what it had for the next try.
func (d *Driver) startServerIn(ctx context.Context, spec fleet.SessionSpec, existingDir, priorCopy string) (*server, error) {
	extra, err := d.sessionEnvFor(spec)
	if err != nil {
		return nil, err
	}
	if extra == nil {
		extra = map[string]string{}
	}
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return nil, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: "create: this driver has been shut down", Machine: d.machine}
	}
	dir := existingDir
	fresh := existingDir == ""
	if fresh {
		dir, err = sessionDirs(d.sessionRoot)
		if err != nil {
			return nil, err
		}
	}
	cleanup := func() {
		if fresh {
			_ = os.RemoveAll(dir)
		}
	}
	// muster #281: the sandbox request is resolved and validated here, before a
	// process exists, so a refusal costs nothing but the directory removed below.
	var plan *sandboxPlan
	var profile string
	if spec.Sandbox != nil {
		plan, err = d.planSandboxFor(spec, dir, priorCopy)
		if err == nil {
			profile, err = plan.profile()
			if err != nil {
				err = &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + err.Error(), Machine: d.machine}
			}
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		if plan.cache != nil {
			extra[packageCacheEnv] = string(plan.cache.Path)
		}
	}
	srv, err := startServer(ctx, d.bin, string(spec.Cwd), d.username, d.buildEnv(dir, extra), profile)
	if err != nil {
		cleanup()
		return nil, err
	}
	if plan != nil {
		srv.sandbox = plan.state()
	}
	srv.dir = dir
	if !fresh {
		// A directory that already held a session is not this call's to remove,
		// whatever happens to the process now.
		srv.persisted.Store(true)
	}
	if !d.trackServer(srv) {
		srv.pauseForShutdown()
		if fresh {
			_ = os.RemoveAll(dir)
		}
		return nil, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: "create: this driver has been shut down", Machine: d.machine}
	}
	return srv, nil
}

// discardServer tears down a server whose session never came to exist. A
// no-op in shared mode, where the server is not this driver's to stop.
func (d *Driver) discardServer(srv *server) {
	if d.shared == nil {
		srv.teardown()
	}
}

// idemBegin claims key for one create. leader is true when the caller now owns
// it; otherwise wait is closed when the create that does own it finishes.
func (d *Driver) idemBegin(key string) (wait <-chan struct{}, leader bool) {
	d.idemMu.Lock()
	defer d.idemMu.Unlock()
	if ch, ok := d.inflight[key]; ok {
		return ch, false
	}
	d.inflight[key] = make(chan struct{})
	return nil, true
}

// idemEnd releases key and wakes anyone waiting on it.
func (d *Driver) idemEnd(key string) {
	d.idemMu.Lock()
	defer d.idemMu.Unlock()
	if ch, ok := d.inflight[key]; ok {
		delete(d.inflight, key)
		close(ch)
	}
}

// pinOutcomeForAgent reports what this driver actually knows about the two
// pins it accepts (muster #84): agent is genuinely observable, because
// opencode's own create response — and the cache markSeen keeps of it —
// names the agent the runtime started with, so a request that reached this
// point (a rejected model shape refuses before ever calling the runtime;
// see Create) either matches or does not, and this driver can say which.
// Model has no such echo anywhere in this substrate's responses, so a
// requested model is reported unresolved rather than asserting a match this
// driver never actually confirmed. Effort refuses at Create rather than
// reaching here at all (§2.1), so it never appears.
func pinOutcomeForAgent(spec fleet.SessionSpec, appliedAgent string) *fleet.PinOutcome {
	out := &fleet.PinOutcome{}
	if spec.Agent != "" {
		out.Agent = fleet.PinResolved(string(spec.Agent), appliedAgent, string(spec.Agent) == appliedAgent,
			fleet.PinObserved, "opencode's own create response names the agent it started with")
	}
	if spec.Model != "" {
		out.Model = fleet.PinUnresolved(spec.Model,
			"this driver's create response does not name which model the runtime is using")
	}
	if out.Agent == nil && out.Model == nil {
		return nil
	}
	return out
}

// sendPrompt is the shared body of Create's optional opening message and
// Send's ordinary delivery.
func (d *Driver) sendPrompt(ctx context.Context, srv *server, id, text string) error {
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	}
	path := "/session/" + url.PathEscape(id) + "/prompt_async"
	return d.do(ctx, srv, "POST", path, body, nil)
}

// Send delivers input (§3) via opencode's prompt_async, which starts the
// session working immediately — there is no staged "composer" this
// substrate holds text in ahead of submission, unlike the tmux driver's
// pane. So Submit:false has nothing honest to do: ErrUnsupported rather
// than silently submitting anyway (§5.6). The same absence of a composer
// means ResumeIfStranded and ReplaceIfStranded (muster #112) are both
// silently no-ops here rather than errors: there is nothing for either to
// act on, and neither opt-in changes this method's behaviour.
//
// This driver has one delivery path and no inbox, so a caller that INSISTS on
// the inbox (route "inbox", #184) is refused — with nothing sent — rather than
// quietly served by the only path there is. An explicit request is never
// downgraded. Auto and terminal are both this driver's one path; its receipts
// name no route, because there is nothing to distinguish.
func (d *Driver) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	if opts.Route == fleet.RouteInbox {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "route \"inbox\" was requested but this runtime has no inbox delivery. Nothing was written",
		}.WithRoute(fleet.RouteInbox), nil
	}
	// #185: a route naming an external delivery module (anything but auto,
	// terminal or inbox, which the service has already validated) cannot be
	// honoured here either: this runtime has one path and no modules.
	// Refused with nothing sent, never served by the one path there is.
	if opts.Route != "" && opts.Route != fleet.RouteAuto && opts.Route != fleet.RouteTerminal && opts.Route != fleet.RouteInbox {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  fmt.Sprintf("route %q names a delivery module but this runtime has none. Nothing was written", string(opts.Route)),
		}, nil
	}
	if !opts.Submit {
		return fleet.DeliveryReceipt{}, driver.ErrUnsupported
	}
	known, ok := d.wasSeen(ref.ID)
	if !ok {
		return fleet.DeliveryReceipt{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if err := d.requireLive(known); err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if err := d.sendPrompt(ctx, known.srv, ref.ID, text); err != nil {
		if isNotFound(err) {
			return fleet.DeliveryReceipt{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return fleet.DeliveryReceipt{}, err
	}
	// A new turn supersedes the one a restart interrupted (muster #282).
	d.clearInterrupted(ref.ID)
	// HTTP 204 from prompt_async means the runtime accepted the message
	// and will process it; it is not synchronous confirmation that the
	// agent has started reading it (that only shows up on the status
	// endpoint moments later, per #55's own measurement). ConfirmsDelivery
	// is false for exactly this reason, and Queued is the honest outcome
	// for it — not Submitted, which this driver has not verified.
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
}

// State reads current state (§3).
//
// This driver's session universe is bounded to what it has seen (package
// doc) — an id it never created or listed is ErrNoSuchSession without a
// round trip. For a known id, the status endpoint alone tells the whole
// story when the id is PRESENT (busy or retry); only an ABSENT id needs a
// second call, to tell "genuinely idle" apart from "no longer exists"
// (§5.7, and #55's own "idle-never-used vs idle-finished" collapse, which
// this driver does not attempt to resolve any further than the runtime
// itself can).
func (d *Driver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	st, err := d.readState(ctx, ref)
	if err != nil {
		return st, err
	}
	// muster #281: the profile is a fact of the session's process, not of this
	// read, so it is stamped on whatever the read concluded — including dead.
	if known, ok := d.wasSeen(ref.ID); ok && known.srv != nil {
		st.Sandbox = known.srv.sandbox
	}
	return st, nil
}

func (d *Driver) readState(ctx context.Context, ref fleet.SessionRef) (fleet.SessionState, error) {
	known, ok := d.wasSeen(ref.ID)
	if !ok {
		return fleet.SessionState{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if known.srv == nil {
		// Listed, not running (muster #282): the reason is the whole answer.
		return fleet.UnknownState(fleet.ConfidenceObserved, known.unknown), nil
	}
	srv := known.srv
	// muster #280: in isolated mode the session IS its process. One that has
	// exited is a session that no longer exists — read from the process itself,
	// not inferred from a failed HTTP call (which would be "unreachable", a
	// different and retryable thing).
	if d.shared == nil && srv.proc.exited() {
		return fleet.InferredState(fleet.StatusDead,
			"the session's runtime process has exited", nil), nil
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var statuses statusMap
	if err := d.do(ctx, srv, "GET", "/session/status", nil, &statuses); err != nil {
		// §5.7, applied at the single-session grain: the read failed, so
		// this is an error — never a synthesized idle. Propagated as-is,
		// which the HTTP layer maps to 504/401 (writeDriverError adopts
		// a *fleet.Error's Kind verbatim), never a 200 carrying "idle".
		return fleet.SessionState{}, err
	}
	if ws, present := statuses[ref.ID]; present {
		return classify(true, ws), nil
	}

	// Absent from a map we just read successfully. Confirm the session
	// still exists before calling it idle — a session this driver saw
	// before but that has since been deleted (by another client, or by
	// this driver's own Close) is `dead`, not `idle`.
	var sess wireSession
	err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(ref.ID), nil, &sess)
	switch {
	case err == nil:
		refreshed := known
		refreshed.cwd, refreshed.name, refreshed.agent = fleet.AbsolutePath(sess.Directory), sess.Title, sess.Agent
		refreshed.startedAt = time.UnixMilli(sess.Time.Created)
		d.markSeen(sess.ID, refreshed)
		st := classify(false, wireStatus{})
		// #77: absent from the status map is idle ONLY when the last thing
		// that happened was not a turn the provider refused. classify has
		// no way to see that on its own — it only ever sees the status map
		// — so it is checked here, once, on the one path that reaches a
		// confirmed-idle verdict.
		st.LastTurn = d.lastTurnFailure(ctx, srv, ref.ID)
		if st.LastTurn == nil {
			st.LastTurn = known.interrupted // muster #282
		}
		return st, nil
	case isNotFound(err):
		return fleet.InferredState(fleet.StatusDead,
			"session was previously observed and no longer exists on the runtime", nil), nil
	default:
		return fleet.SessionState{}, err
	}
}

// Respond is not implemented. opencode's blocking-question surface —
// tool-permission approvals, and a newer structured "question" reply API —
// has no boot-time enumerated-menu equivalent of fleet.SessionPrompt, and
// building one honestly (deciding what counts as a PromptKind, how Nonce
// is derived, what corroboration means here) is real design work outside
// what #55 asks for. ErrUnsupported rather than a guessed mapping (§5.6).
func (d *Driver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	return fleet.DeliveryReceipt{}, driver.ErrUnsupported
}

// Interrupt asks the runtime to abort a session's current turn (§3, 202
// intent-only semantics on the wire — see fleet.Ack).
func (d *Driver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	known, ok := d.wasSeen(ref.ID)
	if !ok {
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if err := d.requireLive(known); err != nil {
		return fleet.Ack{}, err
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	path := "/session/" + url.PathEscape(ref.ID) + "/abort"
	if err := d.do(ctx, known.srv, "POST", path, nil, nil); err != nil {
		if isNotFound(err) {
			return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return fleet.Ack{}, err
	}
	return fleet.Ack{Accepted: true}, nil
}

// Close destroys a session (§3, §5.4).
//
// Corroboration follows the same shape the tmux driver's Close uses: the
// caller's own observed StartedAt, when supplied, is the strong guarantee;
// this driver's own last sighting (seen) is the weaker fallback when the
// caller supplied nothing. Both are §5.4's "at least one independent
// attribute", applied to the one attribute this substrate actually offers
// — time.created never changes for a given id here.
//
// In isolated mode closing the session closes its PROCESS (muster #280): the
// server's whole process group is killed and its scoped directory removed, so
// nothing the session started outlives it. One process holds one session, so
// an id cannot have been recycled inside it and an unreachable server is no
// reason to refuse — the caller's expectation is then checked against the
// cached start time, and the process is torn down regardless.
func (d *Driver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	prior, seen := d.wasSeen(ref.ID)
	if !seen {
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if prior.srv == nil {
		return d.closeUnlaunched(req, ref, prior)
	}
	priorStart := prior.startedAt
	srv := prior.srv
	isolated := d.shared == nil
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var sess wireSession
	liveStart := priorStart
	err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(ref.ID), nil, &sess)
	switch {
	case err == nil:
		liveStart = time.UnixMilli(sess.Time.Created)
	case isNotFound(err):
		// #78: the runtime itself just confirmed this id is gone — the
		// cache entry describes a session that no longer exists, on
		// exactly the same authority Close's own success path relies
		// on below, so it is pruned here too rather than only there.
		d.forgetSeen(ref.ID)
		d.discardServer(srv)
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	case isolated && isUnreachable(err):
		// The server is gone or not answering; the session's process is the
		// session, so closing it is exactly "tear it down". Corroborate with
		// what this driver last saw (liveStart == priorStart).
	default:
		return fleet.Ack{}, err
	}

	if want := req.Expect.StartedAt; want != nil {
		if !liveStart.Equal(*want) {
			return fleet.Ack{}, fmt.Errorf(
				"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
				fleet.ErrAmbiguousTarget, ref.ID, liveStart.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	} else if !liveStart.Equal(priorStart) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q was recycled since this driver last observed it (weak check: caller supplied no expected start time)",
			fleet.ErrAmbiguousTarget, ref.ID)
	}

	if err := d.do(ctx, srv, "DELETE", "/session/"+url.PathEscape(ref.ID), nil, nil); err != nil {
		switch {
		case isNotFound(err):
			d.forgetSeen(ref.ID)
			d.discardServer(srv)
			return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		case isolated:
			// Best effort: the process is about to be killed, which removes the
			// session more thoroughly than the request would have.
		default:
			return fleet.Ack{}, err
		}
	}
	// #78: List answers entirely from this driver's own cache (knownIDs),
	// and nothing had ever pruned a closed session from it — a session the
	// runtime's own store had genuinely dropped kept being listed,
	// correctly attributed, indefinitely. The map itself stays: it is the
	// documented workaround for the runtime's own unreliable bulk listing,
	// not the defect. Only the missing prune on a confirmed close is.
	d.forgetSeen(ref.ID)
	// Announced before the teardown, and the server marked gone first, so the
	// bus connection ending with the process is not reported as a gap (#284).
	d.announceClosed(srv, prior, fleet.SessionRef{Machine: d.machine, ID: ref.ID, Name: prior.name})
	d.discardServer(srv)
	return fleet.Ack{Accepted: true}, nil
}

// Discard is not implemented. There is no composer holding unsent text on
// this substrate for the same reason Send refuses Submit:false — a message
// delivered here is delivered, not staged. ErrUnsupported (§5.6).
func (d *Driver) Discard(ctx context.Context, req fleet.Request, ref fleet.SessionRef, expectDigest string, opts driver.DiscardOptions) (fleet.Ack, error) {
	return fleet.Ack{}, driver.ErrUnsupported
}

// Rename is not implemented. opencode has a session title, not an
// addressable name this driver's ids are keyed on; renaming would change
// display text without changing the id callers actually address by,
// which is not what §3's rename means. ErrUnsupported (§5.6) rather than
// a rename that silently does something else.
func (d *Driver) Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.RenameAck, error) {
	return fleet.RenameAck{}, driver.ErrUnsupported
}

// List returns every session this driver knows about (see the package
// doc's scope-boundary note) in one Collection (§9, driver.Driver.List's
// doc comment on why the return type is Collection[Session] rather than a
// bare slice).
//
// # A measured finding this driver deliberately works around
//
// The obvious implementation calls GET /session for the whole listing and
// filters it down to known ids. That endpoint's behaviour is not what its
// OpenAPI description promises: measured live, a fresh server queried with
// no `directory` filter returned ZERO sessions for one this very driver had
// just created and could immediately read back by id (GET /session/{id}
// succeeded throughout). It appears to be scoped to some notion of a
// "current project" this driver never set, undocumented in the API
// description, and not worth depending on.
//
// So List never calls it. Every session this driver knows about already has
// its cwd, title and agent cached locally from the moment it was created or
// last read (see knownSession, markSeen) — the same memory State uses to
// tell "genuinely idle" apart from "no longer exists" — so this needs only
// ONE HTTP call regardless of how many sessions this driver knows about:
// GET /session/status for the whole busy/retry map. The tmux driver's List
// measured and documented an equivalent O(1)-spawns discipline for its own
// substrate; this is that same discipline reached by a different route
// after the substrate's own listing endpoint turned out not to be usable
// for it.
//
// The cost carried forward: a session this driver's cache still holds but
// that was deleted by another client (never through this driver's own
// Close) keeps appearing here — as idle, since it is absent from the status
// map — until a direct State() call on it discovers the 404 and reclassifies
// it as dead. This is the same class of staleness SupportsResume: false
// already declares, not a new one, and is stated here rather than hidden.
func (d *Driver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	known := d.knownIDs()
	now := d.now()
	sourceStatus := fleet.SourceOK

	// One status read per DISTINCT server, concurrently. In shared mode that is
	// one read; in isolated mode one per live session, and a session whose
	// process has exited is reported dead without any read.
	type statusRead struct {
		statuses statusMap
		err      error
	}
	reads := make(map[*server]*statusRead)
	for _, info := range known {
		if info.srv == nil {
			continue // listed, not running (muster #282): nothing to read
		}
		if _, ok := reads[info.srv]; !ok {
			reads[info.srv] = &statusRead{}
		}
	}
	var wg sync.WaitGroup
	for srv, r := range reads {
		if d.shared == nil && srv.proc.exited() {
			continue
		}
		wg.Add(1)
		go func(srv *server, r *statusRead) {
			defer wg.Done()
			r.err = d.do(ctx, srv, "GET", "/session/status", nil, &r.statuses)
		}(srv, r)
	}
	wg.Wait()

	out := make([]fleet.Session, 0, len(known))
	var firstErr error
	for id, info := range known {
		r := reads[info.srv]
		var st fleet.SessionState
		switch {
		case info.srv == nil:
			st = fleet.UnknownState(fleet.ConfidenceObserved, info.unknown)
		case d.shared == nil && info.srv.proc.exited():
			st = fleet.InferredState(fleet.StatusDead, "the session's runtime process has exited", nil)
		case r.err != nil:
			// Session identity is known (this driver's own cache); which of
			// them are busy is not. Degraded, not unreachable — this machine
			// answered, only partially.
			if firstErr == nil {
				firstErr = r.err
			}
			sourceStatus = fleet.SourceDegraded
			st = fleet.UnknownState(fleet.ConfidenceObserved,
				fmt.Sprintf("session identity is known but its status could not be read: %v", r.err))
		default:
			ws, present := r.statuses[id]
			st = classify(present, ws)
			if !present && info.interrupted != nil {
				st.LastTurn = info.interrupted // muster #282
			}
		}

		if info.srv != nil {
			st.Sandbox = info.srv.sandbox // muster #281
		}
		startedAt := info.startedAt
		sess := fleet.Session{
			SessionRef: fleet.SessionRef{Machine: d.machine, ID: id, Name: info.name},
			StartedAt:  &startedAt,
			Runtime:    d.runtime,
			Cwd:        info.cwd,
			Agent:      fleet.AgentId(info.agent),
			State:      st,
		}
		if matchesFilter(sess, filter) {
			out = append(out, sess)
		}
	}

	src := fleet.SourceStatus{Machine: d.machine, Status: sourceStatus, ObservedAt: now}
	if firstErr != nil {
		src.Error = firstErr.Error()
	}
	return fleet.NewCollection(out, []fleet.SourceStatus{src})
}
