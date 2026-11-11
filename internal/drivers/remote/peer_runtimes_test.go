package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #459: a peer that runs more than one runtime is learned as a table, one
// entry per available runtime it lists for itself on the one /v1/runtimes read,
// while Capabilities, Models and Runtime keep answering for the first of them.

func peerRuntimesBody(items ...fleet.RuntimeInfo) map[string]any {
	return map[string]any{
		"items":    items,
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
}

func availableRuntime(rt fleet.RuntimeId, caps fleet.DriverCapabilities, models *fleet.RuntimeModels) fleet.RuntimeInfo {
	caps.Source = fleet.CapabilitiesObserved
	return fleet.RuntimeInfo{Machine: "peerbox", Runtime: rt, Status: fleet.RuntimeAvailable, Capabilities: caps, Models: models}
}

// twoRuntimes is a peer listing opencode ahead of claude-code-tmux, so the order
// the peer used and the runtime-id order differ.
func twoRuntimes() map[string]any {
	readAt := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	def := "deepseek/deepseek-flash"
	return peerRuntimesBody(
		availableRuntime("opencode", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true},
			&fleet.RuntimeModels{Source: fleet.ModelsFromRuntime, ReadAt: &readAt,
				IDs: []string{"deepseek/deepseek-flash", "deepseek/deepseek-v4-pro"}, Default: &def}),
		availableRuntime("claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000, DeliversRawKeys: true},
			&fleet.RuntimeModels{Source: fleet.ModelsDeclared, IDs: []string{"opus", "sonnet"}}),
	)
}

func TestPeerRuntimesAPeerWithTwoRuntimesYieldsTwoEntriesEachWithItsOwn(t *testing.T) {
	d := New("peerbox", peerServing(t, 200, twoRuntimes(), nil).URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	rows := d.PeerRuntimes()
	if len(rows) != 2 {
		t.Fatalf("PeerRuntimes = %+v, want two entries", rows)
	}
	cc, oc := rows[0], rows[1]
	if cc.Runtime != "claude-code-tmux" || oc.Runtime != "opencode" {
		t.Fatalf("runtimes = %q, %q; want runtime-id order, not the order the peer used", cc.Runtime, oc.Runtime)
	}
	if !cc.Capabilities.DeliversRawKeys || cc.Capabilities.SupportsResume {
		t.Errorf("claude-code-tmux capabilities = %+v, want its own", cc.Capabilities)
	}
	if !oc.Capabilities.SupportsResume || oc.Capabilities.DeliversRawKeys {
		t.Errorf("opencode capabilities = %+v, want its own", oc.Capabilities)
	}
	for _, r := range rows {
		if r.Capabilities.Source != fleet.CapabilitiesObserved || r.Capabilities.ObservedAt == nil {
			t.Errorf("%s: capabilities %+v, want observed with a local read time", r.Runtime, r.Capabilities)
		}
	}
	if cc.Models.Source != fleet.ModelsDeclared || len(cc.Models.IDs) != 2 || cc.Models.IDs[0] != "opus" {
		t.Errorf("claude-code-tmux models = %+v, want its own two ids", cc.Models)
	}
	if oc.Models.Source != fleet.ModelsFromRuntime || len(oc.Models.IDs) != 2 || oc.Models.IDs[1] != "deepseek/deepseek-v4-pro" {
		t.Errorf("opencode models = %+v, want its own two ids", oc.Models)
	}
	if oc.Models.ReadAt == nil || !oc.Models.ReadAt.Equal(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("opencode readAt = %v, want the peer's passed through", oc.Models.ReadAt)
	}

	// The single-value accessors answer for the first available row in the peer's
	// own order, as they did before there was a table.
	if got := d.Runtime(); got != "opencode" {
		t.Errorf("Runtime() = %q, want the first row the peer listed", got)
	}
	if c := d.Capabilities(); !c.SupportsResume || c.DeliversRawKeys {
		t.Errorf("Capabilities() = %+v, want the first row's", c)
	}
	if m := d.Models(); m.Source != fleet.ModelsFromRuntime || len(m.IDs) != 2 {
		t.Errorf("Models() = %+v, want the first row's", m)
	}

	// A caller that edits what it was handed must not reach the cache.
	oc.Models.IDs[0] = "tampered"
	if again := d.PeerRuntimes(); again[1].Models.IDs[0] != "deepseek/deepseek-flash" {
		t.Errorf("the cache was reached through a returned slice: %v", again[1].Models.IDs)
	}
}

func TestPeerRuntimesAPeerWithOneRuntimeYieldsTheSingleValueAsOneEntry(t *testing.T) {
	def := "opus"
	body := peerRuntimesBody(availableRuntime("claude-code-tmux",
		fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true},
		&fleet.RuntimeModels{Source: fleet.ModelsDeclared, IDs: []string{"opus"}, Default: &def}))
	d := New("peerbox", peerServing(t, 200, body, nil).URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	rows := d.PeerRuntimes()
	if len(rows) != 1 || rows[0].Runtime != d.Runtime() {
		t.Fatalf("PeerRuntimes = %+v, want the one runtime %q", rows, d.Runtime())
	}
	if got, want := rows[0].Capabilities, d.Capabilities(); got.SupportsResume != want.SupportsResume ||
		got.Source != want.Source || got.DeadlineMs != want.DeadlineMs {
		t.Errorf("capabilities = %+v, want what Capabilities() answers: %+v", got, want)
	}
	if got, want := rows[0].Models, d.Models(); got.Source != want.Source || len(got.IDs) != len(want.IDs) {
		t.Errorf("models = %+v, want what Models() answers: %+v", got, want)
	}
}

// An unavailable runtime stays out of the table, whether it comes first or not,
// and the first AVAILABLE row is the one the single-value accessors answer for.
func TestPeerRuntimesLeavesOutAnUnavailableRuntimeEvenAheadOfTheOthers(t *testing.T) {
	body := peerRuntimesBody(
		fleet.RuntimeInfo{
			Machine: "peerbox", Runtime: "aaa-late", Status: fleet.RuntimeUnavailable,
			Cause: fleet.CauseProbeTimeout, Reason: "did not answer in time",
			Capabilities: fleet.DriverCapabilities{DeadlineMs: 30000, Source: fleet.CapabilitiesAssumed},
		},
		availableRuntime("opencode", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true}, nil),
		availableRuntime("claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000, DeliversRawKeys: true}, nil),
	)
	d := New("peerbox", peerServing(t, 200, body, nil).URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	rows := d.PeerRuntimes()
	if len(rows) != 2 || rows[0].Runtime != "claude-code-tmux" || rows[1].Runtime != "opencode" {
		t.Fatalf("PeerRuntimes = %+v, want the two available runtimes and not aaa-late", rows)
	}
	if got := d.Runtime(); got != "opencode" {
		t.Errorf("Runtime() = %q, want the first available row, not the unavailable one", got)
	}
	// A runtime without a models block reads as none, with an empty list.
	if m := rows[0].Models; m.Source != fleet.ModelsNone || m.IDs == nil || len(m.IDs) != 0 {
		t.Errorf("a row with no models: %+v, want none with an empty (not null) list", m)
	}
}

// A row for some other machine (a peer reports its own peers too) is not this
// peer's runtime, and a runtime listed twice is one entry.
func TestPeerRuntimesTakesOnlyThePeersOwnRowsAndEachRuntimeOnce(t *testing.T) {
	elsewhere := availableRuntime("opencode", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true}, nil)
	elsewhere.Machine = "thirdbox"
	body := peerRuntimesBody(
		elsewhere,
		availableRuntime("claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000, DeliversRawKeys: true}, nil),
		availableRuntime("claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000, SupportsResume: true}, nil),
	)
	d := New("peerbox", peerServing(t, 200, body, nil).URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	rows := d.PeerRuntimes()
	if len(rows) != 1 || rows[0].Runtime != "claude-code-tmux" || !rows[0].Capabilities.DeliversRawKeys {
		t.Fatalf("PeerRuntimes = %+v, want the first claude-code-tmux row only", rows)
	}
}

// All of a peer's runtimes are one cache, aged by the one observation time: past
// capabilityStaleness every entry degrades to the assumed floor with no models,
// not only the first. The runtimes stay listed — a stale answer is still an answer
// about which runtimes exist.
func TestPeerRuntimesAStaleCacheDegradesEveryEntry(t *testing.T) {
	now := time.Now()
	d := New("peerbox", peerServing(t, 200, twoRuntimes(), nil).URL, withClock(func() time.Time { return now }))
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	for _, r := range d.PeerRuntimes() {
		if r.Capabilities.Source != fleet.CapabilitiesObserved {
			t.Fatalf("%s before staleness: %+v, want observed", r.Runtime, r.Capabilities)
		}
	}

	now = now.Add(capabilityStaleness + time.Second)
	rows := d.PeerRuntimes()
	if len(rows) != 2 {
		t.Fatalf("PeerRuntimes past staleness = %+v, want both runtimes still listed", rows)
	}
	for _, r := range rows {
		if r.Capabilities.Source != fleet.CapabilitiesAssumed || r.Capabilities.SupportsResume || r.Capabilities.DeliversRawKeys {
			t.Errorf("%s: capabilities %+v, want the assumed floor", r.Runtime, r.Capabilities)
		}
		if r.Models.Source != fleet.ModelsNone || len(r.Models.IDs) != 0 {
			t.Errorf("%s: models %+v, want none — a list the peer may no longer stand behind", r.Runtime, r.Models)
		}
	}
}

// A peer that has not answered has no table: nothing is listed under a runtime id
// nobody learned.
func TestPeerRuntimesAnUnreachedPeerHasNoEntries(t *testing.T) {
	d := New("peerbox", "http://127.0.0.1:1")
	if rows := d.PeerRuntimes(); len(rows) != 0 {
		t.Fatalf("before any answer: %+v, want none", rows)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening there any more
	d = New("peerbox", url)
	if err := d.RefreshCapabilities(context.Background(), caller); err == nil {
		t.Fatal("RefreshCapabilities against a peer that is not there returned no error")
	}
	if rows := d.PeerRuntimes(); len(rows) != 0 {
		t.Fatalf("after a refresh that reached nothing: %+v, want none", rows)
	}
	if d.CapabilitiesKnown() {
		t.Error("capabilities read as known for a peer that never answered")
	}
}

// The table is replaced whole by each answer: a runtime the peer stops listing
// leaves it, and one it starts listing joins.
func TestPeerRuntimesEachRefreshReplacesTheTable(t *testing.T) {
	var mu sync.Mutex
	body := twoRuntimes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	d := New("peerbox", srv.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if n := len(d.PeerRuntimes()); n != 2 {
		t.Fatalf("first answer: %d entries, want 2", n)
	}

	mu.Lock()
	body = peerRuntimesBody(availableRuntime("claude-code-tmux", fleet.DriverCapabilities{DeadlineMs: 5000}, nil))
	mu.Unlock()
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	rows := d.PeerRuntimes()
	if len(rows) != 1 || rows[0].Runtime != "claude-code-tmux" {
		t.Fatalf("after the peer dropped opencode: %+v, want claude-code-tmux alone", rows)
	}
	if d.Runtime() != "claude-code-tmux" {
		t.Errorf("Runtime() = %q, want the single value to follow", d.Runtime())
	}
}
