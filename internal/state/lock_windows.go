//go:build windows

package state

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// The lock is byte 0. The owner's record starts after it (recordAt), because a
// byte-range lock on Windows stops other handles reading the bytes it covers and the
// process that is refused has to read the record.
const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2

	errorLockViolation = syscall.Errno(33)
	errorIOPending     = syscall.Errno(997)
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// lockFile takes the byte exclusively for this handle with LockFileEx, without
// waiting. The system drops it when the handle closes or the process ends.
func lockFile(f *os.File) error {
	ol := syscall.Overlapped{}
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) || errors.Is(err, errorIOPending) {
		return errHeld
	}
	return err
}

// unlockFile gives the byte up at once; closing the handle alone leaves the system
// to do it when it gets to it.
func unlockFile(f *os.File) error {
	ol := syscall.Overlapped{}
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	return err
}
