//go:build darwin

package opencode

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/futurelastic/muster/internal/privfs"
)

// muster #433: the one call that makes the package-cache copy cheap is reached
// by a literal system call number, which nothing promises. These tests run on
// the hosted macOS runner (arm64) and on a developer's Mac (either), and they
// FAIL — not skip — when the host answers EINVAL or ENOSYS to the number, so a
// macOS that moves it, or an architecture that does not honour it, is found in
// CI and not as ninety seconds on every create.

// cloneFixture makes a tree with what a package cache has: nested directories, a
// read-only directory of read-only files (a Go module cache is all of that), and
// a link.
func cloneFixture(t *testing.T) (src, dst string) {
	t.Helper()
	base := resolvePath(t.TempDir())
	src, dst = filepath.Join(base, "seed"), filepath.Join(base, "clone")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "mod", "x"), 0o755))
	must(os.WriteFile(filepath.Join(src, "top"), []byte("top"), 0o644))
	must(os.WriteFile(filepath.Join(src, "mod", "x", "ro.go"), []byte("package x"), 0o444))
	must(os.Chmod(filepath.Join(src, "mod", "x"), 0o555))
	must(os.Symlink("top", filepath.Join(src, "link")))
	// Registered after TempDir's own cleanup, so it runs first: a plain
	// os.RemoveAll cannot empty a directory it may not write to.
	t.Cleanup(func() { _ = privfs.RemoveAll(base) })
	return src, dst
}

func skipIfCannotClone(t *testing.T, err error) {
	t.Helper()
	switch {
	case err == nil:
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EXDEV):
		t.Skipf("this volume cannot clone (not a literal-number problem): %v", err)
	case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENOSYS):
		t.Fatalf("the host does not honour clonefileat as syscall %d: %v — copyTree would never take its fast path here", sysClonefileat, err)
	default:
		t.Fatalf("cloneDirectory: %v", err)
	}
}

func TestCloneDirectoryClonesATreeAsOneCall(t *testing.T) {
	src, dst := cloneFixture(t)
	skipIfCannotClone(t, cloneDirectory(src, dst))

	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		want, err := os.Lstat(p)
		if err != nil {
			return err
		}
		got, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("%s is missing from the clone: %v", rel, err)
			return nil
		}
		if got.Mode() != want.Mode() {
			t.Errorf("%s: mode %v in the clone, %v in the seed", rel, got.Mode(), want.Mode())
		}
		if got.Mode()&fs.ModeSymlink != 0 {
			if a, _ := os.Readlink(filepath.Join(dst, rel)); a != "top" {
				t.Errorf("%s: the link was followed or rewritten: %q", rel, a)
			}
		} else if got.Mode().IsRegular() {
			a, _ := os.ReadFile(p)
			b, err := os.ReadFile(filepath.Join(dst, rel))
			if err != nil || string(a) != string(b) {
				t.Errorf("%s: %q in the clone, %q in the seed (%v)", rel, b, a, err)
			}
		}
		if st, ok := got.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			t.Errorf("%s belongs to %d, not to the caller", rel, st.Uid)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// What copyTree relies on: a clone does not replace, and a seed that is not
// there is a plain ENOENT — the answer a call that IS clonefileat gives.
func TestCloneDirectoryRefusesAnExistingDestinationAndAMissingSeed(t *testing.T) {
	src, dst := cloneFixture(t)
	skipIfCannotClone(t, cloneDirectory(src, dst))
	if err := cloneDirectory(src, dst); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("a clone onto an existing destination = %v; want EEXIST", err)
	}
	if err := cloneDirectory(filepath.Join(filepath.Dir(src), "no-such-seed"), filepath.Join(filepath.Dir(src), "other")); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("a clone of a seed that is not there = %v; want ENOENT", err)
	}
}

// With the real call and no cp to fall back on, a host that can clone makes the
// session's copy as one clone.
func TestCopyTreeTakesTheOneCallPathOnAHostThatCanClone(t *testing.T) {
	src, dst := cloneFixture(t)
	probe := filepath.Join(filepath.Dir(src), "probe")
	skipIfCannotClone(t, cloneDirectory(src, probe))
	useSeams(t, nil, "/nonexistent/cp")

	how, err := copyTree(context.Background(), src, dst)
	if err != nil || how != "clone" {
		t.Fatalf("copyTree = %q, %v; want clone without a single cp", how, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "top")); err != nil || string(b) != "top" {
		t.Fatalf("the copy's top = %q, %v", b, err)
	}
}
