//go:build !unix && !windows

package opencode

// reapStale has nothing to stop off unix. Reaping a server a previous service
// left behind needs a process table query and a signal to the server's process
// group, neither of which exists here; and off unix the driver refuses to start
// anyway (procgroup.Supported is false there), so no server this
// package launched is ever left running to be found. (#303)
func reapStale(r *sessionRecord) bool { return false }

// scanServers has no process table to read here (muster #398).
func scanServers(root string) ([]serverProc, int, error) { return nil, 0, ErrScanUnsupported }

// inspectServer finds nothing: there is no process table to read.
func inspectServer(pid int, root string) (serverProc, bool) { return serverProc{}, false }

// stopGroups stops nothing: scanServers never finds anything to stop.
func stopGroups(pids []int) []int { return nil }

// stopStrays has no process table to read here (muster #405).
func stopStrays(root, only string) (stopped, left []strayProc, err error) {
	return nil, nil, ErrScanUnsupported
}
