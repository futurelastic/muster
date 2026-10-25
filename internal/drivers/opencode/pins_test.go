package opencode

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #84, #362: this substrate's create response echoes the agent name it
// was given, listed or not, and a message sent without an agent runs as the
// default one — so the echo is not evidence of anything. A create that asked
// for an agent is therefore reported unresolved, with the reason, and never
// honoured on the strength of the echo alone.
func TestCreate_PinsAgentIsUnresolvedUntilATurnShowsIt(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t", Agent: "coder"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins == nil || sess.Pins.Agent == nil {
		t.Fatalf("Pins = %+v, want an Agent entry", sess.Pins)
	}
	a := sess.Pins.Agent
	if a.Requested != "coder" {
		t.Errorf("Requested = %q, want coder", a.Requested)
	}
	if a.Honoured != nil {
		t.Errorf("Honoured = %v, want nil — the fake server echoes the same agent back, which proves nothing", *a.Honoured)
	}
	if a.Source != "" || a.Applied != "" {
		t.Errorf("Source = %q, Applied = %q, want both empty while unresolved", a.Source, a.Applied)
	}
	if a.Evidence == "" {
		t.Error("Evidence is empty; an unresolved pin must say why")
	}
}

// A requested model has no echo anywhere in this substrate's create
// response, so it must be reported unresolved rather than assumed honoured.
func TestCreate_PinsReportsModelUnresolved(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: "anthropic/claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins == nil || sess.Pins.Model == nil {
		t.Fatalf("Pins = %+v, want a Model entry", sess.Pins)
	}
	if sess.Pins.Model.Honoured != nil {
		t.Errorf("Honoured = %v, want nil — this substrate's response never names the model", sess.Pins.Model.Honoured)
	}
	if sess.Pins.Model.Evidence == "" {
		t.Error("Evidence is empty; an unresolved pin must say why")
	}
}

// muster #373: the model pin resolves the way the agent pin does — from the
// runtime's own record of a turn that ran, never from the create. The newest
// assistant message names the provider and model the turn ran on; a user message
// only says what was asked for.
func TestModel_ThePinResolvesFromTheNewestAssistantMessage(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	spec := fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: "anthropic/claude"}

	sess, err := d.Create(context.Background(), testReq, "k1", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	replay := func() *fleet.PinResult {
		t.Helper()
		again, err := d.Create(context.Background(), testReq, "k1", spec)
		if err != nil {
			t.Fatalf("replayed Create: %v", err)
		}
		if again.ID != sess.ID {
			t.Fatalf("replayed Create returned session %s, want %s", again.ID, sess.ID)
		}
		if again.Pins == nil || again.Pins.Model == nil {
			t.Fatalf("Pins = %+v, want a Model entry", again.Pins)
		}
		return again.Pins.Model
	}
	msg := func(role, provider, model string) wireMessage {
		return wireMessage{Info: wireMessageInfo{Role: role, ProviderID: provider, ModelID: model}}
	}

	// Before a turn: the create response names no model, so the pin is unresolved
	// and says why.
	if p := replay(); p.Honoured != nil || p.Evidence == "" || p.Requested != "anthropic/claude" {
		t.Errorf("before any turn: pin = %+v, want requested anthropic/claude, unresolved, with a reason", p)
	}

	// A user message is what was asked for, not what ran.
	f.setLastMessage(sess.ID, msg("user", "anthropic", "claude"))
	if p := replay(); p.Honoured != nil {
		t.Errorf("with only a user message Honoured = %v, want nil", *p.Honoured)
	}

	// The turn ran on another model: the silent fallback to a default.
	f.setLastMessage(sess.ID, msg("assistant", "openai", "gpt"))
	p := replay()
	if p.Honoured == nil || *p.Honoured {
		t.Fatalf("a turn that ran on openai/gpt: Honoured = %v, want false", p.Honoured)
	}
	if p.Applied != "openai/gpt" || p.Source != fleet.PinObserved || p.Requested != "anthropic/claude" {
		t.Errorf("pin = %+v, want requested anthropic/claude, applied openai/gpt, source observed", p)
	}

	// The newest turn ran on the requested model.
	f.setLastMessage(sess.ID, msg("user", "anthropic", "claude"))
	f.setLastMessage(sess.ID, msg("assistant", "anthropic", "claude"))
	p = replay()
	if p.Honoured == nil || !*p.Honoured {
		t.Fatalf("a turn that ran on anthropic/claude: Honoured = %v, want true", p.Honoured)
	}
	if p.Applied != "anthropic/claude" || p.Source != fleet.PinObserved {
		t.Errorf("pin = %+v, want applied anthropic/claude, source observed", p)
	}

	// Half a model is not a model: no evidence either way.
	for _, half := range []wireMessage{msg("assistant", "anthropic", ""), msg("assistant", "", "claude"), msg("assistant", "", "")} {
		f.setLastMessage(sess.ID, half)
		if p := replay(); p.Honoured != nil || p.Applied != "" {
			t.Errorf("a record naming %q/%q: pin = %+v, want unresolved", half.Info.ProviderID, half.Info.ModelID, p)
		}
	}
}

// A model id may itself contain a slash ("provider/vendor/model"): the request is
// split at the FIRST one, and the record's provider and model join back to the
// same spelling, so a turn that ran on exactly that model reads honoured.
func TestModel_ASlashInTheModelIDStillMatches(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	spec := fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: "anthropic/vendor/claude"}

	sess, err := d.Create(context.Background(), testReq, "k1", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.setLastMessage(sess.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "anthropic", ModelID: "vendor/claude"}})
	again, err := d.Create(context.Background(), testReq, "k1", spec)
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if p := again.Pins.Model; p == nil || p.Honoured == nil || !*p.Honoured || p.Applied != "anthropic/vendor/claude" {
		t.Errorf("pin = %+v, want honoured, applied anthropic/vendor/claude", p)
	}
}

// The agent and the model are read from the same record but resolve apart: a
// message that names one and not the other resolves that pin and leaves the other
// unresolved, and neither is read as an answer for its sibling.
func TestPins_AgentAndModelResolveApart(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build", "tester"}
	d := newTestDriver(t, f)
	spec := fleet.SessionSpec{Cwd: "/work/x", Name: "t", Agent: "tester", Model: "anthropic/claude"}

	sess, err := d.Create(context.Background(), testReq, "k1", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	replay := func() *fleet.PinOutcome {
		t.Helper()
		again, err := d.Create(context.Background(), testReq, "k1", spec)
		if err != nil {
			t.Fatalf("replayed Create: %v", err)
		}
		if again.Pins == nil || again.Pins.Agent == nil || again.Pins.Model == nil {
			t.Fatalf("Pins = %+v, want an Agent and a Model entry", again.Pins)
		}
		return again.Pins
	}

	// The turn's record names the model and not the agent.
	f.setLastMessage(sess.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "anthropic", ModelID: "claude"}})
	pins := replay()
	if pins.Model.Honoured == nil || !*pins.Model.Honoured {
		t.Errorf("model pin = %+v, want honoured from the record", pins.Model)
	}
	if pins.Agent.Honoured != nil {
		t.Errorf("agent pin Honoured = %v, want nil: the record names no agent", *pins.Agent.Honoured)
	}

	// The next turn's record names the agent and not the model.
	f.setLastMessage(sess.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", Agent: "build"}})
	pins = replay()
	if pins.Agent.Honoured == nil || *pins.Agent.Honoured {
		t.Errorf("agent pin = %+v, want honoured=false: the turn ran as build", pins.Agent)
	}
	if pins.Model.Honoured != nil {
		t.Errorf("model pin Honoured = %v, want nil: the record names no model", *pins.Model.Honoured)
	}
}

// The runtime's history is read only for a session that pinned something it can
// answer. A model-only pin counts (it used to read nothing, because the read was
// gated on an agent being requested), and a session that pinned neither never
// costs the runtime a request.
func TestPins_TheHistoryIsReadOnlyForASessionThatPinnedSomething(t *testing.T) {
	reads := func(f *fakeServer) int {
		n := 0
		for _, r := range f.requestsSnapshot() {
			if r.method == "GET" && strings.HasSuffix(r.path, "/message") {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		name      string
		spec      fleet.SessionSpec
		wantReads int
	}{
		{"model only", fleet.SessionSpec{Cwd: "/work/x", Model: "anthropic/claude"}, 1},
		{"agent only", fleet.SessionSpec{Cwd: "/work/x", Agent: "build"}, 1},
		{"neither", fleet.SessionSpec{Cwd: "/work/x"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServer(t)
			f.agents = []string{"build"}
			d := newTestDriver(t, f)
			if _, err := d.Create(context.Background(), testReq, "k1", tc.spec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			before := reads(f)
			if _, err := d.Create(context.Background(), testReq, "k1", tc.spec); err != nil {
				t.Fatalf("replayed Create: %v", err)
			}
			if got := reads(f) - before; got != tc.wantReads {
				t.Errorf("a replay read the message history %d time(s), want %d", got, tc.wantReads)
			}
		})
	}
}

func TestCreate_NoPinsRequestedLeavesFieldAbsent(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins != nil {
		t.Errorf("Pins = %+v, want nil", sess.Pins)
	}
}
