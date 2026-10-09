package opencode

import "testing"

// This driver's whole reason to exist (#55): it is the first local driver
// able to declare ObservesState: true, and it must declare its raw-key and
// resume limits honestly rather than optimistically.
func TestCapabilities_DeclaresObservedStateAndItsRealLimits(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	caps := d.Capabilities()

	if !caps.ObservesState {
		t.Error("ObservesState = false; this is the entire point of #55")
	}
	if caps.DeliversRawKeys {
		t.Error("DeliversRawKeys = true; this substrate has no screen for a raw key to land on")
	}
	if caps.ConfirmsDelivery {
		t.Error("ConfirmsDelivery = true; prompt_async's 204 is acceptance, not confirmation")
	}
	if caps.SupportsResume {
		t.Error("SupportsResume = true with no state directory; nothing is remembered across a restart (see package doc)")
	}
	if !caps.SupportsPin.Model {
		t.Error("SupportsPin.Model = false; Create genuinely honours a provider/model hint")
	}
	if !caps.SupportsPin.Agent {
		t.Error("SupportsPin.Agent = false; Create genuinely honours an agent hint")
	}
	if caps.SupportsPin.Effort {
		t.Error("SupportsPin.Effort = true; there is no analogous parameter and Create refuses it")
	}
	if !caps.ValidatesModel {
		t.Error("ValidatesModel = false; Create checks a model against the runtime's own catalog (#287)")
	}
	if caps.Source != "observed" {
		t.Errorf("Source = %q, want \"observed\" — a local driver describing itself", caps.Source)
	}
	if err := caps.Validate(); err != nil {
		t.Errorf("Validate: %v (§4.4: DeadlineMs must be positive)", err)
	}
}

func TestCapabilities_DeadlineIsConfigurable(t *testing.T) {
	f := newFakeServer(t)
	d, err := newDriverWithOptions(t, f, WithDeadline(1234))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Capabilities().DeadlineMs; got != 1234 {
		t.Errorf("DeadlineMs = %d, want 1234", got)
	}
}
