package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fake seams: speak for a host this test is not running on. The active security
// modules default to the usual list with landlock in it; withModules says another.
func withHost(t *testing.T, goarch string, abi func() (int, error)) {
	t.Helper()
	prevABI, prevArch, prevLSM := hostABI, hostGOARCH, hostLSM
	hostABI = abi
	hostGOARCH = func() string { return goarch }
	hostLSM = func() ([]string, error) { return []string{"lockdown", "capability", "landlock", "yama"}, nil }
	t.Cleanup(func() { hostABI, hostGOARCH, hostLSM = prevABI, prevArch, prevLSM })
}

func withModules(t *testing.T, modules []string, err error) {
	t.Helper()
	prev := hostLSM
	hostLSM = func() ([]string, error) { return modules, err }
	t.Cleanup(func() { hostLSM = prev })
}

func abiOf(n int) func() (int, error)    { return func() (int, error) { return n, nil } }
func abiErr(e error) func() (int, error) { return func() (int, error) { return 0, e } }

func refusal(t *testing.T) string {
	t.Helper()
	sup, why := HostSupport()
	if sup != nil {
		t.Fatalf("the host was offered the mechanism: %+v", sup)
	}
	if !strings.HasPrefix(why, MechanismLinux+": ") {
		t.Fatalf("a refusal that does not name its mechanism: %q", why)
	}
	return why
}

func TestHostSupportRefusesAnArchitectureNobodyMeasured(t *testing.T) {
	withLauncher(t, self(t))
	withHost(t, "riscv64", abiOf(7))
	if why := refusal(t); !strings.Contains(why, "riscv64") {
		t.Errorf("reason = %q", why)
	}
}

func TestHostSupportRefusesWithoutALauncher(t *testing.T) {
	withLauncher(t, "")
	withHost(t, "amd64", abiOf(7))
	if why := refusal(t); !strings.Contains(why, "launcher") {
		t.Errorf("reason = %q", why)
	}
}

func TestHostSupportSaysWhyTheKernelHasNoLandlock(t *testing.T) {
	withLauncher(t, self(t))
	withHost(t, "amd64", abiErr(syscall.ENOSYS))
	if why := refusal(t); !strings.Contains(why, "no Landlock") {
		t.Errorf("ENOSYS reason = %q", why)
	}
	withHost(t, "amd64", abiErr(syscall.EOPNOTSUPP))
	if why := refusal(t); !strings.Contains(why, "not enabled") || !strings.Contains(why, "lsm=") {
		t.Errorf("EOPNOTSUPP reason = %q", why)
	}
	withHost(t, "amd64", abiErr(syscall.EPERM))
	if why := refusal(t); !strings.Contains(why, "operation not permitted") {
		t.Errorf("other errors are reported as they are: %q", why)
	}
}

func TestHostSupportNamesTheFloorWhenTheKernelIsBelowIt(t *testing.T) {
	withLauncher(t, self(t))
	for _, abi := range []int{1, 4, 5} {
		withHost(t, "amd64", abiOf(abi))
		why := refusal(t)
		if !strings.Contains(why, fmt.Sprintf("ABI %d", abi)) || !strings.Contains(why, "6.12") {
			t.Errorf("ABI %d reason = %q", abi, why)
		}
	}
}

// fakeLauncher is a launcher that is not the muster binary.
func fakeLauncher(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "launcher")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostSupportRefusesWhenTheConfinedSelfTestFails(t *testing.T) {
	withHost(t, "amd64", abiOf(7))
	withLauncher(t, fakeLauncher(t, "echo 'FAIL: reading a file outside the grants: got <nil>'; exit 125"))
	why := refusal(t)
	if !strings.Contains(why, "self-test") || !strings.Contains(why, "reading a file outside the grants") {
		t.Errorf("reason = %q", why)
	}
	// A launcher that is not the muster binary says nothing the check recognises.
	withLauncher(t, fakeLauncher(t, "exit 0"))
	if why := refusal(t); !strings.Contains(why, "self-test") {
		t.Errorf("reason = %q", why)
	}
}

func TestHostSupportRefusesWhenTheSelfTestDoesNotFinish(t *testing.T) {
	withHost(t, "amd64", abiOf(7))
	withLauncher(t, fakeLauncher(t, "exec sleep 5"))
	prev := selfTestTimeout
	selfTestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { selfTestTimeout = prev })
	if why := refusal(t); !strings.Contains(why, "did not finish") {
		t.Errorf("reason = %q", why)
	}
}

func TestHostSupportOffersTheMechanismOnlyAfterARealConfinedSelfTestPasses(t *testing.T) {
	requireABI(t, MinLandlockABI)
	withLauncher(t, self(t))
	sup, why := HostSupport()
	if sup == nil {
		t.Fatalf("a host with Landlock ABI %d refused: %s", landlockHostABI(t), why)
	}
	if sup.Mechanism != MechanismLinux || len(sup.Denies) != 3 || len(sup.Network) != 2 {
		t.Errorf("support = %+v", sup)
	}
	if len(sup.PackageCache) != 0 || len(sup.PackageManagers) != 0 {
		t.Errorf("the shared host check names a package cache: %+v", sup)
	}
}

func TestHostSupportOnAKernelBelowTheFloorRefusesWithoutRunningAnything(t *testing.T) {
	if landlockHostABI(t) >= MinLandlockABI {
		t.Skip("this host's kernel is at or above the floor")
	}
	withLauncher(t, self(t))
	why := refusal(t)
	if !strings.Contains(why, "ABI") && !strings.Contains(why, "Landlock") {
		t.Errorf("reason = %q", why)
	}
}

// muster #457 — the report `muster doctor` shows. The refusals above are what the
// capability says; these are what the same check says it saw.

func factOf(t *testing.T, rep *HostReport, name string) string {
	t.Helper()
	v := rep.Fact(name)
	if v == "" {
		t.Fatalf("the report has no %q fact: %+v", name, rep.Facts)
	}
	return v
}

// everyFact is the facts a Linux report always carries, in the order they are made.
var everyFact = []string{FactArchitecture, FactLauncher, FactLandlockABI, FactModules, FactSelfTest}

func TestCheckHostNamesEveryFactInOrderWhatEverItRefuses(t *testing.T) {
	cases := []struct {
		name     string
		launcher string
		arch     string
		abi      func() (int, error)
	}{
		{"an unmeasured architecture", self(t), "riscv64", abiOf(7)},
		{"no launcher", "", "amd64", abiOf(7)},
		{"no Landlock", self(t), "amd64", abiErr(syscall.ENOSYS)},
		{"Landlock not enabled", self(t), "amd64", abiErr(syscall.EOPNOTSUPP)},
		{"a kernel below the floor", self(t), "amd64", abiOf(5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLauncher(t, tc.launcher)
			withHost(t, tc.arch, tc.abi)
			rep := CheckHost()
			if rep.Support != nil || rep.Reason == "" || rep.Mechanism != MechanismLinux {
				t.Fatalf("a refused host reported %+v", rep)
			}
			if len(rep.Facts) != len(everyFact) {
				t.Fatalf("facts = %+v, want one each of %v", rep.Facts, everyFact)
			}
			for i, name := range everyFact {
				if rep.Facts[i].Name != name || rep.Facts[i].Value == "" {
					t.Errorf("fact %d = %+v, want %q", i, rep.Facts[i], name)
				}
			}
			// The test starts nothing when an earlier check refused.
			if v := rep.Fact(FactSelfTest); !strings.HasPrefix(v, "not run") {
				t.Errorf("self-test = %q", v)
			}
		})
	}
}

func TestCheckHostSaysTheFloorAgainstTheABIItFound(t *testing.T) {
	withLauncher(t, self(t))
	withHost(t, "amd64", abiOf(5))
	abi := factOf(t, CheckHost(), FactLandlockABI)
	for _, want := range []string{"5", "below the 6", "Linux 6.12"} {
		if !strings.Contains(abi, want) {
			t.Errorf("ABI fact %q does not say %q", abi, want)
		}
	}
	// And one at the floor says it meets it, without the word below.
	withHost(t, "amd64", abiOf(7))
	abi = factOf(t, CheckHost(), FactLandlockABI)
	if !strings.Contains(abi, "7") || !strings.Contains(abi, "6 or newer") || strings.Contains(abi, "below") {
		t.Errorf("ABI fact = %q", abi)
	}
}

func TestCheckHostGivesEachKernelAnswerAboutLandlockItsOwnFact(t *testing.T) {
	withLauncher(t, self(t))
	for _, tc := range []struct {
		err  error
		want string
	}{
		{syscall.ENOSYS, "no Landlock"},
		{syscall.EOPNOTSUPP, "not enabled"},
		{syscall.EPERM, "operation not permitted"},
	} {
		withHost(t, "amd64", abiErr(tc.err))
		rep := CheckHost()
		if v := factOf(t, rep, FactLandlockABI); !strings.Contains(v, tc.want) {
			t.Errorf("%v: ABI fact = %q, want %q", tc.err, v, tc.want)
		}
		if !strings.Contains(rep.Reason, tc.want) {
			t.Errorf("%v: reason = %q, want %q", tc.err, rep.Reason, tc.want)
		}
	}
}

func TestCheckHostListsTheActiveModulesAndSaysWhenLandlockIsNotOneOfThem(t *testing.T) {
	withLauncher(t, self(t))
	withHost(t, "amd64", abiErr(syscall.EOPNOTSUPP))

	withModules(t, []string{"lockdown", "capability", "landlock"}, nil)
	v := factOf(t, CheckHost(), FactModules)
	if v != "lockdown, capability, landlock" {
		t.Errorf("modules = %q", v)
	}

	withModules(t, []string{"lockdown", "capability", "yama"}, nil)
	v = factOf(t, CheckHost(), FactModules)
	if !strings.Contains(v, "lockdown, capability, yama") || !strings.Contains(v, "landlock is not among them") {
		t.Errorf("modules = %q", v)
	}

	withModules(t, nil, nil)
	if v := factOf(t, CheckHost(), FactModules); !strings.Contains(v, "none listed") || !strings.Contains(v, "landlock is not among them") {
		t.Errorf("an empty list = %q", v)
	}

	// A kernel with no securityfs mounted (a container) cannot say, and that decides
	// nothing: the kernel's own answer about Landlock is what the host is judged by.
	withModules(t, nil, os.ErrNotExist)
	rep := CheckHost()
	if v := factOf(t, rep, FactModules); !strings.Contains(v, "cannot be read") {
		t.Errorf("an unreadable list = %q", v)
	}
	if !strings.Contains(rep.Reason, "not enabled") {
		t.Errorf("reason = %q: an unreadable module list changed the refusal", rep.Reason)
	}
}

func TestReadActiveModulesSplitsTheKernelsLine(t *testing.T) {
	prev := activeModulesFile
	t.Cleanup(func() { activeModulesFile = prev })

	activeModulesFile = filepath.Join(t.TempDir(), "lsm")
	if err := os.WriteFile(activeModulesFile, []byte("lockdown,capability,landlock,yama,apparmor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readActiveModules()
	if err != nil || strings.Join(got, "|") != "lockdown|capability|landlock|yama|apparmor" {
		t.Errorf("modules = %v, err = %v", got, err)
	}

	if err := os.WriteFile(activeModulesFile, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readActiveModules(); err != nil || len(got) != 0 {
		t.Errorf("an empty file = %v, %v", got, err)
	}

	activeModulesFile = filepath.Join(t.TempDir(), "absent")
	if _, err := readActiveModules(); err == nil {
		t.Error("a file that is not there read without an error")
	}
}

func TestCheckHostRunsTheSelfTestOnlyWhenNothingEarlierRefused(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	launcher := fakeLauncher(t, "touch "+marker+"; echo ok")

	// Below the floor: the launcher must not be started.
	withHost(t, "amd64", abiOf(5))
	withLauncher(t, launcher)
	CheckHost()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the self-test was started on a kernel that is below the floor")
	}

	// At the floor it is, and what it says is the fact — and the reason.
	withHost(t, "amd64", abiOf(7))
	withLauncher(t, fakeLauncher(t, "echo 'FAIL: reading a file outside the grants: got <nil>'; exit 125"))
	rep := CheckHost()
	if v := factOf(t, rep, FactSelfTest); !strings.Contains(v, "reading a file outside the grants") {
		t.Errorf("self-test fact = %q", v)
	}
	if !strings.HasSuffix(rep.Reason, rep.Fact(FactSelfTest)) {
		t.Errorf("the fact and the reason disagree:\nfact   %q\nreason %q", rep.Fact(FactSelfTest), rep.Reason)
	}

	withLauncher(t, launcher)
	rep = CheckHost()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the self-test was not started: %v", err)
	}
	// A launcher that answers "ok" passes the self-test, which is all it asks of one:
	// the check cannot tell it from the muster binary, and does not need to.
	if rep.Support == nil || !strings.HasPrefix(rep.Fact(FactSelfTest), "passed") {
		t.Errorf("a launcher that answered ok: %+v", rep)
	}
}

// The capability and the row read one report: for every state a host can be in,
// what HostSupport answers is exactly what CheckHost found.
func TestHostSupportIsTheAnswerOfTheReport(t *testing.T) {
	states := []struct {
		name     string
		launcher string
		arch     string
		abi      func() (int, error)
	}{
		{"an unmeasured architecture", self(t), "riscv64", abiOf(7)},
		{"no launcher", "", "amd64", abiOf(7)},
		{"no Landlock", self(t), "amd64", abiErr(syscall.ENOSYS)},
		{"Landlock not enabled", self(t), "amd64", abiErr(syscall.EOPNOTSUPP)},
		{"another error", self(t), "amd64", abiErr(syscall.EPERM)},
		{"below the floor", self(t), "amd64", abiOf(4)},
		{"a failing self-test", fakeLauncher(t, "echo FAIL; exit 125"), "amd64", abiOf(7)},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			withLauncher(t, st.launcher)
			withHost(t, st.arch, st.abi)
			rep := CheckHost()
			sup, why := HostSupport()
			if sup != nil || rep.Support != nil {
				t.Fatalf("a refused host was offered the mechanism: %+v / %+v", sup, rep.Support)
			}
			if why != rep.Reason || why == "" {
				t.Errorf("HostSupport reason %q, report reason %q", why, rep.Reason)
			}
		})
	}
}

func TestCheckHostOffersTheMechanismWithEveryFactSaidAfterARealSelfTest(t *testing.T) {
	requireABI(t, MinLandlockABI)
	withLauncher(t, self(t))
	rep := CheckHost()
	if rep.Support == nil {
		t.Fatalf("a host with Landlock ABI %d refused: %s", landlockHostABI(t), rep.Reason)
	}
	if rep.Reason != "" || rep.Platform != "linux" {
		t.Errorf("report = %+v", rep)
	}
	if v := factOf(t, rep, FactSelfTest); !strings.HasPrefix(v, "passed") {
		t.Errorf("self-test = %q", v)
	}
	if len(rep.Facts) != len(everyFact) {
		t.Errorf("facts = %+v", rep.Facts)
	}
	sup, why := HostSupport()
	if sup == nil || why != "" {
		t.Errorf("HostSupport disagrees with a report that offers the mechanism: %+v %q", sup, why)
	}
}
