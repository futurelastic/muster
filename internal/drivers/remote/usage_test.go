package remote

import (
	"context"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// state.usage crosses a peer relay untouched (muster #285): the machine that
// runs the session is the one that read its runtime's figures, so what arrives
// is what the far end summed, and this driver neither drops nor re-derives it.
func TestUsageSurvivesARelay(t *testing.T) {
	reasoning, cost := int64(7), 0.42
	st := fleet.InferredState(fleet.StatusIdle, "idle", nil)
	asOf := time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	st.Usage = fleet.NewUsage(
		fleet.UsageFigures{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Reasoning: &reasoning, Cost: &cost},
		fleet.ConfidenceObserved, asOf,
		fleet.NewTurnUsage(fleet.UsageFigures{Input: 1, Output: 1}, asOf))
	srv := peerServing(t, 200, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
		Runtime:    "claude-code-tmux",
		State:      st,
	}, nil)
	got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	u := got.Usage
	if u == nil || u.Input != 1 || u.Output != 2 || u.CacheRead != 3 || u.CacheWrite != 4 ||
		u.Reasoning == nil || *u.Reasoning != 7 || u.Cost == nil || *u.Cost != 0.42 ||
		u.Source != fleet.ConfidenceObserved || !u.AsOf.Equal(asOf) ||
		u.LastTurn == nil || u.LastTurn.Input != 1 {
		t.Fatalf("relayed usage = %+v", u)
	}
}

// A peer on a build that predates the field sends no key: "not yet known", never a block of zeros.
func TestAPeerWithoutUsageRelaysNone(t *testing.T) {
	srv := peerRaw(t, `{"machine":"peerbox","id":"s1","runtime":"claude-code-tmux",`+
		`"state":{"status":"idle","confidence":"inferred","evidence":"e"}}`)
	got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage != nil {
		t.Errorf("an absent key arrived as %+v", got.Usage)
	}
}
