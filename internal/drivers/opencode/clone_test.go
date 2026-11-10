//go:build unix

package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #433: the package-cache copy takes the create's context, and a copy
// that is cancelled leaves nothing behind. The two seams (cloneDir, copyBinary)
// stand in for the one call and the per-file copies, so each is cancelled at a
// moment the test chooses instead of one it hopes for.

// seedTree makes a small tree to copy and returns its real path.
func seedTree(t *testing.T) string {
	t.Helper()
	src := resolvePath(t.TempDir())
	if err := os.MkdirAll(filepath.Join(src, "go", "mod", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"top": "t", "go/mod/x/y": "y"} {
		if err := os.WriteFile(filepath.Join(src, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func useSeams(t *testing.T, clone func(src, dst string) error, cp string) {
	t.Helper()
	oc, ob := cloneDir, copyBinary
	t.Cleanup(func() { cloneDir, copyBinary = oc, ob })
	if clone != nil {
		cloneDir = clone
	}
	if cp != "" {
		copyBinary = cp
	}
}

func noClone(string, string) error { return errCloneUnsupported }

// A cancel that arrives before anything has started costs nothing: no call, no
// process, no directory.
func TestCopyTreeCancelledBeforeItStartsDoesNothing(t *testing.T) {
	calls := 0
	useSeams(t, func(string, string) error { calls++; return nil }, "/nonexistent/cp")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(t.TempDir(), "pkgcache")

	how, err := copyTree(ctx, seedTree(t), dst)
	if !errors.Is(err, context.Canceled) || how != "" {
		t.Fatalf("copyTree(cancelled) = %q, %v; want the context's own error", how, err)
	}
	if calls != 0 {
		t.Fatalf("the clone was attempted %d times for a copy nobody was waiting for", calls)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("a cancelled copy left %s behind: %v", dst, err)
	}
}

// The per-file copies are child processes: a cancel kills the one running and
// removes what it had written, and the answer comes back in moments, not at the
// copy's own pace.
func TestCopyTreeCancelKillsTheCopyAndLeavesNothing(t *testing.T) {
	work := t.TempDir()
	started := filepath.Join(work, "started")
	// A stand-in cp: makes the destination (its last argument), says so, and
	// then takes far longer than the test. exec, so the kill lands on the sleeper.
	script := filepath.Join(work, "slow-cp")
	body := "#!/bin/sh\nfor a; do last=\"$a\"; done\nmkdir -p \"$last/partial\"\n: > '" + started + "'\nexec sleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	useSeams(t, noClone, script)

	dst := filepath.Join(t.TempDir(), "pkgcache")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		how string
		err error
	}
	done := make(chan result, 1)
	go func() {
		how, err := copyTree(ctx, seedTree(t), dst)
		done <- result{how, err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in copy never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) || r.how != "" {
			t.Fatalf("copyTree = %q, %v; want the context's own error", r.how, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the copy did not stop with its context")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("a cancelled copy left %s behind: %v", dst, err)
	}
}

// One clone is a single call that cannot be interrupted. A cancel during it is
// honoured when the call returns, and the destination it made is removed before
// copyTree answers — so a caller that then removes the session directory is not
// racing a clone still filling it.
func TestCopyTreeCancelDuringTheCloneWaitsForItThenRemovesIt(t *testing.T) {
	inFlight, release := make(chan struct{}), make(chan struct{})
	useSeams(t, func(src, dst string) error {
		if err := os.MkdirAll(filepath.Join(dst, "half"), 0o755); err != nil {
			return err
		}
		close(inFlight)
		<-release
		return nil
	}, "/nonexistent/cp")

	dst := filepath.Join(t.TempDir(), "pkgcache")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		how string
		err error
	}
	done := make(chan result, 1)
	go func() {
		how, err := copyTree(ctx, seedTree(t), dst)
		done <- result{how, err}
	}()

	<-inFlight
	cancel()
	select {
	case r := <-done:
		t.Fatalf("copyTree answered (%q, %v) while the clone was still writing %s", r.how, r.err, dst)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) || r.how != "" {
			t.Fatalf("copyTree = %q, %v; want the context's own error", r.how, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("copyTree did not answer after the clone returned")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("a cancelled clone left %s behind: %v", dst, err)
	}
}

// A clone that works is reported as one and starts no process.
func TestCopyTreeOneCloneIsEnough(t *testing.T) {
	useSeams(t, func(src, dst string) error { return os.Mkdir(dst, 0o755) }, "/nonexistent/cp")
	how, err := copyTree(context.Background(), seedTree(t), filepath.Join(t.TempDir(), "pkgcache"))
	if err != nil || how != "clone" {
		t.Fatalf("copyTree = %q, %v; want clone", how, err)
	}
}

// A destination that exists is replaced, not copied into: `cp -cR src dst` makes
// dst/<name of src> when dst is there already, which is what a relaunch with no
// recorded copy would get after a start that was cancelled part-way.
func TestCopyTreeReplacesAStaleDestinationInsteadOfNestingInIt(t *testing.T) {
	useSeams(t, noClone, "")
	src := seedTree(t)
	dst := filepath.Join(t.TempDir(), "pkgcache")
	if err := os.MkdirAll(filepath.Join(dst, "left-over"), 0o755); err != nil {
		t.Fatal(err)
	}

	how, err := copyTree(context.Background(), src, dst)
	if err != nil || (how != "clone" && how != "copy") {
		t.Fatalf("copyTree = %q, %v", how, err)
	}
	for rel, want := range map[string]string{"top": "t", "go/mod/x/y": "y"} {
		if got, err := os.ReadFile(filepath.Join(dst, rel)); err != nil || string(got) != want {
			t.Fatalf("%s in the copy = %q, %v; want %q", rel, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "left-over")); !os.IsNotExist(err) {
		t.Fatalf("what an earlier attempt left is still in the copy: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, filepath.Base(src))); !os.IsNotExist(err) {
		t.Fatalf("the seed was copied INTO the destination, not made into it: %v", err)
	}
}

// When nothing can copy, the answer says why, and nothing is left.
func TestCopyTreeAllWaysFailLeavesNothingAndSaysWhy(t *testing.T) {
	useSeams(t, noClone, "/usr/bin/false")
	dst := filepath.Join(t.TempDir(), "pkgcache")
	how, err := copyTree(context.Background(), seedTree(t), dst)
	if err == nil || how != "" {
		t.Fatalf("copyTree = %q, %v; want a failure", how, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("a failure of the copy was reported as a cancellation: %v", err)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("a failed copy left %s behind: %v", dst, err)
	}
}

// planSandboxFor is where the create's context reaches the copy: a create whose
// caller has gone is refused with that, not with "copying the package cache
// failed", and the session's private cache is not there afterwards.
func TestPlanSandboxForStopsTheCopyWithItsContext(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	useSeams(t, noClone, "/nonexistent/cp")
	seed := managerSeed(t, fleet.PackageManagerGo)
	dir, cwd := t.TempDir(), t.TempDir()
	spec := fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Sandbox: &fleet.SandboxSpec{
		PackageCache: &fleet.SandboxPackageCache{Path: fleet.AbsolutePath(seed)},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.planSandboxFor(ctx, spec, dir, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("planSandboxFor(cancelled) = %v; want the context's own error", err)
	}
	var fe *fleet.Error
	if errors.As(err, &fe) && strings.Contains(fe.Message, "copying the package cache") {
		t.Fatalf("a cancelled create was reported as a failed copy: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "pkgcache")); !os.IsNotExist(err) {
		t.Fatalf("a cancelled plan left the session's cache behind: %v", err)
	}
}

// The create itself: a caller that gives up during the copy gets its own error
// back, and the session directory the create made is gone — no directory, no
// process, nothing for a restart to adopt.
func TestCreateCancelledDuringThePackageCacheCopyLeavesNoDirectory(t *testing.T) {
	d, root := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	inFlight, release := make(chan struct{}), make(chan struct{})
	useSeams(t, func(src, dst string) error {
		if err := os.MkdirAll(filepath.Join(dst, "half"), 0o755); err != nil {
			return err
		}
		close(inFlight)
		<-release
		return nil
	}, "/nonexistent/cp")
	seed := managerSeed(t, fleet.PackageManagerGo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := d.Create(ctx, testReq, "k-cancelled", fleet.SessionSpec{
			Cwd: fleet.AbsolutePath(resolvePath(t.TempDir())),
			Sandbox: &fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{
				Path: fleet.AbsolutePath(seed), Managers: []fleet.PackageManager{fleet.PackageManagerGo},
			}},
		})
		errc <- err
	}()

	<-inFlight
	cancel()
	close(release)
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create after its caller gave up = %v; want the context's own error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Create did not return after its context was cancelled")
	}
	if left, _ := filepath.Glob(filepath.Join(root, "*")); len(left) != 0 {
		t.Fatalf("a cancelled create left %v behind", left)
	}
}
