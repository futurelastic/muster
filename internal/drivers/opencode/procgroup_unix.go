//go:build unix

package opencode

import (
	"os/exec"
	"syscall"
)

// groupsSupported says whether this platform can put a server in its own
// process group and signal the group. The driver refuses to start without it:
// closing a session must not leave its tools running.
const groupsSupported = true

// configureGroup makes the child the leader of a new process group.
func configureGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the child's whole process group, ignoring ESRCH
// (an already-empty group).
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
