package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// muster #414: a delivery module the operator enabled and the daemon skipped
// left the daemon ok by every measure but one log line. GET /v1/health now says
// so, under `modules`, and says nothing when no module is configured.

func healthBody(t *testing.T, svc *Service) []byte {
	t.Helper()
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+"/v1/health", nil))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// With nothing configured the body has no `modules` key at all: a machine that
// never used the feature answers exactly as it did before.
func TestHealth_NoModulesKeyWhenNoneConfigured(t *testing.T) {
	svc := New("test-machine")
	if strings.Contains(string(healthBody(t, svc)), `"modules"`) {
		t.Error("health carries a `modules` key although no module is configured")
	}

	// An explicit empty status is the same as none.
	svc.SetDeliveryModulesStatus(DeliveryModulesStatus{})
	if strings.Contains(string(healthBody(t, svc)), `"modules"`) {
		t.Error("health carries a `modules` key for an empty status")
	}
}

// A configured module that was skipped is visible to a script that reads only
// health: enabled lists what was asked for, loaded what was found, skipped the
// rest with the reason the log line gives.
func TestHealth_ReportsAConfiguredButSkippedModule(t *testing.T) {
	svc := New("test-machine")
	svc.SetDeliveryModulesStatus(DeliveryModulesStatus{
		Enabled: []string{"relay-a", "relay-b"},
		Loaded:  []string{"relay-a"},
		Skipped: []SkippedDeliveryModule{
			{Name: "Bad Name", Reason: "not a legal module name"},
			{Name: "relay-b", Reason: "modules directory absent"},
		},
	})

	var health struct {
		Modules *struct {
			Enabled []string `json:"enabled"`
			Loaded  []string `json:"loaded"`
			Skipped []struct {
				Name   string `json:"name"`
				Reason string `json:"reason"`
			} `json:"skipped"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(healthBody(t, svc), &health); err != nil {
		t.Fatal(err)
	}
	m := health.Modules
	if m == nil {
		t.Fatal("health has no `modules` block although a module is configured")
	}
	if strings.Join(m.Enabled, ",") != "relay-a,relay-b" || strings.Join(m.Loaded, ",") != "relay-a" {
		t.Errorf("enabled=%v loaded=%v, want [relay-a relay-b] and [relay-a]", m.Enabled, m.Loaded)
	}
	if len(m.Skipped) != 2 ||
		m.Skipped[0].Name != "Bad Name" || m.Skipped[0].Reason != "not a legal module name" ||
		m.Skipped[1].Name != "relay-b" || m.Skipped[1].Reason != "modules directory absent" {
		t.Errorf("skipped = %+v, want the illegal name then relay-b with the reasons given", m.Skipped)
	}
}

// All three lists are arrays whatever was passed: a poller indexing
// `.modules.skipped | length` must not meet a null.
func TestHealth_ModulesListsAreNeverNull(t *testing.T) {
	svc := New("test-machine")
	svc.SetDeliveryModulesStatus(DeliveryModulesStatus{Enabled: []string{"relay-a"}})
	body := string(healthBody(t, svc))
	for _, want := range []string{`"enabled":["relay-a"]`, `"loaded":[]`, `"skipped":[]`} {
		if !strings.Contains(strings.ReplaceAll(body, " ", ""), want) {
			t.Errorf("health body lacks %s: %s", want, body)
		}
	}

	// Only an illegal name configured: nothing is enabled, and it is still
	// reported — it was configured and dropped the same way.
	svc = New("test-machine")
	svc.SetDeliveryModulesStatus(DeliveryModulesStatus{
		Skipped: []SkippedDeliveryModule{{Name: "Relay", Reason: "not a legal module name"}},
	})
	body = string(healthBody(t, svc))
	if !strings.Contains(body, `"modules"`) || !strings.Contains(strings.ReplaceAll(body, " ", ""), `"enabled":[]`) || !strings.Contains(body, `"Relay"`) {
		t.Errorf("an illegal name alone is not reported: %s", body)
	}
}
