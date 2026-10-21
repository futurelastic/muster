package service

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// A registered driver is not an available runtime when the program each of its
// sessions runs is not on the machine (muster #342). The driver here stands for
// the terminal runtime: it registers with nothing but its multiplexer, and can
// say whether its agent program is found.

// agentDriver is a stub whose agent program the test installs and removes.
type agentDriver struct {
	idDriver
	missing atomic.Bool
	asked   atomic.Int32
}

func (a *agentDriver) AgentProgramMissing(context.Context) (string, bool) {
	a.asked.Add(1)
	return "claude", a.missing.Load()
}

func newAgentService(t *testing.T) (*Service, *agentDriver, *idDriver) {
	t.Helper()
	svc := New("test-machine")
	ok := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	term := &agentDriver{idDriver: idDriver{Driver: stub.Driver{DeadlineMs: 200}}}
	if err := svc.RegisterLocalDriver("rt-ok", ok); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("rt-term", term); err != nil {
		t.Fatal(err)
	}
	return svc, term, ok
}

func TestAgentProgramMissing_RowIsUnavailableNotInstalled(t *testing.T) {
	svc, term, _ := newAgentService(t)
	term.missing.Store(true)

	rows := runtimeRows(t, svc)
	row, idx := findRow(rows, "rt-term")
	if idx < 0 {
		t.Fatalf("rows = %+v, want rt-term listed", rows)
	}
	if row.Status != fleet.RuntimeUnavailable || row.Cause != fleet.CauseNotInstalled {
		t.Errorf("rt-term = %+v, want unavailable / not-installed", row)
	}
	if !strings.Contains(row.Reason, `"claude"`) || strings.Contains(row.Reason, "/") {
		t.Errorf("reason = %q, want a sentence naming the program and no path", row.Reason)
	}
	if ok, i := findRow(rows, "rt-ok"); i < 0 || !ok.Available() || i > idx {
		t.Errorf("rt-ok = %+v at %d; want it available and listed before the unavailable row at %d", ok, i, idx)
	}
}

func TestAgentProgramMissing_BecomesAvailableWithoutARestart(t *testing.T) {
	svc, term, _ := newAgentService(t)
	term.missing.Store(true)
	if row, _ := findRow(runtimeRows(t, svc), "rt-term"); row.Available() {
		t.Fatalf("rt-term = %+v, want unavailable while the program is missing", row)
	}
	term.missing.Store(false)
	row, _ := findRow(runtimeRows(t, svc), "rt-term")
	if !row.Available() || row.Cause != "" || row.Reason != "" {
		t.Errorf("rt-term = %+v, want available with no cause once the program is installed", row)
	}
}

func TestAgentProgramMissing_CreateIsRefusedBeforeTheDriverIsAsked(t *testing.T) {
	svc, term, _ := newAgentService(t)
	term.missing.Store(true)
	srv := newServer(svc, t)

	resp, env := createOn(t, srv, "rt-term")
	if resp.StatusCode != http.StatusNotImplemented || env.Error.Kind != fleet.ErrorUnsupported ||
		env.Error.Retryable || env.Error.Reason != fleet.ReasonRuntimeUnavailable {
		t.Errorf("create: status %d, %+v; want 501 unsupported, not retryable, runtime-unavailable", resp.StatusCode, env.Error)
	}
	if term.created != 0 {
		t.Errorf("the driver was asked to create %d sessions it could not run", term.created)
	}

	term.missing.Store(false)
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", []byte(`{"cwd":"/tmp","runtime":"rt-term"}`))
	req.Header.Set("Idempotency-Key", "k-after-install")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode >= 400 || term.created != 1 {
		t.Errorf("after install: status %d, created %d; want the create served", r2.StatusCode, term.created)
	}
}

func TestAgentProgramMissing_HealthSaysSo(t *testing.T) {
	svc, term, _ := newAgentService(t)
	term.missing.Store(true)
	srv := newServer(svc, t)

	var body struct {
		Drivers []struct {
			Runtime fleet.RuntimeId        `json:"runtime"`
			Status  fleet.RuntimeStatus    `json:"status"`
			Cause   fleet.UnavailableCause `json:"cause"`
		} `json:"drivers"`
	}
	getJSON(t, srv, "/v1/health", &body)
	if len(body.Drivers) != 2 || body.Drivers[0].Runtime != "rt-ok" ||
		body.Drivers[1].Runtime != "rt-term" || body.Drivers[1].Status != fleet.RuntimeUnavailable ||
		body.Drivers[1].Cause != fleet.CauseNotInstalled {
		t.Errorf("drivers = %+v, want rt-ok then rt-term unavailable / not-installed", body.Drivers)
	}
}
