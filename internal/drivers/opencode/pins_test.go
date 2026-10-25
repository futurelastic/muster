package opencode

import (
	"context"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #84, #362: this substrate's create response echoes the agent name it
// was given, listed or not, and a message sent without an agent runs as the
// default one — so the echo is not evidence of anything. A create that asked
// for an agent is therefore reported unresolved, with the reason, and never
// honoured on the strength of the echo alone.
func TestCreate_PinsAgentIsUnresolvedUntilATurnShowsIt(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t", Agent: "coder"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins == nil || sess.Pins.Agent == nil {
		t.Fatalf("Pins = %+v, want an Agent entry", sess.Pins)
	}
	a := sess.Pins.Agent
	if a.Requested != "coder" {
		t.Errorf("Requested = %q, want coder", a.Requested)
	}
	if a.Honoured != nil {
		t.Errorf("Honoured = %v, want nil — the fake server echoes the same agent back, which proves nothing", *a.Honoured)
	}
	if a.Source != "" || a.Applied != "" {
		t.Errorf("Source = %q, Applied = %q, want both empty while unresolved", a.Source, a.Applied)
	}
	if a.Evidence == "" {
		t.Error("Evidence is empty; an unresolved pin must say why")
	}
}

// A requested model has no echo anywhere in this substrate's create
// response, so it must be reported unresolved rather than assumed honoured.
func TestCreate_PinsReportsModelUnresolved(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: "anthropic/claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins == nil || sess.Pins.Model == nil {
		t.Fatalf("Pins = %+v, want a Model entry", sess.Pins)
	}
	if sess.Pins.Model.Honoured != nil {
		t.Errorf("Honoured = %v, want nil — this substrate's response never names the model", sess.Pins.Model.Honoured)
	}
}

func TestCreate_NoPinsRequestedLeavesFieldAbsent(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins != nil {
		t.Errorf("Pins = %+v, want nil", sess.Pins)
	}
}
