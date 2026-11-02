package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Every other test in this package must not depend on the security-session
// context the run itself happens to be in (muster #336): on a developer's Mac
// that is usually Aqua.
func init() {
	readManagerName = func(context.Context) (string, error) { return "Background\n", nil }
}

// ctxMux wraps the package's fake multiplexer with the three calls the #336
// code makes: whether a server answers, its pid, and what `launchctl
// managername` says when the server runs it.
//
// The server's answer travels the way a real server's does since #389: the
// probe's `run-shell -b` job writes it to a file. The fake plays the job, so a
// test can make the job slow (jobWrites false: it never finishes) or the
// client slow (clientGate: the client call blocks until the channel closes).
type ctxMux struct {
	*fakeMux
	mu           sync.Mutex
	serverUp     bool
	listErr      error // overrides the "no server" answer when serverUp is false
	serverCtx    string
	serverPID    string
	newSessions  int
	listSessions int

	neverAnswers bool              // the job never writes its answer
	clientGate   chan struct{}     // when set, a run-shell client call blocks until it is closed
	runShells    [][]string        // the args of every run-shell sent
	runShellCtxs []context.Context // the context each run-shell client ran under
}

// probeAnswerRE finds where the probe's job was told to leave its answer.
var probeAnswerRE = regexp.MustCompile(`/bin/mv -f \S+ (\S+)'`)

// playProbeJob does what the server-context probe's `run-shell -b` job does
// inside a real server: write `answer` where the script says, whole or not at
// all. It returns false when script is not the probe's job.
func playProbeJob(script, answer string) bool {
	mt := probeAnswerRE.FindStringSubmatch(script)
	if mt == nil {
		return false
	}
	part := mt[1] + ".part"
	if err := os.WriteFile(part, []byte(answer+"\n"), 0o600); err != nil {
		return true // the directory is gone: the job fails quietly, as a real one does
	}
	_ = os.Rename(part, mt[1])
	return true
}

func (m *ctxMux) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	switch args[0] {
	case "list-sessions":
		m.listSessions++
		up, lerr := m.serverUp, m.listErr
		m.mu.Unlock()
		if up {
			return []byte("$1\n"), nil
		}
		if lerr != nil {
			return nil, lerr
		}
		return nil, &exec.ExitError{Stderr: []byte("no server running on /tmp/tmux-501/default\n")}
	case "display-message":
		pid := m.serverPID
		m.mu.Unlock()
		return []byte(pid + "\n"), nil
	case "run-shell":
		c, never, gate := m.serverCtx, m.neverAnswers, m.clientGate
		m.runShells = append(m.runShells, append([]string(nil), args...))
		m.runShellCtxs = append(m.runShellCtxs, ctx)
		m.mu.Unlock()
		if gate != nil {
			<-gate
		}
		// The job: leave the answer where the script says to. A real server
		// never prints it back to the client, and neither does this one.
		if !never {
			playProbeJob(args[len(args)-1], c)
		}
		return nil, nil
	case "new-session":
		m.newSessions++
		m.serverUp = true
	}
	m.mu.Unlock()
	return m.fakeMux.exec(ctx, name, args...)
}

func ctxDriver(m *ctxMux, goos, managerName string, extra ...Option) *Driver {
	opts := []Option{
		withExec(m.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		withGOOS(goos),
		withManagerName(func(context.Context) (string, error) {
			if managerName == "" {
				return "", errors.New("launchctl: not found")
			}
			return managerName + "\n", nil
		}),
	}
	return New("testbox", append(opts, extra...)...)
}

func createOnce(d *Driver, key string) (fleet.Session, error) {
	return d.Create(context.Background(), testCaller, key, fleet.SessionSpec{Name: "fresh", Cwd: "/work/x"})
}

func wantUnsafeContext(t *testing.T, err error) {
	t.Helper()
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want a *fleet.Error, got %v", err)
	}
	if fe.Reason != "unsafe_server_context" {
		t.Errorf("reason = %q", fe.Reason)
	}
	if fe.Kind != fleet.ErrorUnsupported || fe.Retryable {
		t.Errorf("kind=%s retryable=%v; want unsupported, not retryable", fe.Kind, fe.Retryable)
	}
	if !strings.Contains(fe.Message, "FLEET_TMUX_ALLOW_DESKTOP_SERVER") {
		t.Errorf("message must name the override: %q", fe.Message)
	}
}

// muster #336, done-when 1: a launchctl that says Aqua, no server running —
// the first create is refused, no server is started, the error says why.
func TestCreateFromAquaWithNoServerIsRefusedAndStartsNothing(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions()}
	d := ctxDriver(m, "darwin", "Aqua")
	_, err := createOnce(d, "k1")
	wantUnsafeContext(t, err)
	if m.newSessions != 0 {
		t.Fatalf("new-session ran %d times; the server must not be started", m.newSessions)
	}
	// The refusal reserved nothing: the same key succeeds once the operator
	// fixes the context (here: the override), as a fresh create.
	d2 := ctxDriver(m, "darwin", "Aqua", WithAllowDesktopServer(true))
	if _, err := createOnce(d2, "k1"); err != nil {
		t.Fatalf("with the explicit override: %v", err)
	}
	if m.newSessions != 1 {
		t.Errorf("new-session ran %d times with the override, want 1", m.newSessions)
	}
}

// The other half of done-when 1: Background starts the server as today, and
// pays for no extra multiplexer call to find out.
func TestCreateFromBackgroundStartsTheServerAsToday(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions()}
	d := ctxDriver(m, "darwin", "Background")
	if _, err := createOnce(d, "k1"); err != nil {
		t.Fatalf("create from Background: %v", err)
	}
	if m.newSessions != 1 {
		t.Errorf("new-session ran %d times, want 1", m.newSessions)
	}
	// Compared with the same create on a host the guard does not apply to, so
	// the calls the create makes for its own reasons do not count.
	base := &ctxMux{fakeMux: twoSessions()}
	if _, err := createOnce(ctxDriver(base, "linux", "Background"), "k1"); err != nil {
		t.Fatal(err)
	}
	if m.listSessions != base.listSessions {
		t.Errorf("a Background service made %d list-sessions calls, %d without the guard", m.listSessions, base.listSessions)
	}
}

// A context that cannot be read is not Background; and "exactly Background"
// means a near miss does not pass either.
func TestCreateRefusesAContextItCannotReadOrDoesNotRecognise(t *testing.T) {
	for _, mn := range []string{"", "background", "LoginWindow", "Aqua extra", "System"} {
		t.Run("managername="+mn, func(t *testing.T) {
			m := &ctxMux{fakeMux: twoSessions()}
			d := ctxDriver(m, "darwin", mn)
			_, err := createOnce(d, "k1")
			wantUnsafeContext(t, err)
			if m.newSessions != 0 {
				t.Fatal("a server was started")
			}
		})
	}
}

// An Aqua service creating into a server that already exists starts nothing,
// so it is not what the guard refuses; the health report carries the warning.
func TestCreateFromAquaIntoARunningServerProceeds(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions(), serverUp: true}
	d := ctxDriver(m, "darwin", "Aqua")
	if _, err := createOnce(d, "k1"); err != nil {
		t.Fatalf("create into an existing server: %v", err)
	}
}

// Whether a server runs being unanswerable is not "no server" — starting one
// blind from a desktop context is the hazard.
func TestCreateFromAquaRefusesWhenServerStateCannotBeRead(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions(), listErr: &exec.ExitError{Stderr: []byte("error connecting to /tmp/x (Permission denied)\n")}}
	d := ctxDriver(m, "darwin", "Aqua")
	_, err := createOnce(d, "k1")
	wantUnsafeContext(t, err)
}

// Done-when 3: non-macOS hosts are unchanged, whatever launchctl would say.
func TestNonDarwinIsUnchanged(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions()}
	d := ctxDriver(m, "linux", "Aqua")
	if _, err := createOnce(d, "k1"); err != nil {
		t.Fatalf("create on linux: %v", err)
	}
	if got := d.Capabilities().ServerContext; got != nil {
		t.Errorf("a non-macOS host reports a server context: %+v", got)
	}
}

// muster #336, done-when 2: the health output carries both contexts, pinned
// on the wire shape.
func TestCapabilitiesReportServiceAndServerContext(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "Aqua", serverPID: "4242"}
	d := ctxDriver(m, "darwin", "Background")
	raw, err := json.Marshal(d.Capabilities())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ServerContext *struct {
			Service string `json:"service"`
			Server  *struct {
				PID     int    `json:"pid"`
				Context string `json:"context"`
			} `json:"server"`
			Warning string `json:"warning"`
		} `json:"serverContext"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	sc := wire.ServerContext
	if sc == nil || sc.Server == nil {
		t.Fatalf("serverContext missing from %s", raw)
	}
	if sc.Service != "Background" || sc.Server.Context != "Aqua" || sc.Server.PID != 4242 {
		t.Errorf("serverContext = %+v server=%+v", sc, sc.Server)
	}
	// A server already in a desktop context warns even though the service is fine.
	if !strings.Contains(sc.Warning, `"Aqua"`) {
		t.Errorf("warning = %q; a desktop-context server must be a visible warning", sc.Warning)
	}
}

func TestCapabilitiesHealthyAndNoServerCases(t *testing.T) {
	t.Run("both Background: no warning", func(t *testing.T) {
		m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "Background", serverPID: "7"}
		sc := ctxDriver(m, "darwin", "Background").Capabilities().ServerContext
		if sc == nil || sc.Warning != "" || sc.Server == nil || sc.Server.Context != "Background" {
			t.Errorf("serverContext = %+v", sc)
		}
	})
	t.Run("no server: absent server, service reported", func(t *testing.T) {
		m := &ctxMux{fakeMux: twoSessions()}
		sc := ctxDriver(m, "darwin", "Aqua").Capabilities().ServerContext
		if sc == nil || sc.Service != "Aqua" || sc.Server != nil {
			t.Errorf("serverContext = %+v", sc)
		}
		if sc.Warning == "" {
			t.Error("an Aqua service must carry a warning")
		}
	})
	t.Run("a server that does not say is unknown, not Background", func(t *testing.T) {
		m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "launchctl: command not found", serverPID: "7"}
		sc := ctxDriver(m, "darwin", "Background").Capabilities().ServerContext
		if sc == nil || sc.Server == nil || sc.Server.Context != "unknown" || sc.Warning == "" {
			t.Errorf("serverContext = %+v", sc)
		}
	})
}

// Capabilities is read by every health request; the measurement is cached and
// a create drops the cache so a freshly started server is not reported absent.
func TestServerContextMeasurementIsCachedUntilACreate(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions()}
	d := ctxDriver(m, "darwin", "Background")
	d.Capabilities()
	d.Capabilities()
	if m.listSessions != 1 {
		t.Errorf("list-sessions ran %d times for two reads, want 1", m.listSessions)
	}
	if _, err := createOnce(d, "k1"); err != nil {
		t.Fatal(err)
	}
	sc := d.Capabilities().ServerContext
	if sc == nil || sc.Server == nil {
		t.Errorf("after a create the server must be reported: %+v", sc)
	}
}

// shells returns the run-shell calls and the contexts their clients ran under.
func (m *ctxMux) shells() ([][]string, []context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]string(nil), m.runShells...), append([]context.Context(nil), m.runShellCtxs...)
}

// stepClock is a clock a test moves by hand, so the 30 s window can pass
// without the test waiting for it.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// muster #389: the only `run-shell` this service sends is backgrounded and
// silent, and its client is not one a timeout can kill. A foreground one made
// the server print the job's output to a client the probe's own timeout had
// killed, and the server died with every session in it.
func TestServerProbeSendsOnlyABackgroundSilentJobWhoseClientCannotBeKilled(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "Background", serverPID: "7"}
	sc := ctxDriver(m, "darwin", "Background").Capabilities().ServerContext
	if sc == nil || sc.Server == nil || sc.Server.Context != "Background" {
		t.Fatalf("the answer must still reach the health report: %+v", sc)
	}
	calls, ctxs := m.shells()
	if len(calls) != 1 {
		t.Fatalf("%d run-shell calls for one measurement, want 1: %q", len(calls), calls)
	}
	args := calls[0]
	if len(args) != 3 || args[0] != "run-shell" || args[1] != "-b" {
		t.Fatalf("run-shell must be backgrounded (-b) and carry only its job: %q", args)
	}
	job := args[2]
	if !strings.Contains(job, ">/dev/null 2>&1") || !strings.HasSuffix(job, "exit 0") {
		t.Errorf("the job must print nothing and always exit 0, or the server has something to say to its client: %q", job)
	}
	if ctxs[0].Done() != nil {
		t.Error("the run-shell client ran under a context that can cancel it: a timeout would kill it mid-job")
	}
}

// muster #389, done-when 3: a server's context is read once per server run,
// not every health window.
func TestServerContextIsAskedOfAServerOnce(t *testing.T) {
	clk := &stepClock{t: time.Unix(1785760000, 0)}
	m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "Background", serverPID: "7:100"}
	d := ctxDriver(m, "darwin", "Background", withClock(clk.now))
	asked := func() int { c, _ := m.shells(); return len(c) }
	read := func() string {
		sc := d.Capabilities().ServerContext
		if sc == nil || sc.Server == nil {
			t.Fatalf("no server context: %+v", sc)
		}
		return sc.Server.Context
	}

	if got := read(); got != "Background" || asked() != 1 {
		t.Fatalf("first read: context %q after %d asks", got, asked())
	}
	for i := 0; i < 4; i++ {
		clk.advance(serverContextTTL + time.Second)
		if got := read(); got != "Background" {
			t.Fatalf("window %d: context %q", i, got)
		}
	}
	if asked() != 1 {
		t.Errorf("the server was asked %d times across five health windows, want 1", asked())
	}

	// A new server run — a new start time under the same pid, or a new pid —
	// is a different server and is asked afresh.
	for i, id := range []string{"7:200", "9:200"} {
		m.mu.Lock()
		m.serverPID, m.serverCtx = id, "Aqua"
		m.mu.Unlock()
		clk.advance(serverContextTTL + time.Second)
		if got := read(); got != "Aqua" {
			t.Errorf("server %q: context %q, want Aqua", id, got)
		}
		if asked() != 2+i {
			t.Errorf("after server %q: %d asks, want %d", id, asked(), 2+i)
		}
	}
}

// A probe that got no answer is not remembered as one: the next window asks again.
func TestServerContextNoAnswerIsAskedAgainNextWindow(t *testing.T) {
	clk := &stepClock{t: time.Unix(1785760000, 0)}
	m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "Background", serverPID: "7", neverAnswers: true}
	d := ctxDriver(m, "darwin", "Background", withClock(clk.now), withProbeJob("", 150*time.Millisecond))
	if sc := d.Capabilities().ServerContext; sc == nil || sc.Server == nil || sc.Server.Context != "unknown" {
		t.Fatalf("a job that never answers must read unknown: %+v", sc)
	}
	m.mu.Lock()
	m.neverAnswers = false
	m.mu.Unlock()
	clk.advance(serverContextTTL + time.Second)
	if sc := d.Capabilities().ServerContext; sc == nil || sc.Server == nil || sc.Server.Context != "Background" {
		t.Fatalf("the second window must ask again and get its answer: %+v", sc)
	}
	if calls, _ := m.shells(); len(calls) != 2 {
		t.Errorf("%d asks, want 2", len(calls))
	}
}

// muster #389, done-when 1, against the fake: a job that outlives the probe's
// timeout leaves the probe unknown, its client uncancelled, and nothing behind.
func TestServerProbeWhoseJobOutlivesItsTimeoutLeavesNothingToCrashOn(t *testing.T) {
	m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "Background", serverPID: "7", neverAnswers: true}
	d := ctxDriver(m, "darwin", "Background", withProbeJob("", 150*time.Millisecond))
	start := time.Now()
	sc := d.Capabilities().ServerContext
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the health read took %v with a 150ms probe", took)
	}
	if sc == nil || sc.Server == nil || sc.Server.Context != "unknown" {
		t.Fatalf("serverContext = %+v", sc)
	}
	calls, ctxs := m.shells()
	if len(calls) != 1 || ctxs[0].Done() != nil {
		t.Fatalf("calls=%d; the client must run under a context that cannot be cancelled", len(calls))
	}
	// The private directory the job was to answer in is gone; a job that
	// finishes later finds nothing to write to and says nothing.
	mt := probeAnswerRE.FindStringSubmatch(calls[0][2])
	if mt == nil {
		t.Fatalf("no answer path in %q", calls[0][2])
	}
	if _, err := os.Stat(filepath.Dir(mt[1])); !os.IsNotExist(err) {
		t.Errorf("the probe left %s behind (%v)", filepath.Dir(mt[1]), err)
	}
}

// A server that is slow to take the command holds the client. The probe does
// not kill it (that is what crashed servers), and does not stack a second one
// on top of it either.
func TestServerProbeKeepsOneUnkilledClientWhileAServerIsSlow(t *testing.T) {
	clk := &stepClock{t: time.Unix(1785760000, 0)}
	gate := make(chan struct{})
	m := &ctxMux{fakeMux: twoSessions(), serverUp: true, serverCtx: "Background", serverPID: "7", clientGate: gate}
	d := ctxDriver(m, "darwin", "Background", withClock(clk.now), withProbeJob("", 150*time.Millisecond))
	read := func() string {
		sc := d.Capabilities().ServerContext
		if sc == nil || sc.Server == nil {
			t.Fatalf("no server context: %+v", sc)
		}
		return sc.Server.Context
	}
	if got := read(); got != "unknown" {
		t.Fatalf("a client the server has not released: %q, want unknown", got)
	}
	clk.advance(serverContextTTL + time.Second)
	if got := read(); got != "unknown" {
		t.Fatalf("second window: %q, want unknown", got)
	}
	if calls, ctxs := m.shells(); len(calls) != 1 || ctxs[0].Done() != nil {
		t.Fatalf("%d run-shell clients while the first was still held, want exactly 1, uncancelled", len(calls))
	}

	close(gate) // the server takes the command; the client returns
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.sctxCache.probeMu.Lock()
		busy := d.sctxCache.probing
		d.sctxCache.probeMu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never released after its client returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	clk.advance(serverContextTTL + time.Second)
	if got := read(); got != "Background" {
		t.Errorf("once the client is free the next window asks and answers: %q", got)
	}
}

// The job's paths come from the temp directory, and a job is run by whatever
// shell the user configured; a path any of them reads specially is not carried.
func TestServerProbeScriptRefusesAPathItCannotCarry(t *testing.T) {
	if got := serverProbeScript("/var/folders/ab/T/muster-ctx-1/answer", ""); got == "" {
		t.Error("an ordinary temp path must be carried")
	}
	for _, p := range []string{"/tmp/a b/answer", "/tmp/a'b/answer", "/tmp/a#b/answer", "/tmp/a$b/answer", "/tmp/a;b/answer", ""} {
		if got := serverProbeScript(p, ""); got != "" {
			t.Errorf("path %q produced a job: %q", p, got)
		}
	}
}

// Live (FLEET_TMUX_INTEGRATION=1, macOS only), against a PRIVATE server: the
// context the driver reports for a running server is the one `launchctl
// managername` answers when the server runs it — measured through the real
// multiplexer, not a fake: the job `run-shell -b` starts inside the server
// writes the answer to a file, and that file is a trustworthy carrier for it.
func TestLiveServerContextIsReadFromTheServer(t *testing.T) {
	m := newLiveMux(t)
	if _, err := exec.LookPath("launchctl"); err != nil {
		t.Skip("no launchctl: not macOS")
	}
	want, err := exec.Command("launchctl", "managername").Output()
	if err != nil {
		t.Skipf("launchctl managername: %v", err)
	}
	m.run("new-session", "-d", "-s", "ctxprobe", "sleep", "60")
	d := New("livebox", WithBinary(m.wrapper))
	sc := d.Capabilities().ServerContext
	if sc == nil || sc.Server == nil {
		t.Fatalf("no server context reported for a running server: %+v", sc)
	}
	if sc.Server.Context != cleanContextName(string(want)) {
		t.Errorf("server context = %q, launchctl says %q", sc.Server.Context, strings.TrimSpace(string(want)))
	}
	if sc.Server.PID <= 0 {
		t.Errorf("server pid = %d", sc.Server.PID)
	}
}

// muster #389, done-when 1, live (FLEET_TMUX_INTEGRATION=1) against a PRIVATE
// server: a probe whose job outlives its timeout does not take the server
// down. The job is held for a second under a 200ms timeout, so the probe has
// given up long before it ends — the case that used to kill the server when the
// job finished and the server wrote to the client the timeout had killed.
func TestLiveProbeWhoseJobOutlivesItsTimeoutDoesNotCrashTheServer(t *testing.T) {
	m := newLiveMux(t)
	m.run("new-session", "-d", "-s", "ctxprobe", "sleep", "60")
	pid := strings.TrimSpace(m.run("display-message", "-p", "#{pid}"))
	if pid == "" || strings.Contains(pid, "no server") {
		t.Fatalf("no server to probe: %q", pid)
	}
	d := New("livebox", WithBinary(m.wrapper), withGOOS("darwin"), withProbeJob("sleep 1; ", 200*time.Millisecond))
	sc := d.Capabilities().ServerContext
	if sc == nil || sc.Server == nil || sc.Server.Context != "unknown" {
		t.Fatalf("a probe cut off by its timeout must read unknown: %+v", sc)
	}
	time.Sleep(3 * time.Second) // the job ends, and the server may do whatever it does then
	if got := strings.TrimSpace(m.run("display-message", "-p", "#{pid}")); got != pid {
		t.Fatalf("the server died or was replaced: pid %q before, %q after", pid, got)
	}
	if got := m.run("list-sessions", "-F", "#{session_name}"); !strings.Contains(got, "ctxprobe") {
		t.Fatalf("the server lost its session: %q", got)
	}
}
