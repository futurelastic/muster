package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #412: the default limit on `prompt` and `text` is 32768 bytes. The
// number is pinned by value here on purpose. Every other test refers to
// defaultMaxInputBytes by name and would follow the constant to any value, so
// a change of the default has to fail one test that says what the owner
// decided and when.
func TestDefaultMaxInputBytesIs32768(t *testing.T) {
	if defaultMaxInputBytes != 32768 {
		t.Fatalf("defaultMaxInputBytes = %d, want 32768 (owner decision 2026-11-06, #412): "+
			"a real briefing prompt is 8-15 KB and a fresh install must accept it", defaultMaxInputBytes)
	}
	if got := New("self").MaxInputBytes(); got != 32768 {
		t.Errorf("an unconfigured service reports %d, want 32768", got)
	}
}

// createWithPrompt posts a create whose prompt is n bytes and returns the
// status. The driver is one whose Create succeeds, so a 201 means the request
// got past the length check and was served, not that it was refused for another
// reason further down.
func createWithPrompt(t *testing.T, svc *Service, n int) (*http.Response, *idDriver) {
	t.Helper()
	d := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]string{
		"runtime": "stub", "cwd": "/tmp", "prompt": strings.Repeat("x", n),
	})
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
	req.Header.Set("Idempotency-Key", "key-412")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, d
}

// A machine whose config does not set maxInputBytes accepts a 15 KB create
// prompt: the case that was refused about 140 times in half an hour on a fresh
// install, with "400 ... over the 1024-byte limit".
func TestCreateSession_Accepts15KBPromptWithNoConfiguredLimit(t *testing.T) {
	resp, d := createWithPrompt(t, New("test-machine"), 15*1024)
	if resp.StatusCode != http.StatusCreated {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 201 for a 15 KB prompt on an unconfigured machine; body: %s", resp.StatusCode, body)
	}
	if d.created != 1 {
		t.Errorf("driver.created = %d, want 1 — the request never reached the driver", d.created)
	}
}

// The setting stays overridable downwards (#412): an operator who configures
// 1024 gets the old behaviour back, refused before any driver is resolved, and
// the message names the configured limit rather than the shipped default.
func TestCreateSession_ConfiguredLimitOf1024StillRefuses15KBPrompt(t *testing.T) {
	svc := New("test-machine")
	if err := svc.SetMaxInputBytes(1024); err != nil {
		t.Fatal(err)
	}
	resp, d := createWithPrompt(t, svc, 15*1024)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a configured 1024 must still refuse a 15 KB prompt", resp.StatusCode)
	}
	if d.created != 0 {
		t.Errorf("driver.created = %d, want 0 — the refusal must come before any driver", d.created)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorInvalid {
		t.Errorf("error kind = %q, want %q", env.Error.Kind, fleet.ErrorInvalid)
	}
	if !strings.Contains(env.Error.Message, "1024-byte limit") {
		t.Errorf("message = %q, want it to name the configured 1024-byte limit", env.Error.Message)
	}
}

// Just past the new default is still refused: the cap stayed, only its default
// moved.
func TestCreateSession_RefusesOneByteOverTheNewDefault(t *testing.T) {
	resp, _ := createWithPrompt(t, New("test-machine"), 32768+1)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a prompt one byte over 32768", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if !strings.Contains(env.Error.Message, "32768-byte limit") {
		t.Errorf("message = %q, want it to name the 32768-byte limit", env.Error.Message)
	}
}

// GET /v1/health and GET /v1/machines both report the default when nothing is
// configured, so a caller can size a brief by asking (#130) and gets 32768.
func TestUnconfiguredMachineReportsTheNewDefault(t *testing.T) {
	svc := New("test-machine")
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+path, nil))
		if err != nil {
			t.Fatalf("Do %s: %v", path, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	var health struct {
		MaxInputBytes int `json:"maxInputBytes"`
	}
	if err := json.NewDecoder(get("/v1/health").Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.MaxInputBytes != 32768 {
		t.Errorf("health maxInputBytes = %d, want 32768", health.MaxInputBytes)
	}

	var machines struct {
		Items []fleet.MachineInfo `json:"items"`
	}
	if err := json.NewDecoder(get("/v1/machines").Body).Decode(&machines); err != nil {
		t.Fatalf("decode machines: %v", err)
	}
	var sawSelf bool
	for _, it := range machines.Items {
		if !it.Self {
			continue
		}
		sawSelf = true
		if it.MaxInputBytes != 32768 {
			t.Errorf("self entry maxInputBytes = %d, want 32768", it.MaxInputBytes)
		}
	}
	if !sawSelf {
		t.Fatal("no self entry in /v1/machines")
	}
}
