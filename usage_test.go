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

// #313: the context block's wire shape. `window` is an explicit null — the
// block says the runtime did not state one — while the block itself is absent,
// not zero, when nothing was read.
func TestUsage_ContextWireShape(t *testing.T) {
	at := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	u := NewUsage(UsageFigures{Input: 1}, ConfidenceInferred, at, nil)
	b, _ := json.Marshal(u)
	if strings.Contains(string(b), `"context"`) {
		t.Errorf("a usage block with no context size carries the key: %s", b)
	}
	u.Context = &ContextSize{Tokens: 142000, Model: "m", At: at}
	b, _ = json.Marshal(u)
	if !strings.Contains(string(b), `"context":{"tokens":142000,"window":null,"model":"m","at":"2026-10-10T00:00:00Z"}`) {
		t.Errorf("wire = %s", b)
	}
	w := int64(200000)
	u.Context.Window = &w
	b, _ = json.Marshal(u)
	if !strings.Contains(string(b), `"window":200000`) {
		t.Errorf("a stated window was dropped: %s", b)
	}
	var back Usage
	if err := json.Unmarshal(b, &back); err != nil || back.Context == nil || back.Context.Window == nil || *back.Context.Window != 200000 {
		t.Errorf("round trip = %+v, %v", back.Context, err)
	}
}

func TestDriverCapabilities_ReportsContextIsItsOwnFlag(t *testing.T) {
	b, err := json.Marshal(DriverCapabilities{Source: CapabilitiesObserved, ReportsUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"reportsUsage":true`) || !strings.Contains(string(b), `"reportsContext":false`) {
		t.Errorf("a driver that sums usage but not per-request figures must say so: %s", b)
	}
}

// The context size moving is material — a client drawing a ring off the feed
// needs every request — and its timestamp alone is not.
func TestMateriallyDiffers_ContextIsMaterial(t *testing.T) {
	at := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	mk := func(tokens int64) SessionState {
		s := InferredState(StatusIdle, "e", nil)
		s.Usage = NewUsage(UsageFigures{Input: 10}, ConfidenceInferred, at, nil)
		if tokens > 0 {
			s.Usage.Context = &ContextSize{Tokens: tokens, Model: "m", At: at}
		}
		return s
	}
	if mk(100).MateriallyDiffers(mk(100)) {
		t.Error("identical context reads as a change")
	}
	if !mk(100).MateriallyDiffers(mk(101)) {
		t.Error("a new context size must be material")
	}
	if !mk(0).MateriallyDiffers(mk(100)) || !mk(100).MateriallyDiffers(mk(0)) {
		t.Error("the context appearing or vanishing must be material")
	}
	later := mk(100)
	later.Usage.Context.At = at.Add(time.Hour)
	if mk(100).MateriallyDiffers(later) {
		t.Error("the context's timestamp alone must not fire an event")
	}
	win := int64(1_000_000)
	stated := mk(100)
	stated.Usage.Context.Window = &win
	if !mk(100).MateriallyDiffers(stated) {
		t.Error("a window becoming known must be material")
	}
}
