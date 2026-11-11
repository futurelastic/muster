package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
)

// muster #456: the sandbox capability is declared only when the runtime itself
// answered its version report under the policy a create would use.
//
// Two kinds of test. The lifetime of the answer is checked with a driver built by
// hand and a probe that is a function, so it runs on every platform and needs no
// process. What the probe does — start the runtime through the launcher, under a
// plan, in a create's kind of environment — is checked with stand-in runtimes on
// a host that can confine one: macOS, and Linux where the kernel has Landlock
// ABI 6 (MUSTER_SANDBOX_REQUIRE=1 turns the skip for a missing kernel into a failure,
// as in sandbox_linux_test.go).

// ---- the lifetime of the answer --------------------------------------------

type measureClock struct{ now time.Time }

func (c *measureClock) Now() time.Time          { return c.now }
func (c *measureClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// measuredDriver is a driver with a host that can confine and a probe that is a
// function: answer is what it says (empty: the runtime started), probes counts
// how often it ran.
func measuredDriver(answer *atomic.Value, probes *atomic.Int32) (*Driver, *measureClock) {
	clock := &measureClock{now: time.Unix(1_800_000_000, 0)}
	d := &Driver{
		sandbox: &fleet.SandboxSupport{Mechanism: "test-mechanism"},
		now:     clock.Now,
		// The refresh the service runs in the background is run in line, so a test
		// reads its effect without waiting for a goroutine.
		sandboxGo: func(f func()) { f() },
		sandboxProbe: func(context.Context) string {
			probes.Add(1)
			return answer.Load().(string)
		},
	}
	return d, clock
}

func TestSandboxMeasuredIsDeclaredOnlyWhenTheRuntimeAnsweredAndIsKeptForTheMultiplexersLifetimes(t *testing.T) {
	var answer atomic.Value
	var probes atomic.Int32
	answer.Store("")
	d, clock := measuredDriver(&answer, &probes)

	sup, why := d.sandboxSupportNow()
	if sup == nil || why != "" || probes.Load() != 1 {
		t.Fatalf("first read: support=%v why=%q probes=%d; want it declared after one measurement", sup, why, probes.Load())
	}
	if sup != d.sandbox {
		t.Fatalf("what is declared is the host's support, not a copy that could drift from it")
	}
	// Inside its lifetime the answer is returned without another run.
	clock.advance(sandbox.MeasuredFoundTTL - time.Second)
	if sup, _ := d.sandboxSupportNow(); sup == nil || probes.Load() != 1 {
		t.Fatalf("a positive answer was measured again inside %v (probes=%d)", sandbox.MeasuredFoundTTL, probes.Load())
	}
	// Past it, the stale answer is returned at once and the refresh is what changes
	// the next read: a runtime that stopped starting is withheld from then on.
	answer.Store("it needs a file the policy denies")
	clock.advance(2 * time.Second)
	if sup, _ := d.sandboxSupportNow(); sup == nil || probes.Load() != 2 {
		t.Fatalf("a stale answer should be returned while it is refreshed (probes=%d)", probes.Load())
	}
	sup, why = d.sandboxSupportNow()
	if sup != nil || why != "it needs a file the policy denies" {
		t.Fatalf("after the refresh: support=%v why=%q; want it withheld with the reason", sup, why)
	}
}

func TestSandboxMeasuredFailureIsTrustedBrieflyAndBacksOffInTheBackgroundNotForACreate(t *testing.T) {
	var answer atomic.Value
	var probes atomic.Int32
	answer.Store("not yet")
	d, clock := measuredDriver(&answer, &probes)

	if sup, why := d.sandboxSupportNow(); sup != nil || why != "not yet" {
		t.Fatalf("first read: support=%v why=%q", sup, why)
	}
	// A failure is believed for the short lifetime, and then measured again in the
	// background; each failure in a row doubles the wait, up to the limit.
	wait := sandbox.MeasuredFailTTL
	for i := 2; i <= 6; i++ {
		before := probes.Load()
		clock.advance(wait - time.Second)
		d.sandboxSupportNow()
		if probes.Load() != before {
			t.Fatalf("round %d: measured again %v after a failure, before the wait of %v", i, wait-time.Second, wait)
		}
		clock.advance(2 * time.Second)
		d.sandboxSupportNow()
		if probes.Load() != before+1 {
			t.Fatalf("round %d: not measured again after the wait of %v", i, wait)
		}
		if wait *= 2; wait > sandbox.MeasuredFailMaxTTL {
			wait = sandbox.MeasuredFailMaxTTL
		}
	}
	// A create waits for its answer and is not made to sit out the backoff: the
	// runtime may have been fixed a moment ago.
	answer.Store("")
	clock.advance(sandbox.MeasuredFailTTL + time.Second)
	before := probes.Load()
	if sup, why := d.sandboxCurrent(context.Background(), true); sup == nil || why != "" || probes.Load() != before+1 {
		t.Fatalf("a create after the short wait: support=%v why=%q probes=%d->%d; want a fresh measurement that found it", sup, why, before, probes.Load())
	}
}

func TestSandboxMeasuredInterruptedMeasurementIsNotRemembered(t *testing.T) {
	var answer atomic.Value
	var probes atomic.Int32
	answer.Store("")
	d, _ := measuredDriver(&answer, &probes)
	d.sandboxProbe = func(ctx context.Context) string {
		probes.Add(1)
		<-ctx.Done()
		return "interrupted"
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sup, why := d.sandboxCurrent(ctx, true); sup != nil || !strings.Contains(why, "interrupted") {
		t.Fatalf("support=%v why=%q; want it withheld as interrupted", sup, why)
	}
	// What a caller who gave up saw says nothing about the runtime: the next
	// caller measures, and finds it working.
	d.sandboxProbe = func(context.Context) string { probes.Add(1); return "" }
	if sup, why := d.sandboxCurrent(context.Background(), true); sup == nil || why != "" {
		t.Fatalf("after an interrupted measurement: support=%v why=%q; want a fresh measurement", sup, why)
	}
}

// Where the host offers nothing, or there is one shared server, there is no
// runtime to start under a policy: the host's own answer, with its reason, is all
// the capability has, and nothing is run.
func TestSandboxMeasuredIsNotTakenWhereTheHostOffersNothingOrServerIsShared(t *testing.T) {
	var probes atomic.Int32
	probe := func(context.Context) string { probes.Add(1); return "" }
	none := &Driver{sandboxWhy: "this host cannot confine anything", sandboxProbe: probe, now: time.Now}
	if sup, why := none.sandboxSupportNow(); sup != nil || why != "this host cannot confine anything" {
		t.Fatalf("no host support: support=%v why=%q", sup, why)
	}
	shared := &Driver{
		sandbox: &fleet.SandboxSupport{Mechanism: "test-mechanism"}, shared: &server{},
		sandboxProbe: probe, now: time.Now,
	}
	if sup, _ := shared.sandboxSupportNow(); sup == nil {
		t.Fatalf("shared mode keeps the answer it had")
	}
	if probes.Load() != 0 {
		t.Fatalf("a runtime was started under a policy where there is none to measure (%d runs)", probes.Load())
	}
}

// A create that needs confinement is refused with the measured reason before any
// directory or process exists, and a session recorded as sandboxed is not
// relaunched without it.
func TestSandboxMeasuredRefusalReachesACreateAndARelaunch(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	d.sandbox = &fleet.SandboxSupport{Mechanism: "test-mechanism"}
	d.sandboxProbe = func(context.Context) string { return sandboxReasonPrefix + "it needs a file the policy denies" }

	_, err := d.Create(context.Background(), testReq, "k-measured", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{},
	})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || !strings.Contains(fe.Message, "it needs a file the policy denies") ||
		!strings.Contains(fe.Message, "refusing rather than start the session unconfined") {
		t.Fatalf("a create that needs confinement: want an unsupported refusal carrying the measured reason, got %v", err)
	}
	if len(d.live) != 0 {
		t.Fatal("a refused create started a server")
	}
	if got := d.Capabilities(); got.Sandbox != nil || !strings.Contains(got.SandboxUnavailable, "it needs a file the policy denies") {
		t.Fatalf("capabilities: sandbox=%v unavailable=%q; want it absent with the measured reason", got.Sandbox, got.SandboxUnavailable)
	}
	dir := t.TempDir()
	d.sessionRoot = filepath.Dir(dir)
	err = d.checkRelaunchable(&sessionRecord{Dir: dir, Sandbox: &fleet.SandboxSpec{}})
	if err == nil || !strings.Contains(err.Error(), "it needs a file the policy denies") || !strings.Contains(err.Error(), "not relaunched unconfined") {
		t.Fatalf("a sandboxed session's relaunch: want a refusal carrying the measured reason, got %v", err)
	}
}

func TestProbeFailureSaysWhatTheLauncherSaidOrWhatTheRuntimePrintedLast(t *testing.T) {
	boom := errors.New("exit status 1")
	for _, c := range []struct {
		name string
		out  string
		want string
	}{
		{"the launcher's own line wins", "noise\nmuster sandbox: starting /x/opencode: permission denied\n", "the launcher declined to start it: starting /x/opencode: permission denied"},
		{"the runtime's last line", "first\n  env: 'node': Permission denied  \n\n", "it exited without a version (exit status 1): env: 'node': Permission denied"},
		{"nothing printed", "", "it exited without a version (exit status 1)"},
		{"control characters are not carried", "bad\x1b[31m\x00line", "it exited without a version (exit status 1): bad [31m line"},
	} {
		if got := probeFailure(boom, []byte(c.out)); got != c.want {
			t.Errorf("%s:\n  got  %q\n  want %q", c.name, got, c.want)
		}
	}
	long := strings.Repeat("x", 500)
	if got := probeFailure(boom, []byte(long)); len(got) > 300 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long line is cut to one short line, got %d bytes", len(got))
	}
}

// ---- the probe, against stand-in runtimes -----------------------------------

// hostSandboxDriver is a driver, with the stand-in runtime at bin, on a host that
// can confine one, or a skip saying why not (a failure where the lane must have
// one). The host's answer is read from the driver's own host check and not from
// Capabilities, because Capabilities is now the thing under test: a runtime that
// cannot start under the policy is meant to leave it absent on a host that can
// confine.
func hostSandboxDriver(t *testing.T, bin string, extra ...Option) *Driver {
	t.Helper()
	skipOnWindows(t, "the stand-in runtimes are shell scripts, and there is no sandbox to measure under")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := sandbox.Launcher()
	sandbox.SetLauncher(exe)
	t.Cleanup(func() { sandbox.SetLauncher(prev) })
	d, _ := newIsolatedDriver(t, append([]Option{WithBinary(bin)}, extra...)...)
	if d.sandbox == nil {
		msg := "this host cannot enforce a sandbox: " + d.sandboxWhy
		if runtime.GOOS == "linux" && os.Getenv("MUSTER_SANDBOX_REQUIRE") == "1" {
			t.Fatalf("%s, and MUSTER_SANDBOX_REQUIRE=1 says this lane must have one", msg)
		}
		t.Skip(msg)
	}
	return d
}

// standInRuntime writes an executable shell script named opencode in a directory of its
// own, which is the install root a create would grant.
func standInRuntime(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "install")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// outsideFile is a file that no create's policy grants: a directory of its own
// under the test's temp directory.
func outsideFile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "needed")
	if err := os.WriteFile(p, []byte("a file the runtime needs"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A runtime that answers only when it is NOT able to read a file outside the
// grants, and only when it is started the way a create starts it (the variable
// the driver gives every session): the unconfined probe at start sees neither and
// answers, so the driver starts; a measurement that ran the version report
// without the policy, or without a session's environment, would see the file and
// fail it — and the capability would be withheld on a host that can confine.
func TestSandboxMeasuredRuntimeIsStartedUnderThePolicyAndOffered(t *testing.T) {
	outside := outsideFile(t)
	bin := standInRuntime(t, fmt.Sprintf(`if [ -n "$OPENCODE_DISABLE_CLAUDE_CODE" ] && cat %q >/dev/null 2>&1; then
  echo "the version report ran without the policy" >&2
  exit 9
fi
echo "stand-in 1.0"
`, outside))
	d := hostSandboxDriver(t, bin)
	caps := d.Capabilities()
	if caps.Sandbox == nil || caps.SandboxUnavailable != "" {
		t.Fatalf("a runtime that answers under the policy was withheld: unavailable=%q", caps.SandboxUnavailable)
	}
}

// runtimeWithInterpreterOutsideTheGrants is a stand-in runtime whose #! line names
// an interpreter in a directory no create's policy opens: a copy of sh. Linux
// only, because macOS kills a copy of one of its own binaries, so there is no
// interpreter to put outside the grants there. It runs where nothing confines it.
func runtimeWithInterpreterOutsideTheGrants(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("a copy of a system binary does not run on this platform")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh to copy: %v", err)
	}
	interp := filepath.Join(t.TempDir(), "elsewhere", "sh")
	if err := os.MkdirAll(filepath.Dir(interp), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := os.ReadFile(sh)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(interp, in, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "install")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!"+interp+"\necho \"stand-in 1.0\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil || !strings.Contains(string(out), "stand-in") {
		t.Fatalf("the stand-in does not run even without a policy: %v %s", err, out)
	}
	return bin
}

// The issue's own example: an interpreter outside the granted directories. The
// runtime starts where nothing confines it, and cannot where something does; the
// capability says so up front and a create is refused with that reason, where it
// used to fail with a generic start error.
func TestSandboxMeasuredRuntimeWhoseInterpreterIsOutsideTheGrantsIsWithheldWithAReason(t *testing.T) {
	// Unconfined it runs (the helper checks), so the driver starts, and what is
	// withheld below is the sandbox and not the runtime.
	bin := runtimeWithInterpreterOutsideTheGrants(t)
	d := hostSandboxDriver(t, bin)
	caps := d.Capabilities()
	if caps.Sandbox != nil {
		t.Fatalf("a runtime that cannot start under the policy was declared sandboxable: %+v", caps.Sandbox)
	}
	if !strings.HasPrefix(caps.SandboxUnavailable, sandboxReasonPrefix) {
		t.Fatalf("the reason does not say it is the runtime that did not start: %q", caps.SandboxUnavailable)
	}
	if runtime.GOOS == "linux" && !strings.Contains(caps.SandboxUnavailable, "the launcher declined to start it: starting "+bin) {
		t.Fatalf("on Linux the launcher's own reason should reach the capability: %q", caps.SandboxUnavailable)
	}
	_, err := d.Create(context.Background(), testReq, "k-interp", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{},
	})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || !strings.Contains(fe.Message, sandboxReasonPrefix) {
		t.Fatalf("a sandboxed create: want an unsupported refusal with the measured reason, got %v", err)
	}
	if len(d.live) != 0 {
		t.Fatal("a refused create started a server")
	}
}

// What the runtime needs that the policy does not open is withheld with the
// runtime's own last words, and the operator's grant (the one place a read path
// beyond the defaults can come from) is what offers it again.
func TestSandboxMeasuredRuntimeThatNeedsAFileTheGrantsDoNotOpenIsOfferedOnceItIsGranted(t *testing.T) {
	outside := outsideFile(t)
	bin := standInRuntime(t, fmt.Sprintf(`cat %q >/dev/null || { echo "cannot read the file this runtime needs" >&2; exit 1; }
echo "stand-in 1.0"
`, outside))
	d := hostSandboxDriver(t, bin)
	caps := d.Capabilities()
	if caps.Sandbox != nil {
		t.Fatalf("a runtime that needs a file outside the grants was declared sandboxable")
	}
	if !strings.Contains(caps.SandboxUnavailable, "cannot read the file this runtime needs") {
		t.Fatalf("the reason should carry what the runtime said: %q", caps.SandboxUnavailable)
	}

	granted := hostSandboxDriver(t, bin, WithSandboxReadPaths([]string{filepath.Dir(outside)}))
	if granted.Capabilities().Sandbox == nil {
		t.Fatalf("the same runtime with its directory granted should be offered: %q", granted.Capabilities().SandboxUnavailable)
	}
}

// A runtime that does not answer in time is withheld with that, and the process
// is not left behind.
func TestSandboxMeasuredRuntimeThatDoesNotAnswerInTimeIsWithheld(t *testing.T) {
	prev := sandboxProbeTimeout
	sandboxProbeTimeout = 700 * time.Millisecond
	t.Cleanup(func() { sandboxProbeTimeout = prev })
	// Only a start that looks like a session's hangs: the unconfined probe at
	// start must answer, or the driver would never be built.
	bin := standInRuntime(t, `if [ -n "$OPENCODE_DISABLE_CLAUDE_CODE" ]; then exec sleep 30; fi
echo "stand-in 1.0"
`)
	d := hostSandboxDriver(t, bin)
	started := time.Now()
	caps := d.Capabilities()
	if caps.Sandbox != nil || !strings.Contains(caps.SandboxUnavailable, "did not answer within") {
		t.Fatalf("a runtime that hangs: sandbox=%v unavailable=%q", caps.Sandbox, caps.SandboxUnavailable)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the measurement waited %v for a runtime that was never going to answer", took)
	}
}
