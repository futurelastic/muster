package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// muster #485: the wire form of a closed session's final usage.

func TestClosedUsage_WireForm(t *testing.T) {
	reasoning, cost := int64(12), 0.76
	asOf := time.Date(2026, 1, 1, 0, 0, 20, 0, time.UTC)
	u := NewClosedUsage(UsageFigures{Input: 311, Output: 42, CacheRead: 3013, CacheWrite: 60, Reasoning: &reasoning, Cost: &cost},
		ConfidenceObserved, asOf)
	u.Provider, u.Model, u.Credential = "p", "m", CredentialSubscription

	raw, err := json.Marshal(ClosedSession{SessionRef: SessionRef{ID: "s"}, Usage: u})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	usage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object in %s", raw)
	}
	want := map[string]any{
		"input": 311.0, "output": 42.0, "cacheRead": 3013.0, "cacheWrite": 60.0,
		"reasoning": 12.0, "cost": 0.76, "credential": "subscription",
		"provider": "p", "model": "m", "source": "observed", "asOf": "2026-01-01T00:00:20Z",
	}
	if len(usage) != len(want) {
		t.Errorf("usage has keys %v, want exactly %v", usage, want)
	}
	for k, v := range want {
		if usage[k] != v {
			t.Errorf("usage[%q] = %v, want %v", k, usage[k], v)
		}
	}

	var back ClosedSession
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	b := back.Usage
	if b == nil || b.Input != 311 || b.Output != 42 || b.CacheRead != 3013 || b.CacheWrite != 60 ||
		b.Reasoning == nil || *b.Reasoning != 12 || b.Cost == nil || *b.Cost != 0.76 ||
		b.Provider != "p" || b.Model != "m" || b.Credential != CredentialSubscription || !b.AsOf.Equal(asOf) {
		t.Errorf("round trip = %+v, want %+v", b, u)
	}
	if f := b.Figures(); f.Input != 311 || f.CacheRead != 3013 || f.Cost == nil {
		t.Errorf("figures after a round trip = %+v", f)
	}
}

// No figure is not a zero figure: the key is absent, and so are the optional
// figures a runtime did not report.
func TestClosedUsage_AbsentIsAbsentNotZero(t *testing.T) {
	raw, err := json.Marshal(ClosedSession{SessionRef: SessionRef{ID: "s"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "usage") {
		t.Errorf("a closed session with no figure carries a usage key: %s", raw)
	}
	raw, err = json.Marshal(NewClosedUsage(UsageFigures{Input: 1}, ConfidenceObserved, time.Unix(0, 0).UTC()))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"reasoning", "cost", "credential", "provider", "model"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("an unreported %s is on the wire: %s", k, raw)
		}
	}

	dead := InferredState(StatusDead, "the session was closed", nil)
	raw, err = json.Marshal(SessionStatePayload{Ref: SessionRef{ID: "s"}, State: dead})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "finalUsage") {
		t.Errorf("a payload with no final figure carries the key: %s", raw)
	}
	raw, err = json.Marshal(SessionStatePayload{Ref: SessionRef{ID: "s"}, State: dead, FinalUsage: NewClosedUsage(UsageFigures{Input: 1}, ConfidenceObserved, time.Unix(0, 0).UTC())})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"finalUsage":{`) {
		t.Errorf("the final figure is not on the payload: %s", raw)
	}
}
