package fleet

import (
	"fmt"
	"slices"
)

// The session posture a create may ask for (muster #344).
//
// A caller that drives several runtimes used to have to know, per runtime, how
// that runtime spells "run unattended and confined": one permission-mode flag,
// one pre-approval mechanism, whether a sandbox is accepted at all. A field the
// caller did not know for a runtime was dropped, and the session started with
// that runtime's default posture — nothing confined, a permission prompt that
// can park it for good.
//
// A Posture says what the work NEEDS, never how a runtime spells it. The
// service lowers it onto the per-runtime fields that already exist —
// SessionSpec.PermissionMode, SessionSpec.Sandbox, SessionSpec.IsolateEnvironment
// and SessionSpec.IsolateConfiguration (muster #392) — using only what the
// runtime's own DriverCapabilities declare, and refuses the create when the runtime cannot
// enforce all of it. Nothing starts with a weaker posture than the one asked.
//
// # One mapping, not one per driver
//
// What a runtime can enforce is already a function of capabilities it declares
// (Sandbox, CreatePermissionModes, IsolatesEnvironment, IsolatesConfiguration),
// so the mapping lives here, once. The dry run, the capability report and the create itself all call
// the same functions, so they cannot disagree; and the driver that finally
// receives the lowered fields still enforces them and still refuses what it
// cannot (§2.1) — this is a second reading of the same capability, not a second
// place the enforcement is decided.
//
// The per-runtime fields keep working, unchanged, for one release; they are
// deprecated in favour of this one (docs/api.md).

// ReasonPostureUnenforceable marks the unsupported (501, non-retryable) a create
// returns when the runtime it resolved cannot enforce the posture asked for
// (muster #344). The message names what is missing.
const ReasonPostureUnenforceable = "posture-unenforceable"

// Attendance says whether the session may stop to ask a person.
type Attendance string

const (
	// AttendanceAttended is the ordinary posture: the runtime asks before it
	// acts, and a person answers. The default.
	AttendanceAttended Attendance = "attended"
	// AttendanceUnattended means no permission prompts: the session never parks
	// on a question nobody is there to answer.
	//
	// It always implies a confined session (see Posture.Lower): a prompt-free
	// agent that can read the whole user account is the failure this field
	// exists to prevent, so a runtime that cannot confine one refuses. A caller
	// that really wants prompt-free and unconfined keeps using the per-runtime
	// `permissionMode: "bypass"`.
	AttendanceUnattended Attendance = "unattended"
)

// Configuration says where the session's own configuration comes from: the
// settings layers it loads and the tool servers it connects to (muster #392).
type Configuration string

const (
	// ConfigurationInherit is today's behaviour and the default: the session
	// also loads the configuration of the account the service runs as — the
	// user-level settings file with its hooks, plugins and permission rules, and
	// the user-level tool servers.
	ConfigurationInherit Configuration = "inherit"
	// ConfigurationGiven means the session's configuration is exactly what the
	// create named — its `settings`, its `mcpConfig` and its `contextRef` — plus
	// the working directory's own project settings, and nothing from the account
	// the service runs as. A tool server the account adds later is not open to
	// it: the guarantee is that nothing is inherited, not that named servers are
	// denied one by one.
	//
	// An `agent` the create names must be defined in the working directory's
	// project layer; one defined only at user level is not found (the runtime
	// refuses to start, which is visible, not a degraded session).
	ConfigurationGiven Configuration = "given"
)

// PostureFiles is the file-system part of a posture. Its presence asks for file
// access to be confined to the session's working directory and private
// directory, plus the paths named here.
type PostureFiles struct {
	// WritePaths are directories readable and writable in addition to the
	// working directory — for example a linked worktree's git common
	// directory. The shared temporary directories can never be named.
	WritePaths []AbsolutePath `json:"writePaths,omitempty"`
	// ReadPaths are directories readable in addition to the defaults.
	ReadPaths []AbsolutePath `json:"readPaths,omitempty"`
}

// PosturePackageCache is the package-cache part of a posture (muster #432): a
// directory the operator keeps warm, which each session starts from a copy of.
//
// Naming one implies a confined session, as unattended does: the guarantee is
// that a session cannot write to the seed, and nothing but the sandbox stops an
// unconfined process writing wherever the account can.
type PosturePackageCache struct {
	// Seed is the directory on the machine the session runs on. It holds one
	// directory per package manager the session uses — `cargo/`, `go/`, `npm/`.
	// Refreshing it is the operator's job; a session never writes to it.
	Seed AbsolutePath `json:"seed"`
	// Mode is "private" (the default): the session starts from its own copy of
	// the seed, so what it installs reaches no other session and not the seed. The
	// alternative, "shared", grants the seed itself WRITABLE to every session that
	// names it, which removes the wall between sessions (docs/api.md); it is not
	// recommended for an unattended one.
	Mode PackageCacheMode `json:"mode,omitempty"`
	// Managers names the package managers the session uses, which is what decides
	// the variables it is handed: CARGO_HOME for "cargo", GOMODCACHE for "go",
	// npm_config_cache for "npm", each pointing at that manager's directory in the
	// session's copy. At least one; a manager whose directory the seed lacks
	// refuses the create.
	Managers []PackageManager `json:"managers"`
}

// Posture is the optional `posture` field of a create (muster #344). Every field
// present is a requirement; an absent one asks for nothing.
type Posture struct {
	// Attendance defaults to attended.
	Attendance Attendance `json:"attendance,omitempty"`
	// Files, when present, confines file access (even as an empty object: the
	// working directory and nothing else beyond the defaults).
	Files *PostureFiles `json:"files,omitempty"`
	// Network is "open" or "closed". Closed allows loopback only and, like
	// Files, needs a runtime that can confine the session.
	Network SandboxNetwork `json:"network,omitempty"`
	// IsolatedEnvironment requires the session's process to carry only a built
	// environment (SessionSpec.IsolateEnvironment).
	IsolatedEnvironment bool `json:"isolatedEnvironment,omitempty"`
	// Configuration is "inherit" (the default) or "given" (muster #392): with
	// "given" the session loads only the configuration the create named. Like
	// IsolatedEnvironment it only narrows what the session reaches.
	Configuration Configuration `json:"configuration,omitempty"`
	// PackageCache, when present, starts the session from a package-cache seed
	// and points the named package managers at it (muster #432).
	PackageCache *PosturePackageCache `json:"packageCache,omitempty"`
}

// Unattended reports whether the posture asks for a prompt-free session.
func (p Posture) Unattended() bool { return p.Attendance == AttendanceUnattended }

// Validate checks the shape of a posture, with the same path rules a sandbox's
// granted paths follow. It does not decide whether any runtime can enforce it.
func (p Posture) Validate() error {
	switch p.Attendance {
	case "", AttendanceAttended, AttendanceUnattended:
	default:
		return fmt.Errorf("posture.attendance %q is not one of %q, %q", string(p.Attendance), AttendanceAttended, AttendanceUnattended)
	}
	switch p.Network {
	case "", SandboxNetworkOpen, SandboxNetworkClosed:
	default:
		return fmt.Errorf("posture.network %q is not one of %q, %q", string(p.Network), SandboxNetworkOpen, SandboxNetworkClosed)
	}
	switch p.Configuration {
	case "", ConfigurationInherit, ConfigurationGiven:
	default:
		return fmt.Errorf("posture.configuration %q is not one of %q, %q", string(p.Configuration), ConfigurationInherit, ConfigurationGiven)
	}
	if pc := p.PackageCache; pc != nil {
		if err := checkSandboxPath("posture.packageCache.seed", pc.Seed); err != nil {
			return err
		}
		switch pc.Mode {
		case "", PackageCacheShared, PackageCachePrivate:
		default:
			return fmt.Errorf("posture.packageCache.mode %q is not one of %q, %q",
				string(pc.Mode), PackageCacheShared, PackageCachePrivate)
		}
		if len(pc.Managers) == 0 {
			return fmt.Errorf("posture.packageCache.managers: name at least one of %q, %q, %q — "+
				"the managers are what decide which variables the session is given",
				PackageManagerCargo, PackageManagerGo, PackageManagerNpm)
		}
		if err := checkPackageManagers("posture.packageCache.managers", pc.Managers); err != nil {
			return err
		}
	}
	if p.Files != nil {
		for _, path := range p.Files.ReadPaths {
			if err := checkSandboxPath("posture.files.readPaths", path); err != nil {
				return err
			}
		}
		for _, path := range p.Files.WritePaths {
			if err := checkSandboxPath("posture.files.writePaths", path); err != nil {
				return err
			}
			if sharedTemp(path) {
				return fmt.Errorf("posture.files.writePaths: %q is, or contains, a directory shared between sessions "+
					"(the system's temporary directories); each session has its own private temporary directory", string(path))
			}
		}
	}
	return nil
}

// Given reports whether the posture asks for a session whose configuration is
// only what the create named (muster #392).
func (p Posture) Given() bool { return p.Configuration == ConfigurationGiven }

// Confines reports whether the posture needs an enforced sandbox: it names file
// access or a closed network, it is unattended (which implies confinement), or it
// names a package-cache seed (whose protection from the session is the sandbox's).
func (p Posture) Confines() bool {
	return p.Files != nil || p.Network == SandboxNetworkClosed || p.Unattended() || p.PackageCache != nil
}

// PostureSupport is what a runtime can enforce of a posture, derived from its
// capabilities by PostureSupportFrom and reported on RuntimeInfo.Posture.
type PostureSupport struct {
	// Unattended: a posture with attendance "unattended" is enforceable (the
	// runtime takes the prompt-free mode AND can confine the session).
	Unattended bool `json:"unattended"`
	// Files: file access can be confined to the working directory plus named
	// paths.
	Files bool `json:"files"`
	// Network lists the network postures that can be enforced. Empty when the
	// runtime cannot confine a session at all.
	Network []SandboxNetwork `json:"network"`
	// IsolatedEnvironment: the session's process can carry a built environment.
	IsolatedEnvironment bool `json:"isolatedEnvironment"`
	// GivenConfiguration: the session can be started with only the configuration
	// the create named (muster #392).
	GivenConfiguration bool `json:"givenConfiguration"`
	// PackageCache lists the modes a posture's package cache may use (muster
	// #432). Empty when the runtime cannot start a session from a cache seed.
	PackageCache []PackageCacheMode `json:"packageCache"`
	// PackageManagers lists the package managers it can point at one.
	PackageManagers []PackageManager `json:"packageManagers"`
}

// PostureSupportFrom derives what a runtime can enforce from what it declares.
// A runtime that cannot confine a session reports nothing enforceable beyond an
// empty attended posture. The default multiplexer runtime is such a runtime on a
// host where its agent CLI did not start and authenticate under the profile when
// that was measured, and on every host that has no sandbox mechanism
// (docs/api.md, #344, #345).
func PostureSupportFrom(c DriverCapabilities) PostureSupport {
	s := PostureSupport{
		Network: []SandboxNetwork{}, IsolatedEnvironment: c.IsolatesEnvironment, GivenConfiguration: c.IsolatesConfiguration,
		PackageCache: []PackageCacheMode{}, PackageManagers: []PackageManager{},
	}
	if c.Sandbox == nil {
		return s
	}
	s.Files = true
	s.Network = append(s.Network, c.Sandbox.Network...)
	s.PackageCache = append(s.PackageCache, c.Sandbox.PackageCache...)
	s.PackageManagers = append(s.PackageManagers, c.Sandbox.PackageManagers...)
	for _, m := range c.CreatePermissionModes {
		if m == PermissionModeBypass {
			s.Unattended = true
		}
	}
	return s
}

// PostureMapping is a posture lowered onto the per-runtime create fields: what
// the service hands the driver in their place, and what the dry run shows.
type PostureMapping struct {
	// PermissionMode is "bypass" for an unattended posture, otherwise empty.
	PermissionMode string `json:"permissionMode,omitempty"`
	// Sandbox is present when the posture confines the session.
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`
	// IsolateEnvironment mirrors Posture.IsolatedEnvironment.
	IsolateEnvironment bool `json:"isolateEnvironment,omitempty"`
	// IsolateConfiguration mirrors Posture.Configuration == "given" (muster #392).
	IsolateConfiguration bool `json:"isolateConfiguration,omitempty"`
}

// Lower maps the posture onto the per-runtime create fields and decides whether
// the runtime declaring caps can enforce it. On a refusal it returns an
// *Error of kind unsupported with Reason ReasonPostureUnenforceable, naming
// what is missing, and the mapping is not meaningful.
//
// Capabilities from a runtime nobody has reached (Source assumed) never refuse
// here: they say "unknown", never "incapable" (§4.3, D3). The mapping is
// returned and the driver that owns the session — or the relay's own checks
// against the peer — makes the call, exactly as for the per-runtime fields.
func (p Posture) Lower(caps DriverCapabilities) (PostureMapping, error) {
	var m PostureMapping
	if p.Unattended() {
		m.PermissionMode = PermissionModeBypass
	}
	if p.Confines() {
		m.Sandbox = &SandboxSpec{Network: p.Network}
		if p.Files != nil {
			m.Sandbox.ReadPaths = append([]AbsolutePath(nil), p.Files.ReadPaths...)
			m.Sandbox.WritePaths = append([]AbsolutePath(nil), p.Files.WritePaths...)
		}
	}
	if pc := p.PackageCache; pc != nil {
		mode := pc.Mode
		if mode == "" {
			mode = PackageCachePrivate
		}
		m.Sandbox.PackageCache = &SandboxPackageCache{
			Path: pc.Seed, Mode: mode, Managers: append([]PackageManager(nil), pc.Managers...),
		}
	}
	m.IsolateEnvironment = p.IsolatedEnvironment
	m.IsolateConfiguration = p.Given()

	if caps.Source == CapabilitiesAssumed {
		return m, nil
	}
	sup := PostureSupportFrom(caps)
	refuse := func(format string, args ...any) (PostureMapping, error) {
		return PostureMapping{}, &Error{
			Kind:    ErrorUnsupported,
			Reason:  ReasonPostureUnenforceable,
			Message: "create: posture cannot be enforced by this runtime: " + fmt.Sprintf(format, args...) + "; refusing rather than start the session with a weaker posture (#344)",
		}
	}
	if m.Sandbox != nil && !sup.Files {
		why := "it cannot confine a session (no operating-system sandbox"
		if caps.SandboxUnavailable != "" {
			why += ": " + caps.SandboxUnavailable
		}
		why += ")"
		if p.Unattended() {
			why += ", and an unattended session is always confined"
		}
		return refuse("%s", why)
	}
	if p.Network == SandboxNetworkClosed {
		closed := false
		for _, n := range sup.Network {
			if n == SandboxNetworkClosed {
				closed = true
			}
		}
		if !closed {
			return refuse("it cannot close the network")
		}
	}
	if p.Unattended() && !sup.Unattended {
		return refuse("it has no prompt-free mode")
	}
	if p.IsolatedEnvironment && !sup.IsolatedEnvironment {
		return refuse("it cannot isolate the session's environment")
	}
	if p.Given() && !sup.GivenConfiguration {
		return refuse("it cannot start a session with only the configuration the create names (it would also load the account's own settings and tool servers)")
	}
	if pc := m.Sandbox; pc != nil && pc.PackageCache != nil {
		mode := pc.PackageCache.Mode
		if !slices.Contains(sup.PackageCache, mode) {
			return refuse("it cannot start a session with a %s package cache", string(mode))
		}
		for _, want := range pc.PackageCache.Managers {
			if !slices.Contains(sup.PackageManagers, want) {
				return refuse("it cannot point %s at a package cache (%s)", string(want), want.EnvVar())
			}
		}
	}
	return m, nil
}
