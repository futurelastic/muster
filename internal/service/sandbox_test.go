package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// muster #281: a peer's /v1/health is how a relaying machine learns whether it
// may forward `sandbox` without it being silently dropped — and a dropped
// sandbox is a session started unconfined.
func TestHealth_ReportsSupportsSandbox(t *testing.T) {
	_, srv := newTestServer(t)
	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+"/v1/health", nil))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := body["supportsSandbox"].(bool); !ok || !got {
		t.Errorf("supportsSandbox = %v (present=%v), want true", body["supportsSandbox"], ok)
	}
}

// The create body field reaches the driver, and a caller holding only create may
// send it: it narrows what the session reaches, it grants nothing.
func TestCreate_PassesSandboxToTheDriver(t *testing.T) {
	_, d, srv := carryServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "sbx-1",
		`{"runtime":"stub","cwd":"/w","sandbox":{"readPaths":["/srv/data"],"network":"closed"}}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	sb := d.last().Sandbox
	if sb == nil || len(sb.ReadPaths) != 1 || sb.ReadPaths[0] != "/srv/data" || sb.Network != "closed" {
		t.Errorf("the driver's spec lost the sandbox: %+v", sb)
	}
	code, _, raw = carryCreate(t, srv, testToken, "sbx-2", `{"runtime":"stub","cwd":"/w"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if d.last().Sandbox != nil {
		t.Error("an absent sandbox arrived as a request for one")
	}
}

// A malformed sandbox is a 400 before any driver is asked, whatever the machine.
func TestCreate_RefusesAMalformedSandbox(t *testing.T) {
	_, d, srv := carryServer(t, nil)
	for name, body := range map[string]string{
		"relative path":    `{"runtime":"stub","cwd":"/w","sandbox":{"readPaths":["data"]}}`,
		"shared temporary": `{"runtime":"stub","cwd":"/w","sandbox":{"writePaths":["/tmp"]}}`,
		"unknown network":  `{"runtime":"stub","cwd":"/w","sandbox":{"network":"filtered"}}`,
	} {
		code, _, raw := carryCreate(t, srv, testToken, "bad-"+strings.ReplaceAll(name, " ", "-"), body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, code, raw)
		}
	}
	d.mu.Lock()
	n := len(d.specs)
	d.mu.Unlock()
	if n != 0 {
		t.Errorf("a malformed sandbox reached the driver %d time(s)", n)
	}
}
