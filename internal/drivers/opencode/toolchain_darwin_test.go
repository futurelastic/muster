//go:build darwin

package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #484: on macOS a confined session runs `git` and a zsh here-document
// with the environment the driver gives it and nothing added.
//
// Two defects hid behind one symptom. `/usr/bin/git` is a shim that loads the
// toolchain from the developer directory `xcode-select` chose, and a versioned
// Xcode app under /Applications is outside the profile's read set; and zsh
// writes a here-document's temp file under TMPPREFIX, which defaults to
// /tmp/zsh, not under TMPDIR. These run the real programs from inside a real
// confined session's process, so the answer is the host's, not a model of it.
func TestConfinedSessionRunsGitAndAHereDocumentWithNoExtraEnvironment(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)

	// The control: the same commands in a session that is NOT confined. Without
	// it a host with no working git would make the confined result meaningless.
	open := mustCreate(t, d, "k-toolchain-open", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if r := sh(t, d, open.ID, "git --version"); r.Rc != 0 || !strings.Contains(r.Out, "git version") {
		t.Skipf("git does not run even outside a sandbox on this host, so this proves nothing: %+v", r)
	}

	// Say which host this ran on: the defect shows only where the selected
	// developer directory is not under a root the profile already reads.
	if out, err := exec.Command("/usr/bin/xcode-select", "-p").Output(); err == nil {
		t.Logf("selected developer directory: %s", strings.TrimSpace(string(out)))
	}

	sess, cwd := sandboxedSession(t, d, "k-toolchain-sbx", fleet.SandboxSpec{})

	r := sh(t, d, sess.ID, "cd "+cwd+" && git init -q . && git status --short && git --version")
	if r.Rc != 0 || !strings.Contains(r.Out, "git version") {
		t.Errorf("git did not run in a confined session with the environment the driver gave it: %+v", r)
	}
	if strings.Contains(r.Out, "xcrun: error") {
		t.Errorf("the toolchain shim could not load its developer directory: %+v", r)
	}

	// zsh (the default login shell on macOS) writes here-documents under
	// TMPPREFIX; run it explicitly, since the harness's own shell is not zsh.
	r = sh(t, d, sess.ID, "/bin/zsh -c 'cat <<EOF\nhere-document-works\nEOF\n'")
	if r.Rc != 0 || !strings.Contains(r.Out, "here-document-works") {
		t.Errorf("a here-document did not work in a confined zsh with the environment the driver gave it: %+v", r)
	}
}

// A confined session is handed what the plan decided, and nothing a create said
// can take its place (muster #484): the host is stated here, not measured, so the
// result does not depend on which Xcode this machine has.
func TestConfinedSessionIsGivenTheCommandLineToolsAndItsOwnHereDocumentPrefix(t *testing.T) {
	real := developerToolchain
	developerToolchain = func() (string, string) {
		return "/Applications/Xcode-15.2.0.app/Contents/Developer", "/Library/Developer/CommandLineTools"
	}
	t.Cleanup(func() { developerToolchain = real })

	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	sess, _ := sandboxedSession(t, d, "k-toolchain-env", fleet.SandboxSpec{})
	vars := inside(t, d, sess.ID).vars()
	if got, want := vars["DEVELOPER_DIR"], "/Library/Developer/CommandLineTools"; got != want {
		t.Errorf("DEVELOPER_DIR = %q, want %q", got, want)
	}
	if tmp, prefix := vars["TMPDIR"], vars["TMPPREFIX"]; tmp == "" || !strings.HasPrefix(prefix, tmp+"/") {
		t.Errorf("TMPPREFIX = %q, want a path inside TMPDIR = %q", prefix, tmp)
	}

	// An unconfined session has no profile to make the toolchain unreadable, so it
	// is given none of it.
	open := mustCreate(t, d, "k-toolchain-env-open", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if v, ok := inside(t, d, open.ID).vars()["DEVELOPER_DIR"]; ok {
		t.Errorf("an unconfined session was given DEVELOPER_DIR = %q", v)
	}

	// And a create may not supply it: refused, and nothing starts.
	_, err := d.Create(context.Background(), testReq, "k-toolchain-env-override", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{}, Env: map[string]string{"DEVELOPER_DIR": "/somewhere"},
	})
	if kindOfErr(err) != fleet.ErrorInvalid {
		t.Errorf("a create naming DEVELOPER_DIR = %v, want ErrorInvalid", err)
	}
}

// darwinUserTempDir is the per-user temporary directory the system names
// (confstr's _CS_DARWIN_USER_TEMP_DIR): where code that ignores TMPDIR writes,
// and where other processes keep their own files. Empty when it cannot be read.
func darwinUserTempDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Clean(dir)
}

// muster #505: the toolchain shim behind `git` (and `cc`, `python3`) keeps a lookup
// cache in the per-user temporary directory, which a confined session may not
// write, so every call used to print `couldn't create cache file` into the output
// the model reads. The shim reads the cache's path from `xcrun_db`; the driver
// points it inside the session's own tmp, where the profile already allows a write.
//
// This runs the real shim from inside a real confined session's process, and it
// asserts both halves: the noise is gone AND the cache is where it was sent, so a
// silent success that wrote nowhere would not pass.
func TestConfinedSessionsToolchainShimCachesInsideTheSession(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)

	// The control: the shim runs in a session that is NOT confined. Without it a
	// host with no working shim would make the confined result mean nothing.
	open := mustCreate(t, d, "k-xcrun-open", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if r := sh(t, d, open.ID, "/usr/bin/git --version"); r.Rc != 0 || !strings.Contains(r.Out, "git version") {
		t.Skipf("the /usr/bin/git shim does not run even outside a sandbox on this host, so this proves nothing: %+v", r)
	}

	sess, _ := sandboxedSession(t, d, "k-xcrun-sbx", fleet.SandboxSpec{})
	vars := inside(t, d, sess.ID).vars()
	cache, tmp := vars[xcrunDBEnv], vars["TMPDIR"]
	if cache == "" || !filepath.IsAbs(cache) || !strings.HasPrefix(cache, tmp+"/") {
		t.Errorf("%s = %q, want an absolute path inside the session's TMPDIR = %q", xcrunDBEnv, cache, tmp)
	}

	// Stdout and stderr arrive together, so any complaint is in Out.
	r := sh(t, d, sess.ID, "/usr/bin/git --version")
	if r.Rc != 0 || !strings.HasPrefix(r.Out, "git version") || strings.Contains(r.Out, "error") || strings.Contains(r.Out, "warning") {
		t.Errorf("/usr/bin/git --version in a confined session did not run clean: %+v", r)
	}
	if strings.Contains(r.Out, "cache file") {
		t.Errorf("the shim still tried to write its cache outside the session: %+v", r)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("the shim's cache is not at %s, where the driver sent it: %v", cache, err)
	}

	// An unconfined session is given none of this: nothing refuses its write.
	if v, ok := inside(t, d, open.ID).vars()[xcrunDBEnv]; ok {
		t.Errorf("an unconfined session was given %s = %q", xcrunDBEnv, v)
	}

	// And a create may not supply it: refused, and nothing starts.
	_, err := d.Create(context.Background(), testReq, "k-xcrun-override", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{}, Env: map[string]string{xcrunDBEnv: "/somewhere/xcrun_db"},
	})
	if kindOfErr(err) != fleet.ErrorInvalid {
		t.Errorf("a create naming %s = %v, want ErrorInvalid", xcrunDBEnv, err)
	}
}

// muster #505: the fix above moves one file; it does NOT open the per-user
// temporary directory. That directory holds other processes' hand-off files, and
// a grant on a name inside it (`tmp.` for `mktemp`'s fallback) would match every
// directory another process made with `mktemp -d`, so a confined session could
// write into one it did not create. This is the guard: whoever adds that grant
// breaks this test and has to answer the reason it was left out.
func TestConfinedSessionStillCannotWriteTheUserTempDir(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	temp := darwinUserTempDir(t)
	if temp == "" {
		t.Skip("the per-user temporary directory cannot be read on this host")
	}

	// A directory made OUTSIDE the sandbox, named the way `mktemp -d` names one:
	// another process's, which a prefix grant would hand to the session.
	foreign, err := os.MkdirTemp(temp, "tmp.")
	if err != nil {
		t.Skipf("cannot write the per-user temporary directory from the test either: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(foreign) })
	fresh := filepath.Join(temp, "muster-505-probe-"+filepath.Base(foreign))
	t.Cleanup(func() { _ = os.Remove(fresh) })

	// The control: the same writes in a session that is NOT confined, so a refusal
	// below cannot be a mistake in the probe.
	open := mustCreate(t, d, "k-usertemp-open", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if r := sh(t, d, open.ID, "echo x > '"+filepath.Join(foreign, "control")+"'"); r.Rc != 0 {
		t.Skipf("an unconfined session cannot write %s either, so this proves nothing: %+v", temp, r)
	}

	sess, _ := sandboxedSession(t, d, "k-usertemp-sbx", fleet.SandboxSpec{})
	for name, target := range map[string]string{
		"a new name":                      fresh,
		"a file in another process's dir": filepath.Join(foreign, "planted"),
	} {
		if r := sh(t, d, sess.ID, "echo x > '"+target+"'"); r.Rc == 0 {
			t.Errorf("a confined session wrote %s in the per-user temporary directory: %+v", name, r)
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("%s exists after a confined session was refused", target)
		}
	}

	// What a program that names no directory meets, stated so the decision is on
	// the record. On macOS `mktemp -d` (and `mktemp -t`) take the per-user
	// temporary directory FIRST and consult TMPDIR only when that is unavailable
	// (mktemp(1)), so handing a child TMPDIR does not help: the call is refused
	// whatever the environment says. What works is a template the caller places
	// itself, under the TMPDIR the session has. The profile is not widened for it.
	for _, cmd := range []string{"env -i /usr/bin/mktemp -d", `env -i TMPDIR="$TMPDIR" /usr/bin/mktemp -d`} {
		if r := sh(t, d, sess.ID, cmd); r.Rc == 0 {
			t.Errorf("%q created a directory in the per-user temporary directory: %+v", cmd, r)
		}
	}
	if r := sh(t, d, sess.ID, `env -i TMPDIR="$TMPDIR" /usr/bin/mktemp -d "$TMPDIR/probe.XXXXXXXX"`); r.Rc != 0 {
		t.Errorf("mktemp given a template under the session's TMPDIR did not work: %+v", r)
	}
}
