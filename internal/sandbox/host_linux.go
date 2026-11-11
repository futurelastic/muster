package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
)

// reasonPrefix starts every refusal, so a caller reading "sandboxUnavailable"
// can tell which mechanism declined and what it was looking for.
const reasonPrefix = MechanismLinux + ": "

// selfTestTimeout bounds the throwaway child. A variable so a test can shorten it.
var selfTestTimeout = 10 * time.Second

// The seams the tests use to speak for a host they are not running on.
var (
	hostABI    = landlockABI
	hostGOARCH = func() string { return runtime.GOARCH }
	hostLSM    = readActiveModules
)

// activeModulesFile is where the kernel lists the security modules that are on. A
// variable so a test can read a file of its own.
var activeModulesFile = "/sys/kernel/security/lsm"

// readActiveModules is the kernel's list of active security modules, in the order
// it names them.
func readActiveModules() ([]string, error) {
	raw, err := os.ReadFile(activeModulesFile)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range strings.Split(strings.TrimSpace(string(raw)), ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out, nil
}

// CheckHost looks at what the host can enforce and reports it (muster #457);
// HostSupport is its answer and `muster doctor` shows its facts.
//
// The Linux mechanism is declared only when four things are true, checked in this
// order so the reason names the first that is not: the architecture is one whose
// system call numbers were measured; the service has said which binary is its
// launcher; the kernel's Landlock is at least the ABI the policy needs (and says
// why, when it has none); and a process confined by a real policy, started
// through the launcher exactly as a session will be, was seen unable to do the
// things the capability claims. The last is not a formality, as macOS's probe is
// not: it is what finds a container profile that blocks the calls, a module that
// is built but not enabled, or a launcher path that is not the muster binary.
//
// The first three are read whatever the others say, because reading them costs
// nothing and an operator fixing one wants to know about the next. The fourth
// starts a process, and is run only when there is a launcher and a kernel to run
// it on. The active security modules are a fifth fact that decides nothing: the
// kernel's answer about Landlock is what is trusted, and the list is what an
// operator checks it against.
func CheckHost() *HostReport {
	rep := newHostReport(MechanismLinux)
	refuse := func(format string, args ...any) {
		rep.refuse(reasonPrefix + fmt.Sprintf(format, args...))
	}

	arch := hostGOARCH()
	if _, ok := TableFor(arch); ok {
		rep.add(FactArchitecture, "%s (its system call numbers were measured)", arch)
	} else {
		rep.add(FactArchitecture, "%s (its system call numbers have not been measured; amd64 and arm64 only)", arch)
		refuse("the system call numbers of %s have not been measured (amd64 and arm64 only), so a filter for it would be a guess", arch)
	}

	launcher := Launcher()
	if launcher == "" {
		rep.add(FactLauncher, "not named (this process has not named its own executable as the launcher)")
		refuse("this process has not named its own executable as the sandbox launcher, so it cannot start a session through one")
	} else {
		rep.add(FactLauncher, "named (this process can start a session through itself)")
	}

	abi, err := hostABI()
	switch {
	case err == syscall.ENOSYS:
		rep.add(FactLandlockABI, "none: the kernel has no Landlock (built without it, or a container's seccomp profile blocks the system call)")
		refuse("the kernel has no Landlock (built without it, or a container's seccomp profile blocks the system call)")
	case err == syscall.EOPNOTSUPP:
		rep.add(FactLandlockABI, "none: Landlock is built into the kernel but not enabled")
		refuse("Landlock is built into the kernel but not enabled (the boot parameter lsm= lacks landlock)")
	case err != nil:
		rep.add(FactLandlockABI, "unknown: asking the kernel for its Landlock version failed: %v", err)
		refuse("asking the kernel for its Landlock version: %v", err)
	case abi < MinLandlockABI:
		rep.add(FactLandlockABI, "%d, below the %d the mechanism needs (Linux 6.12)", abi, MinLandlockABI)
		refuse("Landlock ABI %d is below the %d this mechanism needs (Linux 6.12: abstract unix sockets and signals are scoped)", abi, MinLandlockABI)
	default:
		rep.add(FactLandlockABI, "%d (the mechanism needs %d or newer, Linux 6.12)", abi, MinLandlockABI)
	}

	if modules, err := hostLSM(); err != nil {
		rep.add(FactModules, "cannot be read: %v", err)
	} else {
		listed := strings.Join(modules, ", ")
		if listed == "" {
			listed = "none listed"
		}
		if !containsModule(modules, "landlock") {
			listed += "; landlock is not among them"
		}
		rep.add(FactModules, "%s", listed)
	}

	if rep.refused() {
		rep.add(FactSelfTest, "not run: an earlier check refused, and the test needs a launcher and a kernel with Landlock")
		return rep
	}
	if why := selfTest(launcher); why != "" {
		rep.add(FactSelfTest, "%s", why)
		rep.refuse(reasonPrefix + why)
		return rep
	}
	rep.add(FactSelfTest, "passed (a process confined the way a session will be could not do what the capability says it cannot)")
	rep.Support = &fleet.SandboxSupport{
		Mechanism:       MechanismLinux,
		Denies:          Denies(),
		Network:         []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
		PackageCache:    []fleet.PackageCacheMode{},
		PackageManagers: []fleet.PackageManager{},
	}
	return rep
}

func containsModule(modules []string, name string) bool {
	for _, m := range modules {
		if m == name {
			return true
		}
	}
	return false
}

// selfTest runs the launcher's in-process checks in a throwaway child, under a
// policy built the way a create builds one, and returns why they did not pass.
func selfTest(launcher string) string {
	dir, err := os.MkdirTemp("", "muster-sandbox-probe-")
	if err != nil {
		return "making a probe directory: " + err.Error()
	}
	defer os.RemoveAll(dir)
	inside := filepath.Join(dir, "inside")
	outside := filepath.Join(dir, "outside")
	for _, d := range []string{inside, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return "making a probe directory: " + err.Error()
		}
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("not for a session"), 0o600); err != nil {
		return "making a probe file: " + err.Error()
	}
	// A real listener, so a connect that wrongly succeeded is a success the
	// check can see rather than a refusal from a port nobody holds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "opening a probe listener: " + err.Error()
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	plan := &Plan{
		Mechanism: MechanismLinux,
		Network:   fleet.SandboxNetworkClosed,
		Read:      SystemReadRootsFor(MechanismLinux),
		Write:     []string{ResolvePath(inside)},
	}
	policy, err := plan.Policy()
	if err != nil {
		return "rendering the probe policy: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), selfTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, LauncherArg, "--policy", string(policy), "--self-test", ResolvePath(secret), strconv.Itoa(port), "--")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return fmt.Sprintf("the confined self-test did not finish in %s", selfTestTimeout)
	}
	if err != nil || text != "ok" {
		if text == "" {
			text = fmt.Sprint(err)
		}
		return "a process confined the way a session will be failed the self-test (" + oneLine(text) + ")"
	}
	return ""
}

// oneLine keeps a child's output to one short line of a reason.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
