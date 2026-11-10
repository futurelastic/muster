package opencode

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
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

// readOne reads one session the way GET /v1/machines/{machine}/sessions/{id} does:
// a listing narrowed to the id (muster #339), which is where a caller sees pins.
func readOne(t *testing.T, d *Driver, id string) fleet.Session {
	t.Helper()
	col, err := d.List(context.Background(), testReq, driver.ListFilter{ID: id})
	if err != nil {
		t.Fatalf("List(id): %v", err)
	}
	for _, s := range col.Items() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %s is not listed", id)
	return fleet.Session{}
}

// messageReadsWith counts the requests the fake server saw for a session's message
// history, optionally only those with the given query ("" counts every one).
func messageReadsWith(f *fakeServer, query string) int {
	n := 0
	for _, r := range f.requestsSnapshot() {
		if r.method == "GET" && strings.HasSuffix(r.path, "/message") && (query == "" || r.query == query) {
			n++
		}
	}
	return n
}

// muster #437: the pins and the model resolve on a READ of the session, not only on
// a repeat of the create. #373 specified them for "the session"; the read is what
// GET answers from, and it used to build neither (the session reported model: null
// and pins.model.honoured: null forever, however many turns had run).
func TestRead_ByIDResolvesTheModelPinAfterATurn(t *testing.T) {
	for _, model := range []string{"anthropic/claude", "anthropic/vendor/claude"} {
		t.Run(model, func(t *testing.T) {
			provider, modelID, _ := strings.Cut(model, "/")
			f := newFakeServer(t)
			d := newTestDriver(t, f)
			sess, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: model})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			msg := func(role, provider, model string) wireMessage {
				return wireMessage{Info: wireMessageInfo{Role: role, ProviderID: provider, ModelID: model}}
			}

			// Before a turn: requested, unresolved, with the reason, and no model.
			got := readOne(t, d, sess.ID)
			if got.Pins == nil || got.Pins.Model == nil {
				t.Fatalf("Pins = %+v, want a Model entry on a read", got.Pins)
			}
			if p := got.Pins.Model; p.Requested != model || p.Honoured != nil || p.Evidence == "" {
				t.Errorf("before any turn: pin = %+v, want requested %s, unresolved, with a reason", p, model)
			}
			if got.Model != "" {
				t.Errorf("before any turn: Model = %q, want empty: nothing has been observed", got.Model)
			}

			// A user message is what was asked for, not what ran.
			f.setLastMessage(sess.ID, msg("user", provider, modelID))
			if got := readOne(t, d, sess.ID); got.Pins.Model.Honoured != nil || got.Model != "" {
				t.Errorf("with only a user message: pin = %+v, Model = %q, want unresolved and empty", got.Pins.Model, got.Model)
			}

			// The turn ran on the requested model.
			f.setLastMessage(sess.ID, msg("assistant", provider, modelID))
			got = readOne(t, d, sess.ID)
			p := got.Pins.Model
			if p.Honoured == nil || !*p.Honoured {
				t.Fatalf("a turn that ran on %s: Honoured = %v, want true", model, p.Honoured)
			}
			if p.Applied != model || p.Source != fleet.PinObserved || p.Requested != model {
				t.Errorf("pin = %+v, want requested and applied %s, source observed", p, model)
			}
			if got.Model != model {
				t.Errorf("Model = %q, want %q: the session read names the model the turn ran on", got.Model, model)
			}

			// The next turn fell back to another model: not honoured, and named.
			f.setLastMessage(sess.ID, msg("assistant", "openai", "gpt"))
			got = readOne(t, d, sess.ID)
			p = got.Pins.Model
			if p.Honoured == nil || *p.Honoured || p.Applied != "openai/gpt" || got.Model != "openai/gpt" {
				t.Errorf("a turn that ran on openai/gpt: pin = %+v, Model = %q, want honoured=false and applied openai/gpt", p, got.Model)
			}

			// Half a model is not a model.
			for _, half := range []wireMessage{msg("assistant", provider, ""), msg("assistant", "", modelID), msg("assistant", "", "")} {
				f.setLastMessage(sess.ID, half)
				if got := readOne(t, d, sess.ID); got.Pins.Model.Honoured != nil || got.Model != "" {
					t.Errorf("a record naming %q/%q: pin = %+v, Model = %q, want unresolved and empty",
						half.Info.ProviderID, half.Info.ModelID, got.Pins.Model, got.Model)
				}
			}
		})
	}
}

// The agent pin had the same gap (it resolved only on a repeat of the create) and
// resolves from the same record, so a read reports both, each on its own half.
func TestRead_ByIDResolvesTheAgentPinToo(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build", "tester"}
	d := newTestDriver(t, f)
	sess, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{
		Cwd: "/work/x", Name: "t", Agent: "tester", Model: "anthropic/claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	f.setLastMessage(sess.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", Agent: "tester", ProviderID: "anthropic", ModelID: "claude"}})
	got := readOne(t, d, sess.ID)
	if got.Pins == nil || got.Pins.Agent == nil || got.Pins.Model == nil {
		t.Fatalf("Pins = %+v, want an Agent and a Model entry", got.Pins)
	}
	if a := got.Pins.Agent; a.Honoured == nil || !*a.Honoured || a.Applied != "tester" || a.Source != fleet.PinObserved {
		t.Errorf("agent pin = %+v, want honoured, applied tester, observed", a)
	}
	if m := got.Pins.Model; m.Honoured == nil || !*m.Honoured {
		t.Errorf("model pin = %+v, want honoured", m)
	}

	// The turn ran as the default agent: not honoured, and the model is unaffected.
	f.setLastMessage(sess.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", Agent: "build", ProviderID: "anthropic", ModelID: "claude"}})
	got = readOne(t, d, sess.ID)
	if a := got.Pins.Agent; a.Honoured == nil || *a.Honoured || a.Applied != "build" {
		t.Errorf("agent pin = %+v, want honoured=false applied build", a)
	}
	if m := got.Pins.Model; m.Honoured == nil || !*m.Honoured {
		t.Errorf("model pin = %+v, want honoured: the agent's miss is not the model's", m)
	}
}

// The read of one session pays one request for this, and only for a session that
// pinned something (#339, #363: a read stays cheap). A listing pays nothing: it
// reports the pins as requested and unresolved and says it did not look, never the
// create-time "no turn has run yet", which is false once one has.
func TestRead_OnlyAReadByIDAsksTheRuntimeForTheRecord(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build"}
	d := newTestDriver(t, f)
	pinned, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: "/work/x", Name: "p", Model: "anthropic/claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plain, err := d.Create(context.Background(), testReq, "k2", fleet.SessionSpec{Cwd: "/work/y", Name: "q"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.setLastMessage(pinned.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "anthropic", ModelID: "claude"}})

	// The evidence read is the one with the tail the create replay uses.
	const evidenceQuery = "limit=8"
	before := messageReadsWith(f, evidenceQuery)
	if got := readOne(t, d, plain.ID); got.Pins != nil || got.Model != "" {
		t.Errorf("a session that pinned nothing: Pins = %+v, Model = %q, want neither", got.Pins, got.Model)
	}
	if n := messageReadsWith(f, evidenceQuery) - before; n != 0 {
		t.Errorf("a read of a session that pinned nothing read the evidence %d time(s), want 0", n)
	}
	before = messageReadsWith(f, evidenceQuery)
	if got := readOne(t, d, pinned.ID); got.Pins == nil || got.Pins.Model.Honoured == nil || !*got.Pins.Model.Honoured {
		t.Fatalf("a read by id: pins = %+v, want the model honoured", got.Pins)
	}
	if n := messageReadsWith(f, evidenceQuery) - before; n != 1 {
		t.Errorf("a read by id of a pinned session read the evidence %d time(s), want exactly 1", n)
	}

	// A listing: no request for any session's history, and the reason says so.
	before = messageReadsWith(f, "")
	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := messageReadsWith(f, "") - before; n != 0 {
		t.Errorf("a listing read the message history %d time(s) (a listing's cost is constant, #339)", n)
	}
	var seen bool
	for _, s := range col.Items() {
		if s.ID != pinned.ID {
			continue
		}
		seen = true
		if s.Pins == nil || s.Pins.Model == nil {
			t.Fatalf("a listing: Pins = %+v, want the Model pin as requested", s.Pins)
		}
		if m := s.Pins.Model; m.Requested != "anthropic/claude" || m.Honoured != nil || m.Evidence != pinUnreadListing {
			t.Errorf("a listing: model pin = %+v, want requested, unresolved, evidence %q", m, pinUnreadListing)
		}
		if s.Model != "" {
			t.Errorf("a listing: Model = %q, want empty (nothing was observed)", s.Model)
		}
	}
	if !seen {
		t.Fatal("the pinned session is not in the listing")
	}
}

// A read that could not ask says so, and is never read as "no turn has run": the
// failed request is a different fact from an empty history.
func TestRead_AFailedReadOfTheRecordIsNotReportedAsNoTurn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	sess, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: "anthropic/claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.setLastMessage(sess.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", ProviderID: "anthropic", ModelID: "claude"}})
	f.messagesDown = true
	got := readOne(t, d, sess.ID)
	f.messagesDown = false
	if got.Pins == nil || got.Pins.Model == nil {
		t.Fatalf("Pins = %+v, want the Model pin as requested", got.Pins)
	}
	if m := got.Pins.Model; m.Honoured != nil || m.Evidence != pinUnreadFailed {
		t.Errorf("model pin = %+v, want unresolved with evidence %q", m, pinUnreadFailed)
	}
	if got.Model != "" {
		t.Errorf("Model = %q, want empty", got.Model)
	}
}

// The created event carries what the create response carries (requested, not yet
// resolved), so a subscriber and a caller agree.
func TestSubscribe_TheCreatedEventCarriesThePinsAsRequested(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build"}
	d := newTestDriver(t, f)
	s := subscribe(t, d, driver.SubscribeFilter{})
	if _, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{
		Cwd: "/work/x", Name: "t", Agent: "build", Model: "anthropic/claude"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ev := next(t, s)
	sess, ok := ev.Payload.(fleet.Session)
	if ev.Kind != fleet.EventSessionCreated || !ok {
		t.Fatalf("event = %s %+v, want session.created", ev.Kind, ev.Payload)
	}
	if sess.Pins == nil || sess.Pins.Agent == nil || sess.Pins.Model == nil {
		t.Fatalf("created event Pins = %+v, want an Agent and a Model entry", sess.Pins)
	}
	if a, m := sess.Pins.Agent, sess.Pins.Model; a.Requested != "build" || a.Honoured != nil || m.Requested != "anthropic/claude" || m.Honoured != nil {
		t.Errorf("created event pins = %+v / %+v, want both requested and unresolved", a, m)
	}
}
