package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// GET /v1/runtimes carries a `models` block on every row (muster #400): what a
// client may offer as the choices for a create, or "none" when the runtime
// cannot say. These tests pin that every KIND of row has one, and that none is
// ever null where a list belongs.

// modelsDriver is a driver that reports its own model list.
type modelsDriver struct {
	stub.Driver
	models fleet.RuntimeModels
}

func (m *modelsDriver) Models() fleet.RuntimeModels { return m.models }

var _ driver.ModelsReporter = (*modelsDriver)(nil)

// peerModelsDriver is a peer row with a scripted capability value and models.
type peerModelsDriver struct {
	peerCapsDriver
	models fleet.RuntimeModels
}

func (p *peerModelsDriver) Models() fleet.RuntimeModels { return p.models }

func TestListRuntimes_EveryRowCarriesModels(t *testing.T) {
	svc := New("test-machine")
	readAt := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	def := "deepseek/deepseek-flash"
	if err := svc.RegisterLocalDriver("rt-listing", &modelsDriver{
		Driver: stub.Driver{DeadlineMs: 200},
		models: fleet.RuntimeModels{
			Source: fleet.ModelsFromRuntime, ReadAt: &readAt,
			IDs: []string{"deepseek/deepseek-flash", "deepseek/deepseek-v4-pro"}, Default: &def,
		},
	}); err != nil {
		t.Fatal(err)
	}
	// A driver with no way to say reads "none", and a loosely formed value is
	// fixed on the way out.
	if err := svc.RegisterLocalDriver("rt-silent", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("rt-loose", &modelsDriver{
		Driver: stub.Driver{DeadlineMs: 200},
		models: fleet.RuntimeModels{Source: fleet.ModelsDeclared},
	}); err != nil {
		t.Fatal(err)
	}
	b := &lateBuild{}
	b.set(nil, &driver.UnavailableError{Cause: fleet.CauseNotInstalled, Err: errors.New("not found")})
	stop := svc.StartOptionalRuntime(context.Background(), optional(b))
	defer stop()
	if err := svc.RegisterPeerDriver("peerbox", &peerModelsDriver{
		peerCapsDriver: peerCapsDriver{
			caps:    fleet.DriverCapabilities{DeadlineMs: 3000, Source: fleet.CapabilitiesObserved},
			runtime: "opencode",
		},
		models: fleet.RuntimeModels{Source: fleet.ModelsFromRuntime, ReadAt: &readAt, IDs: []string{"deepseek/deepseek-flash"}},
	}); err != nil {
		t.Fatal(err)
	}

	rows := runtimeRows(t, svc)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Models == nil {
			t.Fatalf("row %s/%s has no models block: every row this build emits carries one", r.Machine, r.Runtime)
		}
		if r.Models.IDs == nil {
			t.Errorf("row %s/%s: ids is null, want a list", r.Machine, r.Runtime)
		}
	}

	listing, _ := findRow(rows, "rt-listing")
	if m := listing.Models; m.Source != fleet.ModelsFromRuntime || len(m.IDs) != 2 || m.Default == nil || *m.Default != def ||
		m.ReadAt == nil || !m.ReadAt.Equal(readAt) {
		t.Errorf("rt-listing models = %+v, want the driver's value as it said it", m)
	}
	silent, _ := findRow(rows, "rt-silent")
	if silent.Models.Source != fleet.ModelsNone || len(silent.Models.IDs) != 0 {
		t.Errorf("a driver with no reporter: %+v, want none and no ids", silent.Models)
	}
	loose, _ := findRow(rows, "rt-loose")
	if loose.Models.Source != fleet.ModelsDeclared || loose.Models.IDs == nil || len(loose.Models.IDs) != 0 {
		t.Errorf("a declared value with nil ids: %+v, want declared and an empty list", loose.Models)
	}
	late, _ := findRow(rows, "rt-late")
	if late.Status != fleet.RuntimeUnavailable || late.Models.Source != fleet.ModelsNone {
		t.Errorf("an enabled runtime with no driver: %+v, want unavailable and none", late)
	}
	peer, _ := findRow(rows, "opencode")
	if peer.Machine != "peerbox" || peer.Models.Source != fleet.ModelsFromRuntime || len(peer.Models.IDs) != 1 {
		t.Errorf("a peer's row: %+v, want the peer driver's value", peer)
	}
}

// The wire form: `ids` is an array and `default` is present (null when
// unknown), `readAt` absent unless the runtime was read.
func TestRuntimesWire_ModelsShape(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-silent", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	srv := newServer(svc, t)
	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/runtimes", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}
	raw, ok := body.Items[0]["models"]
	if !ok {
		t.Fatal("the row has no `models` key")
	}
	var models map[string]json.RawMessage
	if err := json.Unmarshal(raw, &models); err != nil {
		t.Fatal(err)
	}
	if string(models["source"]) != `"none"` || string(models["ids"]) != `[]` || string(models["default"]) != `null` {
		t.Errorf("models = %s, want source none, ids [], default null", raw)
	}
	if _, has := models["readAt"]; has {
		t.Errorf("models = %s: readAt is present for a list nothing was read for", raw)
	}
}
