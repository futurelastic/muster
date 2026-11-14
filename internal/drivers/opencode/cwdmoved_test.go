package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/sandbox"
)

// muster #474: a sandbox profile matches the real path of a session's working
// directory as it was when the profile was planned. Rename the directory's parent
// and leave a link at the old name, and the session's process keeps using the old
// name, which now resolves to a path the profile does not name: every access is
// refused, and the runtime answers 500 to every request without saying why.
//
// Everything here uses directories made under t.TempDir; the macOS temp root is
// itself behind a system link, so the cases that must NOT be reported get that
// for free.

// moveable is a working directory under a parent that can be renamed.
type moveable struct {
	root string // as t.TempDir gave it, links and all
	cwd  string // root/a/s: what a create is given
}

func newMoveable(t *testing.T) moveable {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "a", "s")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return moveable{root: root, cwd: cwd}
}

// rename moves root/a to root/b and says where the working directory is now.
func (m moveable) rename(t *testing.T) string {
	t.Helper()
	if err := os.Rename(filepath.Join(m.root, "a"), filepath.Join(m.root, "b")); err != nil {
		t.Fatal(err)
	}
	return realPath(t, filepath.Join(m.root, "b", "s"))
}

// link leaves a link at the old name, pointing at the new one.
func (m moveable) link(t *testing.T) {
	t.Helper()
	if err := os.Symlink(filepath.Join(m.root, "b"), filepath.Join(m.root, "a")); err != nil {
		t.Skipf("this host cannot make a symbolic link: %v", err)
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func planFor(t *testing.T, d *Driver, cwd string) *sandboxPlan {
	t.Helper()
	plan, err := d.planSandbox(fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Sandbox: &fleet.SandboxSpec{}}, t.TempDir())
	if err != nil {
		t.Fatalf("planning a sandbox for %s: %v", cwd, err)
	}
	return plan
}

func hasPath(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

// quoted is a path as the driver's messages print it: %q, which doubles the
// backslashes of a Windows path.
func quoted(p string) string { return fmt.Sprintf("%q", p) }

// requireRenameableWorkdir skips where a directory cannot be renamed while a
// process has it as its working directory, which is the case these tests set up:
// Windows refuses the rename, so the failure they are about cannot arise there.
func requireRenameableWorkdir(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not rename a directory a running process has as its working directory")
	}
}

// noted is the server a start leaves for a session planned for cwd.
func noted(t *testing.T, d *Driver, cwd string) *server {
	t.Helper()
	srv := &server{}
	noteWorkdir(srv, planFor(t, d, cwd), cwd)
	return srv
}

// ---- the plan follows the directory ----------------------------------------

func TestPlan_TheWorkdirIsPlannedFromWhereItResolvesNow(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	m := newMoveable(t)
	before := planFor(t, d, m.cwd)
	oldReal := realPath(t, m.cwd)
	if before.cwd != oldReal || !hasPath(before.write, oldReal) {
		t.Fatalf("the first plan grants %v (cwd %q), want the real path %q", before.write, before.cwd, oldReal)
	}

	newReal := m.rename(t)
	m.link(t)

	// The old name now resolves somewhere else, and a plan made from it (which is
	// what every start does) grants where it resolves now.
	after := planFor(t, d, m.cwd)
	if after.cwd != newReal || !hasPath(after.write, newReal) {
		t.Errorf("the plan after the move grants %v (cwd %q), want it to cover %q", after.write, after.cwd, newReal)
	}
	if hasPath(after.write, oldReal) {
		t.Errorf("the plan after the move still grants the old real path %q: %v", oldReal, after.write)
	}
}

func TestPlan_AnUnmovedWorkdirGetsTheSamePlan(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this host cannot make a symbolic link: %v", err)
	}
	dir := t.TempDir()
	plan := func(cwd string) *sandboxPlan {
		p, err := d.planSandbox(fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Sandbox: &fleet.SandboxSpec{}}, dir)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := plan(real), plan(link)
	if a.cwd != b.cwd || !reflect.DeepEqual(a.write, b.write) || !reflect.DeepEqual(a.read, b.read) {
		t.Errorf("a directory reached through a link made on purpose is planned differently:\n real: %q %v\n link: %q %v",
			a.cwd, a.write, b.cwd, b.write)
	}
}

// ---- detection --------------------------------------------------------------

func TestCwdMoved_ReportsAMoveBehindASymlink(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	m := newMoveable(t)
	srv := noted(t, d, m.cwd)
	planned := srv.cwdPlanned
	newReal := m.rename(t)
	m.link(t)

	cause := d.cwdMoved(srv)
	for _, want := range []string{"moved", quoted(planned), quoted(newReal), "create it again"} {
		if !strings.Contains(cause, want) {
			t.Errorf("the cause %q does not say %q", cause, want)
		}
	}
	if strings.Contains(cause, "next start") {
		t.Errorf("a driver with no state directory promises a restart it cannot make: %q", cause)
	}

	// With a state directory the service does relaunch what it has a record of.
	withState := newStateDriver(t, t.TempDir())
	t.Cleanup(func() { _ = withState.Shutdown() })
	if got := withState.cwdMoved(srv); !strings.Contains(got, "next start relaunches it") {
		t.Errorf("with a state directory the cause should name the restart that fixes it: %q", got)
	}
}

func TestCwdMoved_SilentWhenNothingMoved(t *testing.T) {
	d, _ := newIsolatedDriver(t)

	// A directory nobody moved, given by the path t.TempDir returned (which on
	// macOS is not its real path).
	plain := t.TempDir()
	if got := d.cwdMoved(noted(t, d, plain)); got != "" {
		t.Errorf("an unmoved directory is reported: %q", got)
	}

	// A working directory that is a link by design.
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this host cannot make a symbolic link: %v", err)
	}
	if got := d.cwdMoved(noted(t, d, link)); got != "" {
		t.Errorf("a working directory that is a link by design is reported: %q", got)
	}
}

func TestCwdMoved_AGoneDirectorySaysSoWithoutPromisingARestart(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	m := newMoveable(t)
	srv := noted(t, d, m.cwd)
	m.rename(t) // and no link: the old name resolves to nothing

	cause := d.cwdMoved(srv)
	if !strings.Contains(cause, "no longer resolves") || !strings.Contains(cause, quoted(srv.cwdPlanned)) {
		t.Errorf("the cause does not say the directory is gone, and what the sandbox was planned for: %q", cause)
	}
	// A start would refuse a working directory that is not there.
	if strings.Contains(cause, "relaunch") || strings.Contains(cause, "restart") {
		t.Errorf("a directory that is gone is not fixed by a restart, and the cause says so: %q", cause)
	}
}

func TestCwdMoved_NoPlanNoCheck(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	if got := d.cwdMoved(nil); got != "" {
		t.Errorf("no server: %q", got)
	}
	if got := d.cwdMoved(&server{}); got != "" {
		t.Errorf("a server with no sandbox plan: %q", got)
	}
	// A host whose grant follows the directory itself has nothing to compare.
	srv := &server{}
	noteWorkdir(srv, &sandboxPlan{mechanism: sandbox.MechanismLinux, cwd: "/planned"}, "/given")
	if srv.cwdGiven != "" || srv.cwdPlanned != "" {
		t.Errorf("recorded a working directory to check on a host whose grant follows the directory: %+v", srv)
	}
	noteWorkdir(srv, nil, "/given")
	if srv.cwdGiven != "" {
		t.Errorf("recorded a working directory for a session with no plan: %+v", srv)
	}
}

func TestExplainReadFailure_KeepsTheKindAndNeverWrapsTwice(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	m := newMoveable(t)
	srv := noted(t, d, m.cwd)
	m.rename(t)
	m.link(t)

	refusal := &fleet.Error{Kind: fleet.ErrorInvalid, Message: "opencode: GET /session/status: 500 boom", Machine: "test-machine", Retryable: false}
	got := d.explainReadFailure(srv, refusal)
	var fe *fleet.Error
	if !errors.As(got, &fe) || fe.Kind != refusal.Kind || fe.Machine != refusal.Machine || fe.Retryable != refusal.Retryable {
		t.Fatalf("the explained error changed shape: %#v", got)
	}
	if !strings.Contains(fe.Message, "moved") || !strings.Contains(fe.Message, refusal.Message) {
		t.Errorf("the message must carry the cause and the runtime's own text: %q", fe.Message)
	}
	if refusal.Message != "opencode: GET /session/status: 500 boom" {
		t.Error("the original error was edited in place")
	}
	if again := d.explainReadFailure(srv, got); !reflect.DeepEqual(again, got) {
		t.Errorf("explaining twice wraps twice: %q", again)
	}

	// No answer at all, or a credential problem, is not a move.
	for _, kind := range []fleet.ErrorKind{fleet.ErrorUnreachable, fleet.ErrorUnauthorized} {
		e := &fleet.Error{Kind: kind, Message: "x"}
		if d.explainReadFailure(srv, e) != error(e) {
			t.Errorf("a %s error was blamed on a moved directory", kind)
		}
	}
	if err := errors.New("plain"); d.explainReadFailure(srv, err) != err {
		t.Error("an error that is not the runtime's refusal was changed")
	}
}

// ---- what a caller sees -----------------------------------------------------

// movedAndBroken is two sessions on an isolated driver, the second planned for a
// directory that is then moved behind a link while its runtime fails every
// request; the first is left alone. Planning is by hand, as the profile is not
// enforced on every host.
func movedAndBroken(t *testing.T) (d *Driver, healthy, broken fleet.Session, planned, newReal string) {
	t.Helper()
	requireRenameableWorkdir(t)
	d, _ = newIsolatedDriver(t)
	healthy = mustCreate(t, d, "ok", fleet.SessionSpec{})
	m := newMoveable(t)
	broken = mustCreate(t, d, "moved", fleet.SessionSpec{Cwd: fleet.AbsolutePath(m.cwd)})
	known, ok := d.wasSeen(broken.ID)
	if !ok {
		t.Fatal("the session is not known")
	}
	noteWorkdir(known.srv, planFor(t, d, m.cwd), m.cwd)
	planned = known.srv.cwdPlanned
	newReal = m.rename(t)
	m.link(t)
	breakServer(t, m.cwd) // through the link: the session's own process is still in its directory
	return d, healthy, broken, planned, newReal
}

func TestList_AMovedWorkdirIsUnknownWithTheCause(t *testing.T) {
	d, healthy, broken, planned, newReal := movedAndBroken(t)

	check := func(t *testing.T, filter driver.ListFilter, byID bool) {
		t.Helper()
		col, err := d.List(context.Background(), testReq, filter)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var saw bool
		for _, s := range col.Items() {
			switch s.ID {
			case broken.ID:
				saw = true
				if s.State.Status != fleet.StatusUnknown {
					t.Errorf("the moved session is %q, want unknown", s.State.Status)
				}
				ev := s.State.Evidence
				for _, want := range []string{"the working directory moved", quoted(planned), quoted(newReal), "500"} {
					if !strings.Contains(ev, want) {
						t.Errorf("the evidence %q does not say %q", ev, want)
					}
				}
			case healthy.ID:
				if s.State.Status == fleet.StatusUnknown {
					t.Errorf("the session nobody moved is unknown: %s", s.State.Evidence)
				}
			}
		}
		if !saw {
			t.Fatal("the moved session is not listed")
		}
		if !byID {
			if srcs := col.Sources(); len(srcs) != 1 || srcs[0].Status != fleet.SourceDegraded {
				t.Errorf("Sources = %+v, want exactly one SourceDegraded", srcs)
			}
		}
	}
	t.Run("whole listing", func(t *testing.T) { check(t, driver.ListFilter{}, false) })
	t.Run("by id", func(t *testing.T) { check(t, driver.ListFilter{ID: broken.ID}, true) })
}

func TestState_AMovedWorkdirFailsWithTheCause(t *testing.T) {
	d, _, broken, planned, _ := movedAndBroken(t)

	_, err := d.State(context.Background(), testReq, broken.SessionRef)
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		t.Fatalf("State = %v, want the runtime's refusal as a *fleet.Error", err)
	}
	if fe.Kind != fleet.ErrorInvalid {
		t.Errorf("the kind changed to %q; the cause is added to the message, not to the contract", fe.Kind)
	}
	// The runtime's 500 is retryable on its own (muster #477); a moved directory is
	// not transient, so naming the cause takes that back.
	if fe.Retryable {
		t.Error("a moved working directory was marked retryable: asking again fails the same way")
	}
	for _, want := range []string{"the working directory moved", quoted(planned), "500"} {
		if !strings.Contains(fe.Message, want) {
			t.Errorf("the message %q does not say %q", fe.Message, want)
		}
	}
}

func TestList_ABrokenUnmovedSessionKeepsTheBareError(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	cwd := t.TempDir()
	sess := mustCreate(t, d, "s", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	known, _ := d.wasSeen(sess.ID)
	noteWorkdir(known.srv, planFor(t, d, cwd), cwd) // planned, and not moved
	breakServer(t, cwd)

	s, ok := listed(t, d)[sess.ID]
	if !ok {
		t.Fatal("the session is not listed")
	}
	const bare = "session identity is known but its status could not be read:"
	if s.State.Status != fleet.StatusUnknown || !strings.HasPrefix(s.State.Evidence, bare) || strings.Contains(s.State.Evidence, "moved") {
		t.Errorf("a failure that is not a move changed its evidence: %q", s.State.Evidence)
	}
	_, err := d.State(context.Background(), testReq, sess.SessionRef)
	if err == nil || strings.Contains(err.Error(), "moved") {
		t.Errorf("State = %v, want the runtime's own refusal and no cause", err)
	}
}

// ---- end to end, where the host enforces the profile ------------------------

// A real sandboxed session, a real rename: the profile refuses the old name before
// the restart and allows it after, and the session is reported with its cause in
// between. (The test runtime does not read the directory by itself, so the failing
// of every request is stood in for by breakServer, as in the other tests.)
func TestSandboxedSession_AMovedWorkdirIsReportedAndARestartPlansItAgain(t *testing.T) {
	state := t.TempDir()
	d := newStateDriver(t, state)
	requireSandboxHost(t, d)
	t.Cleanup(func() { _ = d.Shutdown() })

	m := newMoveable(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(m.cwd), Sandbox: &fleet.SandboxSpec{}})
	if err := os.WriteFile(filepath.Join(m.cwd, "f"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := sh(t, d, sess.ID, "cat "+filepath.Join(m.cwd, "f")); r.Rc != 0 || r.Out != "kept" {
		t.Fatalf("before the move the session cannot read its own file: %+v", r)
	}

	newReal := m.rename(t)
	m.link(t)
	breakServer(t, m.cwd)

	// The premise: the profile planned for the old real path refuses the old name.
	if r := sh(t, d, sess.ID, "cat "+filepath.Join(m.cwd, "f")); r.Rc == 0 {
		t.Fatalf("the profile allowed the old name after the move; the failure this test is about did not happen: %+v", r)
	}
	s := listed(t, d)[sess.ID]
	if s.State.Status != fleet.StatusUnknown || !strings.Contains(s.State.Evidence, "the working directory moved") {
		t.Fatalf("the moved session is %q (%q), want unknown with the moved-directory cause", s.State.Status, s.State.Evidence)
	}

	// The service stops and starts again over the same state: every session it has
	// a record of is relaunched under a plan made from where its directory is now.
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(newReal, ".fake-fail-all")); err != nil {
		t.Fatal(err)
	}
	d2, _ := restartWith(t, state, 0)
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatal("the session is not listed after the restart")
	}
	if got.State.Status == fleet.StatusUnknown {
		t.Fatalf("the session is still unknown after the restart: %s", got.State.Evidence)
	}
	if got.State.Sandbox == nil || !hasPath(pathsOf(got.State.Sandbox), newReal) {
		t.Errorf("the restarted session's sandbox does not cover the directory's real path %q: %+v", newReal, got.State.Sandbox)
	}
	if r := sh(t, d2, sess.ID, "cat "+filepath.Join(m.cwd, "f")); r.Rc != 0 || r.Out != "kept" {
		t.Errorf("after the restart the old name still cannot be read: %+v", r)
	}
}

// pathsOf is every path the report says the session may write.
func pathsOf(st *fleet.SandboxState) []string {
	var out []string
	for _, p := range st.WritePaths {
		out = append(out, string(p))
	}
	return out
}
