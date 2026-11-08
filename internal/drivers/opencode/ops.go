package opencode

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/privfs"
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
// passes Agent on every message it sends (the runtime chooses the agent per
// message, so the create body alone pins nothing — muster #362) and refuses one
// the runtime does not list; it carries Model in the create body and reports it
// unresolved until a turn's own record names the model it ran on (muster #373);
// and it REFUSES rather than silently drops Effort, McpConfig,
// Settings (muster #247), Resume and ConversationId (muster #224) — none of which
// this substrate has an honest mechanism for. ContextRef is honoured (muster
// #371): the file is read here, before anything starts, and its text rides on
// every prompt as the runtime's system field (context.go). PermissionMode
// "bypass" (muster #283) is honoured only together with a sandbox this driver
// enforces, and refused `unsupported` otherwise (permission.go). Env is honoured
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
				// By now a turn may have run; its record is the one thing that can
				// resolve the agent pin (muster #362).
				Pins:         pinOutcomeForAgent(spec, d.turnEvidenceFor(ctx, info, ref.ID, spec)),
				Conversation: info.conversationRef(),
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
		"effort":    spec.Effort != "",
		"mcpConfig": len(spec.McpConfig) > 0,
		// muster #247: launch-time CLI settings have no meaning for a runtime
		// with no CLI to hand them to.
		"settings": len(spec.Settings) > 0,
		"resume":   spec.Resume != "",
		// muster #224: opencode's session.create has no equivalent of
		// Claude Code's --session-id — this substrate assigns its own id and
		// this driver has no honest way to make it start under a caller's
		// instead. Refused for the same reason as resume, three lines above.
		"conversationId": spec.ConversationId != "",
		// muster #392: the server loads the account's own configuration (its
		// user-level config and tool servers) before any session exists, and has
		// no per-session switch that narrows what it loaded. Never offered, so
		// Capabilities.IsolatesConfiguration is false and the service refuses a
		// posture asking for it first (posture-unenforceable); this is the
		// driver's own refusal for a caller that sets the field directly.
		"isolateConfiguration": spec.IsolateConfiguration,
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
	// muster #283: bypass is honoured inside an enforced sandbox only. Checked
	// after the sandbox refusal above so a host that cannot confine says so in
	// the capability's own words.
	if err := d.checkPermissionMode(spec); err != nil {
		return fleet.Session{}, err
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

	// muster #371: the brief is read and validated before a process exists, so an
	// unreadable one costs nothing.
	contextText, err := d.readContext(spec.ContextRef)
	if err != nil {
		return fleet.Session{}, err
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
		// muster #293: before a process exists, so a refusal costs nothing.
		if err := d.checkModelProvider(spec.Model); err != nil {
			return fleet.Session{}, err
		}
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
	// muster #287: a wrong id would otherwise be accepted here and fail at the
	// first turn, opaquely. muster #293: a runtime with nothing the operator
	// allows would otherwise answer on whatever it has, so that is a refusal too —
	// read for every create, with or without a model. Refused before any session
	// exists, and the process started for it is torn down again.
	if err := d.checkProviders(ctx, srv, spec.Model, spec.Cwd); err != nil {
		d.discardServer(srv)
		return fleet.Session{}, err
	}
	// muster #362: an agent the runtime does not list would be echoed back by the
	// create and replaced by a default at the first turn. Refused here, on the same
	// terms and at the same point as a model outside the catalog.
	if err := d.checkAgent(ctx, srv, spec.Cwd, string(spec.Agent)); err != nil {
		d.discardServer(srv)
		return fleet.Session{}, err
	}

	// muster #419: a session is about to appear on this server, and a prompt may
	// start it; whatever was read of the server before is no longer the truth.
	defer d.invalidateStatus(srv)

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
	created := knownSession{
		srv: srv, cwd: cwd, name: sess.Title, agent: sess.Agent, specAgent: string(spec.Agent), startedAt: startedAt,
		contextRef: spec.ContextRef, context: contextText,
		// muster #378: the runtime named this session's id in its create response.
		conversation: capturedAtCreate(sess.ID),
	}
	if d.store != nil && d.shared == nil {
		// Before the session is announced or handed an id: a session the caller
		// can see but a restart cannot find would make SupportsResume a lie.
		rec := &sessionRecord{
			ID: sess.ID, Cwd: string(cwd), Name: sess.Title, Agent: sess.Agent,
			SpecAgent: string(spec.Agent), SpecMarker: spec.Marker,
			StartedAtMs: sess.Time.Created, Dir: srv.dir, EnvNames: callerEnvNames,
			Sandbox: spec.Sandbox, PermissionMode: spec.PermissionMode,
			// The path, never the text: a restart reads the file again (muster #371).
			ContextRef: string(spec.ContextRef),
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
		if err := d.sendPrompt(ctx, srv, sess.ID, spec.Prompt, string(spec.Agent), contextText); err != nil {
			// The session exists but could not be started. Reported as a
			// failure of THIS create — a caller retrying with the same
			// idempotency key gets the session back (see below) and may
			// resend the prompt via Send.
			d.idemStore(key, ref)
			return fleet.Session{}, fmt.Errorf("create: session %s was started but its opening prompt failed: %w", sess.ID, err)
		}
		d.turnStarted(sess.ID) // muster #426
	}
	d.idemStore(key, ref)
	// No turn has run yet, so there is nothing to resolve the agent pin from.
	return fleet.Session{
		SessionRef: ref, Cwd: spec.Cwd, Agent: fleet.AgentId(sess.Agent),
		Pins:         pinOutcomeForAgent(spec, turnEvidence{}),
		Conversation: created.conversationRef(),
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
	// muster #396: the operator's agents are read here, before a directory or a
	// process exists, on every create and every relaunch — an unusable file
	// refuses the start, it is never skipped.
	agents, err := d.operatorAgentsFor()
	if err != nil {
		return nil, err
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
			_ = privfs.RemoveAll(dir)
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
	var args []string
	bypass := spec.PermissionMode == fleet.PermissionModeBypass
	// muster #283, #293, #298: the tool posture, the outside-directory rules
	// (derived from the sandbox plan above) and the provider policy live in this
	// session's own directory, never in a file another session would read. The
	// provider policy goes to EVERY session — a session with no credential must
	// not fall back to the runtime's hosted provider — and is written again on a
	// relaunch, so a changed operator setting applies from the next start. The
	// operator's agents (muster #396) ride in the same file for the same reason.
	if err := writeSessionConfig(dir, bypass, plan, d.providers, agents); err != nil {
		cleanup()
		return nil, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + err.Error(), Machine: d.machine}
	}
	// muster #420: the runtime's own log is owner-only before the runtime can write
	// to it, on a relaunch as on a first start.
	if err := privateRuntimeLog(dir); err != nil {
		cleanup()
		return nil, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + err.Error(), Machine: d.machine}
	}
	if bypass {
		args = bypassArgs()
	}
	srv, err := startServer(ctx, d.bin, string(spec.Cwd), d.username, d.buildEnv(dir, extra), profile, args...)
	if err != nil {
		cleanup()
		return nil, err
	}
	if spec.PermissionMode == fleet.PermissionModeBypass {
		srv.permission = fleet.PermissionModeBypass
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
			_ = privfs.RemoveAll(dir)
		}
		return nil, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: "create: this driver has been shut down", Machine: d.machine}
	}
	srv.touch(d.now()) // muster #421: a server that has just started is not idle
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
// pins it accepts (muster #84).
//
// Both are resolved only from the runtime's own record of a turn that ran
// (muster #362 for the agent, #373 for the model; agents.go). The create response
// is NOT that record: the runtime echoes back whatever agent name it was given,
// listed or not, and a message sent without an agent runs as the default one — so
// an echo that matched the request proved nothing, and reading it as "honoured"
// was the defect this replaced. The same response names no model at all. Until a
// turn's record names the agent (or the model) the pin is unresolved, with the
// reason; once it does, honoured says whether the turn ran as the agent that was
// asked for (or on the model that was), and applied names the one it did run as —
// the model as "provider/model", the spelling a create asks for.
//
// The two resolve apart: an assistant message that names the model and not the
// agent resolves the model pin and leaves the agent pin unresolved, and the other
// way round. Effort refuses at Create rather than reaching here at all (§2.1), so
// it never appears.
func pinOutcomeForAgent(spec fleet.SessionSpec, ev turnEvidence) *fleet.PinOutcome {
	out := &fleet.PinOutcome{}
	if spec.Agent != "" {
		if ev.ran {
			out.Agent = fleet.PinResolved(string(spec.Agent), ev.agent, string(spec.Agent) == ev.agent, fleet.PinObserved,
				"the runtime's own record of the session's newest assistant message names the agent that turn ran as")
		} else {
			out.Agent = fleet.PinUnresolved(string(spec.Agent), pinAgentUnresolvedEvidence)
		}
	}
	if spec.Model != "" {
		if ev.modelRan {
			out.Model = fleet.PinResolved(spec.Model, ev.modelUsed, spec.Model == ev.modelUsed, fleet.PinObserved,
				"the runtime's own record of the session's newest assistant message names the provider and model that turn ran on")
		} else {
			out.Model = fleet.PinUnresolved(spec.Model, pinModelUnresolvedEvidence)
		}
	}
	if out.Agent == nil && out.Model == nil {
		return nil
	}
	return out
}

// sendPrompt is the shared body of Create's optional opening message and
// Send's ordinary delivery.
//
// agent is the session's requested agent, or empty for none. It rides on EVERY
// message (muster #362): in this runtime the agent is chosen per message, and one
// sent without it runs as the default agent whatever the session was created
// with. Empty is the session that never asked for one, which runs as the default
// by its own choice.
//
// system is the session's context (muster #371), or empty for a session with
// none. It rides on EVERY prompt as the runtime's own system field and never in
// the user part: the field applies to the one prompt that carries it, so a prompt
// sent without it runs without the context (context.go has the measurement).
func (d *Driver) sendPrompt(ctx context.Context, srv *server, id, text, agent, system string) error {
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	}
	if agent != "" {
		body["agent"] = agent
	}
	if system != "" {
		body["system"] = system
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
	// muster #421: a session whose server was stopped for being idle is started
	// again here, under the session's lock, and held until the prompt is accepted.
	known, unlock, err := d.liveFor(ctx, ref.ID)
	if err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	defer unlock()
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	// muster #419: the runtime accepts a prompt before it reports the session
	// working; a status read made in between must not be kept.
	defer d.invalidateStatus(known.srv)

	if err := d.sendPrompt(ctx, known.srv, ref.ID, text, known.specAgent, known.context); err != nil {
		if isNotFound(err) {
			return fleet.DeliveryReceipt{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return fleet.DeliveryReceipt{}, err
	}
	// A new turn supersedes the one a restart interrupted (muster #282), and is
	// one a listing will have to read the end of (muster #426).
	d.turnStarted(ref.ID)
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
		st.PermissionMode = known.srv.permission // muster #283
		// muster #285: only where the session has a live server to ask. A read
		// that failed or found the session gone has no figures worth adding.
		if st.Status != fleet.StatusDead && st.Status != fleet.StatusUnknown {
			if d.phaseOf(known.srv) == phaseParked {
				// muster #421: no process to ask, so what was read before the stop.
				st.Usage = d.usageSnapshot(ref.ID, true)
			} else {
				st.Usage = d.usageForState(ctx, known.srv, ref.ID, st.Status == fleet.StatusIdle)
			}
			st.Scratch = d.scratchOf(known.srv) // muster #419: the last walk's figure, never a wait
		}
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
	// different and retryable thing). Unless this driver stopped it on purpose
	// (muster #421): that is an idle session, read from memory — a state read never
	// starts a process.
	switch d.phaseOf(srv) {
	case phaseParked:
		return d.parkedState(known), nil
	case phaseExited:
		return fleet.InferredState(fleet.StatusDead,
			"the session's runtime process has exited", nil), nil
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	statuses, err := d.statusOf(ctx, srv)
	if err != nil {
		// §5.7, applied at the single-session grain: the read failed, so
		// this is an error — never a synthesized idle. Propagated as-is,
		// which the HTTP layer maps to 504/401 (writeDriverError adopts
		// a *fleet.Error's Kind verbatim), never a 200 carrying "idle".
		return fleet.SessionState{}, err
	}
	if ws, present := statuses[ref.ID]; present {
		// muster #283: busy is the runtime's word for "its turn has not ended",
		// which a turn stopped on a permission ask also satisfies.
		st := d.withPermission(ctx, srv, ref.ID, classify(true, ws))
		if st.Status == fleet.StatusWorking || st.Status == fleet.StatusWaitingInput {
			d.sawWorking(ref.ID) // muster #426: a turn is running, so the last one is no longer the newest
		}
		return st, nil
	}

	// Absent from a map we just read successfully. Confirm the session
	// still exists before calling it idle — a session this driver saw
	// before but that has since been deleted (by another client, or by
	// this driver's own Close) is `dead`, not `idle`.
	//
	// muster #419: what this confirmation reads is kept for the window
	// (statuscache.go), so a session polled while idle costs the runtime one
	// confirmation per window and not one per poll.
	var (
		sess     wireSession
		lastTurn *fleet.TurnEnd
	)
	if m, kept := d.idleFor(srv, ref.ID); kept {
		sess, lastTurn = m.sess, copyTurnEnd(m.lastTurn)
	} else {
		gen := srv.status.generation()
		err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(ref.ID), nil, &sess)
		switch {
		case err == nil:
			// #77: absent from the status map is idle ONLY when the last thing
			// that happened was not a turn the provider refused. classify has
			// no way to see that on its own — it only ever sees the status map
			// — so it is checked here, once, on the one path that reaches a
			// confirmed-idle verdict.
			res := d.turnVerdict(ctx, srv, ref.ID)
			lastTurn = res.turn
			// muster #426: what this read learned is kept for listings too, which
			// also puts the record right after a restart.
			d.noteTurn(ref.ID, known.turn.gen, res, false)
			d.keepIdle(srv, ref.ID, gen, idleMemo{sess: sess, lastTurn: copyTurnEnd(lastTurn)})
		case isNotFound(err):
			d.forgetIdle(srv, ref.ID)
			return fleet.InferredState(fleet.StatusDead,
				"session was previously observed and no longer exists on the runtime", nil), nil
		default:
			return fleet.SessionState{}, err
		}
	}
	// Merged into the entry as it is now, not written back from the copy read above:
	// the session's server may have been replaced meanwhile (muster #421), and a
	// session closed meanwhile must not be put back.
	d.refreshSeen(sess.ID, func(k *knownSession) {
		k.cwd, k.name, k.agent = fleet.AbsolutePath(sess.Directory), sess.Title, sess.Agent
		k.startedAt = time.UnixMilli(sess.Time.Created)
	})
	st := classify(false, wireStatus{})
	st.LastTurn = lastTurn
	if st.LastTurn == nil {
		st.LastTurn = known.interrupted // muster #282
	}
	return st, nil
}

// copyTurnEnd returns a copy a caller may hold: a kept one is shared.
func copyTurnEnd(t *fleet.TurnEnd) *fleet.TurnEnd {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// usageForState is the usage figures State attaches. For a session that is idle
// they are kept for the window with the rest of its confirmation, so polling an
// idle session does not read its message record on every poll; for any other it
// reads, as before.
func (d *Driver) usageForState(ctx context.Context, srv *server, id string, idle bool) *fleet.Usage {
	if !idle {
		return d.usageOf(ctx, srv, id, false)
	}
	if m, kept := d.idleFor(srv, id); kept && m.haveUsage {
		return copyUsage(m.usage)
	}
	gen := srv.status.generation()
	u := d.usageOf(ctx, srv, id, true)
	d.keepIdleUsage(srv, id, gen, copyUsage(u))
	return u
}

// Respond answers the tool-permission ask a session is parked on (§3, muster
// #296): once, always or reject, through the runtime's own permission-reply
// endpoint. permission.go has the contract and what is refused.
//
// Only a tool-permission ask is answerable here. A runtime "question" is a
// different surface with a different shape, and mapping it onto this one would be
// the guessed mapping §5.6 forbids: such a session is answered `unsupported`,
// with the reason, and the caller still has Interrupt.
func (d *Driver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	known, unlock, err := d.liveFor(ctx, ref.ID) // muster #421: starts a parked session again
	if err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	defer unlock()
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	defer d.invalidateStatus(known.srv) // muster #419: an answered ask changes the status
	return d.answerAsk(ctx, known.srv, ref.ID, resp)
}

// Interrupt asks the runtime to abort a session's current turn (§3, 202
// intent-only semantics on the wire — see fleet.Ack).
func (d *Driver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	known, unlock, err := d.liveFor(ctx, ref.ID) // muster #421: starts a parked session again
	if err != nil {
		return fleet.Ack{}, err
	}
	defer unlock()
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	defer d.invalidateStatus(known.srv) // muster #419: an aborted turn changes the status
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
	// muster #421: under the session's lock, so a close and a park or a start of the
	// same session do not interleave.
	defer d.lockSession(ref.ID)()
	prior, seen := d.wasSeen(ref.ID)
	if !seen {
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if prior.srv == nil {
		return d.closeUnlaunched(req, ref, prior)
	}
	if d.phaseOf(prior.srv) == phaseParked {
		// muster #421: no process to stop and none to ask, exactly as for a session
		// found on disk and never started: closing it is removing what it left. The
		// directory is the server's, so it is handed over here.
		prior.dir = prior.srv.dir
		return d.closeUnlaunched(req, ref, prior)
	}
	priorStart := prior.startedAt
	srv := prior.srv
	isolated := d.shared == nil
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	defer d.invalidateStatus(srv) // muster #419: in shared mode the server outlives the close

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
// # One call per server per window, not per listing (muster #419)
//
// In isolated mode every session has a server of its own, so that one call is
// one per live server, and a listing made once a second made every idle server
// answer a request every second — measured at about 2 % of a core per request
// per second, for servers that were doing nothing else. The status map is
// therefore read through a cache (statuscache.go): shared by concurrent
// listings, kept for a short window, and dropped as soon as the runtime's event
// stream or one of this driver's own writes says it changed. A listing can show
// a change the runtime made on its own up to the window late — 2 s with no event
// stream attached to the server, 30 s with one that went quiet while staying
// open — and never a change of this driver's making.
//
// The cost carried forward: a session this driver's cache still holds but
// that was deleted by another client (never through this driver's own
// Close) keeps appearing here — as idle, since it is absent from the status
// map — until a direct State() call on it discovers the 404 and reclassifies
// it as dead. This is the same class of staleness SupportsResume: false
// already declares, not a new one, and is stated here rather than hidden.
//
// # How the last turn ended (muster #363, #426)
//
// idle alone is not how a session's last turn ended, and the runtime's status map
// does not say: a turn the provider refused leaves the map and the session reads
// idle. The runtime's message record does say, at the price of one request per
// session. With filter.ID set the call is about ONE session, so that request is made
// (lastTurnFailure): the answer a supervisor polls for one session is State's. An
// unfiltered listing does not ask per session per call. It reports what this driver
// has learned (lastturn.go): the failure a turn's end reported on the event bus, a
// State read found, or a read this listing made itself (readFinishedTurns), which is
// one for a session that was seen working or was sent input since the last, and none
// for an idle session nothing has happened to. A listing therefore shows a failed
// turn whether or not anything is subscribed, and keeps its constant cost.
//
// What it cannot show is a failure the driver has not learned of: after a service
// restart the record is empty, so a row omits it until a turn runs or a read by id
// finds it. The read by id stays the authority.
func (d *Driver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	known := d.knownIDs()
	if filter.ID != "" {
		// muster #363: a caller that wants one session is not served by
		// reading the status of every server this driver runs.
		only := make(map[string]knownSession, 1)
		if info, ok := known[filter.ID]; ok {
			only[filter.ID] = info
		}
		known = only
	}
	now := d.now()
	sourceStatus := fleet.SourceOK

	// One status read per DISTINCT server, concurrently. In shared mode that is
	// one read; in isolated mode one per live session, and a session whose
	// process has exited is reported dead without any read.
	type statusRead struct {
		statuses statusMap
		err      error
		// ids are the sessions of the server this listing is about (muster #426).
		ids []string
	}
	reads := make(map[*server]*statusRead)
	for id, info := range known {
		if info.srv == nil {
			continue // listed, not running (muster #282): nothing to read
		}
		r, ok := reads[info.srv]
		if !ok {
			r = &statusRead{}
			reads[info.srv] = r
		}
		r.ids = append(r.ids, id)
	}
	var wg sync.WaitGroup
	for srv, r := range reads {
		if d.phaseOf(srv) != phaseRunning {
			continue // exited, or parked (muster #421): nothing to ask
		}
		wg.Add(1)
		go func(srv *server, r *statusRead) {
			defer wg.Done()
			r.statuses, r.err = d.statusOf(ctx, srv)
			if r.err == nil && filter.ID == "" {
				d.readFinishedTurns(ctx, srv, r.ids, r.statuses)
			}
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
		case d.phaseOf(info.srv) == phaseParked:
			st = d.parkedState(info) // muster #421: idle, from memory; a listing never starts a process
		case d.phaseOf(info.srv) == phaseExited:
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
			if present {
				// One extra read per BUSY session, never per idle one: a
				// parked ask must not show as working in a listing either (#283).
				st = d.withPermission(ctx, info.srv, id, st)
				if (st.Status == fleet.StatusWorking || st.Status == fleet.StatusWaitingInput) && !info.turn.seenWorking {
					d.sawWorking(id) // muster #426: the last turn is no longer the newest
				}
			}
			if !present {
				// muster #363: absent from the status map is idle, and idle
				// alone is not how the last turn ended. State reads the
				// runtime's newest message to say so; a read about ONE session
				// can afford the same single request.
				if filter.ID != "" {
					res := d.turnVerdict(ctx, info.srv, id)
					d.noteTurn(id, info.turn.gen, res, false) // muster #426
					st.LastTurn = res.turn
					if st.LastTurn == nil {
						st.LastTurn = info.interrupted // muster #282
					}
				} else {
					// muster #426: a whole listing cannot, so it shows what this
					// driver learned of the turn's end; readFinishedTurns made the
					// one read a turn that just ended needs.
					st.LastTurn = d.recordedTurn(id)
				}
			}
		}

		if info.srv != nil {
			st.Sandbox = info.srv.sandbox           // muster #281
			st.PermissionMode = info.srv.permission // muster #283
			// muster #285: what a State read or a turn's end last learned, with no
			// request made here; AsOf says how old it is.
			st.Usage = d.usageSnapshot(id, st.Status == fleet.StatusIdle)
			if st.Status != fleet.StatusDead {
				st.Scratch = d.scratchOf(info.srv) // muster #419
			}
		}
		startedAt := info.startedAt
		sess := fleet.Session{
			SessionRef: fleet.SessionRef{Machine: d.machine, ID: id, Name: info.name},
			StartedAt:  &startedAt,
			Runtime:    d.runtime,
			Cwd:        info.cwd,
			Agent:      fleet.AgentId(info.agent),
			State:      st,
			// muster #378: the session's own id, captured at create.
			Conversation: info.conversationRef(),
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
