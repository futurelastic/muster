package opencode

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/service"
)

// muster #426: an unfiltered listing carries the failure a session's newest turn
// ended in, with no request per idle session per call. The fake runtime counts the
// message reads it receives (the request a listing used to leave out), and a clock
// the test moves says how long after a write each listing is made.

func failedReply(message string) wireMessage {
	var e wireAssistantError
	e.Name = "APIError"
	e.Data.Message = message
	return wireMessage{Info: wireMessageInfo{Role: "assistant", Error: &e}}
}

func okReply() wireMessage { return wireMessage{Info: wireMessageInfo{Role: "assistant"}} }

func userPrompt() wireMessage { return wireMessage{Info: wireMessageInfo{Role: "user"}} }

// messageReadsOf counts GET /session/{id}/message requests the fake received.
func messageReadsOf(f *fakeServer, id string) int {
	return readsOf(f, "GET", "/session/"+id+"/message", true)
}

// uncachedDriver is a driver over f with the status cache off and a clock the test
// moves, so only the record under test decides what a listing does.
func uncachedDriver(t *testing.T, f *fakeServer) (*Driver, *stepClock) {
	t.Helper()
	clk := newStepClock()
	d, err := newDriverWithOptions(t, f, withClock(clk.now))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, clk
}

func sendInput(t *testing.T, d *Driver, ref fleet.SessionRef) {
	t.Helper()
	if _, err := d.Send(context.Background(), testReq, ref, "go", driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func rowOf(t *testing.T, d *Driver, ref fleet.SessionRef) fleet.SessionState {
	t.Helper()
	row, ok := listAll(t, d)[ref.ID]
	if !ok {
		t.Fatalf("the listing does not hold %s", ref.ID)
	}
	return row.State
}

func TestListing_AFailedTurnReadsFailedWithNothingSubscribed(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	clk.advance(writeSettle + time.Second)

	sendInput(t, d, ref)
	f.setLastMessage(ref.ID, userPrompt())
	f.setLastMessage(ref.ID, failedReply("payment required"))

	// Inside the settle the runtime may not have recorded the turn: nothing is read.
	if st := rowOf(t, d, ref); st.Status != fleet.StatusIdle || st.LastTurn != nil {
		t.Errorf("right after the Send the row = %s lastTurn %+v, want idle with none yet", st.Status, st.LastTurn)
	}
	if n := messageReadsOf(f, ref.ID); n != 0 {
		t.Errorf("a listing inside the settle made %d message reads, want 0", n)
	}

	clk.advance(writeSettle + time.Second)
	st := rowOf(t, d, ref)
	lt := st.LastTurn
	if st.Status != fleet.StatusIdle || lt == nil || lt.Outcome != "failed" ||
		!strings.Contains(lt.Reason, "APIError") || !strings.Contains(lt.Reason, "payment required") {
		t.Fatalf("row = %s lastTurn %+v, want idle with the failed turn and its reason", st.Status, lt)
	}
	if f.busConns() != 0 {
		t.Errorf("%d bus connections open; nothing subscribed", f.busConns())
	}
	if n := messageReadsOf(f, ref.ID); n != 1 {
		t.Errorf("the listing made %d message reads, want 1", n)
	}
	for i := 0; i < 10; i++ {
		if lt := rowOf(t, d, ref).LastTurn; lt == nil || lt.Outcome != "failed" {
			t.Fatalf("listing %d: lastTurn = %+v, want the failure still", i, lt)
		}
	}
	if n := messageReadsOf(f, ref.ID); n != 1 {
		t.Errorf("ten more listings made %d message reads in all, want 1", n)
	}
}

func TestListing_ASessionSeenWorkingIsReadWithoutWaitingOutTheSettle(t *testing.T) {
	f := newFakeServer(t)
	d, _ := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")

	sendInput(t, d, ref)
	f.setBusy(ref.ID)
	if st := rowOf(t, d, ref); st.Status != fleet.StatusWorking || st.LastTurn != nil {
		t.Fatalf("while busy the row = %s lastTurn %+v, want working with none", st.Status, st.LastTurn)
	}
	f.clearStatus(ref.ID)
	f.setLastMessage(ref.ID, failedReply("rate limited"))

	if lt := rowOf(t, d, ref).LastTurn; lt == nil || lt.Outcome != "failed" || !strings.Contains(lt.Reason, "rate limited") {
		t.Errorf("lastTurn = %+v, want the failure at once: the session was seen working", lt)
	}
}

func TestListing_ALaterTurnDropsTheFailure(t *testing.T) {
	f := newFakeServer(t)
	d, clk := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	sendInput(t, d, ref)
	f.setLastMessage(ref.ID, failedReply("payment required"))
	clk.advance(writeSettle + time.Second)
	if rowOf(t, d, ref).LastTurn == nil {
		t.Fatal("the failure was not shown")
	}

	// A new turn is sent: the old failure still reads until the runtime reports the
	// session working, never a healthy idle in between.
	sendInput(t, d, ref)
	if lt := rowOf(t, d, ref).LastTurn; lt == nil || lt.Outcome != "failed" {
		t.Errorf("right after the second Send lastTurn = %+v, want the previous failure", lt)
	}
	f.setBusy(ref.ID)
	if st := rowOf(t, d, ref); st.Status != fleet.StatusWorking || st.LastTurn != nil {
		t.Errorf("while the new turn runs the row = %s lastTurn %+v, want working with none", st.Status, st.LastTurn)
	}
	f.clearStatus(ref.ID)
	f.setLastMessage(ref.ID, okReply())
	if st := rowOf(t, d, ref); st.Status != fleet.StatusIdle || st.LastTurn != nil {
		t.Errorf("after a turn that succeeded the row = %s lastTurn %+v, want idle with none", st.Status, st.LastTurn)
	}
}

func TestListing_AWatchedSessionFeedsTheSameRecordWithNoReadOfItsOwn(t *testing.T) {
	f := newFakeServer(t)
	d, _ := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	// The runtime reports the failure on the bus; its message record says nothing.
	f.emit("session.status", status(ref.ID, "busy"))
	next(t, s)
	f.emit("session.error", map[string]any{"sessionID": ref.ID, "error": map[string]any{
		"name": "APIError", "data": map[string]any{"message": "payment required", "isRetryable": false}}})
	f.emit("session.status", status(ref.ID, "idle"))
	next(t, s)

	before := messageReadsOf(f, ref.ID)
	lt := rowOf(t, d, ref).LastTurn
	if lt == nil || lt.Outcome != "failed" || !strings.Contains(lt.Reason, "payment required") {
		t.Errorf("lastTurn = %+v, want the failure the bus reported", lt)
	}
	if n := messageReadsOf(f, ref.ID) - before; n != 0 {
		t.Errorf("the listing made %d message reads, want 0: the stream had settled the turn", n)
	}

	// A later turn that ends well replaces it.
	f.emit("session.status", status(ref.ID, "busy"))
	next(t, s)
	f.setLastMessage(ref.ID, okReply())
	f.emit("session.status", status(ref.ID, "idle"))
	next(t, s)
	if lt := rowOf(t, d, ref).LastTurn; lt != nil {
		t.Errorf("lastTurn = %+v after a turn that succeeded, want none", lt)
	}
}

func TestListing_ASessionNothingHasHappenedToIsNotReadAndAStateReadHealsTheRecord(t *testing.T) {
	f := newFakeServer(t)
	d, _ := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	// As after a restart: the runtime's record holds a failure this driver never saw.
	f.setLastMessage(ref.ID, failedReply("payment required"))

	if lt := rowOf(t, d, ref).LastTurn; lt != nil {
		t.Errorf("lastTurn = %+v for a session no turn was seen on, want none (the record is empty)", lt)
	}
	if n := messageReadsOf(f, ref.ID); n != 0 {
		t.Errorf("the listing made %d message reads for a session nothing happened to, want 0", n)
	}

	st, err := d.State(context.Background(), testReq, ref)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.LastTurn == nil || st.LastTurn.Outcome != "failed" {
		t.Fatalf("State lastTurn = %+v, want the failure: the read by id is the authority", st.LastTurn)
	}
	before := messageReadsOf(f, ref.ID)
	if lt := rowOf(t, d, ref).LastTurn; lt == nil || lt.Outcome != "failed" {
		t.Errorf("listing lastTurn = %+v after the State read, want the failure it found", lt)
	}
	if n := messageReadsOf(f, ref.ID) - before; n != 0 {
		t.Errorf("the listing made %d message reads, want 0", n)
	}
}

func TestListing_IdleSessionsCostNoMessageReads(t *testing.T) {
	f := newFakeServer(t)
	d, clk := uncachedDriver(t, f)
	var refs []fleet.SessionRef
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		ref := createOne(t, d, "/work/"+key, key)
		f.setLastMessage(ref.ID, failedReply("payment required"))
		refs = append(refs, ref)
	}
	for i := 0; i < 5; i++ {
		clk.advance(writeSettle + time.Second)
		listAll(t, d)
	}
	for _, ref := range refs {
		if n := messageReadsOf(f, ref.ID); n != 0 {
			t.Errorf("session %s: %d message reads over five listings, want 0", ref.ID, n)
		}
	}
}

func TestListing_AFailedReadShowsNothingNewAndIsTriedAgain(t *testing.T) {
	f := newFakeServer(t)
	d, clk := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	sendInput(t, d, ref)
	f.setLastMessage(ref.ID, failedReply("payment required"))
	f.mu.Lock()
	f.messagesDown = true
	f.mu.Unlock()

	clk.advance(writeSettle + time.Second)
	if st := rowOf(t, d, ref); st.Status != fleet.StatusIdle || st.LastTurn != nil {
		t.Errorf("with the message read failing the row = %s lastTurn %+v, want idle with none (not a guess)", st.Status, st.LastTurn)
	}
	// Not tried again inside the settle...
	before := messageReadsOf(f, ref.ID)
	rowOf(t, d, ref)
	if n := messageReadsOf(f, ref.ID) - before; n != 0 {
		t.Errorf("a second listing at once made %d message reads, want 0", n)
	}
	// ...and tried again after it, once the runtime answers.
	f.mu.Lock()
	f.messagesDown = false
	f.mu.Unlock()
	clk.advance(writeSettle + time.Second)
	if lt := rowOf(t, d, ref).LastTurn; lt == nil || lt.Outcome != "failed" {
		t.Errorf("lastTurn = %+v after the runtime answered, want the failure", lt)
	}
}

func TestListing_ASessionWhoseTurnIsNeverRecordedIsGivenUpOn(t *testing.T) {
	f := newFakeServer(t)
	d, clk := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	sendInput(t, d, ref)
	f.setLastMessage(ref.ID, userPrompt()) // the prompt is recorded, its reply never is

	for i := 0; i < 14; i++ {
		clk.advance(3 * time.Second)
		rowOf(t, d, ref)
	}
	reads := messageReadsOf(f, ref.ID)
	if reads < 2 {
		t.Fatalf("%d message reads over 42 s, want the listing to have retried", reads)
	}
	for i := 0; i < 3; i++ {
		clk.advance(3 * time.Second)
		rowOf(t, d, ref)
	}
	if n := messageReadsOf(f, ref.ID); n != reads {
		t.Errorf("%d message reads after the give-up, want it to stop at %d", n, reads)
	}
}

func TestListing_ARecordWrittenUnderAnOlderTurnIsDropped(t *testing.T) {
	f := newFakeServer(t)
	d, _ := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	d.turnStarted(ref.ID)
	gen := d.turnGen(ref.ID)
	d.sawWorking(ref.ID) // a newer turn starts while the read for the old one is out

	d.noteTurn(ref.ID, gen, turnRead{turn: &fleet.TurnEnd{Outcome: "failed", Reason: "old"}, settled: true}, true)
	if lt := d.recordedTurn(ref.ID); lt != nil {
		t.Errorf("recorded = %+v, want the read of the older turn dropped", lt)
	}
	// Within one turn a read that finds nothing does not remove a failure already kept.
	gen = d.turnGen(ref.ID)
	d.noteTurn(ref.ID, gen, turnRead{turn: &fleet.TurnEnd{Outcome: "failed", Reason: "bus"}, settled: true}, true)
	d.noteTurn(ref.ID, gen, turnRead{settled: true}, false)
	if lt := d.recordedTurn(ref.ID); lt == nil || lt.Reason != "bus" {
		t.Errorf("recorded = %+v, want the failure the bus reported to survive a read that shows none", lt)
	}
}

func TestListing_SendsAndListingsRacingEachOtherAreSafe(t *testing.T) {
	f := newFakeServer(t)
	d, clk := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, failedReply("payment required"))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			sendInput(t, d, ref)
			f.setBusy(ref.ID)
			f.clearStatus(ref.ID)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			clk.advance(writeSettle)
			_, _ = d.List(context.Background(), testReq, driver.ListFilter{})
		}
	}()
	wg.Wait()
}

func TestListing_ThroughTheServiceTheRowCarriesTheFailure(t *testing.T) {
	f := newFakeServer(t)
	d, clk := uncachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	svc := service.New("test-machine")
	if err := svc.RegisterLocalDriver(DefaultRuntime, d); err != nil {
		t.Fatal(err)
	}
	sendInput(t, d, ref)
	f.setLastMessage(ref.ID, failedReply("payment required"))
	clk.advance(writeSettle + time.Second)

	col, err := svc.ListSessions(context.Background(), testReq, service.ScopeLocal, driver.ListFilter{}, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var found bool
	for _, s := range col.Items() {
		if s.ID != ref.ID {
			continue
		}
		found = true
		if lt := s.State.LastTurn; s.State.Status != fleet.StatusIdle || lt == nil || lt.Outcome != "failed" {
			t.Errorf("service row = %s lastTurn %+v, want idle with the failed turn", s.State.Status, lt)
		}
	}
	if !found {
		t.Fatal("the service listing does not hold the session")
	}
}
