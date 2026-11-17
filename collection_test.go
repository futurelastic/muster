package fleet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewCollection_RequiresSources(t *testing.T) {
	_, err := NewCollection[Session](nil, nil)
	if !errors.Is(err, ErrCollectionNeedsSources) {
		t.Fatalf("NewCollection(nil, nil) err = %v, want ErrCollectionNeedsSources", err)
	}

	_, err = NewCollection([]Session{{}}, nil)
	if !errors.Is(err, ErrCollectionNeedsSources) {
		t.Fatalf("NewCollection(items, nil) err = %v, want ErrCollectionNeedsSources — items with no sources is exactly the bug §9 forbids", err)
	}
}

func TestCollection_CompleteIsDerivedNotSupplied(t *testing.T) {
	now := time.Now()

	allOK, err := NewCollection([]int{1, 2}, []SourceStatus{
		{Machine: "a", Status: SourceOK, ObservedAt: now},
		{Machine: "b", Status: SourceOK, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	if !allOK.Complete() {
		t.Fatal("Complete() = false, want true when every source is ok")
	}

	oneUnreachable, err := NewCollection([]int{1}, []SourceStatus{
		{Machine: "a", Status: SourceOK, ObservedAt: now},
		{Machine: "b", Status: SourceUnreachable, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	if oneUnreachable.Complete() {
		t.Fatal("Complete() = true, want false when a source is unreachable")
	}

	oneDegraded, err := NewCollection([]int{1}, []SourceStatus{
		{Machine: "a", Status: SourceDegraded, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	// A degraded source answered, but its data is not to be trusted at
	// face value (§13.2) — Complete must not read as "fully answered"
	// merely because the source technically replied.
	if oneDegraded.Complete() {
		t.Fatal("Complete() = true, want false when a source reports degraded")
	}
}

func TestCollection_JSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original, err := NewCollection([]string{"x", "y"}, []SourceStatus{
		{Machine: "m1", Status: SourceOK, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Collection[string]
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Items()) != 2 || decoded.Items()[0] != "x" || decoded.Items()[1] != "y" {
		t.Fatalf("Items() = %v, want [x y]", decoded.Items())
	}
	if len(decoded.Sources()) != 1 || decoded.Sources()[0].Machine != "m1" {
		t.Fatalf("Sources() = %v", decoded.Sources())
	}
	if !decoded.Complete() {
		t.Fatal("Complete() = false, want true")
	}
}

func TestCollection_UnmarshalRejectsEmptySources(t *testing.T) {
	wire := `{"items":[],"sources":[],"complete":true}`
	var c Collection[int]
	if err := json.Unmarshal([]byte(wire), &c); !errors.Is(err, ErrCollectionNeedsSources) {
		t.Fatalf("Unmarshal err = %v, want ErrCollectionNeedsSources — a wire envelope claiming completeness with zero sources is the bug §9 exists to prevent", err)
	}
}

func TestCollection_UnmarshalRecomputesCompleteFromSources(t *testing.T) {
	// A foreign implementation's envelope claims complete:true while a
	// source clearly failed. This process must not trust that boolean
	// verbatim — see NewCollection's doc comment.
	wire := `{"items":[],"sources":[{"machine":"m1","status":"unreachable","observedAt":"2026-01-01T00:00:00Z"}],"complete":true}`
	var c Collection[int]
	if err := json.Unmarshal([]byte(wire), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if c.Complete() {
		t.Fatal("Complete() = true, want false — recomputed from Sources, not trusted from the wire")
	}
}

// #10.b: Quotas are a fact about willingness, independent of Status, which is
// a fact about reachability — and it must stay that way through both
// construction and the wire. One block per credential (#490).
func TestSourceStatus_QuotaIsIndependentOfStatusAndRoundTrips(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	blocked, err := NewCollection([]int{1}, []SourceStatus{
		{Machine: "a", Status: SourceOK, ObservedAt: now,
			Quotas: []QuotaBlock{{Credential: "c-1", Since: now, ResetHint: "Aug 10"}}},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	// A source reporting SourceOK with an account block set is exactly the
	// case §9's envelope exists to make expressible: reachable AND
	// answering, but not willing. Complete() reads Status alone — Quotas
	// must not move it, or the two axes have been folded back into one.
	if !blocked.Complete() {
		t.Fatal("Complete() = false, want true — an ok source stays ok regardless of Quota")
	}
	if q := blocked.Sources()[0].Quotas; len(q) != 1 || q[0].ResetHint != "Aug 10" || q[0].Credential != "c-1" {
		t.Fatalf("Quotas not carried on construction: %+v", blocked.Sources()[0])
	}

	b, err := json.Marshal(blocked)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Collection[int]
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// The single-block field this replaced is never written (muster #490): a
	// client still reading it would read "no block" and keep dispatching.
	if strings.Contains(string(b), `"quota":`) {
		t.Errorf("the old machine-wide quota field is still on the wire: %s", b)
	}
	got := decoded.Sources()[0]
	if len(got.Quotas) != 1 || got.Quotas[0].ResetHint != "Aug 10" || got.Quotas[0].Credential != "c-1" {
		t.Fatalf("Quotas did not survive the wire: %+v", got)
	}
	if got.Status != SourceOK {
		t.Errorf("Status = %q, want ok — unaffected by Quotas", got.Status)
	}

	// The common case — no block — must not put an empty object on the
	// wire (json:",omitempty" bears the whole weight of this).
	clean, err := NewCollection([]int{1}, []SourceStatus{{Machine: "a", Status: SourceOK, ObservedAt: now}})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	cb, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(cb), `"quota"`) {
		t.Errorf("an absent block must not appear on the wire at all: %s", cb)
	}
}

func TestCollection_EmptyItemsWithOKSourceIsNotAFailure(t *testing.T) {
	// §5.7: "no sessions on that machine" and "could not reach that
	// machine" are different facts. This is the first one: zero items,
	// source ok.
	now := time.Now()
	c, err := NewCollection[Session](nil, []SourceStatus{{Machine: "m1", Status: SourceOK, ObservedAt: now}})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	if len(c.Items()) != 0 {
		t.Fatalf("Items() = %v, want empty", c.Items())
	}
	if !c.Complete() {
		t.Fatal("Complete() = false, want true — an ok source that legitimately found nothing is still complete")
	}
}

// A snapshot that carries where it sits in the sequence is what lets a client
// list once and watch deltas. It must survive the wire, and its absence must
// survive too: omitted means "not a resume point", which a decoder that
// defaulted it to zero would turn into "resume from the beginning".
func TestCollection_FeedPositionRoundTripsAndAbsenceSurvives(t *testing.T) {
	col, err := NewCollection([]string{"a"}, []SourceStatus{{Machine: "one", Status: SourceOK, ObservedAt: time.Now()}})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}

	if col.Feed() != nil {
		t.Error("a collection nobody stamped must not claim a position")
	}
	raw, err := json.Marshal(col)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "feed") {
		t.Errorf("an unstamped collection must omit feed entirely, got %s", raw)
	}

	stamped := col.WithFeed(41, "epoch-1")
	raw, err = json.Marshal(stamped)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Collection[string]
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Feed() == nil {
		t.Fatal("a stamped position was lost in transit")
	}
	if back.Feed().Cursor != 41 || back.Feed().Epoch != "epoch-1" {
		t.Errorf("feed = %+v; want cursor 41 in epoch-1", *back.Feed())
	}
}

// WithFeed returns a copy. Stamping one view of a collection must not reach
// back into the value somebody else is already holding.
func TestCollection_WithFeedDoesNotMutateTheOriginal(t *testing.T) {
	col, err := NewCollection([]string{"a"}, []SourceStatus{{Machine: "one", Status: SourceOK, ObservedAt: time.Now()}})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	_ = col.WithFeed(7, "e")
	if col.Feed() != nil {
		t.Error("WithFeed mutated its receiver")
	}
}
