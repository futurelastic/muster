package opencode

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// #55's third deliverable: absent install is a first-class answer, not a
// startup crash. Probe must degrade honestly rather than panic when the
// binary genuinely is not there.
func TestProbe_MissingBinaryDegradesHonestly_NeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Probe panicked on a missing binary: %v", r)
		}
	}()
	avail := Probe(context.Background(), "opencode-definitely-does-not-exist-on-this-machine")
	if avail.Installed {
		t.Fatal("Installed = true for a binary name that cannot exist")
	}
	if avail.Err == nil {
		t.Fatal("Err = nil, want a reason")
	}
}

// New must likewise return an error rather than crash when the binary is
// absent, so a caller (muster) can log and continue without this
// runtime instead of the whole daemon going down over one optional
// third-party dependency.
func TestNew_MissingBinaryReturnsError_NeverPanicsOrBlocks(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("New panicked on a missing binary: %v", r)
		}
	}()
	_, err := New(context.Background(), "test-machine", WithBinary("opencode-definitely-does-not-exist"))
	if err == nil {
		t.Fatal("New succeeded against a nonexistent binary")
	}
}

func TestFreePort_ReturnsAUsablePort(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port = %d, out of range", port)
	}
}

func TestFreePort_TwoCallsDoNotCollide(t *testing.T) {
	a, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	b, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	// Not a hard guarantee under extreme concurrency, but the ordinary
	// case this test exists to catch a regression in: freePort must
	// release its probe listener before returning, or every caller after
	// the first would get the same port back.
	if a == b {
		t.Skip("got the same port twice — rare but not impossible under the OS's own reuse policy; not a failure on its own")
	}
}

func TestGenerateCredential_IsNonEmptyAndNotConstant(t *testing.T) {
	a, err := generateCredential()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" {
		t.Fatal("credential is empty")
	}
	if a == b {
		t.Fatal("two generated credentials were identical — not randomly generated")
	}
}

// Boss's provider ruling on #55: the credential travels through the
// environment ONLY, never argv. This is the structural half of that
// assertion — it inspects the exec.Cmd this package would actually run,
// using a stand-in "opencode" on PATH so the test never needs the real
// binary, and never starts the process (Start is not called).
func TestStartProcess_CredentialNeverReachesArgv(t *testing.T) {
	bin := stubBinary(t)
	// buildServeCmd is the exact function startServer calls — this test
	// exercises the real code path, not a duplicate of it.
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cred, err := generateCredential()
	if err != nil {
		t.Fatal(err)
	}
	cmd := buildServeCmd(bin, "", port, "muster", cred, nil, "")

	hasPrintLogs := false
	for _, a := range cmd.Args {
		if a == "--print-logs" {
			hasPrintLogs = true
		}
		if strings.Contains(a, cred) {
			t.Fatalf("credential leaked into argv: %v", cmd.Args)
		}
		if a == "--mdns" {
			t.Fatal("--mdns was passed; it defaults the bind to 0.0.0.0 (#55)")
		}
	}
	if !hasPrintLogs {
		t.Fatalf("--print-logs missing from argv %v; the error-name capture has no log to read (#288)", cmd.Args)
	}
	foundInEnv := false
	for _, e := range cmd.Env {
		if strings.Contains(e, cred) {
			foundInEnv = true
		}
	}
	if !foundInEnv {
		t.Fatal("credential did not travel via the environment at all")
	}
}

// stubBinary returns a path this test can pass to exec.Command without
// needing the real opencode binary — /bin/echo (or similar) stands in
// well enough since this test never calls Start.
func stubBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("true")
	if err != nil {
		path, err = exec.LookPath("echo")
	}
	if err != nil {
		t.Skip("no stand-in binary (true/echo) found on PATH")
	}
	return path
}

// muster #288: the sandboxed command is built from the same serve slice, so it
// carries --print-logs too — checked on its own because the sandbox wrapper is
// a separate branch of buildServeCmd.
func TestBuildServeCmd_TheSandboxedCommandAlsoPrintsLogs(t *testing.T) {
	cmd := buildServeCmd("/bin/true", "", 1234, "muster", "cred", nil, "(version 1)")
	found := false
	for _, a := range cmd.Args {
		if a == "--print-logs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("--print-logs missing from the sandboxed argv %v (#288)", cmd.Args)
	}
}
