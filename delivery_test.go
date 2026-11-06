package fleet

import (
	"encoding/json"
	"testing"
)

func TestOutcome_JSONRoundTrip(t *testing.T) {
	for _, want := range []Outcome{OutcomeSubmitted, OutcomeQueued, OutcomeRefused, OutcomeUnknown} {
		b, err := json.Marshal(DeliveryReceipt{Outcome: want})
		if err != nil {
			t.Fatalf("Marshal(%q): %v", want, err)
		}
		var got DeliveryReceipt
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got.Outcome != want {
			t.Fatalf("round trip: got %q, want %q", got.Outcome, want)
		}
	}
}

// muster #411: ended is part of the vocabulary — it crosses the wire, a peer
// relay and a stored receipt without being rewritten.
func TestOutcome_EndedRoundTrips(t *testing.T) {
	b, err := json.Marshal(DeliveryReceipt{Outcome: OutcomeEnded, Reason: "the session's process exited (exit status 0)"})
	if err != nil {
		t.Fatal(err)
	}
	var got DeliveryReceipt
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal(%s): %v", b, err)
	}
	if got.Outcome != OutcomeEnded || got.Reason == "" {
		t.Fatalf("round trip: got %+v", got)
	}
}

func TestOutcome_RejectsUnknownValue(t *testing.T) {
	var o Outcome
	if err := json.Unmarshal([]byte(`"maybe"`), &o); err == nil {
		t.Fatal("expected an error unmarshaling an unrecognised Outcome, got nil")
	}
}

// muster #86: a pending delivery must round-trip with Outcome absent —
// never asserting an outcome that has not resolved. WaitingOn is left
// unclassified here on purpose: this test is about Outcome's own null
// semantics, and an empty WaitingOn keeps the wire shape exactly what #86
// pinned before #126 added the field — see TestPromptDelivery_WaitingOn-
// RoundTrips, below, for the class field's own round trip.
func TestPromptDelivery_PendingRoundTrips(t *testing.T) {
	out := PromptPending("", "accepted at creation, not yet delivered")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `{"outcome":null,"evidence":"accepted at creation, not yet delivered"}` {
		t.Errorf("unexpected wire shape: %s", b)
	}
	var back PromptDelivery
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Outcome != nil {
		t.Errorf("Outcome = %v, want nil", back.Outcome)
	}
	if back.Evidence != out.Evidence {
		t.Errorf("Evidence = %q, want %q", back.Evidence, out.Evidence)
	}
	if back.WaitingOn != "" {
		t.Errorf("WaitingOn = %q, want empty (unclassified)", back.WaitingOn)
	}
}

// muster #126: WaitingOn round-trips alongside Evidence while a
// delivery is pending, and the wire carries it under its own name rather
// than folded into evidence's prose.
func TestPromptDelivery_WaitingOnRoundTrips(t *testing.T) {
	for _, want := range []WaitingReason{WaitingPrompt, WaitingUnsentInput, WaitingStarting} {
		out := PromptPending(want, "diagnosis for "+string(want))
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("waitingOn=%q: Marshal: %v", want, err)
		}
		var back PromptDelivery
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("waitingOn=%q: Unmarshal: %v", want, err)
		}
		if back.WaitingOn != want {
			t.Errorf("waitingOn=%q: round trip gave %q", want, back.WaitingOn)
		}
		if back.Outcome != nil {
			t.Errorf("waitingOn=%q: Outcome = %v, want nil (still pending)", want, back.Outcome)
		}
	}

	// Resolved deliveries never carry a class — WaitingOn is meaningful only
	// while Outcome is nil (this type's own doc).
	resolved := PromptDelivered(OutcomeSubmitted, "delivered")
	if resolved.WaitingOn != "" {
		t.Errorf("a resolved PromptDelivery carries WaitingOn = %q, want empty", resolved.WaitingOn)
	}
}

// A resolved delivery round-trips carrying the same outcome send() would
// have returned for the same delivery.
func TestPromptDelivery_ResolvedRoundTrips(t *testing.T) {
	for _, want := range []Outcome{OutcomeSubmitted, OutcomeQueued, OutcomeRefused, OutcomeUnknown} {
		out := PromptDelivered(want, "evidence")
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("outcome=%q: Marshal: %v", want, err)
		}
		var back PromptDelivery
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("outcome=%q: Unmarshal: %v", want, err)
		}
		if back.Outcome == nil || *back.Outcome != want {
			t.Errorf("outcome=%q: round trip gave %v", want, back.Outcome)
		}
		if back.Evidence != "evidence" {
			t.Errorf("outcome=%q: Evidence = %q, want %q", want, back.Evidence, "evidence")
		}
	}
}

// A PromptDelivery that cannot support what it claims — no evidence, or an
// outcome outside the closed set — must neither encode nor decode. Same
// discipline ConversationRef and ResumeOutcome hold themselves to: silently
// accepting it would let a caller compare two malformed values and conclude
// they describe the same thing.
func TestPromptDelivery_RefusesToPresentSomethingItCannotSupport(t *testing.T) {
	submitted := OutcomeSubmitted
	bad := []struct {
		name string
		out  PromptDelivery
	}{
		{"no evidence, resolved", PromptDelivery{Outcome: &submitted}},
		{"no evidence, pending", PromptDelivery{}},
	}
	for _, c := range bad {
		if b, err := json.Marshal(c.out); err == nil {
			t.Errorf("%s: must not encode, got %s", c.name, b)
		}
	}

	// An outcome outside the closed set must not decode, even with evidence
	// present — the same rule Outcome's own decoder already enforces, now
	// reached through the field that embeds it.
	raw := []byte(`{"outcome":"maybe","evidence":"e"}`)
	var back PromptDelivery
	if err := json.Unmarshal(raw, &back); err == nil {
		t.Errorf("an unrecognised outcome must not decode, got %+v", back)
	}

	good := PromptPending(WaitingUnsentInput, "not resolved yet")
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("a well-formed value must encode: %v", err)
	}
	var got PromptDelivery
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Evidence != good.Evidence || got.Outcome != nil {
		t.Errorf("round trip changed the value: %+v vs %+v", got, *good)
	}
}

// #184: the receipt names the path that made it. Four properties are pinned:
// the field round-trips, an absent field encodes to nothing (so a receipt from
// a driver with one path is byte-identical to before), a route the receipt
// cannot honestly name is refused on the way out, and one this build does not
// know is dropped on the way in rather than failing the whole receipt.

func TestDeliveryReceipt_RouteRoundTrips(t *testing.T) {
	for _, route := range []Route{RouteInbox, RouteTerminal} {
		in := DeliveryReceipt{Outcome: OutcomeDelivered, Reason: "why"}.WithRoute(route)
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", route, err)
		}
		want := `{"outcome":"delivered","reason":"why","delivery":{"route":"` + string(route) + `"}}`
		if string(b) != want {
			t.Fatalf("wire shape = %s, want %s", b, want)
		}
		var out DeliveryReceipt
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if out.RouteOf() != route || out.Outcome != OutcomeDelivered || out.Reason != "why" {
			t.Fatalf("round trip: got %+v, want route %q", out, route)
		}
	}
}

func TestDeliveryReceipt_NoRouteEncodesNoDeliveryKey(t *testing.T) {
	b, err := json.Marshal(DeliveryReceipt{Outcome: OutcomeRefused})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"outcome":"refused"}` {
		t.Fatalf("a receipt naming no path must encode exactly as before #184, got %s", b)
	}
}

func TestDeliveryReceipt_UnrecognisedRouteIsDroppedNotGuessed(t *testing.T) {
	var got DeliveryReceipt
	if err := json.Unmarshal([]byte(`{"outcome":"queued","delivery":{"route":"carrier-pigeon"}}`), &got); err != nil {
		t.Fatalf("a receipt from a newer peer must still decode: %v", err)
	}
	if got.Outcome != OutcomeQueued {
		t.Errorf("outcome = %q, want queued", got.Outcome)
	}
	if got.Delivery != nil {
		t.Errorf("Delivery = %+v, want nil — an unknown route is 'not stated', never a guess", got.Delivery)
	}
	// "auto" is a request, never a path: a receipt claiming it is not believed.
	if err := json.Unmarshal([]byte(`{"outcome":"queued","delivery":{"route":"auto"}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Delivery != nil {
		t.Errorf("Delivery = %+v for route auto, want nil", got.Delivery)
	}
}

func TestDeliveryReceipt_MarshalRefusesInvalidRoute(t *testing.T) {
	for _, bad := range []Route{"", RouteAuto, "carrier-pigeon"} {
		if _, err := json.Marshal(DeliveryReceipt{Outcome: OutcomeQueued, Delivery: &DeliveryPath{Route: bad}}); err == nil {
			t.Errorf("Marshal accepted a receipt naming route %q", bad)
		}
	}
}

func TestDeliveryReceipt_WithRouteIgnoresANonPath(t *testing.T) {
	r := DeliveryReceipt{Outcome: OutcomeQueued}.WithRoute(RouteAuto)
	if r.Delivery != nil {
		t.Errorf("WithRoute(auto) stamped %+v; auto is a request, never a path", r.Delivery)
	}
	if got := (DeliveryReceipt{}).RouteOf(); got != "" {
		t.Errorf("RouteOf on a receipt naming none = %q, want empty", got)
	}
}

func TestDeliveryReceipt_UnknownOutcomeStillRejected(t *testing.T) {
	var got DeliveryReceipt
	if err := json.Unmarshal([]byte(`{"outcome":"maybe"}`), &got); err == nil {
		t.Fatal("the lenient route decoding must not have loosened the outcome check")
	}
}

// #185: a delivery module is a third path a receipt can name, and it always
// names WHICH module — the route alone would be an unnamed fourth state.

func TestDeliveryReceipt_ModuleRouteRoundTrip(t *testing.T) {
	in := DeliveryReceipt{Outcome: OutcomeQueued, Reason: "why"}.WithModule("relay-a")
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"outcome":"queued","reason":"why","delivery":{"route":"module","module":"relay-a"}}`
	if string(b) != want {
		t.Fatalf("wire shape = %s, want %s", b, want)
	}
	var out DeliveryReceipt
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.RouteOf() != RouteModule || out.ModuleOf() != "relay-a" {
		t.Fatalf("round trip: got %+v", out.Delivery)
	}
	// The other two paths never name a module.
	for _, route := range []Route{RouteTerminal, RouteInbox} {
		r := DeliveryReceipt{Outcome: OutcomeDelivered}.WithRoute(route)
		if r.ModuleOf() != "" {
			t.Errorf("route %q names module %q", route, r.ModuleOf())
		}
	}
}

func TestDeliveryReceipt_ModuleRouteWithoutNameRefused(t *testing.T) {
	// Marshal refuses the incoherent shapes rather than emitting them.
	for _, bad := range []*DeliveryPath{
		{Route: RouteModule},
		{Route: RouteTerminal, Module: "relay-a"},
		{Route: RouteInbox, Module: "relay-a"},
	} {
		if _, err := json.Marshal(DeliveryReceipt{Outcome: OutcomeQueued, Delivery: bad}); err == nil {
			t.Errorf("Marshal accepted %+v", bad)
		}
	}
	// WithRoute cannot stamp a nameless module; WithModule("") is a no-op.
	if r := (DeliveryReceipt{Outcome: OutcomeQueued}).WithRoute(RouteModule); r.Delivery != nil {
		t.Errorf("WithRoute(module) stamped %+v", r.Delivery)
	}
	if r := (DeliveryReceipt{Outcome: OutcomeQueued}).WithModule(""); r.Delivery != nil {
		t.Errorf("WithModule(\"\") stamped %+v", r.Delivery)
	}
	// A peer's nameless module route decodes as "not stated", never a guess;
	// the outcome (the part that matters) survives.
	var got DeliveryReceipt
	if err := json.Unmarshal([]byte(`{"outcome":"queued","delivery":{"route":"module"}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != OutcomeQueued || got.Delivery != nil {
		t.Errorf("got %+v / %+v, want queued and no path", got, got.Delivery)
	}
	// module is a receipt value, never a route a caller may request.
	if RouteModule.Valid() {
		t.Error("RouteModule.Valid() = true; a caller requests a module by ITS name")
	}
}
