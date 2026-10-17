package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// A create's `settings` across a federation hop (muster #247): the same
// mixed-version rule conversationId follows — a peer that predates the field
// must never silently drop it.

func launchSettingsPeer(t *testing.T, supports bool, outsideBypass ...string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var creates atomic.Int32
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			h := map[string]any{"build": fleet.Build{}, "maxInputBytes": 1024}
			if supports {
				h["supportsLaunchSettings"] = true
			}
			if outsideBypass != nil {
				h["launchSettingsOutsideBypass"] = outsideBypass
			}
			_ = json.NewEncoder(w).Encode(h)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sessions"):
			creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			last.Store(string(raw))
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
	return srv, &creates, &last
}

func TestCreateForwardsSettingsToAPeerThatCarriesThem(t *testing.T) {
	srv, creates, last := launchSettingsPeer(t, true)
	d := New("peerbox", srv.URL)
	if _, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", PermissionMode: fleet.PermissionModeBypass,
		Settings: json.RawMessage(`{"crossSessionInbound":"accept"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", creates.Load())
	}
	body, _ := last.Load().(string)
	if !strings.Contains(body, `"settings":{"crossSessionInbound":"accept"}`) {
		t.Errorf("create body %s does not carry the settings", body)
	}
}

func TestCreateRefusesASettingsCreateToAPeerThatWouldDropIt(t *testing.T) {
	srv, creates, _ := launchSettingsPeer(t, false)
	d := New("peerbox", srv.URL)
	_, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", PermissionMode: fleet.PermissionModeBypass,
		Settings: json.RawMessage(`{"crossSessionInbound":"accept"}`),
	})
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if creates.Load() != 0 {
		t.Fatalf("the refused create reached the peer %d time(s)", creates.Load())
	}
	// A create with no settings is unaffected.
	if _, err := d.Create(context.Background(), caller, "k2", fleet.SessionSpec{Cwd: "/work"}); err != nil {
		t.Fatalf("plain create: %v", err)
	}
	if creates.Load() != 1 {
		t.Fatal("plain create did not reach the peer")
	}
}

// muster #254: a non-bypass create reaches a peer only if that peer advertises
// the keys outside bypass. A #247-only peer says supportsLaunchSettings:true and
// would refuse (or an older one drop) the field, so the refusal is made here.
func TestNonBypassSettingsNeedAPeerThatListsTheKey(t *testing.T) {
	spec := fleet.SessionSpec{Cwd: "/work", Settings: json.RawMessage(`{"crossSessionInbound":"accept"}`)}

	old, oldCreates, _ := launchSettingsPeer(t, true) // #247 only: no list
	_, err := New("peerbox", old.URL).Create(context.Background(), caller, "k1", spec)
	if kindOf(err) != fleet.ErrorUnsupported || !strings.Contains(err.Error(), "bypass") {
		t.Fatalf("err = %v, want unsupported naming the bypass-only peer", err)
	}
	if oldCreates.Load() != 0 {
		t.Fatalf("the refused create reached the peer %d time(s)", oldCreates.Load())
	}

	other, otherCreates, _ := launchSettingsPeer(t, true, "somethingElse")
	if _, err := New("peerbox", other.URL).Create(context.Background(), caller, "k2", spec); kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want unsupported when the peer does not list the key", err)
	}
	if otherCreates.Load() != 0 {
		t.Fatal("the refused create reached the peer")
	}

	ok, okCreates, last := launchSettingsPeer(t, true, "crossSessionInbound")
	if _, err := New("peerbox", ok.URL).Create(context.Background(), caller, "k3", spec); err != nil {
		t.Fatal(err)
	}
	if okCreates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", okCreates.Load())
	}
	if body, _ := last.Load().(string); !strings.Contains(body, `"settings":{"crossSessionInbound":"accept"}`) {
		t.Errorf("create body %s does not carry the settings", body)
	}

	// A bypass create is unchanged by the list: #247's flag alone suffices.
	bp, _, _ := launchSettingsPeer(t, true)
	if _, err := New("peerbox", bp.URL).Create(context.Background(), caller, "k4", fleet.SessionSpec{
		Cwd: "/work", PermissionMode: fleet.PermissionModeBypass, Settings: spec.Settings,
	}); err != nil {
		t.Fatalf("bypass create against a #247 peer: %v", err)
	}
}

// muster #329: a non-bypass permission mode is forwarded verbatim. The owning
// peer is the authority on what it accepts (an older one refuses a mode it does
// not know, loudly), so the relaying side neither rewrites nor drops it.
func TestCreateForwardsAPermissionModeVerbatim(t *testing.T) {
	for _, mode := range []string{"default", "acceptEdits", "plan", "auto"} {
		srv, creates, last := launchSettingsPeer(t, true)
		d := New("peerbox", srv.URL)
		if _, err := d.Create(context.Background(), caller, "k-"+mode, fleet.SessionSpec{Cwd: "/work", PermissionMode: mode}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		body, _ := last.Load().(string)
		if creates.Load() != 1 || !strings.Contains(body, `"permissionMode":"`+mode+`"`) {
			t.Errorf("%s: creates=%d body %s", mode, creates.Load(), body)
		}
	}
}
