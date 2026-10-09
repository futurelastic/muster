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
	// dir is the session-scoped directory (HOME, TMPDIR, XDG_*), removed with
	// the server. Empty in shared mode.
	dir string

	once       sync.Once
	onTeardown func(*server)
}

// teardown stops the server's process group and removes its session-scoped
// directory. Safe to call any number of times, from any goroutine.
func (s *server) teardown() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.proc.stop()
		if s.dir != "" {
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
func buildServeCmd(bin, workdir string, port int, username, password string, env []string, profile string) *exec.Cmd {
	serve := []string{"serve",
		"--port", strconv.Itoa(port),
		"--hostname", "127.0.0.1",
		// Deliberately no --mdns: it defaults the bind to 0.0.0.0
		// (measured on #55), which nothing here wants — this server is
		// reached only by this driver, over loopback.
	}
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
	// Discarded rather than captured: this driver's own diagnostics come
	// from the HTTP layer it talks to the server over, and capturing
	// stdout/stderr here would be one more place the credential could
	// theoretically be echoed back and retained (opencode does not do
	// this, but the discipline costs nothing and removes the question).
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
func startServer(ctx context.Context, bin, workdir, username string, env []string, profile string) (*server, error) {
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
		cmd := buildServeCmd(bin, workdir, port, username, cred, env, profile)
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
		}
		err = waitReady(ctx, s.baseURL, username, cred, p.done)
		if err == nil {
			return s, nil
		}
		p.stop()
		if errors.Is(err, errExitedBeforeReady) {
			lastErr = err
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
