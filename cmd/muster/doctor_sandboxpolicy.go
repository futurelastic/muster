package main

import (
	"fmt"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/createpolicy"
	"github.com/futurelastic/muster/internal/drivers/opencode"
	"github.com/futurelastic/muster/internal/drivers/tmux"
	"github.com/futurelastic/muster/internal/sandbox"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// enabledRuntimes is the runtimes this machine's environment starts, as the doctor
// can tell without starting them: the primary one FLEET_RUNTIME names and the
// optional opencode one. "stub" is a test runtime and "none" starts nothing.
func enabledRuntimes(runtime string, opencodeOn bool) []fleet.RuntimeId {
	var out []fleet.RuntimeId
	switch runtime {
	case "tmux":
		out = append(out, tmux.DefaultRuntime)
	case "stub":
		out = append(out, "stub")
	}
	if opencodeOn {
		out = append(out, opencode.DefaultRuntime)
	}
	return out
}

// checkSandboxPolicy is the sandbox.policy row (muster #438): the operator's sandbox
// rules, and whether each can be enforced by the runtimes it reaches on THIS machine.
//
// The host report is the same one the sandbox.host row reads, so the two cannot
// disagree; the multiplexer runtime additionally offers none when FLEET_TMUX_SANDBOX=off,
// and the stub runtime never does. It answers only for runtimes this machine enables: one
// config file is commonly shared by machines that do not all run every runtime.
//
//   - `pass` with no sandbox rule: a create is confined only when it asks to be.
//   - `pass` when every runtime a rule reaches can enforce its profile.
//   - `fail` when a REQUIRED rule reaches a runtime that cannot: every create in its
//     scope on that runtime is refused `unsupported`, so the machine cannot start the
//     sessions it was told to.
//   - `warn` when only a DEFAULT rule does: the default is not applied there and a
//     create that names no sandbox runs unconfined, which is not what the rule says
//     and is not a refusal either. `required` is the spelling that refuses.
//
// host is called only when a sandbox rule exists, so a machine with none measures
// nothing; the Linux host check starts a child.
func checkSandboxPolicy(policy createpolicy.Policy, enabled []fleet.RuntimeId, multiplexerOff bool, host func() *sandbox.HostReport) doctorRow {
	row := doctorRow{ID: "sandbox.policy", Refs: []int{438}}
	var rules []createpolicy.Rule
	for _, r := range policy {
		if r.Sandbox != nil {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		row.Status = statusPass
		row.Summary = "no sandbox policy — a create is confined only when it asks for a sandbox"
		row.Detail = "a machine-wide default or requirement is a `sandbox` key on a `createPolicy` rule (docs/install.md); it is read at start"
		return row
	}

	rep := host()
	offered := func(rt fleet.RuntimeId) (bool, string) {
		switch {
		case rt == "stub":
			return false, "the stub runtime offers no sandbox"
		case rt == tmux.DefaultRuntime && multiplexerOff:
			return false, "FLEET_TMUX_SANDBOX=off stops it offering one"
		case rep.Support == nil && rep.Mechanism == "":
			return false, "this platform has no sandbox mechanism"
		case rep.Support == nil:
			return false, "this host is not offered the sandbox — " + rep.Reason
		}
		return true, ""
	}

	var failing, warning, unreached []string
	for i, r := range rules {
		var reach []fleet.RuntimeId
		for _, rt := range enabled {
			if r.AppliesTo.ReachesRuntime(rt) {
				reach = append(reach, rt)
			}
		}
		desc := fmt.Sprintf("rule %d (%s, %s)", i+1, r.Sandbox.Mode, describeRuleScope(r.AppliesTo))
		for _, rt := range reach {
			if good, why := offered(rt); !good {
				line := fmt.Sprintf("%s on %s: %s", desc, rt, why)
				if r.Sandbox.Mode == createpolicy.SandboxRequired {
					failing = append(failing, line)
				} else {
					warning = append(warning, line)
				}
			}
		}
		if len(reach) == 0 {
			unreached = append(unreached, desc+" reaches no runtime this machine enables")
		}
	}
	row.Detail = policyDetail(rules)
	switch {
	case len(failing) > 0:
		row.Status = statusFail
		row.Summary = "a required sandbox cannot be enforced — every create in its scope is refused: " + strings.Join(failing, "; ")
	case len(warning) > 0:
		row.Status = statusWarn
		row.Summary = "a default sandbox is not applied where it cannot be enforced (the create runs unconfined; write `required` to refuse instead): " +
			strings.Join(warning, "; ")
	default:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("%d sandbox rule(s); every enabled runtime they reach can enforce the profile", len(rules))
	}
	if len(unreached) > 0 {
		row.Detail += ". " + strings.Join(unreached, "; ")
	}
	return row
}

// policyDetail is every sandbox rule in words, in the order they are tried.
func policyDetail(rules []createpolicy.Rule) string {
	paths := func(ps []fleet.AbsolutePath) string {
		if len(ps) == 0 {
			return "none"
		}
		s := make([]string, len(ps))
		for j, p := range ps {
			s[j] = string(p)
		}
		return strings.Join(s, ", ")
	}
	parts := make([]string, 0, len(rules))
	for i, r := range rules {
		sp := r.Sandbox.Spec
		parts = append(parts, fmt.Sprintf("rule %d: %s for %s — network %s, read %s, write %s",
			i+1, r.Sandbox.Mode, describeRuleScope(r.AppliesTo), sp.EffectiveNetwork(), paths(sp.ReadPaths), paths(sp.WritePaths)))
	}
	return strings.Join(parts, "; ") + ". The first matching rule decides, in file order; `default` gives the profile to a create that names " +
		"no sandbox, `required` also holds a create's own sandbox to it (it may narrow, never widen); changing it needs a restart"
}

// describeRuleScope is a rule's scope in words, for a row.
func describeRuleScope(s sessionenv.Scope) string {
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
	if len(parts) == 0 {
		return "every create"
	}
	return strings.Join(parts, " and ")
}
