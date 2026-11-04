package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
)

// The sandbox a session may be started under on this runtime (muster #345).
//
// # The shape
//
// This driver starts sessions inside the user's own multiplexer, and the
// multiplexer's server is exactly what a profile must keep a session away from
// (#281). The server therefore stays OUTSIDE the profile, and what is wrapped is
// the agent program alone, inside the pane:
//
//	multiplexer server → pane → login shell (env, cd) → env TMPDIR=… → sandbox-exec -f profile → agent
//
// The login shell is not confined — it only reads the startup files, enters the
// working directory and execs. Everything the agent starts inherits the profile,
// and the profile denies a unix-socket connect outright, so the agent cannot
// reach back to the server and have a command run outside.
//
// # The capability is a measurement, not an assertion
//
// "A sandbox exists on this host" says nothing about whether THIS runtime's
// agent works inside one. Measured on the first host (2026-11): under the profile
// the opencode runtime already ships, the agent CLI started and then failed in
// two ways — it could not read its credential file (logged out), and it insisted
// on a temporary directory shared between users, which the profile refuses. With
// both addressed it authenticated, answered, and wrote its transcript. A host
// whose credentials live in the keychain fails the first test and cannot be
// helped by this driver: allowing the keychain's services would hand the session
// every item the user has.
//
// So the capability is declared only when the agent CLI, run through the same
// launch builder a create uses, reports a logged-in account under the profile
// (measureSandbox). A host that fails keeps refusing, and says why.
//
// # What the profile must allow for this agent, and why it is a short list
//
// Carried here, in the driver, never in caller input (the same rule as the
// system allow-lists):
//
//   - READ the agent's install, its credential file, and its configuration the
//     user wrote for it (settings, instructions, agent and skill definitions,
//     commands, rules, plugins) and the config file beside the config directory.
//     Read-only on purpose: a setting, a hook, a skill or a trusted-folder entry
//     is code or authority a LATER unconfined session would run, so a confined
//     one must not be able to write any of them.
//   - WRITE the session's own transcript directory, and nothing else of the
//     agent's: the service reads the transcript for usage, context and
//     conversation, so it has to be where the service looks.
//   - The agent's other writes (its process registry, caches, update locks,
//     session environment files, browser integration manifests) are denied; the
//     agent degrades and the session works. They are listed in docs/gotchas.d.
//
// Everything the user did not name stays unreadable: a skill or agent definition
// that is a symlink out of the configuration directory needs the CALLER to name
// its target in posture.files.readPaths, as a linked worktree needs its git
// directory in writePaths.
//
// # The private directory, and the one file a session must not be able to write
//
// Each sandboxed session gets <root>/<id>/. The session is granted <id>/work
// (its TMPDIR) and nothing else of it; the profile and the report of what is in
// force sit beside it, unreachable from inside, so a confined session cannot
// change what it is confined by or what it is reported to be confined by.

// agentConfigReads are the entries of the agent's configuration directory a
// sandboxed session may read. Entries that do not exist are harmless.
var agentConfigReads = []string{
	".credentials.json",
	"settings.json", "settings.local.json",
	"CLAUDE.md",
	"agents", "skills", "commands", "rules", "plugins",
	"cache",
}

// agentProjectsDir is the name of the directory under the configuration
// directory where the agent writes one subdirectory of transcripts per working
// directory. The record root the service reads is this directory.
const agentProjectsDir = "projects"

// agentProbeArgs is the measurement: the agent's own account report, which
// needs no model call and no network.
var agentProbeArgs = []string{"auth", "status"}

const (
	sandboxFoundTTL = 5 * time.Minute
	// sandboxFailTTL is how long a failed measurement is trusted when somebody
	// is waiting on it (a create). In the background it doubles with each
	// consecutive failure, up to sandboxFailMaxTTL (muster #404): a host that
	// cannot be confined (credentials in a store the profile denies) used to be
	// re-measured every thirty seconds forever, each time starting a login shell
	// twice and the agent CLI once.
	sandboxFailTTL    = 30 * time.Second
	sandboxFailMaxTTL = 5 * time.Minute

	sandboxProbeBegin = "MUSTER_SANDBOX_PROBE_BEGIN"
	agentPathMark     = "MUSTER_AGENT_PATH="

	// sandboxDirGrace is how long a private directory with no live session is
	// kept before the sweep removes it: a session just created is not yet in
	// the multiplexer's listing.
	sandboxDirGrace = 10 * time.Minute
	// sandboxSweepEvery bounds how often a listing walks the root.
	sandboxSweepEvery = time.Minute
	// sandboxProbeDirGrace is how long a probe directory a crashed service left
	// behind is kept.
	sandboxProbeDirGrace = time.Hour
)

// sandboxProbeTimeout bounds one measurement. A variable so a test can shorten it.
var sandboxProbeTimeout = 20 * time.Second

// agentPathScript prints the path the login shell would run for $1, under the
// same acceptance rule as agentProbeScript: a regular, executable file.
const agentPathScript = `p=$(command -v "$1" 2>/dev/null)
if [ -n "$p" ] && [ -f "$p" ] && [ -x "$p" ]; then
  printf '` + agentPathMark + `%s\n' "$p"
else
  echo ` + agentMissingMark + `
fi`

// sandboxEnterScript enters the working directory, marks where the program's own
// output begins (a login shell's startup files may print anything before it) and
// replaces itself with the program. Everything is positional: nothing the caller
// supplies is parsed as shell syntax.
const sandboxEnterScript = `cd -- "$1" || exit 1
shift
printf '%s\n' ` + sandboxProbeBegin + `
exec "$@"`

// SandboxOptions configure the sandbox this driver offers. The zero value is
// usable once the driver has a state store and a record root.
type SandboxOptions struct {
	// Root is where each sandboxed session's private directory is made. Default:
	// <state directory>/sandbox.
	Root string
	// ConfigDir is the agent CLI's configuration directory. Default: the parent
	// of the record root (WithRecordRoot), when that is named "projects".
	ConfigDir string
	// Home is the service user's home directory, used to refuse a grant that
	// would put it back. Default: the process's.
	Home string
}

// WithSandbox lets this driver confine the sessions it starts (muster #345).
//
// Off by default, like the record root: a driver constructed by a test or by
// another program must not go and run a real agent CLI against a real host
// because a constructor was called. The composition root enables it; whether it
// is then OFFERED is decided on the host, by measurement.
func WithSandbox(o SandboxOptions) Option {
	return func(d *Driver) {
		c := o
		d.sandboxCfg = &c
	}
}

// sandboxMeasurement is the last answer to "can the agent be confined here".
type sandboxMeasurement struct {
	// mu guards the fields below and is never held across a probe: Capabilities
	// is read on the path of nearly every request (for its deadline), so a
	// reader must never wait behind a measurement that is running.
	mu         sync.Mutex
	valid      bool
	at         time.Time
	sup        *fleet.SandboxSupport
	why        string
	prog       string
	refreshing bool
	// failures is how many measurements in a row declared nothing.
	failures int
	// probeMu serialises the measurements themselves.
	probeMu sync.Mutex
}

// ttl is how long the answer is trusted: a positive one for long, a negative
// one briefly (the user may have just logged in). strict is for a caller that is
// about to act on the answer and waits for it (a create): it gets the short
// negative lifetime however many failures came before, so a user who has just
// logged in is not made to wait out a backoff that exists for the background.
func (m *sandboxMeasurement) ttl(strict bool) time.Duration {
	if m.sup != nil {
		return sandboxFoundTTL
	}
	if strict {
		return sandboxFailTTL
	}
	return backoffTTL(sandboxFailTTL, sandboxFailMaxTTL, m.failures)
}

func copySandboxSupport(s *fleet.SandboxSupport) *fleet.SandboxSupport {
	if s == nil {
		return nil
	}
	c := *s
	c.Denies = append([]fleet.SandboxDeny(nil), s.Denies...)
	c.Network = append([]fleet.SandboxNetwork(nil), s.Network...)
	c.PackageCache = append([]fleet.PackageCacheMode{}, s.PackageCache...)
	return &c
}

// peek returns the cached answer and whether it is still inside its lifetime.
func (m *sandboxMeasurement) peek(now time.Time, strict bool) (sup *fleet.SandboxSupport, why, prog string, known, fresh bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid {
		return nil, "", "", false, false
	}
	age := now.Sub(m.at)
	return copySandboxSupport(m.sup), m.why, m.prog, true, age >= 0 && age < m.ttl(strict)
}

// sandboxSupportNow is Capabilities' view: the support declared on this host, or
// nil and the reason.
//
// It does not wait. Once an answer exists it is returned at once, and a stale
// one is refreshed in the background (the next read sees the new answer); only
// the very first read — the service registering the driver, before it serves
// anything — measures in line.
func (d *Driver) sandboxSupportNow() (*fleet.SandboxSupport, string) {
	if d.sandboxCfg == nil {
		return nil, ""
	}
	m := &d.sandboxMeasured
	sup, why, _, known, fresh := m.peek(d.now(), false)
	if known {
		if !fresh {
			d.refreshSandboxAsync()
		}
		return sup, why
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout+5*time.Second)
	defer cancel()
	sup, why, _ = d.sandboxCurrent(ctx, false)
	return sup, why
}

// refreshSandboxAsync retakes the measurement without making a caller wait for
// it. At most one runs at a time.
func (d *Driver) refreshSandboxAsync() {
	m := &d.sandboxMeasured
	m.mu.Lock()
	if m.refreshing {
		m.mu.Unlock()
		return
	}
	m.refreshing = true
	m.mu.Unlock()
	run := func() {
		defer func() {
			m.mu.Lock()
			m.refreshing = false
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout+5*time.Second)
		defer cancel()
		d.sandboxCurrent(ctx, false)
	}
	if d.sandboxGo != nil {
		d.sandboxGo(run)
		return
	}
	go run()
}

// sandboxCurrent returns a measurement inside its lifetime, taking a fresh one
// when the cached one is stale. It waits: it is for a create, which is rare and
// must not act on a stale answer. Concurrent callers share one probe. strict is
// true for a caller acting on the answer (see sandboxMeasurement.ttl).
func (d *Driver) sandboxCurrent(ctx context.Context, strict bool) (*fleet.SandboxSupport, string, string) {
	m := &d.sandboxMeasured
	if sup, why, prog, known, fresh := m.peek(d.now(), strict); known && fresh {
		return sup, why, prog
	}
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	// Another caller may have measured while this one waited.
	if sup, why, prog, known, fresh := m.peek(d.now(), strict); known && fresh {
		return sup, why, prog
	}
	sup, why, prog := d.measureSandbox(ctx)
	if ctx.Err() != nil && sup == nil {
		// Our caller gave up; that says nothing about the host and must not be
		// remembered as an answer about it.
		return nil, "the sandbox measurement was interrupted before it finished", ""
	}
	m.mu.Lock()
	m.sup, m.why, m.prog, m.at, m.valid = copySandboxSupport(sup), why, prog, d.now(), true
	if sup == nil {
		m.failures++
	} else {
		m.failures = 0
	}
	m.mu.Unlock()
	return sup, why, prog
}

// sandboxHome is the home directory the grants are checked against.
func (d *Driver) sandboxHome() string {
	h := ""
	if d.sandboxCfg != nil {
		h = d.sandboxCfg.Home
	}
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	if h != "" {
		h = sandbox.ResolvePath(h)
	}
	return h
}

// sandboxRoot reports where private directories are made, or why there is none.
func (d *Driver) sandboxRoot() (string, string) {
	if d.sandboxCfg != nil && d.sandboxCfg.Root != "" {
		return d.sandboxCfg.Root, ""
	}
	if d.store == nil || d.store.Dir() == "" {
		return "", "this driver has no state directory to hold a sandboxed session's private directory"
	}
	return filepath.Join(d.store.Dir(), "sandbox"), ""
}

// sandboxConfigDir reports the agent's configuration directory, or why it is
// not known. It is derived from the record root the service already reads, so
// the transcript directory the profile grants is the one the service looks in.
func (d *Driver) sandboxConfigDir() (string, string) {
	if d.sandboxCfg != nil && d.sandboxCfg.ConfigDir != "" {
		return d.sandboxCfg.ConfigDir, ""
	}
	if d.conversations == nil {
		return "", "this driver has no record root, so the directory a sandboxed session writes its transcript to is not known"
	}
	root := filepath.Clean(d.conversations.root)
	if filepath.Base(root) != agentProjectsDir {
		return "", fmt.Sprintf("the record root %q is not a %q directory, so the agent's configuration directory is not known", root, agentProjectsDir)
	}
	return filepath.Dir(root), ""
}

// sandboxLayout is where one session's private files live.
type sandboxLayout struct {
	dir     string // <root>/<id>, never granted
	work    string // <dir>/work, granted writable: the session's own space
	tmp     string // <work>/tmp: TMPDIR for the agent
	profile string // <dir>/profile.sb, read by the launcher before it applies it
	report  string // <dir>/sandbox.json, what the service reports
}

func newSandboxLayout(root, session string) sandboxLayout {
	sum := sha256.Sum256([]byte(session))
	dir := filepath.Join(root, "s-"+hex.EncodeToString(sum[:])[:24])
	return sandboxLayout{
		dir:     dir,
		work:    filepath.Join(dir, "work"),
		tmp:     filepath.Join(dir, "work", "tmp"),
		profile: filepath.Join(dir, "profile.sb"),
		report:  filepath.Join(dir, "sandbox.json"),
	}
}

// sandboxRecord is the report written beside a session's profile.
type sandboxRecord struct {
	Session string              `json:"session"`
	Created time.Time           `json:"created"`
	State   *fleet.SandboxState `json:"state"`
}

// refuseSandbox is the one place a create that cannot be confined is turned
// away, so every reason reads the same way.
func (d *Driver) refuseSandbox(why string) error {
	return &fleet.Error{
		Kind:    fleet.ErrorUnsupported,
		Message: "create: this driver cannot enforce a sandbox here (" + why + "); refusing rather than start the session unconfined",
		Machine: d.machine,
	}
}

// sandboxGate decides, before any side effect, whether a create asking for a
// sandbox can be honoured, and returns the agent program it will run (resolved
// to a path). It is Create's first act on a sandboxed spec.
func (d *Driver) sandboxGate(ctx context.Context, spec fleet.SessionSpec) (string, error) {
	if d.sandboxCfg == nil {
		return "", d.refuseSandbox("its sessions run in the user's own multiplexer, outside any profile this service could apply, " +
			"and this service was not configured to wrap the agent in one")
	}
	if err := spec.Sandbox.Validate(); err != nil {
		return "", &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + err.Error(), Machine: d.machine}
	}
	if spec.Sandbox.PackageCache != nil {
		return "", &fleet.Error{
			Kind:    fleet.ErrorUnsupported,
			Message: "create: this driver offers no package-cache mode (sandbox.packageCache); refusing rather than start the session without one",
			Machine: d.machine,
		}
	}
	sup, why, prog := d.sandboxCurrent(ctx, true)
	if sup == nil {
		return "", d.refuseSandbox(why)
	}
	return prog, nil
}

// planSandbox resolves a request against this driver's defaults and the agent's
// own needs. layout.work must already exist.
func (d *Driver) planSandbox(spec fleet.SessionSpec, prog, configDir string, layout sandboxLayout) (*sandbox.Plan, error) {
	sb := spec.Sandbox
	invalid := func(msg string, args ...any) error {
		return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + fmt.Sprintf(msg, args...), Machine: d.machine}
	}
	if err := sb.Validate(); err != nil {
		return nil, invalid("%s", err.Error())
	}
	home := d.sandboxHome()
	// A grant that would include the service user's home puts back exactly what
	// the profile exists to remove.
	check := func(field string, p fleet.AbsolutePath) (string, error) {
		r := sandbox.ResolvePath(string(p))
		if home != "" && sandbox.CoversOrIs(r, home) {
			return "", invalid("%s: %q is, or contains, the service user's home directory; grant the specific directory a session needs", field, string(p))
		}
		return r, nil
	}

	cwd := sandbox.ResolvePath(string(spec.Cwd))
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, invalid("cwd %q is not a directory that exists, which a sandboxed session needs to be started in", string(spec.Cwd))
	}
	if home != "" && sandbox.CoversOrIs(cwd, home) {
		return nil, invalid("cwd %q is, or contains, the service user's home directory, which a sandboxed session would then write to", string(spec.Cwd))
	}

	plan := &sandbox.Plan{Network: sb.Network}
	if plan.Network == "" {
		plan.Network = fleet.SandboxNetworkOpen
	}

	read := append([]string(nil), sandbox.SystemReadRoots...)
	read = append(read, sandbox.InstallRoots(prog, home)...)
	cfg := sandbox.ResolvePath(configDir)
	for _, n := range agentConfigReads {
		read = append(read, sandbox.ResolvePath(filepath.Join(cfg, n)))
	}
	// The agent's own settings file sits beside its configuration directory
	// when that directory is the conventional dot-directory, and inside it
	// otherwise.
	if filepath.Base(cfg) == ".claude" {
		read = append(read, sandbox.ResolvePath(filepath.Join(filepath.Dir(cfg), ".claude.json")))
	} else {
		read = append(read, sandbox.ResolvePath(filepath.Join(cfg, ".claude.json")))
	}

	write := []string{cwd, sandbox.ResolvePath(layout.work)}
	// The session's own transcripts, under both spellings of its directory (the
	// agent records the real path; the service may know it by a link).
	for _, wd := range []string{cwd, string(spec.Cwd)} {
		write = append(write, filepath.Join(cfg, agentProjectsDir, recordDirFor(wd)))
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

	plan.Write = sandbox.Dedupe(write)
	plan.Read = sandbox.Dedupe(append(read, write...))
	return plan, nil
}

// sandboxLaunchArgv turns the agent's argv into the one that runs it under the
// profile. prog replaces argv[0]: the profile is built for the file the login
// shell resolved, and a second PATH lookup after it is applied is a resolution
// the session's environment could influence.
//
// The temporary directory is set HERE, after the login shell's startup files
// have run, so nothing they export can point the agent back at a directory
// shared between users.
func sandboxLaunchArgv(layout sandboxLayout, prog string, agentArgv []string) []string {
	out := []string{
		"/usr/bin/env", "TMPDIR=" + layout.tmp, "CLAUDE_CODE_TMPDIR=" + layout.tmp,
		sandbox.ExecPath, "-f", layout.profile, prog,
	}
	if len(agentArgv) > 1 {
		out = append(out, agentArgv[1:]...)
	}
	return out
}

// writeSandboxFiles makes the private directory, writes the profile and the
// report, and returns the plan's state.
func writeSandboxFiles(layout sandboxLayout, plan *sandbox.Plan, session string, now time.Time) (*fleet.SandboxState, error) {
	if err := os.RemoveAll(layout.dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(layout.tmp, 0o700); err != nil {
		return nil, err
	}
	profile, err := plan.Profile()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(layout.profile, []byte(profile), 0o600); err != nil {
		return nil, err
	}
	st := plan.State()
	b, err := json.Marshal(sandboxRecord{Session: session, Created: now, State: st})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(layout.report, b, 0o600); err != nil {
		return nil, err
	}
	return st, nil
}

// prepareSandbox builds the private directory and profile for a session and
// returns the agent argv wrapped to run under it, plus a function that undoes
// it for a create that fails after this point.
func (d *Driver) prepareSandbox(spec fleet.SessionSpec, name, prog string, argv []string) ([]string, func(), error) {
	root, why := d.sandboxRoot()
	if why != "" {
		return nil, nil, d.refuseSandbox(why)
	}
	configDir, why := d.sandboxConfigDir()
	if why != "" {
		return nil, nil, d.refuseSandbox(why)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create: making the sandbox directory: %w", err)
	}
	layout := newSandboxLayout(root, name)
	// work must exist before the plan resolves it: the profile matches real paths.
	if err := os.MkdirAll(layout.tmp, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create: making the session's private directory: %w", err)
	}
	plan, err := d.planSandbox(spec, prog, configDir, layout)
	if err != nil {
		_ = os.RemoveAll(layout.dir)
		return nil, nil, err
	}
	st, err := writeSandboxFiles(layout, plan, name, d.now())
	if err != nil {
		_ = os.RemoveAll(layout.dir)
		return nil, nil, fmt.Errorf("create: writing the sandbox profile: %w", err)
	}
	d.sandboxMu.Lock()
	if d.sandboxStates == nil {
		d.sandboxStates = map[string]*fleet.SandboxState{}
	}
	d.sandboxStates[name] = st
	d.sandboxMu.Unlock()
	return sandboxLaunchArgv(layout, prog, argv), func() { d.dropSandbox(name) }, nil
}

// dropSandbox forgets a session's sandbox and removes its private directory.
func (d *Driver) dropSandbox(session string) {
	if d.sandboxCfg == nil {
		return
	}
	d.sandboxMu.Lock()
	delete(d.sandboxStates, session)
	d.sandboxMu.Unlock()
	if root, why := d.sandboxRoot(); why == "" {
		_ = os.RemoveAll(newSandboxLayout(root, session).dir)
	}
}

// sandboxStateFor reports the profile a session runs under, or nil when it is
// not sandboxed. The map is filled by Create; after a restart it is filled from
// the report file, and a session with none is remembered as unsandboxed.
func (d *Driver) sandboxStateFor(session string) *fleet.SandboxState {
	if d.sandboxCfg == nil {
		return nil
	}
	d.sandboxMu.Lock()
	defer d.sandboxMu.Unlock()
	if st, ok := d.sandboxStates[session]; ok {
		return copySandboxState(st)
	}
	if d.sandboxStates == nil {
		d.sandboxStates = map[string]*fleet.SandboxState{}
	}
	var st *fleet.SandboxState
	if root, why := d.sandboxRoot(); why == "" {
		if b, err := os.ReadFile(newSandboxLayout(root, session).report); err == nil {
			var rec sandboxRecord
			// The session name is inside the record: two names that hashed alike
			// would otherwise read each other's.
			if json.Unmarshal(b, &rec) == nil && rec.Session == session {
				st = rec.State
			}
		}
	}
	d.sandboxStates[session] = st
	return copySandboxState(st)
}

func copySandboxState(st *fleet.SandboxState) *fleet.SandboxState {
	if st == nil {
		return nil
	}
	c := *st
	c.Denies = append([]fleet.SandboxDeny(nil), st.Denies...)
	c.ReadPaths = append([]fleet.AbsolutePath(nil), st.ReadPaths...)
	c.WritePaths = append([]fleet.AbsolutePath(nil), st.WritePaths...)
	if st.PackageCache != nil {
		pc := *st.PackageCache
		c.PackageCache = &pc
	}
	return &c
}

// sweepSandbox removes private directories no live session owns: a session that
// ended without Close (its process exited and was reaped by another path, the
// service was down). A directory is kept for a grace period so a session created
// a moment ago, not yet in the multiplexer's listing, is not swept from under
// its own launch.
func (d *Driver) sweepSandbox(rows []paneRow) {
	if d.sandboxCfg == nil {
		return
	}
	now := d.now()
	d.sandboxMu.Lock()
	if now.Sub(d.sandboxSwept) < sandboxSweepEvery {
		d.sandboxMu.Unlock()
		return
	}
	d.sandboxSwept = now
	d.sandboxMu.Unlock()

	root, why := d.sandboxRoot()
	if why != "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, r := range rows {
		live[newSandboxLayout(root, r.session).dir] = true
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if live[dir] {
			continue
		}
		grace := sandboxDirGrace
		if strings.HasPrefix(e.Name(), "probe-") {
			grace = sandboxProbeDirGrace
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < grace {
			continue
		}
		_ = os.RemoveAll(dir)
		d.sandboxMu.Lock()
		for name := range d.sandboxStates {
			if newSandboxLayout(root, name).dir == dir {
				delete(d.sandboxStates, name)
			}
		}
		d.sandboxMu.Unlock()
	}
}

// measureSandbox is the measurement: it declares the capability only when the
// agent CLI, started the way a create starts it and confined by the profile a
// create would write, reports a logged-in account.
func (d *Driver) measureSandbox(ctx context.Context) (*fleet.SandboxSupport, string, string) {
	host := d.sandboxHost
	if host == nil {
		host = sandbox.HostSupport
	}
	sup, why := host()
	if sup == nil {
		return nil, why, ""
	}
	if d.agentProgram == "" {
		return nil, "this driver's agent command was replaced, and the measurement is defined only for the default agent CLI", ""
	}
	if filepath.Base(d.agentProgram) != defaultAgentProgram {
		return nil, fmt.Sprintf("the measurement is defined only for the %q CLI, not %q", defaultAgentProgram, d.agentProgram), ""
	}
	configDir, why := d.sandboxConfigDir()
	if why != "" {
		return nil, why, ""
	}
	root, why := d.sandboxRoot()
	if why != "" {
		return nil, why, ""
	}
	prog, why := d.resolveAgentPath(ctx)
	if why != "" {
		return nil, why, ""
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, "making the sandbox directory: " + err.Error(), ""
	}
	probeDir, err := os.MkdirTemp(root, "probe-")
	if err != nil {
		return nil, "making a probe directory: " + err.Error(), ""
	}
	defer os.RemoveAll(probeDir)
	cwd := filepath.Join(probeDir, "cwd")
	layout := sandboxLayout{
		dir:     probeDir,
		work:    filepath.Join(probeDir, "work"),
		tmp:     filepath.Join(probeDir, "work", "tmp"),
		profile: filepath.Join(probeDir, "profile.sb"),
		report:  filepath.Join(probeDir, "sandbox.json"),
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return nil, "making a probe directory: " + err.Error(), ""
	}
	if err := os.MkdirAll(layout.tmp, 0o700); err != nil {
		return nil, "making a probe directory: " + err.Error(), ""
	}
	plan, err := d.planSandbox(fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Sandbox: &fleet.SandboxSpec{}}, prog, configDir, layout)
	if err != nil {
		return nil, "planning the probe: " + err.Error(), ""
	}
	profile, err := plan.Profile()
	if err != nil {
		return nil, "rendering the probe profile: " + err.Error(), ""
	}
	if err := os.WriteFile(layout.profile, []byte(profile), 0o600); err != nil {
		return nil, "writing the probe profile: " + err.Error(), ""
	}

	argv := sandboxLaunchArgv(layout, prog, append([]string{prog}, agentProbeArgs...))
	pctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	var out []byte
	if d.bareExec {
		out, err = d.run(pctx, "/bin/sh", append([]string{"-c", sandboxEnterScript, "muster", cwd}, argv...)...)
	} else {
		out, err = d.run(pctx, d.loginShell(), append([]string{"-lic", sandboxEnterScript, "muster", cwd}, argv...)...)
	}
	loggedIn, ok := parseAuthStatus(out)
	switch {
	case ok && loggedIn:
		sup.PackageCache = []fleet.PackageCacheMode{}
		return sup, "", prog
	case ok:
		return nil, "the agent CLI is not logged in when it runs under the profile on this host; its credentials are most likely in a store " +
			"the profile denies (the keychain), and allowing that store would open every item in it to the session", ""
	case err != nil:
		return nil, "the agent CLI could not be run under the profile to read its login status: " + err.Error(), ""
	default:
		return nil, "the agent CLI printed no login status when it ran under the profile", ""
	}
}

// parseAuthStatus reads the agent's account report out of what the probe printed.
// ok is false when no report could be found, which is "not known", never
// "logged out".
func parseAuthStatus(out []byte) (loggedIn, ok bool) {
	s := string(out)
	i := strings.Index(s, sandboxProbeBegin)
	if i < 0 {
		return false, false
	}
	s = s[i+len(sandboxProbeBegin):]
	a, b := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if a < 0 || b < a {
		return false, false
	}
	var v struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal([]byte(s[a:b+1]), &v); err != nil {
		return false, false
	}
	return v.LoggedIn, true
}

// resolveAgentPath finds the file a session's login shell would run for the
// agent program, as an absolute path, or says why it cannot.
func (d *Driver) resolveAgentPath(ctx context.Context) (string, string) {
	prog := d.agentProgram
	if strings.ContainsRune(prog, filepath.Separator) {
		if abs, err := filepath.Abs(prog); err == nil {
			return abs, ""
		}
		return "", fmt.Sprintf("the agent program %q has no usable path", prog)
	}
	if d.bareExec {
		p, err := exec.LookPath(prog)
		if err != nil {
			return "", fmt.Sprintf("the agent program %q is not on this service's PATH", prog)
		}
		return p, ""
	}
	pctx, cancel := context.WithTimeout(ctx, agentProbeTimeout)
	defer cancel()
	out, _ := d.run(pctx, d.loginShell(), "-lic", agentPathScript, "muster", prog)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, agentPathMark) {
			if p := strings.TrimPrefix(line, agentPathMark); filepath.IsAbs(p) {
				return p, ""
			}
		}
		if line == agentMissingMark {
			return "", fmt.Sprintf("the agent program %q is not found by the login shell", prog)
		}
	}
	return "", fmt.Sprintf("the login shell did not say where the agent program %q is", prog)
}
