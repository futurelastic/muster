package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// addressedPeer is a peer fronted by a driver that knows which of its several
// addresses it is using (#335).
type addressedPeer struct {
	stub.Driver
	addr string
	ok   bool
}

func (p *addressedPeer) Address() (string, bool) { return p.addr, p.ok }

var _ driver.AddressReporter = (*addressedPeer)(nil)

// GET /v1/machines names the address in use for a peer that has several, and
// says nothing at all — not an empty string — for one that has a single address.
func TestMachinesShowsTheAddressInUseOnlyForAMultiAddressPeer(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterPeerDriver("laptop", &addressedPeer{Driver: stub.Driver{DeadlineMs: 500}, addr: "http://10.2.2.5:7000", ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("solo", &addressedPeer{Driver: stub.Driver{DeadlineMs: 500}}); err != nil {
		t.Fatal(err)
	}

	col, err := svc.ListMachines(context.Background(), fleet.Request{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	laptop, _ := machineRow(t, col, "laptop")
	if laptop.Address != "http://10.2.2.5:7000" {
		t.Errorf("laptop address = %q", laptop.Address)
	}
	solo, _ := machineRow(t, col, "solo")
	self, _ := machineRow(t, col, "testbox")
	if solo.Address != "" || self.Address != "" {
		t.Errorf("solo=%q self=%q: only a peer with several addresses reports one", solo.Address, self.Address)
	}
	raw, _ := json.Marshal(solo)
	if strings.Contains(string(raw), `"address"`) {
		t.Errorf("the field must be omitted, not empty: %s", raw)
	}
}
