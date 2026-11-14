package opencode

import (
	"errors"
	"io/fs"
	"os/exec"
	"syscall"
)

// The steps of one start attempt that run before the runtime does: what
// spawnServer does, in order. A failure of any of them is a spawnError carrying
// one of these, and startFailure answers it (muster #479).
const (
	spawnStepPort       = "choosing a port"
	spawnStepCredential = "generating a credential"
	spawnStepSandbox    = "preparing the sandbox"
	spawnStepStart      = "starting the process"
	spawnStepAttach     = "putting the process under control"
)

// spawnError is a start attempt that failed before the runtime ran: no port or
// credential, a sandbox prefix that would not build, an exec that did not happen,
// a child that could not be put under control. It is a marker, so that
// startFailure can tell these four apart from every other plain error a start
// returns and answer them as what they are, instead of as the caller's mistake
// (the default for an error nothing classified is 400 `invalid`).
//
// Error is the cause's own text, as it always was: it names the binary's path and
// carries os/exec's own wording, which is useful to whoever reads a log and is
// exactly what must not reach a caller. startFailure never copies it into an
// answer.
type spawnError struct {
	step string
	err  error
}

func (e *spawnError) Error() string { return e.err.Error() }
func (e *spawnError) Unwrap() error { return e.err }

// spawnFailed marks err as the failure of step.
func spawnFailed(step string, err error) error { return &spawnError{step: step, err: err} }

// lasting reports whether the spawn failed for a cause that stays until somebody
// does something on the machine: the executable (or the launcher in front of it,
// or the directory it starts in) is not there, may not be run, or is not
// something the system can execute. Asking again meets the same, so the answer is
// not retryable.
//
// Only the exec can say so. The sandbox prefix is rendered once before any
// process exists (launcher(), muster #281), and a plan that cannot be written
// down is refused there as invalid; the per-attempt render in spawnServer is the
// same plan with the one port it may bind, so its failing after that render
// passed is not about the request or the profile, and is not a cause known to
// last. The port, the credential and the attach have no cause that waits on
// somebody's hands either.
//
// It is an allow-list on purpose, and the default is the other way. A cause
// nobody recognised is answered retryable: answering a transient failure as final
// is the defect muster #477 fixed (a client that reads it as final stops asking),
// while answering a lasting one as retryable costs a client one bounded retry.
// E2BIG is left out for that reason too: it depends on the request's own
// environment, which is not what this answer is about.
func (e *spawnError) lasting() bool {
	return e.step == spawnStepStart &&
		(errors.Is(e.err, fs.ErrNotExist) ||
			errors.Is(e.err, fs.ErrPermission) ||
			errors.Is(e.err, exec.ErrNotFound) ||
			errors.Is(e.err, syscall.ENOEXEC) ||
			errors.Is(e.err, syscall.ENOTDIR))
}

// cause is what a caller is told of why a spawn failed, in the words of a fixed
// vocabulary and never the cause's own text, which carries the executable's path.
// What an operator needs from it is kept: that the executable is missing, that it
// may not be run, or that the machine is out of something.
func (e *spawnError) cause() string {
	if e.step == spawnStepStart {
		switch {
		case errors.Is(e.err, fs.ErrPermission), errors.Is(e.err, syscall.ENOEXEC):
			return "the runtime's executable may not be run here"
		case errors.Is(e.err, fs.ErrNotExist), errors.Is(e.err, exec.ErrNotFound), errors.Is(e.err, syscall.ENOTDIR):
			return "the runtime's executable, its sandbox launcher or the session's working directory is missing"
		}
	}
	switch {
	case errors.Is(e.err, syscall.EAGAIN), errors.Is(e.err, syscall.ENOMEM):
		return "the machine had no process or memory to spare"
	case errors.Is(e.err, syscall.EMFILE), errors.Is(e.err, syscall.ENFILE):
		return "the machine had no file descriptor to spare"
	default:
		return "the machine could not do it now"
	}
}
