//go:build unix

package opencode

import "syscall"

// alive reports whether a process with this pid exists.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// killProc kills one process outright.
func killProc(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

// killGroupOf kills a process group led by pid, ignoring one already gone.
func killGroupOf(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }
