package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
)

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
	// Err explains why Installed is false. Nil when Installed is true.
	Err error
}

// Probe reports whether opencode can be found and run on this machine.
// bin overrides the default lookup ("opencode" on PATH); empty uses the
// default. It never starts a server and never touches a working
// directory — the two things #55 flagged as not this check's business.
func Probe(ctx context.Context, bin string) Availability {
	if bin == "" {
		bin = defaultBin
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return Availability{Err: fmt.Errorf("opencode: %q not found on PATH: %w", bin, err)}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output()
	if err != nil {
		return Availability{Path: path, Err: fmt.Errorf("opencode: %q --version failed: %w", path, err)}
	}
	return Availability{Installed: true, Path: path, Version: strings.TrimSpace(string(out))}
}

// process owns one spawned opencode server subprocess and its process group.
type process struct {
	cmd *exec.Cmd
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
	// permission is the posture this server's configuration was written with:
	// "bypass" for a pre-approved session (muster #283), empty when none was
	// configured — which is not a claim about what the runtime defaults to.
	permission fleet.PermissionModeState
	// dir is the session-scoped directory (HOME, TMPDIR, XDG_*), removed with
	// the server. Empty in shared mode.
	dir string
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
		if s.dir != "" && !(keepPersisted && s.persisted.Load()) {
			_ = os.RemoveAll(s.dir)
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

// buildServeCmd builds the exec.Cmd for `opencode serve`, and is the one
// place that decides its argv and environment — factored out of startServer so
// process_test.go can assert its shape directly (credential via env only,
// never argv; --mdns never present) against the real code path rather than a
// duplicate of it.
//
// env is the BUILT environment (env.go): the child gets exactly that plus its
// own server credential, and nothing is inherited from this process. A nil env
// is therefore an empty environment, not "inherit".
func buildServeCmd(bin, workdir string, port int, username, password string, env []string, profile string, extraArgs ...string) *exec.Cmd {
	serve := []string{"serve",
		"--port", strconv.Itoa(port),
		"--hostname", "127.0.0.1",
		// Deliberately no --mdns: it defaults the bind to 0.0.0.0
		// (measured on #55), which nothing here wants — this server is
		// reached only by this driver, over loopback.
	}
	// muster #283: arguments a session's create asked for (a bypass session's
	// --pure). Never anything secret; the credential stays in the environment.
	serve = append(serve, extraArgs...)
	var cmd *exec.Cmd
	if profile != "" {
		// muster #281: the runtime runs INSIDE the profile, so every tool it
		// starts inherits it. The profile text holds paths and service names
		// only — nothing secret — so it travels as an argument; the credential
		// still travels through the environment alone.
		cmd = exec.Command(sandboxExecPath, append([]string{"-p", profile, bin}, serve...)...)
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
	// Its own process group, so stop can take down everything the session's
	// tools spawned and not just the server (muster #280).
	configureGroup(cmd)
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
func startServer(ctx context.Context, bin, workdir, username string, env []string, profile string, extraArgs ...string) (*server, error) {
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
		cmd := buildServeCmd(bin, workdir, port, username, cred, env, profile, extraArgs...)
		logs := newLogCapture()
		cmd.Stderr = logs
		// A tool the session ran can hold the stderr pipe open after the server
		// itself is gone; without a bound, Wait would then not return and the
		// process would read as still running.
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("opencode: starting server: %w", err)
		}
		p := &process{cmd: cmd, done: make(chan struct{})}
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
// SIGTERM to the group first, a grace period, then SIGKILL to the group
// ALWAYS — also when the leader exited on its own, because a tool the session
// ran can outlive the server it ran under (muster #280: a `sleep` re-parented
// to init after a timeout was measured on a first one-shot wrapper). ESRCH from
// an already-empty group is ignored.
func (p *process) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	signalGroup(p.cmd, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
	}
	signalGroup(p.cmd, syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
}
