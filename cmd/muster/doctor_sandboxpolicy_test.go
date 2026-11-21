package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/createpolicy"
	"github.com/futurelastic/muster/internal/drivers/opencode"
	"github.com/futurelastic/muster/internal/drivers/tmux"
	"github.com/futurelastic/muster/internal/sandbox"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #438: the sandbox.policy row judges the operator's sandbox rules against
// what this host's runtimes can enforce.

func hostThatOffers() *sandbox.HostReport {
	return &sandbox.HostReport{Platform: "linux", Mechanism: sandbox.MechanismLinux, Support: &fleet.SandboxSupport{Mechanism: sandbox.MechanismLinux}}
}

func hostThatRefuses() *sandbox.HostReport {
	return &sandbox.HostReport{Platform: "linux", Mechanism: sandbox.MechanismLinux, Reason: sandbox.MechanismLinux + ": Landlock ABI 5 is below the 6 this mechanism needs"}
}

func hostWithNoMechanism() *sandbox.HostReport {
	return &sandbox.HostReport{Platform: "plan9", Reason: "no sandbox mechanism on this platform"}
}

func sandboxRuleOf(mode createpolicy.SandboxMode, scope sessionenv.Scope) createpolicy.Rule {
	return createpolicy.Rule{AppliesTo: scope, Sandbox: &createpolicy.SandboxRule{Mode: mode, Spec: fleet.SandboxSpec{
		WritePaths: []fleet.AbsolutePath{"/srv/work"}, Network: fleet.SandboxNetworkClosed}}}
}

func TestCheckSandboxPolicy(t *testing.T) {
	both := []fleet.RuntimeId{tmux.DefaultRuntime, opencode.DefaultRuntime}
	neverMeasured := func() *sandbox.HostReport {
		t.Fatal("a machine with no sandbox rule measured the host")
		return nil
	}
	for _, tc := range []struct {
		name    string
		policy  createpolicy.Policy
		enabled []fleet.RuntimeId
		off     bool
		host    func() *sandbox.HostReport
		want    rowStatus
		summary []string
		detail  []string
	}{
		{
			name: "no policy passes without measuring the host", policy: nil, enabled: both, host: neverMeasured,
			want: statusPass, summary: []string{"no sandbox policy", "only when it asks"},
		},
		{
			name: "an access-only policy is no sandbox policy", enabled: both, host: neverMeasured,
			policy: createpolicy.Policy{{Access: fleet.AgentAccessAllow}},
			want:   statusPass, summary: []string{"no sandbox policy"},
		},
		{
			name: "a required sandbox the host offers passes and is described", enabled: both, host: hostThatOffers,
			policy: createpolicy.Policy{sandboxRuleOf(createpolicy.SandboxRequired, sessionenv.Scope{Agents: []string{"untrusted"}})},
			want:   statusPass, summary: []string{"1 sandbox rule", "can enforce"},
			detail: []string{"rule 1: required for agents untrusted", "network closed", "write /srv/work", "narrow, never widen", "restart"},
		},
		{
			name: "a required sandbox on a host that refuses fails and names the reason", enabled: both, host: hostThatRefuses,
			policy: createpolicy.Policy{sandboxRuleOf(createpolicy.SandboxRequired, sessionenv.Scope{})},
			want:   statusFail, summary: []string{"required sandbox cannot be enforced", "Landlock ABI 5"},
		},
		{
			name: "a required sandbox on a platform with no mechanism fails", enabled: both, host: hostWithNoMechanism,
			policy: createpolicy.Policy{sandboxRuleOf(createpolicy.SandboxRequired, sessionenv.Scope{})},
			want:   statusFail, summary: []string{"no sandbox mechanism"},
		},
		{
			name: "a default on a host that refuses only warns", enabled: both, host: hostThatRefuses,
			policy: createpolicy.Policy{sandboxRuleOf(createpolicy.SandboxDefault, sessionenv.Scope{})},
			want:   statusWarn, summary: []string{"default sandbox is not applied", "write `required`"},
		},
		{
			name: "the multiplexer runtime's opt-out fails a required rule there only", enabled: both, off: true, host: hostThatOffers,
			policy: createpolicy.Policy{sandboxRuleOf(createpolicy.SandboxRequired, sessionenv.Scope{})},
			want:   statusFail, summary: []string{string(tmux.DefaultRuntime), "FLEET_TMUX_SANDBOX=off"},
		},
		{
			name: "the same rule scoped to the other runtime is fine", enabled: both, off: true, host: hostThatOffers,
			policy: createpolicy.Policy{sandboxRuleOf(createpolicy.SandboxRequired, sessionenv.Scope{Runtimes: []string{string(opencode.DefaultRuntime)}})},
			want:   statusPass, summary: []string{"can enforce"},
		},
		{
			name: "a rule for a runtime this machine does not enable is not judged", enabled: []fleet.RuntimeId{tmux.DefaultRuntime}, host: hostThatRefuses,
			policy: createpolicy.Policy{sandboxRuleOf(createpolicy.SandboxRequired, sessionenv.Scope{Runtimes: []string{string(opencode.DefaultRuntime)}})},
			want:   statusPass, detail: []string{"reaches no runtime this machine enables"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := checkSandboxPolicy(tc.policy, tc.enabled, tc.off, tc.host)
			if row.ID != "sandbox.policy" || row.Status != tc.want {
				t.Fatalf("row = %+v, want status %s", row, tc.want)
			}
			for _, s := range tc.summary {
				if !strings.Contains(row.Summary, s) {
					t.Errorf("summary %q lacks %q", row.Summary, s)
				}
			}
			for _, s := range tc.detail {
				if !strings.Contains(row.Detail, s) {
					t.Errorf("detail %q lacks %q", row.Detail, s)
				}
			}
		})
	}
}

// The row is part of the doctor's report, decided from the file the machine loads.
func TestDoctor_ReportsTheSandboxPolicyRow(t *testing.T) {
	prev := doctorSandboxHost
	doctorSandboxHost = func() *sandbox.HostReport { return hostThatRefuses() }
	t.Cleanup(func() { doctorSandboxHost = prev })

	cfg := []any{map[string]any{"sandbox": map[string]any{"mode": "required", "network": "closed"}}}
	_, env := policyCfg(t, cfg)
	env["FLEET_RUNTIME"] = "tmux"
	row := rowByID(t, runChecks(context.Background(), testDoctorEnv(env)), "sandbox.policy")
	if row.Status != statusFail || !strings.Contains(row.Summary, "required sandbox cannot be enforced") {
		t.Fatalf("sandbox.policy = %+v, want a fail for a required sandbox the host refuses", row)
	}

	// A file that does not load makes the policy undecidable, not a pass.
	bad := filepath.Join(t.TempDir(), "fleet.json")
	putCfg(t, bad, startupDoc(map[string]any{"createPolicy": []any{map[string]any{"sandbox": map[string]any{"mode": "strict"}}}}))
	row = rowByID(t, runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_CONFIG": bad, "FLEET_RUNTIME": "none"})), "sandbox.policy")
	if row.Status != statusSkip || !strings.Contains(row.Summary, "config.load") {
		t.Fatalf("sandbox.policy with a refused file = %+v, want undecidable", row)
	}

	// No file at all: no policy, and no host measurement is needed for it.
	doctorSandboxHost = func() *sandbox.HostReport { return hostThatOffers() }
	row = rowByID(t, runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_RUNTIME": "none"})), "sandbox.policy")
	if row.Status != statusPass || !strings.Contains(row.Summary, "no sandbox policy") {
		t.Fatalf("sandbox.policy with no file = %+v", row)
	}
}
