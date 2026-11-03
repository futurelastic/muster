package opencode

import (
	"path/filepath"
	"strings"
	"testing"
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
