package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// This file exercises the driver against a REAL opencode server this
// driver itself spawns, and — for the one test that goes further — a real
// AI provider that costs money per Boss's ruling on #55. It is opt-in
// (FLEET_OPENCODE_INTEGRATION=1), the same convention
// internal/drivers/tmux/integration_test.go uses for the incumbent driver,
// and for the same two reasons: it depends on what is installed on the
// machine it runs on, and CI has no opencode binary and no provider
// credential to spend.
//
// Every test here SKIPS CLEANLY absent that opt-in, which is the hard
// constraint the provider ruling states directly: "a test suite must not
// require a credential to run." Nothing in this package's non-integration
// tests (fake_server_test.go and everything built on it) needs a real
// binary or a network call at all.

func requireOpencodeIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("FLEET_OPENCODE_INTEGRATION") != "1" {
		t.Skip("set FLEET_OPENCODE_INTEGRATION=1 to run against a real opencode server")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("no opencode binary on PATH")
	}
}

// A real server, no provider needed: this verifies New actually spawns
// opencode, waits for readiness, and shuts it down cleanly — the whole
// process.go path that fake_server_test.go necessarily bypasses.
func TestIntegration_NewSpawnsAndShutsDownARealServer(t *testing.T) {
	requireOpencodeIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	d, err := New(ctx, "test-machine")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()

	caps := d.Capabilities()
	if !caps.ObservesState {
		t.Error("a live driver reports ObservesState: false")
	}

	col, err := d.List(ctx, fleet.RequestFrom(fleet.Caller{}), driver.ListFilter{})
	if err != nil {
		t.Fatalf("List against a freshly started, empty server: %v", err)
	}
	if len(col.Items()) != 0 {
		t.Errorf("a freshly started server already has %d sessions", len(col.Items()))
	}
}

// Create against the real server, with no provider call — a session can be
// created and read back without ever prompting it, so this does not need
// FLEET_OPENCODE_INTEGRATION's provider-credential half at all.
func TestIntegration_CreateAndCloseARealSession(t *testing.T) {
	requireOpencodeIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	d, err := New(ctx, "test-machine")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()

	dir := t.TempDir()
	ref, err := d.Create(ctx, fleet.RequestFrom(fleet.Caller{Principal: "integration-test"}), "integration-key-1",
		fleet.SessionSpec{Cwd: fleet.AbsolutePath(dir), Name: "muster #55 integration"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref.ID == "" {
		t.Fatal("Create returned an empty id")
	}

	st, err := d.State(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Status != fleet.StatusIdle {
		t.Errorf("a freshly created, never-prompted session reports %q, want idle", st.Status)
	}
	if st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("Confidence = %q, want observed", st.Confidence)
	}

	if _, err := d.Close(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID}); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// The live measurement #55 itself made: create, prompt, and watch the
// status endpoint report busy and then idle again. This is the one test
// in the package that spends real provider money — reserved, tagged, and
// skipped by default per the hard constraint. A provider must already be
// configured for opencode, and since a session's environment is built rather than
// inherited (muster #280) it reaches the session only through the variables
// FLEET_OPENCODE_SPEND_ENV names — this test does not configure one, on the same reasoning
// requireOpencodeIntegration does not try to detect it: enabling the
// env var is a human asserting the environment is ready.
func TestIntegration_LiveStatusTransition_SpendsProviderMoney(t *testing.T) {
	requireOpencodeIntegration(t)
	if os.Getenv("FLEET_OPENCODE_SPEND_MONEY") != "1" {
		t.Skip("set FLEET_OPENCODE_SPEND_MONEY=1 as well to run the one test that calls a real AI provider")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d, err := New(ctx, "test-machine")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()

	// A session's environment is built, not inherited (muster #280), so the
	// provider key it needs must be handed to it. FLEET_OPENCODE_SPEND_ENV
	// names the variables to forward from this test's own environment,
	// comma-separated (for example the provider's key variable).
	env := map[string]string{}
	for _, name := range strings.Split(os.Getenv("FLEET_OPENCODE_SPEND_ENV"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			env[name] = os.Getenv(name)
		}
	}

	dir := t.TempDir()
	ref, err := d.Create(ctx, fleet.RequestFrom(fleet.Caller{Principal: "integration-test"}), "integration-key-2",
		fleet.SessionSpec{Cwd: fleet.AbsolutePath(dir), Name: "muster #55 live", Env: env})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer d.Close(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})

	receipt, err := d.Send(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID},
		"reply with the single word: ack", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if receipt.Outcome != fleet.OutcomeQueued {
		t.Errorf("Outcome = %q, want queued", receipt.Outcome)
	}

	deadline := time.Now().Add(30 * time.Second)
	var sawWorking bool
	for time.Now().Before(deadline) {
		st, err := d.State(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if st.Status == fleet.StatusWorking {
			sawWorking = true
		}
		if sawWorking && st.Status == fleet.StatusIdle {
			return // observed the full busy -> idle transition #55 measured
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("did not observe a busy->idle transition within the deadline (sawWorking=%v)", sawWorking)
}

// The live measurement #77 itself made, reproduced with an invalid
// credential rather than an unfunded account: a real provider refuses the
// turn at the HTTP layer, before anything billable happens, so this needs
// only FLEET_OPENCODE_INTEGRATION — never FLEET_OPENCODE_SPEND_MONEY. A
// project-local opencode.json in the session's own cwd points a real
// provider (deepseek) at a deliberately bogus API key, which a real
// deepseek endpoint refuses with 401 — the same class of refusal the
// issue's own 402 was, landing on the same assistant-message shape.
func TestIntegration_RefusedTurnReportsFailedLastTurn(t *testing.T) {
	requireOpencodeIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d, err := New(ctx, "test-machine")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()

	dir := t.TempDir()
	const config = `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "deepseek": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "https://api.deepseek.com/v1", "apiKey": "sk-invalid-integration-test-key" },
      "models": { "deepseek-chat": { "name": "DeepSeek Chat" } }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("writing project config: %v", err)
	}

	ref, err := d.Create(ctx, fleet.RequestFrom(fleet.Caller{Principal: "integration-test"}), "integration-key-3",
		fleet.SessionSpec{Cwd: fleet.AbsolutePath(dir), Name: "muster #77 live", Model: "deepseek/deepseek-chat"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer d.Close(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})

	if _, err := d.Send(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID},
		"say hi", driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, err := d.State(ctx, fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if st.Status == fleet.StatusIdle && st.LastTurn != nil {
			if st.LastTurn.Outcome != "failed" {
				t.Errorf("Outcome = %q, want failed", st.LastTurn.Outcome)
			}
			if !strings.Contains(strings.ToLower(st.LastTurn.Reason), "api key") &&
				!strings.Contains(strings.ToLower(st.LastTurn.Reason), "auth") {
				t.Errorf("Reason = %q, expected it to name the real refusal", st.LastTurn.Reason)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("did not observe a failed LastTurn within the deadline — either the refusal never " +
		"happened (provider behaviour changed) or #77's fix regressed")
}
