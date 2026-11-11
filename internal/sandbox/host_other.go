//go:build !darwin

package sandbox

import (
	"fmt"
	"runtime"

	fleet "github.com/futurelastic/muster"
)

// HostSupport reports what the host can enforce, or nil and the reason when it
// cannot — a statement the capability carries, not an error.
//
// This platform has no mechanism: the only one implemented is macOS's
// sandbox-exec (host_darwin.go). The capability is therefore absent rather than
// offered weaker.
func HostSupport() (*fleet.SandboxSupport, string) {
	return nil, fmt.Sprintf("no sandbox mechanism is implemented for %s (only macOS's sandbox-exec is)", runtime.GOOS)
}
