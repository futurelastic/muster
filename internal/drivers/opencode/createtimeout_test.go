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

// shortenReady makes this driver's own readiness budget d for the test. It is a
// package variable, which is safe only because no test in this package runs in
// parallel (availability_test.go says the same of the sandbox probe's).
func shortenReady(t *testing.T, d time.Duration) {
	t.Helper()
	old := readyTimeout
	readyTimeout = d
	t.Cleanup(func() { readyTimeout = old })
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
