// Package createpolicy holds the operator's per-machine policy over what a
// create may be given, decided by the machine's owner rather than by the caller
// (muster #509).
//
// It decides two things, each under its own key of one rule:
//
//   - `access` (muster #509): the access of a portable agent definition. A client
//     sends `agentSpec.access` as what it would like; the operator, who knows what
//     THIS host can enforce, may set or raise it per runtime and agent name — for
//     instance "allow" on a host with no enforceable sandbox, where the only
//     alternative is a session that cannot be made safe. The operator's value wins
//     over the create's.
//   - `sandbox` (muster #438): the sandbox a create runs in. The operator may make
//     a profile the DEFAULT for a create that names none, or REQUIRE one — a
//     create that names its own is then held to it, and a runtime that cannot
//     enforce it refuses the create rather than start it weaker.
//
// A value that neither the operator nor the create can honour is refused with the
// reason by the service; this package never weakens one and never decides whether
// a runtime can enforce anything.
//
// # One surface, not two
//
// The two are the same kind of decision made by the same party, so they share a
// rule object and its scope: a list of rules, each scoped like a sessionEnv
// entry, the first matching rule deciding. The deciding is PER KEY: a rule that
// sets only `access` has no opinion on the sandbox, so a later rule that sets one
// still reaches the creates the first rule matched.
package createpolicy

import (
	"fmt"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// SandboxMode says how binding a rule's sandbox profile is.
type SandboxMode string

const (
	// SandboxDefault: a create that names no sandbox gets the profile. A create
	// that names its own keeps it, wider or narrower: a default is only what is
	// given to a caller who did not ask.
	SandboxDefault SandboxMode = "default"
	// SandboxRequired: every create in scope runs under the profile or a narrower
	// one. A create that names none gets it; one that names its own is accepted
	// only if it grants nothing the profile does not (fleet.SandboxSpec.Within);
	// a runtime that cannot enforce a sandbox refuses the create.
	SandboxRequired SandboxMode = "required"
)

// SandboxRule is the `sandbox` key of a rule: how binding the profile is, and the
// grants it carries. The profile's paths and network are what fleet.SandboxSpec
// names, with the same shape rules; a package cache is a per-create request and
// is not a policy grant.
type SandboxRule struct {
	Mode SandboxMode
	// Spec is the profile: the paths granted in addition to the working directory
	// and the session's own directory, and the network posture (open when empty).
	Spec fleet.SandboxSpec
}

// Rule is one operator-declared decision. It sets `Access`, `Sandbox` or both.
type Rule struct {
	// AppliesTo scopes the rule; a scope naming no axis matches every create. The
	// Agents axis matches an agentSpec's name as well as a named agent.
	AppliesTo sessionenv.Scope
	// Access is the agent access sessions in scope run with. Empty: the rule has no
	// opinion on it.
	Access fleet.AgentAccess
	// Sandbox is the sandbox policy of sessions in scope. Nil: the rule has no
	// opinion on it. A rule that sets neither key is refused rather than left looking
	// like it does something.
	Sandbox *SandboxRule
}

// Policy is an ordered list. For each key, the first rule that sets it and whose
// scope matches a create decides: a rule is a decision, so the order the operator
// wrote it in is the operator's own statement of which one wins, and no other
// order exists to disagree with it.
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
		if r.Access == "" && r.Sandbox == nil {
			return fmt.Errorf("%s sets nothing: name an access, a sandbox or both", at)
		}
		switch r.Access {
		case "", fleet.AgentAccessAsk, fleet.AgentAccessAllow, fleet.AgentAccessSandboxed:
		default:
			return fmt.Errorf("%s: access %q must be %q, %q or %q", at, string(r.Access),
				fleet.AgentAccessAsk, fleet.AgentAccessAllow, fleet.AgentAccessSandboxed)
		}
		if r.Sandbox != nil {
			if err := validateSandbox(at, *r.Sandbox); err != nil {
				return err
			}
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
		// Each key is decided on its own, so each is checked on its own: a rule may
		// be reachable for one key and dead for the other, and the dead half is the
		// same fault as a dead rule.
		for j := 0; j < i; j++ {
			if r.Access != "" && p[j].Access != "" && covers(p[j].AppliesTo, r.AppliesTo) {
				return fmt.Errorf("%s can never apply: createPolicy[%d] already decides the access of every create it would match", at, j)
			}
			if r.Sandbox != nil && p[j].Sandbox != nil && covers(p[j].AppliesTo, r.AppliesTo) {
				return fmt.Errorf("%s can never apply its sandbox: createPolicy[%d] already decides the sandbox of every create it would match", at, j)
			}
		}
	}
	return nil
}

// validateSandbox judges one rule's sandbox key: the mode is one of the two, and
// the profile is well-formed on every platform (fleet.SandboxSpec.Validate) and
// carries no package cache.
func validateSandbox(at string, sb SandboxRule) error {
	switch sb.Mode {
	case SandboxDefault, SandboxRequired:
	case "":
		return fmt.Errorf("%s: sandbox.mode is required (want %q or %q)", at, SandboxDefault, SandboxRequired)
	default:
		return fmt.Errorf("%s: sandbox.mode %q must be %q or %q", at, string(sb.Mode), SandboxDefault, SandboxRequired)
	}
	if sb.Spec.PackageCache != nil {
		return fmt.Errorf("%s: sandbox.packageCache is not a policy grant: a package cache is asked for per create, "+
			"and under a required policy it is accepted when its path lies inside a granted path", at)
	}
	if err := sb.Spec.Validate(); err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	return nil
}

// AccessFor returns the access the operator's policy names for a create on
// runtime with spec, and whether any rule did. Without a rule the caller applies
// the create's own value; this package does not invent a default.
func (p Policy) AccessFor(runtime fleet.RuntimeId, spec fleet.SessionSpec) (fleet.AgentAccess, bool) {
	for _, r := range p {
		if r.Access != "" && r.AppliesTo.Matches(runtime, spec) {
			return r.Access, true
		}
	}
	return "", false
}

// SandboxFor returns the sandbox rule the operator's policy names for a create on
// runtime with spec, and whether any rule did. Without one the create's own
// sandbox, or none, stands.
func (p Policy) SandboxFor(runtime fleet.RuntimeId, spec fleet.SessionSpec) (SandboxRule, bool) {
	for _, r := range p {
		if r.Sandbox != nil && r.AppliesTo.Matches(runtime, spec) {
			return *r.Sandbox, true
		}
	}
	return SandboxRule{}, false
}

// SandboxRulesFor lists, in the order they are tried, the sandbox rules that can
// reach a create on runtime: those whose scope names the runtime or names none. A
// rule scoped by agent or marker is in the list with its scope, because whether it
// applies is the create's, not the runtime's.
func (p Policy) SandboxRulesFor(runtime fleet.RuntimeId) []Rule {
	var out []Rule
	for _, r := range p {
		if r.Sandbox != nil && r.AppliesTo.ReachesRuntime(runtime) {
			out = append(out, r)
		}
	}
	return out
}

// Describe is the policy in words, in the order it is tried, for a log line or a
// refusal to quote. Empty for no rules.
func (p Policy) Describe() string {
	parts := make([]string, 0, len(p))
	for _, r := range p {
		if r.Access != "" {
			parts = append(parts, fmt.Sprintf("access %s for %s", r.Access, describeScope(r.AppliesTo)))
		}
		if r.Sandbox != nil {
			parts = append(parts, fmt.Sprintf("sandbox %s (network %s) for %s", r.Sandbox.Mode, r.Sandbox.Spec.EffectiveNetwork(), describeScope(r.AppliesTo)))
		}
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
