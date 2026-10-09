package opencode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/service"
)

// muster #284: Subscribe over the runtime's own event bus. The fake server's
// GET /event (fake_server_test.go) is the oracle for the mapping; the stand-in
// process (fakeopencode_test.go) is for what only a real process can show — a
// log on stderr, a connection that ends because the process did.

const wait = 5 * time.Second

func subscribe(t *testing.T, d *Driver, filter driver.SubscribeFilter) driver.EventStream {
	t.Helper()
	s, err := d.Subscribe(context.Background(), testReq, filter)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// next reads one event or fails the test.
func next(t *testing.T, s driver.EventStream) fleet.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

// silent asserts nothing more arrives for a short while.
func silent(t *testing.T, s driver.EventStream, why string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if ev, err := s.Next(ctx); err == nil {
		t.Fatalf("%s: got an event: %+v", why, ev)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s: Next: %v", why, err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func statePayload(t *testing.T, ev fleet.Event) fleet.SessionStatePayload {
	t.Helper()
	p, ok := ev.Payload.(fleet.SessionStatePayload)
	if !ok {
		t.Fatalf("payload of %s is %T, want SessionStatePayload", ev.Kind, ev.Payload)
	}
	return p
}

func status(id, typ string) map[string]any {
	return map[string]any{"sessionID": id, "status": map[string]any{"type": typ}}
}

func TestSubscribe_AStatusChangeAndATurnEndArriveAsStateEvents(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", status(ref.ID, "busy"))
	ev := next(t, s)
	if ev.Kind != fleet.EventSessionState || ev.Machine != "test-machine" {
		t.Fatalf("event = %s on %q, want session.state on the driver's machine", ev.Kind, ev.Machine)
	}
	p := statePayload(t, ev)
	if p.Ref.ID != ref.ID || p.State.Status != fleet.StatusWorking {
		t.Errorf("busy -> %+v, want working on %s", p, ref.ID)
	}
	if ev.Cursor != 0 || ev.Epoch != "" {
		t.Errorf("the driver stamped cursor %d epoch %q; those are the service's (§7.3)", ev.Cursor, ev.Epoch)
	}

	f.emit("session.status", status(ref.ID, "idle"))
	p = statePayload(t, next(t, s))
	if p.State.Status != fleet.StatusIdle || p.State.LastTurn != nil {
		t.Errorf("idle -> %+v, want idle with no failed turn", p.State)
	}
}

func TestSubscribe_ARetryIsWorkingWithARetryableTurn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", map[string]any{"sessionID": ref.ID, "status": map[string]any{"type": "retry", "attempt": 2, "message": "rate limited"}})
	p := statePayload(t, next(t, s))
	if p.State.Status != fleet.StatusWorking || p.State.LastTurn == nil || !p.State.LastTurn.Retryable {
		t.Errorf("retry -> %+v, want working + a retryable turn (same as State)", p.State)
	}
}

func TestSubscribe_TheRuntimeReportsATurnEndTwiceAndItIsNewsOnce(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", status(ref.ID, "busy"))
	f.emit("session.status", status(ref.ID, "busy")) // unchanged
	f.emit("session.status", status(ref.ID, "idle"))
	f.emit("session.idle", map[string]any{"sessionID": ref.ID}) // the older spelling of the same fact
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusWorking {
		t.Fatalf("first = %s, want working", got)
	}
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusIdle {
		t.Fatalf("second = %s, want idle", got)
	}
	silent(t, s, "a repeat of an unchanged state")
}

func TestSubscribe_WhatHasNoHonestMappingIsDroppedNotInvented(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	// A message update carries a MESSAGE under info.id; it must not be read as a session.
	f.emit("message.updated", map[string]any{"info": map[string]any{"id": ref.ID, "sessionID": ref.ID}})
	f.emit("message.part.updated", map[string]any{"part": map[string]any{"sessionID": ref.ID}})
	f.emit("session.updated", map[string]any{"info": map[string]any{"id": ref.ID, "title": "renamed by the runtime"}})
	// A question prompt is not a permission ask: Respond is unsupported, and
	// nothing here claims to see it (muster #289 maps permission.* only).
	f.emit("question.asked", map[string]any{"sessionID": ref.ID})
	f.emit("file.edited", map[string]any{"file": "/x"})
	f.emit("server.heartbeat", map[string]any{})
	// A session this driver did not create: a child the runtime started for itself.
	f.emit("session.status", status("ses_child", "busy"))
	// A status type nobody here knows.
	f.emit("session.status", status(ref.ID, "paused"))
	silent(t, s, "events with no honest mapping")

	f.emit("session.status", status(ref.ID, "busy"))
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusWorking {
		t.Errorf("after the dropped ones, busy -> %s, want working", got)
	}
}

// setAsks scripts the asks GET /permission lists.
func (f *fakeServer) setAsks(asks ...wirePermissionAsk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissions = asks
}

func (f *fakeServer) permissionReads() int {
	n := 0
	for _, r := range f.requestsSnapshot() {
		if r.path == "/permission" {
			n++
		}
	}
	return n
}

// muster #289: a session parked on a permission ask reads as waiting_input on
// the stream, as it does to State, whichever spelling the runtime raises.
func TestSubscribe_AParkedPermissionAskIsOneWaitingInputState(t *testing.T) {
	for _, event := range []string{"permission.asked", "permission.updated"} {
		t.Run(event, func(t *testing.T) {
			f := newFakeServer(t)
			d := newTestDriver(t, f)
			ref := createOne(t, d, "/work/x", "k1")
			s := subscribe(t, d, driver.SubscribeFilter{})
			waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

			f.setBusy(ref.ID)
			f.emit("session.status", status(ref.ID, "busy"))
			if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusWorking {
				t.Fatalf("first = %s, want working", got)
			}

			f.setAsks(wirePermissionAsk{ID: "per_1", SessionID: ref.ID, Permission: "bash", Patterns: []string{"rm -rf build"}})
			f.emit(event, map[string]any{"sessionID": ref.ID})
			p := statePayload(t, next(t, s))
			st := p.State
			if p.Ref.ID != ref.ID || st.Status != fleet.StatusWaitingInput || st.WaitingOn != fleet.WaitingPrompt ||
				st.Prompt == nil || st.Prompt.Kind != fleet.PromptToolPermission || len(st.Prompt.Options) != 0 {
				t.Fatalf("state = %+v, want waiting_input with a tool-permission prompt and no options", st)
			}
			// What State says about the same session, the same moment.
			if want := stateOf(t, d, ref.ID); want.Status != st.Status || want.Prompt.Question != st.Prompt.Question {
				t.Errorf("the stream says %+v, State says %+v", st, want)
			}

			// The same ask announced again is not news.
			f.emit(event, map[string]any{"sessionID": ref.ID})
			silent(t, s, "the same ask raised twice")
		})
	}
}

func TestSubscribe_AnAnsweredAskReportsTheSessionsNextState(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.setBusy(ref.ID)
	f.setAsks(wirePermissionAsk{ID: "per_1", SessionID: ref.ID, Permission: "edit"})
	f.emit("permission.asked", map[string]any{"sessionID": ref.ID})
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusWaitingInput {
		t.Fatalf("ask -> %s, want waiting_input", got)
	}

	// Answered: the turn carries on.
	f.setAsks()
	f.emit("permission.replied", map[string]any{"sessionID": ref.ID})
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusWorking {
		t.Fatalf("answered -> %s, want working", got)
	}

	// Asked again, then withdrawn because the turn ended (aborted).
	f.setAsks(wirePermissionAsk{ID: "per_2", SessionID: ref.ID, Permission: "bash"})
	f.emit("permission.asked", map[string]any{"sessionID": ref.ID})
	if p := statePayload(t, next(t, s)).State; p.Status != fleet.StatusWaitingInput || p.Prompt.Nonce != "per_2" {
		t.Fatalf("second ask -> %+v, want waiting_input on per_2", p)
	}
	f.setAsks()
	f.clearStatus(ref.ID)
	f.emit("permission.replied", map[string]any{"sessionID": ref.ID})
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusIdle {
		t.Fatalf("withdrawn with the turn over -> %s, want idle", got)
	}
}

// An ask for another session is not this one's, and a quiet session costs nothing.
func TestSubscribe_OnlyPermissionEventsCostAPermissionRead(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.setBusy(ref.ID)
	f.emit("session.status", status(ref.ID, "busy"))
	next(t, s)
	f.emit("message.part.updated", map[string]any{"part": map[string]any{"sessionID": ref.ID}})
	f.emit("server.heartbeat", map[string]any{})
	f.clearStatus(ref.ID)
	f.emit("session.status", status(ref.ID, "idle"))
	next(t, s)
	silent(t, s, "a quiet session")
	if n := f.permissionReads(); n != 0 {
		t.Errorf("%d permission reads with no permission event, want 0", n)
	}

	// A permission event for a session found idle, never reported otherwise, is
	// no change to announce.
	f.emit("permission.replied", map[string]any{"sessionID": ref.ID})
	silent(t, s, "a reply for a session already reported idle")
}

func TestSubscribe_AnUnreadableStateAfterAPermissionEventEndsTheStreamAsAGap(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.mu.Lock()
	f.statusDown = true
	f.mu.Unlock()
	f.emit("permission.asked", map[string]any{"sessionID": ref.ID})
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, err := s.Next(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next = %v, want the stream to end with an error (a gap), not silence", err)
	}
}

func TestSubscribe_AFailureRidesTheIdleStateThatEndsItsTurn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", status(ref.ID, "busy"))
	next(t, s)
	f.emit("session.error", map[string]any{"sessionID": ref.ID, "error": map[string]any{
		"name": "APIError", "data": map[string]any{"message": "payment required", "isRetryable": false}}})
	silent(t, s, "a failure mid-turn waits for the turn to end")
	f.emit("session.status", status(ref.ID, "idle"))

	p := statePayload(t, next(t, s))
	lt := p.State.LastTurn
	if p.State.Status != fleet.StatusIdle || lt == nil || lt.Outcome != "failed" ||
		!strings.Contains(lt.Reason, "APIError") || !strings.Contains(lt.Reason, "payment required") || lt.Retryable {
		t.Errorf("state = %+v (lastTurn %+v), want idle with a failed turn naming APIError", p.State, lt)
	}
}

func TestSubscribe_AFailureBeforeAnyTurnIsReportedAtOnce(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.error", map[string]any{"sessionID": ref.ID, "error": map[string]any{"name": "UnknownError", "data": map[string]any{"message": "Unexpected server error"}}})
	p := statePayload(t, next(t, s))
	if p.State.Status != fleet.StatusIdle || p.State.LastTurn == nil || p.State.LastTurn.Outcome != "failed" {
		t.Errorf("state = %+v, want idle with a failed turn", p.State)
	}
}

func TestSubscribe_TheIdleStateReadsTheMessageRecordWhenNoFailureWasReported(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	var e wireAssistantError
	e.Name = "ProviderAuthError"
	e.Data.Message = "no key"
	f.setLastMessage(ref.ID, wireMessage{Info: wireMessageInfo{Role: "assistant", Error: &e}})
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", status(ref.ID, "idle"))
	lt := statePayload(t, next(t, s)).State.LastTurn
	if lt == nil || !strings.Contains(lt.Reason, "ProviderAuthError") {
		t.Errorf("lastTurn = %+v, want the message record's failure, as State reports it", lt)
	}
}

func TestSubscribe_ADroppedBusConnectionEndsTheStreamWithAnError(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", status(ref.ID, "busy"))
	f.cutBus()

	// What came before the gap is delivered first, then the gap itself.
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusWorking {
		t.Fatalf("event before the cut = %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, err := s.Next(ctx)
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnreachable || !fe.Retryable {
		t.Fatalf("Next after the bus dropped = %v, want a retryable unreachable error (a gap, not silence)", err)
	}
}

func TestSubscribe_ARuntimeWithNoBusIsUnsupported_AnAuthFailureIsNot(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	createOne(t, d, "/work/x", "k1")

	f.mu.Lock()
	f.busStatus = http.StatusNotFound
	f.mu.Unlock()
	if _, err := d.Subscribe(context.Background(), testReq, driver.SubscribeFilter{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a 404 bus: err = %v, want ErrUnsupported", err)
	}

	f.mu.Lock()
	f.busStatus = http.StatusUnauthorized
	f.mu.Unlock()
	_, err := d.Subscribe(context.Background(), testReq, driver.SubscribeFilter{})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnauthorized {
		t.Errorf("a 401 bus: err = %v, want unauthorized (and not the permanent ErrUnsupported)", err)
	}
}

func TestSubscribe_TheFilterNarrowsWhichSessionsPass(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	a := createOne(t, d, "/work/a", "ka")
	b := createOne(t, d, "/other/b", "kb")

	byID := subscribe(t, d, driver.SubscribeFilter{Sessions: []string{b.ID}})
	byCwd := subscribe(t, d, driver.SubscribeFilter{CwdPrefix: "/work"})
	waitFor(t, "both connections", func() bool { return f.busConns() == 2 })

	f.emit("session.status", status(a.ID, "busy"))
	f.emit("session.status", status(b.ID, "busy"))
	if got := statePayload(t, next(t, byID)).Ref.ID; got != b.ID {
		t.Errorf("by id: got %s, want only %s", got, b.ID)
	}
	if got := statePayload(t, next(t, byCwd)).Ref.ID; got != a.ID {
		t.Errorf("by cwd: got %s, want only %s", got, a.ID)
	}
	silent(t, byID, "the other session")
	silent(t, byCwd, "the other directory")
}

func TestSubscribe_ASubscriptionThatMatchesNothingOpensNoConnection(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	createOne(t, d, "/work/a", "ka")
	subscribe(t, d, driver.SubscribeFilter{Sessions: []string{"ses_nobody"}})
	if n := f.busConns(); n != 0 {
		t.Errorf("%d bus connections for a filter naming no session; the cost of watching is what the filter asked for", n)
	}
}

func TestSubscribe_ACreateAndACloseAreEvents(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	s := subscribe(t, d, driver.SubscribeFilter{}) // nothing to watch yet
	if n := f.busConns(); n != 0 {
		t.Fatalf("%d connections with no session", n)
	}

	ref := createOne(t, d, "/work/x", "k1")
	ev := next(t, s)
	sess, ok := ev.Payload.(fleet.Session)
	if ev.Kind != fleet.EventSessionCreated || !ok || sess.ID != ref.ID || sess.Cwd != "/work/x" || sess.StartedAt == nil {
		t.Fatalf("event = %s %+v, want session.created for %s", ev.Kind, ev.Payload, ref.ID)
	}
	if f.busConns() != 1 {
		t.Errorf("the new session's server was not attached (%d connections)", f.busConns())
	}

	f.emit("session.status", status(ref.ID, "busy"))
	if got := statePayload(t, next(t, s)).State.Status; got != fleet.StatusWorking {
		t.Errorf("a session created after Subscribe: busy -> %s", got)
	}

	if _, err := d.Close(context.Background(), testReq, ref); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ev = next(t, s)
	if p := statePayload(t, ev); ev.Kind != fleet.EventSessionClosed || p.Ref.ID != ref.ID || p.State.Status != fleet.StatusDead {
		t.Errorf("event = %s %+v, want session.closed dead for %s", ev.Kind, p, ref.ID)
	}
	silent(t, s, "after a close")
}

func TestSubscribe_ASessionDeletedOnTheRuntimeIsClosedAndForgotten(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.deleted", map[string]any{"info": map[string]any{"id": ref.ID}})
	ev := next(t, s)
	if ev.Kind != fleet.EventSessionClosed || statePayload(t, ev).Ref.ID != ref.ID {
		t.Fatalf("event = %s, want session.closed for %s", ev.Kind, ref.ID)
	}
	if _, ok := d.wasSeen(ref.ID); ok {
		t.Error("the runtime said the session is gone and the driver still lists it (#78)")
	}
}

func TestSubscribe_Shutdown_EndsTheStream(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	createOne(t, d, "/work/x", "k1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	_ = d.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, err := s.Next(ctx); err == nil {
		t.Fatal("Next succeeded after Shutdown")
	}
	if _, err := d.Subscribe(context.Background(), testReq, driver.SubscribeFilter{}); err == nil {
		t.Error("Subscribe succeeded on a shut-down driver")
	}
}

// --- through the service: cursor, epoch, and the gap marker -------------------

func recvEvent(t *testing.T, ch <-chan fleet.Event) fleet.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a service event")
		return fleet.Event{}
	}
}

func TestSubscribe_ThroughTheService_EventsCarryCursorAndEpoch_AndABusDropIsAGap(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")

	svc := service.New("test-machine")
	if err := svc.RegisterLocalDriver(DefaultRuntime, d); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _, _, release := svc.Events(ctx, service.ScopeLocal, driver.SubscribeFilter{}, 0, "")
	// The service keeps its driver-side stream a minute after the last
	// subscriber leaves; shutting the driver ends it now instead of holding
	// the fake server's connection open for that minute.
	defer func() { release(); _ = d.Shutdown() }()
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit("session.status", status(ref.ID, "busy"))
	f.emit("session.status", status(ref.ID, "idle"))
	busy, idle := recvEvent(t, ch), recvEvent(t, ch)
	if busy.Kind != fleet.EventSessionState || idle.Kind != fleet.EventSessionState {
		t.Fatalf("kinds = %s, %s; want session.state twice", busy.Kind, idle.Kind)
	}
	if busy.Cursor == 0 || idle.Cursor != busy.Cursor+1 || busy.Epoch == "" || busy.Epoch != svc.Epoch() {
		t.Errorf("cursor/epoch = %d/%q then %d/%q, want consecutive cursors under epoch %q",
			busy.Cursor, busy.Epoch, idle.Cursor, idle.Epoch, svc.Epoch())
	}

	// The runtime's bus drops. The service must tell its subscribers, in the
	// sequence, rather than let quiet read as idleness.
	f.cutBus()
	var sawDegraded, sawResync bool
	for !sawResync {
		ev := recvEvent(t, ch)
		switch ev.Kind {
		case fleet.EventSourceStatus:
			if st, ok := ev.Payload.(fleet.SourceStatus); ok && st.Status == fleet.SourceDegraded {
				sawDegraded = true
			}
		case fleet.EventControlResync:
			if p, ok := ev.Payload.(fleet.ControlResyncPayload); ok && p.Reason == fleet.ResyncFeedGap {
				sawResync = true
			}
		}
	}
	if !sawDegraded {
		t.Error("the feed gap was announced without the machine ever being reported degraded")
	}

	// And the feed is back: the next change arrives.
	waitFor(t, "the bus to be re-opened", func() bool { return f.busConns() == 1 })
	f.emit("session.status", status(ref.ID, "busy"))
	for {
		if ev := recvEvent(t, ch); ev.Kind == fleet.EventSessionState {
			if statePayload(t, ev).State.Status != fleet.StatusWorking {
				t.Errorf("after the gap: %+v", ev.Payload)
			}
			break
		}
	}
}

// --- the framing --------------------------------------------------------------

func TestReadSSE_FramesEventsAndSkipsWhatItShould(t *testing.T) {
	body := strings.Join([]string{
		": a comment",
		"event: ignored-field",
		"data: {\"a\":1}",
		"",
		"data: line one",
		"data: line two",
		"",
		"data:no-space",
		"",
		"data: " + strings.Repeat("x", 200), // over the bound: skipped, not decoded
		"",
		"id: 7",
		"", // an event with no data
		"data: after\r",
		"\r",
		"",
	}, "\n")
	var got []string
	err := readSSE(strings.NewReader(body), 64, func(data []byte) { got = append(got, string(data)) })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want a clean end", err)
	}
	want := []string{`{"a":1}`, "line one\nline two", "no-space", "after"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("events = %q, want %q", got, want)
	}
}

func TestReadSSE_ALineLongerThanTheReaderBufferIsOneLine(t *testing.T) {
	long := strings.Repeat("y", 200<<10)
	var got [][]byte
	_ = readSSE(bytes.NewReader([]byte("data: "+long+"\n\n")), 1<<20, func(d []byte) { got = append(got, append([]byte(nil), d...)) })
	if len(got) != 1 || string(got[0]) != long {
		t.Errorf("got %d events (first %d bytes), want the one long event intact", len(got), func() int {
			if len(got) == 0 {
				return 0
			}
			return len(got[0])
		}())
	}
}

// --- the log capture ------------------------------------------------------------

func TestLogCapture_ErrorNameSeesPastTheGenericPlaceholder(t *testing.T) {
	l := newLogCapture()
	start := time.Now()
	_, _ = l.Write([]byte("INFO  starting\nERROR service=llm UnknownError Unexpected server error\n"))
	if got := l.errorName(start); got != "" {
		t.Errorf("errorName = %q from a line naming only UnknownError", got)
	}
	_, _ = l.Write([]byte("ERROR service=provider err=ProviderModelNotFoundError: model x\nWARN later AbortError"))
	// The last write ended without a newline: that line is not complete yet.
	if got := l.errorName(start); got != "ProviderModelNotFoundError" {
		t.Errorf("errorName = %q, want ProviderModelNotFoundError", got)
	}
	_, _ = l.Write([]byte("\n"))
	if got := l.errorName(start); got != "AbortError" {
		t.Errorf("errorName = %q, want the most recent specific name", got)
	}
	if got := l.errorName(start.Add(time.Hour)); got != "" {
		t.Errorf("errorName = %q for a window after every line", got)
	}
}

func TestLogCapture_IsBounded(t *testing.T) {
	l := newLogCapture()
	for i := 0; i < logKeepLines*3; i++ {
		_, _ = l.Write([]byte("line\n"))
	}
	_, _ = l.Write([]byte(strings.Repeat("z", logMaxLine*4) + "\n"))
	l.mu.Lock()
	n, longest := len(l.lines), 0
	for _, ln := range l.lines {
		if len(ln.text) > longest {
			longest = len(ln.text)
		}
	}
	l.mu.Unlock()
	if n > logKeepLines || longest > logMaxLine {
		t.Errorf("kept %d lines, longest %d; bounds are %d and %d", n, longest, logKeepLines, logMaxLine)
	}
	var nilCapture *logCapture
	if _, err := nilCapture.Write([]byte("x")); err != nil || nilCapture.errorName(time.Now()) != "" {
		t.Error("a nil capture (shared mode) must be inert")
	}
}
