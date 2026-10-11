package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// muster #201: the pre-commit secret guard is per-clone git config, and
// nothing said so when it was missing. These tests run REAL git against
// repositories built in a temp directory — the row's whole claim is about what
// git would do, so a fake git would test nothing — and install the guard with
// the repository's own installer, so a change to how the installer switches the
// hook on cannot leave the row answering for a spelling nobody uses any more.

// isolateGit makes the developer's own git configuration and the enclosing
// process's repository selectors irrelevant to a test.
func isolateGit(t *testing.T) {
	t.Helper()
	skipOnWindows(t, "the hook row reads the POSIX execute bit and the installer is a shell script")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_PREFIX"} {
		if v, ok := os.LookupEnv(k); ok {
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newClone builds a checkout that looks like this repository to the row: the
// tracked hook and its installer are copies of the real ones, next to the
// service's command directory. Nothing is installed yet.
func newClone(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "muster"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "muster", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "install.sh"} {
		body, err := os.ReadFile(filepath.Join("..", "..", ".githooks", name))
		if err != nil {
			t.Fatalf("the repository's own %s must be readable from the package: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".githooks", name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// install runs the repository's own installer in dir, the way a person would.
func install(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("sh", ".githooks/install.sh")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
}

// lookPathWith resolves git for real and gitleaks as the test says.
func lookPathWith(haveGitleaks bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if name == "gitleaks" {
			if haveGitleaks {
				return "/fake/gitleaks", nil
			}
			return "", errors.New("not found")
		}
		return exec.LookPath(name)
	}
}

func hookRow(t *testing.T, dir string, haveGitleaks bool) doctorRow {
	t.Helper()
	row := checkPreCommitHook(context.Background(), doctorEnv{
		Dir: dir, LookPath: lookPathWith(haveGitleaks), Timeout: 10 * time.Second,
	})
	if row.ID != "hooks.pre-commit" {
		t.Fatalf("row id is a contract: got %q", row.ID)
	}
	// Rows are pasted into issues on a public repository: never a path.
	if said := row.Summary + row.Detail; (dir != "" && strings.Contains(said, dir)) || strings.Contains(said, os.TempDir()) {
		t.Fatalf("row prints a filesystem path: %+v", row)
	}
	return row
}

func wantRow(t *testing.T, row doctorRow, status rowStatus, inSummary string) {
	t.Helper()
	if row.Status != status || !strings.Contains(row.Summary, inSummary) {
		t.Fatalf("got %s %q, want %s containing %q\ndetail: %s", row.Status, row.Summary, status, inSummary, row.Detail)
	}
}

// The #201 shape: a clone that never ran the installer. Before the row, this
// was a silent state.
func TestHooksRowWarnsWhenTheHookIsNotInstalled(t *testing.T) {
	dir := newClone(t)
	row := hookRow(t, dir, true)
	wantRow(t, row, statusWarn, "runs no pre-commit hook")
	if !strings.Contains(row.Detail, ".githooks/install.sh") {
		t.Fatalf("the row must name the fix: %+v", row)
	}
	if len(row.Refs) == 0 || row.Refs[0] != 201 {
		t.Fatalf("row must cite #201: %+v", row)
	}
}

// The repository's own installer is what turns the row green, from the top of
// the clone and from anywhere below it.
func TestHooksRowPassesAfterTheRepositorysOwnInstaller(t *testing.T) {
	dir := newClone(t)
	install(t, dir)
	wantRow(t, hookRow(t, dir, true), statusPass, "git runs this repository's pre-commit hook")
	wantRow(t, hookRow(t, filepath.Join(dir, "cmd", "muster"), true), statusPass, "git runs this repository's pre-commit hook")
}

// A session works in a linked worktree, not in the clone's own checkout. The
// setting is shared with it, and the row must read it through the worktree.
func TestHooksRowInALinkedWorktree(t *testing.T) {
	dir := newClone(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, dir, "worktree", "add", "-q", "--detach", wt)
	wantRow(t, hookRow(t, wt, true), statusWarn, "runs no pre-commit hook")
	install(t, dir)
	wantRow(t, hookRow(t, wt, true), statusPass, "git runs this repository's pre-commit hook")
}

// With the hook on and no scanner, the hook prints a notice and exits 0: a
// pass here would be exactly the silent verify this command exists to end.
func TestHooksRowWarnsWithoutGitleaks(t *testing.T) {
	dir := newClone(t)
	install(t, dir)
	wantRow(t, hookRow(t, dir, false), statusWarn, "gitleaks is not on this PATH")
}

// The answer is what git would run, not whether one spelling of a setting is
// present: an absolute hooksPath to the same hook is as good as the relative one.
func TestHooksRowAcceptsAnyPathToTheSameHook(t *testing.T) {
	dir := newClone(t)
	git(t, dir, "config", "core.hooksPath", filepath.Join(dir, ".githooks"))
	wantRow(t, hookRow(t, dir, true), statusPass, "git runs this repository's pre-commit hook")
}

func TestHooksRowWarnsOnADifferentHook(t *testing.T) {
	dir := newClone(t)
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "pre-commit"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "core.hooksPath", "other")
	wantRow(t, hookRow(t, dir, true), statusWarn, "not this repository's")
}

// Git ignores a hook without the executable bit. The installer sets it; a
// filesystem that dropped it must not read as installed.
func TestHooksRowWarnsWhenTheHookIsNotExecutable(t *testing.T) {
	dir := newClone(t)
	git(t, dir, "config", "core.hooksPath", ".githooks")
	if err := os.Chmod(filepath.Join(dir, ".githooks", "pre-commit"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantRow(t, hookRow(t, dir, true), statusWarn, "not executable")
}

// doctor is run on service hosts and in every other repository too. There the
// row has nothing to say and must not say it — least of all about a hook that
// belongs to somebody else's repository.
func TestHooksRowIsSkippedOutsideAClone(t *testing.T) {
	isolateGit(t)
	for name, dir := range map[string]string{
		"no directory":       "",
		"a plain directory":  t.TempDir(),
		"a directory absent": filepath.Join(t.TempDir(), "gone"),
	} {
		t.Run(name, func(t *testing.T) {
			wantRow(t, hookRow(t, dir, true), statusSkip, "not run from inside a clone of this repository")
		})
	}

	// Another repository cut from the same scaffold carries the same tracked
	// hook; it is not this repository.
	elsewhere := t.TempDir()
	if err := os.Mkdir(filepath.Join(elsewhere, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, ".githooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, elsewhere, "init", "-q")
	wantRow(t, hookRow(t, elsewhere, true), statusSkip, "not run from inside a clone of this repository")
}

func TestHooksRowIsUnknownWithoutGit(t *testing.T) {
	dir := newClone(t)
	row := checkPreCommitHook(context.Background(), doctorEnv{
		Dir:      dir,
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	})
	wantRow(t, row, statusUnknown, "git is not on PATH")
}

// Run from inside a git hook or a rebase step, git carries variables that make
// -C irrelevant. The row must still answer for the clone it was pointed at.
func TestHooksRowIgnoresRepositorySelectorsInTheEnvironment(t *testing.T) {
	dir := newClone(t)
	installed := newClone(t)
	install(t, installed)
	t.Setenv("GIT_DIR", filepath.Join(installed, ".git"))
	wantRow(t, hookRow(t, dir, true), statusWarn, "runs no pre-commit hook")
}

// --skip means "this is deliberate", and a skipped row must not have run git.
func TestHooksRowSkipDoesNotRunGit(t *testing.T) {
	dir := newClone(t)
	env := doctorEnv{
		Getenv: func(string) string { return "" }, Offline: true, Dir: dir,
		Skip:    map[string]bool{"hooks.pre-commit": true},
		Timeout: time.Second,
		LookPath: func(name string) (string, error) {
			if name == "git" || name == "gitleaks" {
				t.Errorf("a skipped row must not resolve %s", name)
			}
			return "", errors.New("not found")
		},
	}
	row := rowByID(t, runChecks(context.Background(), env), "hooks.pre-commit")
	if row.Status != statusSkip {
		t.Fatalf("got %+v, want skip", row)
	}
}

// The wiring from the process's own working directory to the row: run from
// inside a clone, the command names the clone's state; the exit code, which
// scripts gate on, is the same as anywhere else — a missing hook is a warning.
func TestDoctorReportsTheHookFromTheWorkingDirectory(t *testing.T) {
	dir := newClone(t)
	vars := map[string]string{"FLEET_RUNTIME": "stub", "FLEET_MACHINE": "m1", "FLEET_TOKEN": "t"}
	getenv := func(k string) string { return vars[k] }
	var outHere, outElsewhere, errb bytes.Buffer

	t.Chdir(dir)
	_, codeHere := runDoctor([]string{"doctor", "--json", "--offline"}, getenv, &outHere, &errb)
	_, codeElsewhere := runDoctorAt("", []string{"doctor", "--json", "--offline"}, getenv, &outElsewhere, &errb)

	row := docRow(t, outHere.String(), "hooks.pre-commit")
	if row.Status != statusWarn {
		t.Fatalf("from an unhooked clone: got %+v, want warn", row)
	}
	if elsewhere := docRow(t, outElsewhere.String(), "hooks.pre-commit"); elsewhere.Status != statusSkip {
		t.Fatalf("with no working directory: got %+v, want skip", elsewhere)
	}
	if codeHere != codeElsewhere {
		t.Fatalf("a missing hook changed the exit code: %d here, %d elsewhere", codeHere, codeElsewhere)
	}
}

// A --skip naming the row is accepted anywhere, including where the row would
// only have been skipped anyway: it is a real row id.
func TestDoctorAcceptsSkipOfTheHookRow(t *testing.T) {
	var out, errb bytes.Buffer
	_, code := runDoctorAt("", []string{"doctor", "--offline", "--skip=hooks.pre-commit"},
		func(k string) string {
			return map[string]string{"FLEET_RUNTIME": "stub", "FLEET_MACHINE": "m1", "FLEET_TOKEN": "t"}[k]
		}, &out, &errb)
	if code == 2 || strings.Contains(errb.String(), "names no row") {
		t.Fatalf("--skip=hooks.pre-commit refused (code %d): %s", code, errb.String())
	}
}

func docRow(t *testing.T, jsonOut, id string) doctorRow {
	t.Helper()
	var doc struct {
		Rows []doctorRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, jsonOut)
	}
	for _, r := range doc.Rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row %q in %s", id, jsonOut)
	return doctorRow{}
}
