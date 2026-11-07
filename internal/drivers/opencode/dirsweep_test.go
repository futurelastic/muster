package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// muster #419: the sweep of scoped directories no record names also runs while the
// service is up, not only at start-up.

// liveFixture lays out the state directory the driver is about to be built over, so
// directories can be added to it after the driver exists.
func liveFixture(t *testing.T) (*scopedFixture, string) {
	t.Helper()
	stateDir := t.TempDir()
	f := &scopedFixture{t: t, now: time.Now()}
	f.storeDir, f.root = stateLayout(stateDir)
	for _, d := range []string{f.storeDir, f.root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return f, stateDir
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func TestDirSweep_RemovesWhatLostItsOwnerWhileTheServiceRuns(t *testing.T) {
	f, stateDir := liveFixture(t)
	d := newStateDriver(t, stateDir, withDirSweep(20*time.Millisecond, serversHolding("muster-opencode-2")))
	t.Cleanup(func() { _ = d.Shutdown() })

	// All of these appear after start-up, so only the timer can see them.
	old := 3 * time.Hour
	orphan := f.dir("muster-opencode-1", old, remnantXDG)
	held := f.dir("muster-opencode-2", old, remnantXDG)
	// The record is written before the directory it names exists. The other way
	// round there is a gap in which the directory is old and no record names it, and
	// the timer, ticking every 20 ms, removes it if the test goroutine is descheduled
	// there (muster #424). Written first, there is no moment at which it can be seen
	// as an orphan.
	namedDir := filepath.Join(f.root, "muster-opencode-3")
	f.record("ses_named", namedDir)
	named := f.dir("muster-opencode-3", old, remnantXDG)
	store := f.dir("muster-opencode-4", old, withStore)
	recent := f.dir("muster-opencode-5", 5*time.Minute, remnantXDG)
	scratch := f.dir("muster-opencode-6", old, remnantTmp)

	waitFor(t, "the timer to remove the orphans", func() bool { return gone(orphan) && gone(scratch) })
	// And, a few ticks later, still nothing it should not have touched.
	time.Sleep(100 * time.Millisecond)
	for name, p := range map[string]string{"held by a server": held, "named by a record": named,
		"holding a store": store, "written recently": recent} {
		if gone(p) {
			t.Errorf("a directory %s was removed", name)
		}
	}
}

func TestDirSweep_StopsWithShutdownAndWaitsForTheLoop(t *testing.T) {
	f, stateDir := liveFixture(t)
	d := newStateDriver(t, stateDir, withDirSweep(10*time.Millisecond, noServers))

	if d.sweepDone == nil {
		t.Fatal("no sweep goroutine was started for a driver with a state directory")
	}
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.sweepDone:
	default:
		t.Fatal("Shutdown returned while the sweep goroutine was still running")
	}
	// A directory that appears after Shutdown is not swept, and a second Shutdown is harmless.
	late := f.dir("muster-opencode-1", 3*time.Hour, remnantXDG)
	time.Sleep(100 * time.Millisecond)
	if gone(late) {
		t.Error("a directory was removed after Shutdown")
	}
	if err := d.Shutdown(); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestDirSweep_NotStartedWithoutAStateDirectoryOrWhenSwitchedOff(t *testing.T) {
	d := &Driver{}
	d.startDirSweep()
	if d.sweepStop != nil {
		t.Error("a sweep goroutine was started with no state directory")
	}
	root := t.TempDir()
	d = &Driver{store: &recordStore{dir: filepath.Join(root, "sessions")}, sessionRoot: filepath.Join(root, "dirs"), noStartSweep: true}
	d.startDirSweep()
	if d.sweepStop != nil {
		t.Error("a sweep goroutine was started although the sweep is switched off")
	}
}
