package sandbox

import (
	"fmt"
	"runtime"

	fleet "github.com/futurelastic/muster"
)

// The names of the facts a host check reports (muster #457). Stable, because a
// test and an operator's grep both read them.
const (
	FactArchitecture = "architecture"
	FactLauncher     = "launcher"
	FactLandlockABI  = "Landlock ABI"
	FactModules      = "active security modules"
	FactSelfTest     = "confined self-test"
	FactBinary       = "sandbox-exec"
	FactProbe        = "profile applied once"
)

// HostFact is one thing the host check looked at, said in the words an operator
// reads: whether it was found, what it was, and when it is the reason the host is
// refused, that it is.
type HostFact struct {
	Name  string
	Value string
}

// HostReport is what the host check found, for the host it ran on (muster #457).
//
// It is the one result both the capability and `muster doctor` read: HostSupport
// is its Support and Reason, and the doctor row is its Facts, so a row that says
// the host is fine cannot sit beside a capability that says it is not. The facts
// are the observations behind the answer, in the order they were made — including
// the ones taken after the first refusal, when taking them cannot change what the
// check decided and tells an operator what else is wrong.
type HostReport struct {
	// Platform is the operating system the check ran on.
	Platform string
	// Mechanism names the facility this platform would confine a session with, or
	// is empty where none is implemented (a platform with nothing to measure).
	Mechanism string
	// Support is what the host can enforce, or nil when it cannot.
	Support *fleet.SandboxSupport
	// Reason is why Support is nil, in the form the capability carries it; empty
	// when the host is offered the mechanism.
	Reason string
	// Facts are the observations, in the order they were made.
	Facts []HostFact
}

// Fact is the value of the named fact, or empty when the check did not make it.
func (r *HostReport) Fact(name string) string {
	for _, f := range r.Facts {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

func (r *HostReport) add(name, format string, args ...any) {
	r.Facts = append(r.Facts, HostFact{Name: name, Value: fmt.Sprintf(format, args...)})
}

// refuse records the first refusal; a later one never replaces it, so the reason
// names the first thing that is not true.
func (r *HostReport) refuse(reason string) {
	if r.Reason == "" && r.Support == nil {
		r.Reason = reason
	}
}

// refused reports whether the check has already found a reason to refuse.
func (r *HostReport) refused() bool { return r.Reason != "" }

// HostSupport reports what the host can enforce, or nil and the reason when it
// cannot — a statement the capability carries, not an error. It is the Support and
// Reason of CheckHost.
//
// The returned support names no package-cache mode: that is a decision of the
// driver, which fills it in (or leaves it empty).
func HostSupport() (*fleet.SandboxSupport, string) {
	r := CheckHost()
	return r.Support, r.Reason
}

func newHostReport(mechanism string) *HostReport {
	return &HostReport{Platform: runtime.GOOS, Mechanism: mechanism}
}
