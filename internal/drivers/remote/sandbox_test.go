package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// A create's sandbox across a federation hop (muster #281). The
// rule under test is the MIXED-VERSION case, the mirror of conversationid_test:
// a peer that predates the field must never be handed a create that REQUIRES
// isolation, because it would answer 201 and start the session with its whole
// environment.

func sandboxPeer(t *testing.T, supports bool) (*httptest.Server, *atomic.Int32, *string, *sync.Mutex) {
	t.Helper()
	var creates atomic.Int32
	var mu sync.Mutex
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			h := map[string]any{"build": fleet.Build{}, "maxInputBytes": 1024}
			if supports {
				tr := true
				h["supportsSandbox"] = &tr
			}
			_ = json.NewEncoder(w).Encode(h)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sessions"):
			creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			lastBody = string(raw)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fleet.Session{
				SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
				State:      fleet.ObservedState(fleet.StatusIdle, "fixture", nil),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &creates, &lastBody, &mu
}

func TestCreateForwardsSandboxToAPeerThatCarriesIt(t *testing.T) {
	srv, creates, lastBody, mu := sandboxPeer(t, true)
	d := New("peerbox", srv.URL)
	if _, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{Cwd: "/work", Sandbox: &fleet.SandboxSpec{}}); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", creates.Load())
	}
	mu.Lock()
	body := *lastBody
	mu.Unlock()
	if !strings.Contains(body, `"sandbox":{`) {
		t.Errorf("create body %s does not carry sandbox", body)
	}
}

func TestCreateRefusesASandboxedCreateToAPeerThatWouldDropIt(t *testing.T) {
	srv, creates, _, _ := sandboxPeer(t, false)
	d := New("peerbox", srv.URL)

	_, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{Cwd: "/work", Sandbox: &fleet.SandboxSpec{}})
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if creates.Load() != 0 {
		t.Fatalf("the refused create reached the peer %d time(s)", creates.Load())
	}
	// A create that asks for no sandbox is unaffected.
	if _, err := d.Create(context.Background(), caller, "k2", fleet.SessionSpec{Cwd: "/work"}); err != nil {
		t.Fatalf("plain create: %v", err)
	}
	if creates.Load() != 1 {
		t.Fatalf("plain create did not reach the peer")
	}
}
