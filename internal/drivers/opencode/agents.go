package opencode

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// This file is everything the driver knows about the AGENT pin (muster #362).
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
//     that ran (agentEvidence), never from the create response.

// wireAgent is the subset of one GET /agent entry this driver reads. The
// runtime lists its built-in agents, the operator's configured ones and, when
// the request carries the session's directory, the project's own.
type wireAgent struct {
	Name string `json:"name"`
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
	var wire []wireAgent
	path := "/agent?directory=" + url.QueryEscape(string(cwd))
	if err := d.do(ctx, srv, "GET", path, nil, &wire); err != nil {
		if isUnreachable(err) {
			return nil, false, err
		}
		return nil, false, nil
	}
	for _, a := range wire {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	// A runtime always has agents of its own, so an answer naming none is a body
	// this driver misread, not a positive "nothing exists".
	return names, len(names) > 0, nil
}

// checkAgent refuses a create naming an agent the runtime does not list. Called
// with the session's own server up and before any session exists on it. The list
// is read from the runtime on every create, never compiled in: agents change with
// the project and the operator's configuration.
//
// agent may be empty: the session then runs as the runtime's default, and there
// is nothing to check.
func (d *Driver) checkAgent(ctx context.Context, srv *server, cwd fleet.AbsolutePath, agent string) error {
	if agent == "" {
		return nil
	}
	names, listed, err := d.readAgents(ctx, srv, cwd)
	if err != nil {
		return err
	}
	if !listed {
		return nil
	}
	for _, have := range names {
		if have == agent {
			return nil
		}
	}
	msg := fmt.Sprintf("create: agent %q is not in the runtime's own agent list", agent)
	// The edit-distance helper belongs to the model catalog but works on any list
	// of short names.
	if near := modelCatalog(names).closest(agent, 3); len(near) > 0 {
		msg += "; closest: " + strings.Join(quoteAll(near), ", ")
	}
	return &fleet.Error{Kind: fleet.ErrorInvalid, Message: msg, Machine: d.machine}
}

// agentEvidence is what the runtime's own message records say about the agent a
// session's turns ran as.
type agentEvidence struct {
	// ran is true when the newest assistant message in the session's history
	// names the agent it was handled by; agent is that name.
	ran   bool
	agent string
}

// agentEvidenceTail is how many of the newest messages are read. A turn is one
// user message and the assistant message(s) that answer it, so a handful is
// enough to reach the newest assistant message; the whole history is never read.
const agentEvidenceTail = 8

// observedAgent reads the agent the session's newest assistant message ran as.
//
// An assistant message exists only once the runtime has started a turn, and it
// names the agent that turn resolved to — the runtime's own record, which is
// what a fallback to the default agent shows up in. A user message is not used:
// it records what was asked for. Nothing found, an unreadable history, or an
// assistant message that names no agent is "no evidence" (the zero value), never
// a claim in either direction.
func (d *Driver) observedAgent(ctx context.Context, srv *server, id string) agentEvidence {
	var msgs []wireMessage
	path := fmt.Sprintf("/session/%s/message?limit=%d", url.PathEscape(id), agentEvidenceTail)
	if err := d.do(ctx, srv, "GET", path, nil, &msgs); err != nil {
		return agentEvidence{}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		info := msgs[i].Info
		if info.Role != "assistant" {
			continue
		}
		if info.Agent == "" {
			return agentEvidence{}
		}
		return agentEvidence{ran: true, agent: info.Agent}
	}
	return agentEvidence{}
}

// agentEvidenceFor reads the evidence for a session that asked for an agent, and
// reads nothing for one that did not or whose runtime is not running.
func (d *Driver) agentEvidenceFor(ctx context.Context, known knownSession, id, requested string) agentEvidence {
	if requested == "" || known.srv == nil {
		return agentEvidence{}
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	return d.observedAgent(ctx, known.srv, id)
}

// pinAgentUnresolvedEvidence is the standing reason an agent pin is not resolved:
// the create response is an echo, and no turn has run yet whose record shows
// which agent it ran as.
const pinAgentUnresolvedEvidence = "every message this driver sends names the agent, but the runtime's create response " +
	"only echoes the name it was given; no turn has run yet whose own record shows which agent it ran as"
