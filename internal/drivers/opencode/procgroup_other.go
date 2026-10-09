//go:build !unix

package opencode

import (
	"os/exec"
	"syscall"
)

// groupsSupported is false off unix: there is no process group to signal, so
// New refuses to start rather than offer a Close that leaves tools running.
const groupsSupported = false

func configureGroup(cmd *exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
