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
		{"nothing set", Policy{{}}, "sets nothing"},
		{"a sandbox with no mode", Policy{{Sandbox: &SandboxRule{}}}, "sandbox.mode is required"},
		{"an unknown sandbox mode", Policy{{Sandbox: &SandboxRule{Mode: "strict"}}}, `sandbox.mode "strict"`},
		{"a sandbox path that is not absolute", Policy{{Sandbox: &SandboxRule{Mode: SandboxDefault, Spec: fleet.SandboxSpec{ReadPaths: []fleet.AbsolutePath{"data"}}}}}, "absolute"},
		{"a shared temporary directory", Policy{{Sandbox: &SandboxRule{Mode: SandboxRequired, Spec: fleet.SandboxSpec{WritePaths: []fleet.AbsolutePath{"/tmp"}}}}}, "shared"},
		{"an unknown network", Policy{{Sandbox: &SandboxRule{Mode: SandboxRequired, Spec: fleet.SandboxSpec{Network: "filtered"}}}}, "sandbox.network"},
		{"a package cache in a policy", Policy{{Sandbox: &SandboxRule{Mode: SandboxDefault, Spec: fleet.SandboxSpec{
			PackageCache: &fleet.SandboxPackageCache{Path: "/srv/c"}}}}}, "not a policy grant"},
		{"a sandbox after one that decides every create's", Policy{
			{Sandbox: &SandboxRule{Mode: SandboxDefault}},
			{Sandbox: &SandboxRule{Mode: SandboxRequired}, AppliesTo: sessionenv.Scope{Agents: []string{"a"}}},
		}, "can never apply its sandbox"},
		{"an access key of a rule an earlier rule already decides", Policy{
			{Access: fleet.AgentAccessAsk},
			{Access: fleet.AgentAccessAllow, Sandbox: &SandboxRule{Mode: SandboxDefault}, AppliesTo: sessionenv.Scope{Agents: []string{"a"}}},
		}, "can never apply"},
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

func sb(mode SandboxMode, network fleet.SandboxNetwork) *SandboxRule {
	return &SandboxRule{Mode: mode, Spec: fleet.SandboxSpec{Network: network}}
}

// The keys of a rule are decided on their own: a rule that sets only one has no
// opinion on the other, so a later rule still reaches what it matched.
func TestSandboxFor_TheFirstRuleThatSetsItAndMatchesDecides(t *testing.T) {
	p := Policy{
		{AppliesTo: sessionenv.Scope{Agents: []string{"researcher"}}, Access: fleet.AgentAccessAllow},
		{AppliesTo: sessionenv.Scope{Agents: []string{"untrusted"}}, Sandbox: sb(SandboxRequired, fleet.SandboxNetworkClosed)},
		{AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode"}}, Sandbox: sb(SandboxDefault, "")},
		{Access: fleet.AgentAccessAsk, Sandbox: sb(SandboxRequired, "")},
	}
	if err := Validate(p, known); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cases := []struct {
		name    string
		runtime fleet.RuntimeId
		spec    fleet.SessionSpec
		want    SandboxMode
		network fleet.SandboxNetwork
	}{
		{"the access rule that matches says nothing of the sandbox", "tmux", specNamed("researcher"), SandboxRequired, ""},
		{"the agent a rule names", "opencode", fleet.SessionSpec{Agent: "untrusted"}, SandboxRequired, fleet.SandboxNetworkClosed},
		{"an agentSpec's name matches too", "tmux", specNamed("untrusted"), SandboxRequired, fleet.SandboxNetworkClosed},
		{"a runtime rule", "opencode", specNamed("other"), SandboxDefault, ""},
		{"the catch-all", "tmux", specNamed("other"), SandboxRequired, ""},
	}
	for _, tc := range cases {
		got, ok := p.SandboxFor(tc.runtime, tc.spec)
		if !ok || got.Mode != tc.want || got.Spec.Network != tc.network {
			t.Errorf("%s: SandboxFor = %+v, %v; want mode %q network %q", tc.name, got, ok, tc.want, tc.network)
		}
	}
	// And the access key is decided by the access rules alone.
	if got, ok := p.AccessFor("tmux", specNamed("researcher")); !ok || got != fleet.AgentAccessAllow {
		t.Errorf("AccessFor = %q, %v", got, ok)
	}
	if got, ok := p.AccessFor("tmux", specNamed("untrusted")); !ok || got != fleet.AgentAccessAsk {
		t.Errorf("a sandbox rule matched is not an access decision: %q, %v", got, ok)
	}
}

func TestSandboxFor_NoRuleIsNotADecision(t *testing.T) {
	if _, ok := (Policy)(nil).SandboxFor("opencode", specNamed("x")); ok {
		t.Fatal("an empty policy decided a sandbox")
	}
	p := Policy{{Access: fleet.AgentAccessAllow}}
	if _, ok := p.SandboxFor("opencode", specNamed("x")); ok {
		t.Fatal("an access-only rule decided a sandbox")
	}
}

func TestSandboxRulesFor_ListsWhatCanReachARuntimeInOrder(t *testing.T) {
	p := Policy{
		{AppliesTo: sessionenv.Scope{Runtimes: []string{"tmux"}}, Sandbox: sb(SandboxRequired, "")},
		{AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode"}, Agents: []string{"a"}}, Sandbox: sb(SandboxDefault, "")},
		{Access: fleet.AgentAccessAsk},
		{Sandbox: sb(SandboxDefault, fleet.SandboxNetworkClosed)},
	}
	got := p.SandboxRulesFor("opencode")
	if len(got) != 2 || got[0].Sandbox.Mode != SandboxDefault || got[0].AppliesTo.Agents[0] != "a" || got[1].Sandbox.Spec.Network != fleet.SandboxNetworkClosed {
		t.Fatalf("opencode: %+v, want its agent-scoped rule then the catch-all", got)
	}
	if got := p.SandboxRulesFor("tmux"); len(got) != 2 || got[0].Sandbox.Mode != SandboxRequired {
		t.Fatalf("tmux: %+v", got)
	}
	if got := p.SandboxRulesFor(""); len(got) != 1 {
		t.Fatalf("a runtime that names none is reached only by an unscoped rule: %+v", got)
	}
}

func TestValidate_ASandboxPolicyThatCanApplyPasses(t *testing.T) {
	p := Policy{
		{Sandbox: &SandboxRule{Mode: SandboxRequired, Spec: fleet.SandboxSpec{
			ReadPaths: []fleet.AbsolutePath{"/srv/data"}, WritePaths: []fleet.AbsolutePath{"/srv/work"}, Network: fleet.SandboxNetworkClosed}},
			AppliesTo: sessionenv.Scope{Agents: []string{"untrusted"}}},
		{Access: fleet.AgentAccessSandboxed, Sandbox: &SandboxRule{Mode: SandboxDefault}, AppliesTo: sessionenv.Scope{Runtimes: []string{"opencode"}}},
		{Sandbox: &SandboxRule{Mode: SandboxDefault}},
	}
	if err := Validate(p, known); err != nil {
		t.Fatalf("a reachable sandbox policy was refused: %v", err)
	}
	if got := p.Describe(); !strings.Contains(got, "sandbox required (network closed) for agents untrusted") ||
		!strings.Contains(got, "sandbox default (network open) for every create") {
		t.Errorf("Describe = %q", got)
	}
}
