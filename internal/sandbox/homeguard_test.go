package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// realTemp is a temp directory with its symlinks resolved, as the callers of
// CoversHome hold their paths.
func realTemp(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	return d
}

// upper is p spelled in capitals: the same directory on a case-insensitive
// volume, another name (or none) on a case-sensitive one.
func upper(p string) string { return strings.ToUpper(p) }

// volumeFoldsCase measures, without trusting the code under test, whether the
// volume holding dir treats a name and its capitals as one entry.
func volumeFoldsCase(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "casefold-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASEFOLD-PROBE"))
	return err == nil
}

// skipUnlessFolds skips a test that needs an upper-cased spelling to name the
// same directory: the default macOS volume does, a Linux one does not.
func skipUnlessFolds(t *testing.T, dir string) {
	t.Helper()
	if !volumeFoldsCase(t, dir) {
		t.Skip("this volume is case-sensitive: an upper-cased spelling is another path")
	}
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoversHomeExactCaseIsWhatCoversOrIsSays(t *testing.T) {
	base := realTemp(t)
	home := filepath.Join(base, "svc")
	mkdirs(t, filepath.Join(home, "project"), filepath.Join(base, "svc2"))
	for _, c := range []struct {
		name, root string
		want       bool
	}{
		{"the home itself", home, true},
		{"an ancestor of the home", base, true},
		{"a directory under the home", filepath.Join(home, "project"), false},
		{"a sibling sharing the home's name as a prefix", filepath.Join(base, "svc2"), false},
		{"a path that does not exist, beside the home", filepath.Join(base, "elsewhere"), false},
	} {
		if got := CoversHome(c.root, home); got != c.want {
			t.Errorf("%s: CoversHome(%q, %q) = %v, want %v", c.name, c.root, home, got, c.want)
		}
	}
}

// A link to the home is the same directory under another name; the identity
// test sees it where the spelling test cannot, on any volume.
func TestCoversHomeSeesADirectoryUnderAnotherName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege on Windows")
	}
	base := realTemp(t)
	home := filepath.Join(base, "svc")
	mkdirs(t, home)
	link := filepath.Join(base, "alias")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("cannot make a symlink: %v", err)
	}
	if !CoversHome(link, home) {
		t.Errorf("a link to the home was not seen as covering it")
	}
	if CoversHome(filepath.Join(link, "x"), home) {
		t.Errorf("a path under a link to the home was seen as covering it")
	}
}

func TestCoversHomeHoldsInAnyLetterCase(t *testing.T) {
	base := realTemp(t)
	skipUnlessFolds(t, base)
	home := filepath.Join(base, "svc")
	mkdirs(t, filepath.Join(home, "project"))
	for _, c := range []struct {
		name, root string
		want       bool
	}{
		{"the home in capitals", upper(home), true},
		{"an ancestor of the home in capitals", upper(base), true},
		{"the home with one letter's case changed", filepath.Join(base, "Svc"), true},
		{"a directory under the home in capitals", upper(filepath.Join(home, "project")), false},
		{"a sibling in capitals", upper(filepath.Join(base, "svc2")), false},
	} {
		if got := CoversHome(c.root, home); got != c.want {
			t.Errorf("%s: CoversHome(%q, %q) = %v, want %v", c.name, c.root, home, got, c.want)
		}
	}
}

// The upper-cased spelling is checked against a home that is not there too:
// what the caller types must not decide the answer.
func TestCoversHomeFoldsAPathThatDoesNotExistOnACaseInsensitiveVolume(t *testing.T) {
	base := realTemp(t)
	skipUnlessFolds(t, base)
	home := filepath.Join(base, "nohome")
	for _, root := range []string{upper(home), upper(base)} {
		if !CoversHome(root, home) {
			t.Errorf("CoversHome(%q, %q) = false, want true: a missing home is the same name in any case here", root, home)
		}
	}
	if CoversHome(upper(filepath.Join(home, "sub")), home) {
		t.Errorf("a path under a missing home was seen as covering it")
	}
}

// On a case-sensitive volume the same two spellings are two directories, and
// neither may be mistaken for the other.
func TestCoversHomeKeepsDistinctDirectoriesApartOnACaseSensitiveVolume(t *testing.T) {
	base := realTemp(t)
	if volumeFoldsCase(t, base) {
		t.Skip("this volume is case-insensitive: the two names are one directory")
	}
	home, other := filepath.Join(base, "home"), filepath.Join(base, "HOME")
	mkdirs(t, home, other)
	if CoversHome(other, home) {
		t.Errorf("%q was seen as covering %q on a volume where they differ", other, home)
	}
	if CoversHome(filepath.Join(base, "NOPE"), filepath.Join(base, "nope")) {
		t.Errorf("two missing names that differ in case were folded on a volume that keeps them apart")
	}
}

func TestFoldsCaseAgreesWithTheVolume(t *testing.T) {
	base := realTemp(t)
	want := volumeFoldsCase(t, base)
	if got := foldsCase(base); got != want {
		t.Errorf("foldsCase(%q) = %v, measured %v", base, got, want)
	}
	// A directory whose name has no letters is judged by an ancestor's.
	digits := filepath.Join(base, "123")
	mkdirs(t, digits)
	if got := foldsCase(digits); got != want {
		t.Errorf("foldsCase(%q) = %v, measured %v", digits, got, want)
	}
}

func TestInstallRootsDoesNotOpenTheHomeSpelledInCapitals(t *testing.T) {
	base := realTemp(t)
	skipUnlessFolds(t, base)
	home := filepath.Join(base, "svc")
	mkdirs(t, filepath.Join(home, "bin"))
	bin := filepath.Join(upper(home), "bin", "tool")
	for _, r := range InstallRoots(bin, home) {
		if CoversHome(r, home) {
			t.Fatalf("InstallRoots(%q, %q) granted %q, which is the home", bin, home, r)
		}
	}
	// The same layout outside the home still grants its prefix.
	other := filepath.Join(base, "opt", "tool")
	mkdirs(t, filepath.Join(other, "bin"))
	if got := InstallRoots(filepath.Join(other, "bin", "x"), home); len(got) != 2 || got[1] != other {
		t.Errorf("a <prefix>/bin layout outside the home should grant its prefix: %v", got)
	}
}
