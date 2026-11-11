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
// The cache is memory, so without more it would not survive a restart of the
// service — even though opencode's own SQLite store still has the sessions, and
// a by-id GET works regardless of which process created one. With a state
// directory (WithStateDir, muster #282) it does: see "Surviving a restart".
//
// # Surviving a restart (muster #282)
//
// With WithStateDir, Create writes one record per session (persist.go): the
// runtime's session id, the working directory, the sandbox request, the NAMES of
// the env the create carried, and where the session's scoped directory is. No env
// value and no server credential is ever written. The scoped directory — where
// opencode keeps its database — lives under the state directory instead of the
// temp directory, and stopping the service leaves it alone; only closing the
// session removes it.
//
// At start, New relaunches a process per record (resume.go), with the environment
// rebuilt from this machine's sessionEnv files, and confirms each with a by-id
// read. A session whose create carried caller-supplied env cannot be relaunched
// faithfully — the values were never kept — so it is listed `unknown`, with a
// reason saying it needs re-creation, rather than relaunched with less than it
// was given. So is one that cannot be relaunched for any other reason (the
// sandbox is no longer enforceable, its directory is gone, the runtime lost it):
// a restart adopts and never silently drops. A turn the service's stop cut short
// is reported as `lastTurn.outcome: "interrupted"`, not as an ordinary idle.
// SupportsResume is true exactly when a state directory is configured. Without
// one nothing is written and the old behaviour holds.
//
// Two limits. The idempotency table stays in memory, so a create retried across a
// restart starts a new session. And a session's conversation now lives in the
// state directory, which should be as private as the service's own state.
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
// Discard, Rename and Keys all return driver.ErrUnsupported. Respond answers a
// tool-permission ask and nothing else: opencode's other blocking surface, a
// structured "question" reply, is not a safe fit for §2.3's model without real
// design work of its own (a session-abstraction concept, not a wire mapping
// exercise), so a session parked on one is answered `unsupported` with the
// reason. Inventing a mapping to make that case non-empty would be exactly the
// emulation §5.6 forbids.
//
// # Permissions (muster #283, #296)
//
// A session the runtime has stopped on a tool-permission ask reads
// `waiting_input` with a `tool-permission` prompt instead of `working`, whose
// options are the three answers Respond takes (allow once, allow always,
// reject) and whose nonce is the ask's own id; Respond answers it through the
// runtime's permission-reply endpoint and refuses an answer meant for an ask that
// has since been resolved. `permissionMode: "bypass"` stops asks from arising by
// configuring the session's own runtime to approve its tools, and is honoured
// only inside a sandbox this driver enforces. See permission.go.
//
// # Events
//
// Subscribe is backed by the runtime's own event bus (GET /event), one
// connection per session process; subscribe.go has the mapping, what is
// dropped, and how a dropped connection is reported (muster #284).
//
// # What an idle session costs (muster #419)
//
// A server nobody talks to is cheap; what is not cheap is being asked. Every
// listing asks every live server for its status, and a server pays about 2 % of
// a core per request per second. So the status of a server, the asks it holds and
// the confirmation of an idle session are read once per window and shared, and
// dropped the moment the event stream or one of this driver's own writes says
// they changed. statuscache.go has the windows and what a reader can lose.
//
// # Parking an idle server (muster #421)
//
// What an idle server holds is memory, and reading it more cheaply gives none of
// it back. With WithIdleStop set (and a state directory), a session whose server
// has had nothing to do for the bound is PARKED: the process group is stopped as
// the service stopping stops it, and the session keeps its record and its
// directory. The next Send, Interrupt or Respond starts a server on that directory
// again (relaunch). A parked session reads idle, with evidence that says so —
// never dead, never closed — and no read starts it. A service restart leaves it
// parked while the bound that parked it still holds (muster #423): the record says
// so, and resume rebuilds the stopped server instead of starting one. park.go has
// the lifecycle, the one classifier (phaseOf) every "has the process exited" reader
// goes through, and the locking; docs/adr/421-… has the decisions and what they cost.
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
	"path/filepath"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/privfs"
	"github.com/futurelastic/muster/internal/procgroup"
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

	// How long a create waits for the runtime's model list to settle before it
	// refuses a model the first answer did not list, and how often it asks again
	// meanwhile (muster #384; catalog.go says why). With readyTimeout this stays
	// well inside defaultDeadlineMs, so waiting here cannot be what ends a create.
	defaultCatalogSettle = 8 * time.Second
	defaultCatalogPoll   = 500 * time.Millisecond
)

// DefaultDeadlineMs is the deadline a driver built without WithDeadline
// declares. It is exported so the service can describe a runtime that has no
// driver yet with the same number its driver will declare (muster #297).
const DefaultDeadlineMs = defaultDeadlineMs

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
	// probeTimeout bounds the startup `--version` probe (WithProbeTimeout).
	probeTimeout time.Duration
	now          func() time.Time
	username     string
	client       *http.Client

	// catalogSettle and catalogPoll bound the re-read of the runtime's model list
	// before a model is refused as unlisted (muster #384). Fields, not constants,
	// so a test does not spend seconds on every refusal.
	catalogSettle time.Duration
	catalogPoll   time.Duration

	// lastCatalog is the latest usable answer a catalog read produced, kept so
	// GET /v1/runtimes can list models without a request of its own (muster
	// #400). Nil until the first create has read one. catMu guards it: creates
	// run concurrently.
	catMu       sync.Mutex
	lastCatalog *catalogSnapshot

	// sessionEnv is this machine's declared identity for its sessions (#94);
	// sessionRoot is where session-scoped directories are made (the OS temp
	// directory by default); runtimePath is the operator's extra PATH
	// directories, none under the service user's home. See env.go.
	sessionEnv  []sessionenv.Entry
	sessionRoot string
	runtimePath []string

	// providerIDs is the operator's allow-list (WithProviders), providers the
	// policy built from it in New (muster #293). Empty means the default: the
	// runtime's built-in hosted provider is off and everything else is usable.
	providerIDs []string
	providers   providerPolicy

	// grants is the machine's provider sign-in (WithGrant, muster #451): each
	// session whose policy permits the provider is given its current access token,
	// and every live session is updated when it changes. grantCancel ends the
	// subscription; nil when there is none.
	grants      GrantSource
	grantCancel func()

	// agentsFile is the operator's agent definition file (WithOperatorAgents,
	// muster #396): its path is fixed here, its content is read again on every
	// create and relaunch. Empty means the machine defines none.
	agentsFile string

	// sandbox is what this driver can enforce on this host (nil: nothing) and
	// sandboxWhy says why not, in words a refused create can quote (muster
	// #281). sandboxRead is the operator's extra readable directories — an
	// interpreter or package cache that lives under the service user's home —
	// carried here because the allow-lists never come from a create.
	sandbox     *fleet.SandboxSupport
	sandboxWhy  string
	sandboxRead []string

	// stateDir is where sessions are remembered across a restart (muster #282);
	// empty means they are not. store is derived from it in New.
	stateDir string
	store    *recordStore

	// logf receives the one-line notices this driver has (muster #398): a server it
	// stopped that no session held. Nil logs nothing; WithLogf sets it.
	logf func(format string, args ...any)
	// noStartSweep turns the start-up sweep off. Only a test sets it, so the reap of
	// a recorded server (reapStale) can be asserted without the sweep stopping the
	// same process first.
	noStartSweep bool
	// noStraySweep turns off the stop of processes a session started outside its
	// server's process group (muster #405) — at close, at shutdown and at start-up.
	// Each is one read of the machine's process table, so only a test sets it: the
	// many that start and close sessions for another reason do not each pay for it,
	// and the ones about the sweep turn it back on.
	noStraySweep bool

	// baseURL and password are the shared-mode seam's inputs (WithBaseURL,
	// WithCredential); New folds them into shared.
	baseURL  string
	password string
	// shared is the one server of shared mode; nil in the production,
	// isolated mode, where every session carries its own *server.
	shared *server

	// usageMemos remembers each session's message-record usage between reads
	// (muster #285); usageMu guards the map, each memo its own contents.
	usageMu    sync.Mutex
	usageMemos map[string]*usageMemo

	// sweepStop ends the goroutine that repeats the sweep of directories no record
	// names, sweepDone is closed when it has (orphans.go; muster #419). dirSweepEvery
	// and dirScan are test seams: the interval, and the listing of running servers.
	sweepStop     chan struct{}
	sweepDone     chan struct{}
	sweepStopOnce sync.Once
	dirSweepEvery time.Duration
	dirScan       func(string) ([]serverProc, int, error)

	// idleStop is how long a session's server may have nothing to do before this
	// driver stops it and starts it again on the next input (muster #421; park.go).
	// Zero is off, and it stays off without a state directory. idleSweepEvery is how
	// often the sweep runs (a test seam); idleStopCh and idleDone are the sweep
	// goroutine's stop and its end, nil when there is none.
	idleStop       time.Duration
	idleSweepEvery time.Duration
	idleStopCh     chan struct{}
	idleDone       chan struct{}
	idleStopOnce   sync.Once
	// life holds one lifecycle lock per session (park.go); lifeMu guards the map and
	// is a leaf.
	lifeMu sync.Mutex
	life   map[string]*sync.Mutex

	// scratchLimits bound the measurement of a session's scratch directory
	// (scratch.go). The zero value means the production bounds.
	scratchLimits scratchLimits
	// scratchGate holds one token per walk running; its capacity is how many may
	// run at once across the driver (muster #431). Made by walkGate, once.
	scratchGate     chan struct{}
	scratchGateOnce sync.Once

	// statusTTL is how long a read of a server's status is kept (muster #419;
	// statuscache.go). Set to the production windows in New; a test sets it.
	statusTTL statusTTLs

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
	srv  *server
	cwd  fleet.AbsolutePath
	name string
	// agent is the agent name the runtime's session record carries. It is the
	// runtime's own report, and for a name the create gave it is only an echo —
	// see specAgent for what the session's messages are sent as.
	agent string
	// specAgent is the agent the create asked for: the one every message Send
	// delivers is sent as (muster #362), kept across a restart in the session's
	// record. Empty is a session that asked for none.
	specAgent string
	// specModel is the model the create asked for, "provider/model": what a read
	// reports as the pin's request (muster #437). The runtime never echoes it, so
	// without it kept here — and in the session's record, across a restart — a read
	// could not say what was asked for. Empty is a session that asked for none.
	specModel string
	startedAt time.Time

	// dir is the session's scoped directory, kept for a session with no server
	// so closing it can remove what it left (muster #282).
	dir string
	// unknown is why this session has no running server: it was found on disk
	// after a restart and could not be relaunched. srv is nil exactly then.
	unknown string
	// interrupted is set on a relaunched session whose last turn did not finish
	// before the service stopped. A new turn (Send) clears it.
	interrupted *fleet.TurnEnd
	// turn is what this driver has learned of how the session's newest turn ended
	// (muster #426; lastturn.go), kept so a listing can say so without asking.
	turn turnRecord

	// contextRef is the file the create named as the session's context, and
	// context the text it held when this driver read it (muster #371). The path is
	// what a restart finds again; the text is what every prompt carries as the
	// runtime's system field. Both empty for a session created without one.
	contextRef fleet.AbsolutePath
	context    string

	// conversation is what this driver reports as the session's conversation
	// (muster #378, spec §2.9): captured at create, confirmed after a restart,
	// or unknown with the reason. Always set for a session this driver created
	// or relaunched; see conversation.go.
	conversation *fleet.ConversationRef
}

type idemEntry struct {
	ref     fleet.SessionRef
	created time.Time
}

// idempotencyRetention mirrors the tmux driver's window: long enough to
// cover a caller's realistic retry, short enough not to grow without
// bound. In-memory only: sessions survive a restart (muster #282) but this table
// does not, so a create retried across one starts a new session — a limit the
// package doc states rather than hides.
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

// sandboxUnavailable is the reason Sandbox is absent, or empty when it is present.
func (d *Driver) sandboxUnavailable() string {
	if d.sandbox != nil {
		return ""
	}
	return d.sandboxWhy
}

// WithSandboxReadPaths adds directories every sandboxed session may read, ahead
// of what a create asks for: the interpreter or shared package cache a runtime
// needs that lives somewhere the default profile does not reach (muster #281).
// This is the only place such a grant can come from — a create can add paths of
// its own but can never widen the allow-lists the profile is built on.
func WithSandboxReadPaths(dirs []string) Option { return func(d *Driver) { d.sandboxRead = dirs } }

// WithProviders names the providers, and optionally single models, a session
// may use, and no others (muster #293, #401). The providers are written into
// each session's own configuration as opencode's enabled_providers, a create
// whose model is outside the policy is refused, and a create the runtime has no
// usable model for is refused too.
//
// An entry is a provider id (every model on it); provider/<model> or
// provider/<glob> (only the matching models); or !provider/<glob>, a deny that
// beats any allow. Prefer an allowlist of exact models to a deny pattern: a name
// pattern such as a suffix is not a reliable mark of a model's terms. A malformed
// entry makes New fail, naming it. Model rules are checked at create and on each
// turn's record; they are not in the session's own configuration.
//
// Empty (the default) is not "anything goes": the runtime's built-in hosted
// provider is disabled and every other provider the session has a credential
// for is usable. Name that provider here (it is "opencode") to ask for it, on
// purpose, for sessions that have no credential of their own. Each provider id
// is as the runtime spells it.
func WithProviders(ids []string) Option { return func(d *Driver) { d.providerIDs = ids } }

// WithOperatorAgents names a JSON file of opencode agents every isolated
// session on this machine receives (muster #396), so an agent the operator
// defines once can be named on any create without a change to any repo. The path
// is absolute and fixed here; the file is read again on every create and every
// relaunch, so editing an agent needs no restart, and a file that cannot be used
// refuses the create with the reason. Empty (the default) defines none. Ignored in
// shared mode, which has no configuration of its own to write them into.
// operatoragents.go has the file's shape and how it meets a project's agents.
func WithOperatorAgents(path string) Option { return func(d *Driver) { d.agentsFile = path } }

// WithLogf gives the driver somewhere to say that it stopped a server no
// session held (muster #398): the start-up sweep and the reap of a server the
// previous service left behind each log one line per process. Nothing else is
// logged, and no line carries an environment value or a credential.
func WithLogf(f func(format string, args ...any)) Option { return func(d *Driver) { d.logf = f } }

// withoutStartSweep turns off the sweep New runs before it resumes. Unexported:
// only a test needs the reap of a recorded server to be the one that acts.
func withoutStartSweep() Option { return func(d *Driver) { d.noStartSweep = true } }

// withoutStraySweep and withStraySweep set noStraySweep. Unexported: a test seam.
func withoutStraySweep() Option { return func(d *Driver) { d.noStraySweep = true } }
func withStraySweep() Option    { return func(d *Driver) { d.noStraySweep = false } }

// WithStateDir makes sessions survive a service restart (muster #282): one
// record per session, and the sessions' scoped directories, live under dir. With
// it SupportsResume is true; without it nothing is written and a restart forgets
// every session, as before. Ignored in shared mode, where the server is not this
// driver's to restart.
//
// The directory holds each session's runtime database and conversation, so it
// should be as private as the service's own state: records and directories are
// created readable by the service user alone.
func WithStateDir(dir string) Option { return func(d *Driver) { d.stateDir = dir } }

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

// WithProbeTimeout bounds the `--version` probe New runs (muster #297). A
// non-positive value is ignored, like WithDeadline: the default (5s) stands.
// The service retries a probe that timed out with a longer bound — a binary
// that is slow under load is the same binary later.
func WithProbeTimeout(d time.Duration) Option {
	return func(x *Driver) {
		if d > 0 {
			x.probeTimeout = d
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

// withDirSweep sets the interval of the repeated directory sweep and the listing of
// running servers it uses. Unexported: a test seam.
func withDirSweep(every time.Duration, scan func(string) ([]serverProc, int, error)) Option {
	return func(d *Driver) { d.dirSweepEvery, d.dirScan = every, scan }
}

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
//
// When the refusal is about the binary — not found, not runnable, or its
// version check did not answer in time — the error is a
// *driver.UnavailableError carrying which, so a caller can retry the one cause
// that time fixes (muster #297). Any other error is a refusal of the
// configuration, and no retry changes it.
func New(ctx context.Context, machine fleet.MachineId, opts ...Option) (*Driver, error) {
	d := &Driver{
		machine:      machine,
		runtime:      DefaultRuntime,
		bin:          defaultBin,
		deadline:     defaultDeadlineMs * time.Millisecond,
		probeTimeout: defaultProbeTimeout,
		now:          time.Now,
		username:     defaultUsername,
		client:       &http.Client{},
		seen:         make(map[string]knownSession),
		subs:         make(map[*subscription]struct{}),
		live:         make(map[*server]struct{}),
		idem:         make(map[string]idemEntry),
		inflight:     make(map[string]chan struct{}),
	}
	d.catalogSettle, d.catalogPoll = defaultCatalogSettle, defaultCatalogPoll
	d.statusTTL = statusTTLs{bus: statusTTLBus, bare: statusTTLBare}
	for _, o := range opts {
		o(d)
	}
	pol, err := newProviderPolicy(d.providerIDs)
	if err != nil {
		return nil, err
	}
	d.providers = pol

	if d.baseURL != "" {
		// Test/advanced seam: the caller already has a server (real or
		// fake) and told us where it is. Nothing to spawn or probe, and
		// nothing to isolate: one server serves every session.
		d.shared = &server{baseURL: d.baseURL, username: d.username, password: d.password}
		d.sandbox, d.sandboxWhy = sandboxSupport(true)
		return d, nil
	}

	if !procgroup.Supported() {
		return nil, errors.New("opencode: this platform cannot signal a process group, so a session's tools " +
			"could outlive its close; the driver refuses to start")
	}
	// Everything that is pure configuration is checked BEFORE the probe, for
	// two reasons (muster #297): a misconfigured runtime is not retried — the
	// service reads the plain error as "start-failed", which no amount of
	// waiting fixes — and a retry never repeats work that cannot change.
	if err := checkRuntimePath(d.runtimePath); err != nil {
		return nil, err
	}
	if err := checkSessionEnvEntries(d.sessionEnv, d.runtime); err != nil {
		return nil, err
	}
	// Folded once, on a copy, so a create's names (folded the same way) meet a
	// configured entry exactly. A no-op where names are case-sensitive.
	d.sessionEnv = foldEntryNames(d.sessionEnv)
	if d.stateDir != "" && !filepath.IsAbs(d.stateDir) {
		return nil, fmt.Errorf("opencode: the state directory %q must be an absolute path", d.stateDir)
	}
	avail := probe(ctx, d.bin, d.probeTimeout)
	if !avail.Installed {
		// Typed, so the caller can tell a binary that was slow from one that
		// is not there: only the first is worth trying again.
		return nil, &driver.UnavailableError{
			Cause: avail.Cause,
			Err:   fmt.Errorf("opencode: not available on this machine: %w", avail.Err),
		}
	}
	d.bin = avail.Path
	if d.stateDir != "" {
		storeDir, stateRoot := stateLayout(d.stateDir)
		d.store = &recordStore{dir: storeDir}
		if d.sessionRoot == "" {
			// A session's database must outlive the service, so its directory
			// cannot live under the temp directory (muster #282).
			d.sessionRoot = stateRoot
		}
		for _, dir := range []string{d.store.dir, d.sessionRoot} {
			if err := privfs.MkdirAll(dir); err != nil {
				return nil, fmt.Errorf("opencode: creating the state directory: %w", err)
			}
		}
	}
	if d.sessionRoot == "" {
		d.sessionRoot = os.TempDir()
	}
	d.sandbox, d.sandboxWhy = sandboxSupport(false)
	if d.store != nil {
		// muster #398: a server an earlier service left running is stopped BEFORE
		// anything is resumed, whether or not a record still names it; at this point
		// this driver holds no server, so every one found is not ours.
		d.sweepOrphans()
		// muster #405: and the processes its sessions started that left the server's
		// process group, which no group stop reaches.
		d.sweepOrphanStrays()
		// muster #399: and the scoped directories no record names, after the
		// servers that held them are gone and before a record can be resumed.
		d.sweepOrphanDirs()
		d.resume(ctx)
		// muster #419: and again from time to time, for the directories that lose
		// their owner while the service runs.
		d.startDirSweep()
		// muster #421: and the sessions that have nothing to do are parked, once
		// every one that could be started again has been (above).
		d.startIdleSweep()
	} else if d.idleStop > 0 {
		d.say("opencode: an idle stop was configured but there is no state directory, so a stopped session could not be " +
			"started again; no session will be stopped")
		d.idleStop = 0
	}
	// muster #451: last, so a start that failed above leaves no subscription behind.
	// Every live session's sign-in is rewritten whenever the access token changes.
	if d.grants != nil {
		d.grantCancel = d.grants.Subscribe(d.pushAccess)
	}
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
	// Before the servers are paused: no sweep may be listing or removing
	// directories while they are being stopped (muster #419).
	d.stopDirSweep()
	d.stopIdleSweep() // muster #421: a park in progress finishes first
	d.endSubscriptions()
	if d.grantCancel != nil {
		d.grantCancel() // muster #451: no token renewal reaches a driver that is stopping
	}

	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			// The service stopping is not the session closing (muster #282).
			s.pauseForShutdown()
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
	s.straySweep = !d.noStraySweep
	s.onTeardown = func(s *server) {
		d.mu.Lock()
		delete(d.live, s)
		d.mu.Unlock()
		d.sayStrays("still running when their session ended", s.strayStopped, s.strayLeft)
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
// ValidatesModel is true: Create checks a requested model against the
// runtime's own catalog (catalog.go) — though where a runtime offers no usable
// catalog the create proceeds unchecked, as before.
func (d *Driver) Capabilities() fleet.DriverCapabilities {
	return fleet.DriverCapabilities{
		ObservesState:    true,
		DeliversRawKeys:  false,
		ConfirmsDelivery: false,
		// True only with a state directory (muster #282): every session is then
		// remembered, relaunched at start and confirmed by id, and one that cannot
		// be relaunched faithfully is listed unknown with the reason.
		SupportsResume: d.store != nil,
		SupportsPin: fleet.PinSupport{
			Model:  true,
			Effort: false,
			Agent:  true,
		},
		ValidatesModel: true,
		// IsolatesEnvironment: each session has its own process, started
		// with a built environment (env.go). Shared mode — the test seam —
		// has one server for everyone and cannot.
		IsolatesEnvironment: d.shared == nil,
		// Sandbox: nil where this host cannot deny files, unix sockets and
		// system services together (sandbox.go) — a capability absent, never a
		// weaker profile under the same name.
		Sandbox: d.sandbox,
		// And the reason, so a posture refusal can say why (muster #345).
		SandboxUnavailable: d.sandboxUnavailable(),
		// muster #283: in isolated mode this driver wrote the session's
		// configuration itself and reports it back; shared mode has no posture
		// of its own to report.
		ObservesPermissionMode: d.shared == nil,
		// muster #329: this runtime has no per-session permission-mode flag; it
		// is given a posture (bypass, inside a sandbox) or none.
		CreatePermissionModes: []string{string(fleet.PermissionModeDefault), fleet.PermissionModeBypass},
		// muster #285: tokens and cost are read from the runtime's own message
		// record, summed over every step.
		ReportsUsage: true,
		// muster #379, declared (never derived): see DriverCapabilities.
		//
		// turns: GET …/turns reads the runtime's own message record (#369).
		ReadsTurns: fleet.Declared(true),
		// muster #441: this runtime has no record reader yet (a later change
		// adds one and flips this); until then the route answers unsupported.
		ReadsRecord: fleet.Declared(false),
		// conversation: every session carries the runtime's own id, captured at
		// create (#378, conversation.go).
		ReportsConversation: fleet.Declared(true),
		// respond: a tool-permission ask only. A runtime question is never
		// reported as a prompt and respond answers it `unsupported` (permission.go),
		// and an ask carries a kind, so nothing unclassified is answered.
		Respond: &fleet.RespondSupport{Kinds: []fleet.PromptKind{fleet.PromptToolPermission}},
		// A running session's mode cannot change: the posture is written into the
		// session's configuration at create and there are no raw keys to change it
		// with. CreatePermissionModes above is the only way to choose one.
		PermissionModeChange: &fleet.PermissionModeChangeSupport{Live: false},
		DeadlineMs:           d.deadline.Milliseconds(),
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

// refreshSeen changes what is known of id in place, under the lock, and only if id
// is still known: unlike markSeen it neither writes back an older copy of the entry
// (whose server may have been replaced since, muster #421) nor brings back a
// session that was forgotten meanwhile.
func (d *Driver) refreshSeen(id string, change func(*knownSession)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if k, ok := d.seen[id]; ok {
		change(&k)
		d.seen[id] = k
	}
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
	gone := d.seen[id]
	delete(d.seen, id)
	d.mu.Unlock()
	d.forgetIdle(gone.srv, id) // muster #419
	d.forgetUsage(id)
	d.dropLifeLock(id) // muster #421
	if d.store != nil {
		// Only ever called once the runtime confirmed the session is gone, which is
		// also when it stops being worth finding again (muster #282).
		_ = d.store.remove(id)
	}
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
