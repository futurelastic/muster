//go:build unix

package tmux

import "syscall"

// killPID sends SIGKILL to one process. The compatibility harness uses it on a
// straggler it has already verified is the process it started.
func killPID(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
