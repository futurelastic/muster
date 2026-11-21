package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// A create's agentSpec across a federation hop (muster #509). The rule under test
// is the mixed-version one, the mirror of sandbox_test: a peer runtime that has
// not declared it translates a definition must never be handed one, because it
// would answer 201 and start a session that follows none of it.

// specPeer serves /v1/runtimes with one row per entry of rows (runtime id →
// whether it declares supportsAgentSpec; absent from the map = an older peer that
// never heard of the field) and records the create bodies it is sent.
func specPeer(t *testing.T, rows map[fleet.RuntimeId]*bool, order ...fleet.RuntimeId) (*httptest.Server, *atomic.Int32, *string, *sync.Mutex) {
	t.Helper()
	var creates atomic.Int32
	var mu sync.Mutex
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/runtimes":
			items := make([]fleet.RuntimeInfo, 0, len(order))
			for _, id := range order {
				items = append(items, availableRuntime(id, fleet.DriverCapabilities{DeadlineMs: 5000, SupportsAgentSpec: rows[id]}, nil))
			}
			_ = json.NewEncoder(w).Encode(peerRuntimesBody(items...))
		case r.URL.Path == "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"build": fleet.Build{}, "maxInputBytes": 1024})
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

func tru() *bool { return fleet.Declared(true) }

var defSpec = &fleet.AgentSpec{Name: "researcher", Prompt: "be brief", Access: fleet.AgentAccessSandboxed,
	Knowledge: &fleet.AgentKnowledge{Index: "/m/MEMORY.md", Home: "/m"}}

func TestCreateForwardsAnAgentSpecToAPeerRuntimeThatTranslatesIt(t *testing.T) {
	srv, creates, lastBody, mu := specPeer(t, map[fleet.RuntimeId]*bool{"opencode": tru()}, "opencode")
	d := New("peerbox", srv.URL)
	if _, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{Runtime: "opencode", Cwd: "/work", AgentSpec: defSpec}); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", creates.Load())
	}
	mu.Lock()
	body := *lastBody
	mu.Unlock()
	var got struct {
		AgentSpec *fleet.AgentSpec `json:"agentSpec"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.AgentSpec == nil {
		t.Fatalf("create body %s does not carry agentSpec (%v)", body, err)
	}
	if got.AgentSpec.Name != "researcher" || got.AgentSpec.Prompt != "be brief" || got.AgentSpec.Access != fleet.AgentAccessSandboxed ||
		got.AgentSpec.Knowledge == nil || got.AgentSpec.Knowledge.Home != "/m" {
		t.Errorf("the definition changed in transit: %+v", got.AgentSpec)
	}
}

func TestCreateRefusesAnAgentSpecToAPeerThatWouldDropIt(t *testing.T) {
	cases := []struct {
		name string
		rows map[fleet.RuntimeId]*bool
	}{
		{"an older peer that never heard of the field", map[fleet.RuntimeId]*bool{"opencode": nil}},
		{"a runtime that declares it cannot", map[fleet.RuntimeId]*bool{"opencode": fleet.Declared(false)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, creates, _, _ := specPeer(t, c.rows, "opencode")
			d := New("peerbox", srv.URL)
			_, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{Runtime: "opencode", Cwd: "/work", AgentSpec: defSpec})
			var fe *fleet.Error
			if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || fe.Reason != fleet.ReasonAgentSpecUnsupported {
				t.Fatalf("err = %v, want unsupported with reason agent-spec-unsupported", err)
			}
			if creates.Load() != 0 {
				t.Fatalf("the refused create reached the peer %d time(s)", creates.Load())
			}
			// A create that carries no definition is unaffected.
			if _, err := d.Create(context.Background(), caller, "k2", fleet.SessionSpec{Runtime: "opencode", Cwd: "/work"}); err != nil {
				t.Fatalf("plain create: %v", err)
			}
		})
	}
}

func TestCreateJudgesTheRuntimeTheCreateNames(t *testing.T) {
	srv, creates, _, _ := specPeer(t, map[fleet.RuntimeId]*bool{"opencode": tru(), "claude-code-tmux": nil}, "opencode", "claude-code-tmux")
	d := New("peerbox", srv.URL)
	if _, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{Runtime: "claude-code-tmux", Cwd: "/work", AgentSpec: defSpec}); kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("a runtime that did not declare it was sent a definition: %v", err)
	}
	if creates.Load() != 0 {
		t.Fatal("the refused create reached the peer")
	}
	if _, err := d.Create(context.Background(), caller, "k2", fleet.SessionSpec{Runtime: "opencode", Cwd: "/work", AgentSpec: defSpec}); err != nil {
		t.Fatalf("the runtime that declares it was refused: %v", err)
	}
	// A create that names no runtime goes to the peer's default: the first row it listed.
	if _, err := d.Create(context.Background(), caller, "k3", fleet.SessionSpec{Cwd: "/work", AgentSpec: defSpec}); err != nil {
		t.Fatalf("the default runtime declares it and was refused: %v", err)
	}
}

func TestCreateAsksAgainBeforeRefusingOnStaleEvidence(t *testing.T) {
	// The peer is upgraded between the first read and the create: the cached row
	// says no, the fresh one says yes.
	var declared atomic.Bool
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/runtimes":
			var flag *bool
			if declared.Load() {
				flag = tru()
			}
			_ = json.NewEncoder(w).Encode(peerRuntimesBody(availableRuntime("opencode", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsAgentSpec: flag}, nil)))
		case r.URL.Path == "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"build": fleet.Build{}, "maxInputBytes": 1024})
		case r.Method == http.MethodPost:
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fleet.Session{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
				State: fleet.ObservedState(fleet.StatusIdle, "fixture", nil)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	d := New("peerbox", srv.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	declared.Store(true)
	if _, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{Runtime: "opencode", Cwd: "/work", AgentSpec: defSpec}); err != nil {
		t.Fatalf("an upgraded peer was refused on stale evidence: %v", err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d", creates.Load())
	}
}

func TestCreateRefusesWhenThePeerCannotBeAskedWhetherItTranslates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusBadGateway) }))
	t.Cleanup(srv.Close)
	d := New("peerbox", srv.URL)
	if _, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{Runtime: "opencode", Cwd: "/work", AgentSpec: defSpec}); err == nil {
		t.Fatal("a definition was sent to a peer that could not confirm it translates one")
	}
}
