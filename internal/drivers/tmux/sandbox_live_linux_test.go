package tmux

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
	"github.com/futurelastic/muster/internal/state"
)

// muster #439, the real-substrate half on Linux: a REAL multiplexer server (a
// private one), the REAL launcher (this test binary, which is the service's
// stand-in), the REAL kernel, and a stand-in agent that tries to get out. The
// fake-host tests prove what the driver would run and write; this proves what the
// kernel then refuses, from inside a session's own process.
//
// It needs a kernel with the Landlock ABI the mechanism is offered on, and a
// multiplexer on PATH; without either it skips and says why. With
// MUSTER_SANDBOX_REQUIRE=1 (the dedicated lane) a missing kernel feature is a
// failure, so that lane cannot go quietly grey.
func TestLiveLinuxSandboxedSessionIsConfined(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := sandbox.Launcher()
	sandbox.SetLauncher(exe)
	t.Cleanup(func() { sandbox.SetLauncher(prev) })
	if sup, why := sandbox.HostSupport(); sup == nil {
		msg := "this host cannot enforce a sandbox: " + why
		if os.Getenv("MUSTER_SANDBOX_REQUIRE") == "1" {
			t.Fatalf("%s, and MUSTER_SANDBOX_REQUIRE=1 says this lane must have one", msg)
		}
		t.Skip(msg)
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
	// The dial tool lives with the agent, where the session may run it: the test
	// binary, copied. A link would resolve to a path the session may not read.
	tool := filepath.Join(binDir, "dial")
	if err := copyExecutable(exe, tool); err != nil {
		t.Fatal(err)
	}
	// A loopback listener outside, and a secret in this process's environment.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	// Another process of the same user, with a secret in the environment it was
	// started with (which is what /proc/<pid>/environ holds; a later setenv in this
	// process would not be in it).
	holder := exec.Command("sleep", "120")
	holder.Env = append(os.Environ(), "MUSTER_PROBE_SECRET=hunter2")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	t.Setenv("MUSTER_PROBE_PID", strconv.Itoa(holder.Process.Pid))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	wrapper, _ := privateMux(t)

	sockOut, err := exec_(wrapper, "display-message", "-p", "#{socket_path}")
	if err != nil {
		t.Fatalf("socket path: %v", err)
	}
	socket := strings.TrimSpace(sockOut)

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
if ( cat "/proc/$MUSTER_PROBE_PID/environ" ) 2>/dev/null | grep -q hunter2; then echo read > "$R/environ"; else echo denied > "$R/environ"; fi
if "%[3]s" __unixdial "%[4]s" >/dev/null 2>&1; then echo connected > "$R/socket"; else echo denied > "$R/socket"; fi
if "%[3]s" __tcpdial "%[5]s" >/dev/null 2>&1; then echo connected > "$R/tcp"; else echo denied > "$R/tcp"; fi
echo done > "$R/done"
[ -n "$MUSTER_TEST_NO_SLEEP" ] || sleep 60
`, outside, secret, tool, socket, ln.Addr().String())
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// The control: the same script, outside any profile, does every one of the
	// things the session below must be refused.
	controlDir := filepath.Join(base, "control")
	if err := os.MkdirAll(controlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runIn(controlDir, "MUSTER_TEST_NO_SLEEP=1", "TMPDIR="+t.TempDir()); err != nil {
		t.Fatalf("control run: %v (%s)", err, out)
	}
	for name, want := range map[string]string{"write-cwd": "ok", "write-outside": "ok", "read-outside": "ok", "environ": "read", "socket": "connected", "tcp": "connected"} {
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
	if caps.Sandbox == nil || caps.Sandbox.Mechanism != sandbox.MechanismLinux {
		t.Fatalf("the host enforces the mechanism and the stand-in agent reports logged in, yet it is not declared: %+v / %q", caps.Sandbox, caps.SandboxUnavailable)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ref, err := d.Create(ctx, testCaller, "live-linux-1", fleet.SessionSpec{
		Machine: "testbox", Cwd: fleet.AbsolutePath(work), Name: "confined",
		Sandbox: &fleet.SandboxSpec{Network: fleet.SandboxNetworkClosed}, RemoteControl: boolPtr(false),
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
		t.Fatal("the stand-in agent never ran to its end inside the policy")
	}
	for name, want := range map[string]string{
		"write-cwd":     "ok",     // its working directory is its own
		"write-outside": "denied", // a write outside the allowed paths fails inside the session
		"read-outside":  "denied", // so does a read
		"environ":       "denied", // another process's environment, with /proc readable
		"socket":        "denied", // and the multiplexer's own socket cannot be reached
		"tcp":           "denied", // a closed network reaches not even loopback
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
	if _, err := os.Stat(filepath.Join(layout.dir, "policy.json")); err != nil {
		t.Errorf("the policy file is not where the launcher read it: %v", err)
	}

	got, err := d.State(ctx, testCaller, ref.SessionRef)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if got.Sandbox == nil || got.Sandbox.Mechanism != sandbox.MechanismLinux || got.Sandbox.Network != fleet.SandboxNetworkClosed || len(got.Sandbox.Denies) != 3 {
		t.Fatalf("state.sandbox = %+v", got.Sandbox)
	}

	if _, err := d.Close(ctx, testCaller, ref.SessionRef); err != nil {
		t.Logf("close: %v", err) // the weak-guarantee refusal is not this test's subject
	}
}

// copyExecutable copies a file and keeps it executable.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// exec_ runs a program and returns its output.
func exec_(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// runIn runs the stand-in agent outside any policy, in dir, with extra environment.
func runIn(dir string, env ...string) ([]byte, error) {
	cmd := exec.Command("claude")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}
