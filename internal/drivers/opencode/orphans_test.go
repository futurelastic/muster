package opencode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// muster #398: what makes a process a server of a session root is read from its
// own command line and environment. The matcher is pure, so it is tested on
// every platform.

func TestScopedDirIn(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"state", "opencode", "dirs")
	sep := string(filepath.Separator)
	scoped := func(n string) string { return root + sep + "muster-opencode-" + n + sep + "tmp" }
	spaced := filepath.Join(string(filepath.Separator)+"Users", "a b", "Application Support", "dirs")

	cases := []struct {
		name string
		line string
		root string
		want string
	}{
		{"the server's own TMPDIR", "/bin/opencode serve --port 5 --hostname 127.0.0.1 HOME=/x TMPDIR=" + scoped("123") + " PATH=/usr/bin", root, "muster-opencode-123"},
		{"TMPDIR last on the line", "/bin/opencode serve --port 5 --hostname 127.0.0.1 TMPDIR=" + scoped("9"), root, "muster-opencode-9"},
		{"NUL separators read as spaces", strings.ReplaceAll("opencode\x00serve\x00--port\x005 TMPDIR="+scoped("77")+"\x00PATH=/bin", "\x00", " "), root, "muster-opencode-77"},
		{"a root whose path holds spaces", "opencode serve --port 5 --hostname 127.0.0.1 TMPDIR=" + spaced + sep + "muster-opencode-4" + sep + "tmp LANG=C", spaced, "muster-opencode-4"},
		{"another value holding spaces does not break the match", "opencode serve --port 5 --hostname 127.0.0.1 NOTE=a value with spaces TMPDIR=" + scoped("8") + " X=1", root, "muster-opencode-8"},
		{"a sibling directory of the root", "TMPDIR=" + root + "2" + sep + "muster-opencode-1" + sep + "tmp", root, ""},
		{"a directory deeper than the root", "TMPDIR=" + root + sep + "x" + sep + "muster-opencode-1" + sep + "tmp", root, ""},
		{"a variable that merely ends in TMPDIR", "MY_TMPDIR=" + scoped("1"), root, ""},
		{"not the muster naming", "TMPDIR=" + root + sep + "other-1" + sep + "tmp", root, ""},
		{"the digits are not digits", "TMPDIR=" + root + sep + "muster-opencode-abc" + sep + "tmp", root, ""},
		{"the scoped directory itself, not its tmp", "TMPDIR=" + root + sep + "muster-opencode-1", root, ""},
		{"no TMPDIR at all", "opencode serve --port 5 --hostname 127.0.0.1 HOME=/home/x", root, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scopedDirIn(tc.line, tc.root)
			if tc.want == "" {
				if ok {
					t.Fatalf("scopedDirIn matched %q; want no match", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("scopedDirIn = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
}

func TestServePort(t *testing.T) {
	cases := []struct {
		line string
		port int
	}{
		{"/Users/x/.local/bin/opencode serve --port 49458 --hostname 127.0.0.1 --print-logs --pure", 49458},
		{"/bin/opencode serve --port 7 --hostname 127.0.0.1", 7},
		{"/bin/opencode serve --port 7 --hostname 127.0.0.1 TMPDIR=/x", 7},
		{"/bin/opencode serve --port 7 --hostname 0.0.0.0", 0},
		{"/bin/opencode serve --hostname 127.0.0.1 --port 7", 0},
		{"/bin/opencode run --port 7 --hostname 127.0.0.1", 0},
		{"/bin/opencode serve --port x --hostname 127.0.0.1", 0},
		{"/bin/opencode serve --port 0 --hostname 127.0.0.1", 0},
		{"", 0},
	}
	for _, tc := range cases {
		got, ok := servePort(tc.line)
		if (tc.port == 0) == ok || got != tc.port {
			t.Errorf("servePort(%q) = %d, %v; want %d", tc.line, got, ok, tc.port)
		}
	}
}

// ---- muster #399: scoped directories no record names -----------------------------

// scopedFixture is a state directory laid out as the driver lays it out, with a
// helper to make a scoped directory of a given content and age.
type scopedFixture struct {
	t        *testing.T
	storeDir string
	root     string
	now      time.Time
}

func newScopedFixture(t *testing.T) *scopedFixture {
	t.Helper()
	storeDir, root := stateLayout(t.TempDir())
	for _, d := range []string{storeDir, root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &scopedFixture{t: t, storeDir: storeDir, root: root, now: time.Now()}
}

// dir makes root/<name> holding the given files (path -> content), every entry
// last written age ago. It returns the directory's path.
func (f *scopedFixture) dir(name string, age time.Duration, files map[string]string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	f.age(dir, age)
	return dir
}

// age sets the modification time of everything under dir (the directory itself
// last, since writing a file moves its parent's time).
func (f *scopedFixture) age(dir string, age time.Duration) {
	f.t.Helper()
	when := f.now.Add(-age)
	var paths []string
	_ = filepath.WalkDir(dir, func(p string, _ os.DirEntry, _ error) error {
		paths = append(paths, p)
		return nil
	})
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], when, when); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *scopedFixture) record(id, dir string) {
	f.t.Helper()
	if err := (&recordStore{dir: f.storeDir}).write(&sessionRecord{ID: id, Dir: dir}); err != nil {
		f.t.Fatal(err)
	}
}

func noServers(string) ([]serverProc, int, error) { return nil, 0, nil }

func serversHolding(names ...string) func(string) ([]serverProc, int, error) {
	return func(string) ([]serverProc, int, error) {
		var out []serverProc
		for i, n := range names {
			out = append(out, serverProc{pid: 100 + i, port: 9000 + i, dir: n})
		}
		return out, 0, nil
	}
}

// What a removal that did not finish leaves, as measured: only the runtime's own
// cache and lock files, or only scratch.
var (
	remnantXDG = map[string]string{"xdg/cache/opencode/models.json": "{}", "xdg/state/opencode/locks/.keep": ""}
	remnantTmp = map[string]string{"tmp/dl-home-aB3/data/x": "scratch"}
	withStore  = map[string]string{"xdg/data/opencode/opencode.db": "the conversation", "xdg/cache/opencode/models.json": "{}"}
)

func names(dirs []orphanDir) []string {
	var out []string
	for _, d := range dirs {
		out = append(out, d.name)
	}
	return out
}

func TestOrphanDirs_ListsOnlyDirectoriesNobodyHolds(t *testing.T) {
	f := newScopedFixture(t)
	old := 3 * time.Hour

	named := f.dir("muster-opencode-1", old, remnantXDG)
	f.record("ses_one", named)
	f.dir("muster-opencode-2", old, remnantXDG) // a live server holds this one
	f.dir("muster-opencode-3", old, remnantXDG)
	f.dir("muster-opencode-4", old, withStore)
	f.dir("not-a-scoped-name-5", old, remnantXDG)
	f.dir("muster-opencode-abc", old, remnantXDG)

	// A link, and a file, that merely bear the name. The link's target must not be
	// listed or touched.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "muster-opencode-6")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "muster-opencode-7"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	dirs, unreadable, uninspected, err := orphanDirs(f.storeDir, f.root, serversHolding("muster-opencode-2"))
	if err != nil || unreadable != 0 || uninspected != 0 {
		t.Fatalf("orphanDirs = %v, %d, %d, %v", names(dirs), unreadable, uninspected, err)
	}
	if got, want := strings.Join(names(dirs), ","), "muster-opencode-3,muster-opencode-4"; got != want {
		t.Fatalf("listed %q; want %q (a record's directory, a held one, a link, a file and other names are not orphans)", got, want)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatalf("the link's target was touched: %v", err)
	}
}

func TestOrphanDir_RemovableOnlyWhenItHoldsNoStoreAndHasGoneQuiet(t *testing.T) {
	f := newScopedFixture(t)
	f.dir("muster-opencode-1", 3*time.Hour, remnantXDG)
	f.dir("muster-opencode-2", 3*time.Hour, remnantTmp)
	f.dir("muster-opencode-3", 3*time.Hour, withStore)
	f.dir("muster-opencode-4", 3*time.Hour, map[string]string{"home/work/notes.md": "mine"})
	f.dir("muster-opencode-5", 3*time.Hour, map[string]string{"unknown-top-level-entry": "?"})
	f.dir("muster-opencode-6", 3*time.Hour, nil) // nothing in it at all
	// The directory itself is old, a file deep inside it is not: the top
	// directory's own time does not move when a nested file is written.
	deep := f.dir("muster-opencode-7", 3*time.Hour, remnantXDG)
	fresh := filepath.Join(deep, "xdg", "cache", "opencode", "models.json")
	if err := os.Chtimes(fresh, f.now.Add(-time.Minute), f.now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.dir("muster-opencode-8", 10*time.Minute, remnantXDG)

	dirs, _, _, err := orphanDirs(f.storeDir, f.root, noServers)
	if err != nil {
		t.Fatal(err)
	}
	removable := map[string]bool{}
	for _, d := range dirs {
		removable[d.name] = d.removable(f.now)
	}
	want := map[string]bool{
		"muster-opencode-1": true,
		"muster-opencode-2": true,
		"muster-opencode-3": false, // holds the conversation database
		"muster-opencode-4": false, // a session's own files in its home
		"muster-opencode-5": false, // not in any area known to be disposable
		"muster-opencode-6": true,
		"muster-opencode-7": false, // a file in it was written a minute ago
		"muster-opencode-8": false, // inside the grace period
	}
	for name, w := range want {
		if got, ok := removable[name]; !ok || got != w {
			t.Errorf("%s: removable = %v (listed %v); want %v", name, got, ok, w)
		}
	}
}

func TestInspectDir_SizeAndNewest(t *testing.T) {
	f := newScopedFixture(t)
	f.dir("muster-opencode-1", 5*time.Hour, map[string]string{"tmp/a": "12345", "tmp/sub/b": "123"})
	o := inspectDir(f.root, "muster-opencode-1")
	if o.bytes != 8 || o.holdsStore {
		t.Fatalf("inspectDir = %+v; want 8 bytes and no store", o)
	}
	if d := f.now.Add(-5 * time.Hour).Sub(o.newest); d < -time.Second || d > time.Second {
		t.Fatalf("newest = %v; want about 5h before now", o.newest)
	}
}

func TestOrphanDirs_AnUnreadableScanIsNotAnEmptyAnswer(t *testing.T) {
	f := newScopedFixture(t)
	f.dir("muster-opencode-1", 3*time.Hour, remnantXDG)

	for _, scanErr := range []error{ErrScanUnsupported, errors.New("ps: not found")} {
		dirs, _, _, err := orphanDirs(f.storeDir, f.root, func(string) ([]serverProc, int, error) { return nil, 0, scanErr })
		if !errors.Is(err, scanErr) || dirs != nil {
			t.Errorf("scan error %v: got %v, %v; want the error and no list", scanErr, names(dirs), err)
		}
	}
	// A root that does not exist holds no directories; it is not an error.
	dirs, _, _, err := orphanDirs(f.storeDir, filepath.Join(f.root, "absent"), noServers)
	if err != nil || len(dirs) != 0 {
		t.Errorf("absent root: %v, %v", names(dirs), err)
	}
}

func TestSweepOrphanDirs_RemovesOnlyWhatIsProvablyNobodys(t *testing.T) {
	f := newScopedFixture(t)
	old := 3 * time.Hour
	named := f.dir("muster-opencode-1", old, remnantXDG)
	f.record("ses_one", named)
	held := f.dir("muster-opencode-2", old, remnantXDG)
	gone := f.dir("muster-opencode-3", old, remnantXDG)
	gone2 := f.dir("muster-opencode-4", old, remnantTmp)
	store := f.dir("muster-opencode-5", old, withStore)
	recent := f.dir("muster-opencode-6", 5*time.Minute, remnantXDG)

	var logged logSink
	d := &Driver{store: &recordStore{dir: f.storeDir}, sessionRoot: f.root, logf: logged.add}
	d.sweepOrphanDirsWith(serversHolding("muster-opencode-2"))

	for _, p := range []string{named, held, store, recent} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
	}
	for _, p := range []string{gone, gone2} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was not removed (%v)", filepath.Base(p), err)
		}
	}
	all := strings.Join(logged.lines, "\n")
	if !strings.Contains(all, "muster-opencode-3") || !strings.Contains(all, "muster-opencode-4") {
		t.Errorf("no log line names a removed directory: %q", all)
	}
	if strings.Contains(all, f.root) {
		t.Errorf("a log line carries a filesystem path: %q", all)
	}
}

func TestSweepOrphanDirs_RemovesNothingItCannotVouchFor(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *scopedFixture)
		scan  func(string) ([]serverProc, int, error)
		off   bool
	}{
		{"a record that cannot be read", func(f *scopedFixture) {
			if err := os.WriteFile(filepath.Join(f.storeDir, "ses_bad.json"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, noServers, false},
		{"a process that looks like a server and cannot be inspected", func(*scopedFixture) {},
			func(string) ([]serverProc, int, error) { return nil, 1, nil }, false},
		{"a platform that cannot list processes", func(*scopedFixture) {},
			func(string) ([]serverProc, int, error) { return nil, 0, ErrScanUnsupported }, false},
		{"a process listing that failed", func(*scopedFixture) {},
			func(string) ([]serverProc, int, error) { return nil, 0, errors.New("ps: not found") }, false},
		{"the sweep switched off", func(*scopedFixture) {}, noServers, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopedFixture(t)
			dir := f.dir("muster-opencode-1", 3*time.Hour, remnantXDG)
			tc.setup(f)
			d := &Driver{store: &recordStore{dir: f.storeDir}, sessionRoot: f.root, noStartSweep: tc.off}
			d.sweepOrphanDirsWith(tc.scan)
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("the directory was removed: %v", err)
			}
		})
	}

	// And with no state directory there is no record store to ask: nothing is
	// removed, whatever is under the root.
	f := newScopedFixture(t)
	dir := f.dir("muster-opencode-1", 3*time.Hour, remnantXDG)
	(&Driver{sessionRoot: f.root}).sweepOrphanDirsWith(noServers)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("without a state directory the directory was removed: %v", err)
	}
}

func TestOrphanDirReport_CountsWhatTheDoctorSays(t *testing.T) {
	f := newScopedFixture(t)
	f.dir("muster-opencode-1", time.Hour, map[string]string{"tmp/a": "1234"})
	f.dir("muster-opencode-2", time.Hour, map[string]string{"tmp/a": "12"})
	f.dir("muster-opencode-3", time.Hour, map[string]string{"xdg/data/opencode/opencode.db": "123456"})
	f.record("ses_x", f.dir("muster-opencode-4", time.Hour, remnantXDG))

	rep, err := orphanDirReport(f.storeDir, f.root, noServers)
	if err != nil {
		t.Fatal(err)
	}
	want := OrphanDirReport{Removable: 2, RemovableBytes: 6, HoldStore: 1, HoldStoreBytes: 6}
	if rep != want || rep.Total() != 3 {
		t.Fatalf("report = %+v; want %+v", rep, want)
	}
	if _, err := orphanDirReport(f.storeDir, f.root, func(string) ([]serverProc, int, error) { return nil, 0, ErrScanUnsupported }); !errors.Is(err, ErrScanUnsupported) {
		t.Fatalf("an unsupported platform: err = %v", err)
	}
}

// logSink collects driver notices without a build constraint, unlike logLines.
type logSink struct{ lines []string }

func (l *logSink) add(format string, args ...any) {
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}
