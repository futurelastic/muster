// Package createpolicy holds the operator's per-machine policy over what a
// create may be given, decided by the machine's owner rather than by the caller
// (muster #509).
//
// Today it decides one thing: the `access` of a portable agent definition. A
// client sends `agentSpec.access` as what it would like; the operator, who knows
// what THIS host can enforce, may set or raise it per runtime and agent name —
// for instance "allow" on a host with no enforceable sandbox, where the only
// alternative is a session that cannot be made safe. The operator's value wins
// over the create's. A value that neither can honour is refused with the reason
// by the service; this package never weakens one and never decides whether a
// runtime can enforce a posture.
//
// # One surface, not two
//
// A per-machine sandbox default at create (muster #438) is the same kind of
// decision made by the same party, and it belongs in the same rule object under
// another key, selected by the same scope. This package is where that goes, so
// an operator learns one shape: a list of rules, each scoped like a sessionEnv
// entry, the first matching rule deciding.
package createpolicy

import (
	"fmt"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// Rule is one operator-declared decision.
type Rule struct {
	// AppliesTo scopes the rule; a scope naming no axis matches every create. The
	// Agents axis matches an agentSpec's name as well as a named agent.
	AppliesTo sessionenv.Scope
	// Access is the agent access sessions in scope run with. Required: a rule that
	// sets nothing is refused rather than left looking like it does something.
	Access fleet.AgentAccess
}

// Policy is an ordered list. The first rule whose scope matches a create decides:
// a rule is a decision, so the order the operator wrote it in is the operator's
// own statement of which one wins, and no other order exists to disagree with it.
type Policy []Rule

// Validate checks every rule's shape once, at startup, so a mistake is a message
// an operator reads once rather than a refusal every later create meets. A rule
// after one that matches every create it could can never apply, and is refused:
// the operator wrote something that does nothing, the same fault a misspelt
// runtime id is.
//
// knownRuntimes is the set of runtime ids this build can register; nil skips that
// check, for a caller that wants only the shape.
func Validate(p Policy, knownRuntimes []fleet.RuntimeId) error {
	for i, r := range p {
		at := fmt.Sprintf("createPolicy[%d]", i)
		switch r.Access {
		case fleet.AgentAccessAsk, fleet.AgentAccessAllow, fleet.AgentAccessSandboxed:
		case "":
			return fmt.Errorf("%s: access is required (want one of %q, %q, %q)", at,
				fleet.AgentAccessAsk, fleet.AgentAccessAllow, fleet.AgentAccessSandboxed)
		default:
			return fmt.Errorf("%s: access %q must be %q, %q or %q", at, string(r.Access),
				fleet.AgentAccessAsk, fleet.AgentAccessAllow, fleet.AgentAccessSandboxed)
		}
		for _, id := range r.AppliesTo.Runtimes {
			if id == "" {
				return fmt.Errorf("%s: appliesTo.runtimes has an empty runtime id", at)
			}
			if knownRuntimes != nil && !containsRuntime(knownRuntimes, id) {
				return fmt.Errorf("%s: appliesTo.runtimes names %q, which is not a runtime this service has", at, id)
			}
		}
		for _, a := range r.AppliesTo.Agents {
			if a == "" {
				return fmt.Errorf("%s: appliesTo.agents has an empty agent name", at)
			}
		}
		for j := 0; j < i; j++ {
			if covers(p[j].AppliesTo, r.AppliesTo) {
				return fmt.Errorf("%s can never apply: createPolicy[%d] already decides every create it would match", at, j)
			}
		}
	}
	return nil
}

// AccessFor returns the access the operator's policy names for a create on
// runtime with spec, and whether any rule did. Without a rule the caller applies
// the create's own value; this package does not invent a default.
func (p Policy) AccessFor(runtime fleet.RuntimeId, spec fleet.SessionSpec) (fleet.AgentAccess, bool) {
	for _, r := range p {
		if r.AppliesTo.Matches(runtime, spec) {
			return r.Access, true
		}
	}
	return "", false
}

// Describe is the policy in words, in the order it is tried, for a log line or a
// refusal to quote. Empty for no rules.
func (p Policy) Describe() string {
	parts := make([]string, 0, len(p))
	for _, r := range p {
		parts = append(parts, fmt.Sprintf("access %s for %s", r.Access, describeScope(r.AppliesTo)))
	}
	return strings.Join(parts, "; ")
}

func describeScope(s sessionenv.Scope) string {
	if len(s.Runtimes) == 0 && len(s.Agents) == 0 && len(s.Markers) == 0 {
		return "every create"
	}
	var parts []string
	if len(s.Runtimes) > 0 {
		parts = append(parts, "runtimes "+strings.Join(s.Runtimes, ", "))
	}
	if len(s.Agents) > 0 {
		parts = append(parts, "agents "+strings.Join(s.Agents, ", "))
	}
	if len(s.Markers) > 0 {
		parts = append(parts, "markers "+strings.Join(s.Markers, ", "))
	}
	return strings.Join(parts, " and ")
}

// covers reports whether every create that later can match is already matched by
// earlier, so a rule written after it is unreachable. Only a scope naming no agent
// and no marker covers anything: it matches every create on its runtimes (or on
// every runtime when it names none).
func covers(earlier, later sessionenv.Scope) bool {
	if len(earlier.Agents) > 0 || len(earlier.Markers) > 0 {
		return false
	}
	if len(earlier.Runtimes) == 0 {
		return true
	}
	if len(later.Runtimes) == 0 {
		return false
	}
	for _, id := range later.Runtimes {
		found := false
		for _, e := range earlier.Runtimes {
			if e == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsRuntime(known []fleet.RuntimeId, id string) bool {
	for _, k := range known {
		if string(k) == id {
			return true
		}
	}
	return false
}
