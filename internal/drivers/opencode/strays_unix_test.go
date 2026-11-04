//go:build unix

package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #405: a process a session's tool started in a session of its own is in
// no process group the server's stop reaches. These tests start REAL processes —
// the test binary standing in for a browser (TestMain, "__stray") — and ask the
// operating system whether they are still there.

// strayAt describes one stand-in process.
type strayAt struct {
	// tmpdir is its TMPDIR; empty leaves the environment without one.
	tmpdir string
	// exe, when set, is the program run instead of the test binary (a copy of it
	// placed somewhere the executable fact can see).
	exe string
	// args follow "__stray".
	args []string
}

// spawnStray starts a stand-in in a session of its own, like a browser a tool
// launched with setsid, and returns its pid.
func spawnStray(t *testing.T, s strayAt) int {
	t.Helper()
	exe := s.exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(exe, append([]string{"__stray"}, s.args...)...)
	// A PATH and a HOME, so a line printed without an environment is not what is
	// being tested; TMPDIR only when the case asks for one.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	if s.tmpdir != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+s.tmpdir)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = killProc(pid) })
	return pid
}

// copyOfThisBinary places a copy of the test binary at path, the way a browser a
// tool installed sits inside the session's own temp area.
func copyOfThisBinary(t *testing.T, path string) string {
	t.Helper()
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// detachVia asks the session's stand-in runtime to start a process outside its
// own process group, which then inherits the session's environment.
func detachVia(t *testing.T, d *Driver, id string) int {
	t.Helper()
	known, _ := d.wasSeen(id)
	req, _ := http.NewRequest(http.MethodPost, known.srv.baseURL+"/__test/detach", nil)
	req.SetBasicAuth(known.srv.username, known.srv.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Pid int }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Pid == 0 {
		t.Fatalf("detach: pid %d, err %v", out.Pid, err)
	}
	if !alive(out.Pid) {
		t.Fatalf("the detached process (pid %d) never started", out.Pid)
	}
	t.Cleanup(func() { _ = killProc(out.Pid) })
	return out.Pid
}

// strangers are processes that must survive whichever session is closed.
type strangers struct {
	otherSession, otherRoot, noScope, mentions int
}

// startStrangers starts the processes the rule must not take for a session's: one
// of another session in the same root, one under a root that shares a prefix with
// it, one with no scoped directory at all, and one that only has a path inside the
// session's directory among its arguments.
func startStrangers(t *testing.T, root, session string) strangers {
	t.Helper()
	return strangers{
		otherSession: spawnStray(t, strayAt{tmpdir: scopedTmp(root, 999)}),
		otherRoot:    spawnStray(t, strayAt{tmpdir: scopedTmp(root+"-sibling", 1)}),
		noScope:      spawnStray(t, strayAt{tmpdir: os.TempDir()}),
		mentions:     spawnStray(t, strayAt{tmpdir: os.TempDir(), args: []string{filepath.Join(root, session, "tmp", "a-file")}}),
	}
}

func (s strangers) mustBeAlive(t *testing.T) {
	t.Helper()
	for name, pid := range map[string]int{
		"a process of another session":                 s.otherSession,
		"a process under a root that shares a prefix":  s.otherRoot,
		"a process with no scoped directory":           s.noScope,
		"a process that only mentions the session dir": s.mentions,
	} {
		if !alive(pid) {
			t.Errorf("%s (pid %d) was stopped", name, pid)
		}
	}
}

// ---- the identity rule on real processes -------------------------------------

// Each of the three facts ties a process to its directory by itself, in a process
// group of its own; nothing else does. `only` restricts the sweep to one
// directory, and with no directory named the same call stops every scoped one — the
// start-up case.
func TestStopStrays_StopsWhatBelongsToTheDirectoryAndNothingElse(t *testing.T) {
	needsProcessScan(t)
	root := filepath.Join(t.TempDir(), "dirs")
	session := "muster-opencode-1"
	dir := filepath.Join(root, session)

	byEnv := spawnStray(t, strayAt{tmpdir: scopedTmp(root, 1)})
	byExe := spawnStray(t, strayAt{
		exe:    copyOfThisBinary(t, filepath.Join(dir, "tmp", "pw-browsers", "chromium", "chrome-headless-shell")),
		tmpdir: os.TempDir(),
	})
	byProfile := spawnStray(t, strayAt{tmpdir: os.TempDir(), args: []string{"--user-data-dir=" + filepath.Join(dir, "tmp", "playwright-profile")}})
	other := startStrangers(t, root, session)

	stopped, left, err := stopStrays(root, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("left = %+v; want nothing", left)
	}
	got := map[int]strayBy{}
	for _, p := range stopped {
		if p.dir != session {
			t.Errorf("stopped a process of %s while asked for %s", p.dir, session)
		}
		got[p.pid] = p.by
	}
	want := map[int]strayBy{byEnv: strayByEnvironment, byExe: strayByExecutable, byProfile: strayByProfile}
	if len(got) != len(want) {
		t.Errorf("stopped = %v; want %v", got, want)
	}
	for pid, by := range want {
		if got[pid] != by {
			t.Errorf("pid %d stopped by %q; want %q", pid, got[pid], by)
		}
		waitGone(t, "a process that belongs to the directory ("+string(by)+")", pid)
	}
	other.mustBeAlive(t)

	// Nothing named: every scoped directory under the root, and still no stranger.
	stopped, _, err = stopStrays(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 || stopped[0].pid != other.otherSession || stopped[0].dir != "muster-opencode-999" {
		t.Errorf("with no directory named stopped = %+v; want only the process of muster-opencode-999", stopped)
	}
	waitGone(t, "a process of another session, once all were asked for", other.otherSession)
	for name, pid := range map[string]int{"another root": other.otherRoot, "no scope": other.noScope, "a mention": other.mentions} {
		if !alive(pid) {
			t.Errorf("the process of %s was stopped", name)
		}
	}
}

// The sweep must not stop the process running it, or one of its ancestors, even
// when it carries the very fact that matches: a service started from inside a
// session's own tool shell would otherwise stop itself, or the shell around it.
func TestStopStrays_NeverSignalsItselfOrItsAncestors(t *testing.T) {
	needsProcessScan(t)
	root := filepath.Join(t.TempDir(), "dirs")
	profile := "--user-data-dir=" + filepath.Join(root, "muster-opencode-1", "tmp", "p")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	self, err := exec.Command(exe, "__stopfrom", root, profile).CombinedOutput()
	if err != nil || !strings.Contains(string(self), "stopped=0 left=0") {
		t.Errorf("a process that matches itself: %q, %v; want it to survive and stop nothing", self, err)
	}
	// The parent matches; its child runs the sweep.
	rel, err := exec.Command(exe, "__relay", root, profile).CombinedOutput()
	if err != nil || !strings.Contains(string(rel), "stopped=0 left=0") || !strings.Contains(string(rel), "relay-alive err=<nil>") {
		t.Errorf("a process whose parent matches: %q, %v; want the parent to survive", rel, err)
	}
}

func TestSelfAndAncestors(t *testing.T) {
	m := selfAndAncestors()
	if !m[os.Getpid()] || !m[os.Getppid()] {
		t.Errorf("selfAndAncestors = %v; want this process (%d) and its parent (%d)", m, os.Getpid(), os.Getppid())
	}
	if m[0] || m[1] {
		t.Errorf("selfAndAncestors = %v holds pid 0 or 1", m)
	}
}

// ---- Close, Shutdown, closeUnlaunched, start-up --------------------------------

// The measured case: a process the session's tool started outside the server's
// group is reparented to init when the server goes, and nothing stopped it.
func TestClose_StopsAProcessThatLeftTheSessionsGroup(t *testing.T) {
	needsProcessScan(t)
	d, root := newIsolatedDriver(t, withStraySweep())
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	session := filepath.Base(known.srv.dir)
	left := detachVia(t, d, sess.ID)
	other := startStrangers(t, root, session)

	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, "a process the session started outside its group", left)
	other.mustBeAlive(t)
	if _, err := os.Stat(known.srv.dir); !os.IsNotExist(err) {
		t.Errorf("the session directory survived Close: %v", err)
	}
}

// The service stopping is not the session closing: the directory stays, and so
// must nothing the old server started — the next start runs a new server there.
func TestShutdown_StopsAProcessThatLeftTheGroupAndKeepsThePersistedDirectory(t *testing.T) {
	needsProcessScan(t)
	state := t.TempDir()
	var logged logLines
	d := newStateDriver(t, state, withStraySweep(), WithLogf(logged.add))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	left := detachVia(t, d, sess.ID)

	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, "a process the session started outside its group", left)
	if _, err := os.Stat(known.srv.dir); err != nil {
		t.Errorf("the persisted session's directory is gone after Shutdown: %v", err)
	}
	if !logged.has("stopped 1 process(es)", "dir "+filepath.Base(known.srv.dir)) {
		t.Errorf("no log line says what was stopped: %q", logged.all())
	}
}

// A session listed unknown has no server of this service, only a directory; closing
// it removes the directory, so whatever still runs from it goes first.
func TestClose_AnUnknownSessionStopsWhatRunsFromItsDirectory(t *testing.T) {
	needsProcessScan(t)
	d, _, id, _ := unknownSessionWithStray(t, 0)
	d.noStraySweep = false
	rec := d.store.load(id)
	root := filepath.Dir(rec.Dir)
	left := spawnStray(t, strayAt{tmpdir: filepath.Join(rec.Dir, "tmp")})
	other := startStrangers(t, root, filepath.Base(rec.Dir))
	var logged logLines
	d.logf = logged.add

	if _, err := d.Close(context.Background(), testReq, fleet.SessionRef{Machine: "test-machine", ID: id}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, "a process that runs from the closed session's directory", left)
	other.mustBeAlive(t)
	if !logged.has("stopped 1 process(es)", "dir "+filepath.Base(rec.Dir)) {
		t.Errorf("no log line says what was stopped: %q", logged.all())
	}
}

// Nothing names a process whose session is gone and whose directory too; a start
// finds it where it lives and stops it before anything is resumed.
func TestOpen_StopsProcessesAnEarlierServiceLeftOutsideAnyGroup(t *testing.T) {
	needsProcessScan(t)
	state := t.TempDir()
	_, root := stateLayout(state)
	left := spawnStray(t, strayAt{tmpdir: scopedTmp(root, 777)})
	byExe := spawnStray(t, strayAt{
		exe:    copyOfThisBinary(t, filepath.Join(root, "muster-opencode-778", "tmp", "pw-browsers", "chrome-headless-shell")),
		tmpdir: os.TempDir(),
	})
	// Not the leader of its own group, so the SERVER sweep leaves it alone (#398);
	// it carries a session's TMPDIR all the same, which is what makes it one of the
	// session's processes.
	notLeader := spawnStandIn(t, standIn{tmpdir: scopedTmp(root, 779), leader: false})
	other := startStrangers(t, root, "muster-opencode-777")
	// The other session's process is, by design, also a stray at start-up: nothing
	// is live then. The strangers that must remain are the ones outside the root.
	var logged logLines
	d := newStateDriver(t, state, withStraySweep(), WithLogf(logged.add))
	t.Cleanup(func() { _ = d.Shutdown() })

	waitGone(t, "a stray in a scoped directory with no record", left)
	waitGone(t, "a stray whose executable is in a scoped directory with no record", byExe)
	waitGone(t, "a stray of another scoped directory", other.otherSession)
	waitGone(t, "a process that is not its group's leader but carries a session's TMPDIR", notLeader.pid)
	for name, pid := range map[string]int{"another root": other.otherRoot, "no scope": other.noScope, "a mention": other.mentions} {
		if !alive(pid) {
			t.Errorf("the process of %s was stopped", name)
		}
	}
	if !logged.has("stopped 1 process(es)", "left running by an earlier service", "dir muster-opencode-777") ||
		!logged.has("stopped 1 process(es)", "dir muster-opencode-778") ||
		!logged.has("stopped 1 process(es)", "dir muster-opencode-779") {
		t.Errorf("no line names what was stopped: %q", logged.all())
	}
	for _, line := range logged.all() {
		if strings.Contains(line, "PATH=") || strings.Contains(line, "TMPDIR") || strings.Contains(line, "--user-data-dir") {
			t.Errorf("a log line carries a command line or environment: %q", line)
		}
	}
}

// Without a state directory the root is the OS temp directory, which is not the
// driver's alone: nothing is swept, strays included.
func TestOpen_LeavesStraysAloneWithoutAStateDirectory(t *testing.T) {
	needsProcessScan(t)
	root := t.TempDir()
	pid := spawnStray(t, strayAt{tmpdir: scopedTmp(root, 1)})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(context.Background(), "test-machine", WithBinary(exe), WithSessionRoot(root), withStraySweep())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Shutdown() })
	time.Sleep(200 * time.Millisecond)
	if !alive(pid) {
		t.Error("a driver with no state directory stopped a process under its root")
	}
}

// A close that stops nothing costs a process-table read and says nothing.
func TestClose_WithNothingToStopSaysNothing(t *testing.T) {
	needsProcessScan(t)
	var logged logLines
	d, _ := newIsolatedDriver(t, withStraySweep(), WithLogf(logged.add))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, line := range logged.all() {
		if strings.Contains(line, "process(es)") {
			t.Errorf("a close with nothing to stop logged %q", line)
		}
	}
}
