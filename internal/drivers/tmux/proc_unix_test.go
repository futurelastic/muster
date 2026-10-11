//go:build unix

package tmux

import "syscall"

// pidGone reports whether no process answers to pid any more.
func pidGone(pid int) bool { return syscall.Kill(pid, 0) == syscall.ESRCH }
