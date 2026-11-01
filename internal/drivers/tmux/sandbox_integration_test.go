package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
	"github.com/futurelastic/muster/internal/state"
)

// muster #345, the real-substrate half: a REAL multiplexer server, a REAL
// sandbox-exec, and a stand-in agent that tries to get out. The unit tests prove
// the decision and the profile text; this proves what the host then refuses,
// from inside a session's own process — which is the claim the issue makes.
//
// The stand-in answers the measurement (`auth status`) the way the real CLI
// does, so the whole chain runs: capability → create → wrapper → profile → agent.
//
// It uses a PRIVATE server (privateMux), never the machine's own.
func TestLiveSandboxedSessionIsConfined(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	if sup, why := sandbox.HostSupport(); sup == nil {
		t.Skipf("this host cannot enforce a sandbox: %s", why)
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}

	// The stand-in agent sits in a <prefix>/bin layout, as an installed one does.
	binDir := filepath.Join(base, "agent", "bin")
	work := filepath.Join(base, "work")
	outside := filepath.Join(base, "outside")
	home := filepath.Join(base, "home")
	cfg := filepath.Join(home, ".claude")
	for _, d := range []string{binDir, work, outside, cfg, filepath.Join(base, "state")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("not for the session"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The service must find the agent the way it would: on its PATH. The private
	// server inherits this PATH too, so set it before the server starts.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	wrapper, _ := privateMux(t)

	sockOut, err := exec.Command(wrapper, "display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		t.Fatalf("socket path: %v", err)
	}
	socket := strings.TrimSpace(string(sockOut))
	// Control: outside any profile, the socket IS connectable — so a refusal
	// inside the session means something.
	if err := exec.Command("/usr/bin/nc", "-U", "-w", "1", socket).Run(); err != nil {
		t.Skipf("cannot even connect to the multiplexer's socket from here (%v); the control for this test does not hold", err)
	}

	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  auth) echo '{"loggedIn": true, "authMethod": "stand-in"}'; exit 0 ;;
esac
R="$PWD/results"
mkdir -p "$R"
echo "$TMPDIR" > "$R/tmpdir"
if ( echo x > "$PWD/inside.txt" ) 2>/dev/null; then echo ok > "$R/write-cwd"; else echo denied > "$R/write-cwd"; fi
if ( echo x > "%[1]s/new.txt" ) 2>/dev/null; then echo ok > "$R/write-outside"; else echo denied > "$R/write-outside"; fi
if ( cat "%[2]s" ) >/dev/null 2>&1; then echo ok > "$R/read-outside"; else echo denied > "$R/read-outside"; fi
if /usr/bin/nc -U -w 1 "%[3]s" </dev/null >/dev/null 2>&1; then echo connected > "$R/socket"; else echo denied > "$R/socket"; fi
echo done > "$R/done"
[ -n "$MUSTER_TEST_NO_SLEEP" ] || sleep 60
`, outside, secret, socket)
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// The control: the same script, outside any profile, does every one of the
	// things the session below must be refused. Without this a "denied" could
	// be a typo in the script.
	controlDir := filepath.Join(base, "control")
	if err := os.MkdirAll(controlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctl := exec.Command(filepath.Join(binDir, "claude"))
	ctl.Dir = controlDir
	ctl.Env = append(os.Environ(), "MUSTER_TEST_NO_SLEEP=1", "TMPDIR="+t.TempDir())
	if out, err := ctl.CombinedOutput(); err != nil {
		t.Fatalf("control run: %v (%s)", err, out)
	}
	for name, want := range map[string]string{"write-cwd": "ok", "write-outside": "ok", "read-outside": "ok", "socket": "connected"} {
		b, err := os.ReadFile(filepath.Join(controlDir, "results", name))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Fatalf("control %s = %q (%v), want %q: the test cannot tell a refusal from a failure", name, b, err, want)
		}
	}
	if err := os.Remove(filepath.Join(outside, "new.txt")); err != nil {
		t.Fatalf("control left no file outside: %v", err)
	}

	st, err := state.Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	d := New("testbox",
		WithBinary(wrapper),
		WithBareExec(), // no login shell: the test PATH is the service's PATH
		WithState(st),
		WithRecordRoot(filepath.Join(cfg, agentProjectsDir)),
		WithSandbox(SandboxOptions{Home: home}),
	)
	caps := d.Capabilities()
	if caps.Sandbox == nil {
		t.Fatalf("the host applies a profile and the stand-in agent reports logged in, yet no sandbox is declared: %q", caps.SandboxUnavailable)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ref, err := d.Create(ctx, testCaller, "live-1", fleet.SessionSpec{
		Machine: "testbox", Cwd: fleet.AbsolutePath(work), Name: "confined",
		Sandbox: &fleet.SandboxSpec{}, RemoteControl: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	results := filepath.Join(work, "results")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(results, "done")); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(results, name))
		if err != nil {
			t.Errorf("the session recorded no %s: %v", name, err)
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	if read("done") != "done" {
		t.Fatal("the stand-in agent never ran to its end inside the profile")
	}
	for name, want := range map[string]string{
		"write-cwd":     "ok",     // its working directory is its own
		"write-outside": "denied", // a write outside the allowed paths fails inside the session
		"read-outside":  "denied", // so does a read
		"socket":        "denied", // and the multiplexer's own socket cannot be reached
	} {
		if got := read(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("a file appeared outside the allowed paths")
	}
	layout := newSandboxLayout(filepath.Join(base, "state", "sandbox"), ref.ID)
	if tmp := read("tmpdir"); tmp != layout.tmp {
		t.Errorf("TMPDIR = %q, want the session's private %q", tmp, layout.tmp)
	}

	got, err := d.State(ctx, testCaller, ref.SessionRef)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if got.Sandbox == nil || got.Sandbox.Mechanism != sandbox.Mechanism || len(got.Sandbox.Denies) != 3 {
		t.Fatalf("state.sandbox = %+v", got.Sandbox)
	}

	if _, err := d.Close(ctx, testCaller, ref.SessionRef); err != nil {
		t.Logf("close: %v", err) // the weak-guarantee refusal is not this test's subject
	}
}

func boolPtr(b bool) *bool { return &b }
