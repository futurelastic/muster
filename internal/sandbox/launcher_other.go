//go:build !linux

package sandbox

import (
	"fmt"
	"os"
)

// LauncherMain is the body of `muster __sandbox-exec`. The launcher is the Linux
// mechanism's; on any other platform there is nothing to apply, and the program
// is not run.
func LauncherMain(args []string) int {
	fmt.Fprintln(os.Stderr, LauncherMessagePrefix+"the Linux sandbox launcher is not available on this platform")
	return LauncherRefusedStatus
}
