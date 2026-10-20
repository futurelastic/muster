//go:build !windows

package modclient

import (
	"fmt"
	"os"
)

// modeProblem judges a module file by its POSIX mode: it must be executable by
// its owner, and writable by nobody but its owner, because anyone who could
// rewrite it could run code as the daemon's user.
func modeProblem(_ string, mode os.FileMode) string {
	switch {
	case mode.Perm()&0o100 == 0:
		return "not executable by its owner"
	case mode.Perm()&0o022 != 0:
		return fmt.Sprintf("group- or world-writable (mode %04o)", mode.Perm())
	}
	return ""
}
