//go:build !darwin && !linux

package sandbox

import (
	"fmt"
	"runtime"
)

// CheckHost reports that this platform has nothing to measure (muster #457);
// HostSupport is its answer and `muster doctor` shows it as not applicable.
//
// This platform has no mechanism: the only ones implemented are macOS's
// sandbox-exec (host_darwin.go) and Linux's Landlock and seccomp
// (host_linux.go). The capability is therefore absent rather than offered
// weaker.
func CheckHost() *HostReport {
	rep := newHostReport("")
	rep.refuse(fmt.Sprintf("no sandbox mechanism is implemented for %s (only macOS's sandbox-exec and Linux's Landlock and seccomp are)", runtime.GOOS))
	return rep
}
