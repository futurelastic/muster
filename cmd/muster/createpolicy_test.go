package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #509: the operator's createPolicy — how the file is read and judged at
// start, and that an edit to it needs a restart.

func policyCfg(t *testing.T, rules []any) (*fileConfig, map[string]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.json")
	putCfg(t, path, startupDoc(map[string]any{"createPolicy": rules}))
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("the test's own config does not load: %v", err)
	}
	return cfg, map[string]string{"FLEET_CONFIG": path, "FLEET_RUNTIME": "none"}
}

func TestCreatePolicy_IsReadInFileOrderWithTheSessionEnvScopeShape(t *testing.T) {
	cfg, env := policyCfg(t, []any{
		map[string]any{"access": "allow", "appliesTo": map[string]any{"agents": []any{"researcher"}, "runtimes": []any{"opencode"}}},
		map[string]any{"access": "sandboxed"},
	})
	if err := validateConfigExtras(cfg, envOf(env)); err != nil {
		t.Fatalf("a valid policy was refused: %v", err)
	}
	p := cfg.createPolicy()
	if len(p) != 2 || p[0].Access != fleet.AgentAccessAllow || p[1].Access != fleet.AgentAccessSandboxed {
		t.Fatalf("policy = %+v, want the file's order kept", p)
	}
	if got := p[0].AppliesTo; len(got.Agents) != 1 || got.Agents[0] != "researcher" || len(got.Runtimes) != 1 || got.Runtimes[0] != "opencode" {
		t.Fatalf("scope = %+v", got)
	}
	spec := fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "researcher"}}
	if got, ok := p.AccessFor("opencode", spec); !ok || got != fleet.AgentAccessAllow {
		t.Fatalf("AccessFor = %q, %v", got, ok)
	}
	if (*fileConfig)(nil).createPolicy() != nil {
		t.Fatal("no config file should mean no policy")
	}
}

func TestCreatePolicy_AMistakeStopsTheStartAndShowsInTheDoctor(t *testing.T) {
	cases := []struct {
		name  string
		rules []any
		want  string
	}{
		{"nothing set", []any{map[string]any{}}, "sets nothing"},
		{"a sandbox with no mode", []any{map[string]any{"sandbox": map[string]any{}}}, "sandbox.mode is required"},
		{"an unknown sandbox mode", []any{map[string]any{"sandbox": map[string]any{"mode": "strict"}}}, `sandbox.mode "strict"`},
		{"a relative sandbox path", []any{map[string]any{"sandbox": map[string]any{"mode": "default", "readPaths": []any{"data"}}}}, "absolute"},
		{"unknown access", []any{map[string]any{"access": "yolo"}}, `access "yolo"`},
		{"misspelt runtime", []any{map[string]any{"access": "ask", "appliesTo": map[string]any{"runtimes": []any{"opencod"}}}}, `"opencod"`},
		{"a rule that can never apply", []any{
			map[string]any{"access": "ask"},
			map[string]any{"access": "allow", "appliesTo": map[string]any{"agents": []any{"a"}}},
		}, "can never apply"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, env := policyCfg(t, c.rules)
			startErr := validateConfigExtras(cfg, envOf(env))
			if startErr == nil || !strings.Contains(startErr.Error(), c.want) {
				t.Fatalf("start = %v, want a refusal saying %q", startErr, c.want)
			}
			row := rowByID(t, runChecks(context.Background(), testDoctorEnv(env)), "config.load")
			if row.Status != statusFail || !strings.Contains(row.Detail, c.want) {
				t.Fatalf("config.load = %+v, want the same verdict as the start", row)
			}
		})
	}
}

// The policy is handed to the service once, at start, so an edit to it must read as
// a change only a restart applies and not as one that took effect.
func TestReload_ACreatePolicyEditNeedsARestart(t *testing.T) {
	cfg, _ := policyCfg(t, []any{map[string]any{"access": "allow"}})
	got := restartOnly(&fileConfig{}, cfg)
	for _, name := range got {
		if name == "createPolicy" {
			return
		}
	}
	t.Fatalf("restartOnly = %v, want createPolicy named", got)
}

// muster #438: the sandbox key of a rule — how the file is read, and what the doctor
// says of it against the host.
func TestCreatePolicy_ASandboxRuleIsReadWithItsGrants(t *testing.T) {
	cfg, env := policyCfg(t, []any{
		map[string]any{"appliesTo": map[string]any{"agents": []any{"untrusted"}}, "sandbox": map[string]any{
			"mode": "required", "readPaths": []any{"/srv/data"}, "writePaths": []any{"/srv/work"}, "network": "closed"}},
		map[string]any{"access": "ask", "sandbox": map[string]any{"mode": "default"}},
	})
	if err := validateConfigExtras(cfg, envOf(env)); err != nil {
		t.Fatalf("a valid policy was refused: %v", err)
	}
	p := cfg.createPolicy()
	sb, ok := p.SandboxFor("claude-code-tmux", fleet.SessionSpec{Agent: "untrusted"})
	if !ok || sb.Mode != "required" || sb.Spec.Network != fleet.SandboxNetworkClosed ||
		len(sb.Spec.ReadPaths) != 1 || sb.Spec.ReadPaths[0] != "/srv/data" || len(sb.Spec.WritePaths) != 1 || sb.Spec.WritePaths[0] != "/srv/work" {
		t.Fatalf("SandboxFor = %+v, %v", sb, ok)
	}
	if sb, ok := p.SandboxFor("claude-code-tmux", fleet.SessionSpec{Agent: "other"}); !ok || sb.Mode != "default" {
		t.Fatalf("another agent reaches the catch-all: %+v, %v", sb, ok)
	}
}

func TestCreatePolicy_AnUnknownKeyInASandboxRuleStopsTheLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.json")
	putCfg(t, path, startupDoc(map[string]any{"createPolicy": []any{
		map[string]any{"sandbox": map[string]any{"mode": "default", "packageCache": map[string]any{"path": "/srv/c"}}},
	}}))
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "packageCache") {
		t.Fatalf("loadConfig = %v, want the unknown key named: a package cache is asked for per create, not granted by policy", err)
	}
}

func TestReload_ASandboxRuleEditNeedsARestart(t *testing.T) {
	cfg, _ := policyCfg(t, []any{map[string]any{"sandbox": map[string]any{"mode": "required"}}})
	for _, name := range restartOnly(&fileConfig{}, cfg) {
		if name == "createPolicy" {
			return
		}
	}
	t.Fatal("restartOnly does not name createPolicy for a sandbox rule: the policy is handed to the service once, at start")
}
