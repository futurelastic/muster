package sandbox

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	fleet "github.com/futurelastic/muster"
)

const (
	prSetSeccomp      = 22
	seccompModeFilter = 2
)

// sockFprog is struct sock_fprog: a count, padding to the pointer's alignment,
// and the program. A Go struct has exactly this layout on the 64-bit
// architectures the mechanism is offered on, and the pointer keeps the program
// reachable for the collector while the kernel copies it.
type sockFprog struct {
	length uint16
	_      [6]byte
	filter *byte
}

// installSeccomp attaches the filter for this architecture and posture to the
// calling thread. no_new_privs is already set by applyLandlock, which the kernel
// requires of an unprivileged caller.
func installSeccomp(network fleet.SandboxNetwork) error {
	table, ok := TableFor(runtime.GOARCH)
	if !ok {
		return fmt.Errorf("architecture %s has no syscall table", runtime.GOARCH)
	}
	prog, err := SeccompProgram(table, network)
	if err != nil {
		return err
	}
	fprog := sockFprog{length: uint16(len(prog) / 8), filter: &prog[0]}
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&fprog)), 0, 0, 0); e != 0 {
		return fmt.Errorf("installing the seccomp filter: %w", e)
	}
	runtime.KeepAlive(prog)
	return nil
}
