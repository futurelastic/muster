//go:build darwin

package opencode

import (
	"context"
	"os/exec"
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
