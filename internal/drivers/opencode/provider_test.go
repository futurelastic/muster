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
