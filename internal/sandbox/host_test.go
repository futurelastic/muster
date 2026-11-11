package sandbox

import (
	"reflect"
	"runtime"
	"testing"
)

// muster #457: the report is the one thing the capability and `muster doctor` both
// read, on every platform.
func TestCheckHostIsOneConsistentAnswerOnThisPlatform(t *testing.T) {
	rep := CheckHost()
	if rep.Platform != runtime.GOOS {
		t.Errorf("platform = %q, want %q", rep.Platform, runtime.GOOS)
	}
	switch runtime.GOOS {
	case "darwin":
		if rep.Mechanism != Mechanism {
			t.Errorf("mechanism = %q, want %q", rep.Mechanism, Mechanism)
		}
		if rep.Fact(FactBinary) == "" || (rep.Support != nil && rep.Fact(FactProbe) == "") {
			t.Errorf("facts = %+v", rep.Facts)
		}
	case "linux":
		if rep.Mechanism != MechanismLinux {
			t.Errorf("mechanism = %q, want %q", rep.Mechanism, MechanismLinux)
		}
	default:
		// A platform with nothing to measure says so, and has no facts to give.
		if rep.Mechanism != "" || rep.Support != nil || rep.Reason == "" || len(rep.Facts) != 0 {
			t.Errorf("report = %+v", rep)
		}
	}
	// Offered or refused, never both and never neither.
	if (rep.Support == nil) == (rep.Reason == "") {
		t.Errorf("support %+v with reason %q", rep.Support, rep.Reason)
	}
	if rep.Support != nil && rep.Support.Mechanism != rep.Mechanism {
		t.Errorf("the support names %q, the report %q", rep.Support.Mechanism, rep.Mechanism)
	}
	for _, f := range rep.Facts {
		if f.Name == "" || f.Value == "" {
			t.Errorf("a fact with nothing in it: %+v", f)
		}
	}
}

func TestHostSupportReadsTheReportItIsDerivedFrom(t *testing.T) {
	rep := CheckHost()
	sup, why := HostSupport()
	if why != rep.Reason || !reflect.DeepEqual(sup, rep.Support) {
		t.Errorf("HostSupport = %+v %q, the report says %+v %q", sup, why, rep.Support, rep.Reason)
	}
}

func TestReportKeepsTheFirstRefusal(t *testing.T) {
	rep := newHostReport("m")
	rep.refuse("first")
	rep.refuse("second")
	if rep.Reason != "first" || !rep.refused() {
		t.Errorf("reason = %q", rep.Reason)
	}
	if got := rep.Fact("absent"); got != "" {
		t.Errorf("a fact that was not made = %q", got)
	}
	rep.add("a", "%d-%s", 1, "x")
	if got := rep.Fact("a"); got != "1-x" {
		t.Errorf("fact = %q", got)
	}
}
