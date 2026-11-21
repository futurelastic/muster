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
		{"no access", []any{map[string]any{}}, "access is required"},
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
