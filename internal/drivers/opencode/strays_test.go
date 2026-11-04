package opencode

import (
	"path/filepath"
	"testing"
)

// muster #405: the rule that decides which processes are stopped is a pattern
// match on a command line followed by an environment, so it is tested as one on
// any platform; the tests that start real processes are in strays_unix_test.go.

func TestStrayMatcher_TiesAProcessToItsScopedDirectoryByOneOfThreeFacts(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"state", "opencode", "dirs")
	in := func(name string, parts ...string) string {
		return filepath.Join(append([]string{root, name}, parts...)...)
	}
	m := newStrayMatcher(root)
	for _, c := range []struct {
		name string
		line string
		dir  string
		by   strayBy
	}{
		{"an executable inside the directory", in("muster-opencode-5", "tmp", "pw", "chrome-headless-shell") + " --headless", "muster-opencode-5", strayByExecutable},
		{"an executable in the session's home", in("muster-opencode-5", "home", ".cache", "bin", "tool"), "muster-opencode-5", strayByExecutable},
		{"a profile inside the directory", "/usr/bin/browser --headless --user-data-dir=" + in("muster-opencode-12", "tmp", "profile-x") + " --no-sandbox", "muster-opencode-12", strayByProfile},
		{"a profile as the first argument", "/usr/bin/browser --user-data-dir=" + in("muster-opencode-12", "tmp", "p"), "muster-opencode-12", strayByProfile},
		{"the directory's TMPDIR in the environment", "/usr/bin/tool --flag PATH=/bin TMPDIR=" + in("muster-opencode-7", "tmp") + " HOME=/x", "muster-opencode-7", strayByEnvironment},
		{"the directory's TMPDIR last in the line", "/usr/bin/tool PATH=/bin TMPDIR=" + in("muster-opencode-7", "tmp"), "muster-opencode-7", strayByEnvironment},
	} {
		dir, by, ok := m.match(c.line)
		if !ok || dir != c.dir || by != c.by {
			t.Errorf("%s: match(%q) = %q, %q, %v; want %q, %q, true", c.name, c.line, dir, by, ok, c.dir, c.by)
		}
	}
}

func TestStrayMatcher_DoesNotMatchWhatMerelyMentionsTheDirectory(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"state", "opencode", "dirs")
	dir := filepath.Join(root, "muster-opencode-5")
	m := newStrayMatcher(root)
	for name, line := range map[string]string{
		"an argument that is a path inside it":          "/usr/bin/less " + filepath.Join(dir, "tmp", "log"),
		"an editor opening a file in it":                "/usr/bin/vim " + filepath.Join(dir, "home", "notes"),
		"a program run from outside, working on it":     "/usr/bin/rm -rf " + dir,
		"a profile outside any scoped directory":        "/usr/bin/browser --user-data-dir=" + filepath.Join(root, "other", "tmp"),
		"a profile flag that is not the profile flag":   "/usr/bin/browser --crash-dumps-dir=" + filepath.Join(dir, "tmp"),
		"a variable of another name ending in TMPDIR":   "/usr/bin/tool MY_TMPDIR=" + filepath.Join(dir, "tmp"),
		"a TMPDIR that is only the directory itself":    "/usr/bin/tool TMPDIR=" + dir,
		"a TMPDIR deeper than the tmp directory":        "/usr/bin/tool TMPDIR=" + filepath.Join(dir, "tmp", "deeper"),
		"a TMPDIR that merely starts with the tmp name": "/usr/bin/tool TMPDIR=" + filepath.Join(dir, "tmpx"),
		"another root that shares a prefix":             filepath.Join(root+"-sibling", "muster-opencode-5", "tmp", "tool"),
		"a scoped name that is not made of digits":      filepath.Join(root, "muster-opencode-5x", "tmp", "tool"),
		"a directory without a scoped name":             filepath.Join(root, "something-else", "tmp", "tool"),
		"an executable that only shares the first part": filepath.Join(root, "muster-opencode-5") + "-extra/tool",
		"a plain process":                               "/usr/bin/sleep 600 PATH=/bin TMPDIR=/tmp",
	} {
		if d, by, ok := m.match(line); ok {
			t.Errorf("%s: match(%q) = %q (%s); want no match", name, line, d, by)
		}
	}
}

func TestStrayMatcher_AScopedNameIsTheWholeNameNotItsPrefix(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"state", "dirs")
	m := newStrayMatcher(root)
	line := filepath.Join(root, "muster-opencode-12", "tmp", "tool")
	if got, _, ok := m.match(line); !ok || got != "muster-opencode-12" {
		t.Fatalf("match = %q, %v; want muster-opencode-12", got, ok)
	}
	if _, ok := m.strayFromLine(line, 10, "muster-opencode-1"); ok {
		t.Error("a process of muster-opencode-12 was taken for one of muster-opencode-1")
	}
	if p, ok := m.strayFromLine(line, 10, "muster-opencode-12"); !ok || p.pid != 10 || p.dir != "muster-opencode-12" {
		t.Errorf("strayFromLine = %+v, %v; want pid 10 in muster-opencode-12", p, ok)
	}
	if p, ok := m.strayFromLine(line, 10, ""); !ok || p.dir != "muster-opencode-12" {
		t.Errorf("with no directory named the answer = %+v, %v; want any scoped directory", p, ok)
	}
}

// A root with a space in it — a macOS "Application Support" — is the usual one; a
// command line cannot be split into fields, so the match is on the whole line.
func TestStrayMatcher_ARootWithASpaceInIt(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"Users", "u", "Library", "Application Support", "svc", "opencode", "dirs")
	m := newStrayMatcher(root)
	exe := filepath.Join(root, "muster-opencode-3", "tmp", "browser")
	if d, by, ok := m.match(exe + " --headless"); !ok || d != "muster-opencode-3" || by != strayByExecutable {
		t.Errorf("executable: %q, %q, %v", d, by, ok)
	}
	prof := "/usr/bin/browser --user-data-dir=" + filepath.Join(root, "muster-opencode-3", "tmp", "p")
	if d, by, ok := m.match(prof); !ok || d != "muster-opencode-3" || by != strayByProfile {
		t.Errorf("profile: %q, %q, %v", d, by, ok)
	}
	if _, _, ok := m.match("/usr/bin/browser --user-data-dir=" + filepath.Join(string(filepath.Separator)+"Users", "u", "Library", "Application")); ok {
		t.Error("a path that stops at the space matched")
	}
}

func TestGroupByDir(t *testing.T) {
	got := groupByDir([]strayProc{{pid: 1, dir: "muster-opencode-2"}, {pid: 2, dir: "muster-opencode-1"}, {pid: 3, dir: "muster-opencode-2"}})
	want := []dirCount{{"muster-opencode-1", 1}, {"muster-opencode-2", 2}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("groupByDir = %+v; want %+v", got, want)
	}
}

func TestStopStraysIn_OnlyActsOnAScopedDirectoryByName(t *testing.T) {
	for _, dir := range []string{"", "/tmp", "/tmp/not-scoped", "/tmp/muster-opencode-x", "/tmp/muster-opencode-5/tmp"} {
		stopped, left := stopStraysIn(dir)
		if stopped != nil || left != nil {
			t.Errorf("stopStraysIn(%q) = %v, %v; want nothing", dir, stopped, left)
		}
	}
}
