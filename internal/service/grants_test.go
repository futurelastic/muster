package service

import (
	"encoding/json"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// GET /v1/runtimes says where a runtime's provider sign-in stands (muster #451):
// the one field that turns "every turn on this provider fails" into a reason.

type grantsDriver struct {
	stub.Driver
	grants []fleet.RuntimeGrant
}

func (g *grantsDriver) Grants() []fleet.RuntimeGrant { return g.grants }

var _ driver.GrantReporter = (*grantsDriver)(nil)

func TestListRuntimes_ARowCarriesTheGrantsItsDriverReportsAndOtherRowsCarryNone(t *testing.T) {
	svc := New("test-machine")
	exp := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if err := svc.RegisterLocalDriver("rt-signed-in", &grantsDriver{
		Driver: stub.Driver{DeadlineMs: 200},
		grants: []fleet.RuntimeGrant{{
			Provider: "openai", State: fleet.GrantRevoked, AccessExpiresAt: &exp,
			Reason: "the provider refused the stored refresh token (HTTP 401); sign in again (run: muster grant login)",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("rt-silent", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	rows := runtimeRows(t, svc)
	by := map[fleet.RuntimeId]fleet.RuntimeInfo{}
	for _, r := range rows {
		by[r.Runtime] = r
	}
	got := by["rt-signed-in"].Grants
	if len(got) != 1 || got[0].State != fleet.GrantRevoked || got[0].Provider != "openai" || got[0].AccessExpiresAt == nil {
		t.Fatalf("grants = %+v, want the revoked openai sign-in", got)
	}
	if g := by["rt-silent"].Grants; g != nil {
		t.Fatalf("a runtime with no sign-in reported %+v", g)
	}
	// Absent on the wire when there is none: `grants: []` would read as "signed out".
	raw, _ := json.Marshal(by["rt-silent"])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, present := m["grants"]; present {
		t.Fatalf("a row with no sign-in carries a grants key: %s", raw)
	}
	raw, _ = json.Marshal(by["rt-signed-in"])
	if string(raw) == "" || !json.Valid(raw) {
		t.Fatal("row does not encode")
	}
}
