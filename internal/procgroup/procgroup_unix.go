//go:build unix

package procgroup

import (
	"errors"
	"os/exec"
	"syscall"
)

// Group is a started child and the process group it leads.
type Group struct {
	pid int
}

// Supported reports whether this platform can stop a child's whole tree.
func Supported() bool { return true }

// Prepare makes the child the leader of a new process group. Call it before
// Start.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Attach binds the started child to a Group. It cannot fail here once the
// process exists: the group was created by the fork itself.
func Attach(cmd *exec.Cmd) (*Group, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("procgroup: the command has not been started")
	}
	return &Group{pid: cmd.Process.Pid}, nil
}

// Graceful reports whether Interrupt does anything. It does here: SIGTERM.
func (g *Group) Graceful() bool { return g != nil }

// Interrupt asks every process in the group to stop (SIGTERM). An already
// empty group is not an error.
func (g *Group) Interrupt() error {
	if g == nil || g.pid <= 0 {
		return nil
	}
	return ignoreGone(syscall.Kill(-g.pid, syscall.SIGTERM))
}

// Kill ends every process in the group (SIGKILL). An already empty group is
// not an error. It is safe to call at any time and more than once, which is
// why the group is signalled even after its leader has exited: a tool can
// outlive the process it ran under.
func (g *Group) Kill() error {
	if g == nil || g.pid <= 0 {
		return nil
	}
	return ignoreGone(syscall.Kill(-g.pid, syscall.SIGKILL))
}

// Close releases what the Group holds. Nothing is held on Unix.
func (g *Group) Close() error { return nil }

// Alive reports whether a process with this id exists. A zombie that has not
// been waited for still counts, as it does for kill(2).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ignoreGone treats a group with nothing left to signal as success. That is
// ESRCH everywhere, and also EPERM on macOS, which answers a signal to a group
// holding only exiting processes that way: we created the group, so a refusal
// to signal it can only mean its members are already on their way out.
func ignoreGone(err error) error {
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
