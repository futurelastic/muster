//go:build !unix && !windows

package procgroup

import (
	"errors"
	"os"
	"os/exec"
)

// Group is a started child. Without process groups it can only name the child
// itself.
type Group struct {
	proc *os.Process
}

// Supported is false here: there is no way to stop a child's descendants, so a
// caller that must not leave tools running refuses to start.
func Supported() bool { return false }

// Prepare does nothing on this platform.
func Prepare(cmd *exec.Cmd) {}

// Attach binds the started child to a Group.
func Attach(cmd *exec.Cmd) (*Group, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("procgroup: the command has not been started")
	}
	return &Group{proc: cmd.Process}, nil
}

// Graceful is false: there is nothing to deliver a gentle stop through.
func (g *Group) Graceful() bool { return false }

// Interrupt is not available here.
func (g *Group) Interrupt() error { return errors.ErrUnsupported }

// Kill ends the child itself; its descendants are out of reach here.
func (g *Group) Kill() error {
	if g == nil || g.proc == nil {
		return nil
	}
	return g.proc.Kill()
}

// Close releases nothing.
func (g *Group) Close() error { return nil }

// Alive cannot be answered here and reports false.
func Alive(pid int) bool { return false }
