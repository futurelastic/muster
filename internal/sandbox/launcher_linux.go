package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	fleet "github.com/futurelastic/muster"
)

// The Linux launcher (muster #439): the service's own binary, started where
// macOS starts sandbox-exec, applies the policy to itself and becomes the agent.
//
// # Why this order, and why the thread is pinned
//
// Landlock, no_new_privs and a seccomp filter all attach to the CALLING THREAD
// and are inherited across fork and execve. The Go runtime runs goroutines on
// whatever thread it likes, so the launcher locks its goroutine to one thread
// first, does every step on it, and execs from it: execve discards every other
// thread, so the new program carries exactly the restrictions this thread has.
// Nothing tries to synchronise the restriction across threads, because there are
// none left by the time it matters.
//
// The order is: parse and refuse; query the kernel; build and apply the file
// rules (which sets no_new_privs); apply the filter; mark every inherited
// descriptor close-on-exec; exec. The marking matters: a unix socket the launcher
// was handed (the multiplexer's, a supervisor's) would otherwise survive into the
// agent, already connected, which no rule about CONNECTING could stop.
//
// A failure at any step before exec exits 125 and the program never runs. A
// failure of the exec itself exits 127, by which time the confinement is already
// on; the launcher has no path that runs a program unconfined.

const (
	exitRefused    = LauncherRefusedStatus
	exitExecFailed = LauncherExecFailedStatus

	closeRangeCloexec = 1 << 2
	sysCloseRange     = 436
)

// minABI is the Landlock ABI below which the launcher refuses. It is the
// constant, except in a test binary, whose TestMain lowers it so the rules that
// do not need the newest kernel can be proved on an ordinary CI runner. Nothing
// in the service reads it from anywhere.
var minABI = MinLandlockABI

// applySeccomp installs the filter. A variable so the tests can leave it out and
// prove that Landlock's own scoping closes the abstract-socket and signal routes
// by itself; the service never replaces it.
var applySeccomp = installSeccomp

func refuse(err error) int {
	fmt.Fprintf(os.Stderr, LauncherMessagePrefix+"%v\n", err)
	return exitRefused
}

// LauncherMain is the body of `muster __sandbox-exec`. It returns only on
// failure, or after a self-test; success is the exec, which does not return.
func LauncherMain(args []string) int {
	runtime.LockOSThread() // never unlocked: this thread applies the policy and execs
	a, err := parseLauncherArgs(args)
	if err != nil {
		return refuse(err)
	}
	abi, err := landlockABI()
	if err != nil {
		return refuse(fmt.Errorf("Landlock is not available on this kernel: %w", err))
	}
	if abi < minABI {
		return refuse(fmt.Errorf("Landlock ABI %d is below the %d this mechanism needs (Linux 6.12)", abi, minABI))
	}
	var terminals []string
	if a.policy.OwnTTY {
		terminals = ownTerminals()
	}
	if err := applyLandlock(a.policy, abi, terminals); err != nil {
		return refuse(err)
	}
	if err := applySeccomp(fleet.SandboxNetwork(a.policy.Network)); err != nil {
		return refuse(err)
	}
	if a.selfTest {
		return runSelfTest(a, abi)
	}
	if _, _, e := syscall.Syscall(sysCloseRange, 3, 0xffffffff, closeRangeCloexec); e != 0 {
		return refuse(fmt.Errorf("closing inherited descriptors: %w", e))
	}
	err = syscall.Exec(a.argv[0], a.argv, os.Environ())
	fmt.Fprintf(os.Stderr, LauncherMessagePrefix+"starting %s: %v\n", a.argv[0], err)
	return exitExecFailed
}

// runSelfTest checks, from inside the restriction it has just applied, that the
// restriction does what the capability will claim. It is what HostSupport runs in
// a throwaway child: the host is not offered the mechanism on the strength of a
// kernel version, but because a process confined this way was seen unable to do
// each thing. It prints "ok", or "FAIL: <what>" for the first check that was
// wrong.
func runSelfTest(a launcherArgs, abi int) int {
	fail := func(format string, args ...any) int {
		fmt.Printf("FAIL: "+format+"\n", args...)
		return exitRefused
	}
	denied := func(what string, err error, want ...syscall.Errno) bool {
		for _, w := range want {
			if errors.Is(err, w) {
				return true
			}
		}
		fail("%s: got %v, want %v", what, err, want)
		return false
	}
	if len(a.policy.Write) == 0 {
		return fail("the self-test policy grants no writable directory")
	}
	if err := os.WriteFile(filepath.Join(a.policy.Write[0], "probe"), []byte("x"), 0o600); err != nil {
		return fail("a write inside a granted directory: %v", err)
	}
	if _, err := os.ReadFile(a.outsideFile); !denied("reading a file outside the grants", err, syscall.EACCES) {
		return exitRefused
	}
	if _, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); !denied("creating a unix socket", err, syscall.EACCES) {
		return exitRefused
	}
	if _, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0); !denied("creating a UDP socket under a closed network", err, syscall.EACCES) {
		return exitRefused
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fail("creating a TCP socket: %v", err)
	}
	sa := &syscall.SockaddrInet4{Port: a.tcpPort, Addr: [4]byte{127, 0, 0, 1}}
	err = syscall.Connect(fd, sa)
	syscall.Close(fd)
	if !denied("connecting to loopback under a closed network", err, syscall.EACCES) {
		return exitRefused
	}
	if _, _, e := syscall.Syscall(syscall.SYS_SHMGET, 0, 4096, 0o1600); !denied("creating shared memory", errnoErr(e), syscall.EPERM) {
		return exitRefused
	}
	// Opening another process's environment: the supervisor's credential would be
	// in it. With /proc readable, what stops this is the kernel's scoping of
	// process access, not a missing grant.
	if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", os.Getppid())); !denied("reading the supervisor's environment", err, syscall.EACCES, syscall.EPERM) {
		return exitRefused
	}
	if abi >= 6 {
		if err := syscall.Kill(os.Getppid(), 0); !denied("signalling the supervisor", err, syscall.EPERM) {
			return exitRefused
		}
	}
	fmt.Println("ok")
	return 0
}

// errnoErr turns a raw system call's errno into an error, nil when it is zero.
func errnoErr(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}
