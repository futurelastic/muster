//go:build windows

package opencode

// reapStale has nothing to stop on Windows, by invariant rather than by
// omission. Every server this driver starts is assigned to a Job Object created
// with kill-on-close (internal/procgroup), and the job's handle belongs to the
// service process. When the service ends — cleanly, by crash or by being
// killed — the kernel closes that handle and terminates everything in the job,
// so the next service never finds a server its predecessor left running.
//
// The Unix half exists because a process group survives its parent; a job does
// not. (#303, #306)
func reapStale(r *sessionRecord) bool { return false }
