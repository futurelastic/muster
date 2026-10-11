package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #298: under bypass inside a sandbox, a path outside the working
// directory is answered by the session's own configuration instead of parking
// the session on an ask nobody can answer.
//
// What these tests can and cannot show. The fake server never evaluates a
// permission, so "the turn ends idle with no waiting_input" is a property of the
// runtime, not of this driver. What the driver owns, and what is asserted here,
// is the configuration it writes and the rule order the runtime is known to
// evaluate it with. The runtime end of it is checked once, against the real
// binary, in TestIntegration_ExternalDirectoryRulesAreWhatTheRuntimeResolves.

// ruleOrder reads the external_directory object from a session config with its
// entries in the order they were WRITTEN. Decoding into a map would sort them
// and hide the one thing that matters about the file.
func ruleOrder(t *testing.T, raw []byte) []externalDirectoryRule {
	t.Helper()
	var cfg struct {
		Permission struct {
			External json.RawMessage `json:"external_directory"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, raw)
	}
	if len(cfg.Permission.External) == 0 {
		t.Fatalf("no permission.external_directory in the config:\n%s", raw)
	}
	dec := json.NewDecoder(bytes.NewReader(cfg.Permission.External))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("external_directory is not an object: %s", cfg.Permission.External)
	}
	var out []externalDirectoryRule
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var action string
		if err := dec.Decode(&action); err != nil {
			t.Fatalf("rule %v: %v", k, err)
		}
		out = append(out, externalDirectoryRule{pattern: k.(string), action: action})
	}
	return out
}

// evaluate is the runtime's own rule evaluation for one pattern, reproduced from
// the shipped binary (v1.18.35): `*` matches any run of characters INCLUDING
// path separators, `?` matches one character, everything else is literal, and
// the LAST rule that matches decides. No match is an ask.
func evaluate(rules []externalDirectoryRule, pattern string) string {
	action := "ask"
	for _, r := range rules {
		re := regexp.QuoteMeta(r.pattern)
		re = strings.ReplaceAll(re, `\*`, `.*`)
		re = strings.ReplaceAll(re, `\?`, `.`)
		if regexp.MustCompile("(?s)^" + re + "$").MatchString(pattern) {
			action = r.action
		}
	}
	return action
}

// askFor is the pattern the runtime asks about when a tool names path: the
// parent directory, with `/*` appended (a directory is its own parent here).
func askFor(path string, isDir bool) string {
	if !isDir {
		path = filepath.Dir(path)
	}
	return path + "/*"
}

func TestExternalDirectoryRules_DenyFirstThenAllowTheSandboxsPaths(t *testing.T) {
	plan := &sandboxPlan{
		read:  []string{"/usr", "/work/repo", "/data/ref"},
		write: []string{"/work/repo", "/sessions/s1"},
	}
	plan.read = dedupe(append(plan.read, plan.write...))
	rules := externalDirectoryFor(plan)

	if rules[0].pattern != "*" || rules[0].action != "deny" {
		t.Fatalf("rule 0 = %+v: the catch-all deny must come first, because the last matching rule wins", rules[0])
	}
	got := map[string]string{}
	for _, r := range rules[1:] {
		if r.pattern == "*" {
			t.Errorf("a second catch-all %+v would override the allows before it", r)
		}
		got[r.pattern] = r.action
	}
	for _, root := range plan.read {
		if got[root+"/*"] != "allow" {
			t.Errorf("%s/* is not allowed, but the sandbox opens %s: %v", root, root, rules)
		}
	}

	cases := []struct {
		name, path string
		dir        bool
		want       string
	}{
		{"a file in the working directory", "/work/repo/main.go", false, "allow"},
		{"a nested file in the working directory", "/work/repo/pkg/sub/x.go", false, "allow"},
		{"the working directory itself", "/work/repo", true, "allow"},
		{"a readPaths entry", "/data/ref/spec.md", false, "allow"},
		{"the session's own directory", "/sessions/s1/tmp/scratch", false, "allow"},
		{"a file outside every allowed path", "/etc/hosts", false, "deny"},
		{"a sibling that only shares a prefix", "/work/repo-other/x.go", false, "deny"},
		{"the parent of an allowed root", "/work/x", false, "deny"},
		{"somebody's home", "/Users/someone/.ssh/id_ed25519", false, "deny"},
	}
	for _, c := range cases {
		if got := evaluate(rules, askFor(c.path, c.dir)); got != c.want {
			t.Errorf("%s (%s): the runtime would %s, want %s", c.name, c.path, got, c.want)
		}
	}
}

// The deny comes first in the written FILE, not merely first in the slice: the
// runtime reads an object's entries in written order, and a Go map would sort
// them by byte value and make the file's meaning depend on collation.
func TestExternalDirectoryRules_AreWrittenInOrder(t *testing.T) {
	// "/aaa" sorts after "*" as bytes and "!" would sort before it: a root the
	// sort would have put first, if the writer used a map.
	plan := &sandboxPlan{read: []string{"!odd", "/aaa", "/zzz"}}
	dir := t.TempDir()
	if err := writeSessionConfig(dir, true, plan, providerPolicy{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, permissionConfigRel))
	if err != nil {
		t.Fatal(err)
	}
	rules := ruleOrder(t, raw)
	if len(rules) == 0 || rules[0].pattern != "*" {
		t.Fatalf("rules written as %+v, want the catch-all first", rules)
	}
	want := []string{"*", "!odd/*", "/aaa/*", "/zzz/*"}
	if len(rules) != len(want) {
		t.Fatalf("rules = %+v, want patterns %v", rules, want)
	}
	for i, r := range rules {
		if r.pattern != want[i] {
			t.Errorf("rule %d = %q, want %q", i, r.pattern, want[i])
		}
	}
}

// A root with a glob metacharacter would match more than it names, and the
// allow that results is wider than the sandbox. Leaving it out is a readable
// deny, which is the safe direction.
func TestExternalDirectoryRules_OmitARootThatWouldMatchMoreThanItNames(t *testing.T) {
	plan := &sandboxPlan{read: []string{"/ok/dir", "/srv/data*", "/srv/a?b", "/"}}
	rules := externalDirectoryFor(plan)
	for _, r := range rules[1:] {
		if r.pattern != "/ok/dir/*" {
			t.Errorf("unexpected allow %q: only /ok/dir may be allowed", r.pattern)
		}
	}
	if got := evaluate(rules, "/srv/data-secret/x/*"); got != "deny" {
		t.Errorf("a path matched only by a metacharacter root was %s, want deny", got)
	}
	if got := evaluate(rules, "/anything/*"); got != "deny" {
		t.Errorf("a root of / opened everything: %s", got)
	}
}

func TestWriteSessionConfig_BypassWithoutAPlanIsRefused(t *testing.T) {
	err := writeSessionConfig(t.TempDir(), true, nil, providerPolicy{})
	if err == nil {
		t.Fatal("a bypass config was written with no sandbox plan, so with no boundary behind its approvals")
	}
}

func TestWriteSessionConfig_NoBypassWritesNoPermission(t *testing.T) {
	dir := t.TempDir()
	// A plan alone must not switch the posture on: only an explicit bypass does.
	if err := writeSessionConfig(dir, false, &sandboxPlan{read: []string{"/x"}}, providerPolicy{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, permissionConfigRel))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "permission") || strings.Contains(string(raw), "external_directory") {
		t.Errorf("a session that did not ask for bypass has a permission posture:\n%s", raw)
	}
}

// Through the driver, with a real sandbox plan: the working directory, the
// session's own directory, readPaths, writePaths and the package cache are all
// allowed, and a path outside them is denied. Also the file a relaunch writes.
func TestBypassSession_ConfigAllowsWhatTheSandboxOpensAndDeniesTheRest(t *testing.T) {
	state := t.TempDir()
	d := newStateDriver(t, state)
	requireSandboxHost(t, d)

	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cwd, readDir, writeDir, cache, outside := resolve(t.TempDir()), resolve(t.TempDir()), resolve(t.TempDir()), resolve(t.TempDir()), resolve(t.TempDir())
	sess := mustCreate(t, d, "k-extdir", fleet.SessionSpec{
		Cwd:            fleet.AbsolutePath(cwd),
		PermissionMode: fleet.PermissionModeBypass,
		Sandbox: &fleet.SandboxSpec{
			ReadPaths:    []fleet.AbsolutePath{fleet.AbsolutePath(readDir)},
			WritePaths:   []fleet.AbsolutePath{fleet.AbsolutePath(writeDir)},
			PackageCache: &fleet.SandboxPackageCache{Path: fleet.AbsolutePath(cache), Mode: fleet.PackageCacheShared},
		},
	})
	known, _ := d.wasSeen(sess.ID)
	cfgPath := filepath.Join(known.srv.dir, permissionConfigRel)

	check := func(label string) {
		t.Helper()
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		rules := ruleOrder(t, raw)
		if rules[0].pattern != "*" || rules[0].action != "deny" {
			t.Fatalf("%s: rules = %+v, want the catch-all deny first", label, rules)
		}
		sessDir := resolve(known.srv.dir)
		for name, path := range map[string]string{
			"the working directory":     filepath.Join(cwd, "src", "a.go"),
			"a readPaths entry":         filepath.Join(readDir, "ref.txt"),
			"a writePaths entry":        filepath.Join(writeDir, "out.txt"),
			"the shared package cache":  filepath.Join(cache, "mod", "x"),
			"the session's own tmp dir": filepath.Join(sessDir, "tmp", "scratch.bak"),
		} {
			if got := evaluate(rules, askFor(path, false)); got != "allow" {
				t.Errorf("%s: %s (%s) would be %s, want allow", label, name, path, got)
			}
		}
		if got := evaluate(rules, askFor(filepath.Join(outside, "secret"), false)); got != "deny" {
			t.Errorf("%s: a path outside every allowed root would be %s, want deny", label, got)
		}
	}
	check("create")

	// A relaunch rebuilds the file from the record (the plan is re-derived), so
	// it must carry the same rules and not the posture of an older driver.
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	if got, ok := listed(t, d2)[sess.ID]; !ok || got.State.Status == fleet.StatusUnknown {
		t.Fatalf("the session was not relaunched: %+v", got.State)
	}
	check("relaunch")
}
