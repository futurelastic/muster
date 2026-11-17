package lowprio

import "syscall"

// The two constants are from <sys/resource.h>; package syscall does not export them.
const (
	prioDarwinThread = 3      // PRIO_DARWIN_THREAD: who == 0 is the calling thread
	prioDarwinBG     = 0x1000 // PRIO_DARWIN_BG: background class, CPU and disk I/O both
)

// lower puts the calling thread in the background class. It is per-thread, so it
// is only sound on a goroutine locked to its thread (see Go).
func lower() error {
	return syscall.Setpriority(prioDarwinThread, 0, prioDarwinBG)
}

// current reports whether the calling thread is in the background class.
func current() (bool, error) {
	p, err := syscall.Getpriority(prioDarwinThread, 0)
	return p != 0, err
}
