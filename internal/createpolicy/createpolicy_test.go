package createpolicy

import (
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

var known = []fleet.RuntimeId{"tmux", "opencode", "stub"}

func specNamed(name string) fleet.SessionSpec {
	return fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: name}}
}

func TestAccessFor_TheFirstMatchingRuleDecides(t *testing.T) {
	p := Policy{
		{AppliesTo: sessionenv.Scope{Agents: []string{"researcher"}}, Access: fleet.AgentAccessAllow},
		{AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode"}}, Access: fleet.AgentAccessSandboxed},
	}
	if err := Validate(p, known); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cases := []struct {
		name    string
		runtime fleet.RuntimeId
		spec    fleet.SessionSpec
		want    fleet.AgentAccess
		wantOK  bool
	}{
		{"an agentSpec name matches the agents axis", "opencode", specNamed("researcher"), fleet.AgentAccessAllow, true},
		{"a named agent matches it too", "tmux", fleet.SessionSpec{Agent: "researcher"}, fleet.AgentAccessAllow, true},
		{"the first rule wins over a later one that also matches", "opencode", specNamed("researcher"), fleet.AgentAccessAllow, true},
		{"another agent on opencode reaches the runtime rule", "opencode", specNamed("other"), fleet.AgentAccessSandboxed, true},
		{"another agent on another runtime has no rule", "tmux", specNamed("other"), "", false},
	}
	for _, tc := range cases {
		got, ok := p.AccessFor(tc.runtime, tc.spec)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: AccessFor = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestAccessFor_NoRuleIsNotADecision(t *testing.T) {
	if got, ok := (Policy)(nil).AccessFor("opencode", specNamed("x")); ok || got != "" {
		t.Fatalf("an empty policy decided %q, %v; the create's own value stands", got, ok)
	}
}

func TestValidate_Refusals(t *testing.T) {
	cases := []struct {
		name string
		p    Policy
		want string
	}{
		{"no access", Policy{{}}, "access is required"},
		{"unknown access", Policy{{Access: "yolo"}}, `access "yolo"`},
		{"unknown runtime", Policy{{Access: fleet.AgentAccessAsk, AppliesTo: sessionenv.Scope{Runtimes: []string{"nope"}}}}, "not a runtime"},
		{"empty runtime id", Policy{{Access: fleet.AgentAccessAsk, AppliesTo: sessionenv.Scope{Runtimes: []string{""}}}}, "empty runtime id"},
		{"empty agent name", Policy{{Access: fleet.AgentAccessAsk, AppliesTo: sessionenv.Scope{Agents: []string{""}}}}, "empty agent name"},
		{"a rule after a catch-all", Policy{
			{Access: fleet.AgentAccessAsk},
			{Access: fleet.AgentAccessAllow, AppliesTo: sessionenv.Scope{Agents: []string{"a"}}},
		}, "can never apply"},
		{"a rule after a runtime catch-all it sits inside", Policy{
			{Access: fleet.AgentAccessAsk, AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode", "tmux"}}},
			{Access: fleet.AgentAccessAllow, AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode"}}},
		}, "can never apply"},
	}
	for _, tc := range cases {
		err := Validate(tc.p, known)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v; want an error naming %q", tc.name, err, tc.want)
		}
	}
}

func TestValidate_ReachableRulesPass(t *testing.T) {
	p := Policy{
		{Access: fleet.AgentAccessAllow, AppliesTo: sessionenv.Scope{Agents: []string{"a"}}},
		{Access: fleet.AgentAccessSandboxed, AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode"}}},
		{Access: fleet.AgentAccessAsk, AppliesTo: sessionenv.Scope{Runtimes: []string{"tmux"}}},
	}
	if err := Validate(p, known); err != nil {
		t.Fatalf("a reachable policy was refused: %v", err)
	}
	if err := Validate(nil, known); err != nil {
		t.Fatalf("no policy is the default and must pass: %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := Policy{
		{Access: fleet.AgentAccessAllow, AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode"}, Agents: []string{"r"}}},
		{Access: fleet.AgentAccessAsk},
	}
	got := p.Describe()
	for _, want := range []string{"access allow for runtimes opencode and agents r", "access ask for every create"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe %q lacks %q", got, want)
		}
	}
}
