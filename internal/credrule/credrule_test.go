package credrule

import (
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

var known = []fleet.RuntimeId{"tmux", "opencode", "stub"}

func rule(use fleet.CredentialKind, scope sessionenv.Scope) Rule {
	return Rule{Provider: "openai", Use: use, AppliesTo: scope}
}

func TestChoose_TheFirstMatchingRuleDecides(t *testing.T) {
	rules := Rules{
		rule(fleet.CredentialAPIKey, sessionenv.Scope{Markers: []string{"lane-a"}}),
		rule(fleet.CredentialSubscription, sessionenv.Scope{}),
	}
	if err := Validate(rules, known); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cases := []struct {
		name   string
		spec   fleet.SessionSpec
		want   fleet.CredentialKind
		wantOK bool
	}{
		{"the scoped rule wins for its marker", fleet.SessionSpec{Marker: "lane-a"}, fleet.CredentialAPIKey, true},
		{"anything else falls to the catch-all", fleet.SessionSpec{Marker: "lane-b"}, fleet.CredentialSubscription, true},
		{"no marker at all too", fleet.SessionSpec{}, fleet.CredentialSubscription, true},
	}
	for _, tc := range cases {
		got, ok := rules.Choose("openai", "opencode", tc.spec)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: Choose = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestChoose_NoRuleIsNotADecision(t *testing.T) {
	rules := Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{Agents: []string{"build"}})}
	if got, ok := rules.Choose("openai", "opencode", fleet.SessionSpec{Agent: "plan"}); ok || got != "" {
		t.Fatalf("Choose = %q, %v; a session outside every scope has no rule and the caller applies its default", got, ok)
	}
	if got, ok := (Rules)(nil).Choose("openai", "opencode", fleet.SessionSpec{}); ok || got != "" {
		t.Fatalf("no rules chose %q, %v", got, ok)
	}
	if _, ok := rules.Choose("deepseek", "opencode", fleet.SessionSpec{Agent: "build"}); ok {
		t.Fatal("a rule for one provider decided another")
	}
}

func TestChoose_ARuntimeScopeNeverMatchesAnotherRuntime(t *testing.T) {
	rules := Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{Runtimes: []string{"opencode"}})}
	if _, ok := rules.Choose("openai", "tmux", fleet.SessionSpec{}); ok {
		t.Fatal("a rule scoped to opencode decided a tmux session")
	}
	if got, ok := rules.Choose("openai", "opencode", fleet.SessionSpec{}); !ok || got != fleet.CredentialAPIKey {
		t.Fatalf("Choose = %q, %v on its own runtime", got, ok)
	}
}

func TestValidate_RefusesWhatCannotBeARule(t *testing.T) {
	cases := []struct {
		name  string
		rules Rules
		want  string
	}{
		{"unknown provider", Rules{{Provider: "deepseek", Use: fleet.CredentialAPIKey}}, `provider "deepseek"`},
		{"unknown kind", Rules{{Provider: "openai", Use: "oauth"}}, `use "oauth"`},
		{"empty kind", Rules{{Provider: "openai"}}, "use"},
		{"empty runtime id", Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{Runtimes: []string{""}})}, "empty runtime id"},
		{"unknown runtime id", Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{Runtimes: []string{"opencod"}})}, `"opencod"`},
		{"a rule after a catch-all can never apply", Rules{
			rule(fleet.CredentialSubscription, sessionenv.Scope{}),
			rule(fleet.CredentialAPIKey, sessionenv.Scope{Markers: []string{"lane-a"}}),
		}, "providerCredentials[1] can never apply"},
		{"a rule after a runtime-wide rule on that runtime", Rules{
			rule(fleet.CredentialSubscription, sessionenv.Scope{Runtimes: []string{"opencode"}}),
			rule(fleet.CredentialAPIKey, sessionenv.Scope{Runtimes: []string{"opencode"}, Agents: []string{"a"}}),
		}, "can never apply"},
	}
	for _, tc := range cases {
		err := Validate(tc.rules, known)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

func TestValidate_AcceptsScopedRulesInAnyOrderBeforeAFinalCatchAll(t *testing.T) {
	ok := []Rules{
		nil,
		{rule(fleet.CredentialSubscription, sessionenv.Scope{})},
		{
			rule(fleet.CredentialAPIKey, sessionenv.Scope{Agents: []string{"a"}}),
			rule(fleet.CredentialSubscription, sessionenv.Scope{Markers: []string{"m"}}),
			rule(fleet.CredentialAPIKey, sessionenv.Scope{}),
		},
		{
			// A runtime-wide rule on opencode does not shadow a rule for tmux.
			rule(fleet.CredentialSubscription, sessionenv.Scope{Runtimes: []string{"opencode"}}),
			rule(fleet.CredentialAPIKey, sessionenv.Scope{Runtimes: []string{"tmux"}}),
		},
	}
	for i, rules := range ok {
		if err := Validate(rules, known); err != nil {
			t.Errorf("case %d: Validate = %v", i, err)
		}
	}
	// A nil runtime set skips the runtime-id check and judges the shape only.
	if err := Validate(Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{Runtimes: []string{"anything"}})}, nil); err != nil {
		t.Errorf("nil known set: %v", err)
	}
}

func TestDescribe_SaysWhatEachRuleDoesInTheOrderItIsTried(t *testing.T) {
	rules := Rules{
		rule(fleet.CredentialAPIKey, sessionenv.Scope{Agents: []string{"build"}, Markers: []string{"lane-a"}}),
		rule(fleet.CredentialSubscription, sessionenv.Scope{}),
	}
	got := rules.Describe()
	want := "openai on api-key for agents build and markers lane-a; openai on subscription for every session"
	if got != want {
		t.Fatalf("Describe = %q, want %q", got, want)
	}
	if (Rules)(nil).Describe() != "" {
		t.Fatal("no rules should describe as empty")
	}
}
