package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

// muster #329: the create set is exactly the modes a session can report back,
// minus the "unknown" a reader could not name — so a created session can always
// read as what it was created as.
func TestCreatePermissionModesAreTheReportableModes(t *testing.T) {
	want := []PermissionModeState{
		PermissionModeDefault, PermissionModeAcceptEdits, PermissionModePlan,
		PermissionModeAuto, PermissionModeBypass,
	}
	got := CreatePermissionModes()
	if len(got) != len(want) {
		t.Fatalf("CreatePermissionModes() = %v, want %d members", got, len(want))
	}
	for i, m := range want {
		if got[i] != string(m) {
			t.Errorf("member %d = %q, want %q", i, got[i], m)
		}
		if !ValidCreatePermissionMode(string(m)) || !m.valid() {
			t.Errorf("%q must be both a create value and a reportable state", m)
		}
	}
	for _, bad := range []string{"", "unknown", "yolo", "Plan", "bypassPermissions"} {
		if ValidCreatePermissionMode(bad) {
			t.Errorf("%q must not be a create value", bad)
		}
	}
	// The slice is a copy: a caller editing it cannot change the set.
	got[0] = "tampered"
	if CreatePermissionModes()[0] != string(PermissionModeDefault) {
		t.Error("CreatePermissionModes returned shared storage")
	}
}

// The grant line is escalation against a plain create, and it fails closed.
func TestPermissionModeNeedsSend(t *testing.T) {
	for mode, want := range map[string]bool{
		"":                   false,
		"default":            false,
		"plan":               false,
		"acceptEdits":        true,
		"auto":               true,
		PermissionModeBypass: true,
		"yolo":               true, // unknown: gated, then refused at the boundary
	} {
		if got := PermissionModeNeedsSend(mode); got != want {
			t.Errorf("PermissionModeNeedsSend(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestDriverCapabilities_CreatePermissionModes(t *testing.T) {
	b, err := json.Marshal(DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "createPermissionModes") {
		t.Fatalf("an unsaid list is unknown, not empty, and must not be serialised: %s", b)
	}
	in := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved, CreatePermissionModes: CreatePermissionModes()}
	b, _ = json.Marshal(in)
	if !strings.Contains(string(b), `"createPermissionModes":["default","acceptEdits","plan","auto","bypass"]`) {
		t.Fatalf("wire shape: %s", b)
	}
	var out DriverCapabilities
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.CreatePermissionModes, ",") != "default,acceptEdits,plan,auto,bypass" {
		t.Fatalf("round trip: %v", out.CreatePermissionModes)
	}
}
