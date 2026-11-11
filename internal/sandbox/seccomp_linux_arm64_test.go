//go:build linux && arm64

package sandbox

import (
	"syscall"
	"testing"
)

// The literal numbers in TableARM64 are checked against the syscall package, so a
// typo cannot make the filter refuse the wrong call.
func TestTableARM64MatchesTheSyscallPackage(t *testing.T) {
	want := []uint32{
		syscall.SYS_SHMGET, syscall.SYS_SHMAT, syscall.SYS_SHMCTL, syscall.SYS_SHMDT,
		syscall.SYS_SEMGET, syscall.SYS_SEMOP, syscall.SYS_SEMCTL, syscall.SYS_SEMTIMEDOP,
		syscall.SYS_MSGGET, syscall.SYS_MSGSND, syscall.SYS_MSGRCV, syscall.SYS_MSGCTL,
		syscall.SYS_MQ_OPEN, syscall.SYS_MQ_UNLINK, syscall.SYS_MQ_TIMEDSEND, syscall.SYS_MQ_TIMEDRECEIVE,
		syscall.SYS_MQ_NOTIFY, syscall.SYS_MQ_GETSETATTR,
		syscall.SYS_ADD_KEY, syscall.SYS_REQUEST_KEY, syscall.SYS_KEYCTL,
	}
	sameSet(t, TableARM64.EPerm, want)
	if TableARM64.Socket != syscall.SYS_SOCKET || TableARM64.Socketpair != syscall.SYS_SOCKETPAIR {
		t.Errorf("socket/socketpair = %d/%d", TableARM64.Socket, TableARM64.Socketpair)
	}
}

func sameSet(t *testing.T, got, want []uint32) {
	t.Helper()
	g := map[uint32]bool{}
	for _, n := range got {
		g[n] = true
	}
	if len(got) != len(want) {
		t.Fatalf("table has %d calls, the syscall package names %d", len(got), len(want))
	}
	for _, n := range want {
		if !g[n] {
			t.Fatalf("the syscall package names call %d, the table does not", n)
		}
	}
}
