package privfs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock the test moves: sleeping advances it and nothing else does,
// so a minute of pacing is a few microseconds of test.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.slept += d
	c.mu.Unlock()
}

func (c *fakeClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slept
}

// unpaced is a deleter that never waits.
func unpaced() *discarder { return newDiscarder(pacing{}) }

func settle(t *testing.T, d *discarder) {
	t.Helper()
	if !d.wait(30 * time.Second) {
		t.Fatal("the deleter did not go idle")
	}
}

// aTree makes a directory holding n small files under a nested directory, so a
// deletion has both kinds of entry to meet.
func aTree(t *testing.T, root string, n int) string {
	t.Helper()
	dir := filepath.Join(root, "session")
	sub := filepath.Join(dir, "home", "go")
	if err := MkdirAll(sub); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := WriteFile(filepath.Join(sub, "f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func asideIn(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), DiscardPrefix) {
			out = append(out, filepath.Join(parent, e.Name()))
		}
	}
	return out
}

// muster #436, the point of it: the path is free the moment Discard returns, and
// the tree is still on disk, under a name that says what it is, until the deleter
// gets to it. The deleter is held here by a sleep that does not return.
func TestDiscard_FreesThePathAtOnceAndDeletesTheTreeAfterwards(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce) // a failed assertion must not leave the deleter blocked
	d := newDiscarder(pacing{rate: 1, burst: 1, sleep: func(time.Duration) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
	}})
	parent := t.TempDir()
	dir := aTree(t, parent, 20)

	if err := d.discard(dir); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the path is still there after Discard: %v", err)
	}
	<-reached // the deleter is working, and waiting
	aside := asideIn(t, parent)
	if len(aside) != 1 || !strings.Contains(filepath.Base(aside[0]), "session") {
		t.Fatalf("set-aside directories = %v, want one named after the session directory", aside)
	}
	if _, err := os.Stat(filepath.Join(aside[0], "home", "go")); err != nil {
		t.Errorf("the tree was not intact while the deleter waited: %v", err)
	}
	// The name is free for a new session directory straight away.
	if err := MkdirAll(dir); err != nil {
		t.Errorf("the name was not free to use again: %v", err)
	}

	releaseOnce()
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("the set-aside directory was not deleted: %v", left)
	}
}

// The deleter's rate is a rate: with the burst spent, n entries cost about
// (n - burst) / rate seconds, in whatever clock it is given.
func TestDiscard_PacesTheDeletionAtItsRate(t *testing.T) {
	clk := newFakeClock()
	d := newDiscarder(pacing{rate: 100, burst: 100, now: clk.now, sleep: clk.sleep})
	parent := t.TempDir()
	dir := aTree(t, parent, 1000)

	if err := d.discard(dir); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// 1000 files, 2 directories inside the tree (home, go), and the tree's own rmdir:
	// 1003 entries, of which the first 100 are the burst.
	got, want := clk.total(), 9*time.Second
	if got < want-500*time.Millisecond || got > want+500*time.Millisecond {
		t.Errorf("a 1003 entry tree at 100 a second with a burst of 100 slept %v, want about %v", got, want)
	}
}

// A small tree is under the burst and costs no sleep at all: nearly every directory
// a session leaves is one, and none of them should be slowed.
func TestDiscard_ASmallTreeIsNotSlowed(t *testing.T) {
	clk := newFakeClock()
	d := newDiscarder(pacing{rate: 5000, burst: 500, now: clk.now, sleep: clk.sleep})
	dir := aTree(t, t.TempDir(), 40)
	if err := d.discard(dir); err != nil {
		t.Fatal(err)
	}
	settle(t, d)
	if clk.total() != 0 {
		t.Errorf("a 40 file tree slept %v, want 0", clk.total())
	}
}

func TestDiscard_APathThatIsNotThereIsNotAnError(t *testing.T) {
	if err := unpaced().discard(filepath.Join(t.TempDir(), "never-made")); err != nil {
		t.Errorf("Discard of a missing path = %v, want nil", err)
	}
}

// Only a directory is worth setting aside; a file is one unlink.
func TestDiscard_AFileIsRemovedAtOnce(t *testing.T) {
	d := unpaced()
	parent := t.TempDir()
	f := filepath.Join(parent, "f")
	if err := WriteFile(f, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.discard(f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f); !os.IsNotExist(err) {
		t.Errorf("the file survived: %v", err)
	}
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("a file was set aside: %v", left)
	}
}

// What a stopped process leaves is finished by the next start, and nothing else in
// the directory is touched: the prefix is the only thing ResumeDiscards goes by, and
// the directory it is pointed at may be a shared one.
func TestResumeDiscards_FinishesWhatWasLeftAndNothingElse(t *testing.T) {
	d := unpaced()
	parent := t.TempDir()
	leftover := filepath.Join(parent, DiscardPrefix+"muster-opencode-1-abc")
	if err := MkdirAll(filepath.Join(leftover, "home")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(leftover, "home", "f"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	keep := []string{
		filepath.Join(parent, "muster-opencode-2"),     // a session directory
		filepath.Join(parent, ".removing-somebody"),    // someone else's hidden directory
		filepath.Join(parent, ".muster-removing-file"), // not a directory
	}
	for _, k := range keep[:2] {
		if err := MkdirAll(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteFile(keep[2], []byte("x")); err != nil {
		t.Fatal(err)
	}

	queued, failed, err := d.resume(parent)
	if err != nil {
		t.Fatalf("ResumeDiscards: %v", err)
	}
	if queued != 1 || len(failed) != 0 {
		t.Errorf("queued, failed = %d, %v; want 1 and none", queued, failed)
	}
	settle(t, d)
	if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
		t.Errorf("the leftover survived: %v", err)
	}
	for _, k := range keep {
		if _, err := os.Lstat(k); err != nil {
			t.Errorf("%s was touched: %v", filepath.Base(k), err)
		}
	}
}

func TestResumeDiscards_ADirectoryThatIsNotThereHasNothingToResume(t *testing.T) {
	queued, failed, err := unpaced().resume(filepath.Join(t.TempDir(), "no-such-dir"))
	if queued != 0 || failed != nil || err != nil {
		t.Errorf("= %d, %v, %v; want 0, nil, nil", queued, failed, err)
	}
}

// An entry that is queued or being deleted is not queued again, whether it is the
// hourly sweep asking or another driver in the same process: the same tree is not
// deleted twice, and the second asker does not see a deletion half done as a failure.
func TestDiscard_AnEntryAlreadyQueuedIsNotQueuedAgain(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce) // a failed assertion must not leave the deleter blocked
	d := newDiscarder(pacing{rate: 1, burst: 1, sleep: func(time.Duration) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
	}})
	parent := t.TempDir()
	if err := d.discard(aTree(t, parent, 10)); err != nil {
		t.Fatal(err)
	}
	<-reached // in flight

	for range 3 {
		queued, failed, err := d.resume(parent)
		if err != nil || queued != 0 || len(failed) != 0 {
			t.Fatalf("resume while the entry was being deleted = %d, %v, %v; want 0, none, nil", queued, failed, err)
		}
	}
	releaseOnce()
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if _, failed, _ := d.resume(parent); len(failed) != 0 {
		t.Errorf("a clean deletion was reported as a failure: %v", failed)
	}
}

// Under -race: closes and sweeps from several goroutines against the one deleter.
func TestDiscard_ManyCallersOneDeleter(t *testing.T) {
	d := unpaced()
	parent := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		dir := filepath.Join(parent, "s"+string(rune('a'+i)))
		if err := MkdirAll(filepath.Join(dir, "x")); err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(filepath.Join(dir, "x", "f"), []byte("x")); err != nil {
			t.Fatal(err)
		}
		wg.Add(2)
		go func() { defer wg.Done(); _ = d.discard(dir) }()
		go func() { defer wg.Done(); _, _, _ = d.resume(parent) }()
	}
	wg.Wait()
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// Deferred deletion needs a bound on the deferral. The background deleter gives way
// to everything else on the machine, and when closes arrive faster than it is let run
// a backlog would build until the disk filled with directories nobody uses; a backlog
// calls in a helper at normal priority. Here the background deleter is held, and the
// helper deletes what queued behind it.
func TestDiscard_ABacklogCallsInAHelper(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce) // a failed assertion must not leave the deleter blocked
	d := newDiscarder(pacing{rate: 1, burst: 3, backlog: 2, sleep: func(time.Duration) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
	}})
	parent := t.TempDir()
	if err := d.discard(aTree(t, parent, 30)); err != nil {
		t.Fatal(err)
	}
	<-reached // the background deleter has the big tree and is waiting on its pace

	for i := 0; i < 3; i++ {
		e := filepath.Join(parent, "e"+string(rune('0'+i)))
		if err := MkdirAll(e); err != nil {
			t.Fatal(err)
		}
		if err := d.discard(e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(asideIn(t, parent)) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	left := asideIn(t, parent)
	if len(left) != 1 || !strings.Contains(filepath.Base(left[0]), "session") {
		t.Fatalf("with the background deleter held, the set-aside directories are %v; want the helper to have deleted the three small ones and left the big one", left)
	}

	releaseOnce()
	settle(t, d)
	if left := asideIn(t, parent); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}
