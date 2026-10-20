//go:build windows

package modclient

import (
	"fmt"
	"os"
	"strings"

	"github.com/futurelastic/muster/internal/privfs"
)

// modeProblem judges a module file by its access list. Go reports a made-up mode
// on Windows (it never carries an execute bit, and "writable by others" is an
// ACL, not a bit), so the POSIX test would refuse every module and any other
// reading of the mode would be a guess. What this asks instead is the question
// the POSIX test stands for: could anyone but the service's own account, or the
// machine's own accounts (system, administrators), rewrite the file? Anyone who
// could would run code as the service. Whether the process may be created at all
// is decided by the operating system at exec time, as before.
//
// An access list that cannot be read or understood refuses the module: this
// check guards code that is about to be run, so "unknown" is not a pass.
//
// Not checked here, and therefore the install's job: the modules directory. A
// writable directory lets its owner swap a file even when the file is tight, and
// its default list under a per-machine install directory does not allow that.
func modeProblem(path string, _ os.FileMode) string {
	who, err := privfs.WritableByOthers(path)
	switch {
	case err != nil:
		return "its access list could not be read (" + err.Error() + ")"
	case len(who) > 0:
		return fmt.Sprintf("writable by others (%s)", strings.Join(who, ", "))
	}
	return ""
}
