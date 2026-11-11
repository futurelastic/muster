package sandbox

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// These tests run a REAL confined child: the test binary started through its own
// launcher (main_linux_test.go), applying a real policy to itself and then
// running a probe that tries one thing and reports what the kernel said. Nothing
// about the kernel is mocked, so a test that passes here passed because the
// kernel refused.
//
// Where a kernel is too old for a right, the test skips and says why; with
// MUSTER_SANDBOX_REQUIRE=1 (the dedicated lane, on a kernel that has everything)
// a skip is a failure, so that lane cannot go quietly grey.

// landlockHostABI is what this host's kernel offers, or zero when it offers none.
func landlockHostABI(t *testing.T) int {
	t.Helper()
	abi, err := landlockABI()
	if err != nil {
		return 0
	}
	return abi
}

// requireABI skips (or, on the required lane, fails) when the host's Landlock is
// older than the right under test needs.
func requireABI(t *testing.T, need int) int {
	t.Helper()
	abi := landlockHostABI(t)
	if abi < need {
		msg := fmt.Sprintf("this host's Landlock ABI is %d and the test needs %d", abi, need)
		if os.Getenv("MUSTER_SANDBOX_REQUIRE") == "1" {
			t.Fatalf("%s, and MUSTER_SANDBOX_REQUIRE=1 says this lane must have it", msg)
		}
		t.Skip(msg)
	}
	return abi
}

// self is the test binary, which is both the launcher and the probe.
func self(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// plan is a Linux plan that can run the test binary and nothing of the user's.
func plan(t *testing.T, network fleet.SandboxNetwork, write ...string) *Plan {
	t.Helper()
	read := append(SystemReadRootsFor(MechanismLinux), ResolvePath(filepath.Dir(self(t))))
	var w []string
	for _, p := range write {
		w = append(w, ResolvePath(p))
	}
	return &Plan{Mechanism: MechanismLinux, Network: network, Read: Dedupe(append(read, w...)), Write: Dedupe(w)}
}

type result struct {
	out, stderr string
	exit        int
}

// run starts argv through the plan's launcher and reports what the child printed.
func run(t *testing.T, p *Plan, extraEnv []string, argv ...string) result {
	t.Helper()
	withLauncher(t, self(t))
	wrapped, err := p.WrapInline(argv)
	if err != nil {
		t.Fatal(err)
	}
	return start(t, wrapped, extraEnv)
}

func start(t *testing.T, wrapped []string, extraEnv []string) result {
	t.Helper()
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	abi := landlockHostABI(t)
	floor := MinLandlockABI
	if abi < floor {
		floor = abi
	}
	cmd.Env = append(os.Environ(), "MUSTER_SANDBOX_TEST_MIN_ABI="+strconv.Itoa(floor))
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := result{out: strings.TrimSpace(out.String()), stderr: strings.TrimSpace(errb.String())}
	if ee, ok := err.(*exec.ExitError); ok {
		r.exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("starting the launcher: %v", err)
	}
	return r
}

func probe(t *testing.T, p *Plan, check string, args ...string) string {
	t.Helper()
	r := run(t, p, nil, append([]string{self(t), "__probe", check}, args...)...)
	if r.exit != 0 {
		t.Fatalf("probe %s exited %d: %s %s", check, r.exit, r.out, r.stderr)
	}
	return r.out
}

func need(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// threeDirs are a directory the session may write, one it may only read, and one
// it was not given, each holding a file.
func threeDirs(t *testing.T) (write, read, outside string) {
	t.Helper()
	root := t.TempDir()
	for _, n := range []string{"write", "read", "outside"} {
		if err := os.Mkdir(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, n, "file"), []byte("content of "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "write"), filepath.Join(root, "read"), filepath.Join(root, "outside")
}

func TestFilesAreDeniedOutsideTheGrantsAndAChildInheritsThat(t *testing.T) {
	requireABI(t, 3)
	write, read, outside := threeDirs(t)
	p := plan(t, fleet.SandboxNetworkOpen, write)
	p.Read = Dedupe(append(p.Read, ResolvePath(read)))

	need(t, probe(t, p, "read", filepath.Join(write, "file")), "ok")
	need(t, probe(t, p, "read", filepath.Join(read, "file")), "ok")
	need(t, probe(t, p, "read", filepath.Join(outside, "file")), "EACCES")
	need(t, probe(t, p, "list", outside), "EACCES")
	need(t, probe(t, p, "write", filepath.Join(write, "new")), "ok")
	need(t, probe(t, p, "write", filepath.Join(read, "new")), "EACCES")
	need(t, probe(t, p, "write", filepath.Join(read, "file")), "EACCES")
	need(t, probe(t, p, "write", filepath.Join(outside, "new")), "EACCES")

	// A child of the session is confined as tightly as the session: a shell, and
	// the probe started by that shell, both stay inside.
	r := run(t, p, nil, "/bin/sh", "-c", "cat "+filepath.Join(outside, "file")+" 2>&1; echo exit=$?")
	if strings.Contains(r.out, "content of outside") || !strings.Contains(r.out, "exit=1") {
		t.Errorf("a shell child read what the session could not: %q", r.out)
	}
	r = run(t, p, nil, "/bin/sh", "-c", self(t)+" __probe read "+filepath.Join(outside, "file"))
	need(t, r.out, "EACCES")
}

func TestWritingInsideTheGrantedDirectoryDoesWhatABuildDoes(t *testing.T) {
	requireABI(t, 3)
	write, _, _ := threeDirs(t)
	p := plan(t, fleet.SandboxNetworkOpen, write)
	need(t, probe(t, p, "fsops", write), "ok")
}

func TestAnotherProcessesEnvironmentIsNotReadableEvenThoughProcIsGranted(t *testing.T) {
	requireABI(t, 3)
	// The supervisor's credential is in its environment. /proc is granted read, so
	// what must stop this is the kernel's scoping of process access.
	holder := exec.Command("sleep", "30")
	holder.Env = append(os.Environ(), "MUSTER_PROBE_SECRET=hunter2")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	environ := fmt.Sprintf("/proc/%d/environ", holder.Process.Pid)

	// The control: outside any policy the same read works, so a refusal below is
	// the kernel's and not a path that was never readable.
	control, err := exec.Command(self(t), "__probe", "read", environ).Output()
	if err != nil || strings.TrimSpace(string(control)) != "ok" {
		t.Fatalf("control: an unconfined read of %s = %q, %v; the test cannot tell a refusal from a failure", environ, control, err)
	}
	p := plan(t, fleet.SandboxNetworkOpen)
	got := probe(t, p, "read", environ)
	if got != "EACCES" && got != "EPERM" {
		t.Errorf("another process's environment: %q", got)
	}
	need(t, probe(t, p, "read", "/proc/self/status"), "ok")
}

func TestUnixSocketsCannotBeCreatedButAPreConnectedPairCan(t *testing.T) {
	requireABI(t, 3)
	write, _, _ := threeDirs(t)
	sock := filepath.Join(write, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Even in a directory the session may read and write, and with a listener
	// there: the session cannot name a unix socket at all.
	p := plan(t, fleet.SandboxNetworkOpen, write)
	need(t, probe(t, p, "unix", sock), "socket:EACCES")
	need(t, probe(t, p, "abstract", "muster-probe-"+strconv.Itoa(os.Getpid())), "socket:EACCES")
	need(t, probe(t, p, "socketpair"), "ok")
	need(t, probe(t, p, "socketpair-dgram"), "EACCES")
}

func TestScopingStopsTheAbstractSocketAndTheSignalOnItsOwn(t *testing.T) {
	requireABI(t, 6)
	name := "muster-probe-" + strconv.Itoa(os.Getpid())
	ln, err := net.Listen("unix", "@"+name)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	p := plan(t, fleet.SandboxNetworkOpen)
	only := []string{"MUSTER_SANDBOX_TEST_LANDLOCK_ONLY=1"}

	// Without the filter, the socket can be created; Landlock's scope still
	// refuses to connect it to a listener outside the domain.
	r := run(t, p, only, self(t), "__probe", "abstract", name)
	need(t, r.out, "connect:EPERM")
	// Signal 0 to this process, which is the same user's and would be allowed
	// without the scope.
	r = run(t, p, only, self(t), "__probe", "kill", strconv.Itoa(os.Getpid()))
	need(t, r.out, "EPERM")
	// With everything on, the same two are refused too, by the first layer to see them.
	need(t, probe(t, p, "abstract", name), "socket:EACCES")
	need(t, probe(t, p, "kill", strconv.Itoa(os.Getpid())), "EPERM")
}

func TestSharedMemoryQueuesKeyringsAndIoUringAreRefused(t *testing.T) {
	requireABI(t, 3)
	p := plan(t, fleet.SandboxNetworkOpen)
	need(t, probe(t, p, "shmget"), "EPERM")
	need(t, probe(t, p, "mq_open"), "EPERM")
	need(t, probe(t, p, "keyctl"), "EPERM")
	need(t, probe(t, p, "uring"), "ENOSYS")
}

func TestPtraceOfAnOutsideProcessIsRefused(t *testing.T) {
	requireABI(t, 3)
	// A distribution's own process-attach policy may refuse this too; the test
	// pins the answer a session gets, and the scoping is proved by the environment
	// test above, where nothing else would have refused.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	p := plan(t, fleet.SandboxNetworkOpen)
	need(t, probe(t, p, "ptrace", strconv.Itoa(cmd.Process.Pid)), "EPERM")
}

func TestNoNewPrivsAndTheFilterAreOnInTheChild(t *testing.T) {
	requireABI(t, 3)
	need(t, probe(t, plan(t, fleet.SandboxNetworkOpen), "status"), "NoNewPrivs: 1; Seccomp: 2")
}

func TestAClosedNetworkRefusesEveryEgressAndOnlyTheNamedPortCanBeBound(t *testing.T) {
	requireABI(t, 4)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	p := plan(t, fleet.SandboxNetworkClosed)

	// Not even loopback: the session cannot connect anywhere.
	need(t, probe(t, p, "tcp", target), "connect:EACCES")
	need(t, probe(t, p, "udp"), "EACCES")
	need(t, probe(t, p, "udp6"), "EACCES")
	// The one netlink protocol a resolver reads addresses with is still open.
	need(t, probe(t, p, "netlink"), "ok")
	// Nothing may be bound unless the policy names it.
	need(t, probe(t, p, "serve", "0"), "listen:EACCES")
}

func TestTheNamedPortCanBeBoundAndTheServiceConnectsIn(t *testing.T) {
	requireABI(t, 4)
	// A free port, chosen the way the driver chooses its server's.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	p := plan(t, fleet.SandboxNetworkClosed)
	p.BindTCP = []int{port}
	withLauncher(t, self(t))
	wrapped, err := p.WrapInline([]string{self(t), "__probe", "serve", strconv.Itoa(port)})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Env = append(os.Environ(), "MUSTER_SANDBOX_TEST_MIN_ABI=4")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line := make([]byte, 64)
	n, _ := stdout.Read(line)
	if got := strings.TrimSpace(string(line[:n])); got != "listening" {
		t.Fatalf("the confined server did not start listening: %q", got)
	}
	// The service is not confined, and listen/accept were never restricted: it
	// connects into the session's server, which is the opencode shape.
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("connecting into the confined server: %v", err)
	}
	defer c.Close()
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "pong" {
		t.Fatalf("reply = %q, %v", b, err)
	}
	// A port the policy did not name stays closed to it.
	other := strconv.Itoa(port + 1)
	need(t, probe(t, p, "serve", other), "listen:EACCES")
}

func TestAnOpenNetworkStillReachesTheNetworkAndNamesNothingToBind(t *testing.T) {
	requireABI(t, 3)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	p := plan(t, fleet.SandboxNetworkOpen)
	need(t, probe(t, p, "tcp", target), "connect:ok")
	need(t, probe(t, p, "udp"), "ok")
	need(t, probe(t, p, "udp6"), "ok")
}

func TestANewPseudoTerminalCanBeOpenedBecauseThePtyDirectoryIsGranted(t *testing.T) {
	requireABI(t, 3)
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("this host has no /dev/ptmx")
	}
	need(t, probe(t, plan(t, fleet.SandboxNetworkOpen), "pty"), "ok")
}

func TestTheLauncherRefusesBeforeItRunsAnythingAndNeverRunsUnconfined(t *testing.T) {
	requireABI(t, 3)
	marker := filepath.Join(t.TempDir(), "ran")
	base := plan(t, fleet.SandboxNetworkOpen, filepath.Dir(marker))
	target := []string{self(t), "__probe", "write", marker}

	gone := func(what string) {
		t.Helper()
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the program ran", what)
		}
	}

	// A floor above what the kernel offers.
	withLauncher(t, self(t))
	wrapped, err := base.WrapInline(target)
	if err != nil {
		t.Fatal(err)
	}
	r := start(t, wrapped, []string{"MUSTER_SANDBOX_TEST_MIN_ABI=99"})
	if r.exit != 125 || !strings.Contains(r.stderr, "ABI") {
		t.Errorf("a floor above the kernel: exit %d, %q", r.exit, r.stderr)
	}
	gone("a floor above the kernel")

	// A grant that does not exist is a refusal, not a smaller profile.
	missing := plan(t, fleet.SandboxNetworkOpen)
	missing.Read = append(missing.Read, filepath.Join(t.TempDir(), "not-there"))
	r = run(t, missing, nil, target...)
	if r.exit != 125 || !strings.Contains(r.stderr, "not-there") {
		t.Errorf("a missing grant: exit %d, %q", r.exit, r.stderr)
	}
	gone("a missing grant")

	// A document of another version.
	r = start(t, []string{self(t), LauncherArg, "--policy", `{"version":2,"network":"open","read":[],"write":[],"devices":[],"ownTTY":false,"bindTCP":[]}`, "--", self(t), "__probe", "write", marker}, nil)
	if r.exit != 125 {
		t.Errorf("another version: exit %d, %q", r.exit, r.stderr)
	}
	gone("another version")

	// A program that is not an absolute path is not looked up.
	r = run(t, base, nil, "probe-not-on-a-path", "x")
	if r.exit != 125 {
		t.Errorf("a relative program: exit %d, %q", r.exit, r.stderr)
	}

	// An exec that fails is a different exit, after the confinement is already on.
	r = run(t, base, nil, filepath.Join(t.TempDir(), "no-such-program"))
	if r.exit != 127 || !strings.Contains(r.stderr, "starting") {
		t.Errorf("a program that cannot be started: exit %d, %q", r.exit, r.stderr)
	}
}

func TestADescriptorTheLauncherWasHandedDoesNotSurviveIntoTheProgram(t *testing.T) {
	requireABI(t, 3)
	f, err := os.CreateTemp(t.TempDir(), "handed")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := plan(t, fleet.SandboxNetworkOpen)
	withLauncher(t, self(t))
	// The shell asks whether descriptor 3, the one handed in, is still open.
	wrapped, err := p.WrapInline([]string{"/bin/sh", "-c", "if [ -e /proc/self/fd/3 ]; then echo inherited; else echo closed; fi"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Env = append(os.Environ(), "MUSTER_SANDBOX_TEST_MIN_ABI="+strconv.Itoa(minOf(landlockHostABI(t), MinLandlockABI)))
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	need(t, strings.TrimSpace(string(out)), "closed")
}

func minOf(a, b int) int {
	if a < b {
		return a
	}
	return b
}
