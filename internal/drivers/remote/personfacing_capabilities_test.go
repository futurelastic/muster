package remote

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #379: the four capabilities a person-facing client reads are declared
// by the peer's own driver and relayed whole. The remote driver adds no logic
// for them; these tests hold the property that matters across a federation — a
// peer that predates the fields reads as "not declared", never as the false a
// plain boolean would have decoded to.

func notDeclared(t *testing.T, where string, c fleet.DriverCapabilities) {
	t.Helper()
	if c.ReadsTurns != nil || c.ReportsConversation != nil || c.Respond != nil || c.PermissionModeChange != nil {
		t.Errorf("%s: a field nobody declared reads as %v / %v / %+v / %+v; want all absent (not declared, never \"cannot\")",
			where, c.ReadsTurns, c.ReportsConversation, c.Respond, c.PermissionModeChange)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"readsTurns", "reportsConversation", "respond", "permissionModeChange"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("%s: re-marshalled capabilities carry %q; an undeclared field must stay off the wire: %s", where, k, b)
		}
	}
}

func runtimesBody(caps string) json.RawMessage {
	return json.RawMessage(`{"items":[{"machine":"peerbox","runtime":"claude-code-tmux","status":"available",` +
		`"capabilities":` + caps + `}],"sources":[{"machine":"peerbox","status":"ok","observedAt":"2026-10-26T00:00:00Z"}],"complete":true}`)
}

func TestRefreshCapabilities_OlderPeerLeavesPersonFacingFieldsNotDeclared(t *testing.T) {
	// Exactly what a build from before the fields sends.
	srv := peerServing(t, 200, runtimesBody(
		`{"observesState":false,"deliversRawKeys":true,"supportsResume":true,"supportsPin":{"model":true,"effort":true,"agent":true},`+
			`"deadlineMs":5000,"source":"observed"}`), nil)
	d := New("peerbox", srv.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	caps := d.Capabilities()
	if caps.Source != fleet.CapabilitiesObserved || !caps.DeliversRawKeys {
		t.Fatalf("the peer's declaration was not adopted: %+v", caps)
	}
	notDeclared(t, "an older peer", caps)
}

func TestRefreshCapabilities_AdoptsADeclaredFalse(t *testing.T) {
	srv := peerServing(t, 200, runtimesBody(
		`{"readsTurns":false,"reportsConversation":false,"respond":{"kinds":[],"unclassified":false},`+
			`"permissionModeChange":{"live":false},"deadlineMs":5000,"source":"observed"}`), nil)
	d := New("peerbox", srv.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	c := d.Capabilities()
	if c.ReadsTurns == nil || *c.ReadsTurns || c.ReportsConversation == nil || *c.ReportsConversation {
		t.Errorf("a declared false was lost: %v / %v; \"the peer said it cannot\" is not \"not declared\"", c.ReadsTurns, c.ReportsConversation)
	}
	if c.Respond == nil || len(c.Respond.Kinds) != 0 || c.Respond.Unclassified {
		t.Errorf("Respond = %+v, want declared and answering nothing", c.Respond)
	}
	if c.PermissionModeChange == nil || c.PermissionModeChange.Live {
		t.Errorf("PermissionModeChange = %+v, want declared and not live", c.PermissionModeChange)
	}
	b, _ := json.Marshal(c)
	for _, w := range []string{`"readsTurns":false`, `"reportsConversation":false`, `"permissionModeChange":{"live":false}`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("re-marshalled capabilities lost %s: %s", w, b)
		}
	}
}

func TestRefreshCapabilities_RelaysADeclaredYes(t *testing.T) {
	srv := peerServing(t, 200, runtimesBody(
		`{"readsTurns":true,"reportsConversation":true,"respond":{"kinds":["tool-permission","something-newer"],"unclassified":true},`+
			`"permissionModeChange":{"live":true,"via":"keys"},"deadlineMs":5000,"source":"observed"}`), nil)
	d := New("peerbox", srv.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	c := d.Capabilities()
	if c.ReadsTurns == nil || !*c.ReadsTurns || c.ReportsConversation == nil || !*c.ReportsConversation {
		t.Errorf("declared yes was lost: %v / %v", c.ReadsTurns, c.ReportsConversation)
	}
	if c.Respond == nil || !c.Respond.Unclassified || len(c.Respond.Kinds) != 2 || c.Respond.Kinds[1] != "something-newer" {
		t.Errorf("Respond = %+v; a kind a newer peer lists must pass through unchanged", c.Respond)
	}
	if c.PermissionModeChange == nil || !c.PermissionModeChange.Live || c.PermissionModeChange.Via != "keys" {
		t.Errorf("PermissionModeChange = %+v", c.PermissionModeChange)
	}
}

// A peer nobody has heard from has declared nothing, and a declaration that has
// gone stale is the same floor: neither may keep the last answer's flags.
func TestPersonFacingCapabilities_UnreachedAndStaleAreNotDeclared(t *testing.T) {
	notDeclared(t, "a peer that never answered",
		New("peerbox", "http://127.0.0.1:1", WithDeadline(200*time.Millisecond)).Capabilities())

	srv := peerServing(t, 200, runtimesBody(
		`{"readsTurns":true,"reportsConversation":true,"respond":{"kinds":["tool-permission"],"unclassified":false},`+
			`"permissionModeChange":{"live":true,"via":"keys"},"deadlineMs":5000,"source":"observed"}`), nil)
	now := time.Now()
	d := New("peerbox", srv.URL, withClock(func() time.Time { return now }))
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if d.Capabilities().ReadsTurns == nil {
		t.Fatal("a fresh probe did not adopt the peer's declaration")
	}
	now = now.Add(capabilityStaleness + time.Second)
	notDeclared(t, "a declaration past its shelf life", d.Capabilities())
}
