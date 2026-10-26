package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// muster #293, the measured case: a real runtime, no provider credential (the
// session's environment is built, so none leaks in). By default the runtime's
// hosted provider is off and the create is refused with the reason; naming that
// provider is how it is asked for.
func TestIntegration_NoCredentialIsRefusedByDefaultAndAllowedOnPurpose(t *testing.T) {
	requireOpencodeIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req := fleet.RequestFrom(fleet.Caller{Principal: "test"})

	d, err := New(ctx, "test-machine", WithSessionRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()
	_, err = d.Create(ctx, req, "k-default", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || !strings.Contains(fe.Message, "no model provider is usable") {
		t.Fatalf("Create with no credential = %v, want the unsupported refusal naming that no provider is usable", err)
	}

	allowed, err := New(ctx, "test-machine", WithSessionRoot(t.TempDir()), WithProviders([]string{"opencode"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer allowed.Shutdown()
	sess, err := allowed.Create(ctx, req, "k-allowed", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if err != nil {
		t.Fatalf("Create with the built-in provider allowed on purpose: %v", err)
	}
	if _, err := allowed.Close(ctx, req, sess.SessionRef); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// muster #298: the rules a bypass session's config carries are the ones the
// real runtime resolves, in the order that makes the catch-all deny lose to the
// allows after it. No provider and no model turn: `debug agent` prints the
// permission ruleset the runtime built from the session's own configuration,
// including its own defaults ahead of it, and the evaluation below is the
// runtime's (last match wins), so a change in either the file's shape or the
// runtime's reading of it fails here.
func TestIntegration_ExternalDirectoryRulesAreWhatTheRuntimeResolves(t *testing.T) {
	requireOpencodeIntegration(t)
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("no opencode binary on PATH")
	}

	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	sess, cwd, readDir := resolve(t.TempDir()), resolve(t.TempDir()), resolve(t.TempDir())
	for _, sub := range []string{"home", "tmp", "xdg/config", "xdg/data", "xdg/state", "xdg/cache"} {
		if err := os.MkdirAll(filepath.Join(sess, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	plan := &sandboxPlan{read: dedupe([]string{"/usr", cwd, sess, readDir}), write: dedupe([]string{cwd, sess})}
	// `debug agent` refuses to start with no provider, and this test is about
	// permissions: name the runtime's own built-in one so there is a model to
	// resolve an agent against. No turn is ever run against it.
	providers, err := newProviderPolicy([]string{builtinProvider})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSessionConfig(sess, true, plan, providers); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--pure", "debug", "agent", "build")
	cmd.Dir = cwd
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(sess, "home"), "TMPDIR=" + filepath.Join(sess, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(sess, "xdg", "config"), "XDG_DATA_HOME=" + filepath.Join(sess, "xdg", "data"),
		"XDG_STATE_HOME=" + filepath.Join(sess, "xdg", "state"), "XDG_CACHE_HOME=" + filepath.Join(sess, "xdg", "cache"),
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("opencode debug agent build: %v\n%s", err, stderr.String())
	}
	start := strings.Index(string(out), "{")
	if start < 0 {
		t.Fatalf("no JSON in the runtime's output:\n%s", out)
	}
	var agent struct {
		Permission []struct {
			Permission string `json:"permission"`
			Pattern    string `json:"pattern"`
			Action     string `json:"action"`
		} `json:"permission"`
	}
	if err := json.NewDecoder(strings.NewReader(string(out)[start:])).Decode(&agent); err != nil {
		t.Fatalf("the runtime's agent output is not JSON: %v", err)
	}
	var resolved []externalDirectoryRule
	for _, r := range agent.Permission {
		if r.Permission == "external_directory" {
			resolved = append(resolved, externalDirectoryRule{pattern: r.Pattern, action: r.Action})
		}
	}
	if len(resolved) == 0 {
		t.Fatalf("the runtime resolved no external_directory rules from the session's config:\n%s", out)
	}

	for _, c := range []struct{ name, path, want string }{
		{"a file in the working directory", filepath.Join(cwd, "a.go"), "allow"},
		{"a readPaths entry", filepath.Join(readDir, "ref.txt"), "allow"},
		{"the session's own tmp", filepath.Join(sess, "tmp", "scratch.bak"), "allow"},
		{"a file outside every allowed path", "/etc/hosts", "deny"},
		{"somebody's home", "/Users/someone/.ssh/id_ed25519", "deny"},
	} {
		if got := evaluate(resolved, askFor(c.path, false)); got != c.want {
			t.Errorf("%s (%s): the runtime resolves %s, want %s", c.name, c.path, got, c.want)
		}
	}
}

// muster #371, measured on the real runtime: a context file named by contextRef
// reaches the MODEL as a system message, on the opening prompt and on a later
// Send, and never as something the user said. The model is a local stand-in that
// speaks the OpenAI-compatible wire the runtime is configured to use and records
// the request it was sent, so what is asserted is what the runtime really put on
// the wire, with no credential and no cost.
func TestIntegration_ContextRefReachesTheModelAsSystemContext(t *testing.T) {
	requireOpencodeIntegration(t)

	var mu sync.Mutex
	var requests []struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		// Only the session's own turns: the runtime may make other model calls (a
		// title, say) that are not the subject here.
		if json.Unmarshal(raw, &body) == nil && strings.Contains(string(raw), "USER-TEXT-") {
			mu.Lock()
			requests = append(requests, body)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`
		done := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		_, _ = io.WriteString(w, "data: "+chunk+"\n\ndata: "+done+"\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()

	dir := t.TempDir()
	config := `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "standin": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "` + model.URL + `/v1", "apiKey": "not-a-real-key" },
      "models": { "m": { "name": "M" } }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(t.TempDir(), "brief.md")
	const marker = "BRIEF-MARKER-end-every-reply-with-PINEAPPLE"
	if err := os.WriteFile(brief, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req := fleet.RequestFrom(fleet.Caller{Principal: "integration-test"})
	d, err := New(ctx, "test-machine", WithSessionRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()

	sess, err := d.Create(ctx, req, "k-ctx", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(dir), Name: "ctx", Model: "standin/m", Prompt: "USER-TEXT-ONE",
		ContextRef: fleet.AbsolutePath(brief),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer d.Close(ctx, req, sess.SessionRef)

	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := len(requests)
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("the model received fewer than %d requests", n)
	}
	waitFor(1)
	// Let the first turn finish before the next prompt is sent.
	time.Sleep(2 * time.Second)
	if _, err := d.Send(ctx, req, sess.SessionRef, "USER-TEXT-TWO", driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(2)

	mu.Lock()
	defer mu.Unlock()
	text := func(c any) string {
		b, _ := json.Marshal(c)
		return string(b)
	}
	for i, r := range requests[:2] {
		var system, user bool
		for _, m := range r.Messages {
			switch {
			case m.Role == "system" && strings.Contains(text(m.Content), marker):
				system = true
			case m.Role == "user" && strings.Contains(text(m.Content), marker):
				t.Errorf("request %d: the context arrived as a user message", i+1)
			case m.Role == "user":
				user = true
			}
		}
		if !system {
			t.Errorf("request %d: no system message carries the context file's text", i+1)
		}
		if !user {
			t.Errorf("request %d: no user message at all", i+1)
		}
	}
}

// What a REAL runtime records for a prompt that uses a tool, read back through
// Turns (muster #369). The model is a local stand-in that speaks the
// OpenAI-compatible wire: its first answer says something and calls the
// runtime's read tool, its second (the request that carries the tool's result)
// says something else. No credential and no cost, so it needs only the
// integration opt-in.
//
// What is asserted is what was measured against a live runtime: the one prompt
// is answered by TWO assistant messages, each is a turn, and the tool's output —
// a sentinel in the file the tool read — never reaches a page, whichever way the
// record is read.
func TestIntegration_TurnsReadsWhatARealRuntimeRecorded(t *testing.T) {
	requireOpencodeIntegration(t)

	// The real path: a tool reading a file the runtime resolves outside the
	// session's directory parks on a permission ask, and a symlinked temp
	// directory is exactly that.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "TOOL-OUTPUT-SENTINEL-must-not-be-returned"
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(raw, &body)
		// The request that carries a tool's result ends on it; a new prompt after
		// an earlier tool round does not, so each prompt gets its own tool round.
		sawTool := len(body.Messages) > 0 && body.Messages[len(body.Messages)-1].Role == "tool"
		chunk := func(delta, finish string) string {
			return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` +
				delta + `,"finish_reason":` + finish + `}]}` + "\n\n"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case len(body.Tools) == 0: // the runtime's own housekeeping call (a title), not the session's turn
			_, _ = io.WriteString(w, chunk(`{"role":"assistant","content":"a title"}`, "null")+chunk(`{}`, `"stop"`)+"data: [DONE]\n\n")
		case !sawTool:
			args, _ := json.Marshal(map[string]string{"filePath": note})
			call, _ := json.Marshal(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]string{"name": "read", "arguments": string(args)},
			}}})
			_, _ = io.WriteString(w, chunk(`{"role":"assistant","content":"FIRST-ROUND-TEXT"}`, "null")+
				chunk(string(call), "null")+chunk(`{}`, `"tool_calls"`)+"data: [DONE]\n\n")
		default:
			_, _ = io.WriteString(w, chunk(`{"role":"assistant","content":"SECOND-ROUND-TEXT"}`, "null")+
				chunk(`{}`, `"stop"`)+"data: [DONE]\n\n")
		}
	}))
	defer model.Close()

	config := `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "standin": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "` + model.URL + `/v1", "apiKey": "not-a-real-key" },
      "models": { "m": { "name": "M" } }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req := fleet.RequestFrom(fleet.Caller{Principal: "integration-test"})
	d, err := New(ctx, "test-machine", WithSessionRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()

	sess, err := d.Create(ctx, req, "k-turns", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(dir), Name: "turns", Model: "standin/m", Prompt: "read note.txt and tell me about it",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer d.Close(ctx, req, sess.SessionRef)

	waitForTurns := func(since string, want int) fleet.TurnsPage {
		t.Helper()
		var got fleet.TurnsPage
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			p, err := d.Turns(ctx, req, sess.SessionRef, fleet.TurnsQuery{Since: since})
			if err != nil {
				t.Fatalf("Turns: %v", err)
			}
			got.Turns = append(got.Turns, p.Turns...)
			got.Next = p.Next
			if len(got.Turns) >= want {
				return got
			}
			if since != "" {
				since = p.Next
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("only %d of %d turns arrived: %+v", len(got.Turns), want, got.Turns)
		return got
	}

	first := waitForTurns("", 2)
	if got, want := texts(first), []string{"FIRST-ROUND-TEXT", "SECOND-ROUND-TEXT"}; !eqStrings(got, want) {
		t.Fatalf("turns = %q, want %q (one per assistant message the runtime wrote, in order)", got, want)
	}
	for _, tr := range first.Turns {
		if strings.Contains(tr.Text, sentinel) {
			t.Fatalf("the tool's output reached a turn: %q", tr.Text)
		}
	}

	// Reading on from `next` returns exactly what is new, and the stable record
	// from the start returns the same two turns again.
	if _, err := d.Send(ctx, req, sess.SessionRef, "and once more please", driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	more := waitForTurns(first.Next, 2)
	if len(more.Turns) != 2 || more.Turns[0].Text != "FIRST-ROUND-TEXT" {
		t.Fatalf("after a second prompt, since=next gave %q; want the second prompt's two turns only", texts(more))
	}
	all := getTurns(t, d, sess.SessionRef, "", 0)
	if len(all.Turns) != 4 {
		t.Fatalf("the whole record = %q, want four turns", texts(all))
	}
}
