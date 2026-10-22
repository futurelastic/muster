package remote

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/service"
)

// muster #350 through the real driver: caller -> entry service -> remote
// driver (signing) -> HTTP -> owning service (verifying).

func ownerWith(t *testing.T, self fleet.MachineId, entry service.Principal) *httptest.Server {
	t.Helper()
	svc := service.New(self)
	if err := svc.RegisterLocalDriver("stub", &recordingDriver{Driver: stub.Driver{DeadlineMs: 500}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("entry", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(service.NewMux(svc, service.Config{AllowLocalMutations: true, Principals: []service.Principal{
		entry,
		{Name: "auditor", Token: "auditor-b", Grants: []service.Grant{service.GrantRead}},
	}}))
	t.Cleanup(srv.Close)
	return srv
}

func TestKeyPeerAndTokenPeerSideBySide(t *testing.T) {
	pub, priv, _ := peerkey.Generate()
	keyPath := filepath.Join(t.TempDir(), "machine.key")
	if err := peerkey.SavePrivate(keyPath, priv); err != nil {
		t.Fatal(err)
	}
	signer, err := peerkey.NewSigner(keyPath, "entry")
	if err != nil {
		t.Fatal(err)
	}
	grants := []service.Grant{service.GrantRead, service.GrantSend, service.GrantActingPerson}
	keyed := ownerWith(t, "keyed", service.Principal{Name: "entry", PublicKey: pub, Grants: grants})
	tokened := ownerWith(t, "tokened", service.Principal{Name: "entry", Token: "entry-token", Grants: grants})

	svcA := service.New("entry")
	if err := svcA.RegisterPeerDriver("keyed", New("keyed", keyed.URL, WithSigner(signer), WithSelf("entry"))); err != nil {
		t.Fatal(err)
	}
	if err := svcA.RegisterPeerDriver("tokened", New("tokened", tokened.URL, WithIdentity("entry-token"), WithSelf("entry"))); err != nil {
		t.Fatal(err)
	}
	entry := httptest.NewServer(service.NewMux(svcA, service.Config{AllowLocalMutations: true, AllowPeerRelay: true, Principals: []service.Principal{
		{Name: "webui", Token: "webui-token", Grants: []service.Grant{service.GrantRead, service.GrantSend, service.GrantRelay, service.GrantActingPerson}},
	}}))
	t.Cleanup(entry.Close)

	for _, machine := range []string{"keyed", "tokened"} {
		raw, _ := json.Marshal(map[string]any{"text": "to " + machine})
		req, _ := http.NewRequest(http.MethodPost, entry.URL+"/v1/machines/"+machine+"/sessions/s1/input", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer webui-token")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Fleet-Acting-Person", "u-3")
		req.Header.Set("Fleet-Acting-Issuer", "idp")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", machine, resp.StatusCode)
		}
	}
	for name, srv := range map[string]*httptest.Server{"keyed": keyed, "tokened": tokened} {
		page := audit(t, srv, "auditor-b", name)
		if len(page.Records) != 1 || page.Records[0].Principal != "webui via entry" || page.Records[0].Person != "u-3@idp" {
			t.Errorf("%s: %+v", name, page.Records)
		}
	}

	// /v1/machines says which kind each peer uses. A read through the signing
	// driver is also signed (GET with a query), so the standing probe works.
	req, _ := http.NewRequest(http.MethodGet, entry.URL+"/v1/machines", nil)
	req.Header.Set("Authorization", "Bearer webui-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var machines struct {
		Items []struct {
			Machine string `json:"machine"`
			Peer    struct {
				Auth string `json:"auth"`
			} `json:"peer"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&machines); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range machines.Items {
		got[m.Machine] = m.Peer.Auth
	}
	if got["keyed"] != "key" || got["tokened"] != "token" {
		t.Errorf("peer auth kinds = %v", got)
	}
}

// A peer that does not list the key refuses it, and says why rather than
// reading as unreachable.
func TestKeyPeerUnknownToTheOwnerIsRefused(t *testing.T) {
	_, priv, _ := peerkey.Generate()
	keyPath := filepath.Join(t.TempDir(), "machine.key")
	_ = peerkey.SavePrivate(keyPath, priv)
	signer, _ := peerkey.NewSigner(keyPath, "entry")
	otherPub, _, _ := peerkey.Generate()
	owner := ownerWith(t, "owner", service.Principal{Name: "entry", PublicKey: otherPub, Grants: []service.Grant{service.GrantSend}})

	d := New("owner", owner.URL, WithSigner(signer))
	_, err := d.Send(t.Context(), fleet.Request{Caller: fleet.Caller{Principal: "webui"}}, fleet.SessionRef{Machine: "owner", ID: "s1"}, "x", sendOpts())
	var fe *fleet.Error
	if !asFleetError(err, &fe) || fe.Kind != fleet.ErrorUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func sendOpts() driver.SendOptions { return driver.SendOptions{Submit: true} }

func asFleetError(err error, target **fleet.Error) bool { return errors.As(err, target) }

// A signed GET (with a query) is accepted too: every call passes the one place
// that signs.
func TestSignedReadsAndProbesWork(t *testing.T) {
	pub, priv, _ := peerkey.Generate()
	keyPath := filepath.Join(t.TempDir(), "machine.key")
	_ = peerkey.SavePrivate(keyPath, priv)
	signer, _ := peerkey.NewSigner(keyPath, "entry")
	owner := ownerWith(t, "owner", service.Principal{Name: "entry", PublicKey: pub, Grants: []service.Grant{service.GrantRead}})
	d := New("owner", owner.URL, WithSigner(signer), WithSelf("entry"))
	req := fleet.Request{Caller: fleet.Caller{Principal: "system:self"}}
	if err := d.RefreshCapabilities(t.Context(), req); err != nil {
		t.Fatalf("signed GET /v1/runtimes: %v", err)
	}
	d.refreshStanding(t.Context(), req)
	st := d.PeerStanding()
	if st.Source != fleet.CapabilitiesObserved || len(st.GrantsToMe) != 1 || st.GrantsToMe[0] != "read" || st.Auth != "key" {
		t.Fatalf("standing = %+v", st)
	}
}
