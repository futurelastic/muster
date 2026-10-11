//go:build !unix

package tmux

import "os"

// killPID kills one process where signals by number do not exist. The
// compatibility harness is only ever pointed at a tmux binary, which has no
// native build here (ADR 303), so this exists to keep the package compiling.
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
