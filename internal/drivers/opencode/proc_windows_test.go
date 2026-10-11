//go:build windows

package opencode

import (
	"os"
	"os/exec"

	"github.com/futurelastic/muster/internal/procgroup"
)

// alive reports whether a process with this pid exists and has not exited.
func alive(pid int) bool { return procgroup.Alive(pid) }

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

// shellCommand is the shell a tool the session ran would use.
func shellCommand(cmd string) *exec.Cmd { return exec.Command("cmd.exe", "/c", cmd) }

// longSleeper is a long-lived process found through the PATH the driver built:
// ping.exe lives in the system directory, which is on that PATH.
func longSleeper() *exec.Cmd { return exec.Command("ping", "-n", "600", "127.0.0.1") }

// platformSessionEnv are the variables a session also receives on this
// platform, beyond the ones every platform scopes.
var platformSessionEnv = []string{
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP",
	"SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "PATHEXT",
}

// absToolsDir is an absolute directory outside any profile, for a runtime path.
func absToolsDir() string { return `C:\opt\tools\bin` }

// simulateCrash makes the driver's servers go away as they do when the service
// dies. Each server is in a kill-on-close Job Object whose handle belongs to
// the service, so a crash closes it; closing it here, without Kill, is exactly
// that. It is also why reaping a stale server is a no-op on Windows.
func simulateCrash(d *Driver) {
	d.mu.Lock()
	servers := make([]*server, 0, len(d.live))
	for s := range d.live {
		servers = append(servers, s)
	}
	d.mu.Unlock()
	for _, s := range servers {
		if s.proc != nil {
			_ = s.proc.group.Close()
		}
	}
}
