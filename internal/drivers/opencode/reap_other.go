//go:build !unix && !windows

package opencode

// reapStale has nothing to stop off unix. Reaping a server a previous service
// left behind needs a process table query and a signal to the server's process
// group, neither of which exists here; and off unix the driver refuses to start
// anyway (procgroup.Supported is false there), so no server this
// package launched is ever left running to be found. (#303)
func reapStale(r *sessionRecord) bool { return false }
