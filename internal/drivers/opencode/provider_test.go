package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #293 — the provider policy reaches the runtime through the session's
// OWN configuration, for every session, at create and again at relaunch.

func readSessionConfig(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, permissionConfigRel))
	if err != nil {
		t.Fatalf("the session's own config was not written: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	return cfg
}

func TestProviders_DefaultDisablesTheBuiltInProviderForEverySession(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k-default", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)

	cfg := readSessionConfig(t, known.srv.dir)
	if got, want := cfg["disabled_providers"], []any{"opencode"}; !reflect.DeepEqual(got, want) {
		t.Errorf("disabled_providers = %v, want %v", got, want)
	}
	if _, has := cfg["enabled_providers"]; has {
		t.Errorf("a default session carries an allow-list: %v", cfg)
	}
	if !strings.HasPrefix(inside(t, d, sess.ID).vars()["XDG_CONFIG_HOME"], known.srv.dir) {
		t.Error("the config is not the one the runtime reads (the session's own XDG_CONFIG_HOME)")
	}
}

func TestProviders_AllowListIsTheWholeSetAndNothingElse(t *testing.T) {
	d, _ := newIsolatedDriver(t, WithProviders([]string{"anthropic", " openai ", "anthropic", ""}))
	sess := mustCreate(t, d, "k-allow", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)

	cfg := readSessionConfig(t, known.srv.dir)
	if got, want := cfg["enabled_providers"], []any{"anthropic", "openai"}; !reflect.DeepEqual(got, want) {
		t.Errorf("enabled_providers = %v, want %v (trimmed, de-duplicated, in order)", got, want)
	}
	if _, has := cfg["disabled_providers"]; has {
		t.Errorf("both lists are written, and the runtime resolves that by precedence the operator did not choose: %v", cfg)
	}
}

func TestProviders_BypassKeepsItsPostureBesideThePolicy(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	sess, _ := bypassSession(t, d, "k-bypass-policy")
	known, _ := d.wasSeen(sess.ID)

	cfg := readSessionConfig(t, known.srv.dir)
	if _, has := cfg["permission"]; !has {
		t.Errorf("the bypass posture is gone: %v", cfg)
	}
	if _, has := cfg["disabled_providers"]; !has {
		t.Errorf("a bypass session lost the provider policy: %v", cfg)
	}
}

// A restart reads the operator's CURRENT setting, not what the session was
// created under: the file is rewritten at relaunch.
func TestProviders_RelaunchWritesTheCurrentSetting(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k-relaunch", fleet.SessionSpec{})
	known, _ := d1.wasSeen(sess.ID)
	cfgPath := filepath.Join(known.srv.dir, permissionConfigRel)
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state, WithProviders([]string{"anthropic"}))
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status == fleet.StatusUnknown {
		t.Fatalf("the session was not relaunched: %+v", got.State)
	}
	cfg := readSessionConfig(t, known.srv.dir)
	if want := []any{"anthropic"}; !reflect.DeepEqual(cfg["enabled_providers"], want) {
		t.Errorf("enabled_providers after relaunch = %v, want %v", cfg["enabled_providers"], want)
	}
}

// ---- a turn that ran on a provider outside the set ------------------------

func idleTurnState(t *testing.T, d *Driver, id string) fleet.SessionState {
	t.Helper()
	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: id})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return st
}

func TestState_TurnOnAProviderOutsideTheSetIsReportedNotIdleObserved(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	// The measured case: a clean turn, no error, on the hosted provider.
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{
		Role: "assistant", ProviderID: "opencode", ModelID: "big-pickle",
	}})

	st := idleTurnState(t, d, ref.ID)
	if st.LastTurn == nil || st.LastTurn.Outcome != "failed" {
		t.Fatalf("LastTurn = %+v, want a failed turn: the session ran on a provider nobody chose", st.LastTurn)
	}
	for _, want := range []string{`"opencode"`, `"big-pickle"`, "does not allow"} {
		if !strings.Contains(st.LastTurn.Reason, want) {
			t.Errorf("Reason %q does not say %q", st.LastTurn.Reason, want)
		}
	}
	if st.LastTurn.Retryable {
		t.Error("Retryable = true; running again on the same provider is not a fix")
	}
}

func TestState_TurnOnAnAllowedProviderIsUnremarkable(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{
		Role: "assistant", ProviderID: "anthropic", ModelID: "claude-sonnet-4-5",
	}})
	if st := idleTurnState(t, d, ref.ID); st.LastTurn != nil {
		t.Errorf("LastTurn = %+v, want none for a turn on an allowed provider", st.LastTurn)
	}
}

// A message that names no provider is not evidence either way.
func TestState_TurnNamingNoProviderIsNotClaimedOffPolicy(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{Role: "assistant"}})
	if st := idleTurnState(t, d, ref.ID); st.LastTurn != nil {
		t.Errorf("LastTurn = %+v, want none: nothing says where it ran", st.LastTurn)
	}
}

func TestState_ExplicitlyAllowedBuiltInProviderIsNotReported(t *testing.T) {
	f := newFakeServer(t)
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"opencode"}))
	if err != nil {
		t.Fatal(err)
	}
	ref := createOne(t, d, "/work/x", "key-1")
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "opencode", ModelID: "big-pickle"}})
	if st := idleTurnState(t, d, ref.ID); st.LastTurn != nil {
		t.Errorf("LastTurn = %+v, want none: the operator asked for this provider", st.LastTurn)
	}
}

// The runtime's own error on an off-policy turn is kept, not replaced.
func TestState_OffPolicyTurnThatAlsoErroredCarriesBothReasons(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{
		Role: "assistant", ProviderID: "opencode", ModelID: "big-pickle",
		Error: &wireAssistantError{Name: "MessageOutputLengthError"},
	}})
	st := idleTurnState(t, d, ref.ID)
	if st.LastTurn == nil || !strings.Contains(st.LastTurn.Reason, "does not allow") ||
		!strings.Contains(st.LastTurn.Reason, "MessageOutputLengthError") {
		t.Errorf("LastTurn = %+v, want both the policy reason and the runtime's", st.LastTurn)
	}
}

// ---- the provider-only policy, pinned as it is ------------------------------

// muster #401 lets a policy name single models. A policy that names providers
// only must behave exactly as it did before: the same words in every refusal,
// the same bytes in the session's own configuration. This table pins those
// words and bytes for the four shapes a provider-only policy takes — the
// default, one provider, two, and the built-in provider on purpose — so a change
// to the model-level code that moves one of them fails here, naming it.
//
// The expected strings are literal on purpose. Building them from the code under
// test would pin nothing.
func TestProviderOnlyPolicy_WordsAndBytesAreThoseOfBeforeModelRules(t *testing.T) {
	const (
		noUsableHead = `create: no model provider is usable: the runtime lists none that this machine allows (allowed: `
		noUsableTail = `. Give the session a credential for an allowed provider, or change the operator's provider setting; ` +
			`a session is never started on a provider nobody chose`
	)
	listed := modelCatalog{"anthropic/c1", "openai/g1", "anthropic/c2", "opencode/big-pickle"}

	cases := []struct {
		name string
		ids  []string

		describe string
		keys     string
		// pinBuiltIn is whether a create pinned to opencode/big-pickle is refused.
		pinBuiltIn bool
		// pinOther is whether a create pinned to zzz/model is refused.
		pinOther bool
		// turnProvider is a provider the policy refuses, for the turn-time reason.
		turnProvider string
	}{
		{
			name:         "default",
			ids:          nil,
			describe:     `any provider except the runtime's built-in hosted provider "opencode" (disabled by default)`,
			keys:         `{"disabled_providers":["opencode"]}`,
			pinBuiltIn:   true,
			pinOther:     false,
			turnProvider: "opencode",
		},
		{
			name:         "one provider",
			ids:          []string{"anthropic"},
			describe:     `only "anthropic"`,
			keys:         `{"enabled_providers":["anthropic"]}`,
			pinBuiltIn:   true,
			pinOther:     true,
			turnProvider: "zzz",
		},
		{
			name:         "two providers",
			ids:          []string{"anthropic", "openai"},
			describe:     `only "anthropic", "openai"`,
			keys:         `{"enabled_providers":["anthropic","openai"]}`,
			pinBuiltIn:   true,
			pinOther:     true,
			turnProvider: "zzz",
		},
		{
			name:         "the built-in provider on purpose",
			ids:          []string{"opencode"},
			describe:     `only "opencode"`,
			keys:         `{"enabled_providers":["opencode"]}`,
			pinBuiltIn:   false,
			pinOther:     true,
			turnProvider: "zzz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServer(t)
			d, err := newDriverWithOptions(t, f, WithProviders(tc.ids))
			if err != nil {
				t.Fatal(err)
			}
			if got := d.providers.describe(); got != tc.describe {
				t.Errorf("describe() = %q, want %q", got, tc.describe)
			}
			keys, err := json.Marshal(d.providers.configKeys())
			if err != nil {
				t.Fatal(err)
			}
			if string(keys) != tc.keys {
				t.Errorf("configKeys() = %s, want %s", keys, tc.keys)
			}

			refusal := func(model string) string {
				t.Helper()
				err := d.checkModelProvider(model)
				if err == nil {
					return ""
				}
				fe, ok := err.(*fleet.Error)
				if !ok || fe.Kind != fleet.ErrorUnsupported {
					t.Fatalf("checkModelProvider(%q) = %v, want an unsupported fleet error", model, err)
				}
				return fe.Message
			}
			if got, want := refusal("opencode/big-pickle"), `create: model "opencode/big-pickle" is on provider "opencode", `+
				`which this machine does not allow (allowed: `+tc.describe+`)`; tc.pinBuiltIn && got != want {
				t.Errorf("pin on the built-in provider:\n got %q\nwant %q", got, want)
			} else if !tc.pinBuiltIn && got != "" {
				t.Errorf("pin on the built-in provider = %q, want it to pass", got)
			}
			if got, want := refusal("zzz/model"), `create: model "zzz/model" is on provider "zzz", `+
				`which this machine does not allow (allowed: `+tc.describe+`)`; tc.pinOther && got != want {
				t.Errorf("pin on another provider:\n got %q\nwant %q", got, want)
			} else if !tc.pinOther && got != "" {
				t.Errorf("pin on another provider = %q, want it to pass", got)
			}

			noneNil := d.noUsableProvider(nil).(*fleet.Error)
			wantNil := noUsableHead + tc.describe + `); it lists no provider at all, so the session has no credential for any` + noUsableTail
			if noneNil.Kind != fleet.ErrorUnsupported || noneNil.Message != wantNil {
				t.Errorf("noUsableProvider(nil):\n got %q\nwant %q", noneNil.Message, wantNil)
			}
			noneListed := d.noUsableProvider(listed).(*fleet.Error)
			wantListed := noUsableHead + tc.describe + `); it lists only "anthropic", "openai", "opencode"` + noUsableTail
			if noneListed.Kind != fleet.ErrorUnsupported || noneListed.Message != wantListed {
				t.Errorf("noUsableProvider(listed):\n got %q\nwant %q", noneListed.Message, wantListed)
			}

			ref := createOne(t, d, "/work/x", "key-1")
			f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{
				Role: "assistant", ProviderID: tc.turnProvider, ModelID: "m1",
			}})
			st := idleTurnState(t, d, ref.ID)
			wantReason := `the turn ran on provider "` + tc.turnProvider + `" (model "m1"), which this machine does not allow (allowed: ` + tc.describe + `)`
			if st.LastTurn == nil || st.LastTurn.Outcome != "failed" || st.LastTurn.Retryable || st.LastTurn.Reason != wantReason {
				t.Errorf("turn-time LastTurn = %+v, want failed, not retryable, reason %q", st.LastTurn, wantReason)
			}
		})
	}
}

// ---- model rules (muster #401) -----------------------------------------------

func TestNewProviderPolicy_Grammar(t *testing.T) {
	valid := []string{
		"anthropic",
		"anthropic/claude-sonnet-4-5",
		"opencode/deepseek-v4.1-flash",
		"opencode/*",
		"opencode/deepseek-*",
		"p/a*b",
		"p/x/y", // the model part is everything after the first "/"
		"!opencode/*-free",
		"!p/x/y",
	}
	for _, e := range valid {
		if _, err := newProviderPolicy([]string{e}); err != nil {
			t.Errorf("%q: %v, want it accepted", e, err)
		}
	}

	invalid := map[string]string{
		"!p":      "a deny names models",
		"/m":      "names no provider",
		"p/":      `nothing follows the "/"`,
		"!!p/x":   "a provider id has no whitespace",
		"p*":      "a provider id has no whitespace",
		"p*/m":    "a provider id has no whitespace",
		"p/a?":    "a model pattern has no whitespace",
		"p/[a]":   "a model pattern has no whitespace",
		"p/a\\b":  "a model pattern has no whitespace",
		"a b":     "a provider id has no whitespace",
		"p/a b":   "a model pattern has no whitespace",
		"!/x":     "names no provider",
		"!p/":     `nothing follows the "/"`,
		"!p/a[b]": "a model pattern has no whitespace",
	}
	for e, why := range invalid {
		_, err := newProviderPolicy([]string{"anthropic", e})
		if err == nil {
			t.Errorf("%q was accepted", e)
			continue
		}
		if !strings.Contains(err.Error(), "provider policy entry "+fmt.Sprintf("%q", e)) || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %q, want the entry named and %q said", e, err, why)
		}
	}
}

// Blanks and exact repeats go; a model entry names its provider once in enabled.
func TestNewProviderPolicy_ShapeOfTheParsedPolicy(t *testing.T) {
	p, err := newProviderPolicy([]string{
		" deepseek ", "", "opencode/deepseek-v4-pro", "deepseek", "opencode/deepseek-v4.1-flash", "!opencode/*-free", "opencode/*",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"deepseek", "opencode"}; !reflect.DeepEqual(p.allow, want) {
		t.Errorf("allow = %v, want %v (opencode/* is the whole provider)", p.allow, want)
	}
	if len(p.models) != 2 || len(p.deny) != 1 {
		t.Errorf("models = %v, deny = %v, want two model rules and one deny", p.models, p.deny)
	}
	if want := []string{"deepseek", "opencode"}; !reflect.DeepEqual(p.enabled, want) {
		t.Errorf("enabled = %v, want %v: each provider once, in the order first named", p.enabled, want)
	}
	if want := []string{"deepseek", "opencode/deepseek-v4-pro", "opencode/deepseek-v4.1-flash", "opencode/*"}; !reflect.DeepEqual(p.allowed, want) {
		t.Errorf("allowed = %v, want %v", p.allowed, want)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"deepseek-v4-pro", "deepseek-v4-pro", true},
		{"deepseek-v4-pro", "deepseek-v4-pro2", false},
		{"deepseek-v4-pro", "Deepseek-v4-pro", false}, // case-sensitive, as the runtime is
		{"*", "", true},
		{"*", "anything/at/all", true},
		{"*-free", "big-free", true},
		{"*-free", "big-pickle", false}, // a suffix says nothing about a model without it
		{"*-free", "-free", true},
		{"big-*", "big-", true},
		{"a*b", "a/b", true}, // "*" runs over "/", unlike path.Match
		{"a*b", "ab", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbY", false},
		{"**", "x", true},
		{"x*", "", false},
		{"", "", true},
		{"", "x", false},
		{"deepseek-v4.1-*", "deepseek-v4.1-flash", true},
		{"deepseek-v4.1-*", "deepseek-v4x1-flash", false}, // "." is a dot, not a wildcard
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.s); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestJudge(t *testing.T) {
	mk := func(entries ...string) providerPolicy {
		t.Helper()
		p, err := newProviderPolicy(entries)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name     string
		policy   providerPolicy
		provider string
		model    string
		want     verdict
		rule     string
	}{
		{"default, ordinary provider", mk(), "deepseek", "m", permitted, ""},
		{"default, built-in provider", mk(), "opencode", "m", providerNotAllowed, ""},
		{"whole provider", mk("deepseek"), "deepseek", "anything", permitted, ""},
		{"a provider nobody named", mk("deepseek"), "other", "m", providerNotAllowed, ""},
		// The exact-model allowlist, the form to prefer.
		{"exact model, the one allowed", mk("opencode/deepseek-v4-pro"), "opencode", "deepseek-v4-pro", permitted, ""},
		{"exact model, another on the same provider", mk("opencode/deepseek-v4-pro"), "opencode", "big-pickle", modelNotAllowed, ""},
		{"exact model, its provider is not whole", mk("opencode/deepseek-v4-pro"), "deepseek", "m", providerNotAllowed, ""},
		{"two exact models", mk("opencode/a", "opencode/b"), "opencode", "b", permitted, ""},
		{"a glob allow", mk("opencode/deepseek-*"), "opencode", "deepseek-v4-pro", permitted, ""},
		{"a glob allow, no match", mk("opencode/deepseek-*"), "opencode", "grok-code", modelNotAllowed, ""},
		// Deny.
		{"deny beats a whole-provider allow", mk("opencode", "!opencode/*-free"), "opencode", "big-free", modelDenied, "!opencode/*-free"},
		{"deny beats it written first", mk("!opencode/*-free", "opencode"), "opencode", "big-free", modelDenied, "!opencode/*-free"},
		{"deny beats a model allow", mk("opencode/big-free", "!opencode/*-free"), "opencode", "big-free", modelDenied, "!opencode/*-free"},
		{"deny does not touch another model", mk("opencode", "!opencode/*-free"), "opencode", "paid-x", permitted, ""},
		{"deny is per provider", mk("deepseek", "opencode", "!opencode/*"), "deepseek", "x", permitted, ""},
		{"deny alone, an ordinary provider", mk("!deepseek/v3*"), "deepseek", "v3-old", modelDenied, "!deepseek/v3*"},
		{"deny alone leaves the built-in provider off", mk("!deepseek/v3*"), "opencode", "m", providerNotAllowed, ""},
		{"a model id with a slash of its own", mk("p/x/y"), "p", "x/y", permitted, ""},
	}
	for _, c := range cases {
		v, rule := c.policy.judge(c.provider, c.model)
		if v != c.want || rule != c.rule {
			t.Errorf("%s: judge(%q, %q) = (%v, %q), want (%v, %q)", c.name, c.provider, c.model, v, rule, c.want, c.rule)
		}
	}
	// permits is the provider-level answer: a provider allowed for some models is
	// one the runtime must have on, and a deny never turns it off.
	if p := mk("opencode/deepseek-v4-pro"); !p.permits("opencode") || p.permits("deepseek") {
		t.Error("permits() must be true for a provider named by a model rule, false for one nobody named")
	}
	if p := mk("opencode", "!opencode/*"); !p.permits("opencode") {
		t.Error("permits() must not be turned off by a deny")
	}
	// For a policy of providers alone, the model-level answer is the provider-level one.
	for _, entries := range [][]string{nil, {"deepseek"}, {"deepseek", "opencode"}} {
		p := mk(entries...)
		for _, id := range []string{"deepseek/a", "opencode/b", "other/c"} {
			if p.permitsModel(id) != p.permits(providerOfModel(id)) {
				t.Errorf("%v: permitsModel(%q) disagrees with permits(%q)", entries, id, providerOfModel(id))
			}
		}
	}
}

func TestConfigKeys_ModelRules(t *testing.T) {
	keys := func(entries ...string) string {
		t.Helper()
		p, err := newProviderPolicy(entries)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(p.configKeys())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// A provider allowed for one model is still a provider the runtime must enable.
	// The one model entry is the helper model (muster #402): a name, not a rule.
	if got, want := keys("deepseek", "opencode/deepseek-v4-pro"),
		`{"enabled_providers":["deepseek","opencode"],"small_model":"opencode/deepseek-v4-pro"}`; got != want {
		t.Errorf("exact-model allowlist: %s, want %s", got, want)
	}
	if got, want := keys("opencode/a", "opencode/b"),
		`{"enabled_providers":["opencode"],"small_model":"opencode/a"}`; got != want {
		t.Errorf("two models on one provider: %s, want %s", got, want)
	}
	// A deny alone is not an allow list: the default-off built-in provider stays.
	if got, want := keys("!deepseek/v3*"), `{"disabled_providers":["opencode"]}`; got != want {
		t.Errorf("deny alone: %s, want %s", got, want)
	}
	// Denies never reach the file; the provider-level list is the same without them.
	if keys("deepseek", "opencode", "!opencode/*-free") != keys("deepseek", "opencode") {
		t.Error("a deny changed the bytes written to the session's configuration")
	}
}

func TestDescribe_ModelRules(t *testing.T) {
	for _, c := range []struct {
		entries []string
		want    string
	}{
		{[]string{"deepseek", "opencode/deepseek-v4-pro"}, `only "deepseek", "opencode/deepseek-v4-pro"`},
		{[]string{"deepseek", "opencode", "!opencode/*-free"}, `only "deepseek", "opencode"; denied: "!opencode/*-free"`},
		{[]string{"!deepseek/v3*", "!deepseek/v2*"}, `any provider except the runtime's built-in hosted provider "opencode" (disabled by default); denied: "!deepseek/v3*", "!deepseek/v2*"`},
	} {
		p, err := newProviderPolicy(c.entries)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.describe(); got != c.want {
			t.Errorf("%v: describe() = %q, want %q", c.entries, got, c.want)
		}
	}
}

// ---- the turn-time check with model rules ------------------------------------

// createPinned creates a session pinned to model. A policy that judges single
// models refuses a create that pins none unless the runtime's default is known,
// and these tests are about the turn's record, not that.
func createPinned(t *testing.T, d *Driver, model string) fleet.SessionRef {
	t.Helper()
	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-"+model,
		fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: model})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return sess.SessionRef
}

func TestState_TurnOnAModelOutsideTheExactAllowlistIsFailed(t *testing.T) {
	f := newFakeServer(t)
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"deepseek", "opencode/deepseek-v4-pro"}))
	if err != nil {
		t.Fatal(err)
	}
	ref := createPinned(t, d, "opencode/deepseek-v4-pro")

	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "opencode", ModelID: "deepseek-v4-pro"}})
	if st := idleTurnState(t, d, ref.ID); st.LastTurn != nil {
		t.Errorf("a turn on the allowed model: LastTurn = %+v, want none", st.LastTurn)
	}

	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "opencode", ModelID: "grok-code"}})
	st := idleTurnState(t, d, ref.ID)
	if st.LastTurn == nil || st.LastTurn.Outcome != "failed" || st.LastTurn.Retryable {
		t.Fatalf("a turn on another model of that provider: LastTurn = %+v, want failed and not retryable", st.LastTurn)
	}
	for _, want := range []string{`provider "opencode" (model "grok-code")`, "no allowed model on it matches", `only "deepseek", "opencode/deepseek-v4-pro"`} {
		if !strings.Contains(st.LastTurn.Reason, want) {
			t.Errorf("Reason %q does not say %q", st.LastTurn.Reason, want)
		}
	}
}

func TestState_TurnOnADeniedModelNamesTheRule(t *testing.T) {
	f := newFakeServer(t)
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"deepseek", "opencode", "!opencode/*-free"}))
	if err != nil {
		t.Fatal(err)
	}
	ref := createPinned(t, d, "opencode/paid-x")
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{
		Role: "assistant", ProviderID: "opencode", ModelID: "big-free",
		Error: &wireAssistantError{Name: "MessageOutputLengthError"},
	}})
	st := idleTurnState(t, d, ref.ID)
	if st.LastTurn == nil || st.LastTurn.Outcome != "failed" {
		t.Fatalf("LastTurn = %+v, want failed", st.LastTurn)
	}
	for _, want := range []string{"denied by !opencode/*-free", "MessageOutputLengthError"} {
		if !strings.Contains(st.LastTurn.Reason, want) {
			t.Errorf("Reason %q does not say %q", st.LastTurn.Reason, want)
		}
	}
	// A record naming the provider and no model is judged on the provider alone.
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "opencode"}})
	if st := idleTurnState(t, d, ref.ID); st.LastTurn != nil {
		t.Errorf("a provider-only record on an allowed provider: LastTurn = %+v, want none", st.LastTurn)
	}
}

// The runtime's helper calls (muster #402) run on a small model the runtime picks
// itself, and `small_model` is the key that pins it. Under a policy that judges
// single models the file names an allowed one, from the policy alone; under any
// other it names none and the runtime picks as it always did.
func TestHelperModel(t *testing.T) {
	for _, c := range []struct {
		name    string
		entries []string
		want    string
	}{
		{"no entries: the default posture judges no model", nil, ""},
		{"providers alone: every model on an allowed provider is allowed", []string{"deepseek", "opencode"}, ""},
		{"the first exact entry, in the operator's order", []string{"deepseek", "opencode/b-cheap", "opencode/a-big"}, "opencode/b-cheap"},
		{"a glob cannot name one model without a catalog", []string{"opencode/deepseek-*"}, ""},
		{"a glob is skipped for the exact entry after it", []string{"opencode/deepseek-*", "opencode/deepseek-v4-pro"}, "opencode/deepseek-v4-pro"},
		{"an exact entry a deny matches is not allowed, so it is skipped", []string{"opencode/a", "opencode/b", "!opencode/a"}, "opencode/b"},
		{"every exact entry denied", []string{"opencode/a", "!opencode/a"}, ""},
		{"a deny alone names no model to use", []string{"!opencode/*-free"}, ""},
		{"a deny beside whole providers names no model to use", []string{"opencode", "!opencode/*-free"}, ""},
	} {
		p, err := newProviderPolicy(c.entries)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := p.helperModel(); got != c.want {
			t.Errorf("%s: helperModel() = %q, want %q", c.name, got, c.want)
		}
		_, has := p.configKeys()["small_model"]
		if has != (c.want != "") {
			t.Errorf("%s: small_model written = %v, want %v", c.name, has, c.want != "")
		}
		// Whatever it names is an allowed model: the pin can never be the way a
		// helper call reaches a model the policy refuses.
		if c.want != "" && !p.permitsModel(c.want) {
			t.Errorf("%s: the helper model %q is not allowed by the policy that chose it", c.name, c.want)
		}
	}
}

// The file a session is started with carries the helper model under a policy that
// judges single models, and carries none otherwise — at create and at relaunch,
// like the rest of the policy.
func TestProviders_TheSessionsConfigPinsTheHelperModel(t *testing.T) {
	d, _ := newIsolatedDriver(t, WithProviders([]string{"deepseek", "opencode/deepseek-v4.1-flash", "opencode/deepseek-v4-pro"}))
	// The create pins an allowed model: the stand-in runtime lists none, so an
	// unpinned create under a model-narrowing policy would be refused (muster #401).
	sess := mustCreate(t, d, "k-helper", fleet.SessionSpec{Model: "opencode/deepseek-v4-pro"})
	known, _ := d.wasSeen(sess.ID)
	cfg := readSessionConfig(t, known.srv.dir)
	if got, want := cfg["small_model"], "opencode/deepseek-v4.1-flash"; got != want {
		t.Errorf("small_model = %v, want %q (the first model the operator named exactly, not the one the create pinned)", got, want)
	}

	for _, c := range []struct {
		entries []string
		model   string
	}{
		{nil, "deepseek/flash"},
		{[]string{"anthropic"}, "anthropic/sonnet"},
		{[]string{"opencode", "!opencode/*-free"}, "opencode/paid-x"},
	} {
		d, _ := newIsolatedDriver(t, WithProviders(c.entries))
		sess := mustCreate(t, d, "k-nohelper", fleet.SessionSpec{Model: c.model})
		known, _ := d.wasSeen(sess.ID)
		if cfg := readSessionConfig(t, known.srv.dir); cfg["small_model"] != nil {
			t.Errorf("policy %v wrote small_model = %v; nothing names an allowed helper model there", c.entries, cfg["small_model"])
		}
	}
}
