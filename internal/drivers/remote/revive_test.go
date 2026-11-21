package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// The revive verb across a federation hop (muster #427): relayed with the
// environment, the context file and the corroborating startedAt; a peer that
// predates the route is unsupported, never "no such session"; the peer's own
// refusal is adopted as it is. MayBypass is the serving machine's own answer and
// never crosses the hop.

func TestReviveForwardsEnvContextRefStartedAtAndRuntime(t *testing.T) {
	var rec capture
	back := fleet.Session{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"}, State: fleet.ObservedState(fleet.StatusIdle, "fixture", nil)}
	srv := peerServing(t, 200, back, &rec)
	d := New("peerbox", srv.URL)
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	req := caller
	req.Expect.StartedAt = &started

	got, err := d.Revive(context.Background(), req, fleet.SessionRef{Machine: "peerbox", ID: "s1"}, driver.ReviveSpec{
		Env:        map[string]string{"MODEL_KEY": "v1", "OTHER": "v2"},
		ContextRef: "/work/brief.md",
		MayBypass:  true,
	})
	if err != nil || got.ID != "s1" {
		t.Fatalf("Revive = %+v, %v; want the peer's session back whole", got, err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/machines/peerbox/sessions/s1/revive" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
	if !strings.Contains(rec.query, "startedAt=2026-03-04T05%3A06%3A07Z") {
		t.Errorf("query = %q, want startedAt forwarded", rec.query)
	}
	for _, want := range []string{`"MODEL_KEY":"v1"`, `"OTHER":"v2"`, `"contextRef":"/work/brief.md"`} {
		if !strings.Contains(rec.body, want) {
			t.Errorf("body = %q, want %s", rec.body, want)
		}
	}
	if strings.Contains(strings.ToLower(rec.body), "bypass") {
		t.Errorf("body = %q: the serving machine's own answer about its principal must not be carried", rec.body)
	}
}

func TestReviveOnAPeerWithoutTheRouteIsUnsupported(t *testing.T) {
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(bare.Close)
	spec := driver.ReviveSpec{Env: map[string]string{"A": "b"}}
	_, err := New("oldbox", bare.URL).Revive(context.Background(), caller, fleet.SessionRef{Machine: "oldbox", ID: "s1"}, spec)
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("bare 404 → %v, want unsupported", err)
	}

	enveloped := peerServing(t, 404, fleet.ErrorEnvelope{Error: fleet.Error{Kind: fleet.ErrorNotFound, Message: "gone"}}, nil)
	_, err = New("peerbox", enveloped.URL).Revive(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"}, spec)
	if kindOf(err) != fleet.ErrorNotFound {
		t.Fatalf("enveloped 404 → %v, want not_found (the session really is gone)", err)
	}
}

// The peer's own refusal — its grant, its reason — is adopted as it is.
func TestRevivePeerRefusalIsAdoptedAsIs(t *testing.T) {
	spec := driver.ReviveSpec{Env: map[string]string{"A": "b"}}
	ref := fleet.SessionRef{Machine: "peerbox", ID: "s1"}

	refused := peerServing(t, 409, fleet.ErrorEnvelope{Error: fleet.Error{
		Kind: fleet.ErrorConflict, Reason: fleet.ReasonNotRevivable, Message: "revive: the session is running",
	}}, nil)
	_, err := New("peerbox", refused.URL).Revive(context.Background(), caller, ref, spec)
	fe, ok := err.(*fleet.Error)
	if !ok || fe.Kind != fleet.ErrorConflict || fe.Reason != fleet.ReasonNotRevivable {
		t.Fatalf("err = %v; want the peer's conflict with its reason", err)
	}

	denied := peerServing(t, 401, fleet.ErrorEnvelope{Error: fleet.Error{Kind: fleet.ErrorUnauthorized, Message: "principal x does not hold the create grant"}}, nil)
	_, err = New("peerbox", denied.URL).Revive(context.Background(), caller, ref, spec)
	if kindOf(err) != fleet.ErrorUnauthorized || !strings.Contains(err.Error(), "create grant") {
		t.Fatalf("err = %v, want the peer's unauthorized, naming the grant", err)
	}
}

// The capability is the peer's own declaration, relayed whole: a peer that predates
// it reads "not declared", and a declared false is not lost.
func TestRefreshCapabilitiesRelaysRevives(t *testing.T) {
	older := peerServing(t, 200, runtimesBody(`{"deadlineMs":5000,"source":"observed"}`), nil)
	d := New("peerbox", older.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if c := d.Capabilities().Revives; c != nil {
		t.Errorf("an older peer reads revives = %v; want not declared", *c)
	}

	declared := peerServing(t, 200, runtimesBody(`{"revives":true,"deadlineMs":5000,"source":"observed"}`), nil)
	d = New("peerbox", declared.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if c := d.Capabilities().Revives; c == nil || !*c {
		t.Errorf("revives = %v; want a declared yes relayed", c)
	}

	no := peerServing(t, 200, runtimesBody(`{"revives":false,"deadlineMs":5000,"source":"observed"}`), nil)
	d = New("peerbox", no.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if c := d.Capabilities().Revives; c == nil || *c {
		t.Errorf("revives = %v; want a declared false kept (\"cannot\" is not \"not declared\")", c)
	}
}
