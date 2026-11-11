package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/remote"
)

// muster #460: a create relayed to a peer that runs several runtimes is lowered
// against the capabilities of the runtime the body names, not against the one
// row the peer driver adopted as its own (#403) — the body already carries
// `runtime` on, so the peer starts the session on the runtime named; only the
// check made here, before the relay, was looking at the wrong one.

// recordingPeer is a peer that lists the runtimes it is given on
// GET /v1/runtimes, says it carries the create fields a lowered posture uses, and
// records every create it is sent. Its row ORDER is runtime-id order (the order a
// peer driver adopts its first row from), so the runtime listed first is the one
// the driver's own Capabilities() describe.
type recordingPeer struct {
	srv *httptest.Server

	mu      sync.Mutex
	creates []map[string]any
}

func newRecordingPeer(t *testing.T, rows ...fleet.RuntimeInfo) *recordingPeer {
	t.Helper()
	p := &recordingPeer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runtimes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":    rows,
				"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
				"complete": true,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			// The peer carries every create field a lowered posture can use, so
			// the relay's own per-machine checks (#281, #280, #392) let it through
			// and what is under test is only the check made before them.
			_, _ = w.Write([]byte(`{"supportsSandbox":true,"supportsIsolateEnvironment":true,"supportsIsolateConfiguration":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/peerbox/sessions":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			p.mu.Lock()
			p.creates = append(p.creates, body)
			p.mu.Unlock()
			rt, _ := body["runtime"].(string)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"machine": "peerbox", "id": "peer-s-1", "runtime": rt,
				"state": map[string]any{"status": string(fleet.StatusIdle)},
			})
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *recordingPeer) created() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.creates...)
}

// confiningCaps is what a runtime that can confine and run unattended declares.
func confiningCaps() fleet.DriverCapabilities {
	c := fleet.DriverCapabilities{DeadlineMs: 5000}
	c.IsolatesEnvironment = true
	c.IsolatesConfiguration = true
	c.CreatePermissionModes = []string{string(fleet.PermissionModeDefault), fleet.PermissionModeBypass}
	c.Sandbox = &fleet.SandboxSupport{
		Mechanism: "test",
		Denies:    []fleet.SandboxDeny{fleet.SandboxDenyFiles, fleet.SandboxDenyUnixSockets, fleet.SandboxDenySystemServices},
		Network:   []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
	}
	return c
}

// weakCaps is a runtime that can enforce no posture beyond an attended one.
func weakCaps() fleet.DriverCapabilities {
	return fleet.DriverCapabilities{
		DeadlineMs:            5000,
		CreatePermissionModes: []string{string(fleet.PermissionModeDefault)},
	}
}

// relayCreateServer stands up an entry machine whose only way to peerbox is a real
// remote driver that has read the peer's table, and returns the entry's server.
func relayCreateServer(t *testing.T, peer *recordingPeer) *httptest.Server {
	t.Helper()
	svc := New("testbox")
	d := remote.New("peerbox", peer.srv.URL, remote.WithIdentity("peer-token"))
	if err := svc.RegisterPeerDriver("peerbox", d); err != nil {
		t.Fatal(err)
	}
	if err := d.RefreshCapabilities(t.Context(), peerCaller); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true, AllowPeerRelay: true}))
	t.Cleanup(srv.Close)
	return srv
}

func relayCreate(t *testing.T, srv *httptest.Server, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/peerbox/sessions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

const unattendedPosture = `{"attendance":"unattended","files":{"writePaths":["/srv/git"]},"network":"closed","isolatedEnvironment":true}`

// The measured case of #403: the peer driver adopts the first row of its table
// (runtime-id order), so a create naming the SECOND runtime used to be lowered
// against the first one's capabilities. Here the first runtime cannot enforce an
// unattended posture and the second can: naming the second must go through, with
// the fields lowered, and must have gone to the peer under the runtime it named.
func TestRelayedCreatePosture_NamingTheSecondRuntimeUsesItsCapabilities(t *testing.T) {
	peer := newRecordingPeer(t,
		peerRuntimeRow("peerbox", "aaa-plain", weakCaps(), nil),
		peerRuntimeRow("peerbox", "zzz-confined", confiningCaps(), nil),
	)
	srv := relayCreateServer(t, peer)

	code, raw := relayCreate(t, srv, "rc-1",
		`{"runtime":"zzz-confined","cwd":"/w","posture":`+unattendedPosture+`}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (the named runtime can enforce it): %s", code, raw)
	}
	got := peer.created()
	if len(got) != 1 {
		t.Fatalf("the peer was sent %d creates, want 1", len(got))
	}
	if got[0]["runtime"] != "zzz-confined" || got[0]["permissionMode"] != fleet.PermissionModeBypass || got[0]["isolateEnvironment"] != true {
		t.Errorf("create sent to the peer = %+v, want runtime zzz-confined with the posture lowered onto it", got[0])
	}
	if got[0]["sandbox"] == nil {
		t.Errorf("create sent to the peer carries no sandbox: %+v", got[0])
	}
	if _, kept := got[0]["posture"]; kept {
		t.Errorf("the posture itself went to the peer beside what it lowers to: %+v", got[0])
	}
}

// The other direction, same peer: naming the runtime that cannot enforce the
// posture is refused here with the error a local runtime gives, and the peer is
// never sent the create. This is the case that, before, depended on which row the
// driver happened to have adopted.
func TestRelayedCreatePosture_NamingARuntimeThatCannotEnforceItIsRefused(t *testing.T) {
	peer := newRecordingPeer(t,
		peerRuntimeRow("peerbox", "aaa-confined", confiningCaps(), nil),
		peerRuntimeRow("peerbox", "zzz-plain", weakCaps(), nil),
	)
	srv := relayCreateServer(t, peer)

	// The adopted first row CAN enforce this; the runtime it names cannot.
	code, raw := relayCreate(t, srv, "rc-2",
		`{"runtime":"zzz-plain","cwd":"/w","posture":`+unattendedPosture+`}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", code, raw)
	}
	e := postureErr(t, raw)
	if e.Kind != fleet.ErrorUnsupported || e.Reason != fleet.ReasonPostureUnenforceable || e.Machine != "peerbox" {
		t.Errorf("error = %+v, want the posture-unenforceable refusal a local runtime gives, on peerbox", e)
	}
	if got := peer.created(); len(got) != 0 {
		t.Errorf("a refused create reached the peer: %+v", got)
	}
}

// Both runtimes of a two-runtime peer, each way round: the answer follows the
// runtime named whichever of the two the driver adopted.
func TestRelayedCreatePosture_EachRuntimeOfATwoRuntimePeerAnswersForItself(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstCaps  fleet.DriverCapabilities // the adopted row
		secondCaps fleet.DriverCapabilities
		named      fleet.RuntimeId
		wantCode   int
	}{
		{"first can, second cannot: first named", confiningCaps(), weakCaps(), "aaa-rt", http.StatusCreated},
		{"first can, second cannot: second named", confiningCaps(), weakCaps(), "zzz-rt", http.StatusNotImplemented},
		{"first cannot, second can: first named", weakCaps(), confiningCaps(), "aaa-rt", http.StatusNotImplemented},
		{"first cannot, second can: second named", weakCaps(), confiningCaps(), "zzz-rt", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newRecordingPeer(t,
				peerRuntimeRow("peerbox", "aaa-rt", tc.firstCaps, nil),
				peerRuntimeRow("peerbox", "zzz-rt", tc.secondCaps, nil),
			)
			srv := relayCreateServer(t, peer)
			code, raw := relayCreate(t, srv, "rc-3",
				`{"runtime":"`+string(tc.named)+`","cwd":"/w","posture":`+unattendedPosture+`}`)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", code, tc.wantCode, raw)
			}
			wantSent := 0
			if tc.wantCode == http.StatusCreated {
				wantSent = 1
			}
			if got := peer.created(); len(got) != wantSent {
				t.Errorf("the peer was sent %d creates, want %d", len(got), wantSent)
			}
		})
	}
}

// A create that names no runtime behaves as it did: lowered against the peer
// driver's own answer, the first row of its table.
func TestRelayedCreatePosture_NamingNoRuntimeKeepsTheDriversOwnAnswer(t *testing.T) {
	// First row cannot enforce: refused, as before.
	weakFirst := newRecordingPeer(t,
		peerRuntimeRow("peerbox", "aaa-plain", weakCaps(), nil),
		peerRuntimeRow("peerbox", "zzz-confined", confiningCaps(), nil),
	)
	code, raw := relayCreate(t, relayCreateServer(t, weakFirst), "rc-4a", `{"cwd":"/w","posture":`+unattendedPosture+`}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("no runtime named, first row cannot enforce: status = %d, want 501: %s", code, raw)
	}
	if got := weakFirst.created(); len(got) != 0 {
		t.Errorf("a refused create reached the peer: %+v", got)
	}

	// First row can: forwarded, and no runtime is invented in the body.
	strongFirst := newRecordingPeer(t,
		peerRuntimeRow("peerbox", "aaa-confined", confiningCaps(), nil),
		peerRuntimeRow("peerbox", "zzz-plain", weakCaps(), nil),
	)
	code, raw = relayCreate(t, relayCreateServer(t, strongFirst), "rc-4b", `{"cwd":"/w","posture":`+unattendedPosture+`}`)
	if code != http.StatusCreated {
		t.Fatalf("no runtime named, first row can enforce: status = %d, want 201: %s", code, raw)
	}
	got := strongFirst.created()
	if len(got) != 1 {
		t.Fatalf("the peer was sent %d creates, want 1", len(got))
	}
	if rt, _ := got[0]["runtime"].(string); rt != "" {
		t.Errorf("runtime in the create sent to the peer = %q, want none", rt)
	}
}

// A runtime the peer's table does not list is not refused here: like a peer
// nobody has reached it is "unknown", never "incapable", so the lowered fields go
// on and the peer — which owns the question of what it runs — answers.
func TestRelayedCreatePosture_ARuntimeTheTableDoesNotListIsLeftToThePeer(t *testing.T) {
	peer := newRecordingPeer(t,
		peerRuntimeRow("peerbox", "aaa-plain", weakCaps(), nil),
	)
	srv := relayCreateServer(t, peer)

	code, raw := relayCreate(t, srv, "rc-5",
		`{"runtime":"not-listed","cwd":"/w","posture":`+unattendedPosture+`}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (the peer decides): %s", code, raw)
	}
	got := peer.created()
	if len(got) != 1 || got[0]["runtime"] != "not-listed" || got[0]["permissionMode"] != fleet.PermissionModeBypass {
		t.Errorf("create sent to the peer = %+v, want runtime not-listed with the posture lowered onto it", got)
	}
}

// peerRuntimeCapabilities on its own, for the two drivers the service can meet: one
// that keeps a table, and one that does not (only Capabilities() and Runtime()).
func TestPeerRuntimeCapabilities(t *testing.T) {
	strong, weak := confiningCaps(), weakCaps()
	strong.Source, weak.Source = fleet.CapabilitiesObserved, fleet.CapabilitiesObserved

	table := &peerTableDriver{
		peerCapsDriver: peerCapsDriver{caps: weak, runtime: "aaa-rt"},
		rows: []driver.PeerRuntime{
			{Runtime: "aaa-rt", Capabilities: weak},
			{Runtime: "zzz-rt", Capabilities: strong},
		},
	}
	if got := peerRuntimeCapabilities(table, "zzz-rt"); got.Sandbox == nil || got.Source != fleet.CapabilitiesObserved {
		t.Errorf("zzz-rt = %+v, want its own row", got)
	}
	if got := peerRuntimeCapabilities(table, "aaa-rt"); got.Sandbox != nil {
		t.Errorf("aaa-rt = %+v, want its own row", got)
	}
	if got := peerRuntimeCapabilities(table, "nope"); got.Source != fleet.CapabilitiesAssumed || got.Sandbox != nil {
		t.Errorf("a runtime the table does not list = %+v, want the assumed floor", got)
	}

	// A driver with no table answers as the one runtime it has: its own id, its own
	// capabilities — and anything else is unknown.
	single := &peerCapsDriver{caps: weak, runtime: "aaa-rt"}
	if got := peerRuntimeCapabilities(single, "aaa-rt"); got.Source != fleet.CapabilitiesObserved {
		t.Errorf("single-runtime driver, its own runtime = %+v, want its capabilities", got)
	}
	if got := peerRuntimeCapabilities(single, "zzz-rt"); got.Source != fleet.CapabilitiesAssumed {
		t.Errorf("single-runtime driver, another runtime = %+v, want the assumed floor", got)
	}
}
