//go:build unix

package opencode

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reapStale stops a server the PREVIOUS service left running, so a relaunch
// does not put two processes on one session database. A recorded pid is only
// acted on when the process answering to it is demonstrably the same server —
// the recorded binary serving the recorded port — because a pid alone may now
// belong to anything. Returns whether it stopped one.
func reapStale(r *sessionRecord) bool {
	if r.Pid <= 1 || r.Port == 0 {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(r.Pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	cmdline := string(out)
	if !strings.Contains(cmdline, " serve ") || !strings.Contains(cmdline, "--port "+strconv.Itoa(r.Port)) {
		return false
	}
	if r.Bin != "" && !strings.Contains(cmdline, r.Bin) {
		return false
	}
	_ = syscall.Kill(-r.Pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(r.Pid, 0) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-r.Pid, syscall.SIGKILL)
	return true
}
