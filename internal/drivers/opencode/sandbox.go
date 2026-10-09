package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// The sandbox a session may be started under (muster #281).
//
// # The mechanism, and why it replaces rather than nests
//
// On macOS the driver wraps the runtime's process in the host's own sandbox
// facility (sandbox-exec, with a profile it generates). Every process the
// session starts inherits the profile, so a tool the agent runs is confined as
// tightly as the runtime itself.
//
// A macOS profile cannot be nested: applying a second one from inside the first
// fails with "Operation not permitted". A harness that ships its own sandbox
// therefore fails closed under this one and refuses every shell command, so the
// driver's profile REPLACES the harness's own, and a driver whose runtime has
// one must tell it to stand down. This runtime has none, so there is nothing to
// switch off; the rule is recorded here because the next driver inherits it.
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
//     can add to it.
//
// A platform where the host cannot deny all three does not declare the
// capability (see sandboxSupport) rather than offering a weaker profile under
// the same name.
//
// # What it does not do
//
// Network posture is open or closed, nothing finer: the facility cannot filter
// by hostname, and the model API needs the network. The model provider key is
// in the session's environment and the profile does not hide it from the
// session's own tools. And the profile confines a session; it does not make its
// working directory trustworthy afterwards (see fleet.SandboxSpec).

// sandboxExecPath is the macOS sandbox launcher. Absolute on purpose: a PATH
// lookup is exactly the kind of resolution a session's environment must not
// influence.
const sandboxExecPath = "/usr/bin/sandbox-exec"

// sandboxMechanism names the facility, as the capability and the state report it.
const sandboxMechanism = "macos-sandbox-exec"

// systemReadRoots are the directories every process needs to run at all. They
// hold the system and its tools, not the user's data: nothing under a home
// directory is here.
var systemReadRoots = []string{
	"/usr", "/bin", "/sbin", "/System", "/Library",
	"/private/etc", "/private/var/db", "/private/var/select",
	"/dev", "/opt/homebrew", "/Applications/Xcode.app",
}

// deviceWrites are the few device files a program writes to as a matter of
// course. Nothing else under /dev is writable.
var deviceWrites = []string{"/dev/null", "/dev/zero", "/dev/tty", "/dev/dtracehelper", "/dev/ptmx"}

// machAllow is the complete list of system services a sandboxed session may
// look up. Everything else — the clipboard, application launching, scripting
// of other applications — is denied. Measured against a shell, git, curl, node
// and a package manager making real requests.
var machAllow = []string{
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

// resolverSocket is the system name resolver's socket: the one unix-domain
// socket a session may connect to, and only while the network is open.
const resolverSocket = "/private/var/run/mDNSResponder"

// packageCacheEnv names the variable that tells a session where its package
// cache is. A driver-owned name: a create may not set it.
const packageCacheEnv = "MUSTER_PACKAGE_CACHE"

// sandboxSupport reports what this driver can enforce on this host, or nil when
// it cannot — which is a statement the capability carries, not an error.
//
// The probe actually applies a profile once: a service that itself runs inside
// a sandbox cannot start a nested one, and discovering that at the first create
// would refuse a caller for a reason the capability should have told it.
func sandboxSupport(shared bool) (*fleet.SandboxSupport, string) {
	if shared {
		return nil, "this driver is in shared mode (one server for every session), which cannot confine one of them"
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Sprintf("no sandbox mechanism is implemented for %s (only macOS's sandbox-exec is)", runtime.GOOS)
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return nil, sandboxExecPath + " is not present"
	}
	out, err := exec.Command(sandboxExecPath, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
	if err != nil {
		return nil, fmt.Sprintf("the host refused to apply a sandbox profile (%s): the service may itself be running inside one, which cannot be nested",
			strings.TrimSpace(string(out)))
	}
	return &fleet.SandboxSupport{
		Mechanism: sandboxMechanism,
		Denies:    []fleet.SandboxDeny{fleet.SandboxDenyFiles, fleet.SandboxDenyUnixSockets, fleet.SandboxDenySystemServices},
		Network:   []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
		PackageCache: []fleet.PackageCacheMode{
			fleet.PackageCacheShared, fleet.PackageCachePrivate,
		},
	}, ""
}

// sandboxPlan is a resolved sandbox request: every path real, every default
// applied, ready to be rendered as a profile and reported back.
type sandboxPlan struct {
	network fleet.SandboxNetwork
	read    []string
	write   []string
	cache   *fleet.SandboxPackageCacheState
}

// state renders the plan as the report a session carries.
func (p *sandboxPlan) state() *fleet.SandboxState {
	toPaths := func(in []string) []fleet.AbsolutePath {
		out := make([]fleet.AbsolutePath, len(in))
		for i, s := range in {
			out[i] = fleet.AbsolutePath(s)
		}
		return out
	}
	st := &fleet.SandboxState{
		Mechanism:  sandboxMechanism,
		Denies:     []fleet.SandboxDeny{fleet.SandboxDenyFiles, fleet.SandboxDenyUnixSockets, fleet.SandboxDenySystemServices},
		Network:    p.network,
		ReadPaths:  toPaths(p.read),
		WritePaths: toPaths(p.write),
	}
	if p.cache != nil {
		c := *p.cache
		st.PackageCache = &c
	}
	return st
}

// real resolves symlinks in p when it exists, because the facility matches real
// paths: a grant on /tmp/x does not cover /private/tmp/x. A path that does not
// exist is returned cleaned, as given.
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// coversOrIs reports whether granting root would include path (root equals path
// or is an ancestor).
func coversOrIs(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// installRoots are the directories holding the runtime itself, which the
// session must be able to read to execute it: the binary's directory, and its
// parent when that is the conventional <prefix>/bin layout — unless the parent
// would be the service user's home or one of its ancestors, which no layout
// justifies opening.
func installRoots(bin, home string) []string {
	resolved := resolvePath(bin)
	dir := filepath.Dir(resolved)
	roots := []string{dir}
	if filepath.Base(dir) == "bin" {
		parent := filepath.Dir(dir)
		if home == "" || !coversOrIs(parent, home) {
			roots = append(roots, parent)
		}
	}
	return roots
}

// planSandbox resolves a create's sandbox request against this driver's
// defaults, validates it, and — for a private package cache — makes the
// session's copy. It runs before the server starts and before any directory
// other than sessionDir exists, so a refusal has no side effect to undo beyond
// sessionDir, which the caller removes.
func (d *Driver) planSandbox(spec fleet.SessionSpec, sessionDir string) (*sandboxPlan, error) {
	sb := spec.Sandbox
	invalid := func(msg string, args ...any) error {
		return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + fmt.Sprintf(msg, args...), Machine: d.machine}
	}
	if err := sb.Validate(); err != nil {
		return nil, invalid("%s", err.Error())
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		home = resolvePath(home)
	}
	// A grant that would include the service user's home puts back exactly what
	// the profile exists to remove.
	check := func(field string, p fleet.AbsolutePath) (string, error) {
		r := resolvePath(string(p))
		if home != "" && coversOrIs(r, home) {
			return "", invalid("%s: %q is, or contains, the service user's home directory; grant the specific directory a session needs", field, string(p))
		}
		return r, nil
	}

	cwd := resolvePath(string(spec.Cwd))
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, invalid("cwd %q is not a directory that exists, which a sandboxed session needs to be started in", string(spec.Cwd))
	}
	if home != "" && coversOrIs(cwd, home) {
		return nil, invalid("cwd %q is, or contains, the service user's home directory, which a sandboxed session would then write to", string(spec.Cwd))
	}

	plan := &sandboxPlan{network: sb.Network}
	if plan.network == "" {
		plan.network = fleet.SandboxNetworkOpen
	}

	write := []string{cwd, resolvePath(sessionDir)}
	read := append([]string(nil), systemReadRoots...)
	read = append(read, installRoots(d.bin, home)...)
	for _, p := range d.sandboxRead {
		read = append(read, resolvePath(p))
	}
	for _, p := range sb.ReadPaths {
		r, err := check("sandbox.readPaths", p)
		if err != nil {
			return nil, err
		}
		read = append(read, r)
	}
	for _, p := range sb.WritePaths {
		r, err := check("sandbox.writePaths", p)
		if err != nil {
			return nil, err
		}
		write = append(write, r)
	}

	if pc := sb.PackageCache; pc != nil {
		src, err := check("sandbox.packageCache.path", pc.Path)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			return nil, invalid("sandbox.packageCache.path %q is not a directory that exists", string(pc.Path))
		}
		mode := pc.Mode
		if mode == "" {
			mode = fleet.PackageCachePrivate
		}
		st := &fleet.SandboxPackageCacheState{Mode: mode}
		switch mode {
		case fleet.PackageCacheShared:
			st.Path = fleet.AbsolutePath(src)
			write = append(write, src)
		case fleet.PackageCachePrivate:
			dst := filepath.Join(resolvePath(sessionDir), "pkgcache")
			how, err := copyTree(src, dst)
			if err != nil {
				return nil, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: fmt.Sprintf("create: copying the package cache for this session: %v", err), Machine: d.machine}
			}
			st.Path, st.Copy = fleet.AbsolutePath(dst), how
			// dst is inside the session directory already granted writable.
		}
		plan.cache = st
	}

	plan.read = dedupe(append(read, write...))
	plan.write = dedupe(write)
	return plan, nil
}

// copyTree makes dst a copy of src, copy-on-write where the filesystem offers
// it, and reports which it was. `cp -c` fails rather than copying when the
// filesystem cannot clone, so the fallback is a separate, plain copy — and the
// report says so, since "instant" and "slow" are not the same promise.
func copyTree(src, dst string) (string, error) {
	clone, cloneErr := exec.Command("/bin/cp", "-cR", src, dst).CombinedOutput()
	if cloneErr == nil {
		return "clone", nil
	}
	_ = os.RemoveAll(dst)
	if plain, err := exec.Command("/bin/cp", "-R", src, dst).CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s / %s", err, strings.TrimSpace(string(clone)), strings.TrimSpace(string(plain)))
	}
	return "copy", nil
}

func dedupe(in []string) []string {
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

// profile renders the plan as sandbox-exec profile text.
//
// Later rules win over earlier ones in this language, so the order is the
// meaning: allow everything, then take away files, sockets and services, then
// give back the allow-lists.
func (p *sandboxPlan) profile() (string, error) {
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
	if err := subpaths("file-read*", p.read); err != nil {
		return "", err
	}
	if err := subpaths("file-write*", p.write); err != nil {
		return "", err
	}
	b.WriteString("(allow file-write*")
	for _, s := range deviceWrites {
		q, _ := sbplString(s)
		b.WriteString("\n  (literal " + q + ")")
	}
	b.WriteString("\n  (subpath \"/dev/fd\")\n  (regex #\"^/dev/ttys[0-9]+$\"))\n\n")

	b.WriteString(";; unix sockets: no connect, except (network open only) the name resolver.\n")
	b.WriteString("(deny network-outbound (remote unix-socket))\n")
	if p.network == fleet.SandboxNetworkClosed {
		b.WriteString(";; network closed: loopback only.\n")
		b.WriteString("(deny network-outbound)\n(allow network-outbound (remote ip \"localhost:*\"))\n")
		b.WriteString("(deny network-inbound)\n(allow network-inbound (local ip \"localhost:*\"))\n")
	} else {
		q, _ := sbplString(resolverSocket)
		b.WriteString("(allow network-outbound (literal " + q + "))\n")
	}
	b.WriteString("\n;; system services: none but the allow-list.\n")
	b.WriteString("(deny mach-lookup)\n(deny appleevent-send)\n(deny lsopen)\n(allow mach-lookup")
	for _, n := range machAllow {
		q, _ := sbplString(n)
		b.WriteString("\n  (global-name " + q + ")")
	}
	b.WriteString(")\n")
	return b.String(), nil
}
