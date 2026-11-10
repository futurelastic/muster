//go:build darwin

package opencode

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// muster #433: one clonefileat(2) of a whole directory, reached without cgo or
// golang.org/x/sys, which this repo does not depend on.
//
// syscall.Syscall6 on darwin is a bare trap on both architectures (amd64 adds
// the BSD class, arm64 loads the number into R16 and runs SVC 0x80), so a
// literal number reaches the kernel as it would from C. The number is the one
// <sys/syscall.h> has carried since the call was introduced with APFS. It is not
// a documented interface, which is why this is a fast path and never the only
// one: copyTree falls back on any error, and a "success" is only believed after
// the destination is seen to be a directory. clone_test.go fails — it does not
// skip — if the host answers EINVAL or ENOSYS, so a macOS that moves the number
// is noticed by CI rather than by a slow create.
const (
	// 462 is clonefileat(2); 517 is fclonefileat(2), which takes a descriptor
	// and answers EINVAL to a path.
	sysClonefileat = 462

	// atFdcwd is AT_FDCWD (-2 on darwin): both paths are absolute here, so the
	// working directory is never consulted.
	atFdcwd = ^uintptr(1)

	// cloneNoFollow: a link at the top is cloned as a link, not followed.
	// cloneNoOwnerCopy: the clone belongs to the caller, as a plain `cp -R`
	// leaves it, instead of copying the seed's owner when the service is root.
	cloneNoFollow    = 0x0001
	cloneNoOwnerCopy = 0x0002
)

// cloneDirectory makes dst, which must not exist, a copy-on-write clone of the
// directory src as one operation. The cost is the kernel's walk of the tree, not
// one process and one call per file. The call cannot be interrupted once it has
// started; copyTree's contract for that is in its comment.
//
// It fails, leaving dst absent, when the filesystem cannot clone (ENOTSUP) or
// src and dst are on different volumes (EXDEV).
func cloneDirectory(src, dst string) error {
	s, err := syscall.BytePtrFromString(src)
	if err != nil {
		return err
	}
	d, err := syscall.BytePtrFromString(dst)
	if err != nil {
		return err
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(sysClonefileat,
			atFdcwd, uintptr(unsafe.Pointer(s)),
			atFdcwd, uintptr(unsafe.Pointer(d)),
			cloneNoFollow|cloneNoOwnerCopy, 0)
		if errno != syscall.EINTR {
			break
		}
	}
	if errno != 0 {
		return &os.PathError{Op: "clonefileat", Path: src, Err: errno}
	}
	if fi, err := os.Lstat(dst); err != nil || !fi.IsDir() {
		// The call said yes and what it made is not the directory that was asked
		// for. Not a state to build a session on.
		return fmt.Errorf("clonefileat: reported success but %q is not a directory", dst)
	}
	return nil
}

// hostClones: this host has a call that clones a directory (clonefileat, above).
func hostClones() bool { return true }
