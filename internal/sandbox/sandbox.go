// Package sandbox renders and probes the operating-system sandbox profile the
// drivers wrap a session's agent in (muster #281, #345).
//
// It is the one place the profile is written, so the two drivers that offer a
// sandbox — the one that owns its runtime's server and the one that starts
// sessions inside the user's terminal multiplexer — cannot drift into denying
// different things under the same name.
//
// # The mechanism, and why it replaces rather than nests
//
// On macOS the profile is applied with the host's own facility (sandbox-exec).
// Every process the session starts inherits it, so a tool the agent runs is
// confined as tightly as the agent itself. A macOS profile cannot be nested:
// applying a second one from inside the first fails with "Operation not
// permitted", so a runtime that ships its own sandbox must be told to stand
// down, and a service that itself runs inside a profile cannot offer this one
// (HostSupport applies a profile once to find out, rather than discovering it at
// the first create).
//
// # The Linux mechanism
//
// Linux has no host facility that takes a profile, so the same contract is met by
// two kernel pieces applied by the service's own binary to itself before it
// becomes the agent (muster #439): Landlock for files — and, from Linux 6.12, for
// abstract unix sockets and signals — and a seccomp filter for everything a file
// rule cannot name (unix socket creation, the network under a closed posture,
// shared memory and queues, io_uring). The policy is the document policy.go
// renders, the launcher is launcher_linux.go, and the host is offered the
// mechanism only after a process confined that way was seen unable to do each
// thing (host_linux.go). Where the kernel cannot back all three classes the
// capability is absent, with the reason, exactly as on any platform that lacks one.
//
// What differs from macOS is stated rather than hidden: a closed network is not
// "loopback only" there but no outbound connection at all, because the kernel's
// network rules cannot tell loopback from anywhere else; and a session cannot
// create a unix socket, where on macOS it can create one it may not connect.
//
// # What the profile denies
//
// Three classes, together, because each is a way out of the other two:
//
//   - FILES. Reads and writes are denied everywhere and granted per path. The
//     metadata of a path (does it exist, how large) stays readable, so a program
//     can walk to a directory it was granted; the contents of anything else are
//     refused.
//   - UNIX SOCKETS. A path-based file deny does not cover connecting to a
//     socket. Measured: inside a profile that denied the home directory, the
//     multiplexer's server socket was still connectable — and a command started
//     through it runs OUTSIDE the profile, with the user's full home — and so
//     was the SSH agent's. Connects are denied outright; only the system's name
//     resolver is allowed back, and only while the network is open.
//   - SYSTEM SERVICES. With the two above closed, the clipboard was still
//     readable, `open` still launched applications outside the profile and
//     AppleEvents still drove other running applications. Every service lookup
//     is denied except a short allow-list a process needs to resolve users,
//     names and the clock. The list is carried here, in the driver; no create
//     can add to it. The keychain's services are not on it: allowing them would
//     open every item the user has to the session.
//
// A platform where the host cannot deny all three does not declare the
// capability rather than offering a weaker profile under the same name.
//
// # What it does not do
//
// Network posture is open or closed, nothing finer: the facility cannot filter
// by hostname, and the model API needs the network. The model provider key is
// in the session's environment and the profile does not hide it from the
// session's own tools. And the profile confines a session; it does not make its
// working directory trustworthy afterwards (see fleet.SandboxSpec).
package sandbox

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// ExecPath is the macOS sandbox launcher. Absolute on purpose: a PATH lookup is
// exactly the kind of resolution a session's environment must not influence.
const ExecPath = "/usr/bin/sandbox-exec"

// Mechanism names the facility, as the capability and the state report it.
const Mechanism = "macos-sandbox-exec"

// SystemReadRoots are the directories every process needs to run at all. They
// hold the system and its tools, not the user's data: nothing under a home
// directory is here.
var SystemReadRoots = []string{
	"/usr", "/bin", "/sbin", "/System", "/Library",
	"/private/etc", "/private/var/db", "/private/var/select",
	"/dev", "/opt/homebrew", "/Applications/Xcode.app",
}

// DeviceWrites are the few device files a program writes to as a matter of
// course. Nothing else under /dev is writable.
var DeviceWrites = []string{"/dev/null", "/dev/zero", "/dev/tty", "/dev/dtracehelper", "/dev/ptmx"}

// MachAllow is the complete list of system services a sandboxed session may
// look up. Everything else — the clipboard, application launching, scripting
// of other applications, the keychain — is denied. Measured against a shell,
// git, curl, node and a package manager making real requests.
var MachAllow = []string{
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.system.DirectoryService.libinfo_v1",
	"com.apple.system.notification_center",
	"com.apple.system.logger",
	"com.apple.logd",
	"com.apple.diagnosticd",
	"com.apple.cfprefsd.daemon",
	"com.apple.SystemConfiguration.configd",
	"com.apple.SystemConfiguration.DNSConfiguration",
	"com.apple.dnssd.service",
	"com.apple.mDNSResponder",
	"com.apple.trustd",
	"com.apple.trustd.agent",
	"com.apple.bsd.dirhelper",
	"com.apple.FSEvents",
}

// ResolverSocket is the system name resolver's socket: the one unix-domain
// socket a session may connect to, and only while the network is open.
const ResolverSocket = "/private/var/run/mDNSResponder"

// Denies is the list of access classes every profile denies, in the order the
// capability and the state report them.
func Denies() []fleet.SandboxDeny {
	return []fleet.SandboxDeny{fleet.SandboxDenyFiles, fleet.SandboxDenyUnixSockets, fleet.SandboxDenySystemServices}
}

// ResolvePath resolves symlinks in p when it exists, because the facility
// matches real paths: a grant on /tmp/x does not cover /private/tmp/x. A path
// that does not exist is returned cleaned, as given.
func ResolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// CoversOrIs reports whether granting root would include path (root equals path
// or is an ancestor).
func CoversOrIs(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// InstallRoots are the directories holding the runtime itself, which the
// session must be able to read to execute it: the binary's directory, and its
// parent when that is the conventional <prefix>/bin layout — unless the parent
// would be the service user's home or one of its ancestors, which no layout
// justifies opening.
func InstallRoots(bin, home string) []string {
	resolved := ResolvePath(bin)
	dir := filepath.Dir(resolved)
	roots := []string{dir}
	if filepath.Base(dir) == "bin" {
		parent := filepath.Dir(dir)
		if home == "" || !CoversOrIs(parent, home) {
			roots = append(roots, parent)
		}
	}
	return roots
}

// Dedupe returns in without repeats, sorted.
func Dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Plan is a resolved sandbox request: every path real, every default applied,
// ready to be rendered as a profile and reported back.
type Plan struct {
	// Mechanism names the facility the plan is for: MechanismLinux, or empty for
	// the macOS facility (the only one before #439, so a plan that does not say
	// keeps its meaning).
	Mechanism string
	// Network is the posture in force; empty means open.
	Network fleet.SandboxNetwork
	// Read is every readable directory (writable ones included).
	Read []string
	// Write is every writable directory.
	Write []string
	// BindTCP are the ports a session may bind while the network is closed (the
	// port of the runtime server a driver connects into). Only the Linux
	// mechanism reads it: the macOS profile allows loopback as a whole.
	BindTCP []int
}

// State renders the plan as the report a session carries. A driver that has
// more to say (a package cache) adds it to the returned value.
func (p *Plan) State() *fleet.SandboxState {
	toPaths := func(in []string) []fleet.AbsolutePath {
		out := make([]fleet.AbsolutePath, len(in))
		for i, s := range in {
			out[i] = fleet.AbsolutePath(s)
		}
		return out
	}
	network := p.Network
	if network == "" {
		network = fleet.SandboxNetworkOpen
	}
	return &fleet.SandboxState{
		Mechanism:  p.mechanism(),
		Denies:     Denies(),
		Network:    network,
		ReadPaths:  toPaths(p.Read),
		WritePaths: toPaths(p.Write),
	}
}

// sbplString quotes s as a profile string literal. A path with a control
// character cannot be quoted safely and is refused.
func sbplString(s string) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("path %q contains a control character", s)
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}

// Profile renders the plan as sandbox-exec profile text.
//
// Later rules win over earlier ones in this language, so the order is the
// meaning: allow everything, then take away files, sockets and services, then
// give back the allow-lists.
func (p *Plan) Profile() (string, error) {
	var b strings.Builder
	subpaths := func(op string, paths []string) error {
		if len(paths) == 0 {
			return nil
		}
		b.WriteString("(allow " + op)
		for _, s := range paths {
			q, err := sbplString(s)
			if err != nil {
				return err
			}
			b.WriteString("\n  (subpath " + q + ")")
		}
		b.WriteString(")\n")
		return nil
	}

	b.WriteString("(version 1)\n(allow default)\n\n")
	b.WriteString(";; files: denied everywhere, granted per path. Metadata stays readable so a\n")
	b.WriteString(";; program can walk to a directory it was granted.\n")
	b.WriteString("(deny file-read* file-write*)\n(allow file-read-metadata)\n(allow file-read* (literal \"/\"))\n")
	if err := subpaths("file-read*", p.Read); err != nil {
		return "", err
	}
	if err := subpaths("file-write*", p.Write); err != nil {
		return "", err
	}
	b.WriteString("(allow file-write*")
	for _, s := range DeviceWrites {
		q, _ := sbplString(s)
		b.WriteString("\n  (literal " + q + ")")
	}
	b.WriteString("\n  (subpath \"/dev/fd\")\n  (regex #\"^/dev/ttys[0-9]+$\"))\n\n")

	b.WriteString(";; unix sockets: no connect, except (network open only) the name resolver.\n")
	b.WriteString("(deny network-outbound (remote unix-socket))\n")
	if p.Network == fleet.SandboxNetworkClosed {
		b.WriteString(";; network closed: loopback only.\n")
		b.WriteString("(deny network-outbound)\n(allow network-outbound (remote ip \"localhost:*\"))\n")
		b.WriteString("(deny network-inbound)\n(allow network-inbound (local ip \"localhost:*\"))\n")
	} else {
		q, _ := sbplString(ResolverSocket)
		b.WriteString("(allow network-outbound (literal " + q + "))\n")
	}
	b.WriteString("\n;; system services: none but the allow-list.\n")
	b.WriteString("(deny mach-lookup)\n(deny appleevent-send)\n(deny lsopen)\n(allow mach-lookup")
	for _, n := range MachAllow {
		q, _ := sbplString(n)
		b.WriteString("\n  (global-name " + q + ")")
	}
	b.WriteString(")\n")
	return b.String(), nil
}
