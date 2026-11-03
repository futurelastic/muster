//go:build unix

package opencode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #398: a server process must not outlive the session it served. These
// tests start REAL processes (the test binary as a stand-in opencode, or a
// shell wearing a server's command line), because what they assert is whether
// the operating system still has one.

// A record the new service does not relaunch is still a session whose server the
// previous service left running. The caller-supplied environment is the common
// reason a session is not relaunched, and before #398 it was exactly the case
// whose server nothing ever stopped. The start-up sweep is off so it is the reap
// of the RECORDED server that acts.
func TestResume_ANonRelaunchableRecordsServerIsStoppedAtStart(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d1.Shutdown() })
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "withenv", Env: map[string]string{"MODEL_KEY": "a-value"}})
	oldPid := inside(t, d1, sess.ID).Pid
	if !alive(oldPid) {
		t.Fatal("the first service's server is not running")
	}
	// No Shutdown: the service "crashed", and its server is still running.
	simulateCrash(d1)
	t.Cleanup(func() { killGroupOf(oldPid) })

	var logged logLines
	d2 := newStateDriver(t, state, withoutStartSweep(), WithLogf(logged.add))
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusUnknown {
		t.Fatalf("the session after the restart = %+v (listed: %v); want it listed unknown", got.State, ok)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Fatal("a process was started for a session that cannot be relaunched faithfully")
	}
	waitGone(t, "the previous service's server of a session that was not relaunched", oldPid)
	if !logged.has("stopped a server left running", "pid "+strconv.Itoa(oldPid)) {
		t.Errorf("no log line says the server was stopped: %q", logged.all())
	}
}

// unknownSessionWithStray builds the state the close of an unknown session must
// cope with: a session listed unknown and a server the start-up reap did not
// stop, started by hand with its pid, port and binary written into the session's
// record the way a service's own start writes them. portShift moves the recorded
// port off the server's real one.
func unknownSessionWithStray(t *testing.T, portShift int) (d *Driver, state, id string, stray *server) {
	t.Helper()
	state = t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "withenv", Env: map[string]string{"MODEL_KEY": "a-value"}})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d = newStateDriver(t, state, withoutStartSweep())
	t.Cleanup(func() { _ = d.Shutdown() })
	if k, ok := d.wasSeen(sess.ID); !ok || k.srv != nil {
		t.Fatalf("seen = %v, srv = %v; want an unknown session with no server of this service", ok, k.srv)
	}
	rec := d.store.load(sess.ID)
	if rec == nil {
		t.Fatal("the record of the unknown session cannot be read")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stray, err = startServer(ctx, exe, t.TempDir(), "muster", d.buildEnv(rec.Dir, nil), "")
	if err != nil {
		t.Fatalf("starting the stand-in server: %v", err)
	}
	t.Cleanup(stray.teardown)
	d.stampProcess(rec, stray)
	rec.Port += portShift
	if err := d.store.write(rec); err != nil {
		t.Fatal(err)
	}
	if !alive(rec.Pid) {
		t.Fatal("the stand-in server is not running")
	}
	return d, state, sess.ID, stray
}

// Closing a session that was listed unknown stops the server its record still
// names, whatever stopped the start-up reap from seeing it.
func TestClose_AnUnknownSessionStopsTheServerItsRecordNames(t *testing.T) {
	d, state, id, stray := unknownSessionWithStray(t, 0)
	pid := stray.proc.cmd.Process.Pid

	if _, err := d.Close(context.Background(), testReq, fleet.SessionRef{Machine: "test-machine", ID: id}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, "the server an unknown session's record named", pid)
	if len(recordFiles(t, state)) != 0 {
		t.Error("the record outlived Close")
	}
}

// The record is only a pointer: a process that is not the server it names — here
// a server on another port than the recorded one — is not touched by a close.
func TestClose_AnUnknownSessionLeavesAProcessItsRecordDoesNotDescribe(t *testing.T) {
	d, state, id, stray := unknownSessionWithStray(t, 1)
	pid := stray.proc.cmd.Process.Pid

	if _, err := d.Close(context.Background(), testReq, fleet.SessionRef{Machine: "test-machine", ID: id}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !alive(pid) {
		t.Error("Close stopped a process whose port is not the recorded one")
	}
	if len(recordFiles(t, state)) != 0 {
		t.Error("the record outlived Close")
	}
}

// A close the caller's own expectation refuses stops nothing.
func TestClose_ARefusedCloseOfAnUnknownSessionStopsNothing(t *testing.T) {
	d, _, id, stray := unknownSessionWithStray(t, 0)
	pid := stray.proc.cmd.Process.Pid
	wrong := time.Unix(1, 0)
	req := fleet.RequestFrom(fleet.Caller{Principal: "isolation-test"})
	req.Expect.StartedAt = &wrong

	_, err := d.Close(context.Background(), req, fleet.SessionRef{Machine: "test-machine", ID: id})
	if err == nil {
		t.Fatal("Close succeeded against a start time that is not the session's")
	}
	if !alive(pid) {
		t.Error("a refused Close stopped the server")
	}
}

// reapStale answers for a pid only when the process there is the recorded
// server; a stranger holding a recorded pid is not touched.
func TestReapStale_AStrangerOnTheRecordedPidIsLeftAlone(t *testing.T) {
	stranger := spawnStandIn(t, standIn{tmpdir: "/tmp", leader: true})
	rec := &sessionRecord{ID: "ses_x", Pid: stranger.pid, Port: stranger.port + 1, Bin: "/no/such/opencode"}
	if reapStale(rec) {
		t.Error("reapStale stopped a process that is not the recorded server")
	}
	if !alive(stranger.pid) {
		t.Error("an unrelated process was stopped")
	}
}

// ---- the start-up sweep ------------------------------------------------------

func needsProcessScan(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the process scan reads the environment of another process, which only macOS and Linux offer")
	}
}

// logLines collects what a driver says, for the tests that assert on it.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *logLines) has(parts ...string) bool {
	for _, line := range l.all() {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// standIn is a process wearing a server's command line and environment: the
// test binary acting as the stand-in runtime (fakeopencode_test.go), started with
// `serve --port N --hostname 127.0.0.1` and the TMPDIR a scoped directory gives
// it. It has to be such a binary: macOS prints no environment for a protected
// system one (a shell, say), and the scan would then rightly see nothing.
type standIn struct {
	tmpdir string // its TMPDIR
	leader bool   // the leader of its own process group
	secret string // a value in its environment that must never be read back out
}

type runningStandIn struct {
	pid, port int
}

func spawnStandIn(t *testing.T, s standIn) runningStandIn {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + s.tmpdir, "TMPDIR=" + s.tmpdir, "OPENCODE_SERVER_USERNAME=muster", "OPENCODE_SERVER_PASSWORD=" + s.secret}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: s.leader}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Waited on, so a stopped stand-in is gone and not a zombie that still answers
	// signal 0.
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() {
		if s.leader {
			killGroupOf(pid)
		} else {
			_ = killProc(pid)
		}
	})
	return runningStandIn{pid: pid, port: port}
}

func scopedTmp(root string, n int) string {
	return filepath.Join(root, "muster-opencode-"+strconv.Itoa(n), "tmp")
}

func TestSweep_StopsOnlyServersUnderItsRootThatAreLeadersOfTheirGroup(t *testing.T) {
	needsProcessScan(t)
	const secret = "planted-credential-6f1c2a"
	state := t.TempDir()
	_, root := stateLayout(state)
	other := filepath.Join(t.TempDir(), "opencode", "dirs")

	orphan := spawnStandIn(t, standIn{tmpdir: scopedTmp(root, 111), leader: true, secret: secret})
	otherRoot := spawnStandIn(t, standIn{tmpdir: scopedTmp(other, 222), leader: true})
	unscoped := spawnStandIn(t, standIn{tmpdir: "/tmp", leader: true})
	notLeader := spawnStandIn(t, standIn{tmpdir: scopedTmp(root, 333), leader: false})

	var logged logLines
	d := newStateDriver(t, state, WithLogf(logged.add))
	t.Cleanup(func() { _ = d.Shutdown() })

	waitGone(t, "a server under the root with no session", orphan.pid)
	for name, p := range map[string]runningStandIn{
		"a server under another root":              otherRoot,
		"a process with no scoped environment":     unscoped,
		"a process that is not its group's leader": notLeader,
	} {
		if !alive(p.pid) {
			t.Errorf("%s was stopped", name)
		}
	}
	if !logged.has("stopped a server left running", "pid "+strconv.Itoa(orphan.pid), "port "+strconv.Itoa(orphan.port), "dir muster-opencode-111") {
		t.Errorf("no line names the stopped server: %q", logged.all())
	}
	if n := len(logged.all()); n != 1 {
		t.Errorf("%d log lines for one stopped server: %q", n, logged.all())
	}
	for _, line := range logged.all() {
		if strings.Contains(line, secret) || strings.Contains(line, "PASSWORD") {
			t.Errorf("a log line carries the environment: %q", line)
		}
	}
}

// Without a state directory the root is the OS temp directory, which is not the
// driver's alone: nothing is swept.
func TestSweep_DoesNotRunWithoutAStateDirectory(t *testing.T) {
	needsProcessScan(t)
	root := t.TempDir()
	p := spawnStandIn(t, standIn{tmpdir: scopedTmp(root, 1), leader: true})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(context.Background(), "test-machine", WithBinary(exe), WithSessionRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Shutdown() })
	if !alive(p.pid) {
		t.Error("a driver with no state directory stopped a server")
	}
}

// The measured case: a session closed while it was unknown leaves a server with
// no record and no directory. A restart finds it by its own environment.
func TestSweep_StopsAServerWhoseRecordAndDirectoryAreGone(t *testing.T) {
	needsProcessScan(t)
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d1.Shutdown() })
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "closed-while-unknown"})
	oldPid := inside(t, d1, sess.ID).Pid
	k, _ := d1.wasSeen(sess.ID)
	simulateCrash(d1)
	t.Cleanup(func() { killGroupOf(oldPid) })
	if err := os.Remove(filepath.Join(state, "opencode", "sessions", sess.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(k.srv.dir); err != nil {
		t.Fatal(err)
	}

	var logged logLines
	d2 := newStateDriver(t, state, WithLogf(logged.add))
	t.Cleanup(func() { _ = d2.Shutdown() })
	waitGone(t, "a server whose record and directory are gone", oldPid)
	if !logged.has("stopped a server left running", "pid "+strconv.Itoa(oldPid)) {
		t.Errorf("no log line says the server was stopped: %q", logged.all())
	}
}

// ---- scoped directories no record names (muster #399) -------------------------

// Through the real start: with a state directory the driver removes the
// leftovers of a removal that did not finish and keeps a directory that may hold
// a conversation, a directory a record names, and one that was written a moment ago.
func TestOpen_RemovesRemnantDirectoriesNoRecordNames(t *testing.T) {
	needsProcessScan(t)
	state := t.TempDir()
	f := &scopedFixture{t: t, now: time.Now()}
	f.storeDir, f.root = stateLayout(state)
	for _, d := range []string{f.storeDir, f.root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := 3 * time.Hour
	remnant := f.dir("muster-opencode-11", old, remnantXDG)
	scratch := f.dir("muster-opencode-12", old, remnantTmp)
	store := f.dir("muster-opencode-13", old, withStore)
	recent := f.dir("muster-opencode-14", time.Minute, remnantXDG)
	named := f.dir("muster-opencode-15", old, remnantXDG)
	// The record names a directory whose session cannot be relaunched here (it
	// carries caller-supplied environment), so the directory is kept as the
	// record's own.
	if err := (&recordStore{dir: f.storeDir}).write(&sessionRecord{ID: "ses_kept", Dir: named, EnvNames: []string{"X"}}); err != nil {
		t.Fatal(err)
	}

	var logged logLines
	d := newStateDriver(t, state, WithLogf(logged.add))
	t.Cleanup(func() { _ = d.Shutdown() })

	for _, p := range []string{remnant, scratch} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was not removed at start (%v)", filepath.Base(p), err)
		}
	}
	for _, p := range []string{store, recent, named} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed at start: %v", filepath.Base(p), err)
		}
	}
	if !logged.has("removed a session directory", "muster-opencode-11") {
		t.Errorf("no log line says the directory was removed: %q", logged.all())
	}
}

// ---- the count the doctor reports ---------------------------------------------

func TestOrphanServers_CountsServersNoRecordNames(t *testing.T) {
	needsProcessScan(t)
	state := t.TempDir()
	storeDir, root := stateLayout(state)
	recorded := spawnStandIn(t, standIn{tmpdir: scopedTmp(root, 1), leader: true})
	orphan := spawnStandIn(t, standIn{tmpdir: scopedTmp(root, 2), leader: true})
	other := spawnStandIn(t, standIn{tmpdir: scopedTmp(filepath.Join(t.TempDir(), "x"), 3), leader: true})
	_ = other

	pids, _, err := OrphanServers(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 2 || pids[0] != min(recorded.pid, orphan.pid) || pids[1] != max(recorded.pid, orphan.pid) {
		t.Fatalf("with no record: pids = %v; want the two servers under the root, %d and %d", pids, recorded.pid, orphan.pid)
	}

	store := &recordStore{dir: storeDir}
	if err := store.write(&sessionRecord{ID: "ses_recorded", Pid: recorded.pid, Port: recorded.port, Dir: filepath.Join(root, "muster-opencode-1")}); err != nil {
		t.Fatal(err)
	}
	pids, _, err = OrphanServers(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 1 || pids[0] != orphan.pid {
		t.Fatalf("with a record naming one: pids = %v; want only %d", pids, orphan.pid)
	}
}

func TestSplitPSLine(t *testing.T) {
	cases := []struct {
		line           string
		pid, pgid, uid int
		cmd            string
		ok             bool
	}{
		{"  3410  3410   501 /Users/x/.local/bin/opencode serve --port 49458", 3410, 3410, 501, "/Users/x/.local/bin/opencode serve --port 49458", true},
		{"72562 72562     0 /bin/x a  b", 72562, 72562, 0, "/bin/x a  b", true},
		{"  1  1  0 ", 0, 0, 0, "", false},
		{"x 1 2 cmd", 0, 0, 0, "", false},
		{"", 0, 0, 0, "", false},
	}
	for _, tc := range cases {
		pid, pgid, uid, cmd, ok := splitPSLine(tc.line)
		if pid != tc.pid || pgid != tc.pgid || uid != tc.uid || cmd != tc.cmd || ok != tc.ok {
			t.Errorf("splitPSLine(%q) = %d %d %d %q %v; want %d %d %d %q %v", tc.line, pid, pgid, uid, cmd, ok, tc.pid, tc.pgid, tc.uid, tc.cmd, tc.ok)
		}
	}
}

// macOS prints no environment for a protected process; such a line must read as
// "could not be inspected", never as a server that is not ours.
func TestEnvHidden(t *testing.T) {
	if !envHidden("sh -c sleep 30; : serve --port 5 --hostname 127.0.0.1") {
		t.Error("a line with no environment was not read as hidden")
	}
	if envHidden("/bin/opencode serve --port 5 --hostname 127.0.0.1 PATH=/usr/bin HOME=/x TMPDIR=/x/tmp") {
		t.Error("a line with an environment was read as hidden")
	}
}
