package opencode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #419: the size of a session's scratch directory is part of its state.

func writeScratch(t *testing.T, dir string, rel string, n int) {
	t.Helper()
	p := filepath.Join(dir, "tmp", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
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

	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Scratch == nil {
		t.Fatal("State carries no scratch figure")
	}
	if st.Scratch.Bytes != 3000 || st.Scratch.Partial {
		t.Errorf("scratch = %+v, want 3000 bytes, not partial", *st.Scratch)
	}
	if age := time.Since(st.Scratch.AsOf); age < 0 || age > time.Minute {
		t.Errorf("asOf = %s (%s ago), want the moment of the measurement", st.Scratch.AsOf, age)
	}

	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items := col.Items()
	if len(items) != 1 || items[0].State.Scratch == nil || items[0].State.Scratch.Bytes != 3000 {
		t.Errorf("List scratch = %+v, want 3000 bytes", items)
	}
}

func TestScratch_StopsAtItsBoundAndSaysThePartialFigureIsALowerBound(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{inline: true, maxEntries: 10})
	for i := 0; i < 50; i++ {
		writeScratch(t, dir, filepath.Join("many", string(rune('a'+i%26))+string(rune('a'+i/26)), "f"), 100)
	}
	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if err != nil || st.Scratch == nil {
		t.Fatalf("State: %+v, %v", st, err)
	}
	if !st.Scratch.Partial {
		t.Errorf("scratch = %+v, want partial: the walk was cut at 10 entries of 100", *st.Scratch)
	}
	if st.Scratch.Bytes <= 0 || st.Scratch.Bytes >= 5000 {
		t.Errorf("a partial figure is a lower bound: got %d bytes of a possible 5000", st.Scratch.Bytes)
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

	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if err != nil || st.Scratch == nil {
		t.Fatalf("State: %+v, %v", st, err)
	}
	if !st.Scratch.Partial || st.Scratch.Bytes != 700 {
		t.Errorf("scratch = %+v, want 700 bytes and partial", *st.Scratch)
	}
}

func TestScratch_IsMeasuredAtMostOncePerRefresh(t *testing.T) {
	clk := newStepClock()
	d, sess, dir := scratchSession(t, scratchLimits{inline: true, refresh: 10 * time.Minute}, withClock(clk.now))
	read := func() *fleet.ScratchUsage {
		t.Helper()
		st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		return st.Scratch
	}

	writeScratch(t, dir, "one", 100)
	if got := read(); got == nil || got.Bytes != 100 {
		t.Fatalf("first read = %+v, want 100 bytes", got)
	}
	writeScratch(t, dir, "two", 900)
	clk.advance(5 * time.Minute)
	if got := read(); got.Bytes != 100 {
		t.Errorf("a read inside the refresh walked again: %d bytes, want the 100 of the last walk", got.Bytes)
	}
	clk.advance(6 * time.Minute)
	if got := read(); got.Bytes != 1000 {
		t.Errorf("a read past the refresh = %d bytes, want 1000", got.Bytes)
	}
}

// Off the request path: a read that finds no figure answers without one and the
// walk finishes in the background.
func TestScratch_IsMeasuredInTheBackgroundWhenNotInline(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{})
	writeScratch(t, dir, "a", 4096)
	waitFor(t, "the background walk to land", func() bool {
		st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
		return err == nil && st.Scratch != nil && st.Scratch.Bytes == 4096
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

	// A directory with no tmp/ is no size, not a size of zero.
	d2, sess, dir := scratchSession(t, scratchLimits{inline: true})
	if err := os.RemoveAll(filepath.Join(dir, "tmp")); err != nil {
		t.Fatal(err)
	}
	st, err = d2.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Scratch != nil {
		t.Errorf("a missing tmp/ reported %+v, want no figure", *st.Scratch)
	}
}

// Whatever the figure said, closing the session removes the directory it measured.
func TestClose_RemovesTheDirectoryTheScratchFigureWasMeasuredIn(t *testing.T) {
	d, sess, dir := scratchSession(t, scratchLimits{inline: true})
	writeScratch(t, dir, "fixture", 1<<20)
	st, _ := d.State(context.Background(), testReq, fleet.SessionRef{ID: sess.ID})
	if st.Scratch == nil || st.Scratch.Bytes != 1<<20 {
		t.Fatalf("scratch = %+v, want 1 MiB", st.Scratch)
	}
	if _, err := d.Close(context.Background(), testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory the figure was measured in survived Close: %v", err)
	}
}
