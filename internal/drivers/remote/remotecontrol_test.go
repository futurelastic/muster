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

// The remote-control verb across a federation hop (muster #269): relayed with
// its body and the corroborating startedAt, and a peer that predates the route
// is unsupported — never "no such session".

func TestSetRemoteControlForwardsEnabledAndStartedAt(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		var rec capture
		srv := peerServing(t, 202, fleet.Ack{Accepted: true}, &rec)
		d := New("peerbox", srv.URL)
		started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
		req := caller
		req.Expect.StartedAt = &started

		ack, err := d.SetRemoteControl(context.Background(), req, fleet.SessionRef{Machine: "peerbox", ID: "s1"}, enabled)
		if err != nil || !ack.Accepted {
			t.Fatalf("enabled=%v: ack %+v err %v", enabled, ack, err)
		}
		if rec.method != http.MethodPost || rec.path != "/v1/machines/peerbox/sessions/s1/remote-control" {
			t.Errorf("request = %s %s", rec.method, rec.path)
		}
		if !strings.Contains(rec.query, "startedAt=2026-03-04T05%3A06%3A07Z") {
			t.Errorf("query = %q, want startedAt forwarded", rec.query)
		}
		want := `"enabled":true`
		if !enabled {
			want = `"enabled":false`
		}
		if !strings.Contains(rec.body, want) {
			t.Errorf("body = %q, want %s (false must not be dropped)", rec.body, want)
		}
	}
}

func TestSetRemoteControlOnAPeerWithoutTheRouteIsUnsupported(t *testing.T) {
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(bare.Close)
	_, err := New("oldbox", bare.URL).SetRemoteControl(context.Background(), caller, fleet.SessionRef{Machine: "oldbox", ID: "s1"}, true)
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("bare 404 → %v, want unsupported", err)
	}

	enveloped := peerServing(t, 404, fleet.ErrorEnvelope{Error: fleet.Error{Kind: fleet.ErrorNotFound, Message: "gone"}}, nil)
	_, err = New("peerbox", enveloped.URL).SetRemoteControl(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"}, true)
	if kindOf(err) != fleet.ErrorNotFound {
		t.Fatalf("enveloped 404 → %v, want not_found (the session really is gone)", err)
	}
}

// The peer's own refusal — its grant, its capability — is adopted as it is.
func TestSetRemoteControlAdoptsThePeersRefusal(t *testing.T) {
	srv := peerServing(t, 401, fleet.ErrorEnvelope{Error: fleet.Error{Kind: fleet.ErrorUnauthorized, Message: "principal x does not hold the remote-control grant"}}, nil)
	_, err := New("peerbox", srv.URL).SetRemoteControl(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"}, true)
	if kindOf(err) != fleet.ErrorUnauthorized || !strings.Contains(err.Error(), "remote-control grant") {
		t.Fatalf("err = %v, want the peer's unauthorized, naming the grant", err)
	}
}
