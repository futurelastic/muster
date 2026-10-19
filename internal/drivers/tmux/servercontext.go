package tmux

import (
	"context"
	"fmt"
	"os/exec"
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

// serverContextProbeTimeout bounds the two multiplexer calls of one
// measurement, so a wedged server cannot hold a health request.
const serverContextProbeTimeout = 3 * time.Second

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
	ctx, cancel := context.WithTimeout(context.Background(), serverContextProbeTimeout)
	defer cancel()
	out := &fleet.ServerContext{Service: d.ownContext(ctx)}
	if running, err := d.serverRunning(ctx); err == nil && running {
		out.Server = d.measureServer(ctx)
	}
	out.Warning = contextWarning(out)
	c.at, c.have, c.val = now, true, out
	return out
}

// measureServer reads the running server's pid and context. The context is
// asked of the server itself: a shell the server runs inherits the server's
// security session, so `launchctl managername` run there answers for it.
func (d *Driver) measureServer(ctx context.Context) *fleet.ServerProcessContext {
	sp := &fleet.ServerProcessContext{Context: contextUnknown}
	if out, err := d.run(ctx, d.bin, "display-message", "-p", "#{pid}"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && n > 0 {
			sp.PID = n
		}
	}
	if out, err := d.run(ctx, d.bin, "run-shell", "launchctl managername"); err == nil {
		sp.Context = cleanContextName(string(out))
	}
	return sp
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
