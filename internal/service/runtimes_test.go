package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// An enabled runtime with no working driver is listed, refused with a typed
// error, retried when the reason is time, and registered when it answers
// (muster #297). These tests drive that through StartOptionalRuntime with a
// Build the test controls, so nothing here needs a binary or a clock.

// lateBuild is a Build whose outcome the test sets.
type lateBuild struct {
	mu    sync.Mutex
	err   error
	drv   driver.Driver
	calls atomic.Int32
}

func (b *lateBuild) set(drv driver.Driver, err error) {
	b.mu.Lock()
	b.drv, b.err = drv, err
	b.mu.Unlock()
}

func (b *lateBuild) build(ctx context.Context, attempt int) (driver.Driver, error) {
	b.calls.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	return b.drv, nil
}

func timedOut() error {
	return &driver.UnavailableError{Cause: fleet.CauseProbeTimeout, Err: errors.New("opencode: --version failed: signal: killed")}
}

func optional(b *lateBuild) OptionalRuntime {
	return OptionalRuntime{
		Runtime:    "rt-late",
		DeadlineMs: 1234,
		Build:      b.build,
		RetryMin:   5 * time.Millisecond,
		RetryMax:   20 * time.Millisecond,
		Logf:       func(string, ...any) {},
	}
}

func runtimeRows(t *testing.T, svc *Service) []fleet.RuntimeInfo {
	t.Helper()
	col, err := svc.ListRuntimes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return col.Items()
}

func findRow(rows []fleet.RuntimeInfo, rt fleet.RuntimeId) (fleet.RuntimeInfo, int) {
	for i, r := range rows {
		if r.Runtime == rt {
			return r, i
		}
	}
	return fleet.RuntimeInfo{}, -1
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// createOn creates a session, naming runtime in the body when it is not
// empty. The create body, not a query parameter, is where a create names it.
func createOn(t *testing.T, srv *httptest.Server, runtime string) (*http.Response, fleet.ErrorEnvelope) {
	t.Helper()
	body := `{"cwd":"/tmp"}`
	if runtime != "" {
		body = `{"cwd":"/tmp","runtime":"` + runtime + `"}`
	}
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", []byte(body))
	req.Header.Set("Idempotency-Key", "k-"+t.Name()+"-"+runtime)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var env fleet.ErrorEnvelope
	if resp.StatusCode >= 400 {
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
	}
	return resp, env
}

func newServer(svc *Service, t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	return srv
}

// (g) Shape and order: registered rows first, then the unavailable ones, and
// every row says its status.
func TestListRuntimes_UnavailableRuntimeIsListedWithCauseAndReason(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	b := &lateBuild{}
	b.set(nil, &driver.UnavailableError{Cause: fleet.CauseNotInstalled, Err: errors.New(`opencode: "/some/path/opencode" not found`)})
	stop := svc.StartOptionalRuntime(context.Background(), optional(b))
	defer stop()

	rows := runtimeRows(t, svc)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].Runtime != "rt-a" || rows[0].Status != fleet.RuntimeAvailable || !rows[0].Available() {
		t.Errorf("first row = %+v, want the registered runtime, available", rows[0])
	}
	got := rows[1]
	if got.Runtime != "rt-late" || got.Status != fleet.RuntimeUnavailable || got.Available() {
		t.Fatalf("second row = %+v, want rt-late unavailable", got)
	}
	if got.Cause != fleet.CauseNotInstalled {
		t.Errorf("cause = %q, want %q", got.Cause, fleet.CauseNotInstalled)
	}
	if got.Reason == "" || strings.Contains(got.Reason, "/some/path") {
		t.Errorf("reason = %q, want a sentence that never carries the binary's path", got.Reason)
	}
	if got.Capabilities.Source != fleet.CapabilitiesAssumed || got.Capabilities.DeadlineMs != 1234 {
		t.Errorf("capabilities = %+v, want the assumed floor with the declared deadline", got.Capabilities)
	}
	if b.calls.Load() != 1 {
		t.Errorf("Build ran %d times, want 1 — not-installed is not retried", b.calls.Load())
	}
}

// (h) A probe that timed out is retried and the driver swapped in when it
// answers; the row moves from unavailable to available, once.
func TestStartOptionalRuntime_ProbeTimeoutIsRetriedAndRegisteredWhenItAnswers(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	b := &lateBuild{}
	b.set(nil, timedOut())
	stop := svc.StartOptionalRuntime(context.Background(), optional(b))
	defer stop()

	row, _ := findRow(runtimeRows(t, svc), "rt-late")
	if row.Status != fleet.RuntimeUnavailable || row.Cause != fleet.CauseProbeTimeout {
		t.Fatalf("row = %+v, want unavailable / probe-timeout", row)
	}
	// Let it fail at least once more, so the retry is seen to be running.
	eventually(t, "a retry", func() bool { return b.calls.Load() >= 3 })

	late := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	b.set(late, nil)
	eventually(t, "the late runtime to register", func() bool {
		r, _ := findRow(runtimeRows(t, svc), "rt-late")
		return r.Status == fleet.RuntimeAvailable
	})
	rows := runtimeRows(t, svc)
	n := 0
	for _, r := range rows {
		if r.Runtime == "rt-late" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("rt-late listed %d times, want exactly 1: %+v", n, rows)
	}
	r, _ := findRow(rows, "rt-late")
	if r.Cause != "" || r.Reason != "" || !r.Available() {
		t.Errorf("registered row = %+v, want no cause or reason", r)
	}

	// The retry has ended: nothing calls Build any more.
	settled := b.calls.Load()
	time.Sleep(60 * time.Millisecond)
	if got := b.calls.Load(); got != settled {
		t.Errorf("Build ran %d more times after the runtime registered", got-settled)
	}
}

// stop() ends the retry and waits for it; after it returns nothing registers.
func TestStartOptionalRuntime_StopHaltsTheRetry(t *testing.T) {
	svc := New("test-machine")
	b := &lateBuild{}
	b.set(nil, timedOut())
	stop := svc.StartOptionalRuntime(context.Background(), optional(b))
	eventually(t, "a retry", func() bool { return b.calls.Load() >= 2 })
	stop()
	stop() // idempotent

	settled := b.calls.Load()
	b.set(&idDriver{Driver: stub.Driver{DeadlineMs: 200}}, nil)
	time.Sleep(60 * time.Millisecond)
	if got := b.calls.Load(); got != settled {
		t.Errorf("Build ran %d more times after stop()", got-settled)
	}
	if r, _ := findRow(runtimeRows(t, svc), "rt-late"); r.Status != fleet.RuntimeUnavailable {
		t.Errorf("row after stop = %+v, want it still unavailable (nothing registered it)", r)
	}
}

// A refusal no waiting fixes is recorded once and not retried; an error that
// is not typed is a start failure.
func TestStartOptionalRuntime_ConfigurationRefusalIsNotRetried(t *testing.T) {
	svc := New("test-machine")
	b := &lateBuild{}
	b.set(nil, errors.New("opencode: the state directory must be an absolute path"))
	stop := svc.StartOptionalRuntime(context.Background(), optional(b))
	defer stop()
	time.Sleep(60 * time.Millisecond)
	if got := b.calls.Load(); got != 1 {
		t.Errorf("Build ran %d times, want 1", got)
	}
	r, _ := findRow(runtimeRows(t, svc), "rt-late")
	if r.Cause != fleet.CauseStartFailed {
		t.Errorf("cause = %q, want %q", r.Cause, fleet.CauseStartFailed)
	}
}

// A healthy first attempt registers synchronously, before the call returns.
func TestStartOptionalRuntime_FirstAttemptIsSynchronous(t *testing.T) {
	svc := New("test-machine")
	b := &lateBuild{}
	b.set(&idDriver{Driver: stub.Driver{DeadlineMs: 200}}, nil)
	stop := svc.StartOptionalRuntime(context.Background(), optional(b))
	defer stop()
	if r, _ := findRow(runtimeRows(t, svc), "rt-late"); r.Status != fleet.RuntimeAvailable {
		t.Fatalf("row = %+v, want available immediately", r)
	}
	if err := svc.SetDefaultRuntime("rt-late"); err != nil {
		t.Errorf("SetDefaultRuntime on a registered runtime: %v", err)
	}
}

// (i) A create naming an enabled-but-unusable runtime is told so: retryable
// while it is being retried, not retryable when it needs somebody's hands,
// and still not_found for an id nobody enabled.
func TestCreate_NamingAnUnavailableRuntimeAnswersTyped(t *testing.T) {
	svc := New("test-machine")
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	if err := svc.RegisterLocalDriver("rt-a", a); err != nil {
		t.Fatal(err)
	}
	slow := &lateBuild{}
	slow.set(nil, timedOut())
	so := optional(slow)
	so.Runtime = "rt-slow"
	stopSlow := svc.StartOptionalRuntime(context.Background(), so)
	defer stopSlow()
	gone := &lateBuild{}
	gone.set(nil, &driver.UnavailableError{Cause: fleet.CauseNotInstalled, Err: errors.New("missing")})
	go_ := optional(gone)
	go_.Runtime = "rt-gone"
	stopGone := svc.StartOptionalRuntime(context.Background(), go_)
	defer stopGone()
	srv := newServer(svc, t)

	resp, env := createOn(t, srv, "rt-slow")
	if resp.StatusCode != http.StatusGatewayTimeout || env.Error.Kind != fleet.ErrorUnreachable ||
		!env.Error.Retryable || env.Error.Reason != fleet.ReasonRuntimeUnavailable {
		t.Errorf("retrying runtime: status %d, %+v; want 504 unreachable retryable runtime-unavailable", resp.StatusCode, env.Error)
	}
	resp, env = createOn(t, srv, "rt-gone")
	if resp.StatusCode != http.StatusNotImplemented || env.Error.Kind != fleet.ErrorUnsupported ||
		env.Error.Retryable || env.Error.Reason != fleet.ReasonRuntimeUnavailable {
		t.Errorf("not-installed runtime: status %d, %+v; want 501 unsupported, not retryable, runtime-unavailable", resp.StatusCode, env.Error)
	}
	resp, env = createOn(t, srv, "never-enabled")
	if resp.StatusCode != http.StatusNotFound || env.Error.Reason != "" {
		t.Errorf("unknown runtime: status %d, %+v; want a plain 404", resp.StatusCode, env.Error)
	}
	if a.created != 0 {
		t.Errorf("rt-a served %d creates it was never asked for", a.created)
	}
}

// (j) A default naming a runtime that is still being tried is accepted; one
// that is not installed, or a typo, is refused as before.
func TestSetDefaultRuntime_AcceptsARetryingRuntimeOnly(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	slow := &lateBuild{}
	slow.set(nil, timedOut())
	so := optional(slow)
	so.Runtime = "rt-slow"
	defer svc.StartOptionalRuntime(context.Background(), so)()
	gone := &lateBuild{}
	gone.set(nil, &driver.UnavailableError{Cause: fleet.CauseNotInstalled, Err: errors.New("missing")})
	g := optional(gone)
	g.Runtime = "rt-gone"
	defer svc.StartOptionalRuntime(context.Background(), g)()

	if err := svc.SetDefaultRuntime("rt-slow"); err != nil {
		t.Errorf("SetDefaultRuntime(retrying) = %v, want nil", err)
	}
	if err := svc.SetDefaultRuntime("rt-gone"); err == nil {
		t.Error("SetDefaultRuntime(not-installed) accepted; guardrail 1 wants it refused at startup")
	}
	if err := svc.SetDefaultRuntime("typo"); err == nil {
		t.Error("SetDefaultRuntime(typo) accepted")
	}
}

// (k) Routing while one runtime is late. The point of every case: a runtime
// that is merely late is never silently routed around.
func TestResolve_WhileARuntimeIsLate(t *testing.T) {
	build := func(t *testing.T, held map[string]bool, defaultRuntime fleet.RuntimeId) (*idDriver, *httptest.Server) {
		t.Helper()
		svc := New("test-machine")
		a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}, held: held}
		if err := svc.RegisterLocalDriver("rt-a", a); err != nil {
			t.Fatal(err)
		}
		b := &lateBuild{}
		b.set(nil, timedOut())
		stop := svc.StartOptionalRuntime(context.Background(), optional(b))
		t.Cleanup(stop)
		if defaultRuntime != "" {
			if err := svc.SetDefaultRuntime(defaultRuntime); err != nil {
				t.Fatal(err)
			}
		}
		return a, newServer(svc, t)
	}

	t.Run("no default: a bare create is ambiguous, not quietly sent to the one that is up", func(t *testing.T) {
		a, srv := build(t, nil, "")
		resp, _ := createOn(t, srv, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		if a.created != 0 {
			t.Errorf("rt-a served %d creates", a.created)
		}
	})
	t.Run("default is the late runtime: the create is told it is unavailable", func(t *testing.T) {
		a, srv := build(t, nil, "rt-late")
		resp, env := createOn(t, srv, "")
		if resp.StatusCode != http.StatusGatewayTimeout || env.Error.Reason != fleet.ReasonRuntimeUnavailable {
			t.Errorf("status %d, %+v; want 504 runtime-unavailable", resp.StatusCode, env.Error)
		}
		if a.created != 0 {
			t.Errorf("rt-a served %d creates — the default is rt-late and must not fall back silently", a.created)
		}
	})
	t.Run("default is the registered runtime: it serves", func(t *testing.T) {
		a, srv := build(t, nil, "rt-a")
		resp, _ := createOn(t, srv, "")
		if resp.StatusCode != http.StatusCreated || resp.Header.Get("Fleet-Runtime") != "rt-a" ||
			resp.Header.Get("Fleet-Runtime-Resolution") != "default" || a.created != 1 {
			t.Errorf("status %d, Fleet-Runtime %q, resolution %q, created %d; want the default to serve",
				resp.StatusCode, resp.Header.Get("Fleet-Runtime"), resp.Header.Get("Fleet-Runtime-Resolution"), a.created)
		}
	})
	t.Run("a bare id the registered runtime holds still resolves there", func(t *testing.T) {
		_, srv := build(t, map[string]bool{"abc": true}, "rt-a")
		req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/abc", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Fleet-Runtime") != "rt-a" {
			t.Errorf("status %d, runtime %q; want 200 from rt-a", resp.StatusCode, resp.Header.Get("Fleet-Runtime"))
		}
	})
	t.Run("a bare id nobody holds is refused: the late runtime might be the one", func(t *testing.T) {
		_, srv := build(t, nil, "rt-a")
		req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/nowhere", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env fleet.ErrorEnvelope
		_ = json.NewDecoder(resp.Body).Decode(&env)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(env.Error.Message, "rt-late") {
			t.Errorf("status %d, %q; want 400 naming rt-late", resp.StatusCode, env.Error.Message)
		}
	})
	t.Run("a permanently unavailable runtime does not change routing", func(t *testing.T) {
		svc := New("test-machine")
		a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
		if err := svc.RegisterLocalDriver("rt-a", a); err != nil {
			t.Fatal(err)
		}
		b := &lateBuild{}
		b.set(nil, &driver.UnavailableError{Cause: fleet.CauseNotInstalled, Err: errors.New("missing")})
		defer svc.StartOptionalRuntime(context.Background(), optional(b))()
		srv := newServer(svc, t)
		resp, _ := createOn(t, srv, "")
		if resp.StatusCode != http.StatusCreated || a.created != 1 {
			t.Errorf("status %d, created %d; want the sole registered runtime to serve, as before", resp.StatusCode, a.created)
		}
	})
}

// (l) Listing and resolving while the late runtime registers: every snapshot
// has the runtime exactly once, and nothing races.
func TestListRuntimes_ConcurrentWithLateRegistration(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	b := &lateBuild{}
	b.set(nil, timedOut())
	stop := svc.StartOptionalRuntime(context.Background(), optional(b))
	defer stop()

	var wg sync.WaitGroup
	done := make(chan struct{})
	var bad atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				n := 0
				for _, r := range runtimeRows(t, svc) {
					if r.Runtime == "rt-late" {
						n++
					}
				}
				if n != 1 {
					bad.Add(1)
				}
				_, _, _, _ = svc.resolveSessionDriver(context.Background(), fleet.Request{}, "test-machine", "x", "", time.Second)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	b.set(&idDriver{Driver: stub.Driver{DeadlineMs: 200}}, nil)
	eventually(t, "registration", func() bool {
		r, _ := findRow(runtimeRows(t, svc), "rt-late")
		return r.Status == fleet.RuntimeAvailable
	})
	time.Sleep(20 * time.Millisecond)
	close(done)
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d snapshots listed rt-late other than exactly once", bad.Load())
	}
}

// getJSON GETs path with the test token and decodes the body into v.
func getJSON(t *testing.T, srv *httptest.Server, path string, v any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+path, nil))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}

// listingDriver is a stub whose List answers an empty, ok listing — the stub's
// own List is "unsupported", which reads degraded and would hide what these
// tests are looking for.
type listingDriver struct{ stub.Driver }

func (d *listingDriver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	return fleet.NewCollection[fleet.Session](nil, []fleet.SourceStatus{{Machine: "test-machine", Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

// sessionsEnvelope is the part of GET /v1/sessions these tests read.
type sessionsEnvelope struct {
	Sources  []fleet.SourceStatus `json:"sources"`
	Complete bool                 `json:"complete"`
}

func degradedSources(env sessionsEnvelope) []fleet.SourceStatus {
	var out []fleet.SourceStatus
	for _, s := range env.Sources {
		if s.Status == fleet.SourceDegraded {
			out = append(out, s)
		}
	}
	return out
}

// (k) muster #300: an enabled runtime that is still being retried keeps its
// sessions out of the list, and the list says so — a degraded source naming the
// runtime, and complete=false — instead of a local source that reads ok. Only a
// runtime being retried is reported: one that is not installed has nothing
// waiting to arrive. When the runtime registers, the source goes with it.
func TestListSessions_APendingRuntimeMakesTheLocalSourceDegraded(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", &listingDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	slow := &lateBuild{}
	slow.set(nil, timedOut())
	so := optional(slow)
	so.Runtime = "rt-slow"
	defer svc.StartOptionalRuntime(context.Background(), so)()
	gone := &lateBuild{}
	gone.set(nil, &driver.UnavailableError{Cause: fleet.CauseNotInstalled, Err: errors.New("missing")})
	g := optional(gone)
	g.Runtime = "rt-gone"
	defer svc.StartOptionalRuntime(context.Background(), g)()
	srv := newServer(svc, t)

	var env sessionsEnvelope
	getJSON(t, srv, "/v1/sessions?scope=local", &env)
	if env.Complete {
		t.Error("complete = true with a runtime still being retried; the list is missing whatever that runtime holds")
	}
	deg := degradedSources(env)
	if len(deg) != 1 {
		t.Fatalf("degraded sources = %+v, want exactly the retrying runtime", deg)
	}
	if deg[0].Machine != "test-machine" {
		t.Errorf("source machine = %q, want the machine the runtime is on", deg[0].Machine)
	}
	for _, want := range []string{"rt-slow", string(fleet.CauseProbeTimeout)} {
		if !strings.Contains(deg[0].Error, want) {
			t.Errorf("source error = %q, want it to name %q", deg[0].Error, want)
		}
	}
	if strings.Contains(deg[0].Error, "rt-gone") {
		t.Errorf("source error = %q, a runtime that needs somebody's hands is not pending", deg[0].Error)
	}
	if len(env.Sources) != 2 {
		t.Errorf("sources = %+v, want the registered driver's source and the pending runtime's", env.Sources)
	}

	// It answers: the runtime registers, and the list stops saying it is late.
	slow.set(&listingDriver{Driver: stub.Driver{DeadlineMs: 200}}, nil)
	eventually(t, "the runtime to register", func() bool {
		r, _ := findRow(runtimeRows(t, svc), "rt-slow")
		return r.Status == fleet.RuntimeAvailable
	})
	getJSON(t, srv, "/v1/sessions?scope=local", &env)
	if !env.Complete || len(degradedSources(env)) != 0 {
		t.Errorf("after registering: complete=%v degraded=%+v, want a complete, all-ok list", env.Complete, degradedSources(env))
	}
}

// (l) With no registered driver the service used to synthesize one ok source
// for itself — "nothing here". That must not be said while a runtime is on its
// way: the only source is the degraded one.
func TestListSessions_OnlyAPendingRuntimeIsNotAnEmptyOK(t *testing.T) {
	svc := New("test-machine")
	slow := &lateBuild{}
	slow.set(nil, timedOut())
	defer svc.StartOptionalRuntime(context.Background(), optional(slow))()
	srv := newServer(svc, t)

	var env sessionsEnvelope
	getJSON(t, srv, "/v1/sessions?scope=local", &env)
	if len(env.Sources) != 1 || env.Sources[0].Status != fleet.SourceDegraded {
		t.Fatalf("sources = %+v, want the one degraded source for rt-late", env.Sources)
	}
	if env.Complete {
		t.Error("complete = true, want false")
	}
}

// (m) /v1/health `drivers` is a roster of enabled runtimes: an enabled runtime
// with no driver is a row with its status, cause and reason, after the
// registered ones — the order and the vocabulary GET /v1/runtimes uses.
func TestHealth_DriversListsAnEnabledRuntimeWithNoDriver(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	slow := &lateBuild{}
	slow.set(nil, timedOut())
	so := optional(slow)
	so.Runtime = "rt-slow"
	defer svc.StartOptionalRuntime(context.Background(), so)()
	gone := &lateBuild{}
	gone.set(nil, &driver.UnavailableError{Cause: fleet.CauseNotInstalled, Err: errors.New(`"/some/path/x" not found`)})
	g := optional(gone)
	g.Runtime = "rt-gone"
	defer svc.StartOptionalRuntime(context.Background(), g)()
	srv := newServer(svc, t)

	var body struct {
		Drivers []struct {
			Runtime      fleet.RuntimeId          `json:"runtime"`
			Status       fleet.RuntimeStatus      `json:"status"`
			Cause        fleet.UnavailableCause   `json:"cause"`
			Reason       string                   `json:"reason"`
			Capabilities fleet.DriverCapabilities `json:"capabilities"`
		} `json:"drivers"`
	}
	getJSON(t, srv, "/v1/health", &body)
	if len(body.Drivers) != 3 {
		t.Fatalf("drivers = %+v, want the registered runtime and both enabled ones", body.Drivers)
	}
	d := body.Drivers
	if d[0].Runtime != "rt-a" || d[0].Status != fleet.RuntimeAvailable || d[0].Cause != "" || d[0].Reason != "" {
		t.Errorf("first = %+v, want the registered runtime, available, no cause", d[0])
	}
	if d[1].Runtime != "rt-gone" || d[1].Status != fleet.RuntimeUnavailable || d[1].Cause != fleet.CauseNotInstalled {
		t.Errorf("second = %+v, want rt-gone unavailable / not-installed", d[1])
	}
	if d[2].Runtime != "rt-slow" || d[2].Status != fleet.RuntimeUnavailable || d[2].Cause != fleet.CauseProbeTimeout {
		t.Errorf("third = %+v, want rt-slow unavailable / probe-timeout", d[2])
	}
	for _, row := range d[1:] {
		if row.Reason == "" || strings.Contains(row.Reason, "/some/path") {
			t.Errorf("%s reason = %q, want a sentence that never carries the binary's path", row.Runtime, row.Reason)
		}
		if row.Capabilities.Source != fleet.CapabilitiesAssumed {
			t.Errorf("%s capabilities = %+v, want the assumed floor", row.Runtime, row.Capabilities)
		}
	}
}
