package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// secretOnACommandLine stands for a credential a client passed inline.
const secretOnACommandLine = "SECRET-TOKEN-0123456789abcdef"

// scriptedProvider is an OpenAI-compatible endpoint that answers the first
// request carrying tools with one bash tool call, and everything else with
// text. It is what lets a real runtime evaluate a bash permission without a
// model, a credential or a network.
func scriptedProvider(t *testing.T, command string) *httptest.Server {
	t.Helper()
	sse := func(w http.ResponseWriter, chunks ...any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Tools    []any `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		toolResult := false
		for _, m := range body.Messages {
			toolResult = toolResult || m.Role == "tool"
		}
		if len(body.Tools) > 0 && !toolResult {
			args, _ := json.Marshal(map[string]string{"command": command, "description": "probe"})
			call := map[string]any{"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": string(args)}}
			sse(w, chunk(map[string]any{"role": "assistant", "tool_calls": []any{call}}, nil), chunk(map[string]any{}, "tool_calls"))
			return
		}
		sse(w, chunk(map[string]any{"role": "assistant", "content": "done"}, nil), chunk(map[string]any{}, "stop"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// muster #420, the "done when": a session that runs a command containing an auth
// header leaves no header value in its opencode log. This starts the REAL
// runtime exactly as the driver does (startServer, a built environment, the
// private log), points it at a scripted provider that asks for such a command,
// and reads the log file back.
//
// It would fail with runtimeLogLevel = "INFO": the log then carries a line
// `evaluated permission=bash pattern="curl -H 'Authorization: Bearer …'"`.
func TestIntegration_ARuntimeLogHoldsNoCommandText(t *testing.T) {
	requireOpencodeIntegration(t)
	if testing.Short() {
		t.Skip("starts a real runtime and a scripted provider")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("no opencode binary on PATH")
	}
	command := "curl -s -H 'Authorization: Bearer " + secretOnACommandLine + "' http://127.0.0.1:1/x"
	provider := scriptedProvider(t, command)

	dir, err := sessionDirs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"provider":{"fake":{"npm":"@ai-sdk/openai-compatible","name":"fake",`+
		`"options":{"baseURL":%q,"apiKey":"x"},"models":{"m":{"name":"m","tool_call":true,`+
		`"limit":{"context":8000,"output":1000}}}}},"permission":{"bash":"allow"}}`, provider.URL+"/v1")
	cfgDir := filepath.Join(scopedEnv(dir)["XDG_CONFIG_HOME"], "opencode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "opencode.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privateRuntimeLog(dir); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + strings.Join(systemPath(), string(os.PathListSeparator)), "OPENCODE_DISABLE_CLAUDE_CODE=1"}
	for k, v := range scopedEnv(dir) {
		env = append(env, k+"="+v)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	srv, err := startServer(ctx, bin, t.TempDir(), "muster", env, nil)
	if err != nil {
		t.Fatalf("startServer: %v", err)
	}
	defer srv.teardown()

	post := func(path, body string) []byte {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.baseURL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(srv.username, srv.password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", path, resp.StatusCode, out)
		}
		return out
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(post("/session", "{}"), &created); err != nil || created.ID == "" {
		t.Fatalf("creating a session: %v", err)
	}
	post("/session/"+created.ID+"/message",
		`{"model":{"providerID":"fake","modelID":"m"},"parts":[{"type":"text","text":"run it"}]}`)

	// The control: the command really went through the runtime's bash tool (the
	// session's own message list holds the call with its input), so a log that
	// does not hold it is a log that left it out, not one that never saw it.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.baseURL+"/session/"+created.ID+"/message", nil)
	req.SetBasicAuth(srv.username, srv.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(messages, []byte(`"tool":"bash"`)) || !bytes.Contains(messages, []byte(secretOnACommandLine)) {
		t.Fatalf("the runtime never ran the scripted bash command; messages = %.1500s", messages)
	}

	// The runtime writes its log behind the request that caused the line: read at
	// once, the file stops short of the bash call (measured: it ended three steps
	// before it), and a negative check on that proves nothing. There is no line to
	// wait for at the capped level, so this waits out a settle time that the
	// mutation check (runtimeLogLevel = "INFO" must fail this test) showed enough.
	time.Sleep(2 * time.Second)
	srv.teardown()

	logDir := runtimeLogDir(dir)
	logs, _ := filepath.Glob(filepath.Join(logDir, "*.log"))
	if len(logs) == 0 {
		t.Fatalf("the runtime wrote no log file under %s", logDir)
	}
	for _, p := range logs {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(got, []byte(secretOnACommandLine)) {
			t.Errorf("%s holds the secret from a bash command line:\n%s", filepath.Base(p), got)
		}
		wantMode(t, p, 0o600)
	}
	wantMode(t, logDir, 0o700)
	if name := srv.logs.errorName(time.Time{}); name != "" {
		t.Errorf("the capture read error name %q from a run that logged none", name)
	}
}

// The cap must keep what #288 reads. A model the runtime does not know is
// logged by name at ERROR; with the log capped the driver still sees it.
func TestIntegration_ACappedRuntimeLogStillNamesAnError(t *testing.T) {
	requireOpencodeIntegration(t)
	if testing.Short() {
		t.Skip("starts a real runtime")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("no opencode binary on PATH")
	}
	dir, err := sessionDirs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := privateRuntimeLog(dir); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + strings.Join(systemPath(), string(os.PathListSeparator)), "OPENCODE_DISABLE_CLAUDE_CODE=1"}
	for k, v := range scopedEnv(dir) {
		env = append(env, k+"="+v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	srv, err := startServer(ctx, bin, t.TempDir(), "muster", env, nil)
	if err != nil {
		t.Fatalf("startServer: %v", err)
	}
	defer srv.teardown()

	do := func(path, body string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.baseURL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(srv.username, srv.password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
	}
	var created struct {
		ID string `json:"id"`
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.baseURL+"/session", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(srv.username, srv.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	do("/session/"+created.ID+"/message", `{"model":{"providerID":"nope","modelID":"nada"},"parts":[{"type":"text","text":"hi"}]}`)

	name := srv.logs.waitErrorName(ctx, time.Time{}, 5*time.Second)
	if name != "ProviderModelNotFoundError" {
		t.Fatalf("error name from the capped log = %q, want ProviderModelNotFoundError (#288)", name)
	}
}
