//go:build linux && amd64

package sandbox

import (
	"sort"
	"syscall"
	"testing"
)

// The literal numbers in TableAMD64 are checked against the syscall package, so a
// typo cannot make the filter refuse the wrong call.
func TestTableAMD64MatchesTheSyscallPackage(t *testing.T) {
	want := []uint32{
		syscall.SYS_SHMGET, syscall.SYS_SHMAT, syscall.SYS_SHMCTL, syscall.SYS_SHMDT,
		syscall.SYS_SEMGET, syscall.SYS_SEMOP, syscall.SYS_SEMCTL, syscall.SYS_SEMTIMEDOP,
		syscall.SYS_MSGGET, syscall.SYS_MSGSND, syscall.SYS_MSGRCV, syscall.SYS_MSGCTL,
		syscall.SYS_MQ_OPEN, syscall.SYS_MQ_UNLINK, syscall.SYS_MQ_TIMEDSEND, syscall.SYS_MQ_TIMEDRECEIVE,
		syscall.SYS_MQ_NOTIFY, syscall.SYS_MQ_GETSETATTR,
		syscall.SYS_ADD_KEY, syscall.SYS_REQUEST_KEY, syscall.SYS_KEYCTL,
	}
	sameSet(t, TableAMD64.EPerm, want)
	if TableAMD64.Socket != syscall.SYS_SOCKET || TableAMD64.Socketpair != syscall.SYS_SOCKETPAIR {
		t.Errorf("socket/socketpair = %d/%d", TableAMD64.Socket, TableAMD64.Socketpair)
	}
}

func sameSet(t *testing.T, got, want []uint32) {
	t.Helper()
	g := append([]uint32(nil), got...)
	w := append([]uint32(nil), want...)
	sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
	sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
	if len(g) != len(w) {
		t.Fatalf("table has %d calls, the syscall package names %d:\n got %v\nwant %v", len(g), len(w), g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("table differs from the syscall package:\n got %v\nwant %v", g, w)
		}
	}
}
