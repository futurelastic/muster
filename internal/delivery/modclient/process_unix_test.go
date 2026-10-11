//go:build unix

package modclient_test

import (
	"syscall"
	"testing"
)

// assertLeadsItsOwnGroup checks the child is the leader of a process group of
// its own, so a kill of the group takes down whatever it spawned.
func assertLeadsItsOwnGroup(t *testing.T, pid int) {
	t.Helper()
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Errorf("pgid of the child = %d (%v), want its own pid %d", pgid, err, pid)
	}
}
