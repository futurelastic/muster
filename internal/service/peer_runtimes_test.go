package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/remote"
)

// muster #459: a peer that runs several runtimes is listed here as one
// GET /v1/runtimes row per runtime, in runtime-id order, each with its own
// capabilities, posture and models — one driver per peer all the same.

var peerCaller = fleet.Request{Caller: fleet.Caller{Principal: "test", Credential: "peer-token"}}

func peerRuntimeRow(machine fleet.MachineId, rt fleet.RuntimeId, caps fleet.DriverCapabilities, models *fleet.RuntimeModels) fleet.RuntimeInfo {
	caps.Source = fleet.CapabilitiesObserved
	return fleet.RuntimeInfo{Machine: machine, Runtime: rt, Status: fleet.RuntimeAvailable, Capabilities: caps, Models: models}
}

// peerRuntimesServer answers a peer's /v1/runtimes with the rows given and any
// other path (the health probe) with an empty object.
func peerRuntimesServer(t *testing.T, rows ...fleet.RuntimeInfo) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/runtimes" {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":    rows,
			"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
			"complete": true,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// registerPeer registers a real remote driver for peerbox and returns it.
func registerPeer(t *testing.T, svc *Service, url string) *remote.Driver {
	t.Helper()
	d := remote.New("peerbox", url, remote.WithIdentity("peer-token"))
	if err := svc.RegisterPeerDriver("peerbox", d); err != nil {
		t.Fatal(err)
	}
	return d
}

func peerRowsOf(t *testing.T, svc *Service) ([]fleet.RuntimeInfo, fleet.SourceStatus) {
	t.Helper()
	col, err := svc.ListRuntimes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var rows []fleet.RuntimeInfo
	for _, it := range col.Items() {
		if it.Machine == "peerbox" {
			rows = append(rows, it)
		}
	}
	var src fleet.SourceStatus
	var found bool
	for _, s := range col.Sources() {
		if s.Machine == "peerbox" {
			src, found = s, true
		}
	}
	if !found {
		t.Fatal("the peer contributed no SourceStatus")
	}
	return rows, src
}

func TestListRuntimes_APeerWithTwoRuntimesYieldsTwoRows(t *testing.T) {
	readAt := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	srv := peerRuntimesServer(t,
		peerRuntimeRow("peerbox", "opencode", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true},
			&fleet.RuntimeModels{Source: fleet.ModelsFromRuntime, ReadAt: &readAt, IDs: []string{"deepseek/deepseek-flash"}}),
		peerRuntimeRow("peerbox", "claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000, DeliversRawKeys: true},
			&fleet.RuntimeModels{Source: fleet.ModelsDeclared, IDs: []string{"opus", "sonnet"}}),
	)
	svc := New("testbox")
	d := registerPeer(t, svc, srv.URL)
	if err := d.RefreshCapabilities(context.Background(), peerCaller); err != nil {
		t.Fatal(err)
	}

	rows, src := peerRowsOf(t, svc)
	if len(rows) != 2 {
		t.Fatalf("rows for the peer = %+v, want one per runtime", rows)
	}
	cc, oc := rows[0], rows[1]
	if cc.Runtime != "claude-code-tmux" || oc.Runtime != "opencode" {
		t.Fatalf("runtimes = %q, %q; want runtime-id order", cc.Runtime, oc.Runtime)
	}
	if !cc.Capabilities.DeliversRawKeys || cc.Capabilities.SupportsResume || !oc.Capabilities.SupportsResume || oc.Capabilities.DeliversRawKeys {
		t.Errorf("capabilities were not each row's own: %+v / %+v", cc.Capabilities, oc.Capabilities)
	}
	if cc.Models == nil || len(cc.Models.IDs) != 2 || cc.Models.Source != fleet.ModelsDeclared {
		t.Errorf("claude-code-tmux models = %+v, want its own", cc.Models)
	}
	if oc.Models == nil || len(oc.Models.IDs) != 1 || oc.Models.Source != fleet.ModelsFromRuntime {
		t.Errorf("opencode models = %+v, want its own", oc.Models)
	}
	for _, r := range rows {
		if r.Status != fleet.RuntimeAvailable || r.Capabilities.Source != fleet.CapabilitiesObserved || r.Posture == nil {
			t.Errorf("%s: %+v, want an available, observed row with a posture", r.Runtime, r)
		}
	}
	if src.Status != fleet.SourceOK {
		t.Errorf("peer source = %+v, want ok", src)
	}
}

func TestListRuntimes_APeerWithOneRuntimeYieldsOneRowAsBefore(t *testing.T) {
	srv := peerRuntimesServer(t,
		peerRuntimeRow("peerbox", "claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true}, nil))
	svc := New("testbox")
	d := registerPeer(t, svc, srv.URL)
	if err := d.RefreshCapabilities(context.Background(), peerCaller); err != nil {
		t.Fatal(err)
	}

	rows, src := peerRowsOf(t, svc)
	if len(rows) != 1 {
		t.Fatalf("rows for the peer = %+v, want exactly one", rows)
	}
	r := rows[0]
	if r.Runtime != "claude-code-tmux" || !r.Capabilities.SupportsResume || r.Capabilities.Source != fleet.CapabilitiesObserved {
		t.Errorf("row = %+v, want the peer's one runtime, observed", r)
	}
	// A peer row that carried no models reads as none, with a list rather than null.
	if r.Models == nil || r.Models.Source != fleet.ModelsNone || r.Models.IDs == nil {
		t.Errorf("models = %+v, want none with an empty list", r.Models)
	}
	if src.Status != fleet.SourceOK {
		t.Errorf("peer source = %+v, want ok", src)
	}
}

// A runtime the peer lists as unavailable is not a row here, even when it is
// listed first: only what the peer can use is shown.
func TestListRuntimes_APeersUnavailableRuntimeStaysOut(t *testing.T) {
	srv := peerRuntimesServer(t,
		fleet.RuntimeInfo{
			Machine: "peerbox", Runtime: "aaa-late", Status: fleet.RuntimeUnavailable,
			Cause: fleet.CauseProbeTimeout, Reason: "did not answer in time",
			Capabilities: fleet.DriverCapabilities{DeadlineMs: 30000, Source: fleet.CapabilitiesAssumed},
		},
		peerRuntimeRow("peerbox", "opencode", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true}, nil),
		peerRuntimeRow("peerbox", "claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000}, nil),
	)
	svc := New("testbox")
	d := registerPeer(t, svc, srv.URL)
	if err := d.RefreshCapabilities(context.Background(), peerCaller); err != nil {
		t.Fatal(err)
	}

	rows, _ := peerRowsOf(t, svc)
	if len(rows) != 2 || rows[0].Runtime != "claude-code-tmux" || rows[1].Runtime != "opencode" {
		t.Fatalf("rows for the peer = %+v, want the two available runtimes only", rows)
	}
}

// A peer nobody has heard from has no runtime to name: no row, and a degraded
// source that says so — one driver, so no row per runtime of an unknown set.
func TestListRuntimes_AnUnreachedPeerHasNoRowsAndADegradedSource(t *testing.T) {
	srv := peerRuntimesServer(t) // never asked: the driver is not refreshed
	svc := New("testbox")
	registerPeer(t, svc, srv.URL)

	rows, src := peerRowsOf(t, svc)
	if len(rows) != 0 {
		t.Fatalf("rows for an unreached peer = %+v, want none", rows)
	}
	if src.Status != fleet.SourceDegraded {
		t.Errorf("peer source = %+v, want degraded", src)
	}
}

// peerTableDriver is a peer driver whose table of runtimes is scripted, to show
// the service degrading every row the driver gives it.
type peerTableDriver struct {
	peerCapsDriver
	rows []driver.PeerRuntime
}

func (p *peerTableDriver) PeerRuntimes() []driver.PeerRuntime { return p.rows }

var _ driver.PeerRuntimesReporter = (*peerTableDriver)(nil)

// A stale cache reaches this endpoint as rows whose capabilities are the assumed
// floor: every one of them is listed as such, with no posture and no models, and
// the peer's source is degraded — not just the first row.
func TestListRuntimes_AStaleCacheDegradesEveryRow(t *testing.T) {
	floor := func(rt fleet.RuntimeId) driver.PeerRuntime {
		return driver.PeerRuntime{
			Runtime:      rt,
			Capabilities: fleet.DriverCapabilities{DeadlineMs: 5000, Source: fleet.CapabilitiesAssumed},
			Models:       fleet.NoModels(),
		}
	}
	svc := New("testbox")
	if err := svc.RegisterPeerDriver("peerbox", &peerTableDriver{
		peerCapsDriver: peerCapsDriver{caps: fleet.DriverCapabilities{DeadlineMs: 5000, Source: fleet.CapabilitiesAssumed}, runtime: "claude-code-tmux"},
		rows:           []driver.PeerRuntime{floor("claude-code-tmux"), floor("opencode")},
	}); err != nil {
		t.Fatal(err)
	}

	rows, src := peerRowsOf(t, svc)
	if len(rows) != 2 {
		t.Fatalf("rows for the peer = %+v, want both runtimes still listed", rows)
	}
	for _, r := range rows {
		if r.Capabilities.Source != fleet.CapabilitiesAssumed || r.Posture != nil {
			t.Errorf("%s: %+v, want assumed capabilities and no posture", r.Runtime, r)
		}
		if r.Models == nil || r.Models.Source != fleet.ModelsNone {
			t.Errorf("%s: models = %+v, want none", r.Runtime, r.Models)
		}
	}
	if src.Status != fleet.SourceDegraded {
		t.Errorf("peer source = %+v, want degraded", src)
	}
}
