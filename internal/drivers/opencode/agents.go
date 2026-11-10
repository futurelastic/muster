package opencode

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// This file is everything the driver knows about the AGENT pin (muster #362), and
// the one record both pins are read from: the model pin (muster #373) resolves from
// the same newest assistant message (turnEvidence), so it is read here too.
//
// In this runtime the agent is chosen per MESSAGE, not per session. The session
// record carries an agent name, but the runtime echoes whatever name the create
// gave it without looking it up, and a message sent without an `agent` runs as
// the runtime's default one. Measured: a create naming an agent that exists
// nowhere, and one naming a real project agent, both answered with the name they
// were given; the first turn of each ran as the default agent. So the pin has
// three parts, and each is here or in sendPrompt:
//
//   - every message the driver sends names the session's agent (ops.go);
//   - a name the runtime does not list is refused at create (checkAgent), the
//     same way a model outside the catalog is, instead of being accepted and then
//     replaced by a default at the first turn;
//   - a pin is reported honoured only from the runtime's own record of a turn
//     that ran (turnEvidence), never from the create response.
//
// An agent may also pin a model of its own (muster #402): an operator-defined
// agent (operatoragents.go) or a project's own may carry one, and a turn run as
// that agent uses it. Measured against the real runtime: an agent that pins
// `model` is listed with `model: {providerID, modelID}`; one that does not is
// listed without the key. checkAgent reads it from the same listing it already
// reads for the name, and judges it against the provider policy before any
// session exists.

// wireAgent is the subset of one GET /agent entry this driver reads. The
// runtime lists its built-in agents, the operator's configured ones and, when
// the request carries the session's directory, the project's own.
type wireAgent struct {
	Name string `json:"name"`
	// Model is the model the agent pins, absent for an agent that pins none.
	Model *struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
	} `json:"model"`
}

// listedAgent is one agent the runtime listed: its name, and the
// "provider/model" id it pins — the spelling a create asks for — or "" when it
// pins none. One half of a model alone is not a model, as in turnEvidence.
type listedAgent struct {
	name  string
	model string
}

// readAgents asks the session's own server which agents it would run a message
// as. listed is false when the runtime gave no usable answer — the endpoint is
// absent (an older runtime), it answered an error, or its body is not a list this
// driver can read — which is "this driver could not tell", never "there are none":
// the create then goes ahead exactly as it did before the check existed (§5.7).
// A server that does not answer at all is an error, because the create would fail
// on it anyway.
//
// The request carries cwd as `directory`, exactly as the session create does.
// Without it the runtime answers for its own working directory and leaves out the
// project agents under the session's — measured: a project agent defined in the
// session's directory was missing from an undirected listing and present in the
// directed one, and a refusal built on the first would refuse the one way a
// persona reaches an isolated session.
func (d *Driver) readAgents(ctx context.Context, srv *server, cwd fleet.AbsolutePath) (names []string, listed bool, err error) {
	agents, listed, err := d.readAgentEntries(ctx, srv, cwd)
	for _, a := range agents {
		names = append(names, a.name)
	}
	return names, listed, err
}

// readAgentEntries is readAgents with each agent's pinned model kept (muster
// #402); listed means what it means there.
func (d *Driver) readAgentEntries(ctx context.Context, srv *server, cwd fleet.AbsolutePath) (agents []listedAgent, listed bool, err error) {
	var wire []wireAgent
	path := "/agent?directory=" + url.QueryEscape(string(cwd))
	if err := d.do(ctx, srv, "GET", path, nil, &wire); err != nil {
		if isUnreachable(err) {
			return nil, false, err
		}
		return nil, false, nil
	}
	for _, a := range wire {
		if a.Name == "" {
			continue
		}
		entry := listedAgent{name: a.Name}
		if m := a.Model; m != nil && m.ProviderID != "" && m.ModelID != "" {
			entry.model = m.ProviderID + "/" + m.ModelID
		}
		agents = append(agents, entry)
	}
	// A runtime always has agents of its own, so an answer naming none is a body
	// this driver misread, not a positive "nothing exists".
	return agents, len(agents) > 0, nil
}

// checkAgent refuses a create naming an agent the runtime does not list. Called
// with the session's own server up and before any session exists on it. The list
// is read from the runtime on every create, never compiled in: agents change with
// the project and the operator's configuration.
//
// A listed agent that pins a model is judged against the provider policy too
// (muster #402): a turn run as that agent uses the agent's model, whatever the
// create-time checks read as the runtime's default, and only the turn-time check
// would have seen it, after the turn. It is judged whether or not the same
// create also pins a model, because the runtime does not say which of the two a
// turn would use and a refusal on the wrong one costs a re-create while a pass
// on it costs a turn on a model nobody allowed. The judgement reads the whole
// policy, the same one a pinned model is judged by, so a provider-only policy
// refuses an agent whose model sits on a provider it does not allow.
//
// agent may be empty: the session then runs as the runtime's default, and there
// is nothing to check.
func (d *Driver) checkAgent(ctx context.Context, srv *server, cwd fleet.AbsolutePath, agent string) error {
	if agent == "" {
		return nil
	}
	agents, listed, err := d.readAgentEntries(ctx, srv, cwd)
	if err != nil {
		return err
	}
	if !listed {
		return nil
	}
	names := make([]string, 0, len(agents))
	for _, have := range agents {
		if have.name == agent {
			return d.checkAgentModel(agent, have.model)
		}
		names = append(names, have.name)
	}
	msg := fmt.Sprintf("create: agent %q is not in the runtime's own agent list", agent)
	// The edit-distance helper belongs to the model catalog but works on any list
	// of short names.
	if near := modelCatalog(names).closest(agent, 3); len(near) > 0 {
		msg += "; closest: " + strings.Join(quoteAll(near), ", ")
	}
	return &fleet.Error{Kind: fleet.ErrorInvalid, Message: msg, Machine: d.machine}
}

// checkAgentModel refuses an agent whose pinned model the provider policy does
// not allow, naming the rule that refused. model is "" for an agent that pins
// none, which has nothing to judge here: the model it then runs on is the
// session's, judged where that is learned (catalog.go).
func (d *Driver) checkAgentModel(agent, model string) error {
	if model == "" {
		return nil
	}
	provider, id, _ := strings.Cut(model, "/")
	v, rule := d.providers.judge(provider, id)
	var why string
	switch v {
	case permitted:
		return nil
	case modelDenied:
		why = "is denied by " + rule + " on this machine"
	case modelNotAllowed:
		why = fmt.Sprintf("is on provider %q, where this machine allows only some models and none matches it", provider)
	default:
		why = fmt.Sprintf("is on provider %q, which this machine does not allow", provider)
	}
	return &fleet.Error{
		Kind: fleet.ErrorUnsupported,
		Message: fmt.Sprintf("create: agent %q pins model %q, which %s (allowed: %s); "+
			"use an agent that pins no model or one this machine allows, or change the operator's provider setting",
			agent, model, why, d.providers.describe()),
		Machine: d.machine,
	}
}

// turnEvidence is what the runtime's own message records say about the agent and
// the model a session's turns ran as. The two are read from the same message but
// stand apart: a record can name one and not the other, and each pin resolves, or
// stays unresolved, on its own half.
type turnEvidence struct {
	// ran is true when the newest assistant message in the session's history
	// names the agent it was handled by; agent is that name.
	ran   bool
	agent string

	// modelRan is true when that message names both the provider and the model it
	// ran on (muster #373); modelUsed is "provider/model", the same spelling a
	// create asks for, so the two compare directly.
	modelRan  bool
	modelUsed string
}

// turnEvidenceTail is how many of the newest messages are read. A turn is one
// user message and the assistant message(s) that answer it, so a handful is
// enough to reach the newest assistant message; the whole history is never read.
const turnEvidenceTail = 8

// observedTurn reads the agent and model the session's newest assistant message
// ran as.
//
// An assistant message exists only once the runtime has started a turn, and it
// names the agent and the model that turn resolved to — the runtime's own record,
// which is what a fallback to the default agent, or to the default model, shows
// up in. A user message is not used: it records what was asked for. Nothing
// found, an unreadable history, or an assistant message that names neither is "no
// evidence" (the zero value), never a claim in either direction. A model counts
// only when the record names both halves of it; one half alone is not a model.
func (d *Driver) observedTurn(ctx context.Context, srv *server, id string) turnEvidence {
	ev, _ := d.readTurnEvidence(ctx, srv, id)
	return ev
}

// readTurnEvidence is observedTurn that also says whether the read failed, for a
// caller that must not report a failed read as "no turn has run" (muster #437: a
// session read states why a pin is unresolved). On an error the evidence is the
// zero value, as observedTurn returns it.
func (d *Driver) readTurnEvidence(ctx context.Context, srv *server, id string) (turnEvidence, error) {
	var msgs []wireMessage
	path := fmt.Sprintf("/session/%s/message?limit=%d", url.PathEscape(id), turnEvidenceTail)
	if err := d.do(ctx, srv, "GET", path, nil, &msgs); err != nil {
		return turnEvidence{}, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		info := msgs[i].Info
		if info.Role != "assistant" {
			continue
		}
		var ev turnEvidence
		if info.Agent != "" {
			ev.ran, ev.agent = true, info.Agent
		}
		if info.ProviderID != "" && info.ModelID != "" {
			ev.modelRan, ev.modelUsed = true, info.ProviderID+"/"+info.ModelID
		}
		return ev, nil
	}
	return turnEvidence{}, nil
}

// turnEvidenceFor reads the evidence for a session that pinned an agent or a
// model, and reads nothing for one that pinned neither or whose runtime is not
// running.
func (d *Driver) turnEvidenceFor(ctx context.Context, known knownSession, id string, spec fleet.SessionSpec) turnEvidence {
	if (spec.Agent == "" && spec.Model == "") || known.srv == nil || known.srv.park.Load() != nil {
		return turnEvidence{}
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	return d.observedTurn(ctx, known.srv, id)
}

// Why a session READ (List, which GET on a session is answered from; muster #437)
// leaves a pin unresolved when it did not consult the runtime's message record. The
// create-time sentences above say no turn has run yet, which would be false here.
const (
	// pinUnreadListing is the reason on a listing of sessions: it asks the runtime
	// for no session's message record, so the constant cost #339 and #426 keep is
	// not undone. A read of one session by id does read it.
	pinUnreadListing = "this read is a listing of sessions, which asks the runtime for no session's message record; " +
		"read the session by id for the evidence"
	// pinUnreadStopped is the reason when the session's runtime is not there to ask:
	// stopped while idle, exited, listed unknown after a restart, or not answering.
	pinUnreadStopped = "the session's runtime is not answering (stopped, exited or unreachable), " +
		"so its message record cannot be read now"
	// pinUnreadFailed is the reason when the runtime was asked and the read failed.
	pinUnreadFailed = "the runtime's message record of the session could not be read just now"
)

// pinsOfRead is what a session read reports of the pins and of the model the newest
// turn ran on (muster #437).
//
// Only a read of ONE session by id consults the runtime's message record, with one
// bounded request, and only for a session that pinned an agent or a model — a
// session that pinned neither reports no pins and no model, as before. The
// request is the same read a repeat of a create makes (turnEvidenceFor), so the
// two resolve a pin identically. Any other read reports each pin as requested and
// unresolved, with the reason it was not read.
//
// The returned model is the "provider/model" the newest assistant message names,
// "" when it names none or nothing was read: Session.Model is an observed value,
// never an echo of the request.
//
// live says the session's runtime is up and its status read succeeded.
func (d *Driver) pinsOfRead(ctx context.Context, info knownSession, id string, byID, live bool) (*fleet.PinOutcome, string) {
	if info.specAgent == "" && info.specModel == "" {
		return nil, ""
	}
	unread := func(why string) (*fleet.PinOutcome, string) {
		return pinOutcomeFrom(info.specAgent, info.specModel, turnEvidence{}, why), ""
	}
	switch {
	case !byID:
		return unread(pinUnreadListing)
	case !live || info.srv == nil:
		return unread(pinUnreadStopped)
	}
	ev, err := d.readTurnEvidence(ctx, info.srv, id)
	if err != nil {
		return unread(pinUnreadFailed)
	}
	model := ""
	if ev.modelRan {
		model = ev.modelUsed
	}
	return pinOutcomeFrom(info.specAgent, info.specModel, ev, ""), model
}

// pinAgentUnresolvedEvidence is the standing reason an agent pin is not resolved:
// the create response is an echo, and no turn has run yet whose record shows
// which agent it ran as.
const pinAgentUnresolvedEvidence = "every message this driver sends names the agent, but the runtime's create response " +
	"only echoes the name it was given; no turn has run yet whose own record shows which agent it ran as"

// pinModelUnresolvedEvidence is the standing reason a model pin is not resolved:
// the create response does not name the model the runtime is using, and no turn
// has run yet whose record shows which one it ran on.
const pinModelUnresolvedEvidence = "the model is sent in the create body, but the runtime's create response does not name " +
	"which model it is using; no turn has run yet whose own record shows which model it ran on"
