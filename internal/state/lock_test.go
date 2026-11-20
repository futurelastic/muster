package state

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// holdEnv makes this test binary hold a directory's lock until it is killed, so a
// test can have a second process own it (muster #507).
const holdEnv = "MUSTER_TEST_HOLD_STATE_DIR"

// helperLock keeps the helper's lock reachable, so it is not closed under it.
var helperLock *Lock

// sleepEnv makes this test binary do nothing until it is killed: the stand-in for a
// runtime server the service started and a crash then left running.
const sleepEnv = "MUSTER_TEST_SLEEP"

// childEnv makes the holder start such a process after it holds the directory.
const childEnv = "MUSTER_TEST_HOLD_STARTS_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(sleepEnv) != "" {
		select {}
	}
	if dir := os.Getenv(holdEnv); dir != "" {
		l, err := Acquire(dir)
		if err != nil {
			println("hold: " + err.Error())
			os.Exit(3)
		}
		helperLock = l
		if os.Getenv(childEnv) != "" {
			c := exec.Command(os.Args[0])
			c.Env = append(os.Environ(), sleepEnv+"=1")
			if err := c.Start(); err != nil {
				println("hold: " + err.Error())
				os.Exit(4)
			}
			println("child " + strconv.Itoa(c.Process.Pid))
		}
		println("held") // the parent waits for this line
		select {}       // until killed: a crash, as far as the lock is concerned
	}
	os.Exit(m.Run())
}

// A directory is owned by one process at a time. The second Acquire is refused with
// a *HeldError that says who has it, and refusing it leaves the first owner's lock
// whole.
func TestAcquire_ASecondOwnerIsRefusedAndTheFirstKeepsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()

	second, err := Acquire(dir)
	if err == nil {
		second.Release()
		t.Fatal("a second Acquire of an owned directory succeeded")
	}
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("refusal = %T %v, want *HeldError", err, err)
	}
	if held.Holder == nil || held.Holder.Pid != os.Getpid() {
		t.Errorf("the refusal does not name the owner: %+v", held.Holder)
	}
	if held.Holder != nil && held.Holder.Started.IsZero() {
		t.Error("the owner's start time was not recorded")
	}
	if msg := err.Error(); !strings.Contains(msg, dir) || !strings.Contains(msg, "pid") {
		t.Errorf("the message does not say which directory or whose: %q", msg)
	}

	// The refused attempt must not have taken anything away from the owner.
	if _, err := Acquire(dir); err == nil {
		t.Error("the owner lost the directory to a refused attempt")
	}
}

// Releasing gives the directory back at once, and a Release that is repeated, or
// made on no Lock, is harmless.
func TestAcquire_ReleaseGivesTheDirectoryBack(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("a second Release: %v", err)
	}
	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	defer again.Release()

	var none *Lock
	if err := none.Release(); err != nil {
		t.Errorf("Release on a nil Lock: %v", err)
	}
}

// No directory configured is nothing to own: the service runs in memory, and two of
// them may.
func TestAcquire_NoDirectoryIsNothingToOwn(t *testing.T) {
	for i := 0; i < 2; i++ {
		l, err := Acquire("")
		if err != nil || l != nil {
			t.Fatalf("Acquire(\"\") = %v, %v; want nil, nil", l, err)
		}
	}
}

// A lock file left by a service that is gone proves nothing: the lock is the
// operating system's, not the file's, so a start finds the directory free whatever
// the file says, and replaces the record with its own.
func TestAcquire_AStaleRecordIsNotAnOwner(t *testing.T) {
	dir := t.TempDir()
	stale := append([]byte{0}, `{"pid":999999,"started":"2020-01-01T00:00:00Z","exe":"/old/muster"}`+strings.Repeat(" ", 200)+"\n"...)
	if err := os.WriteFile(filepath.Join(dir, lockName), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire over a stale record: %v", err)
	}
	defer l.Release()

	// Read the way a refused start does, through a handle of its own and from where
	// the record begins: on Windows the locked byte cannot be read by another handle.
	f, err := os.Open(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := readHolder(f); got == nil || got.Pid != os.Getpid() {
		t.Errorf("the record still names someone else: %+v", got)
	}
	if fi, err := f.Stat(); err != nil || fi.Size() >= int64(len(stale)) {
		t.Errorf("the record was not cut to the new content: %v bytes, was %d (%v)", fi.Size(), len(stale), err)
	}
}

// Starting the lock must create the directory the way Open does, so the order a
// service calls them in does not matter.
func TestAcquire_CreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if _, err := Open(dir); err != nil {
		t.Errorf("Open after Acquire: %v", err)
	}
}

// The lock file is not a document: nothing the store loads or saves is named like
// it, and holding the lock does not get in the way of the store's own reads and
// writes of the same directory.
func TestAcquire_TheStoreStillWorksUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	type doc struct{ N int }
	if err := s.Save("x", doc{N: 7}); err != nil {
		t.Fatal(err)
	}
	var got doc
	if ok, err := s.Load("x", &got); err != nil || !ok || got.N != 7 {
		t.Fatalf("Load = %v, %v, %+v", ok, err, got)
	}
}

// The case the lock is for: another PROCESS owns the directory. It is refused with
// that process's pid, and when that process dies without releasing anything — a
// kill, a crash — the directory is free again with no cleanup.
func TestAcquire_AnotherProcessOwnsItUntilItDies(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), holdEnv+"="+dir)
	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	select {
	case l, ok := <-lines:
		if !ok || l != "held" {
			t.Fatalf("the holder said %q (ok=%v), want held", l, ok)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the holder never took the directory")
	}

	_, err = Acquire(dir)
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("Acquire against a live owner = %v, want *HeldError", err)
	}
	if held.Holder == nil || held.Holder.Pid != cmd.Process.Pid {
		t.Errorf("the refusal names %+v, want pid %d", held.Holder, cmd.Process.Pid)
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := Acquire(dir)
		if err == nil {
			defer l.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the directory is still owned %v after its owner was killed: %v", 10*time.Second, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// What the lock is for outlives nothing it should not: a process the owner started
// does not keep the directory owned after the owner is gone. This is the shape of the
// failure the lock exists to stop — a service killed while its runtime servers keep
// running — so a descriptor that leaked into a child would turn the next start's
// refusal into a permanent one.
func TestAcquire_AChildOfTheOwnerDoesNotKeepTheDirectory(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), holdEnv+"="+dir, childEnv+"=1")
	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	childPid := 0
	sc := bufio.NewScanner(out)
	deadline := time.After(20 * time.Second)
	lines := make(chan string, 8)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
ready:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the holder exited before it was ready")
			}
			if strings.HasPrefix(l, "child ") {
				childPid, _ = strconv.Atoi(strings.TrimPrefix(l, "child "))
			}
			if l == "held" {
				break ready
			}
		case <-deadline:
			t.Fatal("the holder never took the directory")
		}
	}
	if childPid == 0 {
		t.Fatal("the holder did not report its child")
	}
	if p, err := os.FindProcess(childPid); err == nil {
		t.Cleanup(func() { _ = p.Kill() })
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	wait := time.Now().Add(10 * time.Second)
	for {
		l, err := Acquire(dir)
		if err == nil {
			l.Release()
			return
		}
		if time.Now().After(wait) {
			t.Fatalf("the owner is gone but its child (pid %d) still keeps the directory: %v", childPid, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
