package opencode

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #419: the size of a session's scratch directory is part of its state.
// muster #431: it is the size of the whole directory, split by area, and it does
// not depend on the session being alive.

// writeIn writes n bytes at rel, relative to the session's directory (so "tmp/a",
// "home/.cache/x", "xdg/data/y" or a bare "stray" at the top).
func writeIn(t *testing.T, dir string, rel string, n int) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeScratch writes into tmp/, where most of these tests put their files.
func writeScratch(t *testing.T, dir string, rel string, n int) {
	t.Helper()
	writeIn(t, dir, "tmp/"+rel, n)
}

// onDisk is the test's own answer to "how big is this": the sizes of the regular
// files under root, no link followed. It shares nothing with the driver's walk, so
// the two agreeing means something. A fresh session's directory is not empty (it
// holds the session's own configuration), so tests compare against this rather than
// against a number they typed.
func onDisk(t *testing.T, root string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(root, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			info, ierr := e.Info()
			if ierr != nil {
				return ierr
			}
			n += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("measuring %s by hand: %v", root, err)
	}
	return n
}

func scratchSession(t *testing.T, lim scratchLimits, extra ...Option) (*Driver, fleet.Session, string) {
	t.Helper()
	d, _ := newIsolatedDriver(t, append([]Option{withScratchLimits(lim)}, extra...)...)
	sess := mustCreate(t, d, "k-scratch", fleet.SessionSpec{})
	k, ok := d.wasSeen(sess.ID)
	if !ok || k.srv == nil || k.srv.dir == "" {
		t.Fatalf("seen = %v, srv = %v; want a live session with a directory", ok, k.srv)
	}
	return d, sess, k.srv.dir
}

// stateScratch reads the session's state and returns its scratch figure, failing the
// test when there is none.
func stateScratch(t *testing.T, d *Driver, sess fleet.Session) *fleet.ScratchUsage {
	t.Helper()
	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Scratch == nil {
		t.Fatalf("State (status %s) carries no scratch figure", st.Status)
	}
	return st.Scratch
}

// sumOf is what Bytes has to equal: the areas added up.
func sumOf(areas map[string]int64) int64 {
	var n int64
	for _, b := range areas {
		n += b
	}
	return n
}

func TestState_ReportsTheSizeOfTheSessionsScratchDirectory(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{inline: true})
	writeScratch(t, dir, "a", 1000)
	writeScratch(t, dir, "deep/er/b", 2000)
	// A link is not followed: a file outside the directory is not scratch.
	outside := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(outside, make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "tmp", "link")); err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}

	got := stateScratch(t, d, sess)
	if got.Areas["tmp"] != 3000 || got.Partial {
		t.Errorf("scratch = %+v, want 3000 bytes in tmp, not partial", *got)
	}
	if age := time.Since(got.AsOf); age < 0 || age > time.Minute {
		t.Errorf("asOf = %s (%s ago), want the moment of the measurement", got.AsOf, age)
	}

	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items := col.Items()
	if len(items) != 1 || items[0].State.Scratch == nil || items[0].State.Scratch.Areas["tmp"] != 3000 {
		t.Errorf("List scratch = %+v, want 3000 bytes in tmp", items)
	}
}

// muster #431, first Done-when: the figure is the whole directory, and says where
// the bytes sit.
func TestState_ReportsTheWholeScratchDirectoryAndEachArea(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{inline: true})
	writeIn(t, dir, "tmp/build/out.o", 1000)
	writeIn(t, dir, "home/.cache/go-build/ab/cd", 20_000)
	writeIn(t, dir, "home/.cargo/registry/x", 300)
	writeIn(t, dir, "xdg/cache/npm/pkg.tgz", 4_000)
	writeIn(t, dir, "stray", 55) // outside the three areas
	// A link at the top of the directory is not followed either.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "big"), make([]byte, 9_000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}

	want := map[string]int64{
		"tmp":   onDisk(t, filepath.Join(dir, "tmp")),
		"home":  onDisk(t, filepath.Join(dir, "home")),
		"xdg":   onDisk(t, filepath.Join(dir, "xdg")),
		"other": 55,
	}
	// The oracle must have seen what was written, or the comparison below is
	// two zeros agreeing.
	if want["tmp"] != 1000 || want["home"] != 20_300 || want["xdg"] < 4_000 {
		t.Fatalf("hand-measured areas = %v, want tmp 1000, home 20300, xdg at least 4000", want)
	}

	got := stateScratch(t, d, sess)
	if !reflect.DeepEqual(got.Areas, want) {
		t.Errorf("areas = %v, want %v", got.Areas, want)
	}
	if total := onDisk(t, dir); got.Bytes != total || got.Bytes != sumOf(got.Areas) {
		t.Errorf("bytes = %d, want the whole directory's %d, which is also the areas' sum %d", got.Bytes, total, sumOf(got.Areas))
	}
	if got.Partial {
		t.Errorf("scratch = %+v, want a complete figure", *got)
	}
}

// muster #431: the three areas are there when empty, and "other" is not.
func TestState_NamesTheAreasEvenWhenEmptyAndOtherOnlyWhenItHoldsSomething(t *testing.T) {
	d, sess, _ := scratchSession(t, scratchLimits{inline: true})
	got := stateScratch(t, d, sess)
	for _, a := range []string{"tmp", "home", "xdg"} {
		if _, ok := got.Areas[a]; !ok {
			t.Errorf("areas = %v, want %q present", got.Areas, a)
		}
	}
	if _, ok := got.Areas["other"]; ok {
		t.Errorf("areas = %v, want no \"other\" when the directory holds nothing outside the three", got.Areas)
	}
}

// muster #431, second Done-when: a session whose process died keeps its directory
// until it is closed, so it keeps its figure. Each read path is checked on its own
// driver, so that List is not simply replaying a figure State measured.
func TestScratch_ADeadSessionWhoseDirectoryExistsReportsItsFigure(t *testing.T) {
	kill := func(t *testing.T, d *Driver, sess fleet.Session) {
		t.Helper()
		known, _ := d.wasSeen(sess.ID)
		_ = known.srv.proc.group.Kill()
		select {
		case <-known.srv.proc.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the process did not exit")
		}
	}

	t.Run("State", func(t *testing.T) {
		d, sess, dir := scratchSession(t, scratchLimits{inline: true})
		writeScratch(t, dir, "fixture", 12_345)
		kill(t, d, sess)
		if _, err := os.Lstat(dir); err != nil {
			t.Fatalf("the dead session's directory is gone (%v): the test has nothing to measure", err)
		}
		st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if st.Status != fleet.StatusDead {
			t.Fatalf("Status = %q, want dead", st.Status)
		}
		if st.Scratch == nil || st.Scratch.Areas["tmp"] != 12_345 || st.Scratch.Bytes != onDisk(t, dir) {
			t.Errorf("dead session's scratch = %+v, want its directory's %d bytes, 12345 of them in tmp", st.Scratch, onDisk(t, dir))
		}
	})

	t.Run("List", func(t *testing.T) {
		d, sess, dir := scratchSession(t, scratchLimits{inline: true})
		writeScratch(t, dir, "fixture", 12_345)
		kill(t, d, sess)
		col, err := d.List(context.Background(), testReq, driver.ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		items := col.Items()
		if len(items) != 1 || items[0].State.Status != fleet.StatusDead {
			t.Fatalf("List = %+v, want the one dead session", items)
		}
		if sc := items[0].State.Scratch; sc == nil || sc.Areas["tmp"] != 12_345 || sc.Bytes != onDisk(t, dir) {
			t.Errorf("dead session's scratch in List = %+v, want its directory's %d bytes, 12345 of them in tmp", sc, onDisk(t, dir))
		}
	})
}

// A synthetic tree: a directory holding n regular files of size bytes each, handed to
// the walk the way filepath.WalkDir would hand them, without existing on disk. Building
// 200,000 real files took 75 s on the machine this was written on, and the old bound
// is a count of entries, so the count is what the test needs.
type fakeInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// bigTmp presents n files of size bytes under any root named tmp, and defers to the
// file system for every other root.
func bigTmp(n int, size int64) func(string, fs.WalkDirFunc) error {
	dirEntry := fs.FileInfoToDirEntry(fakeInfo{name: "tmp", mode: fs.ModeDir | 0o700})
	fileEntry := fs.FileInfoToDirEntry(fakeInfo{name: "f", size: size, mode: 0o600})
	return func(root string, fn fs.WalkDirFunc) error {
		if filepath.Base(root) != "tmp" {
			return filepath.WalkDir(root, fn)
		}
		if err := fn(root, dirEntry, nil); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if err := fn(filepath.Join(root, "f"), fileEntry, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

// muster #431, third Done-when: a tree over the entry bound the walk used to stop at
// is measured in full. The production bounds are used: the point is that they let it
// finish.
func TestScratch_ATreeOverTheOldEntryBoundIsMeasuredInFull(t *testing.T) {
	const oldEntryBound = 200_000 // what a walk stopped at before #431
	const files, each = 250_000, 4
	if files <= oldEntryBound {
		t.Fatal("the synthetic tree must be larger than the old bound")
	}

	// The test would prove nothing if the tree did not trip the old bound.
	t.Run("the old bound stopped it", func(t *testing.T) {
		d, sess, _ := scratchSession(t, scratchLimits{inline: true, maxEntries: oldEntryBound, walkDir: bigTmp(files, each)})
		got := stateScratch(t, d, sess)
		if !got.Partial || got.Areas["tmp"] >= files*each {
			t.Fatalf("scratch = %+v, want a partial figure at the old bound", *got)
		}
	})

	t.Run("the production bounds measure it in full", func(t *testing.T) {
		d, sess, _ := scratchSession(t, scratchLimits{inline: true, walkDir: bigTmp(files, each)})
		got := stateScratch(t, d, sess)
		if got.Partial || got.Areas["tmp"] != files*each {
			t.Errorf("scratch = %+v, want %d bytes in tmp, not partial", *got, files*each)
		}
		if got.Bytes != sumOf(got.Areas) || got.Bytes < files*each {
			t.Errorf("bytes = %d, want the areas' sum %d, at least %d", got.Bytes, sumOf(got.Areas), files*each)
		}
	})
}

func TestScratch_StopsAtItsBoundAndSaysThePartialFigureIsALowerBound(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{inline: true, maxEntries: 10})
	for i := 0; i < 50; i++ {
		writeScratch(t, dir, filepath.Join("many", string(rune('a'+i%26))+string(rune('a'+i/26)), "f"), 100)
	}
	got := stateScratch(t, d, sess)
	if !got.Partial {
		t.Errorf("scratch = %+v, want partial: the walk was cut at 10 entries of 100", *got)
	}
	if got.Areas["tmp"] <= 0 || got.Areas["tmp"] >= 5000 {
		t.Errorf("a partial figure is a lower bound: got %d bytes in tmp of a possible 5000", got.Areas["tmp"])
	}
	if got.Bytes != sumOf(got.Areas) {
		t.Errorf("bytes = %d, want the areas' sum %d even when partial", got.Bytes, sumOf(got.Areas))
	}
}

func TestScratch_APartOfTheTreeThatCannotBeReadMakesTheFigureAlowerBound(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the walker is not allowed to read")
	}
	d, sess, dir := scratchSession(t, scratchLimits{inline: true})
	writeScratch(t, dir, "ok", 700)
	writeScratch(t, dir, "locked/inside", 9000)
	locked := filepath.Join(dir, "tmp", "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	got := stateScratch(t, d, sess)
	if !got.Partial || got.Areas["tmp"] != 700 {
		t.Errorf("scratch = %+v, want 700 bytes in tmp and partial", *got)
	}
}

func TestScratch_IsMeasuredAtMostOncePerRefresh(t *testing.T) {
	clk := newStepClock()
	d, sess, dir := scratchSession(t, scratchLimits{inline: true, refresh: 10 * time.Minute}, withClock(clk.now))
	read := func() int64 {
		t.Helper()
		return stateScratch(t, d, sess).Areas["tmp"]
	}

	writeScratch(t, dir, "one", 100)
	if got := read(); got != 100 {
		t.Fatalf("first read = %d bytes in tmp, want 100", got)
	}
	writeScratch(t, dir, "two", 900)
	clk.advance(5 * time.Minute)
	if got := read(); got != 100 {
		t.Errorf("a read inside the refresh walked again: %d bytes, want the 100 of the last walk", got)
	}
	clk.advance(6 * time.Minute)
	if got := read(); got != 1000 {
		t.Errorf("a read past the refresh = %d bytes, want 1000", got)
	}
}

// Off the request path: a read that finds no figure answers without one and the
// walk finishes in the background.
func TestScratch_IsMeasuredInTheBackgroundWhenNotInline(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{})
	writeScratch(t, dir, "a", 4096)
	waitFor(t, "the background walk to land", func() bool {
		st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
		return err == nil && st.Scratch != nil && st.Scratch.Areas["tmp"] == 4096
	})
}

// muster #431: raising the bound on one walk must not let a machine with many
// sessions run a walker per session at once. While every place is taken a walk waits
// for one, and a read in the meantime still answers without a figure.
func TestScratch_WalksQueueBehindTheDriverWideLimit(t *testing.T) {
	d, _ := newIsolatedDriver(t, withScratchLimits(scratchLimits{maxWalks: 1}))
	a := mustCreate(t, d, "k-a", fleet.SessionSpec{})
	b := mustCreate(t, d, "k-b", fleet.SessionSpec{})
	read := func(s fleet.Session) *fleet.ScratchUsage {
		t.Helper()
		st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: s.ID})
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		return st.Scratch
	}

	gate := d.walkGate()
	gate <- struct{}{} // the one place is taken: some other walk is running
	read(a)
	read(b) // both start a walk, and both wait their turn
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if read(a) != nil || read(b) != nil {
			t.Fatal("a walk ran while every place was taken")
		}
		time.Sleep(20 * time.Millisecond)
	}

	<-gate // the other walk ends
	waitFor(t, "both queued walks to land, one after the other", func() bool {
		return read(a) != nil && read(b) != nil
	})
}

func TestScratch_NotReportedWhereThereIsNothingToMeasure(t *testing.T) {
	// Shared mode: the one server is not this driver's, and has no directory of its own.
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: ref.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Scratch != nil {
		t.Errorf("shared mode reported a scratch figure: %+v", *st.Scratch)
	}

	// A directory that is gone is no size, not a size of zero.
	d2, sess, dir := scratchSession(t, scratchLimits{inline: true})
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	st, err = d2.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Scratch != nil {
		t.Errorf("a missing directory reported %+v, want no figure", *st.Scratch)
	}
}

// muster #431: a figure is a fact about a directory. When the directory goes (the
// session was closed, or swept), the figure goes with it at the next walk instead of
// being replayed.
func TestScratch_AFigureEndsWhenItsDirectoryIsGone(t *testing.T) {
	clk := newStepClock()
	d, sess, dir := scratchSession(t, scratchLimits{inline: true, refresh: 10 * time.Minute}, withClock(clk.now))
	writeScratch(t, dir, "fixture", 800)
	if got := stateScratch(t, d, sess); got.Areas["tmp"] != 800 {
		t.Fatalf("first read = %+v, want 800 bytes in tmp", *got)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	clk.advance(11 * time.Minute)
	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Scratch != nil {
		t.Errorf("a figure outlived its directory: %+v", *st.Scratch)
	}
}

// Whatever the figure said, closing the session removes the directory it measured.
func TestClose_RemovesTheDirectoryTheScratchFigureWasMeasuredIn(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{inline: true})
	writeScratch(t, dir, "fixture", 1<<20)
	st, _ := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if st.Scratch == nil || st.Scratch.Areas["tmp"] != 1<<20 {
		t.Fatalf("scratch = %+v, want 1 MiB in tmp", st.Scratch)
	}
	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory the figure was measured in survived Close: %v", err)
	}
}
