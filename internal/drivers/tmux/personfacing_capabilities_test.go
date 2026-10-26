package tmux

import (
	"encoding/json"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #379: what a person-facing client reads before it offers a control,
// declared by the driver for what it really does.

// turns and conversation both read the runtime's own record store, which only
// WithRecordRoot wires. Without it neither can resolve, so the declaration is
// "cannot" — present and false, not undeclared.
func TestCapabilities_TurnsAndConversationFollowTheRecordRoot(t *testing.T) {
	without := newTestDriver(twoSessions())
	with := New("testbox", withExec(twoSessions().exec), WithRecordRoot(t.TempDir()))

	for name, tc := range map[string]struct {
		d    *Driver
		want bool
	}{"no record root": {without, false}, "record root": {with, true}} {
		c := tc.d.Capabilities()
		if c.ReadsTurns == nil || *c.ReadsTurns != tc.want {
			t.Errorf("%s: ReadsTurns = %v, want declared %v", name, c.ReadsTurns, tc.want)
		}
		if c.ReportsConversation == nil || *c.ReportsConversation != tc.want {
			t.Errorf("%s: ReportsConversation = %v, want declared %v", name, c.ReportsConversation, tc.want)
		}
	}
	// A declared "can read turns" must be a driver that implements the read.
	if _, ok := any(with).(driver.TurnReader); !ok {
		t.Error("declares ReadsTurns but does not implement driver.TurnReader")
	}
}

// respond answers every prompt this driver reports, classified or not; and a
// running session's mode changes through keys, which needs both flags below.
func TestCapabilities_RespondAndPermissionModeChange(t *testing.T) {
	c := newTestDriver(twoSessions()).Capabilities()

	if c.Respond == nil || !c.Respond.Unclassified {
		t.Fatalf("Respond = %+v, want declared, answering an unclassified prompt", c.Respond)
	}
	got, want := c.Respond.Kinds, fleet.PromptKinds()
	if len(got) != len(want) {
		t.Fatalf("Respond.Kinds = %v, want every kind %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Respond.Kinds[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	p := c.PermissionModeChange
	if p == nil || !p.Live || p.Via != fleet.PermissionModeChangeViaKeys {
		t.Fatalf("PermissionModeChange = %+v, want live via keys", p)
	}
	// "via keys" is only true if the driver delivers keys and reads the mode back.
	if !c.DeliversRawKeys || !c.ObservesPermissionMode {
		t.Errorf("declares a mode change through keys with DeliversRawKeys=%v ObservesPermissionMode=%v",
			c.DeliversRawKeys, c.ObservesPermissionMode)
	}
}

// The wire shape a client reads for this runtime, with a record root configured.
func TestCapabilities_PersonFacingWireShape(t *testing.T) {
	c := New("testbox", withExec(twoSessions().exec), WithRecordRoot(t.TempDir())).Capabilities()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		`"readsTurns":true`, `"reportsConversation":true`,
		`"respond":{"kinds":["resume-chooser","folder-trust","external-imports","settings-trust","tool-permission","bypass-permissions","feedback-review"],"unclassified":true}`,
		`"permissionModeChange":{"live":true,"via":"keys"}`,
	} {
		if !strings.Contains(string(b), w) {
			t.Errorf("wire shape lacks %s:\n%s", w, b)
		}
	}
}
