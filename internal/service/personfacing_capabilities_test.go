package service

import (
	"encoding/json"
	"net/http"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// declaringDriver is a stub that declares exactly the capabilities it is given,
// so a test controls which of the person-facing fields (muster #379) exist.
type declaringDriver struct {
	*stub.Driver
	caps fleet.DriverCapabilities
}

func (d declaringDriver) Capabilities() fleet.DriverCapabilities { return d.caps }

// The four capabilities a person-facing client reads cross GET /v1/runtimes as
// the driver declared them: a declared "can" and a declared "cannot" are both on
// the wire, and a peer that predates the fields sends none of them — a missing
// key, never a false a client would read as "cannot".
func TestRuntimes_PersonFacingCapabilitiesPassThrough(t *testing.T) {
	svc, srv := newTestServer(t) // registers the stub runtime, which declares "cannot"

	if err := svc.RegisterLocalDriver("declared", declaringDriver{Driver: &stub.Driver{DeadlineMs: 200}, caps: fleet.DriverCapabilities{
		DeadlineMs: 200, Source: fleet.CapabilitiesObserved,
		ReadsTurns: fleet.Declared(true), ReportsConversation: fleet.Declared(true),
		Respond:              &fleet.RespondSupport{Kinds: []fleet.PromptKind{fleet.PromptToolPermission}},
		PermissionModeChange: &fleet.PermissionModeChangeSupport{Live: true, Via: fleet.PermissionModeChangeViaKeys},
	}}); err != nil {
		t.Fatal(err)
	}
	// A peer built before the fields: observed, and says nothing about them.
	if err := svc.RegisterPeerDriver("otherbox", declaringDriver{Driver: &stub.Driver{DeadlineMs: 200}, caps: fleet.DriverCapabilities{
		DeadlineMs: 200, Source: fleet.CapabilitiesObserved, SupportsResume: true,
	}}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+"/v1/runtimes", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Items []struct {
			Machine      string                     `json:"machine"`
			Runtime      string                     `json:"runtime"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	keys := []string{"readsTurns", "reportsConversation", "respond", "permissionModeChange"}
	want := map[string]map[string]string{
		"test-machine/declared": {
			"readsTurns": `true`, "reportsConversation": `true`,
			"respond":              `{"kinds":["tool-permission"],"unclassified":false}`,
			"permissionModeChange": `{"live":true,"via":"keys"}`,
		},
		"test-machine/stub": {
			"readsTurns": `false`, "reportsConversation": `false`,
			"respond":              `{"kinds":[],"unclassified":false}`,
			"permissionModeChange": `{"live":false}`,
		},
		"otherbox/": nil, // not declared: every key absent
	}
	seen := map[string]bool{}
	for _, it := range body.Items {
		id := it.Machine + "/" + it.Runtime
		w, ok := want[id]
		if !ok {
			continue
		}
		seen[id] = true
		for _, k := range keys {
			got, present := it.Capabilities[k]
			if w == nil {
				if present {
					t.Errorf("%s: %s = %s; a peer that predates the field sends none, and must read as not declared", id, k, got)
				}
				continue
			}
			if !present || string(got) != w[k] {
				t.Errorf("%s: %s = %s (present %v), want %s", id, k, got, present, w[k])
			}
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("no row for %s in /v1/runtimes", id)
		}
	}
}

// The record capabilities (muster #441, #444) cross GET /v1/runtimes as declared:
// a runtime that reads a record says so and names the format; one that cannot
// says false and names none (there is no format to name); a peer that predates
// the fields sends neither, which is not declared and never "cannot".
func TestRuntimes_RecordCapabilitiesPassThrough(t *testing.T) {
	svc, srv := newTestServer(t) // the stub runtime declares readsRecord:false

	if err := svc.RegisterLocalDriver("declared", declaringDriver{Driver: &stub.Driver{DeadlineMs: 200}, caps: fleet.DriverCapabilities{
		DeadlineMs: 200, Source: fleet.CapabilitiesObserved,
		ReadsRecord: fleet.Declared(true), RecordFormat: fleet.DeclaredInt(fleet.RecordFormat),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("otherbox", declaringDriver{Driver: &stub.Driver{DeadlineMs: 200}, caps: fleet.DriverCapabilities{
		DeadlineMs: 200, Source: fleet.CapabilitiesObserved, SupportsResume: true,
	}}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+"/v1/runtimes", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Items []struct {
			Machine      string                     `json:"machine"`
			Runtime      string                     `json:"runtime"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	// "" means the key must be absent.
	want := map[string]map[string]string{
		"test-machine/declared": {"readsRecord": `true`, "recordFormat": `1`},
		"test-machine/stub":     {"readsRecord": `false`, "recordFormat": ``},
		"otherbox/":             {"readsRecord": ``, "recordFormat": ``},
	}
	seen := map[string]bool{}
	for _, it := range body.Items {
		id := it.Machine + "/" + it.Runtime
		w, ok := want[id]
		if !ok {
			continue
		}
		seen[id] = true
		for k, wv := range w {
			got, present := it.Capabilities[k]
			switch {
			case wv == "" && present:
				t.Errorf("%s: %s = %s; it must be absent", id, k, got)
			case wv != "" && (!present || string(got) != wv):
				t.Errorf("%s: %s = %s (present %v), want %s", id, k, got, present, wv)
			}
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("no row for %s in /v1/runtimes", id)
		}
	}
}
