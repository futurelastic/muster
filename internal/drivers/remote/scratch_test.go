package remote

import (
	"context"
	"reflect"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// state.scratch crosses a peer relay untouched (muster #419, #431): the machine that
// keeps the directory is the one that measured it, so what arrives is its figure,
// its split into areas and its time, and this driver neither drops nor re-derives
// any of them.
func TestScratchSurvivesARelay(t *testing.T) {
	st := fleet.ObservedState(fleet.StatusIdle, "idle", nil)
	asOf := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	areas := map[string]int64{"tmp": 2_900_000_000, "home": 150_000_000, "xdg": 50_000_000}
	st.Scratch = &fleet.ScratchUsage{Bytes: 3_100_000_000, Areas: areas, AsOf: asOf, Partial: true}
	srv := peerServing(t, 200, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
		Runtime:    "opencode",
		State:      st,
	}, nil)
	got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Scratch; s == nil || s.Bytes != 3_100_000_000 || !s.AsOf.Equal(asOf) || !s.Partial || !reflect.DeepEqual(s.Areas, areas) {
		t.Fatalf("relayed scratch = %+v, want areas %v", s, areas)
	}
}

// A peer on a build that has the figure but not its split sends no `areas`: the figure
// arrives as it was sent, with no split made up for it.
func TestAPeerWithoutAreasRelaysTheFigureAlone(t *testing.T) {
	srv := peerRaw(t, `{"machine":"peerbox","id":"s1","runtime":"opencode",`+
		`"state":{"status":"idle","confidence":"observed","evidence":"e",`+
		`"scratch":{"bytes":4096,"asOf":"2026-11-07T01:02:03Z"}}}`)
	got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Scratch; s == nil || s.Bytes != 4096 || s.Areas != nil {
		t.Fatalf("relayed scratch = %+v, want 4096 bytes and no areas", s)
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
