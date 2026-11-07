//go:build !windows

package privfs

import (
	"os"
	"path/filepath"
	"testing"
)

// lockedTree builds what a package cache leaves in a session's directory:
// directories at r-x holding files at r--, nested, and one directory with no
// access at all. A cleanup puts the modes back so the test's own temp
// directory can be removed whatever RemoveAll did.
func lockedTree(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not stop an unlink, so there is nothing to test")
	}
	dir := filepath.Join(t.TempDir(), "session")
	pkg := filepath.Join(dir, "tmp", "home", "data", "agents", "reviewer", "1.0.0")
	empty := filepath.Join(dir, "tmp", "home", "locked")
	for _, d := range []string{pkg, empty} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pkg, "agent.md"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "inner"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	// Innermost first, so each chmod can still reach its target.
	for _, d := range []string{
		pkg,
		filepath.Dir(pkg),
		filepath.Join(dir, "tmp", "home", "data", "agents"),
		filepath.Join(dir, "tmp", "home", "data"),
	} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(empty, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	return dir
}

// The measured case (muster #417): plain os.RemoveAll stops at the first
// read-only directory.
func TestRemoveAll_ATreeWithReadOnlyDirectoriesAndFilesIsRemoved(t *testing.T) {
	dir := lockedTree(t)
	if err := RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory survived: %v", err)
	}
}

func TestRemoveAll_TheStandardLibraryCannotRemoveTheSameTree(t *testing.T) {
	dir := lockedTree(t)
	if err := os.RemoveAll(dir); err == nil {
		t.Fatal("os.RemoveAll removed a tree of read-only directories: the other tests here prove nothing")
	}
}

// A link inside the tree is removed as a link. Its target — a read-only
// directory outside — keeps its mode and its contents.
func TestRemoveAll_NeverFollowsASymlinkOutOfTheTree(t *testing.T) {
	dir := lockedTree(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	// The link sits in a read-only directory, so it is only reachable through
	// the permission fix.
	linkDir := filepath.Join(dir, "tmp", "home", "data")
	if err := os.Chmod(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(linkDir, 0o500); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory survived: %v", err)
	}
	fi, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("the link's target is gone: %v", err)
	}
	if fi.Mode().Perm() != 0o500 {
		t.Errorf("the link's target is now mode %#o: the walk followed the link, want 0500", fi.Mode().Perm())
	}
	if fi, err := os.Stat(keep); err != nil || fi.Mode().Perm() != 0o400 {
		t.Errorf("the file beyond the link was touched: %v / %v", fi, err)
	}
}

// The path itself being a link: the link goes, what it names stays.
func TestRemoveAll_ALinkAtThePathIsRemovedNotItsTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(link); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "f")); err != nil {
		t.Errorf("the link's target lost its contents: %v", err)
	}
}
