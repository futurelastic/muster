package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #508: the service listens before the startup reconcile has finished.
// Reads are answered the whole time; a call that changes one of this machine's
// own sessions is refused, retryably and by name, until the reconcile has applied
// its result.

func reconcilingServer(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	svc, srv := newTestServer(t)
	svc.SetReconciling(true)
	return svc, srv
}

func callAs(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestMutationWhileReconcilingIsRetryableUnreachable(t *testing.T) {
	_, srv := reconcilingServer(t)
	for _, c := range []struct{ name, method, path, body string }{
		{"create", "POST", "/v1/machines/test-machine/sessions", `{"cwd":"/tmp"}`},
		{"input", "POST", "/v1/machines/test-machine/sessions/s1/input", `{"text":"x"}`},
		{"close", "DELETE", "/v1/machines/test-machine/sessions/s1", ``},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := callAs(t, srv, c.method, c.path, c.body)
			if resp.StatusCode != http.StatusGatewayTimeout {
				t.Fatalf("status = %d, want 504", resp.StatusCode)
			}
			if got := resp.Header.Get("Retry-After"); got != "5" {
				t.Errorf("Retry-After = %q, want 5", got)
			}
			e := decodeError(t, resp).Error
			if e.Kind != fleet.ErrorUnreachable || e.Reason != fleet.ReasonReconciling || !e.Retryable {
				t.Errorf("error = %+v, want unreachable / reconciling / retryable", e)
			}
		})
	}

	// And it ends: once the window closes the same call is no longer held.
	svc, srv := reconcilingServer(t)
	svc.SetReconciling(false)
	resp := callAs(t, srv, "POST", "/v1/machines/test-machine/sessions/s1/input", `{"text":"x"}`)
	if e := decodeError(t, resp).Error; e.Reason == fleet.ReasonReconciling {
		t.Errorf("still held after the reconcile finished: %+v", e)
	}
}

func TestReadWhileReconcilingIsServed(t *testing.T) {
	_, srv := reconcilingServer(t)
	for _, path := range []string{
		"/v1/health",
		"/v1/sessions",
		"/v1/runtimes",
	} {
		if resp := callAs(t, srv, "GET", path, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d while reconciling, want 200", path, resp.StatusCode)
		}
	}
}

// A caller who may not do the thing hears that first, not a "try later" that
// would send it round again for nothing.
func TestUnauthorizedBeatsReconcilingGate(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	svc.SetReconciling(true)
	table := NewPrincipalTable([]Principal{{Name: "reader", Token: "tok-read", Grants: []Grant{GrantRead}}})
	srv := httptest.NewServer(NewMux(svc, Config{Table: table}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/machines/test-machine/sessions/s1/input", strings.NewReader(`{"text":"x"}`))
	req.Header.Set("Authorization", "Bearer tok-read")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a principal without the send grant got %d while reconciling, want 401", resp.StatusCode)
	}

	// The legacy single-token path: no grant to hold, but the host's own
	// switch comes first.
	svc2 := New("test-machine")
	if err := svc2.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	svc2.SetReconciling(true)
	srv2 := httptest.NewServer(NewMux(svc2, Config{Token: testToken})) // mutations not allowed
	t.Cleanup(srv2.Close)
	if resp := callAs(t, srv2, "POST", "/v1/machines/test-machine/sessions/s1/input", `{"text":"x"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a host that refuses mutations got %d while reconciling, want 401", resp.StatusCode)
	}
}

// A call aimed at a peer changes nothing here; this machine's own window does
// not hold it.
func TestRelayedMutationNotGatedByLocalReconcile(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("otherbox", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	svc.SetReconciling(true)
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true, AllowPeerRelay: true}))
	t.Cleanup(srv.Close)
	resp := callAs(t, srv, "POST", "/v1/machines/otherbox/sessions/s1/input", `{"text":"x"}`)
	if resp.StatusCode == http.StatusGatewayTimeout {
		if e := decodeError(t, resp).Error; e.Reason == fleet.ReasonReconciling {
			t.Errorf("a relayed call was held by this machine's own reconcile: %+v", e)
		}
	}
}

func TestReconcilingReportsSince(t *testing.T) {
	svc := New("test-machine")
	if on, _ := svc.Reconciling(); on {
		t.Fatal("a fresh service reports a reconcile pending")
	}
	svc.SetReconciling(true)
	if on, since := svc.Reconciling(); !on || since.IsZero() {
		t.Fatalf("Reconciling() = %v, %v", on, since)
	}
	svc.SetReconciling(false)
	if on, _ := svc.Reconciling(); on {
		t.Fatal("still pending after SetReconciling(false)")
	}
}
