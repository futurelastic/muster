package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// muster #391: the stalled outcome and the stalled state field.

func TestOutcomeStalledRoundTripsWithItsReason(t *testing.T) {
	in := DeliveryReceipt{Outcome: OutcomeStalled, Reason: "the session reads working and its screen has not changed"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"outcome":"stalled"`) {
		t.Fatalf("wire form %s does not name the outcome", b)
	}
	var out DeliveryReceipt
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal(%s): %v", b, err)
	}
	if out.Outcome != OutcomeStalled || out.Reason != in.Reason {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
	if out.Delivery != nil {
		t.Errorf("a stalled receipt names a path %+v; it is decided before any path is chosen", out.Delivery)
	}
}

func TestStalledStateRoundTripsAndStaysWorking(t *testing.T) {
	since := time.Date(2026, 11, 1, 3, 4, 5, 0, time.UTC)
	in := ObservedState(StatusWorking, "spinner line in running form", &since)
	in.Stalled = &Stall{Since: since, Reason: "its screen has not changed"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"status":"working"`, `"stalled":{"since":"2026-11-01T03:04:05Z","reason":"its screen has not changed"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("wire form %s lacks %s", b, want)
		}
	}
	var out SessionState
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Stalled == nil || !out.Stalled.Since.Equal(since) || out.Stalled.Reason != in.Stalled.Reason {
		t.Fatalf("round trip lost the stall: %+v", out.Stalled)
	}
	if out.Status != StatusWorking {
		t.Errorf("status = %s, want working", out.Status)
	}

	// Absent means not judged: the key is not on the wire at all.
	in.Stalled = nil
	b, _ = json.Marshal(in)
	if strings.Contains(string(b), "stalled") {
		t.Errorf("a state with no stall carries the key: %s", b)
	}
}

func TestAStallIsAMaterialChangeByItsSinceNotItsReason(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	working := ObservedState(StatusWorking, "spinner line in running form", &t0)

	stalled := working
	stalled.Stalled = &Stall{Since: t0, Reason: "quiet for 30m0s"}

	if !stalled.MateriallyDiffers(working) || !working.MateriallyDiffers(stalled) {
		t.Error("a session going stalled, or clearing, is not material; a mirror would keep believing it is making progress")
	}

	// The same stall read again later: the prose carries how long, which moves
	// on every read, and must not turn each poll into an event.
	later := working
	later.Stalled = &Stall{Since: t0, Reason: "quiet for 43h0m0s"}
	if stalled.MateriallyDiffers(later) {
		t.Error("a stall that merely continues is a material change; every poll would emit an event")
	}

	// A new quiet stretch is a new fact, even with no stall in between.
	next := working
	next.Stalled = &Stall{Since: t0.Add(2 * time.Hour), Reason: "quiet for 30m0s"}
	if !stalled.MateriallyDiffers(next) {
		t.Error("a new quiet stretch does not fire")
	}
}
