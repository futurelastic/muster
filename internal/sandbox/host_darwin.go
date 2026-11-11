//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// HostSupport reports what the host can enforce, or nil and the reason when it
// cannot — a statement the capability carries, not an error.
//
// It actually applies a profile once: a service that itself runs inside a
// sandbox cannot start a nested one, and discovering that at the first create
// would refuse a caller for a reason the capability should have told it.
//
// The returned support names no package-cache mode: that is a decision of the
// driver, which fills it in (or leaves it empty).
func HostSupport() (*fleet.SandboxSupport, string) {
	if _, err := os.Stat(ExecPath); err != nil {
		return nil, ExecPath + " is not present"
	}
	out, err := exec.Command(ExecPath, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
	if err != nil {
		return nil, fmt.Sprintf("the host refused to apply a sandbox profile (%s): the service may itself be running inside one, which cannot be nested",
			strings.TrimSpace(string(out)))
	}
	return &fleet.SandboxSupport{
		Mechanism:    Mechanism,
		Denies:       Denies(),
		Network:      []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
		PackageCache: []fleet.PackageCacheMode{},
		// muster #432: a driver that starts sessions from a package cache says
		// which managers it points at one; the default is none.
		PackageManagers: []fleet.PackageManager{},
	}, ""
}
