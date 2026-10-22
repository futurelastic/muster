package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #349. A request may name the person it acts for; it is recorded only
// when the principal that authenticated holds the acting-person grant, and a
// claim that is not honoured is dropped AND recorded.

var actingPrincipals = []Principal{
	{Name: "entrybox", Token: "peer-token", Grants: []Grant{GrantSend, GrantActingPerson}},
	{Name: "ungranted-peer", Token: "ungranted-peer-token", Grants: []Grant{GrantSend}},
	{Name: "webui", Token: "webui-token", Grants: []Grant{GrantSend, GrantActingPerson}},
	{Name: "agent", Token: "agent-token", Grants: []Grant{GrantSend}},
	{Name: "auditor", Token: "auditor-token", Grants: []Grant{GrantRead}},
}

func actingServer(t *testing.T) *httptest.Server {
	t.Helper()
	svc := New("owner")
	if err := svc.RegisterLocalDriver("stub", &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []string{"entrybox", "ungranted-peer"} {
		if err := svc.RegisterPeerDriver(fleet.MachineId(peer), &stub.Driver{}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: actingPrincipals}))
	t.Cleanup(srv.Close)
	return srv
}

func actingHeaders(id, issuer string) map[string]string {
	h := map[string]string{}
	if id != "" {
		h[actingPersonHeader] = id
	}
	if issuer != "" {
		h[actingIssuerHeader] = issuer
	}
	return h
}

func withHeaders(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func recordFor(t *testing.T, srv *httptest.Server, text string) InputRecord {
	t.Helper()
	_, page := getAudit(t, srv, "owner", "auditor-token", "sha256="+textSHA256(text))
	if len(page.Records) != 1 {
		t.Fatalf("records for %q = %+v", text, page.Records)
	}
	return page.Records[0]
}

func TestActingPerson_RecordedLocallyFromAGrantedPrincipal(t *testing.T) {
	srv := actingServer(t)
	if resp := postInput(t, srv, "webui-token", actingHeaders("u-42", "idp-a"), map[string]any{"text": "hello"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	r := recordFor(t, srv, "hello")
	if r.Person != "u-42@idp-a" || r.PersonDropped != "" {
		t.Errorf("person = %q, dropped = %q", r.Person, r.PersonDropped)
	}
	// The principal is unchanged, so a filter on it still finds the call.
	if r.Principal != "webui" || r.Direction != "local" {
		t.Errorf("record = %+v", r)
	}
}

func TestActingPerson_DroppedAndRecordedFromAPrincipalWithoutTheGrant(t *testing.T) {
	srv := actingServer(t)
	// The call itself still goes through: the claim is attribution, never a gate.
	if resp := postInput(t, srv, "agent-token", actingHeaders("u-42", "idp-a"), map[string]any{"text": "from an agent"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	r := recordFor(t, srv, "from an agent")
	if r.Person != "" {
		t.Errorf("an ungranted principal's claim was believed: %+v", r)
	}
	if !strings.Contains(r.PersonDropped, "u-42@idp-a") || !strings.Contains(r.PersonDropped, "agent") ||
		!strings.Contains(r.PersonDropped, string(GrantActingPerson)) {
		t.Errorf("the drop is not explained: %q", r.PersonDropped)
	}
}

func TestActingPerson_RelayedInHonouredOnlyFromAConfiguredPeerHoldingTheGrant(t *testing.T) {
	srv := actingServer(t)
	relayed := map[string]string{onBehalfOfHeader: "webui"}

	postInput(t, srv, "peer-token", withHeaders(relayed, actingHeaders("u-7", "idp-a")), map[string]any{"text": "via granted peer"})
	r := recordFor(t, srv, "via granted peer")
	if r.Person != "u-7@idp-a" || r.Principal != "webui via entrybox" || r.Direction != "relayed-in" {
		t.Errorf("granted peer: %+v", r)
	}

	// A configured peer that was not given the grant.
	postInput(t, srv, "ungranted-peer-token", withHeaders(relayed, actingHeaders("u-7", "idp-a")), map[string]any{"text": "via ungranted peer"})
	r = recordFor(t, srv, "via ungranted peer")
	if r.Person != "" || !strings.Contains(r.PersonDropped, "ungranted-peer") {
		t.Errorf("ungranted peer: %+v", r)
	}

	// A caller that is not a peer at all, relaying or not: even holding the
	// grant itself, an on-behalf-of assertion it has no standing to make
	// poisons the claim with it.
	postInput(t, srv, "webui-token", withHeaders(relayed, actingHeaders("u-7", "idp-a")), map[string]any{"text": "via non-peer"})
	r = recordFor(t, srv, "via non-peer")
	if r.Person != "" || !strings.Contains(r.PersonDropped, "not a configured peer") {
		t.Errorf("non-peer relay: %+v", r)
	}
	if r.Direction != "local" {
		t.Errorf("a non-peer's relay claim must not read as relayed-in: %+v", r)
	}
}

func TestActingPerson_MalformedClaimsAreDropped(t *testing.T) {
	srv := actingServer(t)
	for name, h := range map[string]map[string]string{
		"no issuer":    actingHeaders("u-1", ""),
		"no id":        actingHeaders("", "idp-a"),
		"too long":     actingHeaders(strings.Repeat("x", 129), "idp-a"),
		"control char": {actingPersonHeader: "a\tb", actingIssuerHeader: "idp-a"},
	} {
		text := "malformed " + name
		postInput(t, srv, "webui-token", h, map[string]any{"text": text})
		r := recordFor(t, srv, text)
		if r.Person != "" || r.PersonDropped == "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func TestActingPerson_NoClaimLeavesTheRecordUntouched(t *testing.T) {
	srv := actingServer(t)
	postInput(t, srv, "webui-token", nil, map[string]any{"text": "plain"})
	if r := recordFor(t, srv, "plain"); r.Person != "" || r.PersonDropped != "" {
		t.Errorf("record = %+v", r)
	}
}

// With no principal table a shared token names nobody, so no caller can hold
// the grant: the claim is dropped.
func TestActingPerson_DroppedOnAMachineWithNoPrincipalTable(t *testing.T) {
	svc := New("owner")
	if err := svc.RegisterLocalDriver("stub", &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: "shared", AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	postInput(t, srv, "shared", actingHeaders("u-1", "idp-a"), map[string]any{"text": "shared token"})
	_, page := getAudit(t, srv, "owner", "shared", "sha256="+textSHA256("shared token"))
	if len(page.Records) != 1 || page.Records[0].Person != "" || !strings.Contains(page.Records[0].PersonDropped, "no principal table") {
		t.Errorf("records = %+v", page.Records)
	}
}

// The mutation audit line leads with the person.
func TestActingPerson_ActorLineIsPersonThenPrincipalViaPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(onBehalfOfHeader, "webui")
	p := Principal{Name: "entrybox"}
	req = req.WithContext(withActing(req.Context(), actingResult{Person: &fleet.Person{ID: "u-7", Issuer: "idp-a"}}))
	if got := actorOf(p, req); got != "u-7@idp-a · webui via entrybox" {
		t.Errorf("actor = %q", got)
	}
}
