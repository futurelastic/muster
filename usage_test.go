package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUsage_WireShapeIsFlatAndOmitsWhatTheRuntimeDidNotReport(t *testing.T) {
	at := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	b, err := json.Marshal(NewUsage(UsageFigures{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}, ConfidenceInferred, at, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"source":"inferred","asOf":"2026-10-10T00:00:00Z"}`
	if string(b) != want {
		t.Fatalf("wire = %s\nwant   %s", b, want)
	}
	// Reasoning and cost are present when reported — including a reported zero.
	zero := 0.0
	b, _ = json.Marshal(NewUsage(UsageFigures{Cost: &zero}, ConfidenceObserved, at, nil))
	if !strings.Contains(string(b), `"cost":0`) {
		t.Errorf("a reported zero cost was dropped: %s", b)
	}
}

func TestUsageFigures_AddKeepsAbsenceApartFromZero(t *testing.T) {
	r, c := int64(5), 0.5
	a := UsageFigures{Input: 1}
	b := UsageFigures{Input: 2, Reasoning: &r, Cost: &c}
	sum := a.Add(b)
	if sum.Input != 3 || sum.Reasoning == nil || *sum.Reasoning != 5 || sum.Cost == nil || *sum.Cost != 0.5 {
		t.Errorf("sum = %+v", sum)
	}
	if none := a.Add(a); none.Reasoning != nil || none.Cost != nil {
		t.Errorf("two sides that reported neither produced %+v", none)
	}
	if !(UsageFigures{}).IsZero() || (UsageFigures{Cost: new(float64)}).IsZero() {
		t.Error("IsZero must treat a reported zero cost as a figure")
	}
}

// A request completing moves usage and nothing else on the state, so a feed
// that did not fire on it would leave a mirror showing the old spend.
func TestMateriallyDiffers_UsageIsMaterial(t *testing.T) {
	at := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	base := InferredState(StatusIdle, "e", nil)
	same := func() SessionState {
		s := base
		s.Usage = NewUsage(UsageFigures{Input: 10}, ConfidenceInferred, at, NewTurnUsage(UsageFigures{Input: 10}, at))
		return s
	}
	if same().MateriallyDiffers(same()) {
		t.Error("identical usage reads as a change")
	}
	if !base.MateriallyDiffers(same()) || !same().MateriallyDiffers(base) {
		t.Error("usage appearing or vanishing must be material")
	}
	more := same()
	more.Usage.Input = 11
	if !same().MateriallyDiffers(more) {
		t.Error("a larger total must be material")
	}
	later := same()
	later.Usage.LastTurn = NewTurnUsage(UsageFigures{Input: 10}, at.Add(time.Minute))
	if !same().MateriallyDiffers(later) {
		t.Error("a new last turn must be material")
	}
	// AsOf alone moving is not a change a consumer acts on.
	asOf := same()
	asOf.Usage.AsOf = at.Add(time.Hour)
	if same().MateriallyDiffers(asOf) {
		t.Error("asOf alone must not fire an event")
	}
}
