package service

import (
	"encoding/json"
	"net/http"
	"testing"
)

// muster #280: a peer's /v1/health is how a relaying machine learns whether it
// may forward `isolateEnvironment` without it being silently dropped.
func TestHealth_ReportsSupportsIsolateEnvironment(t *testing.T) {
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
	if got, ok := body["supportsIsolateEnvironment"].(bool); !ok || !got {
		t.Errorf("supportsIsolateEnvironment = %v (present=%v), want true", body["supportsIsolateEnvironment"], ok)
	}
}

// The create body field reaches the driver, and a caller holding only create
// may send it: it narrows what the session gets, it grants nothing.
func TestCreate_PassesIsolateEnvironmentToTheDriver(t *testing.T) {
	_, d, srv := carryServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "iso-1", `{"runtime":"stub","cwd":"/w","isolateEnvironment":true}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if !d.last().IsolateEnvironment {
		t.Error("the driver's spec lost isolateEnvironment")
	}
	code, _, raw = carryCreate(t, srv, testToken, "iso-2", `{"runtime":"stub","cwd":"/w"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if d.last().IsolateEnvironment {
		t.Error("an absent isolateEnvironment arrived as true")
	}
}
