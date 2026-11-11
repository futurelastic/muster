package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/credrule"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #458: the operator's providerCredentials rule — how the file is read and
// judged at start, and what the doctor says about it and about the usage mark.

func credCfg(t *testing.T, rules []any) (*fileConfig, string, map[string]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.json")
	putCfg(t, path, startupDoc(map[string]any{"providerCredentials": rules}))
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("the test's own config does not load: %v", err)
	}
	return cfg, path, map[string]string{"FLEET_CONFIG": path, "FLEET_RUNTIME": "none"}
}

func TestProviderCredentials_AreReadInFileOrderWithTheSessionEnvScopeShape(t *testing.T) {
	cfg, _, env := credCfg(t, []any{
		map[string]any{"provider": "openai", "use": "api-key", "appliesTo": map[string]any{"markers": []any{"metered"}, "agents": []any{"build"}}},
		map[string]any{"provider": "openai", "use": "subscription"},
	})
	if err := validateConfigExtras(cfg, envOf(env)); err != nil {
		t.Fatalf("a valid rule list was refused: %v", err)
	}
	rules := cfg.credentialRules()
	if len(rules) != 2 || rules[0].Use != fleet.CredentialAPIKey || rules[1].Use != fleet.CredentialSubscription {
		t.Fatalf("rules = %+v, want the file's order kept", rules)
	}
	if got := rules[0].AppliesTo; len(got.Markers) != 1 || got.Markers[0] != "metered" || len(got.Agents) != 1 || got.Agents[0] != "build" {
		t.Fatalf("scope = %+v", got)
	}
	if kind, ok := rules.Choose("openai", "opencode", fleet.SessionSpec{Marker: "metered"}); !ok || kind != fleet.CredentialAPIKey {
		t.Fatalf("Choose = %q, %v", kind, ok)
	}
	if (*fileConfig)(nil).credentialRules() != nil {
		t.Fatal("no config file should mean no rules")
	}
}

func TestProviderCredentials_AMistakeStopsTheStartAndShowsInTheDoctor(t *testing.T) {
	cases := []struct {
		name  string
		rules []any
		want  string
	}{
		{"unknown kind", []any{map[string]any{"provider": "openai", "use": "oauth"}}, `use "oauth"`},
		{"unknown provider", []any{map[string]any{"provider": "deepseek", "use": "api-key"}}, `provider "deepseek"`},
		{"misspelt runtime", []any{map[string]any{"provider": "openai", "use": "api-key",
			"appliesTo": map[string]any{"runtimes": []any{"opencod"}}}}, `"opencod"`},
		{"a rule that can never apply", []any{
			map[string]any{"provider": "openai", "use": "subscription"},
			map[string]any{"provider": "openai", "use": "api-key", "appliesTo": map[string]any{"agents": []any{"a"}}},
		}, "can never apply"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, _, env := credCfg(t, c.rules)
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

func signedInState(t *testing.T, now time.Time, mut func(*grant.Grant)) string {
	t.Helper()
	state := t.TempDir()
	g := grant.Grant{Refresh: "R", Access: "A", ExpiresAtMs: now.Add(48 * time.Hour).UnixMilli(), SignedInAtMs: now.UnixMilli()}
	store := grant.NewStore(state)
	signed, err := store.Replace(g)
	if err != nil {
		t.Fatal(err)
	}
	// A sign-in clears a revocation and a usage mark, so they are set afterwards, the
	// way the service sets them.
	if mut != nil {
		if _, ok, err := store.Update(signed.Generation, mut); err != nil || !ok {
			t.Fatalf("Update: %v (applied %v)", err, ok)
		}
	}
	return state
}

func TestDoctorGrantRow_ReportsAProviderRefusingTheSignIn(t *testing.T) {
	now := time.Now()
	t.Run("a mark the running service keeps current is a warning", func(t *testing.T) {
		state := signedInState(t, now, func(g *grant.Grant) {
			g.Limit = &grant.Limit{SinceMs: now.Add(-20 * time.Minute).UnixMilli(), UpdatedMs: now.Add(-30 * time.Second).UnixMilli(),
				Message: "The usage limit has been reached"}
		})
		row := checkOpencodeGrant(true, state, "openai", now)
		if row.Status != statusWarn || !strings.Contains(row.Summary, "refusing requests") {
			t.Fatalf("%+v", row)
		}
		for _, want := range []string{"every session on the subscription", "recover on their own", "The usage limit has been reached"} {
			if !strings.Contains(row.Detail, want) {
				t.Errorf("detail %q does not say %q", row.Detail, want)
			}
		}
	})
	t.Run("a mark nobody has confirmed lately is said to be old, not current", func(t *testing.T) {
		state := signedInState(t, now, func(g *grant.Grant) {
			g.Limit = &grant.Limit{SinceMs: now.Add(-5 * time.Hour).UnixMilli(), UpdatedMs: now.Add(-4 * time.Hour).UnixMilli()}
		})
		row := checkOpencodeGrant(true, state, "openai", now)
		if row.Status != statusPass || !strings.Contains(row.Detail, "not current") {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("no mark, no change", func(t *testing.T) {
		if row := checkOpencodeGrant(true, signedInState(t, now, nil), "openai", now); row.Status != statusPass || row.Detail != "" {
			t.Fatalf("%+v", row)
		}
	})
}

func TestDoctorCredentialsRow(t *testing.T) {
	now := time.Now()
	key := []sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: "/x/key"}}
	sub := func() credrule.Rules {
		return credrule.Rules{{Provider: "openai", Use: fleet.CredentialSubscription}}
	}
	apik := func() credrule.Rules {
		return credrule.Rules{{Provider: "openai", Use: fleet.CredentialAPIKey}}
	}
	signedIn := signedInState(t, now, nil)
	cases := []struct {
		name      string
		enabled   bool
		state     string
		providers string
		rules     credrule.Rules
		env       []sessionenv.Entry
		status    rowStatus
		summary   string
	}{
		{"runtime not enabled", false, signedIn, "", nil, nil, statusSkip, "not enabled"},
		{"no rule, nothing to choose between", true, t.TempDir(), "", nil, nil, statusSkip, "no providerCredentials rule"},
		{"no rule but both credentials reach sessions", true, signedIn, "openai", nil, key, statusWarn, "no providerCredentials rule chooses"},
		{"no rule, a key only", true, t.TempDir(), "openai", nil, key, statusSkip, "no providerCredentials rule"},
		{"a subscription rule and a sign-in", true, signedIn, "openai", sub(), key, statusPass, "openai on subscription for every session"},
		{"a subscription rule and no sign-in", true, t.TempDir(), "openai", sub(), key, statusWarn, "no usable ChatGPT sign-in"},
		{"an api-key rule and a key entry", true, signedIn, "openai", apik(), key, statusPass, "openai on api-key"},
		{"an api-key rule and no key entry", true, signedIn, "openai", apik(), nil, statusWarn, "no sessionEnv entry supplies OPENAI_API_KEY"},
		{"a rule for a provider the policy does not allow", true, signedIn, "deepseek", apik(), key, statusWarn, "applies to no session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := checkOpencodeCredentials(c.enabled, c.state, c.providers, c.rules, c.env)
			if row.ID != "runtime.opencode.credentials" || row.Status != c.status || !strings.Contains(row.Summary, c.summary) {
				t.Fatalf("%+v, want status %s saying %q", row, c.status, c.summary)
			}
		})
	}
	t.Run("a revoked sign-in is not a sign-in the rule can use", func(t *testing.T) {
		revoked := signedInState(t, now, func(g *grant.Grant) { g.Revoked = &grant.Revoked{AtMs: now.UnixMilli(), Status: 401} })
		row := checkOpencodeCredentials(true, revoked, "openai", sub(), key)
		if row.Status != statusWarn {
			t.Fatalf("%+v", row)
		}
	})
}

// #458: the rule is handed to the driver once, at start, so an edit to it must read as
// a change only a restart applies and not as one that took effect.
func TestReload_ACredentialRuleEditNeedsARestart(t *testing.T) {
	running := &fileConfig{}
	next := &fileConfig{}
	if got := restartOnly(running, next); len(got) != 0 {
		t.Fatalf("restartOnly of two empty configs = %v", got)
	}
	cfg, _, _ := credCfg(t, []any{map[string]any{"provider": "openai", "use": "api-key"}})
	got := restartOnly(running, cfg)
	found := false
	for _, name := range got {
		if name == "providerCredentials" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restartOnly = %v, want providerCredentials named", got)
	}
}
