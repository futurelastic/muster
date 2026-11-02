package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #336: on macOS every process in a multiplexer server inherits the
// security session of whoever started the server. When this driver's first
// create finds no server it starts one itself, and the server then carries
// THIS process's context. A service in the desktop (Aqua) context therefore
// puts every agent session in it, and an agent runtime that moves its saved
// login into the login Keychain loses it the moment the same sessions are
// reached from the Background context, which cannot read that Keychain.
// Nothing shows that this happened or which context the server is in.
//
// Two halves, both macOS-only (any other platform: no change):
//
//   - the guard: before the create that would START a server, the service's
//     own context must be exactly "Background". Anything else — a desktop
//     context, or one that cannot be read — refuses the create, unsupported and
//     not retryable, reason fleet.ReasonUnsafeServerContext. An operator may
//     override it only with the explicit WithAllowDesktopServer setting; it is
//     never the default.
//   - the report: Capabilities carries fleet.ServerContext, so GET /v1/health
//     shows this service's context and the running server's. A server already
//     running in a desktop context is a visible warning even when the service
//     is fine, because the service attaches to an existing server and cannot
//     change its context.

// contextBackground is the one launchd session name a server may be started in.
const contextBackground = "Background"

// contextUnknown is reported when a context could not be read.
const contextUnknown = "unknown"

// serverContextTTL bounds how often the server's own context is re-measured
// for GET /v1/health. A server's context never changes for its lifetime, so
// the only thing that goes stale is "which server (if any) is running".
const serverContextTTL = 30 * time.Second

// serverContextProbeTimeout bounds one measurement, so a wedged server cannot
// hold a health request.
const serverContextProbeTimeout = 3 * time.Second

// serverProbePoll is how often a measurement looks for the answer its job
// leaves behind.
const serverProbePoll = 20 * time.Millisecond

// managerNameFunc returns this process's launchd session name.
type managerNameFunc func(ctx context.Context) (string, error)

// readManagerName is `launchctl managername`. A variable so tests never depend
// on the context the test run itself happens to be in.
var readManagerName managerNameFunc = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "launchctl", "managername").Output()
	return string(out), err
}

// contextNameRE accepts what a launchd session name looks like (Aqua,
// Background, LoginWindow, System, StandardIO). Anything else, including an
// error message or a banner, reads as unknown rather than being echoed onto
// the wire.
var contextNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,31}$`)

func cleanContextName(raw string) string {
	s := strings.TrimSpace(raw)
	if !contextNameRE.MatchString(s) {
		return contextUnknown
	}
	return s
}

// WithAllowDesktopServer is the explicit operator override of the guard: a
// create may start the server from a context that is not Background. Off
// unless the composition root turns it on.
func WithAllowDesktopServer(on bool) Option {
	return func(d *Driver) { d.allowDesktopServer = on }
}

func withGOOS(goos string) Option { return func(d *Driver) { d.goos = goos } }
func withManagerName(f managerNameFunc) Option {
	return func(d *Driver) { d.managerName = f }
}

// contextAware reports whether this host has a security-session context to
// guard. Only macOS does.
func (d *Driver) contextAware() bool {
	goos := d.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	return goos == "darwin"
}

func (d *Driver) ownContext(ctx context.Context) string {
	read := d.managerName
	if read == nil {
		read = readManagerName
	}
	out, err := read(ctx)
	if err != nil {
		return contextUnknown
	}
	return cleanContextName(out)
}

// guardServerStart is called before the create that may start the server. It
// returns nil when the create may proceed.
//
// The cheap check is first and decides the common case: a Background service
// proceeds without a single extra multiplexer call. Only a service that is not
// Background asks whether a server already exists, because a create into an
// existing server starts nothing and so is not what this guards.
func (d *Driver) guardServerStart(ctx context.Context) error {
	if !d.contextAware() || d.allowDesktopServer {
		return nil
	}
	own := d.ownContext(ctx)
	if own == contextBackground {
		return nil
	}
	running, err := d.serverRunning(ctx)
	if err != nil {
		// Whether a server exists is unknown, and the context is not
		// Background: starting one blind is exactly the hazard.
		return d.unsafeContextError(own, "whether a server is already running could not be determined")
	}
	if running {
		return nil
	}
	return d.unsafeContextError(own, "no server is running, so this create would start one")
}

func (d *Driver) unsafeContextError(own, why string) error {
	shown := own
	if shown == contextUnknown {
		shown = "a context that could not be read"
	}
	return &fleet.Error{
		Kind:      fleet.ErrorUnsupported,
		Reason:    fleet.ReasonUnsafeServerContext,
		Retryable: false,
		Message: fmt.Sprintf(
			"create: refusing to start the session server from %s: %s, and every session it ever "+
				"runs would inherit this service's security session (an agent runtime that keeps its "+
				"login in the login Keychain loses it when reached from the Background context). "+
				"Run the service in the user's Background launchd domain (load it into user/<uid> with "+
				"LimitLoadToSessionType=Background), or start the server from such a context first; "+
				"an operator who accepts the risk sets FLEET_TMUX_ALLOW_DESKTOP_SERVER=1", shown, why),
		Machine: d.machine,
	}
}

// serverRunning asks the multiplexer whether a server answers. An error that
// is the multiplexer saying "no server" is a clean false; any other failure is
// an error, because nothing is known.
func (d *Driver) serverRunning(ctx context.Context) (bool, error) {
	_, err := d.run(ctx, d.bin, "list-sessions", "-F", "#{session_id}")
	if err == nil {
		return true, nil
	}
	if noServerRunning(err) {
		return false, nil
	}
	return false, err
}

// serverContextCache holds the last measurement for Capabilities.
type serverContextCache struct {
	mu   sync.Mutex
	at   time.Time
	have bool
	val  *fleet.ServerContext

	// muster #389: a server's context never changes for its lifetime, so it is
	// asked of a server once. knownKey names the server run that knownCtx was
	// read from (its pid and start time); a different key is a different
	// server and is measured afresh. Both are guarded by mu.
	knownKey string
	knownCtx string

	// probing is set from the moment a probe's multiplexer client is started
	// until that client returns. A probe never kills its client (see
	// probeServerContext), so a client held by a wedged server can outlive the
	// probe that started it; this keeps that to one.
	probing bool
	probeMu sync.Mutex
}

func (d *Driver) forgetServerContext() {
	d.sctxCache.mu.Lock()
	d.sctxCache.have = false
	d.sctxCache.mu.Unlock()
}

// reportServerContext is the Capabilities half. nil off macOS. It never fails:
// an unreadable fact is reported as "unknown", which is itself the finding.
func (d *Driver) reportServerContext() *fleet.ServerContext {
	if !d.contextAware() {
		return nil
	}
	c := &d.sctxCache
	c.mu.Lock()
	defer c.mu.Unlock()
	now := d.now()
	if c.have && now.Sub(c.at) < serverContextTTL {
		return c.val
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.serverProbeTimeout())
	defer cancel()
	out := &fleet.ServerContext{Service: d.ownContext(ctx)}
	if running, err := d.serverRunning(ctx); err == nil && running {
		out.Server = d.measureServer(ctx)
	}
	out.Warning = contextWarning(out)
	c.at, c.have, c.val = now, true, out
	return out
}

func (d *Driver) serverProbeTimeout() time.Duration {
	if d.probeTimeout > 0 {
		return d.probeTimeout
	}
	return serverContextProbeTimeout
}

// measureServer reads the running server's pid and context. Called with
// sctxCache.mu held.
//
// The context is asked of the server itself: a process the server starts
// inherits the server's security session, so `launchctl managername` run
// there answers for it. The answer is kept for as long as the same server run
// is up (muster #389), so the question is put to a server once, not on every
// health window.
func (d *Driver) measureServer(ctx context.Context) *fleet.ServerProcessContext {
	sp := &fleet.ServerProcessContext{Context: contextUnknown}
	c := &d.sctxCache
	key := ""
	if out, err := d.run(ctx, d.bin, "display-message", "-p", "#{pid}:#{start_time}"); err == nil {
		sp.PID, key = serverIdentity(string(out))
	}
	if key != "" && key == c.knownKey && c.knownCtx != "" {
		sp.Context = c.knownCtx
		return sp
	}
	sp.Context = d.probeServerContext(ctx)
	// Only an answer is kept. An unknown is a probe that did not finish (a busy
	// server, a missing launchctl) and is worth asking again at the next window.
	if key != "" && sp.Context != contextUnknown {
		c.knownKey, c.knownCtx = key, sp.Context
	}
	return sp
}

// serverIdentity splits what `display-message -p '#{pid}:#{start_time}'`
// prints into the server's pid and a key naming this run of it. A pid alone
// can be handed to a later server; the start time tells the two apart. A
// multiplexer that prints no start time leaves the pid as the whole key, and a
// pid that does not parse leaves no key at all — nothing is then cached.
func serverIdentity(raw string) (pid int, key string) {
	s := strings.TrimSpace(raw)
	idText, start, _ := strings.Cut(s, ":")
	n, err := strconv.Atoi(idText)
	if err != nil || n <= 0 {
		return 0, ""
	}
	if start = strings.TrimSpace(start); start != "" {
		return n, idText + ":" + start
	}
	return n, idText
}

// probeServerContext asks the running server what its security-session
// context is, and returns "unknown" when it cannot be told in time.
//
// It sends exactly one thing to the server that runs a job: `run-shell -b`
// whose job prints nothing and always exits 0, so the server has nothing to
// write back to the client that asked. The answer travels through a file the
// job renames into place, and this side polls for it.
//
// Why that shape (muster #389): a foreground `run-shell` makes the server
// print the job's output to its command client when the job ends. This
// probe's own timeout used to kill that client while the job was still
// running, and the server then wrote to a client that was gone and died with
// SIGSEGV — every session it hosted with it, five times in three days on one
// busy machine. Backgrounded, silent and exit-0, there is nothing to print and
// nothing to report, whatever happens to the client; and the client is not
// killed on our timeout either (context.WithoutCancel), so even a server that
// is slow to take the command is never answered by a client that left.
func (d *Driver) probeServerContext(ctx context.Context) string {
	c := &d.sctxCache
	c.probeMu.Lock()
	if c.probing {
		c.probeMu.Unlock()
		return contextUnknown
	}
	c.probing = true
	c.probeMu.Unlock()
	release := func() {
		c.probeMu.Lock()
		c.probing = false
		c.probeMu.Unlock()
	}

	dir, err := os.MkdirTemp(safeProbeBase(), "muster-ctx-")
	if err != nil {
		release()
		return contextUnknown
	}
	// The job may still be running when this returns; its rename then fails
	// quietly into /dev/null, which is why the script tolerates it.
	defer os.RemoveAll(dir)
	answer := filepath.Join(dir, "answer")
	script := serverProbeScript(answer, d.probeJobPrefix)
	if script == "" {
		release()
		return contextUnknown
	}

	clientDone := make(chan error, 1)
	go func() {
		defer release()
		_, err := d.run(context.WithoutCancel(ctx), d.bin, "run-shell", "-b", script)
		clientDone <- err
	}()

	tick := time.NewTicker(serverProbePoll)
	defer tick.Stop()
	for {
		if b, err := os.ReadFile(answer); err == nil {
			return cleanContextName(string(b))
		}
		select {
		case <-ctx.Done():
			return contextUnknown
		case err := <-clientDone:
			if err != nil {
				return contextUnknown
			}
			clientDone = nil // the job is queued; keep waiting for its answer
		case <-tick.C:
		}
	}
}

// probeSafePath is what a path embedded in the job may consist of. The job
// runs under whatever the user's configured shell is, so nothing that any of
// them would read specially (quotes, `#` and `$` — the multiplexer expands
// `#` before the shell sees it) is allowed in.
var probeSafePath = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)

// safeProbeBase is the directory the answer file lives under: the temp
// directory, unless its path holds a character the job cannot carry.
func safeProbeBase() string {
	if t := os.TempDir(); probeSafePath.MatchString(t) {
		return t
	}
	return "/tmp"
}

// serverProbeScript is the job body for `run-shell -b`: it writes
// `launchctl managername` (or the reason it could not) to <answer>.part, then
// renames it to <answer>, so a reader sees the answer whole or not at all. The
// outer redirection and `exit 0` are what keep the job silent and successful
// under any shell. "" when answer cannot be carried safely.
func serverProbeScript(answer, prefix string) string {
	if !probeSafePath.MatchString(answer) {
		return ""
	}
	part := answer + ".part"
	inner := prefix + "/bin/launchctl managername >" + part + " 2>&1; /bin/mv -f " + part + " " + answer
	return "/bin/sh -c '" + inner + "' >/dev/null 2>&1; exit 0"
}

// withProbeJob makes the probe slow or short, for tests: prefix runs inside
// the job before it asks, and timeout replaces serverContextProbeTimeout.
func withProbeJob(prefix string, timeout time.Duration) Option {
	return func(d *Driver) { d.probeJobPrefix, d.probeTimeout = prefix, timeout }
}

func contextWarning(c *fleet.ServerContext) string {
	var parts []string
	if c.Server != nil && c.Server.Context != contextBackground {
		parts = append(parts, fmt.Sprintf(
			"the session server is running in the %q context, so every session in it inherits that context "+
				"(an agent runtime keeping its login in the login Keychain loses it from Background); "+
				"this service attaches to an existing server and cannot change it — stop the server and let a "+
				"Background-context service start the next one", c.Server.Context))
	}
	if c.Service != contextBackground {
		parts = append(parts, fmt.Sprintf(
			"this service runs in the %q context, not Background; it refuses to start a session server from there",
			c.Service))
	}
	return strings.Join(parts, "; ")
}
