package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #280: one runtime process per session, with only the environment the
// create asked for. These tests start REAL child processes — the test binary
// re-executed as a stand-in opencode (fakeopencode_test.go) — so what they
// assert is what the operating system actually handed the child.

var testReq = fleet.RequestFrom(fleet.Caller{Principal: "isolation-test"})

func newIsolatedDriver(t *testing.T, extra ...Option) (*Driver, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	opts := append([]Option{WithBinary(exe), WithSessionRoot(root)}, extra...)
	d, err := New(context.Background(), "test-machine", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown() })
	return d, root
}

func mustCreate(t *testing.T, d *Driver, key string, spec fleet.SessionSpec) fleet.Session {
	t.Helper()
	if spec.Cwd == "" {
		spec.Cwd = fleet.AbsolutePath(t.TempDir())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := d.Create(ctx, testReq, key, spec)
	if err != nil {
		t.Fatalf("Create(%s): %v", key, err)
	}
	return sess
}

type insideView struct {
	Env []string `json:"env"`
	Cwd string   `json:"cwd"`
	Pid int      `json:"pid"`
}

func (v insideView) vars() map[string]string {
	m := map[string]string{}
	for _, kv := range v.Env {
		k, val, _ := strings.Cut(kv, "=")
		m[k] = val
	}
	return m
}

// inside asks the session's own process what its environment is.
func inside(t *testing.T, d *Driver, id string) insideView {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok {
		t.Fatalf("session %s is not known to the driver", id)
	}
	req, _ := http.NewRequest(http.MethodGet, known.srv.baseURL+"/__test/env", nil)
	req.SetBasicAuth(known.srv.username, known.srv.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reading the session's environment: %v", err)
	}
	defer resp.Body.Close()
	var v insideView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s (pid %d) is still running", what, pid)
}

func kindOfErr(err error) fleet.ErrorKind {
	var fe *fleet.Error
	if errors.As(err, &fe) {
		return fe.Kind
	}
	return ""
}

// ---- Oracle 1: the service's own environment does not reach a session --------

func TestIsolation_ServiceEnvDoesNotReachSession(t *testing.T) {
	t.Setenv("MUSTER_TEST_SENTINEL", "leaked-from-the-service")
	t.Setenv("LANG", "C.UTF-8")
	d, root := newIsolatedDriver(t)
	cwd := t.TempDir()

	sess := mustCreate(t, d, "k1", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(cwd), Env: map[string]string{"LANE_KEY": "only-mine"},
	})
	v := inside(t, d, sess.ID)
	vars := v.vars()

	if _, leaked := vars["MUSTER_TEST_SENTINEL"]; leaked {
		t.Fatal("the service's sentinel variable reached the session")
	}
	if vars["LANE_KEY"] != "only-mine" {
		t.Errorf("the create's env was not delivered: %q", vars["LANE_KEY"])
	}
	allowed := map[string]bool{
		"PATH": true, "LANG": true, "HOME": true, "TMPDIR": true,
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true,
		"OPENCODE_DISABLE_CLAUDE_CODE": true, "OPENCODE_SERVER_USERNAME": true, "OPENCODE_SERVER_PASSWORD": true,
		"LANE_KEY": true,
	}
	for name := range vars {
		if !allowed[name] {
			t.Errorf("unexpected variable %s in the session's environment", name)
		}
	}
	if vars["OPENCODE_DISABLE_CLAUDE_CODE"] != "1" {
		t.Errorf("OPENCODE_DISABLE_CLAUDE_CODE = %q, want 1", vars["OPENCODE_DISABLE_CLAUDE_CODE"])
	}
	if vars["LANG"] != "C.UTF-8" {
		t.Errorf("LANG = %q, want the service's value", vars["LANG"])
	}

	known, _ := d.wasSeen(sess.ID)
	for _, name := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if !strings.HasPrefix(vars[name], known.srv.dir+string(filepath.Separator)) {
			t.Errorf("%s = %q, want it inside the session's directory %q", name, vars[name], known.srv.dir)
		}
	}
	if !strings.HasPrefix(known.srv.dir, root) {
		t.Errorf("session directory %q is not under the session root %q", known.srv.dir, root)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, p := range strings.Split(vars["PATH"], ":") {
			if p == home || strings.HasPrefix(p, home+"/") {
				t.Errorf("PATH entry %q is under the service user's home", p)
			}
		}
	}
	// The working directory is the one the create named; compare resolved
	// paths because a temp directory may sit behind a symlink.
	want, _ := filepath.EvalSymlinks(cwd)
	got, _ := filepath.EvalSymlinks(v.Cwd)
	if got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

// ---- Oracle 2: two concurrent sessions each see only their own ---------------

func TestIsolation_ConcurrentSessionsSeeOnlyTheirOwnEnv(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	var wg sync.WaitGroup
	ids := make([]string, 2)
	for i, lane := range []string{"a", "b"} {
		wg.Add(1)
		go func(i int, lane string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			sess, err := d.Create(ctx, testReq, "lane-"+lane, fleet.SessionSpec{
				Cwd: fleet.AbsolutePath(t.TempDir()),
				Env: map[string]string{"LANE": lane, "ONLY_" + strings.ToUpper(lane): "yes"},
			})
			if err != nil {
				t.Errorf("Create(%s): %v", lane, err)
				return
			}
			ids[i] = sess.ID
		}(i, lane)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	a, b := inside(t, d, ids[0]).vars(), inside(t, d, ids[1]).vars()
	if a["LANE"] != "a" || b["LANE"] != "b" {
		t.Fatalf("LANE = %q / %q, want a / b", a["LANE"], b["LANE"])
	}
	if _, leak := a["ONLY_B"]; leak {
		t.Error("session a sees session b's variable")
	}
	if _, leak := b["ONLY_A"]; leak {
		t.Error("session b sees session a's variable")
	}
	if a["HOME"] == b["HOME"] || a["OPENCODE_SERVER_PASSWORD"] == b["OPENCODE_SERVER_PASSWORD"] {
		t.Error("the two sessions share a home directory or a server credential")
	}
	ka, _ := d.wasSeen(ids[0])
	kb, _ := d.wasSeen(ids[1])
	if ka.srv == kb.srv || ka.srv.baseURL == kb.srv.baseURL {
		t.Error("the two sessions share a server")
	}
}

// ---- Oracle: close takes the whole process group, and the directory ----------

func TestClose_KillsTheSessionsProcessGroup(t *testing.T) {
	d, root := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	v := inside(t, d, sess.ID)

	// A long-lived grandchild, as a tool the session ran would be.
	req, _ := http.NewRequest(http.MethodPost, known.srv.baseURL+"/__test/spawn", nil)
	req.SetBasicAuth(known.srv.username, known.srv.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var spawned struct{ Pid int }
	if err := json.NewDecoder(resp.Body).Decode(&spawned); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !alive(spawned.Pid) {
		t.Fatalf("the grandchild (pid %d) never started", spawned.Pid)
	}

	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, "the grandchild", spawned.Pid)
	waitGone(t, "the session's server", v.Pid)
	if _, err := os.Stat(known.srv.dir); !os.IsNotExist(err) {
		t.Errorf("the session directory survived Close: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("session root is not empty after Close: %v", entries)
	}
	if _, err := d.State(context.Background(), testReq, sess.SessionRef); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("State after Close = %v, want ErrNoSuchSession", err)
	}
}

func TestShutdown_StopsEverySessionAndLeavesNoDescendant(t *testing.T) {
	d, root := newIsolatedDriver(t)
	var pids []int
	for _, key := range []string{"s1", "s2"} {
		sess := mustCreate(t, d, key, fleet.SessionSpec{})
		known, _ := d.wasSeen(sess.ID)
		pids = append(pids, inside(t, d, sess.ID).Pid)
		req, _ := http.NewRequest(http.MethodPost, known.srv.baseURL+"/__test/spawn", nil)
		req.SetBasicAuth(known.srv.username, known.srv.password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var spawned struct{ Pid int }
		_ = json.NewDecoder(resp.Body).Decode(&spawned)
		resp.Body.Close()
		pids = append(pids, spawned.Pid)
	}
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		waitGone(t, "a session process", pid)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("session root is not empty after Shutdown: %v", entries)
	}
	if err := d.Shutdown(); err != nil {
		t.Errorf("a second Shutdown = %v, want nil", err)
	}
	_, err := d.Create(context.Background(), testReq, "after", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if err == nil {
		t.Error("Create succeeded after Shutdown")
	}
}

// A session whose process died under it is dead — read from the process, not
// reported as an unreachable server.
func TestState_ReportsDeadWhenTheSessionsProcessExited(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	signalGroup(known.srv.proc.cmd, syscall.SIGKILL)
	select {
	case <-known.srv.proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not exit")
	}
	st, err := d.State(context.Background(), testReq, sess.SessionRef)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Status != fleet.StatusDead {
		t.Errorf("Status = %q, want dead", st.Status)
	}
}

// ---- Oracle: a create that fails leaves nothing behind -----------------------

func TestCreate_FailedSpawnLeavesNothingBehind(t *testing.T) {
	for _, marker := range []string{".fake-fail-ready", ".fake-fail-create"} {
		t.Run(marker, func(t *testing.T) {
			d, root := newIsolatedDriver(t)
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, marker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := d.Create(ctx, testReq, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)}); err == nil {
				t.Fatal("Create succeeded against a server that cannot come up")
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Errorf("session root is not empty after a failed create: %v", entries)
			}
			d.mu.RLock()
			live := len(d.live)
			d.mu.RUnlock()
			if live != 0 {
				t.Errorf("%d server(s) still tracked after a failed create", live)
			}
			// No idempotency entry was stored: the same key, now able to
			// succeed, creates a session rather than replaying a failure.
			if err := os.Remove(filepath.Join(cwd, marker)); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Create(ctx, testReq, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)}); err != nil {
				t.Fatalf("retry with the same key: %v", err)
			}
		})
	}
}

// ---- Oracle: this machine's sessionEnv reaches the session -------------------

func TestIsolation_SessionEnvPrecedenceAndFreshRead(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	d, _ := newIsolatedDriver(t, WithSessionEnv([]sessionenv.Entry{
		{Name: "MACHINE_IDENTITY", FromFile: secret, Required: true},
		{Name: "NICE_TO_HAVE", FromFile: secret},
		{Name: "NEEDS_REPAIR", FromFile: missing, Required: true, AppliesTo: sessionenv.Scope{Agents: []string{"broken"}}},
	}))

	// Configured values arrive; the caller wins on a non-required entry.
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Env: map[string]string{"NICE_TO_HAVE": "mine"}})
	vars := inside(t, d, sess.ID).vars()
	if vars["MACHINE_IDENTITY"] != "v1" || vars["NICE_TO_HAVE"] != "mine" {
		t.Errorf("MACHINE_IDENTITY=%q NICE_TO_HAVE=%q", vars["MACHINE_IDENTITY"], vars["NICE_TO_HAVE"])
	}

	// The file is read fresh on every create.
	if err := os.WriteFile(secret, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess2 := mustCreate(t, d, "k2", fleet.SessionSpec{})
	if got := inside(t, d, sess2.ID).vars()["MACHINE_IDENTITY"]; got != "v2" {
		t.Errorf("a rotated file was not picked up: %q", got)
	}

	// A required entry refuses ANY caller value, without reading the file.
	_, err := d.Create(context.Background(), testReq, "k3", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Env: map[string]string{"MACHINE_IDENTITY": "x"},
	})
	if kindOfErr(err) != fleet.ErrorInvalid {
		t.Errorf("required entry + caller value = %v, want ErrorInvalid", err)
	}

	// A required entry this machine cannot back is the operator's problem.
	_, err = d.Create(context.Background(), testReq, "k4", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Agent: "broken",
	})
	if kindOfErr(err) != fleet.ErrorUnsupported {
		t.Errorf("missing required file = %v, want ErrorUnsupported", err)
	}
}

func TestNew_RefusesASessionEnvEntryNamingAVariableTheDriverOwns(t *testing.T) {
	exe, _ := os.Executable()
	_, err := New(context.Background(), "test-machine", WithBinary(exe),
		WithSessionEnv([]sessionenv.Entry{{Name: "HOME", FromFile: "/etc/x"}}))
	if err == nil {
		t.Fatal("New accepted a sessionEnv entry naming HOME")
	}
}

func TestNew_RefusesARuntimePathUnderTheHomeOrRelative(t *testing.T) {
	exe, _ := os.Executable()
	if _, err := New(context.Background(), "m", WithBinary(exe), WithRuntimePath([]string{"relative/bin"})); err == nil {
		t.Error("a relative runtime path was accepted")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if _, err := New(context.Background(), "m", WithBinary(exe), WithRuntimePath([]string{filepath.Join(home, ".local", "bin")})); err == nil {
			t.Error("a runtime path under the home directory was accepted")
		}
	}
	if _, err := New(context.Background(), "m", WithBinary(exe), WithRuntimePath([]string{"/opt/tools/bin"})); err != nil {
		t.Errorf("a system runtime path was refused: %v", err)
	}
}

func TestIsolation_RuntimePathReachesTheSession(t *testing.T) {
	d, _ := newIsolatedDriver(t, WithRuntimePath([]string{"/opt/tools/bin"}))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	path := inside(t, d, sess.ID).vars()["PATH"]
	if !strings.HasPrefix(path, "/opt/tools/bin:") {
		t.Errorf("PATH = %q, want the allow-listed directory first", path)
	}
}

// ---- Oracle: names the driver owns are refused, and nothing starts -----------

func TestCreate_RefusesEnvNamingAVariableTheDriverOwns(t *testing.T) {
	for _, name := range []string{"HOME", "OPENCODE_SERVER_PASSWORD", "XDG_DATA_HOME"} {
		d, root := newIsolatedDriver(t)
		_, err := d.Create(context.Background(), testReq, "k-"+name, fleet.SessionSpec{
			Cwd: fleet.AbsolutePath(t.TempDir()), Env: map[string]string{name: "x"},
		})
		if kindOfErr(err) != fleet.ErrorInvalid {
			t.Errorf("env %s = %v, want ErrorInvalid", name, err)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("env %s: a session directory was made before the refusal", name)
		}
		d.mu.RLock()
		live := len(d.live)
		d.mu.RUnlock()
		if live != 0 {
			t.Errorf("env %s: a server was started before the refusal", name)
		}
	}
}

// ---- Oracle: the same key, concurrently, starts ONE process ------------------

func TestCreate_ConcurrentCreatesWithOneKeyStartOneProcess(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	cwd := fleet.AbsolutePath(t.TempDir())
	var wg sync.WaitGroup
	ids := make([]string, 3)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sess, err := d.Create(ctx, testReq, "same-key", fleet.SessionSpec{Cwd: cwd})
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			ids[i] = sess.ID
		}(i)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("ids = %v, want one id three times", ids)
	}
	d.mu.RLock()
	live := len(d.live)
	d.mu.RUnlock()
	if live != 1 {
		t.Errorf("%d servers are running, want exactly 1", live)
	}
}

// ---- Capabilities and shared mode --------------------------------------------

func TestCapabilities_IsolationIsDeclaredByMode(t *testing.T) {
	iso, _ := newIsolatedDriver(t)
	if !iso.Capabilities().IsolatesEnvironment {
		t.Error("an isolated-mode driver reports IsolatesEnvironment: false")
	}
	shared := newTestDriver(t, newFakeServer(t))
	if shared.Capabilities().IsolatesEnvironment {
		t.Error("a shared-mode driver reports IsolatesEnvironment: true")
	}
}

func TestSharedMode_RefusesEnvAndIsolateEnvironmentAsUnsupported(t *testing.T) {
	d := newTestDriver(t, newFakeServer(t))
	for name, spec := range map[string]fleet.SessionSpec{
		"env":                {Cwd: "/w", Env: map[string]string{"A": "b"}},
		"isolateEnvironment": {Cwd: "/w", IsolateEnvironment: true},
	} {
		_, err := d.Create(context.Background(), testReq, "k-"+name, spec)
		if kindOfErr(err) != fleet.ErrorUnsupported {
			t.Errorf("%s in shared mode = %v, want ErrorUnsupported", name, err)
		}
	}
}

func TestIsolatedMode_HonoursIsolateEnvironment(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	mustCreate(t, d, "k1", fleet.SessionSpec{IsolateEnvironment: true})
}
