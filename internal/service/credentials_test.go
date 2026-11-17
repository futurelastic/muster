package service

import (
	"encoding/json"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// GET /v1/runtimes lists which provider credentials a runtime can be given
// (muster #497), so a client checks a choice against the machine that starts the
// runtime instead of running the runtime's own programs to find out.

type credentialsDriver struct {
	stub.Driver
	creds []fleet.RuntimeCredential
}

func (c *credentialsDriver) Credentials() []fleet.RuntimeCredential { return c.creds }

var _ driver.CredentialReporter = (*credentialsDriver)(nil)

func TestListRuntimes_ARowCarriesTheCredentialsItsDriverReportsAndOtherRowsCarryNone(t *testing.T) {
	svc := New("test-machine")
	want := []fleet.RuntimeCredential{
		{Provider: "openai", Kind: fleet.CredentialAPIKey, Source: fleet.SourceOperatorEnv, State: fleet.CredentialUsable},
		{Provider: "openai", Kind: fleet.CredentialSubscription, Source: fleet.SourceGrant, State: fleet.CredentialRevoked,
			Reason: "the provider refused the stored refresh token; sign in again"},
	}
	if err := svc.RegisterLocalDriver("rt-with-keys", &credentialsDriver{Driver: stub.Driver{DeadlineMs: 200}, creds: want}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("rt-silent", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	by := map[fleet.RuntimeId]fleet.RuntimeInfo{}
	for _, r := range runtimeRows(t, svc) {
		by[r.Runtime] = r
	}
	got := by["rt-with-keys"].Credentials
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("credentials = %+v, want %+v", got, want)
	}
	if c := by["rt-silent"].Credentials; c != nil {
		t.Fatalf("a runtime that cannot say reported %+v", c)
	}
	// Absent on the wire when nobody reported: `credentials: []` would read as "none held".
	raw, _ := json.Marshal(by["rt-silent"])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, present := m["credentials"]; present {
		t.Fatalf("a row nobody reported for carries a credentials key: %s", raw)
	}
	// And the wire spelling a client reads.
	raw, _ = json.Marshal(by["rt-with-keys"])
	var wire struct {
		Credentials []map[string]string `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Credentials) != 2 {
		t.Fatalf("row does not encode: %s (%v)", raw, err)
	}
	if first := wire.Credentials[0]; first["provider"] != "openai" || first["kind"] != "api-key" ||
		first["source"] != "operator-env" || first["state"] != "usable" {
		t.Fatalf("first credential on the wire = %v", first)
	}
	if _, has := wire.Credentials[0]["reason"]; has {
		t.Fatalf("a usable credential carries a reason on the wire: %v", wire.Credentials[0])
	}
}
