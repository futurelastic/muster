package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/service"
)

// muster #297: an enabled runtime whose version probe did not answer is not
// dropped, and "did not answer" is told apart from "is not there". These tests
// use real processes — the test binary standing in for opencode — so the
// timeout they assert is a real kill by the probe's own deadline.
//
// They set an environment variable the stand-in reads, so none of them calls
// t.Parallel.

// gatedBinary makes the stand-in's `--version` block until the returned
// release function is called.
func gatedBinary(t *testing.T) (bin string, release func()) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "open")
	t.Setenv("FAKE_OPENCODE_VERSION_GATE", gate)
	return exe, func() {
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// (a) A name that resolves to nothing is "not installed".
func TestProbe_MissingBinaryIsNotInstalled(t *testing.T) {
	a := Probe(context.Background(), "opencode-definitely-does-not-exist-on-this-machine")
	if a.Installed || a.Cause != fleet.CauseNotInstalled || a.Path != "" {
		t.Errorf("Availability = %+v, want not-installed with no path", a)
	}
	_, cause, err := Locate("opencode-definitely-does-not-exist-on-this-machine")
	if cause != fleet.CauseNotInstalled || err == nil {
		t.Errorf("Locate = %q, %v; want not-installed and an error", cause, err)
	}
}

// (b) A file that is there and cannot be run is "not executable", not "not
// installed" — the fix is a mode, not an install.
func TestProbe_NonExecutableFileIsNotExecutable(t *testing.T) {
	skipOnWindows(t, "a file's executability is a POSIX mode bit")
	p := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Probe(context.Background(), p)
	if a.Installed || a.Cause != fleet.CauseNotExecutable {
		t.Errorf("Availability = %+v, want not-executable", a)
	}
	if _, cause, _ := Locate(p); cause != fleet.CauseNotExecutable {
		t.Errorf("Locate cause = %q, want not-executable", cause)
	}
}

// (c) A binary that does not answer within the deadline is a probe timeout:
// its path is known, and the cause says it was slow, not broken.
func TestProbe_HangPastTheDeadlineIsProbeTimeout(t *testing.T) {
	bin, release := gatedBinary(t)
	defer release()
	start := time.Now()
	a := probe(context.Background(), bin, 100*time.Millisecond)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("probe took %s — the deadline was not applied", took)
	}
	if a.Installed || a.Cause != fleet.CauseProbeTimeout || a.Path == "" {
		t.Errorf("Availability = %+v, want probe-timeout with the path set", a)
	}
}

// A caller that gives up is not a binary that was slow.
func TestProbe_CallerCancellationIsNotAProbeTimeout(t *testing.T) {
	bin, release := gatedBinary(t)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	a := probe(ctx, bin, 10*time.Second)
	if a.Installed || a.Cause == fleet.CauseProbeTimeout {
		t.Errorf("Availability = %+v, want a failure that is not probe-timeout", a)
	}
}

// (d) New refuses with a typed error, and only a timeout reads as retryable.
func TestNew_RefusalIsTypedAndOnlyATimeoutIsRetryable(t *testing.T) {
	bin, release := gatedBinary(t)
	defer release()

	_, err := New(context.Background(), "test-machine", WithBinary(bin), WithProbeTimeout(100*time.Millisecond))
	var ue *driver.UnavailableError
	if !errors.As(err, &ue) || ue.Cause != fleet.CauseProbeTimeout || !ue.Retryable() {
		t.Fatalf("New = %v, want a retryable probe-timeout", err)
	}

	_, err = New(context.Background(), "test-machine", WithBinary("opencode-definitely-does-not-exist"))
	if !errors.As(err, &ue) || ue.Cause != fleet.CauseNotInstalled || ue.Retryable() {
		t.Fatalf("New = %v, want a non-retryable not-installed", err)
	}
}

// (e) Configuration is checked before the probe: a runtime refused for how it
// is configured is a plain error — "start-failed", never retried — even when
// its binary is also missing, so a retry never repeats work that cannot change.
func TestNew_ConfigurationIsRefusedBeforeTheProbe(t *testing.T) {
	_, err := New(context.Background(), "test-machine",
		WithBinary("opencode-definitely-does-not-exist"),
		WithStateDir("relative/state"))
	if err == nil {
		t.Fatal("New succeeded")
	}
	var ue *driver.UnavailableError
	if errors.As(err, &ue) {
		t.Errorf("New = %v, want a plain configuration error, not an UnavailableError", err)
	}
}

// (f) The Done-when of #297: a probe that hangs past its deadline leaves the
// runtime LISTED as unavailable with the reason — not omitted — and the
// runtime is registered once the probe later answers, with no restart.
func TestOptionalRuntime_HungProbeIsListedThenRegisteredWhenItAnswers(t *testing.T) {
	bin, release := gatedBinary(t)
	defer release()

	svc := service.New("test-machine")
	srv := httptest.NewServer(service.NewMux(svc, service.Config{Token: "t297", AllowLocalMutations: true}))
	defer srv.Close()

	var built *Driver
	stop := svc.StartOptionalRuntime(context.Background(), service.OptionalRuntime{
		Runtime:    DefaultRuntime,
		DeadlineMs: DefaultDeadlineMs,
		RetryMin:   10 * time.Millisecond,
		RetryMax:   50 * time.Millisecond,
		Logf:       func(string, ...any) {},
		Build: func(ctx context.Context, attempt int) (driver.Driver, error) {
			// The first try is short (it gates startup); retries get the
			// longer bound main.go gives them — the reason they exist is a
			// binary too slow for the short one, and a race-built stand-in
			// needs more than 150ms just to start.
			within := 150 * time.Millisecond
			if attempt > 0 {
				within = 10 * time.Second
			}
			d, err := New(ctx, "test-machine", WithBinary(bin), WithProbeTimeout(within), WithSessionRoot(t.TempDir()))
			if err != nil {
				return nil, err
			}
			built = d
			return d, nil
		},
	})
	defer func() {
		stop()
		if built != nil {
			_ = built.Shutdown()
		}
	}()

	list := func() fleet.RuntimeInfo {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/runtimes", nil)
		req.Header.Set("Authorization", "Bearer t297")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var col struct {
			Items []fleet.RuntimeInfo `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&col); err != nil {
			t.Fatal(err)
		}
		for _, r := range col.Items {
			if r.Runtime == DefaultRuntime {
				return r
			}
		}
		t.Fatalf("opencode is not listed on GET /v1/runtimes: %+v", col.Items)
		return fleet.RuntimeInfo{}
	}

	// While the probe hangs: listed, unavailable, with the cause and a reason.
	row := list()
	if row.Status != fleet.RuntimeUnavailable || row.Cause != fleet.CauseProbeTimeout || row.Reason == "" {
		t.Fatalf("row = %+v, want unavailable / probe-timeout / a reason", row)
	}
	if row.Capabilities.Source != fleet.CapabilitiesAssumed || row.Capabilities.DeadlineMs != DefaultDeadlineMs {
		t.Errorf("capabilities = %+v, want the assumed floor with the driver's deadline", row.Capabilities)
	}

	// A create naming it is told what it is, not that it does not exist.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions",
		strings.NewReader(`{"cwd":"/tmp","runtime":"opencode"}`))
	req.Header.Set("Authorization", "Bearer t297")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k-297")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var env fleet.ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout || !env.Error.Retryable || env.Error.Reason != fleet.ReasonRuntimeUnavailable {
		t.Errorf("create: status %d, %+v; want 504, retryable, runtime-unavailable", resp.StatusCode, env.Error)
	}

	// The probe answers. No restart: the same service registers the runtime.
	release()
	deadline := time.Now().Add(15 * time.Second)
	for {
		row = list()
		if row.Status == fleet.RuntimeAvailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("opencode was not registered after the probe answered; row = %+v", row)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if row.Cause != "" || row.Reason != "" || row.Capabilities.Source != fleet.CapabilitiesObserved {
		t.Errorf("registered row = %+v, want no cause or reason and observed capabilities", row)
	}
}
