// Package sessionenv is the machine-declared environment every driver that
// starts a session process shares (muster issue #94): an operator names, once,
// the variables this machine's sessions always carry and where their values
// are read from, instead of every caller passing the same variable on every
// create forever.
//
// It lives in its own package, not in a driver, because more than one driver
// applies it — the terminal-multiplexer driver and the opencode driver both
// start a process per session — and "which variable wins, and when a create is
// refused" must be one answer, not two that drift.
//
// # Why provisioning runs where a session starts
//
// A create aimed at this machine can arrive two ways: served locally, or
// relayed here by a peer that merely forwards the request body — env
// included — verbatim. Provisioning in the HTTP handler would run on every
// request THIS SERVICE HANDLES, including one it is only relaying onward to a
// THIRD machine, which would read this machine's files and ship the value over
// the network to a session that will run somewhere else entirely.
// "Per-machine identity" has to mean the machine that runs the session, so
// Provision is called from a driver's Create, after the relay decision has
// already been made.
//
// # Why the declaration is loaded once and the value is not
//
// The list of entries — which variables, from which files, required or not,
// scoped to what — is machine configuration, read at daemon start. The VALUE
// behind each entry is not cached alongside it: it is read fresh from FromFile
// on every Provision. Caching it at startup would silently defeat credential
// rotation — rotate the file, and every session created afterward would keep
// receiving the old value until somebody restarted the service. Reading per
// create means a rotation takes effect on the very next session.
package sessionenv

import (
	"errors"
	"fmt"
	"os"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// Scope narrows which sessions one Entry applies to.
//
// The zero value matches every session on this machine, on every runtime this
// machine registers — including a runtime registered later. Agents and Markers
// are the escape hatch for the session that must deliberately act as something
// other than the configured identity, so that exception costs an operator a
// configuration edit rather than a code change and a deploy. Runtimes is the
// other kind of narrowing: it says which runtime's sessions may carry the
// variable at all, which an operator needs for a credential that belongs to one
// runtime and must not reach another.
//
// The axes combine as Runtimes AND (Agents OR Markers). An empty axis means
// "any". The agents-or-markers pair kept the meaning it had before Runtimes
// existed; Runtimes is a gate in front of it, never a third alternative.
type Scope struct {
	// Runtimes matches the runtime the session is created on — the id of the
	// driver serving the create, not a hint the caller sent (a caller that
	// supplied none still gets a session on the default runtime).
	Runtimes []string
	// Agents matches SessionSpec.Agent.
	Agents []string
	// Markers matches SessionSpec.Marker.
	Markers []string
}

// Matches reports whether a session created by the driver for runtime, with
// spec, falls inside the scope. A scope naming no axis matches everything — the
// common case, an operator declaring an identity for the whole machine rather
// than carving out a runtime or an agent.
//
// runtime is passed by the driver rather than read from spec.Runtime: that
// field is the caller's own hint and is empty precisely when the caller took
// the default, which is the case a credential must be correctly withheld or
// delivered for. A scope naming Runtimes never matches an empty runtime —
// withheld rather than delivered when the runtime is not known.
func (s Scope) Matches(runtime fleet.RuntimeId, spec fleet.SessionSpec) bool {
	if len(s.Runtimes) > 0 {
		found := false
		for _, r := range s.Runtimes {
			if runtime != "" && string(runtime) == r {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(s.Agents) == 0 && len(s.Markers) == 0 {
		return true
	}
	for _, a := range s.Agents {
		if string(spec.Agent) == a {
			return true
		}
	}
	for _, m := range s.Markers {
		if spec.Marker == m {
			return true
		}
	}
	return false
}

// The values an Entry's FromSession may take (muster issue #326).
const (
	// SessionRef exports the session's own ref in the form the /v1 paths and
	// peers use: "<machine>/<id>".
	SessionRef = "ref"
	// SessionID exports the bare session id.
	SessionID = "id"
)

// Entry is one operator-declared variable this machine's sessions should
// carry: read from FromFile at create time rather than at daemon start, or
// taken from the session's own identity (FromSession). Exactly one source.
type Entry struct {
	// Name is the variable name a session process will see. Validated at
	// startup by Validate, not on every create, so a typo is a message an
	// operator reads once, not a refusal every later caller meets.
	Name string
	// FromFile is an absolute path read fresh on every Provision. Never
	// staged, logged, or placed in an argv.
	FromFile string
	// FromSession is the other source (muster issue #326): the created
	// session's own identity, SessionRef or SessionID. A tool running inside a
	// session often has to name that session to a third party that will later
	// answer back through this API, and the id is known only to the service
	// that mints it — the create's env cannot carry it, because the caller
	// learns the id only from the create's answer.
	//
	// The value is delivered like every other: staged, never on a command line,
	// and the scope and precedence rules apply unchanged. It is always
	// available where the driver knows the id before the process starts, so
	// Required here only decides whether a caller-supplied value is refused.
	FromSession string
	// Required makes a missing, unreadable or empty FromFile refuse the
	// create rather than silently omit the variable. Per entry rather than a
	// machine-wide switch, because both answers are legitimate for different
	// variables on the same machine.
	Required bool
	// AppliesTo scopes this entry. See Scope.
	AppliesTo Scope
}

// Validate checks every entry's shape once, at startup, so a misconfiguration
// is a message an operator reads at daemon start rather than a create-time
// refusal every later caller meets.
//
// knownRuntimes is the set of runtime ids this build can register. It is a
// parameter, not something Validate looks up, because the drivers do not exist
// yet when the configuration is first checked — and it is the ids the build
// KNOWS, not the ones that happen to be running, so one configuration file
// shared across machines does not become a startup failure on a machine that
// has an optional runtime switched off. An entry naming an id outside the set
// is a typo. A nil set skips that check, for a caller that only wants the
// shape.
func Validate(entries []Entry, knownRuntimes []fleet.RuntimeId) error {
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !ValidName(e.Name) {
			return fmt.Errorf("sessionEnv: %q is not a usable variable name "+
				"(letters, digits and underscore, not starting with a digit)", e.Name)
		}
		switch {
		case e.FromFile != "" && e.FromSession != "":
			return fmt.Errorf("sessionEnv: %q declares both fromFile and fromSession; "+
				"an entry has exactly one source", e.Name)
		case e.FromFile == "" && e.FromSession == "":
			return fmt.Errorf("sessionEnv: %q has no source; declare fromFile or fromSession", e.Name)
		case e.FromSession != "":
			if e.FromSession != SessionRef && e.FromSession != SessionID {
				return fmt.Errorf("sessionEnv: %q's fromSession %q must be %q or %q",
					e.Name, e.FromSession, SessionRef, SessionID)
			}
		case !strings.HasPrefix(e.FromFile, "/"):
			return fmt.Errorf("sessionEnv: %q's fromFile %q must be an absolute path", e.Name, e.FromFile)
		}
		// Two entries for the same name would leave "which one wins"
		// unanswered — refused here rather than resolved by iteration order,
		// which would make the answer depend on how the config file happened
		// to list them.
		if seen[e.Name] {
			return fmt.Errorf("sessionEnv: %q is declared more than once", e.Name)
		}
		seen[e.Name] = true
		for _, r := range e.AppliesTo.Runtimes {
			// An empty id would never match anything, so the entry would be
			// silently inert — the same fault a typo is, caught the same way.
			if r == "" {
				return fmt.Errorf("sessionEnv: %q's appliesTo.runtimes has an empty runtime id", e.Name)
			}
			if knownRuntimes != nil && !containsRuntime(knownRuntimes, r) {
				return fmt.Errorf("sessionEnv: %q's appliesTo.runtimes names %q, which is not a "+
					"runtime this service has (want one of: %s)", e.Name, r, joinRuntimes(knownRuntimes))
			}
		}
	}
	return nil
}

func containsRuntime(known []fleet.RuntimeId, id string) bool {
	for _, k := range known {
		if string(k) == id {
			return true
		}
	}
	return false
}

func joinRuntimes(known []fleet.RuntimeId) string {
	names := make([]string, len(known))
	for i, k := range known {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// ValidName reports whether name is usable as an environment variable name:
// letters, digits and underscore, not starting with a digit.
func ValidName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Provision merges this machine's declared identity into spec.Env, and is
// where muster issue #94's precedence table is implemented:
//
//   - required entry, caller silent           → configuration provides it
//   - required entry, caller supplies ANY value → refuse, uncompared
//   - non-required entry, caller supplies a value → the caller wins
//   - non-required entry, caller silent          → configuration provides it
//
// A session outside an entry's AppliesTo scope never meets that entry at all:
// not provisioned, not required, not compared against.
//
// runtime is the id of the driver doing the provisioning — the runtime this
// session is being created on. It is what AppliesTo.Runtimes is matched
// against; see Scope.Matches for why it is not read from spec.
//
// sessionID is the id the session being created will have, for the entries that
// export it (FromSession). A driver that cannot know it before the process
// starts passes "" and must refuse such entries at construction
// (CheckSessionSource); a "" that reaches here anyway makes a required entry an
// unsupported-on-this-machine refusal and leaves an optional one unset, never a
// value that names no session.
//
// skip, when non-nil, is asked about each in-scope entry name and drops the
// entry entirely (neither read nor required) when it returns true — a driver
// whose delivery module owns a variable's namespace says so here.
//
// # A required entry never compares a caller's value against the configured one — this was an equality oracle
//
// A caller holding only the create grant could otherwise supply a CANDIDATE
// value for a variable it has no read access to and learn, from whether the
// create succeeded or was refused, whether the candidate matched the configured
// secret. So a required entry refuses ANY caller-supplied value for that name,
// without reading the configured file and without comparing — the read and the
// comparison are what made the outcome depend on the secret, so both are
// skipped entirely rather than performed and then discarded, which would
// reopen the same oracle as a timing difference. The refusal message says only
// that the name is required on this machine and must be omitted.
//
// # Which refusal is which kind
//
//   - The CALLER supplied a value for a name a required entry owns.
//     Correctable by the caller — omit the field — so this returns a bare
//     error, which the HTTP layer maps to "invalid" / 400.
//   - This MACHINE cannot back a required entry: the file is missing,
//     unreadable, or empty. No correction to the request fixes it, so it is a
//     typed *fleet.Error with Kind ErrorUnsupported (501), and the message
//     addresses the operator, not the caller.
func Provision(entries []Entry, spec fleet.SessionSpec, machine fleet.MachineId, runtime fleet.RuntimeId, sessionID string, skip func(name string) bool) (map[string]string, error) {
	if len(entries) == 0 {
		return spec.Env, nil
	}

	var merged map[string]string
	for _, entry := range entries {
		if !entry.AppliesTo.Matches(runtime, spec) {
			continue
		}
		if skip != nil && skip(entry.Name) {
			continue
		}

		_, callerSet := spec.Env[entry.Name]

		// A required entry refuses ANY caller-supplied value for this name,
		// before the configured file is read and without ever comparing.
		if entry.Required && callerSet {
			return nil, fmt.Errorf(
				"create: env %s is required on this machine and must be omitted from the "+
					"request; this machine provides its own value for it",
				entry.Name)
		}

		var value string
		var err error
		source := entry.FromFile
		if entry.FromSession != "" {
			source = "the session's own " + entry.FromSession
			value, err = sessionValue(entry.FromSession, machine, sessionID)
		} else {
			value, err = readFile(entry.FromFile)
		}
		if err != nil {
			if !entry.Required {
				continue // legitimate: not required, silently absent (§94)
			}
			return nil, &fleet.Error{
				Kind: fleet.ErrorUnsupported,
				Message: fmt.Sprintf(
					"create: sessionEnv %q is required on this machine but could not be "+
						"provisioned from %s: %v — an operator must repair that; "+
						"retrying this create will not help until they do",
					entry.Name, source, err),
				Machine: machine,
			}
		}

		if callerSet {
			// Only reachable when the entry is NOT required. "The caller
			// wins": the caller's own value is already in spec.Env/merged.
			continue
		}

		if merged == nil {
			merged = make(map[string]string, len(spec.Env)+len(entries))
			for k, v := range spec.Env {
				merged[k] = v
			}
		}
		merged[entry.Name] = value
	}

	if merged == nil {
		return spec.Env, nil
	}
	return merged, nil
}

// sessionValue is a FromSession entry's value. Validated shape-wise at startup,
// so the only runtime failure is an id the driver did not have.
func sessionValue(kind string, machine fleet.MachineId, sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("this driver does not know the session's id before it starts")
	}
	if kind == SessionID {
		return sessionID, nil
	}
	return string(machine) + "/" + sessionID, nil
}

// CheckSessionSource refuses, for a driver that only learns a session's id
// after its process has started, any entry that exports the session's own
// identity and could reach that driver's sessions — the variable could not be
// set, and a silently missing identity is the failure this configuration exists
// to prevent. runtime is the driver's own runtime id; an entry scoped to other
// runtimes does not reach it and is accepted.
func CheckSessionSource(entries []Entry, runtime fleet.RuntimeId) error {
	for _, e := range entries {
		if e.FromSession == "" {
			continue
		}
		reaches := len(e.AppliesTo.Runtimes) == 0
		for _, r := range e.AppliesTo.Runtimes {
			if r == string(runtime) {
				reaches = true
			}
		}
		if reaches {
			return fmt.Errorf("sessionEnv %q exports the session's own %s, which the %s runtime "+
				"cannot do (it learns the id only after the process starts); scope the entry with "+
				"appliesTo.runtimes to the runtimes that can", e.Name, e.FromSession, runtime)
		}
	}
	return nil
}

// CheckEnv rejects what a line-oriented staging format cannot carry
// faithfully: a name that is not a usable variable name, or a value containing
// a newline or NUL (which would arrive as a second, fabricated variable).
func CheckEnv(env map[string]string) error {
	for name, value := range env {
		if !ValidName(name) {
			return fmt.Errorf("env: %q is not a usable variable name "+
				"(letters, digits and underscore, not starting with a digit)", name)
		}
		if strings.ContainsAny(value, "\n\x00") {
			return fmt.Errorf("env: the value of %s contains a newline or NUL, which a "+
				"session's environment cannot carry without inventing a second variable", name)
		}
	}
	return nil
}

// readFile reads and bounds-checks one configured value. The bound is the same
// one CheckEnv enforces on a caller's own values, checked HERE so a bound
// violation in a configured value is reported as this machine's problem (501),
// not folded into a caller's 400.
func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", errors.New("file is empty")
	}
	if strings.ContainsAny(value, "\n\x00") {
		return "", errors.New("value contains a newline or NUL, which the staging format cannot carry")
	}
	return value, nil
}
