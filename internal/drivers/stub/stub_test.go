package stub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

var testCaller = fleet.Request{Caller: fleet.Caller{Principal: "test:unit"}}

func TestDriver_CapabilitiesDefaultsToPositiveDeadline(t *testing.T) {
	d := &Driver{}
	caps := d.Capabilities()
	if err := caps.Validate(); err != nil {
		t.Fatalf("Capabilities().Validate() = %v, want nil — a stub must still declare a deadline (§4.4)", err)
	}
}

func TestDriver_CapabilitiesHonoursExplicitDeadline(t *testing.T) {
	d := &Driver{DeadlineMs: 42}
	if got := d.Capabilities().DeadlineMs; got != 42 {
		t.Fatalf("DeadlineMs = %d, want 42", got)
	}
}

func TestDriver_EveryOperationReturnsUnsupported(t *testing.T) {
	d := &Driver{}
	ctx := context.Background()
	ref := fleet.SessionRef{Machine: "m1", ID: "x"}

	if _, err := d.Create(ctx, testCaller, "key", fleet.SessionSpec{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Create err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Send(ctx, testCaller, ref, "hi", driver.SendOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Send err = %v, want ErrUnsupported", err)
	}
	if _, err := d.State(ctx, testCaller, ref); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("State err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Interrupt(ctx, testCaller, ref); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Interrupt err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Close(ctx, testCaller, ref); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Close err = %v, want ErrUnsupported", err)
	}
	if _, err := d.List(ctx, testCaller, driver.ListFilter{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("List err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Subscribe(ctx, testCaller, driver.SubscribeFilter{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Subscribe err = %v, want ErrUnsupported", err)
	}
}

// This driver supports no operation, and says so in the declared "cannot" form
// of each person-facing capability (muster #379) rather than leaving them
// undeclared: it is describing itself, so "not declared" would be a claim of
// ignorance it does not have.
func TestDriver_DeclaresItCannotDoWhatAPersonFacingClientNeeds(t *testing.T) {
	c := (&Driver{}).Capabilities()
	if c.ReadsTurns == nil || *c.ReadsTurns {
		t.Errorf("ReadsTurns = %v, want declared false", c.ReadsTurns)
	}
	if _, ok := any(&Driver{}).(driver.TurnReader); ok {
		t.Error("declares it cannot read turns but implements driver.TurnReader")
	}
	if c.ReportsConversation == nil || *c.ReportsConversation {
		t.Errorf("ReportsConversation = %v, want declared false", c.ReportsConversation)
	}
	if c.Respond == nil || len(c.Respond.Kinds) != 0 || c.Respond.Unclassified {
		t.Errorf("Respond = %+v, want declared and answering nothing", c.Respond)
	}
	if c.PermissionModeChange == nil || c.PermissionModeChange.Live {
		t.Errorf("PermissionModeChange = %+v, want declared and not live", c.PermissionModeChange)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"respond":{"kinds":[],"unclassified":false}`) {
		t.Errorf("a declared empty kinds list must be [] on the wire, not null: %s", b)
	}
}
