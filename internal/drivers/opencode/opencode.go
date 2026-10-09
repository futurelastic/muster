// Package opencode implements driver.Driver over opencode
// (github.com/sst/opencode), a third-party open-source coding agent this
// driver launches as a subprocess and talks to over HTTP.
//
// This is muster's second local driver, and its whole reason to exist
// is muster issue #55: the first driver against a runtime genuinely
// different from the incumbent multiplexer-driven one, chosen because it
// answers "is this session busy" from a structured API rather than a
// terminal screen. Where the tmux driver infers, this one observes — see
// Capabilities, and state.go's classify.
//
// # We launch it, so discovery is not a problem here
//
// opencode's `serve` subcommand takes --port and --hostname directly, and
// its `acp` subcommand (Agent Client Protocol — a structured stdio channel)
// starts the SAME HTTP server underneath with the same flags. One
// integration therefore covers both surfaces; this driver only needs the
// HTTP one. Because this driver is the one that execs the process, there is
// no port file, no environment variable and no mDNS advertisement to trust:
// the port is chosen by binding an ephemeral listener and closing it
// (process.go's freePort), and the credential is generated here and handed
// to the child only through its environment (process.go's New). --mdns is
// never passed — it defaults the bind to 0.0.0.0, which nothing here wants.
//
// # One server per session, with a built environment (muster #280)
//
// The driver used to run ONE `opencode serve` for every session, and that
// process inherited the service's whole environment and working directory. A
// shared process has no honest way to give one session a variable another does
// not see, so a create carrying `env` was refused, and a caller that wanted a
// session holding only a model-provider key had no way to get it.
//
// Now each create starts its own server (process.go): its own port, its own
// credential, its own process group, started in the session's own working
// directory with an environment that is BUILT rather than inherited (env.go) —
// a small documented base, this machine's sessionEnv, and the create's env.
// Close and Shutdown kill the whole process group and remove the session's
// scoped HOME/TMPDIR/XDG directory, so nothing outlives the session.
//
// Three limits are stated here so nobody reads more into it than it does. The
// model-provider key a session needs is in its environment, and opencode
// exposes that environment to its tool shell: isolation scopes every OTHER
// credential and cannot protect the model key itself, so give each lane its
// own spend-capped key. The filesystem is not confined unless the create asks
// for a sandbox (sandbox.go, #281): without one a session can read what the
// service user can. And the
// working directory's AGENTS.md and CLAUDE.md reach the runtime (up to 64 KB;
// @path imports are not followed by one of the harnesses measured).
//
// A consequence operators will meet: a session no longer sees the service
// user's own opencode login or configuration (HOME is the session's). The
// provider credential it needs arrives through the create's env or this
// machine's sessionEnv, or through a project-level opencode.json in the
// session's working directory.
//
// WithBaseURL keeps a "shared mode" for tests: one already-running server, no
// isolation, and Capabilities.IsolatesEnvironment says so.
//
// # This driver's session universe is what it has cached, not a runtime enumeration
//
// GET /session (list every session) is documented as returning the whole
// collection, unfiltered when no `directory` query parameter is given.
// Measured against a real server, it did not: a query with no filter,
// against a session this very driver had just created and could
// immediately read back with GET /session/{id}, returned zero results.
// The endpoint appears to be scoped to some notion of a "current project"
// this driver never set, which is not documented anywhere the OpenAPI
// description exposes — see List's doc comment for the measurement.
//
// So this driver never calls it. Every session it knows about is cached
// locally from the moment it was created or last confirmed by a direct
// by-id read (the seen map, written by Create and by State's existence
// check) — the same shape as tmux driver's own observed map, arrived at
// for an unrelated reason. List answers entirely from that cache plus one
// call to the status endpoint, never from a runtime-side enumeration.
//
// That has one direct consequence: SupportsResume is false. This driver's
// cache does not survive its own process restarting, so a restarted driver
// reports none of its pre-restart sessions — even though opencode's own
// SQLite store still has them — until they are recreated or a future
// revision adds a way to rediscover them (by-id GET works regardless of
// which process created a session, so this is a real gap to close, not a
// structural one).
//
// # Two traps from #55's measured findings, both handled in state.go
//
// The status endpoint (GET /session/status) omits idle sessions from its
// response map entirely — measured live, not read from source. A read that
// fails (network error, or the runtime rejecting this driver's own
// credential) renders as exactly the same empty-looking absence at the
// HTTP layer as "everyone is idle" would, and §5.7 forbids conflating them.
// See client.go's do, which turns a transport failure or a 401 into a Go
// error rather than an empty successful body, and state.go's classify,
// which is never even called unless the read that fed it demonstrably
// succeeded.
//
// The runtime's own status union also has no member for what a
// multi-session listing must do when it can enumerate *which* sessions
// exist but not *how busy* they are (session list succeeded, status read
// failed): see List's use of fleet.UnknownState per session in that case,
// which is the finer-grained sibling of the same rule.
//
// # What this driver deliberately does not attempt
//
// Respond, Discard, Rename and Keys all return driver.ErrUnsupported.
// opencode has no boot-time enumerated-menu prompt matching
// fleet.SessionPrompt's shape — its blocking questions are tool-permission
// approvals and, in a newer API, structured "question" replies, neither of
// which is a safe fit for §2.3's model without real design work of its own
// (a session-abstraction concept, not a wire mapping exercise). Inventing
// one to make the method non-empty would be exactly the emulation §5.6
// forbids.
//
// # Events
//
// Subscribe is backed by the runtime's own event bus (GET /event), one
// connection per session process; subscribe.go has the mapping, what is
// dropped, and how a dropped connection is reported (muster #284).
//
// KeySender is not implemented at all — this substrate has no screen for a
// raw key to land on, and DeliversRawKeys: false says so structurally
// rather than through a stub that always refuses.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// DefaultRuntime is the runtime id this driver registers under.
const DefaultRuntime fleet.RuntimeId = "opencode"

const (
	defaultBin        = "opencode"
	defaultUsername   = "muster"
	defaultDeadlineMs = 30000
	readyTimeout      = 10 * time.Second
	readyPollInterval = 100 * time.Millisecond
)

// Driver implements driver.Driver over opencode servers this driver itself
// owns the lifecycle of: one server — one process, one process group, one
// credential, one scoped environment — PER SESSION (muster #280).
//
// WithBaseURL turns the driver into "shared mode": it talks to one
// already-running server it did not start, for tests. Shared mode cannot
// isolate anything and says so (Capabilities.IsolatesEnvironment is false).
type Driver struct {
	machine  fleet.MachineId
	runtime  fleet.RuntimeId
	bin      string
	deadline time.Duration
	now      func() time.Time
	username string
	client   *http.Client

	// sessionEnv is this machine's declared identity for its sessions (#94);
	// sessionRoot is where session-scoped directories are made (the OS temp
	// directory by default); runtimePath is the operator's extra PATH
	// directories, none under the service user's home. See env.go.
	sessionEnv  []sessionenv.Entry
	sessionRoot string
	runtimePath []string

	// sandbox is what this driver can enforce on this host (nil: nothing) and
	// sandboxWhy says why not, in words a refused create can quote (muster
	// #281). sandboxRead is the operator's extra readable directories — an
	// interpreter or package cache that lives under the service user's home —
	// carried here because the allow-lists never come from a create.
	sandbox     *fleet.SandboxSupport
	sandboxWhy  string
	sandboxRead []string

	// baseURL and password are the shared-mode seam's inputs (WithBaseURL,
	// WithCredential); New folds them into shared.
	baseURL  string
	password string
	// shared is the one server of shared mode; nil in the production,
	// isolated mode, where every session carries its own *server.
	shared *server

	// subs are the open event streams (subscribe.go); subMu guards the set.
	subMu sync.Mutex
	subs  map[*subscription]struct{}

	mu     sync.RWMutex
	seen   map[string]knownSession // id -> what this driver has locally cached; see package doc's scope boundary
	live   map[*server]struct{}    // every server this driver started and has not torn down, session created or not
	closed bool

	idemMu   sync.Mutex
	idem     map[string]idemEntry
	inflight map[string]chan struct{} // idempotency key -> closed when the create holding it finishes
}

// knownSession is what this driver remembers locally about a session it has
// created or directly read — the cache List reads from instead of the
// runtime's own (unreliable, see List's doc comment) bulk listing endpoint,
// and the memory State's existence check writes back to.
type knownSession struct {
	// srv is the server this session lives on: the routing table for every
	// call about it. Its own process in isolated mode, d.shared in shared mode.
	srv       *server
	cwd       fleet.AbsolutePath
	name      string
	agent     string
	startedAt time.Time
}

type idemEntry struct {
	ref     fleet.SessionRef
	created time.Time
}

// idempotencyRetention mirrors the tmux driver's window: long enough to
// cover a caller's realistic retry, short enough not to grow without
// bound. In-memory only, deliberately — see the package doc's SupportsResume
// note; persisting this without also persisting session recovery would be
// half of #10's guarantee, which is worse than honestly declaring neither.
const idempotencyRetention = 10 * time.Minute

// Option configures a Driver.
type Option func(*Driver)

// WithBinary overrides the executable resolved by Probe / New. Empty (the
// default) resolves "opencode" on PATH.
func WithBinary(bin string) Option { return func(d *Driver) { d.bin = bin } }

// WithSessionEnv declares this machine's identity for its sessions (muster
// #94): the same entries the multiplexer driver applies, with the same
// precedence table, so a variable the operator configures reaches a session on
// either runtime. A name this driver sets itself (HOME, TMPDIR, XDG_*,
// OPENCODE_SERVER_*) is refused at New.
func WithSessionEnv(entries []sessionenv.Entry) Option {
	return func(d *Driver) { d.sessionEnv = entries }
}

// WithSessionRoot sets the directory session-scoped directories are made in.
// Empty (the default) uses the OS temp directory.
func WithSessionRoot(dir string) Option { return func(d *Driver) { d.sessionRoot = dir } }

// WithRuntimePath adds directories to the PATH a session gets, ahead of the
// system directories. Each must be absolute and not under the service user's
// home (checked at New).
func WithRuntimePath(dirs []string) Option { return func(d *Driver) { d.runtimePath = dirs } }

// WithSandboxReadPaths adds directories every sandboxed session may read, ahead
// of what a create asks for: the interpreter or shared package cache a runtime
// needs that lives somewhere the default profile does not reach (muster #281).
// This is the only place such a grant can come from — a create can add paths of
// its own but can never widen the allow-lists the profile is built on.
func WithSandboxReadPaths(dirs []string) Option { return func(d *Driver) { d.sandboxRead = dirs } }

// WithDeadline sets DriverCapabilities.DeadlineMs (§4.4). Non-positive
// values are ignored, so a zero-value Option never produces an
// undeadlined driver.
func WithDeadline(ms int64) Option {
	return func(d *Driver) {
		if ms > 0 {
			d.deadline = time.Duration(ms) * time.Millisecond
		}
	}
}

// WithBaseURL points this driver at an already-running server instead of
// spawning one, and skips Probe entirely. This is the test seam: paired
// with WithHTTPClient it lets state_test.go and client_test.go exercise
// every mapping decision against an httptest.Server, satisfying the
// provider ruling on #55 that the bulk of this driver's tests must not
// require a real opencode install or a paid credential.
func WithBaseURL(url string) Option { return func(d *Driver) { d.baseURL = url } }

// WithHTTPClient replaces the HTTP client. See WithBaseURL.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Driver) {
		if c != nil {
			d.client = c
		}
	}
}

// WithCredential sets the Basic auth identity this driver presents to its
// own server. See WithBaseURL — production callers never need this,
// because New generates a fresh credential and starts the server with it
// in the same call.
func WithCredential(username, password string) Option {
	return func(d *Driver) {
		if username != "" {
			d.username = username
		}
		d.password = password
	}
}

func withClock(f func() time.Time) Option { return func(d *Driver) { d.now = f } }

// New builds a Driver. Unless WithBaseURL was supplied it probes for the
// opencode binary (never a startup crash when it is absent — see Probe) and
// starts NOTHING: a server is started for each session, at Create.
//
// A non-nil error here is itself the "absent install is a first-class
// answer" contract discharged: it is returned to the caller (ordinarily
// cmd/muster/main.go) to decide what to do — log and continue
// without this runtime, in that caller's case — rather than this package
// ever calling log.Fatal or panicking on a machine that simply does not
// have opencode installed.
func New(ctx context.Context, machine fleet.MachineId, opts ...Option) (*Driver, error) {
	d := &Driver{
		machine:  machine,
		runtime:  DefaultRuntime,
		bin:      defaultBin,
		deadline: defaultDeadlineMs * time.Millisecond,
		now:      time.Now,
		username: defaultUsername,
		client:   &http.Client{},
		seen:     make(map[string]knownSession),
		subs:     make(map[*subscription]struct{}),
		live:     make(map[*server]struct{}),
		idem:     make(map[string]idemEntry),
		inflight: make(map[string]chan struct{}),
	}
	for _, o := range opts {
		o(d)
	}

	if d.baseURL != "" {
		// Test/advanced seam: the caller already has a server (real or
		// fake) and told us where it is. Nothing to spawn or probe, and
		// nothing to isolate: one server serves every session.
		d.shared = &server{baseURL: d.baseURL, username: d.username, password: d.password}
		d.sandbox, d.sandboxWhy = sandboxSupport(true)
		return d, nil
	}

	if !groupsSupported {
		return nil, errors.New("opencode: this platform cannot signal a process group, so a session's tools " +
			"could outlive its close; the driver refuses to start")
	}
	avail := Probe(ctx, d.bin)
	if !avail.Installed {
		return nil, fmt.Errorf("opencode: not available on this machine: %w", avail.Err)
	}
	d.bin = avail.Path
	if err := checkRuntimePath(d.runtimePath); err != nil {
		return nil, err
	}
	if err := checkSessionEnvEntries(d.sessionEnv); err != nil {
		return nil, err
	}
	if d.sessionRoot == "" {
		d.sessionRoot = os.TempDir()
	}
	d.sandbox, d.sandboxWhy = sandboxSupport(false)
	return d, nil
}

// Shutdown stops every server this Driver started, kills each one's process
// group and removes each one's session directory. Idempotent, and a no-op for
// a Driver built with WithBaseURL, which started none. After it, Create
// refuses.
func (d *Driver) Shutdown() error {
	d.mu.Lock()
	d.closed = true
	servers := make([]*server, 0, len(d.live))
	for s := range d.live {
		servers = append(servers, s)
	}
	d.seen = make(map[string]knownSession)
	d.mu.Unlock()
	d.endSubscriptions()

	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			s.teardown()
		}(s)
	}
	wg.Wait()
	return nil
}

// trackServer registers a started server so Shutdown can find it even before
// its session exists. It reports false when the driver has been shut down.
func (d *Driver) trackServer(s *server) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	s.onTeardown = func(s *server) {
		d.mu.Lock()
		delete(d.live, s)
		d.mu.Unlock()
	}
	d.live[s] = struct{}{}
	return true
}

var _ driver.Driver = (*Driver)(nil)

// Capabilities declares what this driver can do (§4.3, #55).
//
// ObservesState is true — the first local driver in this repository able
// to say so honestly, and the entire point of #55: GET /session/status
// returns a structured three-variant union, not a screen a classifier
// pattern-matches. DeliversRawKeys is false because this substrate has no
// screen for a raw key to land on — degrade, not emulate (§5.6). SupportsPin
// reflects Create's own refusals: Model and Agent are genuinely honoured
// (opencode's create body carries both), Effort is not — there is no
// analogous parameter, and Create refuses rather than silently drop it.
func (d *Driver) Capabilities() fleet.DriverCapabilities {
	return fleet.DriverCapabilities{
		ObservesState:    true,
		DeliversRawKeys:  false,
		ConfirmsDelivery: false,
		SupportsResume:   false,
		SupportsPin: fleet.PinSupport{
			Model:  true,
			Effort: false,
			Agent:  true,
		},
		// IsolatesEnvironment: each session has its own process, started
		// with a built environment (env.go). Shared mode — the test seam —
		// has one server for everyone and cannot.
		IsolatesEnvironment: d.shared == nil,
		// Sandbox: nil where this host cannot deny files, unix sockets and
		// system services together (sandbox.go) — a capability absent, never a
		// weaker profile under the same name.
		Sandbox:    d.sandbox,
		DeadlineMs: d.deadline.Milliseconds(),
		// A local driver is describing itself — no network between the
		// claim and its subject (same reasoning as the tmux driver's
		// Capabilities).
		Source: fleet.CapabilitiesObserved,
	}
}

// bounded applies this driver's declared deadline, or the caller's if
// shorter (§4.4).
func (d *Driver) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	own := d.now().Add(d.deadline)
	if dl, ok := ctx.Deadline(); ok && dl.Before(own) {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, own)
}

// matchesFilter is the same predicate the tmux driver applies — kept
// identical rather than shared, since the two packages otherwise have no
// dependency on each other and one three-field predicate is not worth an
// import for.
func matchesFilter(s fleet.Session, f driver.ListFilter) bool {
	if f.Status != "" && s.State.Status != f.Status {
		return false
	}
	if f.Agent != "" && s.Agent != f.Agent {
		return false
	}
	if f.CwdPrefix != "" && !strings.HasPrefix(string(s.Cwd), f.CwdPrefix) {
		return false
	}
	return true
}

// idemLookup returns a previously-created session for key, if the record
// has not expired.
func (d *Driver) idemLookup(key string) (fleet.SessionRef, bool) {
	d.idemMu.Lock()
	defer d.idemMu.Unlock()
	e, ok := d.idem[key]
	if !ok || d.now().Sub(e.created) > idempotencyRetention {
		return fleet.SessionRef{}, false
	}
	return e.ref, true
}

func (d *Driver) idemStore(key string, ref fleet.SessionRef) {
	d.idemMu.Lock()
	defer d.idemMu.Unlock()
	d.idem[key] = idemEntry{ref: ref, created: d.now()}
}

// markSeen records what this driver knows about id — see the package doc's
// scope-boundary note and knownSession's own doc comment.
func (d *Driver) markSeen(id string, info knownSession) {
	d.mu.Lock()
	d.seen[id] = info
	d.mu.Unlock()
}

func (d *Driver) wasSeen(id string) (knownSession, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	info, ok := d.seen[id]
	return info, ok
}

// forgetSeen removes id from this driver's cache — the counterpart
// markSeen never had (muster #78). List answers entirely from this
// cache (see knownIDs' own comment on why: the runtime's own bulk listing
// is measured unreliable, not a design this driver chose for its own
// sake), so an id nothing ever prunes from it is an id List reports
// forever, however confidently the runtime itself says otherwise.
//
// Called only once this driver has the runtime's own confirmation the
// session is gone — a successful DELETE, or a 404 on a read that expected
// to find it — never speculatively: forgetting an id this driver might
// still need to corroborate a later call against (§5.4) would trade one
// bug for a worse one.
func (d *Driver) forgetSeen(id string) {
	d.mu.Lock()
	delete(d.seen, id)
	d.mu.Unlock()
}

// knownIDs returns a snapshot of every session this driver has cached,
// keyed by id — used by List, which builds its answer from this cache
// rather than the runtime's own unreliable bulk listing (see List's doc
// comment).
func (d *Driver) knownIDs() map[string]knownSession {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]knownSession, len(d.seen))
	for id, info := range d.seen {
		out[id] = info
	}
	return out
}
