package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	fleet "github.com/futurelastic/muster"
)

// The Linux mechanism's policy and the way a driver launches a session under it
// (muster #439).
//
// Where macOS hands the host's launcher a profile written in the host's own
// language, Linux has no such facility: the kernel offers rules (Landlock) and a
// filter (seccomp), and something has to apply them to a process and then run the
// agent as that process. That something is the service's own binary, started in
// the place `sandbox-exec` is started on macOS: `muster __sandbox-exec <policy>
// -- <program> <args>`. It applies the policy to itself and replaces itself with
// the program, so every process the session starts inherits the confinement and
// the program keeps the pid and the command name the service already follows.
//
// This file is the part of that which is data: the policy a plan renders, and the
// argv that carries it. It has no build tag and makes no system call that is not
// portable, so every platform's tests can check what a driver would run; applying
// the policy is launcher_linux.go.

// MechanismLinux names the Linux mechanism, as the capability and the state
// report it.
const MechanismLinux = "linux-landlock-seccomp"

// LauncherArg is the first argument that turns the service's binary into the
// sandbox launcher. Hidden: it is not a command an operator types.
const LauncherArg = "__sandbox-exec"

// PolicyVersion is the version of the policy document. A launcher refuses a
// document of another version rather than guess what a field meant.
const PolicyVersion = 1

// MinLandlockABI is the oldest Landlock ABI the mechanism is offered on, and so
// the kernel it asks for (Linux 6.12). ABI 6 is the one that scopes abstract unix
// sockets and signals; without it a confined session can still signal the
// supervisor and connect to a service listening in the abstract namespace, and
// "denies unixSockets and systemServices" would be a claim the kernel does not
// back. The policy is the same on every kernel that offers it: a newer ABI does
// not make it harder, because one name would then mean two profiles.
const MinLandlockABI = 6

// LinuxPolicy is the document a plan renders for the launcher. It holds paths and
// a posture and nothing secret: it travels as an argument or as a file the
// session cannot write, and a credential still travels only in the environment.
type LinuxPolicy struct {
	Version int `json:"version"`
	// Network is "open" or "closed".
	Network string `json:"network"`
	// Read are the directories (or files) readable, writable ones included.
	Read []string `json:"read"`
	// Write are the directories (or files) readable and writable.
	Write []string `json:"write"`
	// Devices are device files and directories granted read, write and ioctl; one
	// that does not exist on this host is skipped, as an absent device is not an
	// error the way an absent grant is.
	Devices []string `json:"devices"`
	// OwnTTY grants the terminal the launcher itself runs on, when one of its
	// standard streams is a pseudo-terminal.
	OwnTTY bool `json:"ownTTY"`
	// BindTCP are the only TCP ports a session may bind under network "closed":
	// the port of the runtime server the service connects into.
	BindTCP []int `json:"bindTCP"`
}

// LinuxDevices is the complete list of device files and directories a confined
// session may use. The shared memory directory is not on it: it is one directory
// every session of a user shares. The pty directory is, because opening a new
// pseudo-terminal opens its peer by number, which no list of numbers can name in
// advance; a session can therefore open another pty of the same user, as it can
// on macOS.
var LinuxDevices = []string{
	"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom",
	"/dev/tty", "/dev/ptmx", "/dev/pts",
}

// linuxReadRoots are the directories every process needs to run at all on Linux:
// the system and its tools and libraries, the configuration a program reads to
// resolve users and names, and the two virtual file systems a runtime reads its
// own state from. They hold the system, not the user's data: nothing under a home
// directory, no /var, no /run, no /opt (a state directory could live there).
var linuxReadRoots = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/etc", "/proc", "/sys",
}

// linuxReadLinks are files under /etc that a distribution makes a link out of
// /etc, so granting /etc does not reach what they point at.
var linuxReadLinks = []string{"/etc/resolv.conf", "/etc/mtab"}

// SystemReadRootsFor is the system directories every process of that mechanism
// needs to read. An empty mechanism means macOS's.
func SystemReadRootsFor(mechanism string) []string {
	if mechanism != MechanismLinux {
		return append([]string(nil), SystemReadRoots...)
	}
	var out []string
	for _, p := range linuxReadRoots {
		if _, err := os.Stat(p); err == nil {
			out = append(out, ResolvePath(p))
		}
	}
	for _, p := range linuxReadLinks {
		if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
			out = append(out, r)
		}
	}
	return Dedupe(out)
}

// mechanism is the plan's mechanism; the empty plan is the macOS one, so every
// plan and every record written before there was a second mechanism keeps its
// meaning.
func (p *Plan) mechanism() string {
	if p.Mechanism == "" {
		return Mechanism
	}
	return p.Mechanism
}

// Policy renders the plan as the text the mechanism's launcher reads: profile
// text on macOS, a policy document on Linux.
func (p *Plan) Policy() ([]byte, error) {
	if p.mechanism() != MechanismLinux {
		s, err := p.Profile()
		return []byte(s), err
	}
	network := string(p.Network)
	if network == "" {
		network = string(fleet.SandboxNetworkOpen)
	}
	doc := LinuxPolicy{
		Version: PolicyVersion,
		Network: network,
		Read:    nonNil(p.Read),
		Write:   nonNil(p.Write),
		Devices: append([]string(nil), LinuxDevices...),
		OwnTTY:  true,
		BindTCP: append([]int{}, p.BindTCP...),
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return append([]string(nil), in...)
}

// Validate is the shape rule the launcher applies before it applies anything.
func (d LinuxPolicy) Validate() error {
	if d.Version != PolicyVersion {
		return fmt.Errorf("policy version %d is not the %d this launcher reads", d.Version, PolicyVersion)
	}
	switch fleet.SandboxNetwork(d.Network) {
	case fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed:
	default:
		return fmt.Errorf("policy network %q is not one of %q, %q", d.Network, fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed)
	}
	for _, list := range [][]string{d.Read, d.Write, d.Devices} {
		for _, p := range list {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
				return fmt.Errorf("policy path %q is not an absolute, clean path below the root", p)
			}
		}
	}
	for _, port := range d.BindTCP {
		if port < 1 || port > 65535 {
			return fmt.Errorf("policy bind port %d is not a port", port)
		}
	}
	if d.Network == string(fleet.SandboxNetworkOpen) && len(d.BindTCP) > 0 {
		return errors.New("policy names bind ports under an open network, where nothing is restricted")
	}
	return nil
}

// ProfileFileName is the name a driver gives the file it writes the policy to.
func (p *Plan) ProfileFileName() string {
	if p.mechanism() == MechanismLinux {
		return "policy.json"
	}
	return "profile.sb"
}

// launcher is the path of the binary that carries the Linux launcher. It is set
// once by the service's composition root, to the path of its own executable,
// before any driver is built.
var (
	launcherMu   sync.RWMutex
	launcherPath string
)

// SetLauncher names the binary that runs the Linux launcher when started with
// LauncherArg: the service's own executable. Without it the Linux mechanism is
// not offered, because there would be nothing to start a session through.
func SetLauncher(path string) {
	launcherMu.Lock()
	defer launcherMu.Unlock()
	launcherPath = path
}

// SelfLauncher is the path of the running executable as a launcher can be
// started by it, or empty when it cannot be named. A binary replaced by an
// upgrade while the service runs reads as "<path> (deleted)"; the path itself now
// holds the new build, which speaks the same policy version, so the suffix is
// dropped and the path is checked for what it is.
func SelfLauncher() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	info, err := os.Stat(exe)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return exe
}

// Launcher is the path SetLauncher named, or empty.
func Launcher() string {
	launcherMu.RLock()
	defer launcherMu.RUnlock()
	return launcherPath
}

// errNoLauncher is what wrapping a Linux argv answers when the service did not
// say which binary is the launcher.
var errNoLauncher = errors.New("the service has not named its own executable as the sandbox launcher, so a session cannot be started under the Linux mechanism")

// Wrap turns the program's argv into the one that runs it under the plan's
// policy, read from the file named. On macOS that is exactly
// `sandbox-exec -f <file> <argv...>`.
func (p *Plan) Wrap(file string, argv []string) ([]string, error) {
	if p.mechanism() != MechanismLinux {
		return append([]string{ExecPath, "-f", file}, argv...), nil
	}
	l := Launcher()
	if l == "" {
		return nil, errNoLauncher
	}
	return append([]string{l, LauncherArg, "--policy-file", file, "--"}, argv...), nil
}

// WrapInline is Wrap with the policy carried in the argument list instead of a
// file: for a session whose own directory is writable by the session, where a
// file could be changed before a relaunch reads it. On macOS that is exactly
// `sandbox-exec -p <profile> <argv...>`.
func (p *Plan) WrapInline(argv []string) ([]string, error) {
	text, err := p.Policy()
	if err != nil {
		return nil, err
	}
	if p.mechanism() != MechanismLinux {
		return append([]string{ExecPath, "-p", string(text)}, argv...), nil
	}
	l := Launcher()
	if l == "" {
		return nil, errNoLauncher
	}
	return append([]string{l, LauncherArg, "--policy", string(text), "--"}, argv...), nil
}

// PruneMissing returns the paths that exist, in order. A Linux rule is attached to
// a path that already exists, so a driver-carried grant that does not (a directory
// an agent has not made yet) is not one the plan may report as granted.
func PruneMissing(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// EnsureDirs makes each directory, owner-only: the grants a driver carries for
// the directories an agent writes to as it works (its transcripts) must exist
// before the policy that names them is applied.
func EnsureDirs(paths []string) error {
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("making %s: %w", p, err)
		}
	}
	return nil
}

// RequireExist refuses a caller-named grant that does not exist, naming the field.
// On Linux a path is granted by opening it, so a request for one that is not there
// cannot be honoured, and starting without it would be a weaker profile than the
// one asked for.
func RequireExist(field string, paths []string) error {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("%s: %q does not exist, and the Linux sandbox grants a path by opening it, so it must exist when the session starts", field, p)
		}
	}
	return nil
}
