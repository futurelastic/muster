package sandbox

import (
	"encoding/binary"
	"fmt"

	fleet "github.com/futurelastic/muster"
)

// The seccomp filter of the Linux mechanism (muster #439), assembled as classic
// BPF.
//
// This file is deliberately free of build tags and of any system call: the
// program is data, built from a per-architecture table of syscall numbers, so
// every platform's tests can run it through an interpreter (seccomp_bpf_test.go)
// and the Linux launcher only has to hand the bytes to the kernel.
//
// # What the filter is for
//
// Landlock confines files, and on a new enough kernel abstract unix sockets and
// signals. It cannot confine the rest of what a same-user process can reach, so
// the filter closes four things, and each is a way out of the other classes:
//
//   - unix sockets. socket(AF_UNIX, ...) is refused for every type, which covers
//     pathname sockets (the multiplexer's, the SSH agent's) and abstract ones
//     (the session bus, X11) without a path to name. socketpair(AF_UNIX) stays
//     for stream and seqpacket, because a child-process library cannot spawn a
//     program without one, and such a pair is already connected: it names no
//     peer. A datagram pair is refused, because it can be connected again to any
//     address.
//   - network. Only the families a program resolves names and talks TCP with
//     are open: AF_INET, AF_INET6 and the one netlink protocol the resolver
//     library reads interfaces with. Under network "closed" an inet socket must
//     also be a TCP stream, which refuses UDP (so name resolution), raw and ICMP
//     sockets, and the TCP-like protocols Landlock's port rules do not cover.
//   - other processes' memory and queues. SysV shared memory, semaphores and
//     message queues, POSIX message queues and the kernel keyrings are shared by
//     every process of one user and are not named by a path, so no file rule sees
//     them.
//   - io_uring, which can open a socket or a file without the system call this
//     filter looks at.
//
// A call from another architecture's ABI (the 32-bit entry point of a 64-bit
// kernel, the x32 range) answers ENOSYS, because its numbers are not the ones in
// the table.

// Seccomp return values and the errors the filter answers with.
const (
	seccompRetAllow = 0x7fff0000
	seccompRetErrno = 0x00050000

	errnoEPERM  = 1
	errnoEACCES = 13
	errnoENOSYS = 38
)

// Offsets into struct seccomp_data, and the audit architectures.
const (
	seccompOffNr   = 0
	seccompOffArch = 4
	seccompOffArg0 = 16
	seccompOffArg1 = 24
	seccompOffArg2 = 32

	auditArchX8664   = 0xc000003e
	auditArchAarch64 = 0xc00000b7
)

// Socket families, types and protocols the filter names.
const (
	afUnix    = 1
	afInet    = 2
	afInet6   = 10
	afNetlink = 16

	sockTypeMask      = 0xf // the low nibble: SOCK_NONBLOCK and SOCK_CLOEXEC ride above it
	sockStream        = 1
	sockSeqpacket     = 5
	ipprotoTCP        = 6
	netlinkRouteProto = 0
)

// SyscallTable is the part of the filter that depends on the architecture: the
// audit identity, and the number of every system call the filter names.
type SyscallTable struct {
	// Arch is the AUDIT_ARCH value of the ABI the table is for.
	Arch uint32
	// RejectX32 answers ENOSYS to a number in the x32 range (amd64 only).
	RejectX32 bool
	// Socket and Socketpair are the two calls whose arguments the filter reads.
	Socket, Socketpair uint32
	// EPerm are refused with EPERM: SysV IPC, POSIX message queues, keyrings.
	EPerm []uint32
	// ENoSys are answered with ENOSYS, so a library falls back: io_uring.
	ENoSys []uint32
}

// The io_uring calls are the same number on every architecture that has them.
var ioUringCalls = []uint32{425, 426, 427}

// TableAMD64 is the table for linux/amd64.
var TableAMD64 = SyscallTable{
	Arch:       auditArchX8664,
	RejectX32:  true,
	Socket:     41,
	Socketpair: 53,
	EPerm: []uint32{
		29, 30, 31, 67, // shmget, shmat, shmctl, shmdt
		64, 65, 66, 220, // semget, semop, semctl, semtimedop
		68, 69, 70, 71, // msgget, msgsnd, msgrcv, msgctl
		240, 241, 242, 243, 244, 245, // mq_open, mq_unlink, mq_timedsend, mq_timedreceive, mq_notify, mq_getsetattr
		248, 249, 250, // add_key, request_key, keyctl
	},
	ENoSys: ioUringCalls,
}

// TableARM64 is the table for linux/arm64.
var TableARM64 = SyscallTable{
	Arch:       auditArchAarch64,
	Socket:     198,
	Socketpair: 199,
	EPerm: []uint32{
		194, 196, 195, 197, // shmget, shmat, shmctl, shmdt
		190, 193, 191, 192, // semget, semop, semctl, semtimedop
		186, 189, 188, 187, // msgget, msgsnd, msgrcv, msgctl
		180, 181, 182, 183, 184, 185, // mq_open, mq_unlink, mq_timedsend, mq_timedreceive, mq_notify, mq_getsetattr
		217, 218, 219, // add_key, request_key, keyctl
	},
	ENoSys: ioUringCalls,
}

// TableFor returns the table for a GOARCH, or false when the mechanism has none:
// an architecture this package has not measured is refused rather than guessed.
func TableFor(goarch string) (SyscallTable, bool) {
	switch goarch {
	case "amd64":
		return TableAMD64, true
	case "arm64":
		return TableARM64, true
	}
	return SyscallTable{}, false
}

// bpfInsn is one classic BPF instruction; jt and jf name labels until assemble
// turns them into forward offsets.
type bpfInsn struct {
	op     uint16
	k      uint32
	jt, jf string
}

// bpfAsm is a tiny assembler with forward labels, because a hand-counted jump
// offset is exactly the mistake a security filter cannot afford.
type bpfAsm struct {
	ins    []bpfInsn
	labels map[string]int
}

const (
	bpfLdAbsW = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfAndK   = 0x54 // BPF_ALU | BPF_AND | BPF_K
	bpfJeqK   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJgeK   = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfRetK   = 0x06 // BPF_RET | BPF_K
)

func (a *bpfAsm) ld(off uint32) { a.ins = append(a.ins, bpfInsn{op: bpfLdAbsW, k: off}) }
func (a *bpfAsm) and(k uint32)  { a.ins = append(a.ins, bpfInsn{op: bpfAndK, k: k}) }
func (a *bpfAsm) ret(k uint32)  { a.ins = append(a.ins, bpfInsn{op: bpfRetK, k: k}) }

// jeq jumps to t when the accumulator equals k and to f otherwise; an empty
// label falls through to the next instruction.
func (a *bpfAsm) jeq(k uint32, t, f string) {
	a.ins = append(a.ins, bpfInsn{op: bpfJeqK, k: k, jt: t, jf: f})
}

func (a *bpfAsm) jge(k uint32, t, f string) {
	a.ins = append(a.ins, bpfInsn{op: bpfJgeK, k: k, jt: t, jf: f})
}

func (a *bpfAsm) label(name string) {
	if a.labels == nil {
		a.labels = map[string]int{}
	}
	a.labels[name] = len(a.ins)
}

// assemble resolves the labels and returns the program as the kernel's
// sock_filter array: per instruction a little-endian code, jump-true,
// jump-false and constant. A jump that does not fit its 8-bit field is an
// error, not a wrap-around.
func (a *bpfAsm) assemble() ([]byte, error) {
	out := make([]byte, 0, len(a.ins)*8)
	for i, in := range a.ins {
		offset := func(label string) (uint8, error) {
			if label == "" {
				return 0, nil
			}
			target, ok := a.labels[label]
			if !ok {
				return 0, fmt.Errorf("seccomp filter: unknown label %q", label)
			}
			d := target - (i + 1)
			if d < 0 || d > 255 {
				return 0, fmt.Errorf("seccomp filter: the jump to %q (%d instructions) does not fit", label, d)
			}
			return uint8(d), nil
		}
		jt, err := offset(in.jt)
		if err != nil {
			return nil, err
		}
		jf, err := offset(in.jf)
		if err != nil {
			return nil, err
		}
		var b [8]byte
		binary.LittleEndian.PutUint16(b[0:], in.op)
		b[2], b[3] = jt, jf
		binary.LittleEndian.PutUint32(b[4:], in.k)
		out = append(out, b[:]...)
	}
	return out, nil
}

// SeccompProgram assembles the filter for one architecture table and one
// network posture, and returns the sock_filter array. An unknown posture is
// refused, so a typo cannot silently become "open".
func SeccompProgram(t SyscallTable, network fleet.SandboxNetwork) ([]byte, error) {
	if network != fleet.SandboxNetworkOpen && network != fleet.SandboxNetworkClosed {
		return nil, fmt.Errorf("seccomp filter: network %q is not one of %q, %q", string(network), fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed)
	}
	a := &bpfAsm{}
	a.ld(seccompOffArch)
	a.jeq(t.Arch, "arch_ok", "r_enosys")
	a.label("arch_ok")
	a.ld(seccompOffNr)
	if t.RejectX32 {
		a.jge(0x40000000, "r_enosys", "")
	}
	for _, n := range t.EPerm {
		a.jeq(n, "r_eperm", "")
	}
	for _, n := range t.ENoSys {
		a.jeq(n, "r_enosys", "")
	}
	a.jeq(t.Socket, "socket", "")
	a.jeq(t.Socketpair, "socketpair", "")
	a.ret(seccompRetAllow)

	// socket(domain, type, protocol): an allow-list of families.
	a.label("socket")
	a.ld(seccompOffArg0)
	a.jeq(afUnix, "r_eacces", "")
	a.jeq(afInet, "inet", "")
	a.jeq(afInet6, "inet", "")
	a.jeq(afNetlink, "netlink", "")
	a.ret(seccompRetErrno | errnoEACCES)

	a.label("inet")
	if network == fleet.SandboxNetworkClosed {
		a.ld(seccompOffArg1)
		a.and(sockTypeMask)
		a.jeq(sockStream, "inet_proto", "r_eacces")
		a.label("inet_proto")
		a.ld(seccompOffArg2)
		a.jeq(0, "r_allow", "")
		a.jeq(ipprotoTCP, "r_allow", "r_eacces")
	} else {
		a.ret(seccompRetAllow)
	}

	// Only NETLINK_ROUTE, which the resolver library reads interface addresses
	// with. Its type is not looked at: the protocol is what selects the family.
	a.label("netlink")
	a.ld(seccompOffArg2)
	a.jeq(netlinkRouteProto, "r_allow", "r_eacces")

	// socketpair(domain, type, protocol): unix only, and never a datagram pair.
	a.label("socketpair")
	a.ld(seccompOffArg0)
	a.jeq(afUnix, "socketpair_type", "r_eacces")
	a.label("socketpair_type")
	a.ld(seccompOffArg1)
	a.and(sockTypeMask)
	a.jeq(sockStream, "r_allow", "")
	a.jeq(sockSeqpacket, "r_allow", "r_eacces")

	a.label("r_allow")
	a.ret(seccompRetAllow)
	a.label("r_eperm")
	a.ret(seccompRetErrno | errnoEPERM)
	a.label("r_eacces")
	a.ret(seccompRetErrno | errnoEACCES)
	a.label("r_enosys")
	a.ret(seccompRetErrno | errnoENOSYS)
	return a.assemble()
}
