package opencode

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Create starts a session (§3), honouring the caller's idempotency key
// (§10) with an in-memory table — see the package doc's SupportsResume
// note for why this driver does not attempt to survive a restart.
//
// Agent, Model and Effort are §2.1's "hints, not guarantees": this driver
// genuinely honours Agent and Model (opencode's own create body carries
// both) and REFUSES rather than silently drops Effort, Env, ContextRef,
// McpConfig, Settings (muster #247), PermissionMode, Resume and ConversationId (muster #224) —
// none of which this substrate has an honest mechanism for. TrustCwd and
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
	if spec.Cwd == "" {
		return fleet.Session{}, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: cwd is required", Machine: d.machine}
	}
	for name, set := range map[string]bool{
		"effort":     spec.Effort != "",
		"env":        len(spec.Env) > 0,
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
		// muster #280: a driver in shared mode (one server for every session)
		// has no way to give a session an environment of its own.
		"isolateEnvironment": spec.IsolateEnvironment,
	} {
		if set {
			return fleet.Session{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("create: this driver has no honest way to honour %q (§2.1: refuse rather than drop a hint silently)", name),
				Machine: d.machine,
			}
		}
	}

	ctx, cancel := d.bounded(ctx)
	defer cancel()

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

	var sess wireSession
	path := "/session?directory=" + url.QueryEscape(string(spec.Cwd))
	if err := d.do(ctx, "POST", path, body, &sess); err != nil {
		return fleet.Session{}, err
	}
	startedAt := time.UnixMilli(sess.Time.Created)
	ref := fleet.SessionRef{Machine: d.machine, ID: sess.ID, Name: sess.Title}
	cwd := fleet.AbsolutePath(sess.Directory)
	if cwd == "" {
		cwd = spec.Cwd
	}
	d.markSeen(sess.ID, knownSession{cwd: cwd, name: sess.Title, agent: sess.Agent, startedAt: startedAt})

	if spec.Prompt != "" {
		if err := d.sendPrompt(ctx, sess.ID, spec.Prompt); err != nil {
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
func (d *Driver) sendPrompt(ctx context.Context, id, text string) error {
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	}
	path := "/session/" + url.PathEscape(id) + "/prompt_async"
	return d.do(ctx, "POST", path, body, nil)
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
	if _, ok := d.wasSeen(ref.ID); !ok {
		return fleet.DeliveryReceipt{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if err := d.sendPrompt(ctx, ref.ID, text); err != nil {
		if isNotFound(err) {
			return fleet.DeliveryReceipt{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return fleet.DeliveryReceipt{}, err
	}
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
	if _, ok := d.wasSeen(ref.ID); !ok {
		return fleet.SessionState{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var statuses statusMap
	if err := d.do(ctx, "GET", "/session/status", nil, &statuses); err != nil {
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
	err := d.do(ctx, "GET", "/session/"+url.PathEscape(ref.ID), nil, &sess)
	switch {
	case err == nil:
		d.markSeen(sess.ID, knownSession{
			cwd: fleet.AbsolutePath(sess.Directory), name: sess.Title, agent: sess.Agent,
			startedAt: time.UnixMilli(sess.Time.Created),
		})
		st := classify(false, wireStatus{})
		// #77: absent from the status map is idle ONLY when the last thing
		// that happened was not a turn the provider refused. classify has
		// no way to see that on its own — it only ever sees the status map
		// — so it is checked here, once, on the one path that reaches a
		// confirmed-idle verdict.
		st.LastTurn = d.lastTurnFailure(ctx, ref.ID)
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
	if _, ok := d.wasSeen(ref.ID); !ok {
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	path := "/session/" + url.PathEscape(ref.ID) + "/abort"
	if err := d.do(ctx, "POST", path, nil, nil); err != nil {
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
func (d *Driver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	prior, seen := d.wasSeen(ref.ID)
	if !seen {
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	priorStart := prior.startedAt
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var sess wireSession
	err := d.do(ctx, "GET", "/session/"+url.PathEscape(ref.ID), nil, &sess)
	if err != nil {
		if isNotFound(err) {
			// #78: the runtime itself just confirmed this id is gone — the
			// cache entry describes a session that no longer exists, on
			// exactly the same authority Close's own success path relies
			// on below, so it is pruned here too rather than only there.
			d.forgetSeen(ref.ID)
			return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return fleet.Ack{}, err
	}
	liveStart := time.UnixMilli(sess.Time.Created)

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

	if err := d.do(ctx, "DELETE", "/session/"+url.PathEscape(ref.ID), nil, nil); err != nil {
		if isNotFound(err) {
			d.forgetSeen(ref.ID)
			return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return fleet.Ack{}, err
	}
	// #78: List answers entirely from this driver's own cache (knownIDs),
	// and nothing had ever pruned a closed session from it — a session the
	// runtime's own store had genuinely dropped kept being listed,
	// correctly attributed, indefinitely. The map itself stays: it is the
	// documented workaround for the runtime's own unreliable bulk listing,
	// not the defect. Only the missing prune on a confirmed close is.
	d.forgetSeen(ref.ID)
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
	var statusErr error
	var statuses statusMap
	if err := d.do(ctx, "GET", "/session/status", nil, &statuses); err != nil {
		statusErr = err
		// Session identity is known (this driver's own cache); which of
		// them are busy is not. Degraded, not unreachable — this machine
		// answered, only partially.
		sourceStatus = fleet.SourceDegraded
	}

	out := make([]fleet.Session, 0, len(known))
	for id, info := range known {
		var st fleet.SessionState
		if statusErr != nil {
			st = fleet.UnknownState(fleet.ConfidenceObserved,
				fmt.Sprintf("session identity is known but its status could not be read: %v", statusErr))
		} else {
			ws, present := statuses[id]
			st = classify(present, ws)
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
	if statusErr != nil {
		src.Error = statusErr.Error()
	}
	return fleet.NewCollection(out, []fleet.SourceStatus{src})
}

// Subscribe is not implemented. opencode publishes a global SSE event bus
// (GET /event) that could in principle back this, but mapping its event
// vocabulary onto SubscribeFilter and fleet.Event honestly — which events
// mean session.state, which mean something this model has no analogue
// for — is real design work left undone here rather than half-mapped.
// ErrUnsupported, not ErrNotReady: nothing about this being "not yet" would
// change by asking again (§5.6, and see driver.ErrNotReady's own doc
// comment for the case that distinction exists to cover).
func (d *Driver) Subscribe(ctx context.Context, req fleet.Request, filter driver.SubscribeFilter) (driver.EventStream, error) {
	return nil, driver.ErrUnsupported
}
