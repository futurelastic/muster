//go:build unix

package opencode

import (
	"os/exec"
	"syscall"
)

// alive reports whether a process with this pid exists.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// killProc kills one process outright.
func killProc(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

// killGroupOf kills a process group led by pid, ignoring one already gone.
func killGroupOf(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// shellCommand is the shell a tool the session ran would use.
func shellCommand(cmd string) *exec.Cmd { return exec.Command("/bin/sh", "-c", cmd) }

// longSleeper is a long-lived process found through the PATH the driver built.
func longSleeper() *exec.Cmd { return exec.Command("sleep", "600") }

// platformSessionEnv are the variables a session also receives on this
// platform, beyond the ones every platform scopes.
var platformSessionEnv []string

// absToolsDir is an absolute directory outside any home, for a runtime path.
func absToolsDir() string { return "/opt/tools/bin" }

// simulateCrash makes the driver's servers survive it as they would a crash of
// the service: on Unix a process group outlives its parent, so nothing to do.
func simulateCrash(d *Driver) {}
