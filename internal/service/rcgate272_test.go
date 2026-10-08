package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// rcOwner is an owning machine whose table has a peer holding the
// remote-control grant here, a peer without it, and three ordinary callers:
// send only, send + remote-control, send + human-relay.
func rcOwner(t *testing.T) (*sendCapturingDriver, *httptest.Server) {
	t.Helper()
	svc := New("owner")
	d := &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"entrybox", "plainpeer"} {
		if err := svc.RegisterPeerDriver(fleet.MachineId(p), &stub.Driver{}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "entrybox", Token: "peer-rc", Grants: []Grant{GrantSend, GrantRemoteControl}},
		{Name: "plainpeer", Token: "peer-plain", Grants: []Grant{GrantSend}},
		{Name: "sender", Token: "send-only", Grants: []Grant{GrantSend}},
		{Name: "toggler", Token: "send-rc", Grants: []Grant{GrantSend, GrantRemoteControl}},
		{Name: "person", Token: "send-human", Grants: []Grant{GrantSend, GrantHumanRelay}},
	}}))
	t.Cleanup(srv.Close)
	return d, srv
}

func sendFact(t *testing.T, srv *httptest.Server, d *sendCapturingDriver, token string, headers map[string]string) (remoteControl, humanRelay bool) {
	t.Helper()
	before := len(d.opts)
	resp := postInput(t, srv, token, headers, map[string]any{"text": "/rc", "submit": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(d.opts) != before+1 {
		t.Fatalf("driver saw %d new sends, want 1", len(d.opts)-before)
	}
	o := d.opts[len(d.opts)-1]
	return o.RemoteControl, o.HumanRelay
}

// #272: the service tells the driver, from the caller's own grants, whether
// this send is made with authority over remote control.
func TestInputCarriesTheRemoteControlFactFromTheCallersGrants(t *testing.T) {
	d, srv := rcOwner(t)
	for _, tc := range []struct {
		token     string
		rc, human bool
	}{
		{"send-only", false, false},
		{"send-rc", true, false},
		{"send-human", false, true},
	} {
		rc, human := sendFact(t, srv, d, tc.token, nil)
		if rc != tc.rc || human != tc.human {
			t.Errorf("%s: remoteControl=%v humanRelay=%v, want %v/%v", tc.token, rc, human, tc.rc, tc.human)
		}
	}
}

// Across a peer relay the fact travels as the entering machine's assertion,
// honoured only from a configured peer that itself holds the grant here. A
// peer holding the grant is NOT enough on its own: a relayed send-only caller
// must not inherit the peer's credential.
func TestRemoteControlFactCrossesAPeerRelayOnlyAsAnAssertion(t *testing.T) {
	d, srv := rcOwner(t)
	asserted := map[string]string{onBehalfOfHeader: "someone", remoteControlHeader: "1"}
	silent := map[string]string{onBehalfOfHeader: "someone"}

	if rc, _ := sendFact(t, srv, d, "peer-rc", asserted); !rc {
		t.Error("a trusted peer holding the grant, asserting it: want the fact")
	}
	if rc, _ := sendFact(t, srv, d, "peer-rc", silent); rc {
		t.Error("a relayed send-only caller inherited the peer's grant: want no fact")
	}
	if rc, _ := sendFact(t, srv, d, "peer-plain", asserted); rc {
		t.Error("a peer without the grant here asserted it: want no fact")
	}
	// Not a configured peer: the headers are ignored and its own grants decide.
	if rc, _ := sendFact(t, srv, d, "send-only", asserted); rc {
		t.Error("a non-peer's assertion was honoured")
	}
	if rc, _ := sendFact(t, srv, d, "send-rc", silent); !rc {
		t.Error("a non-peer holding the grant: want the fact from its own grant")
	}
}

// No principal table: one shared token, no per-caller identity. Whoever holds
// it is trusted with the remote-control verb whenever the host permits
// mutations, so the fact holds for them too.
func TestInputCarriesTheRemoteControlFactInSingleTokenMode(t *testing.T) {
	svc := New("owner")
	d := &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: "shared", AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	if rc, _ := sendFact(t, srv, d, "shared", nil); !rc {
		t.Error("single-token mode: want the fact")
	}
}
