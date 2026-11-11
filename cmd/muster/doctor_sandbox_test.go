package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
)

// muster #457: the sandbox.host row reports the host check's own report. The reports
// below are what that check gives, handed in: the sandbox package's tests inject the
// kernel's answers, and these are about what the row makes of them.

func linuxFacts(abi, modules, selfTest string) []sandbox.HostFact {
	return []sandbox.HostFact{
		{Name: sandbox.FactArchitecture, Value: "amd64 (its system call numbers were measured)"},
		{Name: sandbox.FactLauncher, Value: "named (this process can start a session through itself)"},
		{Name: sandbox.FactLandlockABI, Value: abi},
		{Name: sandbox.FactModules, Value: modules},
		{Name: sandbox.FactSelfTest, Value: selfTest},
	}
}

func offered(mechanism string, facts []sandbox.HostFact) *sandbox.HostReport {
	return &sandbox.HostReport{
		Platform:  "linux",
		Mechanism: mechanism,
		Support:   &fleet.SandboxSupport{Mechanism: mechanism},
		Facts:     facts,
	}
}

func TestCheckSandboxHost(t *testing.T) {
	const prefix = sandbox.MechanismLinux + ": "
	for _, tc := range []struct {
		name    string
		report  *sandbox.HostReport
		want    rowStatus
		summary []string
		detail  []string
	}{
		{
			name: "an offered mechanism passes and says what it saw",
			report: offered(sandbox.MechanismLinux, linuxFacts("7 (the mechanism needs 6 or newer, Linux 6.12)",
				"lockdown, capability, landlock, yama", "passed (a process confined the way a session will be could not do what the capability says it cannot)")),
			want:    statusPass,
			summary: []string{"can enforce the sandbox", sandbox.MechanismLinux},
			detail: []string{"mechanism: " + sandbox.MechanismLinux, "Landlock ABI: 7 (the mechanism needs 6 or newer, Linux 6.12)",
				"active security modules: lockdown, capability, landlock, yama", "confined self-test: passed", "the host's half"},
		},
		{
			name: "a kernel below the floor warns with the ABI against it",
			report: &sandbox.HostReport{
				Platform: "linux", Mechanism: sandbox.MechanismLinux,
				Reason: prefix + "Landlock ABI 5 is below the 6 this mechanism needs (Linux 6.12: abstract unix sockets and signals are scoped)",
				Facts: linuxFacts("5, below the 6 the mechanism needs (Linux 6.12)", "lockdown, capability, landlock",
					"not run: an earlier check refused, and the test needs a launcher and a kernel with Landlock"),
			},
			want:    statusWarn,
			summary: []string{"not offered", prefix + "Landlock ABI 5 is below the 6"},
			detail:  []string{"Landlock ABI: 5, below the 6", "Linux 6.12", "confined self-test: not run", "sandboxUnavailable", "unaffected"},
		},
		{
			name: "a module list without landlock is shown beside the kernel's refusal",
			report: &sandbox.HostReport{
				Platform: "linux", Mechanism: sandbox.MechanismLinux,
				Reason: prefix + "Landlock is built into the kernel but not enabled (the boot parameter lsm= lacks landlock)",
				Facts: linuxFacts("none: Landlock is built into the kernel but not enabled", "lockdown, capability, yama; landlock is not among them",
					"not run: an earlier check refused, and the test needs a launcher and a kernel with Landlock"),
			},
			want:    statusWarn,
			summary: []string{"lsm= lacks landlock"},
			detail:  []string{"active security modules: lockdown, capability, yama; landlock is not among them"},
		},
		{
			name: "a self-test that failed is the reason, in the row",
			report: &sandbox.HostReport{
				Platform: "linux", Mechanism: sandbox.MechanismLinux,
				Reason: prefix + "a process confined the way a session will be failed the self-test (FAIL: reading a file outside the grants: got <nil>)",
				Facts: linuxFacts("7 (the mechanism needs 6 or newer, Linux 6.12)", "lockdown, capability, landlock",
					"a process confined the way a session will be failed the self-test (FAIL: reading a file outside the grants: got <nil>)"),
			},
			want:    statusWarn,
			summary: []string{"failed the self-test", "reading a file outside the grants"},
			detail:  []string{"confined self-test: a process confined the way a session will be failed the self-test"},
		},
		{
			name: "a macOS host that cannot apply a profile warns",
			report: &sandbox.HostReport{
				Platform: "darwin", Mechanism: sandbox.Mechanism,
				Reason: "the host refused to apply a sandbox profile (Operation not permitted): the service may itself be running inside one, which cannot be nested",
				Facts: []sandbox.HostFact{{Name: sandbox.FactBinary, Value: "present"},
					{Name: sandbox.FactProbe, Value: "the host refused to apply a sandbox profile (Operation not permitted)"}},
			},
			want:    statusWarn,
			summary: []string{"cannot be nested"},
			detail:  []string{"mechanism: " + sandbox.Mechanism, "sandbox-exec: present"},
		},
		{
			name:    "a platform with no mechanism is not applicable",
			report:  &sandbox.HostReport{Platform: "windows", Reason: "no sandbox mechanism is implemented for windows (only macOS's sandbox-exec and Linux's Landlock and seccomp are)"},
			want:    statusSkip,
			summary: []string{"n/a", "no sandbox mechanism is implemented for windows"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := checkSandboxHost(tc.report)
			if row.ID != "sandbox.host" || row.Status != tc.want {
				t.Fatalf("got %+v, want status %s", row, tc.want)
			}
			for _, want := range tc.summary {
				if !strings.Contains(row.Summary, want) {
					t.Errorf("summary %q does not say %q", row.Summary, want)
				}
			}
			for _, want := range tc.detail {
				if !strings.Contains(row.Detail, want) {
					t.Errorf("detail %q does not say %q", row.Detail, want)
				}
			}
			if strings.Contains(row.Summary+row.Detail, "/home") || strings.Contains(row.Summary+row.Detail, "/tmp") {
				t.Errorf("row carries a path: %+v", row)
			}
			// Never a failure: a host that offers no sandbox is not a broken installation.
			if row.Status == statusFail {
				t.Errorf("the row fails the doctor: %+v", row)
			}
		})
	}
}

func TestSandboxHostRowSitsInTheRunAndNeverFailsIt(t *testing.T) {
	old := doctorSandboxHost
	defer func() { doctorSandboxHost = old }()
	doctorSandboxHost = func() *sandbox.HostReport {
		return &sandbox.HostReport{Platform: "linux", Mechanism: sandbox.MechanismLinux,
			Reason: sandbox.MechanismLinux + ": the kernel has no Landlock", Facts: linuxFacts("none: the kernel has no Landlock", "none listed; landlock is not among them", "not run")}
	}
	env := testDoctorEnv(nil)
	rows := runChecks(context.Background(), env)
	if row := rowByID(t, rows, "sandbox.host"); row.Status != statusWarn || !strings.Contains(row.Summary, "the kernel has no Landlock") {
		t.Errorf("through runChecks: %+v", row)
	}

	// A warning is not an exit code, and the row is in the JSON an operator's script reads.
	_, stdout, _ := runDoctorTest(nil, "--json", "--offline")
	var doc struct {
		Rows []doctorRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	found := false
	for _, r := range doc.Rows {
		if r.ID == "sandbox.host" {
			found = true
			if r.Status != statusWarn {
				t.Errorf("row = %+v", r)
			}
		}
	}
	if !found {
		t.Errorf("no sandbox.host row in %s", stdout)
	}
}

func TestSandboxHostIsNotComputedWhenSkipped(t *testing.T) {
	old := doctorSandboxHost
	defer func() { doctorSandboxHost = old }()
	doctorSandboxHost = func() *sandbox.HostReport {
		t.Error("the host check ran although the row was skipped")
		return &sandbox.HostReport{}
	}
	env := testDoctorEnv(nil)
	env.Skip["sandbox.host"] = true
	rows := runChecks(context.Background(), env)
	if row := rowByID(t, rows, "sandbox.host"); row.Status != statusSkip {
		t.Errorf("a skipped row: %+v", row)
	}
}
