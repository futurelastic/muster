package service

import (
	"fmt"
	"slices"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/createpolicy"
)

// This file is the service's half of the operator's sandbox policy (muster #438):
// what a create that names no sandbox is given, what a create that names its own
// is held to, and what a runtime reports of it. The policy itself — the rules, how
// they are scoped and ordered — is internal/createpolicy's, read from the
// `createPolicy` of the machine's config; what a profile MEANS to a runtime is each
// driver's (docs/spec/session-abstraction.md §2.1).
//
// # Refuse, never degrade
//
// The rule the sandbox itself follows (sandbox.go in the root package) holds here
// too. A REQUIRED sandbox that the runtime cannot enforce refuses the create as
// `unsupported`; it is never started weaker, and never started unconfined with a
// note. A DEFAULT that the runtime cannot enforce is not applied: a default is what
// a caller who did not ask is given, and a caller who did not ask for a sandbox did
// not ask to be refused for lacking one. The row of `GET /v1/runtimes` and `muster
// doctor` say so, so the operator who wanted a refusal knows to write `required`.

// sandboxCannot says why the runtime declaring caps cannot enforce the profile, or
// returns "" when it can. Capabilities nobody has reached (Source assumed) say
// "unknown", never "incapable": the driver that owns the session makes that call,
// as it does for every other per-runtime field (fleet.Posture.Lower).
func sandboxCannot(caps fleet.DriverCapabilities, spec fleet.SandboxSpec) string {
	if caps.Source == fleet.CapabilitiesAssumed {
		return ""
	}
	if caps.Sandbox == nil {
		why := "it cannot confine a session (no operating-system sandbox"
		if caps.SandboxUnavailable != "" {
			why += ": " + caps.SandboxUnavailable
		}
		return why + ")"
	}
	if spec.EffectiveNetwork() == fleet.SandboxNetworkClosed && !slices.Contains(caps.Sandbox.Network, fleet.SandboxNetworkClosed) {
		return "it cannot close the network"
	}
	return ""
}

// applySandboxPolicy runs on a create THIS machine serves, after the posture is
// lowered (so a sandbox a posture asked for counts as the create's own) and before
// anything reads body.Sandbox. It gives a create that names no sandbox the matching
// rule's profile, and holds one that names its own to a required profile: narrower
// or equal is accepted, wider is refused `invalid`, and a runtime that cannot
// enforce a required profile refuses `unsupported`.
//
// A relayed create never comes here: the machine that runs the session applies ITS
// policy, so a policy on this machine cannot decide a session another machine runs.
func (s *Service) applySandboxPolicy(machine fleet.MachineId, runtime fleet.RuntimeId, caps fleet.DriverCapabilities, body *createSessionBody) *fleet.Error {
	match := fleet.SessionSpec{Agent: body.Agent, Marker: body.Marker, AgentSpec: body.AgentSpec}
	rule, ok := s.createPolicySnapshot().SandboxFor(runtime, match)
	if !ok {
		return nil
	}
	required := rule.Mode == createpolicy.SandboxRequired
	if why := sandboxCannot(caps, rule.Spec); why != "" {
		if !required {
			return nil
		}
		return &fleet.Error{
			Kind:   fleet.ErrorUnsupported,
			Reason: fleet.ReasonSandboxRequired,
			Message: fmt.Sprintf("create: this machine's sandbox policy requires a sandbox for this create, and runtime %q cannot enforce one: %s; "+
				"refusing rather than start the session unconfined (#438)", string(runtime), why),
			Machine: machine,
		}
	}

	if body.Sandbox == nil {
		sb := cloneSandbox(rule.Spec)
		body.Sandbox = &sb
		// A definition's knowledge home is granted to a confined session; this one
		// became confined just now, after applyAgentSpec looked.
		if k := agentKnowledge(body); k != nil {
			if ferr := grantKnowledgeHome(machine, body, k); ferr != nil {
				return ferr
			}
			if required {
				if err := body.Sandbox.Within(rule.Spec); err != nil {
					return sandboxWider(machine, "agentSpec.knowledge.home is granted to the session as a sandbox path, and "+err.Error())
				}
			}
		}
		return nil
	}
	if required {
		// A create that names no network has not asked for an open one: what it
		// leaves out is the machine's to say, so it runs on the required network. One
		// that names `open` against a closed requirement is the wider request.
		if body.Sandbox.Network == "" {
			sb := cloneSandbox(*body.Sandbox)
			sb.Network = rule.Spec.Network
			body.Sandbox = &sb
		}
		if err := body.Sandbox.Within(rule.Spec); err != nil {
			return sandboxWider(machine, err.Error())
		}
	}
	return nil
}

// agentKnowledge is the memory home an agent definition names, if it does.
func agentKnowledge(body *createSessionBody) *fleet.AgentKnowledge {
	if body.AgentSpec == nil {
		return nil
	}
	return body.AgentSpec.Knowledge
}

// sandboxWider is the refusal of a create whose sandbox grants more than the
// machine requires. It is a 400: the request is the thing to change, and the
// message names the grant that is wider and what the machine would accept.
func sandboxWider(machine fleet.MachineId, msg string) *fleet.Error {
	return &fleet.Error{
		Kind:    fleet.ErrorInvalid,
		Reason:  fleet.ReasonSandboxRequired,
		Message: msg + "; this machine requires a sandbox, which a create may narrow and never widen (#438)",
		Machine: machine,
	}
}

// cloneSandbox copies a profile so what the policy holds is never aliased into a
// create (the service appends to a create's path lists).
func cloneSandbox(sb fleet.SandboxSpec) fleet.SandboxSpec {
	sb.ReadPaths = slices.Clone(sb.ReadPaths)
	sb.WritePaths = slices.Clone(sb.WritePaths)
	if sb.PackageCache != nil {
		pc := *sb.PackageCache
		pc.Managers = slices.Clone(pc.Managers)
		sb.PackageCache = &pc
	}
	return sb
}

// sandboxPolicyOf is the sandbox rules that can reach a create on runtime, for the
// runtime's row of `GET /v1/runtimes` (muster #438): in the order they are tried,
// each with the scope that decides whether it applies to a given create and whether
// this runtime can enforce it right now. Nil when no rule reaches the runtime.
func (s *Service) sandboxPolicyOf(runtime fleet.RuntimeId, caps fleet.DriverCapabilities) []fleet.RuntimeSandboxPolicy {
	rules := s.createPolicySnapshot().SandboxRulesFor(runtime)
	if len(rules) == 0 {
		return nil
	}
	out := make([]fleet.RuntimeSandboxPolicy, 0, len(rules))
	for _, r := range rules {
		e := fleet.RuntimeSandboxPolicy{
			Mode:        string(r.Sandbox.Mode),
			ReadPaths:   append([]fleet.AbsolutePath{}, r.Sandbox.Spec.ReadPaths...),
			WritePaths:  append([]fleet.AbsolutePath{}, r.Sandbox.Spec.WritePaths...),
			Network:     r.Sandbox.Spec.EffectiveNetwork(),
			Enforceable: sandboxCannot(caps, r.Sandbox.Spec) == "",
		}
		if len(r.AppliesTo.Agents) > 0 || len(r.AppliesTo.Markers) > 0 {
			e.AppliesTo = &fleet.RuntimePolicyScope{Agents: slices.Clone(r.AppliesTo.Agents), Markers: slices.Clone(r.AppliesTo.Markers)}
		}
		out = append(out, e)
	}
	return out
}
