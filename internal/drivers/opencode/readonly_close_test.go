//go:build unix

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #417: a session's directory holds what its tools installed, and a
// package cache installs read-only. Removing the directory has to cope with
// that, or a close fails with "permission denied" and the session stays listed.

// lockedInside adds, under dir, what a package cache leaves: nested directories
// at r-x holding a file at r--. The modes are put back when the test ends so the
// test's own temp directory can go whatever the driver did.
func lockedInside(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not stop an unlink, so there is nothing to test")
	}
	pkg := filepath.Join(dir, "tmp", "tool-home", "data", "agents", "reviewer", "1.0.0")
	if err := os.MkdirAll(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "agent.md"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	// Innermost first, so each chmod can still reach its target.
	for d := pkg; d != filepath.Join(dir, "tmp"); d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
			if e != nil && e.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
}

// The measured case: the session is listed unknown (it carries caller-supplied
// environment, so the new service does not relaunch it) and its close was
// answered with the permission error, leaving it listed.
func TestClose_AnUnknownSessionWhoseDirectoryHoldsReadOnlyFilesIsClosed(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "withenv", Env: map[string]string{"MODEL_KEY": "a-value"}})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	k, ok := d2.wasSeen(sess.ID)
	if !ok || k.srv != nil || k.dir == "" {
		t.Fatalf("seen = %v, srv = %v, dir = %q; want an unknown session with a directory and no server of this service", ok, k.srv, k.dir)
	}
	lockedInside(t, k.dir)

	ack, err := d2.Close(context.Background(), testReq, sess.SessionRef)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !ack.Accepted {
		t.Error("Accepted = false")
	}
	if _, err := os.Lstat(k.dir); !os.IsNotExist(err) {
		t.Errorf("the session directory survived Close: %v", err)
	}
	if _, ok := d2.wasSeen(sess.ID); ok {
		t.Error("the session is still known after Close")
	}
	if left := recordFiles(t, state); len(left) != 0 {
		t.Errorf("records left after close: %v", left)
	}
}

// A live session's close removes the directory too, and used to drop the same
// error without a word: the close answered 202 and the directory stayed.
func TestClose_ALiveSessionWhoseDirectoryHoldsReadOnlyFilesLeavesNoDirectory(t *testing.T) {
	state := t.TempDir()
	d := newStateDriver(t, state)
	t.Cleanup(func() { _ = d.Shutdown() })
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "live"})
	k, ok := d.wasSeen(sess.ID)
	if !ok || k.srv == nil {
		t.Fatalf("seen = %v, srv = %v; want a live session", ok, k.srv)
	}
	dir := k.srv.dir
	lockedInside(t, dir)

	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the session directory survived Close: %v", err)
	}
}

// The start-up sweep logged the failure and skipped the directory on every
// start, so a record-less directory with read-only content was never removed.
func TestOpen_RemovesARemnantDirectoryHoldingReadOnlyFiles(t *testing.T) {
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
	remnant := f.dir("muster-opencode-31", old, remnantXDG)
	lockedInside(t, remnant)
	f.age(remnant, old) // writing the locked files moved the directory's own time

	var logged logLines
	d := newStateDriver(t, state, WithLogf(logged.add))
	t.Cleanup(func() { _ = d.Shutdown() })

	if _, err := os.Lstat(remnant); !os.IsNotExist(err) {
		t.Errorf("the remnant directory was not removed at start (%v); log: %q", err, logged.all())
	}
	if !logged.has("removed a session directory", "muster-opencode-31") {
		t.Errorf("no log line says the directory was removed: %q", logged.all())
	}
}
