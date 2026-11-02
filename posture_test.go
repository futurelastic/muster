package fleet

import (
	"errors"
	"strings"
	"testing"
)

// confinedCaps is what a runtime that can confine a session declares.
func confinedCaps() DriverCapabilities {
	return DriverCapabilities{
		Source:                CapabilitiesObserved,
		IsolatesEnvironment:   true,
		CreatePermissionModes: []string{string(PermissionModeDefault), PermissionModeBypass},
		Sandbox: &SandboxSupport{
			Mechanism: "test", Denies: []SandboxDeny{SandboxDenyFiles},
			Network: []SandboxNetwork{SandboxNetworkOpen, SandboxNetworkClosed},
		},
	}
}

// plainCaps is the default terminal runtime: every mode, no sandbox, no isolation.
func plainCaps() DriverCapabilities {
	return DriverCapabilities{Source: CapabilitiesObserved, CreatePermissionModes: CreatePermissionModes()}
}

func TestPostureLower_UnattendedConfinedRuntimeGetsBypassInsideASandbox(t *testing.T) {
	p := Posture{
		Attendance: AttendanceUnattended,
		Files:      &PostureFiles{WritePaths: []AbsolutePath{"/srv/w"}, ReadPaths: []AbsolutePath{"/srv/r"}},
		Network:    SandboxNetworkClosed, IsolatedEnvironment: true,
	}
	m, err := p.Lower(confinedCaps())
	if err != nil {
		t.Fatal(err)
	}
	if m.PermissionMode != PermissionModeBypass || !m.IsolateEnvironment {
		t.Errorf("mapping = %+v", m)
	}
	if m.Sandbox == nil || m.Sandbox.Network != SandboxNetworkClosed ||
		len(m.Sandbox.WritePaths) != 1 || m.Sandbox.WritePaths[0] != "/srv/w" ||
		len(m.Sandbox.ReadPaths) != 1 || m.Sandbox.ReadPaths[0] != "/srv/r" {
		t.Errorf("sandbox = %+v", m.Sandbox)
	}
	if err := m.Sandbox.Validate(); err != nil {
		t.Errorf("the lowered sandbox is not a valid sandbox: %v", err)
	}
}

// Unattended alone still confines: a prompt-free session that reads the whole
// account is what the posture exists to prevent.
func TestPostureLower_UnattendedAloneImpliesTheDefaultSandbox(t *testing.T) {
	m, err := Posture{Attendance: AttendanceUnattended}.Lower(confinedCaps())
	if err != nil {
		t.Fatal(err)
	}
	if m.PermissionMode != PermissionModeBypass || m.Sandbox == nil {
		t.Errorf("mapping = %+v", m)
	}
}

func TestPostureLower_RefusesWhatTheRuntimeCannotEnforce(t *testing.T) {
	noBypass := confinedCaps()
	noBypass.CreatePermissionModes = []string{string(PermissionModeDefault)}
	openOnly := confinedCaps()
	openOnly.Sandbox.Network = []SandboxNetwork{SandboxNetworkOpen}
	noIso := confinedCaps()
	noIso.IsolatesEnvironment = false

	for name, c := range map[string]struct {
		p    Posture
		caps DriverCapabilities
		want string
	}{
		"unattended on a runtime that cannot confine": {Posture{Attendance: AttendanceUnattended}, plainCaps(), "cannot confine"},
		"files on a runtime that cannot confine":      {Posture{Files: &PostureFiles{}}, plainCaps(), "cannot confine"},
		"closed network on a runtime that cannot":     {Posture{Network: SandboxNetworkClosed}, plainCaps(), "cannot confine"},
		"closed network without the posture":          {Posture{Network: SandboxNetworkClosed}, openOnly, "close the network"},
		"unattended without a prompt-free mode":       {Posture{Attendance: AttendanceUnattended}, noBypass, "prompt-free"},
		"isolation on a runtime that cannot":          {Posture{IsolatedEnvironment: true}, noIso, "isolate"},
		"isolation on a runtime without a sandbox":    {Posture{IsolatedEnvironment: true}, plainCaps(), "isolate"},
	} {
		_, err := c.p.Lower(c.caps)
		var fe *Error
		if !errors.As(err, &fe) {
			t.Errorf("%s: err = %v, want a fleet error", name, err)
			continue
		}
		if fe.Kind != ErrorUnsupported || fe.Reason != ReasonPostureUnenforceable {
			t.Errorf("%s: kind=%s reason=%q", name, fe.Kind, fe.Reason)
		}
		if !strings.Contains(fe.Message, c.want) {
			t.Errorf("%s: message %q lacks %q", name, fe.Message, c.want)
		}
	}
}

// Nothing asked, nothing refused — on the runtime that confines nothing too.
func TestPostureLower_AnEmptyOrAttendedPostureIsEnforceableEverywhere(t *testing.T) {
	for _, p := range []Posture{{}, {Attendance: AttendanceAttended}, {Network: SandboxNetworkOpen}} {
		for _, caps := range []DriverCapabilities{plainCaps(), confinedCaps()} {
			m, err := p.Lower(caps)
			if err != nil {
				t.Fatalf("%+v: %v", p, err)
			}
			if m.PermissionMode != "" || m.Sandbox != nil || m.IsolateEnvironment {
				t.Errorf("%+v lowered to %+v, want nothing", p, m)
			}
		}
	}
}

// A runtime nobody has reached says "unknown", never "incapable" (§4.3, D3).
func TestPostureLower_AssumedCapabilitiesNeverRefuse(t *testing.T) {
	c := plainCaps()
	c.Source = CapabilitiesAssumed
	m, err := Posture{Attendance: AttendanceUnattended}.Lower(c)
	if err != nil {
		t.Fatalf("an unreached runtime was refused locally: %v", err)
	}
	if m.PermissionMode != PermissionModeBypass || m.Sandbox == nil {
		t.Errorf("mapping = %+v: the peer needs the lowered fields to decide", m)
	}
}

func TestPostureSupportFrom(t *testing.T) {
	got := PostureSupportFrom(confinedCaps())
	if !got.Unattended || !got.Files || !got.IsolatedEnvironment || len(got.Network) != 2 {
		t.Errorf("confined = %+v", got)
	}
	got = PostureSupportFrom(plainCaps())
	if got.Unattended || got.Files || got.IsolatedEnvironment || got.Network == nil || len(got.Network) != 0 {
		t.Errorf("plain = %+v (network must be an empty list, not absent)", got)
	}
}

func TestPostureValidate(t *testing.T) {
	for name, p := range map[string]Posture{
		"unknown attendance": {Attendance: "sometimes"},
		"unknown network":    {Network: "filtered"},
		"relative path":      {Files: &PostureFiles{ReadPaths: []AbsolutePath{"data"}}},
		"unclean path":       {Files: &PostureFiles{WritePaths: []AbsolutePath{"/a/../b"}}},
		"shared temporary":   {Files: &PostureFiles{WritePaths: []AbsolutePath{"/tmp"}}},
		"root":               {Files: &PostureFiles{ReadPaths: []AbsolutePath{"/"}}},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Posture{Attendance: AttendanceUnattended, Network: SandboxNetworkClosed,
		Files: &PostureFiles{WritePaths: []AbsolutePath{"/srv/w"}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid posture was refused: %v", err)
	}
}

// A runtime that could offer a sandbox on another host and cannot on this one
// says why, in the refusal itself (muster #345): "posture-unenforceable" with no
// reason sends an operator hunting through logs for what the runtime already knew.
func TestPostureLower_ARefusalCarriesTheReasonTheRuntimeGave(t *testing.T) {
	caps := plainCaps()
	caps.SandboxUnavailable = "the agent CLI is not logged in when it runs under the profile on this host"
	_, err := Posture{Attendance: AttendanceUnattended}.Lower(caps)
	var fe *Error
	if !errors.As(err, &fe) || fe.Reason != ReasonPostureUnenforceable {
		t.Fatalf("err = %v, want posture-unenforceable", err)
	}
	if !strings.Contains(fe.Message, caps.SandboxUnavailable) {
		t.Errorf("message %q does not carry the runtime's reason", fe.Message)
	}
	// A runtime that gave none is refused exactly as before.
	if _, err := (Posture{Attendance: AttendanceUnattended}).Lower(plainCaps()); err == nil ||
		strings.Contains(err.Error(), ": )") {
		t.Errorf("err = %v", err)
	}
}

// muster #392: a session whose configuration is only what the create names is a
// requirement the runtime declares or the create is refused — and it is
// independent of confinement, so the plain terminal runtime can give it.
func configCaps() DriverCapabilities {
	c := plainCaps()
	c.IsolatesConfiguration = true
	return c
}

func TestPostureLower_GivenConfigurationLowersWhereTheRuntimeDeclaresIt(t *testing.T) {
	for name, caps := range map[string]DriverCapabilities{"plain": configCaps(), "confined": func() DriverCapabilities {
		c := confinedCaps()
		c.IsolatesConfiguration = true
		return c
	}()} {
		m, err := Posture{Configuration: ConfigurationGiven}.Lower(caps)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !m.IsolateConfiguration {
			t.Errorf("%s: mapping = %+v, want IsolateConfiguration", name, m)
		}
		// Asking for it must not drag a sandbox or a mode in with it.
		if m.Sandbox != nil || m.PermissionMode != "" || m.IsolateEnvironment {
			t.Errorf("%s: mapping = %+v, want only the configuration", name, m)
		}
	}
}

func TestPostureLower_GivenConfigurationRefusedWhereTheRuntimeCannot(t *testing.T) {
	_, err := Posture{Configuration: ConfigurationGiven}.Lower(plainCaps())
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a fleet error", err)
	}
	if fe.Kind != ErrorUnsupported || fe.Reason != ReasonPostureUnenforceable {
		t.Errorf("kind=%s reason=%q, want unsupported / posture-unenforceable", fe.Kind, fe.Reason)
	}
	if !strings.Contains(fe.Message, "configuration the create names") {
		t.Errorf("message %q does not say what is missing", fe.Message)
	}
}

// Absent and "inherit" are today's behaviour: nothing lowered, nothing refused,
// on a runtime that cannot give the guarantee too.
func TestPostureLower_InheritedConfigurationAsksForNothing(t *testing.T) {
	for _, p := range []Posture{{}, {Configuration: ConfigurationInherit}} {
		for _, caps := range []DriverCapabilities{plainCaps(), configCaps()} {
			m, err := p.Lower(caps)
			if err != nil {
				t.Fatalf("%+v: %v", p, err)
			}
			if m.IsolateConfiguration {
				t.Errorf("%+v lowered to %+v, want no isolation", p, m)
			}
		}
	}
}

// An unreached runtime is never refused locally (§4.3, D3), but the field is
// lowered so the peer, which can decide, receives the requirement.
func TestPostureLower_GivenConfigurationOnAssumedCapabilitiesIsLoweredNotRefused(t *testing.T) {
	c := plainCaps()
	c.Source = CapabilitiesAssumed
	m, err := Posture{Configuration: ConfigurationGiven}.Lower(c)
	if err != nil {
		t.Fatalf("an unreached runtime was refused locally: %v", err)
	}
	if !m.IsolateConfiguration {
		t.Errorf("mapping = %+v: the peer needs the lowered field to decide", m)
	}
}

func TestPostureSupportFrom_GivenConfiguration(t *testing.T) {
	if !PostureSupportFrom(configCaps()).GivenConfiguration {
		t.Error("a runtime declaring IsolatesConfiguration reports no support")
	}
	if PostureSupportFrom(plainCaps()).GivenConfiguration {
		t.Error("a runtime not declaring IsolatesConfiguration reports support")
	}
}

func TestPostureValidate_ConfigurationMustBeKnown(t *testing.T) {
	if err := (Posture{Configuration: "partial"}).Validate(); err == nil {
		t.Error("an unknown configuration was accepted")
	}
	for _, c := range []Configuration{"", ConfigurationInherit, ConfigurationGiven} {
		if err := (Posture{Configuration: c}).Validate(); err != nil {
			t.Errorf("configuration %q refused: %v", c, err)
		}
	}
}
