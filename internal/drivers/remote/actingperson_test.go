package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/service"
)

// muster #349 across one relay hop: caller -> entry service -> remote driver ->
// owning service. The person is recorded on both ends, and dropped (and
// recorded) on the owner when the entry machine is not trusted with the grant.

type recordingDriver struct{ stub.Driver }

func (recordingDriver) Send(context.Context, fleet.Request, fleet.SessionRef, string, driver.SendOptions) (fleet.DeliveryReceipt, error) {
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
}

type auditPage struct {
	Records []struct {
		Principal     string `json:"principal"`
		Person        string `json:"person"`
		PersonDropped string `json:"personDropped"`
		Direction     string `json:"direction"`
	} `json:"records"`
}

func hop(t *testing.T, peerGrants []service.Grant) (entry, owner *httptest.Server) {
	t.Helper()
	// The owning machine: a peer "entry" is configured, with whatever grants
	// the test hands it.
	svcB := service.New("owner")
	if err := svcB.RegisterLocalDriver("stub", &recordingDriver{Driver: stub.Driver{DeadlineMs: 500}}); err != nil {
		t.Fatal(err)
	}
	if err := svcB.RegisterPeerDriver("entry", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	owner = httptest.NewServer(service.NewMux(svcB, service.Config{AllowLocalMutations: true, Principals: []service.Principal{
		{Name: "entry", Token: "entry-on-owner", Grants: peerGrants},
		{Name: "auditor", Token: "auditor-b", Grants: []service.Grant{service.GrantRead}},
	}}))
	t.Cleanup(owner.Close)

	// The entry machine: it reaches the owner as "owner", presenting its own
	// credential for it.
	svcA := service.New("entry")
	rd := New("owner", owner.URL, WithIdentity("entry-on-owner"), WithSelf("entry"))
	if err := svcA.RegisterPeerDriver("owner", rd); err != nil {
		t.Fatal(err)
	}
	entry = httptest.NewServer(service.NewMux(svcA, service.Config{AllowLocalMutations: true, AllowPeerRelay: true, Principals: []service.Principal{
		{Name: "webui", Token: "webui-token", Grants: []service.Grant{service.GrantSend, service.GrantRelay, service.GrantActingPerson}},
		{Name: "plain", Token: "plain-token", Grants: []service.Grant{service.GrantSend, service.GrantRelay}},
		{Name: "auditor", Token: "auditor-a", Grants: []service.Grant{service.GrantRead}},
	}}))
	t.Cleanup(entry.Close)
	return entry, owner
}

func send(t *testing.T, srv *httptest.Server, token, text string, h map[string]string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"text": text})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/owner/sessions/s1/input", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}

func audit(t *testing.T, srv *httptest.Server, token, machine string) auditPage {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/"+machine+"/input-audit?session=s1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page auditPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestActingPersonCrossesOneRelayHop(t *testing.T) {
	entry, owner := hop(t, []service.Grant{service.GrantSend, service.GrantActingPerson})
	send(t, entry, "webui-token", "hello", map[string]string{"Fleet-Acting-Person": "u-9", "Fleet-Acting-Issuer": "idp-a"})

	a := audit(t, entry, "auditor-a", "entry")
	if len(a.Records) != 1 || a.Records[0].Person != "u-9@idp-a" || a.Records[0].Direction != "relayed-out" || a.Records[0].Principal != "webui" {
		t.Errorf("entry end: %+v", a.Records)
	}
	b := audit(t, owner, "auditor-b", "owner")
	if len(b.Records) != 1 || b.Records[0].Person != "u-9@idp-a" || b.Records[0].Direction != "relayed-in" || b.Records[0].Principal != "webui via entry" {
		t.Errorf("owner end: %+v", b.Records)
	}
}

// The owner trusts the entry machine only if it holds the grant there, even
// when the entry machine honoured the claim for its own caller.
func TestActingPersonDroppedByTheOwnerWhenThePeerLacksTheGrant(t *testing.T) {
	entry, owner := hop(t, []service.Grant{service.GrantSend})
	send(t, entry, "webui-token", "hello", map[string]string{"Fleet-Acting-Person": "u-9", "Fleet-Acting-Issuer": "idp-a"})

	b := audit(t, owner, "auditor-b", "owner")
	if len(b.Records) != 1 || b.Records[0].Person != "" || !strings.Contains(b.Records[0].PersonDropped, "entry") {
		t.Errorf("owner end: %+v", b.Records)
	}
	// The entry machine's own record still names the person: it did honour it.
	a := audit(t, entry, "auditor-a", "entry")
	if len(a.Records) != 1 || a.Records[0].Person != "u-9@idp-a" {
		t.Errorf("entry end: %+v", a.Records)
	}
}

// A caller without the grant at the entry machine is dropped there and never
// forwarded, so the owner sees no claim at all.
func TestActingPersonFromAnUngrantedCallerIsNotForwarded(t *testing.T) {
	entry, owner := hop(t, []service.Grant{service.GrantSend, service.GrantActingPerson})
	send(t, entry, "plain-token", "hello", map[string]string{"Fleet-Acting-Person": "u-9", "Fleet-Acting-Issuer": "idp-a"})

	a := audit(t, entry, "auditor-a", "entry")
	if len(a.Records) != 1 || a.Records[0].Person != "" || a.Records[0].PersonDropped == "" {
		t.Errorf("entry end: %+v", a.Records)
	}
	b := audit(t, owner, "auditor-b", "owner")
	if len(b.Records) != 1 || b.Records[0].Person != "" || b.Records[0].PersonDropped != "" {
		t.Errorf("owner end: %+v", b.Records)
	}
}
