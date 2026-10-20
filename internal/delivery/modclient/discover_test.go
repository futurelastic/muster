package modclient_test

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/delivery/modclient"
)

func writeModule(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to the umask; the mode under test must be exact.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func problemFor(ps []modclient.Problem, name string) (modclient.Problem, bool) {
	for _, p := range ps {
		if p.Name == name {
			return p, true
		}
	}
	return modclient.Problem{}, false
}

func TestDiscover_MissingExecutableSkipped(t *testing.T) {
	dir := t.TempDir()
	good := writeModule(t, dir, "present", 0o755)

	found, problems := modclient.Discover(dir, []string{"absent", "present"})
	if len(found) != 1 || found[0].Name != "present" || found[0].Path != good {
		t.Fatalf("found = %+v, want only present at %s", found, good)
	}
	p, ok := problemFor(problems, "absent")
	if !ok || len(problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one for absent", problems)
	}
	if !strings.Contains(p.Reason, "no executable") {
		t.Errorf("reason = %q", p.Reason)
	}
}

func TestDiscover_AbsentOrEmptyDirectory(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "nope")} {
		found, problems := modclient.Discover(dir, []string{"a", "b"})
		if len(found) != 0 || len(problems) != 2 {
			t.Fatalf("dir %q: found=%v problems=%v, want every name a problem", dir, found, problems)
		}
		for _, p := range problems {
			if p.Reason != "modules directory absent" {
				t.Errorf("reason = %q", p.Reason)
			}
		}
	}
	// A directory that exists but is empty is not an error for the directory:
	// each configured name is simply not installed.
	found, problems := modclient.Discover(t.TempDir(), []string{"a"})
	if len(found) != 0 || len(problems) != 1 {
		t.Fatalf("empty dir: found=%v problems=%v", found, problems)
	}
	// And nothing configured means nothing to report at all.
	if f, p := modclient.Discover(t.TempDir(), nil); len(f) != 0 || len(p) != 0 {
		t.Fatalf("no names: found=%v problems=%v", f, p)
	}
	// A regular file where the directory should be.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, p := modclient.Discover(file, []string{"a"}); len(p) != 1 || !strings.Contains(p[0].Reason, "not a directory") {
		t.Fatalf("a file as the directory: %+v", p)
	}
}

func TestDiscover_RefusesGroupWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file's writability by others is an ACL question there, not mode bits; see discover_windows_test.go (windows, #303)")
	}
	dir := t.TempDir()
	writeModule(t, dir, "groupw", 0o775)
	writeModule(t, dir, "worldw", 0o757)
	writeModule(t, dir, "fine", 0o750)

	found, problems := modclient.Discover(dir, []string{"groupw", "worldw", "fine"})
	if len(found) != 1 || found[0].Name != "fine" {
		t.Fatalf("found = %+v, want only fine", found)
	}
	for _, name := range []string{"groupw", "worldw"} {
		p, ok := problemFor(problems, name)
		if !ok || !strings.Contains(p.Reason, "writable") {
			t.Errorf("%s: problem = %+v, want a writable refusal", name, p)
		}
	}
}

func TestDiscover_RefusesWhatIsNotAnExecutableFile(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		// There is no execute bit to be missing on Windows.
		writeModule(t, dir, "noexec", 0o644)
	}
	writeModule(t, dir, "target", 0o755)
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	names := []string{"link", "subdir"}
	want := map[string]string{"link": "not a regular file", "subdir": "not a regular file"}
	if runtime.GOOS != "windows" {
		names = []string{"noexec", "link", "subdir"}
		want["noexec"] = "not executable"
	}
	found, problems := modclient.Discover(dir, names)
	if len(found) != 0 {
		t.Fatalf("found = %+v, want nothing usable", found)
	}
	for name, frag := range want {
		p, ok := problemFor(problems, name)
		if !ok || !strings.Contains(p.Reason, frag) {
			t.Errorf("%s: problem = %+v, want it to mention %q", name, p, frag)
		}
	}
}

func TestDiscover_NeverFollowsAPathInAName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "modules")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeModule(t, root, "outside", 0o755)
	names := []string{"../outside", "sub/outside", `a\b`, "..", ".", "", "x\x00y"}
	found, problems := modclient.Discover(dir, names)
	if len(found) != 0 || len(problems) != len(names) {
		t.Fatalf("found=%v problems=%v: every path-like name must be refused", found, problems)
	}
	for _, p := range problems {
		if p.Reason != "invalid module name" {
			t.Errorf("%q: reason %q", p.Name, p.Reason)
		}
	}
}

func TestDiscover_PreservesOrder(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"c", "a", "b"} {
		writeModule(t, dir, n, 0o755)
	}
	found, _ := modclient.Discover(dir, []string{"c", "a", "b"})
	var got []string
	for _, f := range found {
		got = append(got, f.Name)
	}
	if !reflect.DeepEqual(got, []string{"c", "a", "b"}) {
		t.Errorf("order = %v: order is preference and must be kept", got)
	}
}

func TestParseModuleList(t *testing.T) {
	valid, invalid := modclient.ParseModuleList(" alpha, beta ,,alpha,Bad_Name , gamma-2,terminal,beta,inbox,,9x ")
	if !reflect.DeepEqual(valid, []string{"alpha", "beta", "gamma-2"}) {
		t.Errorf("valid = %v", valid)
	}
	if !reflect.DeepEqual(invalid, []string{"Bad_Name", "terminal", "inbox", "9x"}) {
		t.Errorf("invalid = %v (route words and malformed names are refused)", invalid)
	}
	if v, i := modclient.ParseModuleList(""); len(v) != 0 || len(i) != 0 {
		t.Errorf("empty list: %v %v", v, i)
	}
	if v, i := modclient.ParseModuleList(" , ,, "); len(v) != 0 || len(i) != 0 {
		t.Errorf("blank list: %v %v", v, i)
	}
}

func TestDefaultModulesDir(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "prefix", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "muster")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "prefix", "libexec", "muster", "modules")
	if got := modclient.DefaultModulesDir(exe); got != want {
		t.Errorf("DefaultModulesDir(%s) = %s, want %s", exe, got, want)
	}

	// A link elsewhere resolves to the real binary's prefix.
	linkDir := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "fleetd")
	if err := os.Symlink(exe, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if got := modclient.DefaultModulesDir(link); got != want {
		t.Errorf("through a symlink: %s, want %s", got, want)
	}

	// A path that does not exist falls back to its lexical parent.
	if got, want := modclient.DefaultModulesDir("/opt/x/bin/muster"), filepath.FromSlash("/opt/x/libexec/muster/modules"); got != want {
		t.Errorf("unresolvable path: %s, want %s", got, want)
	}
	if got := modclient.DefaultModulesDir(""); got != "" {
		t.Errorf("empty exe path: %q, want empty", got)
	}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestChildEnv_MinimalAndForwarded(t *testing.T) {
	getenv := envOf(map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/u", "USER": "u", "LANG": "C", "TMPDIR": "/tmp",
		"ACME_TOKEN": "t0k", "ACME_URL": "http://x", "UNRELATED_SECRET": "must-not-leak",
		"AWS_SECRET_ACCESS_KEY": "must-not-leak",
	})
	env, dropped := modclient.ChildEnv(getenv, "/var/state", []string{"ACME_TOKEN", " ACME_URL ", "ACME_UNSET"})
	want := []string{
		"PATH=/usr/bin", "HOME=/home/u", "USER=u", "LANG=C", "TMPDIR=/tmp",
		"FLEET_STATE_DIR=/var/state", "ACME_TOKEN=t0k", "ACME_URL=http://x",
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %q\nwant  %q", env, want)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v; an unset forwarded name is simply absent, not dropped", dropped)
	}
	for _, kv := range env {
		if strings.Contains(kv, "must-not-leak") {
			t.Errorf("an unlisted variable leaked: %s", kv)
		}
	}
}

func TestChildEnv_OnlyWhatIsSet(t *testing.T) {
	env, _ := modclient.ChildEnv(envOf(map[string]string{"PATH": "/bin", "LANG": ""}), "", nil)
	if !reflect.DeepEqual(env, []string{"PATH=/bin"}) {
		t.Errorf("env = %q: empty and unset names are absent, and no state dir means no FLEET_STATE_DIR", env)
	}
	// The result is an EMPTY environment, not "inherit": a nil-vs-empty slip
	// here would hand the child the daemon's whole environment.
	env, _ = modclient.ChildEnv(envOf(nil), "", nil)
	if len(env) != 0 {
		t.Errorf("env = %q", env)
	}
}

func TestChildEnv_NeverForwardsFleetNames(t *testing.T) {
	getenv := envOf(map[string]string{
		"FLEET_LISTEN": "0.0.0.0:1", "FLEET_PEER_TOKEN": "secret", "fleet_lower": "x", "FLEET_STATE_DIR": "/from/env",
		"OK_NAME": "v", "BAD-NAME": "v", "1BAD": "v",
	})
	env, dropped := modclient.ChildEnv(getenv, "/state", []string{
		"FLEET_LISTEN", "FLEET_PEER_TOKEN", "fleet_lower", "FLEET_STATE_DIR", "OK_NAME", "BAD-NAME", "1BAD", "A=B", "FLEET_LISTEN",
	})
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "FLEET_") && kv != "FLEET_STATE_DIR=/state" {
			t.Errorf("a FLEET_ name was forwarded: %s", kv)
		}
		if strings.Contains(kv, "secret") || strings.Contains(kv, "/from/env") {
			t.Errorf("a service value reached the child: %s", kv)
		}
	}
	if !contains(env, "FLEET_STATE_DIR=/state") || !contains(env, "OK_NAME=v") {
		t.Errorf("env = %q, want the state dir from the argument and OK_NAME", env)
	}
	wantDropped := []string{"FLEET_LISTEN", "FLEET_PEER_TOKEN", "fleet_lower", "FLEET_STATE_DIR", "BAD-NAME", "1BAD", "A=B"}
	if !reflect.DeepEqual(dropped, wantDropped) {
		t.Errorf("dropped = %q, want %q (each once, in order; the state dir comes only from the argument)", dropped, wantDropped)
	}
}

func TestChildEnv_IsDeterministic(t *testing.T) {
	getenv := envOf(map[string]string{"PATH": "/p", "HOME": "/h", "A": "1", "B": "2", "C": "3"})
	first, _ := modclient.ChildEnv(getenv, "/s", []string{"C", "A", "B"})
	for i := 0; i < 20; i++ {
		again, _ := modclient.ChildEnv(getenv, "/s", []string{"C", "A", "B"})
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("order changed: %q vs %q", first, again)
		}
	}
	if !reflect.DeepEqual(first[len(first)-3:], []string{"C=3", "A=1", "B=2"}) {
		t.Errorf("forwarded names must keep the operator's order: %q", first)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
