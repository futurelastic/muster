//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// CheckHost looks at what the host can enforce and reports it (muster #457);
// HostSupport is its answer and `muster doctor` shows its facts.
//
// It actually applies a profile once: a service that itself runs inside a
// sandbox cannot start a nested one, and discovering that at the first create
// would refuse a caller for a reason the capability should have told it.
//
// The returned support names no package-cache mode: that is a decision of the
// driver, which fills it in (or leaves it empty).
func CheckHost() *HostReport {
	rep := newHostReport(Mechanism)
	if _, err := os.Stat(ExecPath); err != nil {
		rep.add(FactBinary, "not present")
		rep.add(FactProbe, "not run: there is no sandbox-exec to apply one with")
		rep.refuse(ExecPath + " is not present")
		return rep
	}
	rep.add(FactBinary, "present")
	out, err := exec.Command(ExecPath, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
	if err != nil {
		why := fmt.Sprintf("the host refused to apply a sandbox profile (%s): the service may itself be running inside one, which cannot be nested",
			strings.TrimSpace(string(out)))
		rep.add(FactProbe, "%s", why)
		rep.refuse(why)
		return rep
	}
	rep.add(FactProbe, "yes (a profile was applied to a throwaway process, so a session's can be)")
	rep.Support = &fleet.SandboxSupport{
		Mechanism:    Mechanism,
		Denies:       Denies(),
		Network:      []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
		PackageCache: []fleet.PackageCacheMode{},
		// muster #432: a driver that starts sessions from a package cache says
		// which managers it points at one; the default is none.
		PackageManagers: []fleet.PackageManager{},
	}
	return rep
}
