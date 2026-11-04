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

// scanServers finds nothing to sweep for the same reason (muster #398): a server
// cannot outlive the service here, so there is no orphan to look for, and the
// doctor row is skipped on the platform's ErrScanUnsupported.
func scanServers(root string) ([]serverProc, int, error) { return nil, 0, ErrScanUnsupported }

// inspectServer finds nothing: there is no orphan to look at twice.
func inspectServer(pid int, root string) (serverProc, bool) { return serverProc{}, false }

// stopGroups stops nothing: scanServers never finds anything to stop.
func stopGroups(pids []int) []int { return nil }

// stopStrays stops nothing, for the same reason (muster #405): every process a
// session's tools start is in the job the server was assigned to, which the kernel
// terminates as a whole, so there is no process that left it to look for.
func stopStrays(root, only string) (stopped, left []strayProc, err error) {
	return nil, nil, ErrScanUnsupported
}
