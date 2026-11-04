package opencode

import (
	"context"
	"encoding/json"
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
