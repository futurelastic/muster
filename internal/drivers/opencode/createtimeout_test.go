package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/credrule"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// A create that runs out of time says WHICH budget ran out, and is never a
// refusal (muster #476). These tests cover the three things that end a create
// early, the steps that used to turn one into "invalid", and the phases the
// create notes so a slow start can be attributed.

// shortenReady makes this driver's own readiness budget d for the test, and the
// only one: the reserve is set so large that a request's deadline never extends it
// (muster #478), which is what every test below that is about the own budget means
// by it. It is a package variable, which is safe only because no test in this
// package runs in parallel (availability_test.go says the same of the sandbox
// probe's).
func shortenReady(t *testing.T, d time.Duration) {
	t.Helper()
	old := readyTimeout
	readyTimeout = d
	t.Cleanup(func() { readyTimeout = old })
	reserveReady(t, time.Hour)
}

// reserveReady sets what the wait for readiness leaves of the request's deadline for
// the steps after it (muster #478); a test about the extension calls it after
// shortenReady.
func reserveReady(t *testing.T, d time.Duration) {
	t.Helper()
	old := readyReserve
	readyReserve = d
	t.Cleanup(func() { readyReserve = old })
}

// slowToAnswer is a runtime that answers 503 until it has been up for after, and 200
// from then on: alive, and ready only late.
func slowToAnswer(t *testing.T, after time.Duration) *httptest.Server {
	t.Helper()
	up := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(up) < after {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// notReady is a runtime that answers every request with 503: up, and not ready.
func notReady(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stepTimeoutOf(t *testing.T, err error) *driver.StepTimeout {
	t.Helper()
	var st *driver.StepTimeout
	if !errors.As(err, &st) {
		t.Fatalf("err = %v, want a step timeout", err)
	}
	return st
}

// The driver's own readiness budget ending is reported as that budget, not as the
// caller's deadline: the message the service writes was "no answer within 30s"
// for a create that ended at 10 s.
func TestWaitReady_OwnBudgetIsNamed(t *testing.T) {
	shortenReady(t, 150*time.Millisecond)
	srv := notReady(t)

	start := time.Now()
	err := waitReady(context.Background(), srv.URL, "u", "p", make(chan struct{}))
	st := stepTimeoutOf(t, err)
	if st.Budget != 150*time.Millisecond {
		t.Errorf("Budget = %v, want the readiness budget of 150ms", st.Budget)
	}
	if st.Step != stepReady {
		t.Errorf("Step = %q, want %q", st.Step, stepReady)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to still read as a deadline", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v, want the readiness budget to end it", took)
	}
}

// The caller's deadline ending first is the caller's, and names no budget of the
// driver's.
func TestWaitReady_CallersDeadlineIsTheCallers(t *testing.T) {
	shortenReady(t, 10*time.Second)
	srv := notReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	st := stepTimeoutOf(t, waitReady(ctx, srv.URL, "u", "p", make(chan struct{})))
	if st.Budget != 0 {
		t.Errorf("Budget = %v, want none: the driver's own budget was not what ran out", st.Budget)
	}
	if !errors.Is(st, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's deadline", st)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v, want the caller's deadline to end it", took)
	}
}

// What a start may wait is the larger of its own budget and the request's deadline
// less the reserve for the steps after it (muster #478), and never less than its own
// budget: a deadline that leaves less does not shorten the wait, it ends it.
func TestReadyBudget(t *testing.T) {
	shortenReady(t, 10*time.Second)
	reserveReady(t, 6*time.Second)

	within := func(t *testing.T, got, want time.Duration) {
		t.Helper()
		// A deadline is read off the clock, so the budget is a little under the
		// figure it was built from, never over it.
		if got > want || got < want-time.Second {
			t.Errorf("budget = %v, want about %v", got, want)
		}
	}
	at := func(d time.Duration) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		t.Cleanup(cancel)
		return ctx
	}

	if got := readyBudget(context.Background()); got != 10*time.Second {
		t.Errorf("no deadline: budget = %v, want the readiness budget of 10s", got)
	}
	within(t, readyBudget(at(30*time.Second)), 24*time.Second) // the default deadline
	within(t, readyBudget(at(40*time.Second)), 34*time.Second)
	// It is named in the answer, so it is a figure a person can read.
	if got := readyBudget(at(30 * time.Second)); got%(100*time.Millisecond) != 0 {
		t.Errorf("budget = %v, want whole tenths of a second", got)
	}
	// A deadline with less than the budget and the reserve together to give leaves
	// the budget as it is, and so does one already behind us.
	if got := readyBudget(at(15 * time.Second)); got != 10*time.Second {
		t.Errorf("15s deadline: budget = %v, want the readiness budget of 10s", got)
	}
	if got := readyBudget(at(3 * time.Second)); got != 10*time.Second {
		t.Errorf("3s deadline: budget = %v, want the readiness budget of 10s, ended by the deadline", got)
	}
	gone, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := readyBudget(gone); got != 10*time.Second {
		t.Errorf("past deadline: budget = %v, want the readiness budget of 10s", got)
	}
}

// A runtime that answers after its own budget and inside what the deadline leaves
// after the reserve is waited for (muster #478): it ended a create with a 504 at 10 s
// on a loaded host while 20 s of the request's deadline were unused.
func TestWaitReady_WaitsPastItsOwnBudgetWhenTheDeadlineAllows(t *testing.T) {
	shortenReady(t, 100*time.Millisecond)
	reserveReady(t, 300*time.Millisecond)
	srv := slowToAnswer(t, 600*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := waitReady(ctx, srv.URL, "u", "p", make(chan struct{})); err != nil {
		t.Fatalf("err = %v, want a runtime that answered at 0.6s to be waited for: the deadline left 2.7s", err)
	}
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Errorf("took %v, want the wait to have run past the readiness budget of 100ms", took)
	}
}

// The extended wait still ends before the deadline does, leaving the reserve, and
// the answer names the budget it used rather than the readiness budget it started
// from (muster #478).
func TestWaitReady_ExtendedBudgetEndingIsNamedAndLeavesTheReserve(t *testing.T) {
	shortenReady(t, 100*time.Millisecond)
	reserveReady(t, time.Second)
	srv := notReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err := waitReady(ctx, srv.URL, "u", "p", make(chan struct{}))
	st := stepTimeoutOf(t, err)
	if st.Budget <= 100*time.Millisecond || st.Budget > time.Second {
		t.Errorf("Budget = %v, want the extended budget of about 1s (2s deadline less a 1s reserve), not the 100ms it started from", st.Budget)
	}
	if st.Step != stepReady || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the wait for readiness running past a deadline", err)
	}
	if ctx.Err() != nil {
		t.Errorf("the request's deadline had run out too: the reserve was not left")
	}
	if took := time.Since(start); took < 800*time.Millisecond {
		t.Errorf("took %v, want the extended budget of about 1s to have been waited out", took)
	}
}

// A caller that went away is a cancellation and says so; a child that died is
// still its own answer.
func TestWaitReady_CancelledCallerIsACancellation(t *testing.T) {
	shortenReady(t, 10*time.Second)
	srv := notReady(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := waitReady(ctx, srv.URL, "u", "p", make(chan struct{}))
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a cancellation", err)
	}
	if st := stepTimeoutOf(t, err); st.Budget != 0 {
		t.Errorf("Budget = %v, want none for a caller that left", st.Budget)
	}

	exited := make(chan struct{})
	close(exited)
	if err := waitReady(context.Background(), srv.URL, "u", "p", exited); !errors.Is(err, errExitedBeforeReady) {
		t.Errorf("err = %v, want the child's exit", err)
	}
}

// End to end through Create: a runtime that is alive and not answering ends the
// create with the readiness budget named, nothing is refused, and nothing is left
// behind to be cleaned up by hand.
func TestCreate_ReadinessBudgetEndingIsNotARefusal(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, root := newIsolatedDriver(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".fake-slow-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("its own budget", func(t *testing.T) {
		shortenReady(t, 300*time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := d.Create(ctx, testReq, "k-own", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
		if kind := kindOfErr(err); kind != "" {
			t.Fatalf("err = %v is a %s error; running out of time is not a refusal", err, kind)
		}
		if st := stepTimeoutOf(t, err); st.Budget != 300*time.Millisecond {
			t.Errorf("Budget = %v, want the readiness budget of 300ms", st.Budget)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("session root is not empty after a create that ran out of time: %v", entries)
		}
		d.mu.RLock()
		live := len(d.live)
		d.mu.RUnlock()
		if live != 0 {
			t.Errorf("%d server(s) still tracked after a create that ran out of time", live)
		}
	})

	t.Run("an extended budget", func(t *testing.T) {
		// A request with more time than the readiness budget lets the wait use it, up
		// to the reserve short of the deadline (muster #478), and the create ends
		// there with the budget it used named.
		shortenReady(t, 100*time.Millisecond)
		reserveReady(t, 300*time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		began := time.Now()
		_, err := d.Create(ctx, testReq, "k-extended", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
		if kind := kindOfErr(err); kind != "" {
			t.Fatalf("err = %v is a %s error; running out of time is not a refusal", err, kind)
		}
		st := stepTimeoutOf(t, err)
		if st.Budget <= 100*time.Millisecond || st.Budget > 1700*time.Millisecond {
			t.Errorf("Budget = %v, want the extended budget (2s deadline less a 300ms reserve, less the start), not the 100ms it began as", st.Budget)
		}
		if took := time.Since(began); took < 600*time.Millisecond {
			t.Errorf("create took %v, want it to have waited past the readiness budget of 100ms", took)
		}
		// A budget named at all is the driver's own having run out, not the deadline:
		// the create ended with the reserve still to spare.
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("session root is not empty after a create that ran out of time: %v", entries)
		}
		d.mu.RLock()
		live := len(d.live)
		d.mu.RUnlock()
		if live != 0 {
			t.Errorf("%d server(s) still tracked after a create that ran out of time", live)
		}
	})

	t.Run("the callers deadline", func(t *testing.T) {
		shortenReady(t, 10*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, err := d.Create(ctx, testReq, "k-deadline", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
		if kind := kindOfErr(err); kind != "" {
			t.Fatalf("err = %v is a %s error; running out of time is not a refusal", err, kind)
		}
		st := stepTimeoutOf(t, err)
		if st.Budget != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v (budget %v), want the caller's deadline and no budget of the driver's", err, st.Budget)
		}
	})
}

// A deadline during the catalog settle is the deadline, and the step is named.
func TestCreate_DeadlineEndingDuringTheSettleNamesTheStep(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"p": {"a"}}
	d := newTestDriver(t, f)
	d.catalogSettle, d.catalogPoll = 10*time.Second, 5*time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := d.Create(ctx, fleet.RequestFrom(fleet.Caller{}), "key-settle",
		fleet.SessionSpec{Cwd: "/work/x", Model: "p/b"})
	st := stepTimeoutOf(t, err)
	if st.Step != stepCatalog || st.Budget != 0 {
		t.Errorf("step %q budget %v, want the catalog settle cut short by the caller's deadline", st.Step, st.Budget)
	}
}

// A sign-in renewal cut off by the request's deadline is not "no usable sign-in":
// that told a caller whose create only needed more time to sign in again.
func TestCredential_ARenewalCutShortByTheDeadlineIsNotARefusal(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("")
	g.err = context.DeadlineExceeded
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialSubscription, sessionenv.Scope{})}))

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := d.Create(ctx, testReq, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Model: "openai/gpt-5"})
	if kind := kindOfErr(err); kind == fleet.ErrorInvalid {
		t.Fatalf("Create = %v: a renewal that ran out of time was refused as invalid", err)
	}
	if st := stepTimeoutOf(t, err); st.Step != stepCredential {
		t.Errorf("step = %q, want the sign-in renewal", st.Step)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}

	// While the request is live, a renewal that failed for its own reasons is still
	// the refusal it was.
	g2 := newFakeGrants("")
	g2.err = errors.New("the provider refused the renewal")
	d2, _ := newIsolatedDriver(t, WithGrant(g2), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialSubscription, sessionenv.Scope{})}))
	_, err = d2.Create(context.Background(), testReq, "k2", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Model: "openai/gpt-5"})
	if kindOfErr(err) != fleet.ErrorInvalid {
		t.Errorf("Create = %v, want the refusal a live request still gets", err)
	}
}

// A step of a start that failed because the request's context ended keeps its
// chain; any other failure is the invalid refusal it always was.
func TestInvalidStartKeepsAContextError(t *testing.T) {
	d := &Driver{machine: "test-machine"}
	for _, ctxErr := range []error{context.DeadlineExceeded, context.Canceled} {
		in := errors.Join(errors.New("writing the config"), ctxErr)
		if got := d.invalidStart(in); got != in {
			t.Errorf("invalidStart(%v) = %v, want the error returned as it is", in, got)
		}
	}
	got := d.invalidStart(errors.New("boom"))
	var fe *fleet.Error
	if !errors.As(got, &fe) || fe.Kind != fleet.ErrorInvalid || fe.Message != "create: boom" || fe.Machine != "test-machine" {
		t.Errorf("invalidStart(boom) = %#v, want today's invalid refusal", got)
	}
}

// A sandbox measurement interrupted by the request's deadline is not "this driver
// cannot enforce a sandbox" (501, final): the measurement said nothing.
func TestCreate_InterruptedSandboxMeasurementIsNotUnsupported(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	d.sandbox = &fleet.SandboxSupport{Mechanism: "test-mechanism"}
	d.sandboxProbe = func(ctx context.Context) string {
		<-ctx.Done()
		return sandboxReasonPrefix + "interrupted"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := d.Create(ctx, testReq, "k-measure", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{},
	})
	if kind := kindOfErr(err); kind == fleet.ErrorUnsupported {
		t.Fatalf("Create = %v: an interrupted measurement was refused as unsupported", err)
	}
	if st := stepTimeoutOf(t, err); st.Step != stepMeasure {
		t.Errorf("step = %q, want the sandbox measurement", st.Step)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}
	if len(d.live) != 0 {
		t.Fatal("a create that ran out of time started a server")
	}
}

func phaseNames(p *driver.Phases) []string {
	var names []string
	for _, ph := range p.All() {
		names = append(names, ph.Name)
	}
	return names
}

// A create notes its steps, in order, whether or not it succeeds, so a slow start
// can be attributed. The audit line prints them as phase.<name>.
func TestCreate_NotesItsPhasesInOrder(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t)

	t.Run("a create that succeeds", func(t *testing.T) {
		phases := &driver.Phases{}
		ctx, cancel := context.WithTimeout(driver.WithPhases(context.Background(), phases), 20*time.Second)
		defer cancel()
		if _, err := d.Create(ctx, testReq, "k-ok", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got := strings.Join(phaseNames(phases), " ")
		want := "oc.credential oc.dirs oc.config oc.start oc.ready oc.catalog"
		if got != want {
			t.Errorf("phases = %q, want %q", got, want)
		}
		if line := phases.Fields(); !strings.Contains(line, "phase.oc.ready=") {
			t.Errorf("audit fields %q do not carry phase.oc.ready", line)
		}
	})

	t.Run("a start that fails notes each attempt and never reaches the catalog", func(t *testing.T) {
		cwd := t.TempDir()
		if err := os.WriteFile(filepath.Join(cwd, ".fake-fail-ready"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		phases := &driver.Phases{}
		ctx, cancel := context.WithTimeout(driver.WithPhases(context.Background(), phases), 20*time.Second)
		defer cancel()
		if _, err := d.Create(ctx, testReq, "k-fail", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)}); err == nil {
			t.Fatal("Create succeeded against a server that cannot come up")
		}
		got := strings.Join(phaseNames(phases), " ")
		want := "oc.credential oc.dirs oc.config" + strings.Repeat(" oc.start oc.ready", startAttempts)
		if got != want {
			t.Errorf("phases = %q, want %q", got, want)
		}
	})
}
