package opencode

import (
	"context"
	"strconv"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #485: a closed session's final usage outlives the runtime's own record
// of it. These tests drive the driver's half of that contract: what it keeps when
// a session is removed, for each way one is removed, and what it never invents.

// onModel stamps the provider and model an assistant message ran on.
func onModel(m wireMessage, provider, model string) wireMessage {
	m.Info.ProviderID, m.Info.ModelID = provider, model
	return m
}

func drained(t *testing.T, d *Driver) []driver.CapturedUsage {
	t.Helper()
	return d.DrainFinalUsage()
}

// The oracle: a session closed with no read of its usage before — the service
// restarted, say, and nothing asked — still leaves its whole summed figure behind.
func TestFinalUsage_ACloseKeepsTheSummedFigureEvenWhenNothingReadItBefore(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1000))
	f.setLastMessage(ref.ID, onModel(asstMsg("a1", 1001, 1010,
		step(tok(100, 10, i64(5), 1000, 50), f64(0.25)),
		step(tok(200, 20, i64(7), 2000, 0), f64(0.5)),
		step(tok(1, 2, i64(0), 3, 0), f64(0.01)), // a tiny last step: the sum is not the last
	), "anthropic", "model-a"))
	f.setLastMessage(ref.ID, onModel(asstMsg("a2", 1011, 1020,
		step(tok(10, 10, nil, 10, 10), nil)), "openai", "model-b"))

	if _, err := d.Close(context.Background(), testReq, ref); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := drained(t, d)
	if len(got) != 1 || got[0].ID != ref.ID {
		t.Fatalf("drain = %+v, want exactly the closed session", got)
	}
	u := got[0].Usage
	if u.Input != 311 || u.Output != 42 || u.CacheRead != 3013 || u.CacheWrite != 60 {
		t.Errorf("final = %+v, want the sum over every step: 311/42/3013/60", u)
	}
	if u.Reasoning == nil || *u.Reasoning != 12 {
		t.Errorf("reasoning = %v, want 12", u.Reasoning)
	}
	if u.Cost == nil || *u.Cost < 0.759 || *u.Cost > 0.761 {
		t.Errorf("cost = %v, want what the runtime reported, summed: 0.76", u.Cost)
	}
	if u.Provider != "openai" || u.Model != "model-b" {
		t.Errorf("provider/model = %q/%q, want the newest request's: openai/model-b", u.Provider, u.Model)
	}
	if u.Source != fleet.ConfidenceObserved {
		t.Errorf("source = %q, want observed", u.Source)
	}
	if u.AsOf.UnixMilli() != 1020 {
		t.Errorf("asOf = %v, want the newest counted message's own time", u.AsOf)
	}
	if again := drained(t, d); again != nil {
		t.Errorf("a second drain returned %+v; each figure is reported once", again)
	}
}

// A request that finished after the last read is in the figure: the close reads
// once more before it removes the session.
func TestFinalUsage_ACloseReadsOnceMoreSoALateRequestIsCounted(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3, step(tok(10, 1, nil, 0, 0), nil)))
	if u := readUsage(t, d, ref.ID).Usage; u == nil || u.Input != 10 {
		t.Fatalf("the read before = %+v", u)
	}

	// A request lands after the last read; the session is closed straight away.
	f.setLastMessage(ref.ID, asstMsg("a2", 4, 5, step(tok(90, 9, nil, 0, 0), nil)))
	if _, err := d.Close(context.Background(), testReq, ref); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := drained(t, d)
	if len(got) != 1 || got[0].Usage.Input != 100 || got[0].Usage.Output != 10 {
		t.Fatalf("drain = %+v, want the late request counted: 100/10", got)
	}
}

// A runtime that cannot answer the message read must not cost the close: the
// figure is what was last read, and the close is accepted.
func TestFinalUsage_AFailedReadFallsBackToWhatWasRemembered(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3, step(tok(10, 1, nil, 0, 0), nil)))
	readUsage(t, d, ref.ID)

	f.mu.Lock()
	f.messagesDown = true
	f.mu.Unlock()
	ack, err := d.Close(context.Background(), testReq, ref)
	if err != nil || !ack.Accepted {
		t.Fatalf("Close = %+v, %v; a failed usage read must not fail the close", ack, err)
	}
	got := drained(t, d)
	if len(got) != 1 || got[0].Usage.Input != 10 {
		t.Fatalf("drain = %+v, want the remembered figure", got)
	}
}

// No figure is not a zero figure.
func TestFinalUsage_ASessionThatNeverReportedKeepsNothing(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3)) // no tokens, no cost, no steps

	if _, err := d.Close(context.Background(), testReq, ref); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := drained(t, d); got != nil {
		t.Errorf("drain = %+v; a session with no reported usage has no final figure, not a zero one", got)
	}
}

// The runtime saying the session is gone ends it too, and the figure goes with it.
func TestFinalUsage_ASessionDeletedOnTheRuntimeKeepsWhatWasRemembered(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, asstMsg("a1", 2, 3, step(tok(10, 1, nil, 0, 0), f64(0.1))))
	readUsage(t, d, ref.ID)
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.deleted", map[string]any{"info": map[string]any{"id": ref.ID}})
	ev := next(t, s)
	p := statePayload(t, ev)
	if ev.Kind != fleet.EventSessionClosed || p.FinalUsage == nil || p.FinalUsage.Input != 10 {
		t.Fatalf("event = %s %+v, want session.closed carrying the figure", ev.Kind, p)
	}
	got := drained(t, d)
	if len(got) != 1 || got[0].ID != ref.ID || got[0].Usage.Input != 10 {
		t.Fatalf("drain = %+v", got)
	}
}

// The close event carries the same figure the closed record will.
func TestFinalUsage_TheCloseEventCarriesTheFigure(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	f.setLastMessage(ref.ID, onModel(asstMsg("a1", 2, 3,
		step(tok(10, 1, nil, 0, 0), f64(0.1)), step(tok(20, 2, nil, 0, 0), f64(0.2))), "p", "m"))
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	if _, err := d.Close(context.Background(), testReq, ref); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ev := next(t, s)
	p := statePayload(t, ev)
	if ev.Kind != fleet.EventSessionClosed || p.FinalUsage == nil {
		t.Fatalf("event = %s %+v, want session.closed carrying finalUsage", ev.Kind, p)
	}
	if u := p.FinalUsage; u.Input != 30 || u.Output != 3 || u.Provider != "p" || u.Model != "m" {
		t.Errorf("event figure = %+v", u)
	}
	if p.State.Usage != nil {
		t.Errorf("state.usage = %+v on a closed event; the final figure has its own key", p.State.Usage)
	}
	if got := drained(t, d); len(got) != 1 || got[0].Usage.Input != 30 {
		t.Errorf("drain after the event = %+v; the event must not consume the figure", got)
	}
}

// A close event for a session with no figure carries no key at all.
func TestFinalUsage_ACloseEventWithNoFigureCarriesNone(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	if _, err := d.Close(context.Background(), testReq, ref); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if p := statePayload(t, next(t, s)); p.FinalUsage != nil {
		t.Errorf("finalUsage = %+v for a session that reported nothing", p.FinalUsage)
	}
}

// The read before a removal has its own short budget, so it can never starve the
// removal: with nothing left of the close's own, it is not made at all.
func TestFinalUsage_TheReadIsNotMadeWhenNoTimeIsLeft(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	srv, _ := d.wasSeen(ref.ID)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	before := len(f.requestsSnapshot())
	d.readFinalUsage(ctx, srv.srv, ref.ID)
	for _, r := range f.requestsSnapshot()[before:] {
		if r.path == "/session/"+ref.ID+"/message" {
			t.Errorf("a message read was made with no time left: %+v", r)
		}
	}
}

// The queue is bounded: a driver nobody drains cannot grow without limit, and it
// is the oldest figure that goes.
func TestFinalUsage_TheQueueIsBoundedAndDropsTheOldest(t *testing.T) {
	d := newTestDriver(t, newFakeServer(t))
	for i := 0; i < maxFinals+10; i++ {
		d.keepFinalUsage("s"+strconv.Itoa(i), fleet.ClosedUsage{Input: int64(i)})
	}
	got := d.DrainFinalUsage()
	if len(got) != maxFinals {
		t.Fatalf("kept %d, want %d", len(got), maxFinals)
	}
	if got[0].ID != "s10" || got[len(got)-1].ID != "s"+strconv.Itoa(maxFinals+9) {
		t.Errorf("kept %s..%s, want the newest %d", got[0].ID, got[len(got)-1].ID, maxFinals)
	}
}
