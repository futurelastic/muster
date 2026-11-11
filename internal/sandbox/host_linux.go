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
)

// HostSupport reports what the host can enforce, or nil and the reason when it
// cannot — a statement the capability carries, not an error.
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
// The returned support names no package-cache mode: that is a decision of the
// driver, which fills it in (or leaves it empty).
func HostSupport() (*fleet.SandboxSupport, string) {
	refuse := func(format string, args ...any) (*fleet.SandboxSupport, string) {
		return nil, reasonPrefix + fmt.Sprintf(format, args...)
	}
	if _, ok := TableFor(hostGOARCH()); !ok {
		return refuse("the system call numbers of %s have not been measured (amd64 and arm64 only), so a filter for it would be a guess", hostGOARCH())
	}
	launcher := Launcher()
	if launcher == "" {
		return refuse("this process has not named its own executable as the sandbox launcher, so it cannot start a session through one")
	}
	abi, err := hostABI()
	switch {
	case err == syscall.ENOSYS:
		return refuse("the kernel has no Landlock (built without it, or a container's seccomp profile blocks the system call)")
	case err == syscall.EOPNOTSUPP:
		return refuse("Landlock is built into the kernel but not enabled (the boot parameter lsm= lacks landlock)")
	case err != nil:
		return refuse("asking the kernel for its Landlock version: %v", err)
	case abi < MinLandlockABI:
		return refuse("Landlock ABI %d is below the %d this mechanism needs (Linux 6.12: abstract unix sockets and signals are scoped)", abi, MinLandlockABI)
	}
	if why := selfTest(launcher); why != "" {
		return refuse("%s", why)
	}
	return &fleet.SandboxSupport{
		Mechanism:       MechanismLinux,
		Denies:          Denies(),
		Network:         []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
		PackageCache:    []fleet.PackageCacheMode{},
		PackageManagers: []fleet.PackageManager{},
	}, ""
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
