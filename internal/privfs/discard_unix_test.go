//go:build !windows

package privfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// muster #417 again, through the slow path: a tree a package cache locked — r-x
// directories holding r-- files, and a directory with no access at all — is deleted
// in full, with nothing made writable ahead of it.
func TestDiscard_ATreeAPackageCacheLockedIsDeletedInFull(t *testing.T) {
	dir := lockedTree(t)
	parent := filepath.Dir(dir)
	d := unpaced()
	if err := d.discard(dir); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the path is still there: %v", err)
	}
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("the locked tree was not deleted in full: %v", left)
	}
	if _, failed, _ := d.resume(parent); len(failed) != 0 {
		t.Errorf("failures recorded: %v", failed)
	}
}

// No link is followed: one inside the tree is unlinked as a link, and what it points
// at, outside the tree, is left whole — files and the directory itself.
func TestDiscard_NeverFollowsALinkOutOfTheTree(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(outside, "precious")
	if err := os.WriteFile(precious, []byte("keep"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil { // a delete that followed the link would fail on this, or chmod it
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })

	parent := t.TempDir()
	dir := filepath.Join(parent, "session")
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "home", "to-outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(precious, filepath.Join(dir, "to-file")); err != nil {
		t.Fatal(err)
	}

	d := unpaced()
	if err := d.discard(dir); err != nil {
		t.Fatal(err)
	}
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if b, err := os.ReadFile(precious); err != nil || string(b) != "keep" {
		t.Errorf("what the link pointed at was touched: %q, %v", b, err)
	}
	if fi, err := os.Stat(outside); err != nil || fi.Mode().Perm() != 0o500 {
		t.Errorf("the directory the link pointed at was changed: %v, %v", fi, err)
	}
}

// A path that is itself a link is unlinked and its target is left, as RemoveAll does.
func TestDiscard_APathThatIsALinkIsUnlinkedNotFollowed(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := unpaced().discard(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link survived: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the target was removed: %v", err)
	}
}

// Where the directory cannot be set aside (its parent is not writable) Discard is
// RemoveAll, error and all: a caller that told its own caller about a failed removal
// still can.
func TestDiscard_FallsBackToRemoveAllAndItsErrorWhereItCannotSetAside(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not stop a rename")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "session")
	if err := os.MkdirAll(filepath.Join(dir, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	if err := unpaced().discard(dir); err == nil {
		t.Error("Discard under an unwritable parent = nil, want RemoveAll's error")
	}
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("something was set aside under an unwritable parent: %v", left)
	}
}

// A deletion that fails later is kept, and the next ResumeDiscards reports it and
// tries again — which, once what stopped it is gone, finishes it. Here the parent is
// unwritable, so the tree empties and the last rmdir is refused.
func TestResumeDiscards_ReportsAFailedDeletionAndRetriesIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not stop an unlink")
	}
	d := unpaced()
	parent := t.TempDir()
	aside := filepath.Join(parent, DiscardPrefix+"muster-opencode-9-zz")
	if err := os.MkdirAll(filepath.Join(aside, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aside, "x", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	d.add(aside)
	settle(t, d)
	if _, err := os.Lstat(aside); err != nil {
		t.Fatalf("the directory went although its parent is unwritable: %v", err)
	}

	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	queued, failed, err := d.resume(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].Path != aside || failed[0].Err == nil {
		t.Errorf("failed = %+v, want the one directory and its error", failed)
	}
	if queued != 1 {
		t.Errorf("queued = %d, want the failed directory queued again", queued)
	}
	settle(t, d)
	if _, err := os.Lstat(aside); !os.IsNotExist(err) {
		t.Errorf("the retry did not finish the deletion: %v", err)
	}
	if _, failed, _ := d.resume(parent); len(failed) != 0 {
		t.Errorf("a finished deletion is still reported: %v", failed)
	}
}

// A tool can lock the session's own top directory too (r-x), and on macOS such a
// directory cannot be renamed as it is. It is still set aside, not deleted in the
// caller's thread: the tree was not made writable first, only its top was.
func TestDiscard_SetsAsideADirectoryItsOwnerCannotWriteTo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not stop a rename")
	}
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	d := newDiscarder(pacing{rate: 1, burst: 1, sleep: func(time.Duration) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
	}})
	parent := t.TempDir()
	dir := aTree(t, parent, 20)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, a := range asideIn(t, parent) {
			_ = os.Chmod(a, 0o700)
		}
	})

	if err := d.discard(dir); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	<-reached // the deleter is still working: this did not fall back to a synchronous removal
	if len(asideIn(t, parent)) != 1 {
		t.Errorf("set aside = %v, want the one directory, still being deleted", asideIn(t, parent))
	}
	close(release)
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}
