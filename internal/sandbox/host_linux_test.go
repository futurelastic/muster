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

// fake seams: speak for a host this test is not running on.
func withHost(t *testing.T, goarch string, abi func() (int, error)) {
	t.Helper()
	prevABI, prevArch := hostABI, hostGOARCH
	hostABI = abi
	hostGOARCH = func() string { return goarch }
	t.Cleanup(func() { hostABI, hostGOARCH = prevABI, prevArch })
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
