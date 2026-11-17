//go:build unix

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
)

// muster #436: a close sets the session's directory aside and has it deleted in the
// background, so the close does not spend its time on a package cache.

func scopedIn(t *testing.T, root string) (scoped, aside []string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch {
		case scopedDirName.MatchString(e.Name()):
			scoped = append(scoped, e.Name())
		case strings.HasPrefix(e.Name(), privfs.DiscardPrefix):
			aside = append(aside, e.Name())
		}
	}
	return scoped, aside
}

// With a state directory a live session's close leaves no session directory behind —
// the path it ran in is gone when Close returns — and what it set aside is deleted
// afterwards, in full, with a deep tree in it.
func TestClose_SetsTheDirectoryAsideAndItIsDeletedAfterwards(t *testing.T) {
	state := t.TempDir()
	d := newStateDriver(t, state)
	t.Cleanup(func() { _ = d.Shutdown() })
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "live"})
	k, ok := d.wasSeen(sess.ID)
	if !ok || k.srv == nil {
		t.Fatalf("seen = %v, srv = %v; want a live session", ok, k.srv)
	}
	dir := k.srv.dir
	// A module cache's shape: many small files below a few levels.
	for i := 0; i < 300; i++ {
		writeIn(t, dir, "home/go/pkg/mod/example.org/m@v1/"+strings.Repeat("d/", i%4)+"f"+string(rune('a'+i%26))+string(rune('a'+i/26)), 64)
	}

	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the session directory is still at its path after Close: %v", err)
	}
	settleRemovals(t, state)
}

// Without a state directory nothing would finish a directory left half deleted by a
// stop, so a close deletes before it returns and sets nothing aside.
func TestClose_WithoutAStateDirectoryDeletesBeforeItReturns(t *testing.T) {
	d, root := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	k, _ := d.wasSeen(sess.ID)
	writeIn(t, k.srv.dir, "home/.cache/x", 10)

	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("the session root holds %v straight after Close, want nothing", entries)
	}
}

// A directory set aside is not an orphan: the sweep that lists session directories
// no record names never sees it, so it is not counted, not removed by that path, and
// not mistaken for a session's data.
func TestOrphanDirs_DoNotCountADirectorySetAside(t *testing.T) {
	state := t.TempDir()
	_, root := stateLayout(state)
	aside := filepath.Join(root, privfs.DiscardPrefix+"muster-opencode-5-abc")
	if err := os.MkdirAll(filepath.Join(aside, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Hour)
	if err := os.Chtimes(aside, old, old); err != nil {
		t.Fatal(err)
	}
	rep, err := orphanDirReport(filepath.Join(filepath.Dir(root), "sessions"), root, func(string) ([]serverProc, int, error) { return nil, 0, nil })
	if err != nil {
		t.Fatalf("orphanDirReport: %v", err)
	}
	if rep.Total() != 0 {
		t.Errorf("a directory set aside was counted as an orphan: %+v", rep)
	}
}

// A restart finishes what a stopped service left set aside, says so, and does it
// without holding up the start.
func TestOpen_FinishesTheRemovalAnEarlierRunSetAside(t *testing.T) {
	state := t.TempDir()
	storeDir, root := stateLayout(state)
	for _, d := range []string{storeDir, root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	aside := filepath.Join(root, privfs.DiscardPrefix+"muster-opencode-5-abc")
	if err := os.MkdirAll(filepath.Join(aside, "home", "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aside, "home", "go", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged logLines
	d := newStateDriver(t, state, WithLogf(logged.add))
	t.Cleanup(func() { _ = d.Shutdown() })

	if !privfs.WaitDiscards(30 * time.Second) {
		t.Fatal("the deletion did not finish")
	}
	if _, err := os.Lstat(aside); !os.IsNotExist(err) {
		t.Errorf("the directory set aside by the earlier run survived the start: %v", err)
	}
	if !logged.has("finishing the removal", "set aside") {
		t.Errorf("no log line says the removal was resumed: %q", logged.all())
	}
}
