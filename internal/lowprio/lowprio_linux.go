package lowprio

import "syscall"

// lower sets the calling thread's nice value to 19. On Linux the nice value is
// an attribute of the thread and PRIO_PROCESS takes a thread id, so naming this
// thread's own leaves the rest of the process at its priority. It is only sound on
// a goroutine locked to its thread (see Go).
func lower() error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), 19)
}

// current reports whether the calling thread's nice value is above the default.
func current() (bool, error) {
	// getpriority(2) returns 20-nice through the raw call; package syscall hands
	// that value back as it is.
	p, err := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Gettid())
	return p != 0 && p < 20, err
}
