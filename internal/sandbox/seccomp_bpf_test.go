package sandbox

import (
	"encoding/binary"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// seccompData is the part of struct seccomp_data the filter reads.
type seccompData struct {
	nr   uint32
	arch uint32
	args [6]uint64
}

// runBPF interprets a classic BPF program over a synthetic system call, the way
// the kernel would, and returns its verdict. It knows only the instructions the
// assembler emits, and fails the test on any other, so a new instruction cannot
// slip into the filter unexamined.
func runBPF(t *testing.T, prog []byte, d seccompData) uint32 {
	t.Helper()
	if len(prog)%8 != 0 || len(prog) == 0 {
		t.Fatalf("a program of %d bytes is not a whole number of instructions", len(prog))
	}
	var acc uint32
	pc := 0
	for steps := 0; steps < 10000; steps++ {
		if pc*8 >= len(prog) {
			t.Fatalf("the program ran off its end at instruction %d", pc)
		}
		in := prog[pc*8 : pc*8+8]
		op := binary.LittleEndian.Uint16(in[0:])
		jt, jf := int(in[2]), int(in[3])
		k := binary.LittleEndian.Uint32(in[4:])
		switch op {
		case bpfLdAbsW:
			switch {
			case k == seccompOffNr:
				acc = d.nr
			case k == seccompOffArch:
				acc = d.arch
			case k >= 16 && k < 64 && (k-16)%8 == 0:
				acc = uint32(d.args[(k-16)/8]) // the low word, as on a little-endian kernel
			default:
				t.Fatalf("the program loads offset %d, which the filter does not claim to read", k)
			}
		case bpfAndK:
			acc &= k
		case bpfJeqK:
			if acc == k {
				pc += jt
			} else {
				pc += jf
			}
		case bpfJgeK:
			if acc >= k {
				pc += jt
			} else {
				pc += jf
			}
		case bpfRetK:
			return k
		default:
			t.Fatalf("the program uses instruction code %#x", op)
		}
		pc++
	}
	t.Fatal("the program did not finish")
	return 0
}

func errnoVerdict(e uint32) uint32 { return seccompRetErrno | e }

func program(t *testing.T, tbl SyscallTable, net fleet.SandboxNetwork) []byte {
	t.Helper()
	p, err := SeccompProgram(tbl, net)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p) / 8; n > 255 {
		t.Fatalf("the program is %d instructions long; keep it well under the kernel's limit", n)
	}
	return p
}

var bothTables = map[string]SyscallTable{"amd64": TableAMD64, "arm64": TableARM64}

func TestFilterAnswersEnosysToAnotherAbi(t *testing.T) {
	for name, tbl := range bothTables {
		p := program(t, tbl, fleet.SandboxNetworkOpen)
		// The 32-bit entry point of a 64-bit kernel: its numbers are not the table's.
		got := runBPF(t, p, seccompData{nr: tbl.Socket, arch: 0x40000003})
		if got != errnoVerdict(errnoENOSYS) {
			t.Errorf("%s: a call from another ABI got verdict %#x, want ENOSYS", name, got)
		}
	}
	// The x32 range of amd64 shares the architecture and not the numbers.
	p := program(t, TableAMD64, fleet.SandboxNetworkOpen)
	if got := runBPF(t, p, seccompData{nr: 0x40000000 + TableAMD64.Socket, arch: TableAMD64.Arch}); got != errnoVerdict(errnoENOSYS) {
		t.Errorf("an x32 call got verdict %#x, want ENOSYS", got)
	}
}

func TestFilterRefusesSharedMemoryQueuesAndKeyrings(t *testing.T) {
	for name, tbl := range bothTables {
		for _, net := range []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed} {
			p := program(t, tbl, net)
			for _, nr := range tbl.EPerm {
				if got := runBPF(t, p, seccompData{nr: nr, arch: tbl.Arch}); got != errnoVerdict(errnoEPERM) {
					t.Errorf("%s/%s: call %d got verdict %#x, want EPERM", name, net, nr, got)
				}
			}
			for _, nr := range tbl.ENoSys {
				if got := runBPF(t, p, seccompData{nr: nr, arch: tbl.Arch}); got != errnoVerdict(errnoENOSYS) {
					t.Errorf("%s/%s: call %d got verdict %#x, want ENOSYS", name, net, nr, got)
				}
			}
		}
	}
}

func TestFilterAllowsAnOrdinaryCall(t *testing.T) {
	for name, tbl := range bothTables {
		p := program(t, tbl, fleet.SandboxNetworkClosed)
		named := func(nr uint32) bool {
			for _, list := range [][]uint32{tbl.EPerm, tbl.ENoSys, {tbl.Socket, tbl.Socketpair}} {
				for _, n := range list {
					if n == nr {
						return true
					}
				}
			}
			return false
		}
		// None of these is named in either table, which is all the filter can tell.
		for _, nr := range []uint32{0, 1, 59, 200, 435} {
			if named(nr) {
				continue
			}
			if got := runBPF(t, p, seccompData{nr: nr, arch: tbl.Arch}); got != seccompRetAllow {
				t.Errorf("%s: call %d got verdict %#x, want allow", name, nr, got)
			}
		}
	}
}

func TestFilterSocketMatrix(t *testing.T) {
	const (
		afPacket = 17
		afAlg    = 38
		afVsock  = 40

		sockDgram  = 2
		sockRaw    = 3
		cloexec    = 0x80000
		nonblock   = 0x800
		protoUDP   = 17
		protoMPTCP = 262
		protoAudit = 9
		protoUevt  = 15
	)
	allow, eacces := uint32(seccompRetAllow), errnoVerdict(errnoEACCES)
	type sock struct {
		name                  string
		domain, typ, protocol uint64
		open, closed          uint32
	}
	cases := []sock{
		{"unix stream", afUnix, sockStream, 0, eacces, eacces},
		{"unix dgram", afUnix, sockDgram, 0, eacces, eacces},
		{"unix seqpacket", afUnix, sockSeqpacket, 0, eacces, eacces},
		{"unix stream cloexec", afUnix, sockStream | cloexec | nonblock, 0, eacces, eacces},
		{"tcp", afInet, sockStream, 0, allow, allow},
		{"tcp explicit", afInet, sockStream, ipprotoTCP, allow, allow},
		{"tcp cloexec nonblock", afInet, sockStream | cloexec | nonblock, 0, allow, allow},
		{"tcp v6", afInet6, sockStream, 0, allow, allow},
		{"udp", afInet, sockDgram, 0, allow, eacces},
		{"udp explicit", afInet, sockDgram, protoUDP, allow, eacces},
		{"udp v6", afInet6, sockDgram | cloexec, 0, allow, eacces},
		{"raw", afInet, sockRaw, protoUDP, allow, eacces},
		{"stream that is not tcp", afInet, sockStream, protoUDP, allow, eacces},
		{"mptcp", afInet, sockStream, protoMPTCP, allow, eacces},
		{"netlink route", afNetlink, sockRaw, netlinkRouteProto, allow, allow},
		{"netlink audit", afNetlink, sockRaw, protoAudit, eacces, eacces},
		{"netlink uevent", afNetlink, sockDgram, protoUevt, eacces, eacces},
		{"packet", afPacket, sockRaw, 0, eacces, eacces},
		{"vsock", afVsock, sockStream, 0, eacces, eacces},
		{"crypto", afAlg, sockSeqpacket, 0, eacces, eacces},
		// socketpair is a different call: pre-connected pairs only, and only unix.
		{"pair stream", afUnix, sockStream, 0, allow, allow},
		{"pair seqpacket", afUnix, sockSeqpacket | cloexec, 0, allow, allow},
		{"pair dgram", afUnix, sockDgram, 0, eacces, eacces},
		{"pair inet", afInet, sockStream, 0, eacces, eacces},
	}
	for name, tbl := range bothTables {
		open := program(t, tbl, fleet.SandboxNetworkOpen)
		closed := program(t, tbl, fleet.SandboxNetworkClosed)
		for _, c := range cases {
			nr := tbl.Socket
			if len(c.name) > 5 && c.name[:5] == "pair " {
				nr = tbl.Socketpair
			}
			d := seccompData{nr: nr, arch: tbl.Arch, args: [6]uint64{c.domain, c.typ, c.protocol}}
			if got := runBPF(t, open, d); got != c.open {
				t.Errorf("%s open: %s got verdict %#x, want %#x", name, c.name, got, c.open)
			}
			if got := runBPF(t, closed, d); got != c.closed {
				t.Errorf("%s closed: %s got verdict %#x, want %#x", name, c.name, got, c.closed)
			}
		}
	}
}

func TestFilterRefusesAnUnknownPosture(t *testing.T) {
	if _, err := SeccompProgram(TableAMD64, "loopback"); err == nil {
		t.Fatal("a posture that is neither open nor closed was assembled")
	}
	if _, err := SeccompProgram(TableAMD64, ""); err == nil {
		t.Fatal("an empty posture was assembled: the caller must say which, a default belongs to the plan")
	}
}

func TestTablesNameEachCallOnce(t *testing.T) {
	for name, tbl := range bothTables {
		seen := map[uint32]string{tbl.Socket: "socket", tbl.Socketpair: "socketpair"}
		if tbl.Socket == tbl.Socketpair {
			t.Errorf("%s: socket and socketpair are the same number", name)
		}
		for _, list := range [][]uint32{tbl.EPerm, tbl.ENoSys} {
			for _, n := range list {
				if prev, dup := seen[n]; dup {
					t.Errorf("%s: call %d is named twice (%s)", name, n, prev)
				}
				seen[n] = "listed"
			}
		}
	}
	if _, ok := TableFor("amd64"); !ok {
		t.Error("no table for amd64")
	}
	if _, ok := TableFor("arm64"); !ok {
		t.Error("no table for arm64")
	}
	if _, ok := TableFor("riscv64"); ok {
		t.Error("an architecture nobody measured got a table")
	}
}

func TestAssemblerRefusesAJumpThatDoesNotFit(t *testing.T) {
	a := &bpfAsm{}
	a.jeq(1, "far", "")
	for i := 0; i < 300; i++ {
		a.ret(0)
	}
	a.label("far")
	a.ret(0)
	if _, err := a.assemble(); err == nil {
		t.Fatal("a jump of 300 instructions was packed into an 8-bit field")
	}
	b := &bpfAsm{}
	b.jeq(1, "nowhere", "")
	b.ret(0)
	if _, err := b.assemble(); err == nil {
		t.Fatal("a jump to a label that does not exist was assembled")
	}
}
