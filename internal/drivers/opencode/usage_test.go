package opencode

import (
	"context"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Synthetic fixtures shaped like the runtime's message record: a user message,
// then assistant messages whose step-finish parts carry the per-step figures.

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func tok(in, out int64, reasoning *int64, cr, cw int64) *wireTokens {
	t := &wireTokens{Input: in, Output: out, Reasoning: reasoning}
	t.Cache.Read, t.Cache.Write = cr, cw
	return t
}

func userMsg(id string, created int64) wireMessage {
	m := wireMessage{Info: wireMessageInfo{ID: id, Role: "user"}}
	m.Info.Time.Created = created
	return m
}

func asstMsg(id string, created, completed int64, steps ...wirePart) wireMessage {
	m := wireMessage{Info: wireMessageInfo{ID: id, Role: "assistant"}, Parts: steps}
	m.Info.Time.Created, m.Info.Time.Completed = created, completed
	return m
}

func step(t *wireTokens, cost *float64) wirePart {
	return wirePart{Type: "step-finish", Tokens: t, Cost: cost}
}

func readUsage(t *testing.T, d *Driver, id string) fleet.SessionState {
	t.Helper()
	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: id})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return st
}

// The oracle the issue adds: the last step differs from the sum, and the block
// reports the sum — tokens, reasoning and cost alike.
func TestUsage_SumsEveryStepOfEveryMessage(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1000))
	f.setLastMessage(ref.ID, asstMsg("a1", 1001, 1010,
		step(tok(100, 10, i64(5), 1000, 50), f64(0.25)),
		step(tok(200, 20, i64(7), 2000, 0), f64(0.5)),
		step(tok(1, 2, i64(0), 3, 0), f64(0.01)), // a tiny last step
	))
	f.setLastMessage(ref.ID, asstMsg("a2", 1011, 1020, step(tok(10, 10, nil, 10, 10), nil)))

	st := readUsage(t, d, ref.ID)
	u := st.Usage
	if u == nil {
		t.Fatal("no usage on an idle session whose record reports it")
	}
	if u.Input != 311 || u.Output != 42 || u.CacheRead != 3013 || u.CacheWrite != 60 {
		t.Fatalf("cumulative = %+v, want 311/42/3013/60 (the sum, not the last step)", u)
	}
	if u.Reasoning == nil || *u.Reasoning != 12 {
		t.Errorf("reasoning = %v, want 12 carried as its own figure", u.Reasoning)
	}
	if u.Cost == nil || *u.Cost < 0.759 || *u.Cost > 0.761 {
		t.Errorf("cost = %v, want 0.76 summed over steps", u.Cost)
	}
	if u.Source != fleet.ConfidenceObserved {
		t.Errorf("source = %q, want observed (a structured API)", u.Source)
	}
	if u.AsOf.UnixMilli() != 1020 {
		t.Errorf("asOf = %v, want the newest message's completion", u.AsOf)
	}
	// One user message, so the whole run after it is the last turn.
	lt := u.LastTurn
	if lt == nil || lt.Input != 311 || lt.Output != 42 || lt.At.UnixMilli() != 1020 {
		t.Fatalf("lastTurn = %+v", lt)
	}
}

func TestUsage_LastTurnIsTheNewestFinishedRun(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3, step(tok(1, 1, nil, 0, 0), nil)))
	f.setLastMessage(ref.ID, userMsg("u2", 4))
	f.setLastMessage(ref.ID, asstMsg("a2", 5, 6, step(tok(10, 10, nil, 0, 0), nil)))
	f.setLastMessage(ref.ID, asstMsg("a3", 7, 8, step(tok(20, 20, nil, 0, 0), nil)))

	u := readUsage(t, d, ref.ID).Usage
	if u == nil || u.Input != 31 {
		t.Fatalf("cumulative = %+v", u)
	}
	if u.LastTurn == nil || u.LastTurn.Input != 30 || u.LastTurn.Output != 30 {
		t.Fatalf("lastTurn = %+v, want the second turn's two messages only", u.LastTurn)
	}

	// A turn in progress is in the total and is not yet the last turn.
	f.setLastMessage(ref.ID, userMsg("u3", 9))
	f.setLastMessage(ref.ID, asstMsg("a4", 10, 0, step(tok(100, 100, nil, 0, 0), nil)))
	f.setBusy(ref.ID)
	u = readUsage(t, d, ref.ID).Usage
	if u == nil || u.Input != 131 {
		t.Fatalf("cumulative with a turn running = %+v", u)
	}
	if u.LastTurn == nil || u.LastTurn.Input != 30 {
		t.Fatalf("lastTurn while working = %+v, want the previous finished turn", u.LastTurn)
	}
}

// A message with no step records reports its own figures; one with step
// records is never counted twice.
func TestUsage_MessageFiguresOnlyWhereThereAreNoSteps(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	plain := asstMsg("a1", 2, 3)
	plain.Info.Tokens, plain.Info.Cost = tok(5, 6, nil, 7, 8), f64(0.5)
	stepped := asstMsg("a2", 4, 5, step(tok(1, 1, nil, 0, 0), nil))
	stepped.Info.Tokens, stepped.Info.Cost = tok(999, 999, nil, 999, 999), f64(99)
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, plain)
	f.setLastMessage(ref.ID, stepped)

	u := readUsage(t, d, ref.ID).Usage
	if u == nil || u.Input != 6 || u.Output != 7 || u.CacheRead != 7 || u.CacheWrite != 8 {
		t.Fatalf("cumulative = %+v, want 6/7/7/8", u)
	}
	if u.Cost == nil || *u.Cost != 0.5 {
		t.Errorf("cost = %v, want only the plain message's 0.5", u.Cost)
	}
	if u.Reasoning != nil {
		t.Errorf("reasoning = %v, the runtime reported none", u.Reasoning)
	}
}

func TestUsage_NoReportedFiguresIsAbsentNotZero(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	if st := readUsage(t, d, ref.ID); st.Usage != nil {
		t.Errorf("a session with no history reports %+v, want none", st.Usage)
	}
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3)) // an assistant message that reported nothing
	if st := readUsage(t, d, ref.ID); st.Usage != nil {
		t.Errorf("figures the runtime never reported became %+v", st.Usage)
	}
	if !d.Capabilities().ReportsUsage {
		t.Error("the driver reads usage and must say so")
	}
}

// Later reads fetch a short tail and splice it on: a history longer than the
// window still sums exactly, and messages that finished since are picked up.
func TestUsage_IncrementalTailSplicesOntoWhatIsRemembered(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u0", 1))
	const n = usageTailWindow*2 + 5
	for i := 0; i < n; i++ {
		f.setLastMessage(ref.ID, asstMsg(idOf(i), int64(10+i), int64(11+i), step(tok(1, 2, nil, 0, 0), nil)))
	}
	u := readUsage(t, d, ref.ID).Usage
	if u == nil || u.Input != n {
		t.Fatalf("first read = %+v, want input %d", u, n)
	}
	// Only the tail is fetched from now on; a few more land.
	for i := 0; i < 3; i++ {
		f.setLastMessage(ref.ID, asstMsg(idOf(n+i), int64(100+i), int64(101+i), step(tok(1, 2, nil, 0, 0), nil)))
	}
	u = readUsage(t, d, ref.ID).Usage
	if u == nil || u.Input != n+3 || u.Output != 2*(n+3) {
		t.Fatalf("after three more = %+v, want input %d", u, n+3)
	}
	// And more than a window's worth since the last read: the window widens
	// until it meets what is remembered.
	for i := 0; i < usageTailWindow+10; i++ {
		f.setLastMessage(ref.ID, asstMsg(idOf(n+10+i), int64(200+i), int64(201+i), step(tok(1, 2, nil, 0, 0), nil)))
	}
	u = readUsage(t, d, ref.ID).Usage
	want := int64(n + 3 + usageTailWindow + 10)
	if u == nil || u.Input != want {
		t.Fatalf("after a gap wider than the window = %+v, want input %d", u, want)
	}
}

func idOf(i int) string { return "m" + string(rune('A'+i/26)) + string(rune('a'+i%26)) }

// The turn-end event: the idle state that ends a turn carries the turn's usage.
func TestSubscribe_TheTurnEndEventCarriesTheTurnsUsage(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3,
		step(tok(10, 1, nil, 0, 0), f64(0.1)), step(tok(20, 2, nil, 0, 0), f64(0.2))))
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", status(ref.ID, "busy"))
	if p := statePayload(t, next(t, s)); p.State.Usage != nil {
		t.Errorf("a busy event reports %+v; figures ride the turn's end", p.State.Usage)
	}
	f.emit("session.status", status(ref.ID, "idle"))
	p := statePayload(t, next(t, s))
	u := p.State.Usage
	if u == nil || u.Input != 30 || u.LastTurn == nil || u.LastTurn.Input != 30 || u.LastTurn.Cost == nil {
		t.Fatalf("turn-end event usage = %+v", u)
	}
}

func TestList_ReportsWhatAReadHasAlreadyLearnedWithoutAnotherRequest(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3, step(tok(10, 1, nil, 0, 0), nil)))

	list := func() fleet.Session {
		col, err := d.List(context.Background(), fleet.RequestFrom(fleet.Caller{}), driver.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return col.Items()[0]
	}
	if s := list(); s.State.Usage != nil {
		t.Errorf("before any read the listing has nothing to report, got %+v", s.State.Usage)
	}
	readUsage(t, d, ref.ID)
	if s := list(); s.State.Usage == nil || s.State.Usage.Input != 10 {
		t.Errorf("listing after a read = %+v", s.State.Usage)
	}
}
