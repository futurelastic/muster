package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
	"github.com/futurelastic/muster/internal/procgroup"
	"github.com/futurelastic/muster/internal/sandbox"
)

// defaultProbeTimeout bounds `opencode --version`. It is the bound on a
// STARTUP step, so it is short; a runtime that misses it is not dropped (muster
// #297) — the service tries again, with a longer bound, in the background.
const defaultProbeTimeout = 5 * time.Second

// Availability is what a Probe-style check reports about the local
// opencode install, established WITHOUT starting a server and WITHOUT
// committing to any working directory — muster issue #55's third
// deliverable: "absent install is a first-class answer, not a startup
// crash."
type Availability struct {
	// Installed is true when the binary was found and answered --version.
	Installed bool
	// Path is the resolved executable, set whenever LookPath succeeded —
	// even if the version check that follows it then failed, so a caller
	// diagnosing a broken install still learns which binary was tried.
	Path string
	// Version is `opencode --version`'s trimmed output, best effort.
	Version string
	// Cause classifies Err (muster #297). Empty when Installed is true.
	Cause fleet.UnavailableCause
	// Err explains why Installed is false. Nil when Installed is true.
	Err error
}

// Locate resolves bin (empty means the default, "opencode" on PATH) WITHOUT
// running it, and classifies a failure: not found is "not installed", found
// but not runnable is "not executable". It is the half of Probe that needs no
// process, so a caller that must not execute anything — the doctor — asks the
// same question Probe's first step asks and cannot disagree with it.
func Locate(bin string) (path string, cause fleet.UnavailableCause, err error) {
	if bin == "" {
		bin = defaultBin
	}
	path, err = exec.LookPath(bin)
	if err == nil {
		return path, "", nil
	}
	return "", classifyStartError(err, fleet.CauseProbeFailed), fmt.Errorf("opencode: %q not found on PATH: %w", bin, err)
}

// classifyStartError reads why a binary could not be found or started. Not
// found is "not installed"; present but refused by the operating system —
// permissions, a file that is not an executable — is "not executable".
// fallback is returned for anything else.
func classifyStartError(err error, fallback fleet.UnavailableCause) fleet.UnavailableCause {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return fleet.CauseNotInstalled
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOEXEC):
		return fleet.CauseNotExecutable
	}
	return fallback
}

// Probe reports whether opencode can be found and run on this machine.
// bin overrides the default lookup ("opencode" on PATH); empty uses the
// default. It never starts a server and never touches a working
// directory — the two things #55 flagged as not this check's business.
func Probe(ctx context.Context, bin string) Availability {
	return probe(ctx, bin, defaultProbeTimeout)
}

// probe is Probe with the deadline on `--version` made explicit.
func probe(ctx context.Context, bin string, within time.Duration) Availability {
	path, cause, err := Locate(bin)
	if err != nil {
		return Availability{Cause: cause, Err: err}
	}
	cctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output()
	if err != nil {
		cause := classifyStartError(err, fleet.CauseProbeFailed)
		// Our own deadline fired while the caller's context was still live:
		// the binary did not answer in time, which is a fact about load and
		// not about the install. Never mistake it for a broken binary.
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			cause = fleet.CauseProbeTimeout
		}
		return Availability{Path: path, Cause: cause, Err: fmt.Errorf("opencode: %q --version failed: %w", path, err)}
	}
	return Availability{Installed: true, Path: path, Version: strings.TrimSpace(string(out))}
}

// process owns one spawned opencode server subprocess and its process group.
type process struct {
	cmd *exec.Cmd
	// group is what stops the server's whole tree (internal/procgroup): a
	// process group on Unix, a kill-on-close Job Object on Windows. Nil only in
	// a test that builds a process by hand; stop then falls back to killing the
	// server itself.
	group *procgroup.Group
	// done is closed when the single Wait on cmd returns, so "has it exited"
	// is a channel read anywhere (readiness, State, stop) and Wait is called
	// exactly once.
	done chan struct{}
}

// exited reports whether the child has already been reaped.
func (p *process) exited() bool {
	if p == nil {
		return true
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// server is one running opencode server this driver talks to: its address and
// credential, the process behind it, and the session-scoped directory it owns.
//
// In the production (isolated) mode there is one per session, so its
// credential and environment are that session's alone. In shared mode — the
// test seam WithBaseURL — there is exactly one and proc/dir are nil.
type server struct {
	baseURL  string
	username string
	// password is this server's own credential. Generated per process, held
	// only in memory, and handed to the child through its environment — never
	// argv, never a file this driver writes (the provider ruling on #55).
	// NEVER logged.
	password string
	proc     *process
	// sandbox is the profile this server's process runs under, nil when it is
	// not sandboxed (muster #281). Reported on every state read.
	sandbox *fleet.SandboxState
	// cwdGiven is the working directory exactly as this server's process was
	// started in it, and cwdPlanned the real path its sandbox plan granted for it
	// (muster #474). Both are empty when there is nothing to compare: the session
	// is not sandboxed, or the host's mechanism holds its grant by the directory
	// itself rather than by its path (sandbox.go, noteWorkdir). Written once,
	// before the server is tracked, and read-only after.
	cwdGiven, cwdPlanned string
	// permission is the posture this server's configuration was written with:
	// "bypass" for a pre-approved session (muster #283), empty when none was
	// configured — which is not a claim about what the runtime defaults to.
	permission fleet.PermissionModeState
	// dir is the session-scoped directory (HOME, TMPDIR, XDG_*), removed with
	// the server. Empty in shared mode.
	dir string
	// credential is which of the provider's two accounts this server's session was
	// put on at its start (muster #458); empty when the service chose none. Written
	// once, before the server is tracked, and read-only after.
	credential fleet.CredentialKind
	// logs is the tail of the process's own log (muster #284); nil in shared
	// mode, where the server is not this driver's to read. Every method on it
	// tolerates nil.
	logs *logCapture
	// gone is set once the loss of this server's session has been announced on
	// the event plane, so a deliberate close and the bus connection ending
	// right after it do not both report. Isolated mode only: in shared mode a
	// session's end does not end the connection.
	gone atomic.Bool
	// persisted is set once the session on this server has a record on disk
	// (muster #282). A server that is merely STOPPED with the service keeps its
	// directory — the session's database is what a restart finds again — while a
	// server whose session was closed, or never came to exist, removes it.
	persisted atomic.Bool
	// status keeps what this driver read of the server between requests, so a
	// listing does not ask it again (muster #419; statuscache.go). Zero value ready.
	status statusCache
	// scratch is the last measurement of the session's tmp/ directory (muster #419;
	// scratch.go). Zero value ready.
	scratch scratchMemo

	// park is non-nil once the driver stopped this server on purpose because its
	// session had nothing to do (muster #421; park.go). It is stored BEFORE the
	// process is stopped, so whoever sees the process gone also sees why. A
	// stopped server never runs again — a revive builds a new one — so the flag
	// stays for this server's life, and a failed revive replaces the note, never
	// edits it.
	park atomic.Pointer[parkNote]
	// lastActive is when this server last did something for its session, in unix
	// nanoseconds: it started, this driver wrote to it, or its event stream said the
	// session changed (park.go).
	lastActive atomic.Int64
	// watch is set when a park check found the session busy: it is then checked on
	// every sweep, whatever lastActive says, until it is first seen idle, and that
	// moment is when its quiet time starts. It is how a long turn nobody was
	// subscribed to is still waited out for a full bound afterwards.
	watch atomic.Bool

	// strayStopped and strayLeft are what the teardown found of the processes the
	// session started outside the server's process group (muster #405): stopped, and
	// still running after the wait. Written inside once, read by onTeardown.
	strayStopped, strayLeft []strayProc
	// straySweep is whether the teardown looks for them: set by the driver that
	// tracks the server, so a server nobody tracks looks for none.
	straySweep bool

	once       sync.Once
	onTeardown func(*server)
}

// teardown stops the server's process group and removes its session-scoped
// directory. Safe to call any number of times, from any goroutine.
func (s *server) teardown() { s.stop(false) }

// pauseForShutdown stops the server the way the service stopping does: the
// process group goes, but a persisted session keeps its directory, because the
// session has not been closed and the next start will look for it there.
func (s *server) pauseForShutdown() { s.stop(true) }

func (s *server) stop(keepPersisted bool) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.proc.stop()
		// The group stop does not reach a process that left the group (muster #405);
		// the scoped directory still names one, so this runs before it is removed.
		if s.straySweep {
			s.strayStopped, s.strayLeft = stopStraysIn(s.dir)
		}
		if s.dir != "" && !(keepPersisted && s.persisted.Load()) {
			_ = privfs.RemoveAll(s.dir)
		}
		if s.onTeardown != nil {
			s.onTeardown(s)
		}
	})
}

// freePort asks the OS for an unused TCP port on 127.0.0.1 and returns it
// immediately available for reuse.
//
// This is how "we choose the port" (#55) is actually true rather than
// aspirational: opencode's --port flag takes a number we supply, so there
// is nothing to discover afterwards — no startup-banner parsing, no port
// file, no environment variable the child publishes back to us. The
// alternative, --port 0 and reading the child's own log line for what it
// bound to, is exactly the discovery mechanism the issue found this
// runtime does not need.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("opencode: choosing a port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// generateCredential returns a fresh random Basic-auth password. Held only
// in memory by the caller — see Driver.password's doc comment for the full
// chain of custody the provider ruling on #55 requires.
func generateCredential() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("opencode: generating a credential: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// runtimeLogLevel is the most the runtime may write to its log (muster #420).
//
// The runtime's own permission check logs "evaluated permission=bash
// pattern=<the whole command>" at INFO, to stderr and to the log file in the
// session's directory, so a secret a client passed on a command line is kept in
// plaintext. The runtime has no redaction hook; the one control it offers is
// this level, and it applies to the file as well as to stderr (measured on
// v1.18.3: at WARN neither carries the line, whether the command was allowed,
// denied or asked). WARN rather than ERROR because everything this driver reads
// is ERROR, and WARN is the quietest level that still shows an operator the
// runtime's own warnings; no WARN line carried command text in those runs.
const runtimeLogLevel = "WARN"

// buildServeCmd builds the exec.Cmd for `opencode serve`, and is the one
// place that decides its argv and environment — factored out of startServer so
// process_test.go can assert its shape directly (credential via env only,
// never argv; --mdns never present; --print-logs always present; the log
// capped at runtimeLogLevel) against the real code path rather than a
// duplicate of it.
//
// env is the BUILT environment (env.go): the child gets exactly that plus its
// own server credential, and nothing is inherited from this process. A nil env
// is therefore an empty environment, not "inherit".
func buildServeCmd(bin, workdir string, port int, username, password string, env []string, wrap []string, extraArgs ...string) *exec.Cmd {
	serve := []string{"serve",
		"--port", strconv.Itoa(port),
		"--hostname", "127.0.0.1",
		// muster #288: without this the runtime logs nothing to stderr, and
		// the bounded capture (logcapture.go) has no error name to read.
		// One slice feeds both the plain and the sandboxed command.
		"--print-logs",
		// muster #420: at its default level the runtime logs every permission it
		// evaluates, the FULL bash command included, into its per-session log. A
		// command that carries a credential (an auth header, a token in a URL)
		// then sits in plaintext for as long as the session directory lives.
		// The cap keeps ERROR (what #288 reads) and drops those lines.
		"--log-level", runtimeLogLevel,
		// Deliberately no --mdns: it defaults the bind to 0.0.0.0
		// (measured on #55), which nothing here wants — this server is
		// reached only by this driver, over loopback.
	}
	// muster #283: arguments a session's create asked for (a bypass session's
	// --pure). Never anything secret; the credential stays in the environment.
	serve = append(serve, extraArgs...)
	var cmd *exec.Cmd
	if len(wrap) > 0 {
		// muster #281: the runtime runs INSIDE the profile, so every tool it
		// starts inherits it. wrap is the facility's command and the profile (or
		// policy) it is given: paths and service names only — nothing secret — so it
		// travels as an argument; the credential still travels through the
		// environment alone.
		cmd = exec.Command(wrap[0], append(append([]string(nil), wrap[1:]...), append([]string{bin}, serve...)...)...)
	} else {
		cmd = exec.Command(bin, serve...)
	}
	if workdir != "" {
		cmd.Dir = workdir
	}
	// The credential travels through the ENVIRONMENT ONLY (Boss's provider
	// ruling on #55) — never as a command-line argument, which a process
	// table on the same machine can read, and never written to any file
	// this driver controls.
	cmd.Env = append(append([]string(nil), env...),
		"OPENCODE_SERVER_PASSWORD="+password,
		"OPENCODE_SERVER_USERNAME="+username,
	)
	// Its own process group (a Job Object on Windows), so stop can take down
	// everything the session's tools spawned and not just the server (muster
	// #280).
	procgroup.Prepare(cmd)
	// Stdout is discarded: this driver's diagnostics come from the HTTP layer
	// it talks to the server over. Stderr is nil here too and is replaced by
	// startServer with a bounded in-memory capture (logcapture.go, muster #284)
	// that exposes an error NAME and never the text, so the old reasoning —
	// one more place the credential could be echoed back and retained — still
	// holds: nothing a caller can read comes out of it.
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd
}

// startAttempts bounds how often startServer retries after the child dies
// before becoming ready — the signature of losing the freePort race to
// another process that bound the port in between.
const startAttempts = 3

// startServer execs `opencode serve` for one session and waits for it to
// answer before returning. A failure at any step returns an error rather than
// panicking or calling log.Fatal — this package has no opinion on whether the
// absence of opencode should be fatal to its caller.
//
// freePort-then-exec has a gap in which another process can take the port. It
// is closed by checking, not by hoping: readiness counts only a 200 (a 401
// would mean a DIFFERENT server holds the port, with a different credential)
// and a child that exits before answering is retried on a fresh port.
func startServer(ctx context.Context, bin, workdir, username string, env []string, launch launchFunc, extraArgs ...string) (*server, error) {
	var lastErr error
	for attempt := 0; attempt < startAttempts; attempt++ {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		cred, err := generateCredential()
		if err != nil {
			return nil, err
		}
		// The prefix is asked for per attempt: on Linux the one port a confined
		// session may bind is this attempt's.
		var wrap []string
		if launch != nil {
			wrap, err = launch(port)
			if err != nil {
				return nil, fmt.Errorf("opencode: preparing the sandbox: %w", err)
			}
		}
		cmd := buildServeCmd(bin, workdir, port, username, cred, env, wrap, extraArgs...)
		logs := newLogCapture()
		cmd.Stderr = logs
		// A tool the session ran can hold the stderr pipe open after the server
		// itself is gone; without a bound, Wait would then not return and the
		// process would read as still running.
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("opencode: starting server: %w", err)
		}
		// Attach before anything else can fail or wait: where the platform
		// creates the child suspended it only runs once it is under control, and
		// a failed Attach has already ended and reaped it.
		group, err := procgroup.Attach(cmd)
		if err != nil {
			return nil, fmt.Errorf("opencode: starting server: %w", err)
		}
		p := &process{cmd: cmd, group: group, done: make(chan struct{})}
		go func() {
			_ = cmd.Wait()
			close(p.done)
		}()

		s := &server{
			baseURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
			username: username,
			password: cred,
			proc:     p,
			logs:     logs,
		}
		err = waitReady(ctx, s.baseURL, username, cred, p.done)
		if err == nil {
			return s, nil
		}
		p.stop()
		if errors.Is(err, errExitedBeforeReady) {
			// muster #456: the launcher declining to start the runtime is not a
			// lost port race. Starting again on another port would meet the same
			// refusal, three times over, and end by hiding its reason behind "did
			// not become ready"; it is returned as it is, once.
			if len(wrap) > 0 {
				if why := launcherRefusal(p, logs); why != "" {
					return nil, fmt.Errorf("opencode: %w: %s", errLauncherRefused, why)
				}
			}
			lastErr = err
			if name := logs.errorName(time.Time{}); name != "" {
				// The runtime's own name for why it died (muster #284), not its log.
				lastErr = fmt.Errorf("%w (the runtime logged %s)", err, name)
			}
			continue
		}
		return nil, fmt.Errorf("opencode: server did not become ready: %w", err)
	}
	return nil, fmt.Errorf("opencode: server did not become ready after %d attempts: %w", startAttempts, lastErr)
}

// errExitedBeforeReady is waitReady's answer when the child died before it
// ever answered.
var errExitedBeforeReady = errors.New("the server process exited before it answered")

// errLauncherRefused is what startServer returns when the sandbox launcher did not
// start the runtime (muster #456), wrapped with the launcher's own reason. Never
// retried: unlike a lost port race, the same start refuses the same way.
var errLauncherRefused = errors.New("the sandbox launcher did not start the runtime")

// launcherRefusal returns the launcher's reason when p is a launcher that has
// exited without running the runtime, and "" otherwise. Two things must both be
// true: the exit status is one the launcher gives (sandbox.LauncherRefusedStatus,
// or LauncherExecFailedStatus when the exec of the runtime failed), and its
// standard error carries the launcher's own line. Either alone is not enough: a
// runtime may exit 125 or 127 for reasons of its own, and a line that looks like
// the launcher's is only the launcher's if the process that wrote it is what
// ended. p must have exited.
func launcherRefusal(p *process, logs *logCapture) string {
	if p == nil || p.cmd == nil || p.cmd.ProcessState == nil {
		return ""
	}
	switch p.cmd.ProcessState.ExitCode() {
	case sandbox.LauncherRefusedStatus, sandbox.LauncherExecFailedStatus:
		return logs.launcherRefusal()
	}
	return ""
}

// waitReady polls the server's own session listing (authenticated) until it
// answers 200, the child exits, or ctx / the readiness budget runs out.
func waitReady(ctx context.Context, baseURL, username, password string, exited <-chan struct{}) error {
	cctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	for {
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, baseURL+"/session", nil)
		if err == nil {
			req.SetBasicAuth(username, password)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				// Only a 200 is "ready": the right credential was accepted by
				// the server this driver started. A 401 means another process
				// holds the port (the freePort race), a 5xx means not yet.
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-cctx.Done():
			return cctx.Err()
		case <-exited:
			return errExitedBeforeReady
		case <-ticker.C:
		}
	}
}

// stop terminates the child and everything in its process group. Idempotent.
//
// Where the platform has a graceful stop (SIGTERM to the group), that goes
// first and is given a grace period; then the whole group is killed ALWAYS —
// also when the leader exited on its own, because a tool the session ran can
// outlive the server it ran under (muster #280: a `sleep` re-parented to init
// after a timeout was measured on a first one-shot wrapper). A group that is
// already empty is not an error. Where there is no graceful stop (Windows) it
// goes straight to the kill: the job is terminated and waited until empty.
func (p *process) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	g := p.group
	if g == nil {
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
		return
	}
	if g.Graceful() {
		_ = g.Interrupt()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
		}
	}
	_ = g.Kill()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
	_ = g.Close()
}
