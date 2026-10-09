package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// The composer read across a federation hop (muster #276): forwarded as a GET
// with the corroborating startedAt, and a peer that predates the route is
// unsupported — never "no such session".

func TestComposerForwardsTheReadAndStartedAt(t *testing.T) {
	var rec capture
	want := fleet.ComposerRead{Text: "unsent words", ComposerDigest: "dg"}
	srv := peerServing(t, 200, want, &rec)
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	req := caller
	req.Expect.StartedAt = &started

	got, err := New("peerbox", srv.URL).Composer(context.Background(), req, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	if rec.method != http.MethodGet || rec.path != "/v1/machines/peerbox/sessions/s1/composer" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
	if !strings.Contains(rec.query, "startedAt=2026-03-04T05%3A06%3A07Z") {
		t.Errorf("query = %q, want startedAt forwarded", rec.query)
	}
}

func TestComposerOnAPeerWithoutTheRouteIsUnsupported(t *testing.T) {
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(bare.Close)
	_, err := New("oldbox", bare.URL).Composer(context.Background(), caller, fleet.SessionRef{Machine: "oldbox", ID: "s1"})
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("bare 404 → %v, want unsupported", err)
	}

	enveloped := peerServing(t, 404, fleet.ErrorEnvelope{Error: fleet.Error{Kind: fleet.ErrorNotFound, Message: "gone"}}, nil)
	_, err = New("peerbox", enveloped.URL).Composer(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if kindOf(err) != fleet.ErrorNotFound {
		t.Fatalf("enveloped 404 → %v, want not_found", err)
	}
}
