package remote

import (
	"context"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// state.scratch crosses a peer relay untouched (muster #419): the machine that
// keeps the directory is the one that measured it, so what arrives is its figure
// and its time, and this driver neither drops nor re-derives either.
func TestScratchSurvivesARelay(t *testing.T) {
	st := fleet.ObservedState(fleet.StatusIdle, "idle", nil)
	asOf := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	st.Scratch = &fleet.ScratchUsage{Bytes: 3_100_000_000, AsOf: asOf, Partial: true}
	srv := peerServing(t, 200, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
		Runtime:    "opencode",
		State:      st,
	}, nil)
	got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Scratch; s == nil || s.Bytes != 3_100_000_000 || !s.AsOf.Equal(asOf) || !s.Partial {
		t.Fatalf("relayed scratch = %+v", s)
	}
}

// A peer on a build that predates the field sends no key: no figure, never a size of zero.
func TestAPeerWithoutScratchRelaysNone(t *testing.T) {
	srv := peerRaw(t, `{"machine":"peerbox","id":"s1","runtime":"opencode",`+
		`"state":{"status":"idle","confidence":"observed","evidence":"e"}}`)
	got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Scratch != nil {
		t.Errorf("an absent key arrived as %+v", got.Scratch)
	}
}
