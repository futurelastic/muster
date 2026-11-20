//go:build unix

package state

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes the whole file for this open file description with flock(2),
// without waiting. A description is not inherited by a process this one starts: Go
// opens files close-on-exec, so a server or a pane this service launches does not
// keep the directory owned after the service has gone.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errHeld
		default:
			return err
		}
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
