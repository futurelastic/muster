package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
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
type ctxMux struct {
	*fakeMux
	mu           sync.Mutex
	serverUp     bool
	listErr      error // overrides the "no server" answer when serverUp is false
	serverCtx    string
	serverPID    string
	newSessions  int
	listSessions int
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
		c := m.serverCtx
		m.mu.Unlock()
		return []byte(c + "\n"), nil
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

// Live (FLEET_TMUX_INTEGRATION=1, macOS only), against a PRIVATE server: the
// context the driver reports for a running server is the one `launchctl
// managername` answers when the server runs it — measured through the real
// multiplexer, not a fake. This is what makes `run-shell`'s stdout a trustworthy
// carrier for the answer.
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
