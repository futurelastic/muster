package fleet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDriverCapabilities_Validate_RejectsZeroDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: 0}
	if err := c.Validate(); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Validate() = %v, want ErrNoDeadline", err)
	}
}

func TestDriverCapabilities_Validate_RejectsNegativeDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: -1}
	if err := c.Validate(); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Validate() = %v, want ErrNoDeadline", err)
	}
}

func TestDriverCapabilities_Validate_AcceptsPositiveDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: 3000}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// A driver that cannot toggle remote control declares nothing: the key is
// absent from the wire, so "is there a control to offer" is the field's
// presence and never a pair of falses a client has to interpret (muster #269).
func TestDriverCapabilities_RemoteControlIsOmittedWhenUndeclared(t *testing.T) {
	b, err := json.Marshal(DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "remoteControl") {
		t.Fatalf("an undeclared capability must not be serialised: %s", b)
	}
	// And the floor an unreached peer reports is the same value, so it omits it too.
	b, _ = json.Marshal(DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved}.Assumed())
	if strings.Contains(string(b), "remoteControl") {
		t.Fatalf("an assumed floor must not declare the capability: %s", b)
	}
}

func TestDriverCapabilities_RemoteControlRoundTrips(t *testing.T) {
	in := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved,
		RemoteControl: &RemoteControlSupport{Toggle: true, Off: false}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"remoteControl":{"toggle":true,"off":false}`) {
		t.Fatalf("wire shape: %s", b)
	}
	var out DriverCapabilities
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.RemoteControl == nil || !out.RemoteControl.Toggle || out.RemoteControl.Off {
		t.Fatalf("round trip lost it: %+v", out.RemoteControl)
	}
}
