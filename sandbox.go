package fleet

import (
	"fmt"
	"path/filepath"
	"strings"
)

// The sandbox a create may ask for (muster #281).
//
// Environment isolation (SessionSpec.IsolateEnvironment) decides what a session
// is HANDED. It does not decide what the session can REACH: a process started
// with a clean environment still reads the service user's home directory — git
// and forge CLI configuration, SSH keys, other projects — connects to the
// user's multiplexer and agent sockets, and talks to the desktop's system
// services. A sandbox is the second half: an operating-system profile the
// driver wraps the session's process in, denying all of that by default.
//
// # Refuse, never degrade
//
// A driver that cannot enforce the profile it was asked for refuses the create
// as unsupported, naming what is missing (§2.1, §5.6). It never starts the
// session with a weaker profile under the same name, and a platform that cannot
// deny every class below reports the capability (DriverCapabilities.Sandbox) as
// absent rather than offering something smaller. "Reads denied outside granted
// paths" on its own is not a sandbox: the profile must deny file access, unix
// socket connects and system-service lookups together, because each of the
// three is a way out of the other two.
//
// # What a caller can and cannot say
//
// A caller names ADDITIONAL paths and a network posture. The allow-lists that
// keep the runtime working — the system directories, the runtime's own install,
// the handful of system services a process needs to resolve names and read the
// clock — are carried in the driver, never in caller input, so a create cannot
// widen the profile into the classes it exists to deny.

// SandboxNetwork is the network posture of a sandboxed session.
//
// Only two postures exist because the platform sandbox can offer no more: it
// cannot filter by hostname, and the model API a session talks to needs the
// network. A capability that implied host filtering would be a claim the
// enforcement cannot back.
type SandboxNetwork string

const (
	// SandboxNetworkOpen allows outbound network connections. The default,
	// because a session that cannot reach its model provider cannot work.
	SandboxNetworkOpen SandboxNetwork = "open"
	// SandboxNetworkClosed allows loopback only: the session's own server and
	// nothing beyond the machine.
	SandboxNetworkClosed SandboxNetwork = "closed"
)

// PackageCacheMode says how a session reaches a package cache.
type PackageCacheMode string

const (
	// PackageCacheShared makes the named directory readable and WRITABLE to the
	// session as it is. Every session granted the same directory shares a
	// writable path — a side channel between concurrent sessions.
	PackageCacheShared PackageCacheMode = "shared"
	// PackageCachePrivate gives the session its own copy of the named directory,
	// made when the session starts (a copy-on-write clone where the filesystem
	// has one, a plain copy otherwise), and grants only the copy. No write path
	// is shared between sessions, and what the session installs never reaches
	// the original. The state report says which kind of copy was made.
	PackageCachePrivate PackageCacheMode = "private"
)

// SandboxPackageCache names a package cache a sandboxed session may use, so a
// dependency install is practical without opening the home directory.
type SandboxPackageCache struct {
	// Path is the cache directory on the machine the session runs on.
	Path AbsolutePath `json:"path"`
	// Mode defaults to private.
	Mode PackageCacheMode `json:"mode,omitempty"`
}

// SandboxSpec is the optional `sandbox` field of a create (muster #281).
//
// Its absence means no sandbox. Its presence, even as an empty object, asks for
// the default profile: the session's working directory and private directory
// readable and writable, the runtime's own install and the system readable,
// everything else denied, network open.
//
// The profile is not just a restriction on the runtime's own process. Every
// tool the session starts inherits it, which is the point — and the reason for
// two warnings that belong with the field:
//
//   - The working directory is UNTRUSTED once a sandboxed session has run in
//     it. The session controls any repository configuration inside it
//     (filter drivers, external diff programs, file-system monitors, hooks), so
//     a git command run OUTSIDE the profile on that directory afterwards —
//     by the caller — executes code the session chose. This service never runs
//     git on a session's working directory. A caller extracting the result
//     must do it inside the same profile, or with every exec-capable setting
//     overridden.
//   - Setting the working directory up is the caller's job, and it comes
//     first. A test loop that diffs against the repository's trunk needs the
//     trunk reference to exist before the session starts; it cannot be fetched
//     from inside the profile once the credentials are out of reach.
type SandboxSpec struct {
	// ReadPaths are directories readable in addition to the defaults.
	ReadPaths []AbsolutePath `json:"readPaths,omitempty"`
	// WritePaths are directories readable and writable in addition to the
	// defaults. The shared temporary directories can never be named.
	WritePaths []AbsolutePath `json:"writePaths,omitempty"`
	// Network defaults to open.
	Network SandboxNetwork `json:"network,omitempty"`
	// PackageCache, when set, grants a package cache; see SandboxPackageCache.
	PackageCache *SandboxPackageCache `json:"packageCache,omitempty"`
}

// SandboxDeny names a class of access the profile denies by default.
type SandboxDeny string

const (
	// SandboxDenyFiles: reading and writing files outside the granted paths.
	SandboxDenyFiles SandboxDeny = "files"
	// SandboxDenyUnixSockets: connecting to a unix-domain socket, other than
	// the system's name resolver. A path-based file deny does not cover a
	// connect, and the multiplexer's and the SSH agent's sockets are both
	// reachable that way — a command started through the first runs outside
	// the profile entirely.
	SandboxDenyUnixSockets SandboxDeny = "unixSockets"
	// SandboxDenySystemServices: looking up system services (the clipboard,
	// application launching, scripting of other applications).
	SandboxDenySystemServices SandboxDeny = "systemServices"
)

// SandboxSupport is what a driver declares about enforcing a sandbox
// (DriverCapabilities.Sandbox). A driver offering it enforces every class in
// Denies; one that cannot deny all of them does not declare it.
type SandboxSupport struct {
	// Mechanism names the host's own facility the driver uses.
	Mechanism string `json:"mechanism"`
	// Denies lists the access classes the profile denies by default. Always all
	// of SandboxDeny's values: it is reported so a reader confirms rather than
	// assumes, and so a future mechanism that denies fewer cannot be mistaken
	// for this one.
	Denies []SandboxDeny `json:"denies"`
	// Network lists the postures the mechanism can enforce.
	Network []SandboxNetwork `json:"network"`
	// PackageCache lists the package-cache modes the driver supports.
	PackageCache []PackageCacheMode `json:"packageCache"`
}

// SandboxState is what a running session reports about its own sandbox
// (SessionState.Sandbox): that it runs sandboxed, and under which profile — the
// paths and posture actually in force, defaults included, so a reader can
// confirm the profile rather than assume it from the request.
type SandboxState struct {
	// Mechanism is the host facility enforcing the profile.
	Mechanism string `json:"mechanism"`
	// Denies are the access classes denied by default.
	Denies []SandboxDeny `json:"denies"`
	// Network is the posture in force.
	Network SandboxNetwork `json:"network"`
	// ReadPaths and WritePaths are the directories granted, defaults and the
	// caller's together. Writable paths are also readable.
	ReadPaths  []AbsolutePath `json:"readPaths"`
	WritePaths []AbsolutePath `json:"writePaths"`
	// PackageCache is present when a cache was granted.
	PackageCache *SandboxPackageCacheState `json:"packageCache,omitempty"`
}

// SandboxPackageCacheState says how a package cache was provided.
type SandboxPackageCacheState struct {
	Mode PackageCacheMode `json:"mode"`
	// Path is the directory the session uses: the caller's own for shared, the
	// session's private copy for private.
	Path AbsolutePath `json:"path"`
	// Copy is how a private cache was made: "clone" (copy-on-write) or "copy".
	// Empty for shared.
	Copy string `json:"copy,omitempty"`
}

// Validate checks the shape of a sandbox request. It does not decide whether any
// driver can enforce it — that is the capability's question — only whether the
// request is well-formed and cannot widen the profile into what it denies.
func (s SandboxSpec) Validate() error {
	switch s.Network {
	case "", SandboxNetworkOpen, SandboxNetworkClosed:
	default:
		return fmt.Errorf("sandbox.network %q is not one of %q, %q", string(s.Network), SandboxNetworkOpen, SandboxNetworkClosed)
	}
	for _, p := range s.ReadPaths {
		if err := checkSandboxPath("sandbox.readPaths", p); err != nil {
			return err
		}
	}
	for _, p := range s.WritePaths {
		if err := checkSandboxPath("sandbox.writePaths", p); err != nil {
			return err
		}
		if sharedTemp(p) {
			return fmt.Errorf("sandbox.writePaths: %q is, or contains, a directory shared between sessions "+
				"(the system's temporary directories); each session has its own private temporary directory", string(p))
		}
	}
	if pc := s.PackageCache; pc != nil {
		if err := checkSandboxPath("sandbox.packageCache.path", pc.Path); err != nil {
			return err
		}
		switch pc.Mode {
		case "", PackageCacheShared, PackageCachePrivate:
		default:
			return fmt.Errorf("sandbox.packageCache.mode %q is not one of %q, %q",
				string(pc.Mode), PackageCacheShared, PackageCachePrivate)
		}
	}
	return nil
}

// checkSandboxPath enforces what every granted path must satisfy on every
// platform: absolute, already clean, free of anything that would need escaping
// inside a profile, and not the filesystem root.
func checkSandboxPath(field string, p AbsolutePath) error {
	s := string(p)
	switch {
	case s == "":
		return fmt.Errorf("%s: an empty path", field)
	case !filepath.IsAbs(s):
		return fmt.Errorf("%s: %q must be an absolute path", field, s)
	case filepath.Clean(s) != s:
		return fmt.Errorf("%s: %q must be a clean path (no .., ., doubled or trailing separators)", field, s)
	case s == "/":
		return fmt.Errorf("%s: the filesystem root cannot be granted", field)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return fmt.Errorf("%s: %q contains a control character, quote or backslash", field, s)
		}
	}
	if strings.Contains(s, "(") || strings.Contains(s, ")") || strings.Contains(s, ";") {
		return fmt.Errorf("%s: %q contains a parenthesis or semicolon", field, s)
	}
	return nil
}

// sharedTempRoots are the temporary directories every session on a machine
// shares. Writing there is a cross-session side channel, so no sandbox may be
// granted write access to one or to anything that contains one.
var sharedTempRoots = []string{
	"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/var", "/private/var",
	"/var/folders", "/private/var/folders", "/private",
}

// sharedTemp reports whether p is, or contains, a shared temporary directory.
func sharedTemp(p AbsolutePath) bool {
	s := string(p)
	for _, t := range sharedTempRoots {
		if s == t || strings.HasPrefix(t, s+"/") {
			return true
		}
	}
	return false
}
