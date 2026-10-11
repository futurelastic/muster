//go:build windows

package opencode

import (
	"os"
	"syscall"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited.
const stillActive = 259

// alive reports whether a process with this pid exists and has not exited.
func alive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// killProc kills one process outright.
func killProc(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// killGroupOf has no process group to address here; it kills the process.
func killGroupOf(pid int) { _ = killProc(pid) }
