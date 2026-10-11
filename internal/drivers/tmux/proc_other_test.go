//go:build !unix

package tmux

import "os"

// pidGone reports whether no process answers to pid any more. These tests drive
// a tmux binary and are skipped where there is none (ADR 303); this exists so
// the package's tests compile.
func pidGone(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	_ = p.Release()
	return false
}
