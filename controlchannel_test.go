package fleet

import (
	"encoding/json"
	"testing"
)

func TestControlChannelState_JSONRoundTrip(t *testing.T) {
	all := []ControlChannelState{
		ControlChannelActive, ControlChannelConnecting,
		ControlChannelReconnecting, ControlChannelFailed, ControlChannelOff,
	}
	for _, want := range all {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", want, err)
		}
		var got ControlChannelState
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Errorf("round trip: %q -> %q", want, got)
		}
	}
}

// The set is closed at the decoder. A state outside the set decoded silently would fall
// through every client's branch on the known names to its default, which is the quiet
// wrong answer this type exists to prevent.
func TestControlChannelState_RejectsAnythingOutsideTheSet(t *testing.T) {
	for _, raw := range []string{`"suspended"`, `"connected"`, `"Active"`, `""`} {
		var got ControlChannelState
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("Unmarshal(%s) was accepted as %q", raw, got)
		}
	}
	if _, err := json.Marshal(ControlChannelState("suspended")); err == nil {
		t.Error("marshalling a state outside the set must fail")
	}
}

// Absent must stay absent on the wire. A field that serialised as an empty
// object or a zero state would turn "nothing to report" into a verdict, which
// is exactly the §5.7 collapse the type's doc comment forbids.
func TestAbsentControlChannelIsOmittedEntirely(t *testing.T) {
	st := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred, Evidence: "settled"}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(b); contains(got, "controlChannel") {
		t.Errorf("an unreported channel appeared on the wire: %s", got)
	}

	st.ControlChannel = &ControlChannel{State: ControlChannelFailed}
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back SessionState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.ControlChannel == nil || back.ControlChannel.State != ControlChannelFailed {
		t.Errorf("channel lost in transit: %s", b)
	}
}

// The channel says nothing about what the session is doing, so it must not be
// part of what makes a state materially different... except that it IS: a
// channel going down is precisely the transition a subscriber maintaining a
// mirror needs to be told about, and it changes no other field.
func TestAChannelChangeIsAMaterialChange(t *testing.T) {
	a := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}
	b := a
	b.ControlChannel = &ControlChannel{State: ControlChannelFailed}
	if !b.MateriallyDiffers(a) {
		t.Error("a channel that just failed would reach no subscriber; that is the " +
			"invisibility this field exists to end, moved to the event plane")
	}

	c := b
	c.ControlChannel = &ControlChannel{State: ControlChannelActive}
	if !c.MateriallyDiffers(b) {
		t.Error("failed -> active must be reported; a recovery nobody hears about " +
			"leaves a supervisor believing the session is still unreachable")
	}
	if c.MateriallyDiffers(c) {
		t.Error("an unchanged channel must not fire an event")
	}
}

// Reason is absent-safe on the wire the same way the channel itself is: a
// zero-value Reason must not appear, and a set one must round-trip.
func TestControlChannelReasonOmitsWhenEmptyAndRoundTripsWhenSet(t *testing.T) {
	st := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}
	st.ControlChannel = &ControlChannel{State: ControlChannelFailed}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(b); contains(got, "reason") {
		t.Errorf("an empty Reason appeared on the wire: %s", got)
	}

	st.ControlChannel.Reason = "Remote Control disconnected — this session was ended or archived from another device or app (code 4090)"
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back SessionState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.ControlChannel == nil || back.ControlChannel.Reason != st.ControlChannel.Reason {
		t.Errorf("reason lost in transit: %s", b)
	}
}

// A second failure explained differently from the first is a second event
// worth having, the same call sameTurnEnd already makes for TurnEnd.Reason —
// this pins that ControlChannel.Reason gets the same treatment (#69).
func TestAChannelReasonChangeIsAMaterialChangeEvenWithTheSameState(t *testing.T) {
	a := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}
	a.ControlChannel = &ControlChannel{State: ControlChannelFailed, Reason: "heartbeat failure"}
	b := a
	b.ControlChannel = &ControlChannel{State: ControlChannelFailed, Reason: "worker credential rejected"}
	if !b.MateriallyDiffers(a) {
		t.Error("the same state with a different runtime-reported reason must still fire — " +
			"a second failure explained differently is a second event")
	}
	c := b
	c.ControlChannel = &ControlChannel{State: ControlChannelFailed, Reason: b.ControlChannel.Reason}
	if c.MateriallyDiffers(b) {
		t.Error("an unchanged reason must not fire an event")
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The verb's confirmation is the channel changing, delivered as a session.state
// event (muster #269). Every transition the toggle causes — absent to off, off
// to active, active to off — must be material, or a client that sent the verb
// would never be told it took.
func TestEveryToggleTransitionIsAMaterialChange(t *testing.T) {
	at := func(c *ControlChannel) SessionState {
		return SessionState{Status: StatusIdle, Confidence: ConfidenceInferred, ControlChannel: c}
	}
	off := &ControlChannel{State: ControlChannelOff}
	active := &ControlChannel{State: ControlChannelActive}
	for name, pair := range map[string][2]SessionState{
		"absent → off":    {at(nil), at(off)},
		"off → active":    {at(off), at(active)},
		"active → off":    {at(active), at(off)},
		"failed → active": {at(&ControlChannel{State: ControlChannelFailed}), at(active)},
	} {
		if !pair[1].MateriallyDiffers(pair[0]) {
			t.Errorf("%s did not fire a state event", name)
		}
	}
	if at(off).MateriallyDiffers(at(&ControlChannel{State: ControlChannelOff})) {
		t.Error("an unchanged off must not fire an event")
	}
}
