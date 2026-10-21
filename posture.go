package fleet

import (
	"fmt"
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
// service lowers it onto the three per-runtime fields that already exist —
// SessionSpec.PermissionMode, SessionSpec.Sandbox and
// SessionSpec.IsolateEnvironment — using only what the runtime's own
// DriverCapabilities declare, and refuses the create when the runtime cannot
// enforce all of it. Nothing starts with a weaker posture than the one asked.
//
// # One mapping, not one per driver
//
// What a runtime can enforce is already a function of capabilities it declares
// (Sandbox, CreatePermissionModes, IsolatesEnvironment), so the mapping lives
// here, once. The dry run, the capability report and the create itself all call
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

// Confines reports whether the posture needs an enforced sandbox: it names file
// access or a closed network, or it is unattended (which implies confinement).
func (p Posture) Confines() bool {
	return p.Files != nil || p.Network == SandboxNetworkClosed || p.Unattended()
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
}

// PostureSupportFrom derives what a runtime can enforce from what it declares.
// A runtime that cannot confine a session reports nothing enforceable beyond an
// empty attended posture, which is the truth about the default multiplexer
// runtime today (docs/api.md, #344).
func PostureSupportFrom(c DriverCapabilities) PostureSupport {
	s := PostureSupport{Network: []SandboxNetwork{}, IsolatedEnvironment: c.IsolatesEnvironment}
	if c.Sandbox == nil {
		return s
	}
	s.Files = true
	s.Network = append(s.Network, c.Sandbox.Network...)
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
	m.IsolateEnvironment = p.IsolatedEnvironment

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
		why := "it cannot confine a session (no operating-system sandbox)"
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
	return m, nil
}
