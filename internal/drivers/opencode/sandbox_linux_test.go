package opencode

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
)

// muster #439: the Linux mechanism, through this driver. The test binary is the
// launcher (fakeopencode_test.go's TestMain) and the stand-in runtime, so the
// server a create starts is a real process confined by a real policy, and a
// command run "in the session" is a child of it.
//
// What the kernel needs is Landlock ABI 6. On a host with less the driver does
// not offer the mechanism, so these skip and say why; MUSTER_SANDBOX_REQUIRE=1,
// set on the lane that has such a kernel, makes the skip a failure.

// linuxSandboxDriver names this test binary as the launcher for the test and
// returns a driver built after that, so its capability is measured with one.
func linuxSandboxDriver(t *testing.T, extra ...Option) (*Driver, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := sandbox.Launcher()
	sandbox.SetLauncher(exe)
	t.Cleanup(func() { sandbox.SetLauncher(prev) })
	d, root := newIsolatedDriver(t, extra...)
	if d.Capabilities().Sandbox == nil {
		msg := "this host cannot enforce a sandbox: " + d.sandboxWhy
		if os.Getenv("MUSTER_SANDBOX_REQUIRE") == "1" {
			t.Fatalf("%s, and MUSTER_SANDBOX_REQUIRE=1 says this lane must have one", msg)
		}
		t.Skip(msg)
	}
	return d, root
}

func TestLinuxSandboxIsOfferedAndSaysWhichMechanism(t *testing.T) {
	d, _ := linuxSandboxDriver(t)
	sup := d.Capabilities().Sandbox
	if sup.Mechanism != sandbox.MechanismLinux || len(sup.Denies) != 3 || len(sup.Network) != 2 {
		t.Fatalf("support = %+v", sup)
	}
	// What this driver adds to the host's answer: the private package cache.
	if len(sup.PackageCache) != 2 || len(sup.PackageManagers) == 0 {
		t.Errorf("the driver's own package-cache modes are missing: %+v", sup)
	}
}

func TestLinuxSandboxedSessionIsConfinedAndTheServiceStillReachesItsServer(t *testing.T) {
	d, _ := linuxSandboxDriver(t)
	exe, _ := os.Executable()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("NOT-FOR-A-SESSION"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A real listener outside, to dial.
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
	addr := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	control := mustCreate(t, d, "k-control", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if r := sh(t, d, control.ID, "cat "+outside); r.Rc != 0 || !strings.Contains(r.Out, "NOT-FOR-A-SESSION") {
		t.Fatalf("control: an unsandboxed session should read the file, or the probe proves nothing: %+v", r)
	}

	for _, network := range []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed} {
		// The create returning at all is the first half of the proof: the service,
		// which is not confined, reached the runtime's server over loopback, and the
		// server — under a closed network — was allowed to bind its own port.
		sess, cwd := sandboxedSession(t, d, "k-"+string(network), fleet.SandboxSpec{Network: network})

		if r := sh(t, d, sess.ID, "cat "+outside); r.Rc == 0 || strings.Contains(r.Out, "NOT-FOR-A-SESSION") {
			t.Fatalf("%s: the session read a file outside its grants: %+v", network, r)
		}
		if r := sh(t, d, sess.ID, "cd "+cwd+" && echo hi > f && cat f"); r.Rc != 0 || !strings.Contains(r.Out, "hi") {
			t.Fatalf("%s: the session cannot work in its own directory: %+v", network, r)
		}
		if r := sh(t, d, sess.ID, `echo t > "$TMPDIR/x" && cat "$TMPDIR/x"`); r.Rc != 0 {
			t.Fatalf("%s: the session's private temp directory is not writable: %+v", network, r)
		}
		dial := sh(t, d, sess.ID, exe+" __dial "+addr)
		switch network {
		case fleet.SandboxNetworkOpen:
			if dial.Rc != 0 {
				t.Fatalf("open: the session cannot reach the network: %+v", dial)
			}
		case fleet.SandboxNetworkClosed:
			if dial.Rc == 0 {
				t.Fatalf("closed: the session connected to loopback, which the Linux mechanism refuses: %+v", dial)
			}
		}

		st, err := d.State(context.Background(), testReq, sess.SessionRef)
		if err != nil {
			t.Fatal(err)
		}
		if st.Sandbox == nil || st.Sandbox.Mechanism != sandbox.MechanismLinux || st.Sandbox.Network != network {
			t.Fatalf("%s: the state does not report the mechanism and posture in force: %+v", network, st.Sandbox)
		}
	}
}

func TestLinuxSandboxedCreateRefusesAGrantThatDoesNotExistAndStartsNothing(t *testing.T) {
	d, _ := linuxSandboxDriver(t)
	missing := filepath.Join(t.TempDir(), "not-there")
	for _, spec := range []fleet.SandboxSpec{
		{ReadPaths: []fleet.AbsolutePath{fleet.AbsolutePath(missing)}},
		{WritePaths: []fleet.AbsolutePath{fleet.AbsolutePath(missing)}},
	} {
		_, err := d.Create(context.Background(), testReq, "k-missing", fleet.SessionSpec{
			Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &spec,
		})
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid || !strings.Contains(fe.Message, "does not exist") || !strings.Contains(fe.Message, missing) {
			t.Fatalf("a grant that is not there: want an invalid refusal naming it, got %v", err)
		}
	}
	if len(d.live) != 0 {
		t.Fatal("a refused create started a server")
	}
}

func TestLinuxSandboxReportsOnlyWhatIsGranted(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "operator-configured-but-absent")
	d, _ := linuxSandboxDriver(t, WithSandboxReadPaths([]string{gone}))
	sess, _ := sandboxedSession(t, d, "k-report", fleet.SandboxSpec{})
	st, err := d.State(context.Background(), testReq, sess.SessionRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Sandbox.ReadPaths {
		if string(p) == gone {
			t.Fatalf("the report names a read grant the policy never made: %v", st.Sandbox.ReadPaths)
		}
		if _, err := os.Stat(string(p)); err != nil {
			t.Errorf("the report names %s, which does not exist: %v", p, err)
		}
	}
}

// A copy on this platform is never reported as a clone: GNU cp reads -c as
// --preserve=context and succeeds on a plain copy, so passing it would have
// made every private cache read "clone".
func TestLinuxPrivateCacheCopyIsReportedAsACopy(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "pkgcache")
	how, err := copyTree(context.Background(), seedTree(t), dst)
	if err != nil || how != "copy" {
		t.Fatalf("copyTree = %q, %v; want copy", how, err)
	}
}
