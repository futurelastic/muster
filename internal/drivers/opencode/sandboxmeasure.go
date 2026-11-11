package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
)

// The sandbox capability is a measurement of THIS runtime, not only of the host
// (muster #456).
//
// "The host can confine a process" says nothing about whether the runtime works
// inside the policy: an interpreter outside the granted directories, a file it
// needs that the policy denies. Declared on the host check alone, such a runtime
// fails every sandboxed create with a generic start error, where the capability
// should have been withheld with the reason. So the capability is declared only
// when the runtime answered its own cheap self-report (its version, as the probe
// at start asks it) started the way a create starts it: through the same launcher,
// under the same plan, in the same kind of environment.
//
// The host answer stays what it was, made once when the driver starts
// (sandboxSupport: d.sandbox, d.sandboxWhy). This is the layer on top of it, and
// the only part of the capability that changes while the service runs, because
// what it measures can: a runtime installed somewhere else, a grant the operator
// has since added. The answer is kept for the lifetimes the multiplexer driver
// keeps its own (internal/sandbox/measure.go), so the two never disagree about how
// long to believe one.

// sandboxProbeTimeout bounds one measurement. A variable so a test can shorten it.
var sandboxProbeTimeout = 20 * time.Second

// sandboxMeasurement is the last answer to "does the runtime start under the
// policy here".
type sandboxMeasurement struct {
	// mu guards the fields below and is never held across a probe: Capabilities is
	// read on the path of nearly every request (for its deadline), so a reader
	// must never wait behind a measurement that is running.
	mu         sync.Mutex
	valid      bool
	at         time.Time
	answered   bool
	why        string
	refreshing bool
	// failures is how many measurements in a row found the runtime not working.
	failures int
	// probeMu serialises the measurements themselves.
	probeMu sync.Mutex
}

// peek returns the cached answer and whether it is still inside its lifetime.
func (m *sandboxMeasurement) peek(now time.Time, strict bool) (answered bool, why string, known, fresh bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid {
		return false, "", false, false
	}
	age := now.Sub(m.at)
	return m.answered, m.why, true, age >= 0 && age < sandbox.MeasuredTTL(m.answered, strict, m.failures)
}

// measuresRuntime reports whether the capability needs the runtime measured at
// all: only where the host offers one. In shared mode and on a host that cannot
// confine anything there is nothing to start under a policy, and the host's own
// answer, with its reason, is the whole of it.
func (d *Driver) measuresRuntime() bool { return d.sandbox != nil && d.shared == nil }

// sandboxSupportNow is Capabilities' view: the support this driver declares, or
// nil and the reason.
//
// It does not wait. Once an answer exists it is returned at once, and a stale one
// is refreshed in the background (the next read sees the new answer); only the very
// first read, which is the service registering the driver before it serves
// anything, measures in line.
func (d *Driver) sandboxSupportNow() (*fleet.SandboxSupport, string) {
	if !d.measuresRuntime() {
		return d.sandbox, d.sandboxWhy
	}
	m := &d.sandboxMeasured
	answered, why, known, fresh := m.peek(d.now(), false)
	if !known {
		ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout+5*time.Second)
		defer cancel()
		return d.sandboxCurrent(ctx, false)
	}
	if !fresh {
		d.refreshSandboxAsync()
	}
	return d.declared(answered, why)
}

// declared is the support a measurement's answer means: the host's, when the
// runtime started under the policy, and nothing, with the measurement's reason,
// when it did not.
func (d *Driver) declared(answered bool, why string) (*fleet.SandboxSupport, string) {
	if answered {
		return d.sandbox, ""
	}
	return nil, why
}

// sandboxCurrent returns a measurement inside its lifetime, taking a fresh one
// when the cached one is stale. It waits: it is for a create, which is rare and
// must not act on a stale answer. Concurrent callers share one probe. strict is
// true for a caller acting on the answer (sandbox.MeasuredTTL).
func (d *Driver) sandboxCurrent(ctx context.Context, strict bool) (*fleet.SandboxSupport, string) {
	if !d.measuresRuntime() {
		return d.sandbox, d.sandboxWhy
	}
	m := &d.sandboxMeasured
	if answered, why, known, fresh := m.peek(d.now(), strict); known && fresh {
		return d.declared(answered, why)
	}
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	// Another caller may have measured while this one waited.
	if answered, why, known, fresh := m.peek(d.now(), strict); known && fresh {
		return d.declared(answered, why)
	}
	why := d.measureSandbox(ctx)
	if ctx.Err() != nil && why != "" {
		// Our caller gave up; that says nothing about the runtime and must not be
		// remembered as an answer about it.
		return nil, "the sandbox measurement was interrupted before it finished"
	}
	m.mu.Lock()
	m.answered, m.why, m.at, m.valid = why == "", why, d.now(), true
	if why != "" {
		m.failures++
	} else {
		m.failures = 0
	}
	m.mu.Unlock()
	return d.declared(why == "", why)
}

// refreshSandboxAsync retakes the measurement without making a caller wait for it.
// At most one runs at a time.
func (d *Driver) refreshSandboxAsync() {
	m := &d.sandboxMeasured
	m.mu.Lock()
	if m.refreshing {
		m.mu.Unlock()
		return
	}
	m.refreshing = true
	m.mu.Unlock()
	run := func() {
		defer func() {
			m.mu.Lock()
			m.refreshing = false
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout+5*time.Second)
		defer cancel()
		d.sandboxCurrent(ctx, false)
	}
	if d.sandboxGo != nil {
		d.sandboxGo(run)
		return
	}
	go run()
}

// sandboxReasonPrefix starts the reason a runtime that did not start under the
// policy is withheld with, so a caller reading sandboxUnavailable can tell this
// from the host's own refusal.
const sandboxReasonPrefix = "the runtime did not start under the sandbox policy on this host: "

// measureSandbox is the measurement: it runs the runtime's own version report
// through the launcher, under the plan a create would build, and returns "" when
// it answered or the reason it did not.
//
// The plan is the strictest the capability offers (a closed network): everything
// that network posture takes away, the open one takes away less of, so a runtime
// that answers under it answers under both, and the capability lists both.
//
// d.sandboxProbe replaces the whole of it, for a test that is not about starting a
// process.
func (d *Driver) measureSandbox(ctx context.Context) string {
	if d.sandboxProbe != nil {
		return d.sandboxProbe(ctx)
	}
	fail := func(format string, args ...any) string {
		return sandboxReasonPrefix + fmt.Sprintf(format, args...)
	}
	// Under the temp directory and not the session root: nothing that sweeps the
	// session root for directories no record names can mistake this for one.
	root, err := os.MkdirTemp("", "muster-sandbox-probe-")
	if err != nil {
		return fail("making a probe directory: %v", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	cwd := filepath.Join(root, "cwd")
	dir := filepath.Join(root, "session")
	for _, p := range append([]string{cwd}, scopedSubdirs(dir)...) {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fail("making a probe directory: %v", err)
		}
	}
	spec := fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Sandbox: &fleet.SandboxSpec{Network: fleet.SandboxNetworkClosed}}
	plan, err := d.planSandboxFor(ctx, spec, dir, "")
	if err != nil {
		return fail("planning the probe: %v", err)
	}
	launch, err := plan.launcher()
	if err != nil {
		return fail("rendering the probe policy: %v", err)
	}
	port, err := freePort()
	if err != nil {
		return fail("%v", err)
	}
	wrap, err := launch(port)
	if err != nil {
		return fail("preparing the probe's command: %v", err)
	}

	pctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, wrap[0], append(append([]string(nil), wrap[1:]...), d.bin, "--version")...)
	cmd.Dir = cwd
	// The environment a create gives, without the server's credential: this run
	// serves nothing, so there is none to give.
	cmd.Env = d.buildEnv(dir, nil)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	if ctx.Err() != nil {
		return fail("interrupted")
	}
	if errors.Is(pctx.Err(), context.DeadlineExceeded) {
		return fail("its version report did not answer within %s", sandboxProbeTimeout)
	}
	return fail("%s", probeFailure(err, out))
}

// scopedSubdirs are the directories a session's environment points into, under dir.
func scopedSubdirs(dir string) []string {
	subs := sessionSubdirs()
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = filepath.Join(dir, filepath.FromSlash(s))
	}
	return out
}

// probeFailureMax bounds the runtime's own words in a reason.
const probeFailureMax = 160

// probeFailure says why a version report that was started did not succeed.
//
// The launcher's own line, when there is one, is the reason: it says what it
// declined to do, in words this service owns. Otherwise the runtime started and
// failed, and the exit status is followed by the last line it printed. Printing a
// runtime's text is a thing this driver is careful about for a session's log,
// which can carry a command someone typed; here it is the output of one version
// check, run with an environment that holds no credential, in a directory that
// holds nothing of any session's, and it is what an operator needs to see
// ("env: 'node': Permission denied") instead of a status they must reproduce by
// hand. It is cut to one short line.
func probeFailure(err error, out []byte) string {
	lines := strings.Split(string(out), "\n")
	for _, ln := range lines {
		if why, ok := strings.CutPrefix(strings.TrimSpace(ln), sandbox.LauncherMessagePrefix); ok {
			return "the launcher declined to start it: " + why
		}
	}
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			last = t
			break
		}
	}
	if last == "" {
		return fmt.Sprintf("it exited without a version (%v)", err)
	}
	last = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, last)
	if len(last) > probeFailureMax {
		last = last[:probeFailureMax] + "…"
	}
	return fmt.Sprintf("it exited without a version (%v): %s", err, last)
}
