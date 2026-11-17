// Package lowprio runs a function at the host's lowest scheduling priority.
//
// Go's standard library has no portable way to lower the priority of one
// goroutine: the scheduler moves goroutines between threads freely, and
// setpriority acts on a thread. The one thing that does work is to pin the
// goroutine to a thread, lower that thread, and let the thread die with the
// goroutine — which is what runtime.LockOSThread's contract gives: a goroutine
// that exits without unlocking takes its thread with it, so the lowered
// priority never reaches a goroutine that did not ask for it.
//
// It exists for work that has to happen but should never compete with what the
// machine is for: deleting a closed session's directory (muster #436) and
// measuring a session's scratch space. Both are many small file system calls,
// and on a busy host the cost of those is mostly the other processes' reaction
// to them — which a lower priority softens but does not remove, so callers also
// pace themselves.
package lowprio

import "runtime"

// Go starts fn on a new goroutine at background priority, where the platform
// has one (macOS: the background class, which also throttles the thread's disk
// I/O; Linux: nice 19). Where it has none, or the call is refused, fn still runs,
// at normal priority: a lower priority is a courtesy, never a condition.
func Go(fn func()) {
	go func() {
		runtime.LockOSThread()
		if lower() != nil {
			// The thread is still at normal priority, so it may go back to the
			// pool; returning locked is what retires a lowered one.
			runtime.UnlockOSThread()
		}
		fn()
	}()
}
