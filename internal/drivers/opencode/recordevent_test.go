package opencode

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// session.record on this driver's stream (muster #444): the bus says the record
// moved, a window coalesces the hints, and the event's cursor is what the route's
// own reader answers as `next` — so it resumes after the last entry.

// growthHint is a bus event that means the record moved, shaped like the runtime's
// own (every one carries properties.sessionID).
func growthHint(typ, sessionID string) (string, map[string]any) {
	switch typ {
	case "message.updated":
		return typ, map[string]any{"sessionID": sessionID, "info": map[string]any{"id": "msg_a1", "sessionID": sessionID}}
	case "message.part.updated":
		return typ, map[string]any{"sessionID": sessionID, "part": map[string]any{"id": "prt_1", "sessionID": sessionID}, "time": 1}
	case "message.part.delta":
		return typ, map[string]any{"sessionID": sessionID, "messageID": "msg_a1", "partID": "prt_1", "field": "text", "delta": "x"}
	}
	return typ, map[string]any{"sessionID": sessionID, "messageID": "msg_a1", "partID": "prt_1"}
}

func recordPayload(t *testing.T, ev fleet.Event) fleet.SessionRecordPayload {
	t.Helper()
	if ev.Kind != fleet.EventSessionRecord {
		t.Fatalf("kind = %q, want session.record (payload %+v)", ev.Kind, ev.Payload)
	}
	p, ok := ev.Payload.(fleet.SessionRecordPayload)
	if !ok {
		t.Fatalf("payload of %s is %T, want SessionRecordPayload", ev.Kind, ev.Payload)
	}
	return p
}

// quiet asserts no event arrives for longer than two windows.
func quiet(t *testing.T, s driver.EventStream, why string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*recordWindow)
	defer cancel()
	if ev, err := s.Next(ctx); err == nil {
		t.Fatalf("%s: got an event: %+v", why, ev)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s: Next: %v", why, err)
	}
}

// The oracle: a burst produces exactly one event per window, carrying a cursor that
// resumes after the last entry; no content travels; a later append is the next
// window's event.
func TestSubscribe_RecordEventIsOnePerWindowWithACursorAfterTheLastEntry(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "first question"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 9, rSettled("first answer", 3, 4)))
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	// A burst of every kind of hint, inside one window.
	for i := 0; i < 6; i++ {
		f.emit(growthHint("message.part.delta", ref.ID))
	}
	for _, typ := range []string{"message.updated", "message.part.updated", "message.removed", "message.part.removed"} {
		f.emit(growthHint(typ, ref.ID))
	}

	p := recordPayload(t, next(t, s))
	if p.Ref.ID != ref.ID || p.Cursor == "" || p.StartedAt == nil {
		t.Fatalf("payload = %+v, want the session, a cursor and its start", p)
	}
	quiet(t, s, "the rest of the burst")

	// The cursor is the route's own `next`, and resumes after the last entry.
	if head := getRecord(t, d, ref, "", 0).Next; p.Cursor != head {
		t.Fatalf("event cursor %q != the route's next %q: one code path must decide what a cursor means", p.Cursor, head)
	}
	if got := getRecord(t, d, ref, p.Cursor, 0); len(got.Messages) != 0 {
		t.Fatalf("the event's cursor must resume AFTER the last entry; the route answered %q", shapes(got))
	}

	// A hint that changes nothing a client can read is not news: the cursor is the
	// same one, and a client told to pull would pull an empty page.
	f.emit(growthHint("message.part.delta", ref.ID))
	quiet(t, s, "growth that changed nothing readable")

	// The next append is the next window's event, and what the route returns after
	// the FIRST cursor.
	f.addMessage(ref.ID, tAsst("msg_a2", 10, 19, rSettled("second answer", 11, 12)))
	f.emit(growthHint("message.updated", ref.ID))
	q := recordPayload(t, next(t, s))
	if q.Cursor == p.Cursor {
		t.Fatal("a new entry produced the same cursor")
	}
	if got, want := shapes(getRecord(t, d, ref, p.Cursor, 0)), []string{"text/assistant:second answer"}; !eqStrings(got, want) {
		t.Fatalf("after the first cursor the route returned %q, want %q", got, want)
	}
	if got := getRecord(t, d, ref, q.Cursor, 0); len(got.Messages) != 0 {
		t.Fatalf("the second event's cursor must resume after the last entry; the route answered %q", shapes(got))
	}
}

// The payload is a reference and a cursor and nothing else.
func TestSubscribe_RecordEventCarriesNoContent(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "SECRET-USER-TEXT"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 9, rSettled("SECRET-ASSISTANT-TEXT", 3, 4)))
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit(growthHint("message.updated", ref.ID))
	ev := next(t, s)
	p := recordPayload(t, ev)
	if got := reflect.TypeOf(p).NumField(); got != 3 {
		t.Fatalf("SessionRecordPayload has %d fields, want 3 (ref, startedAt, cursor)", got)
	}
	for _, leak := range []string{"SECRET-USER-TEXT", "SECRET-ASSISTANT-TEXT"} {
		if s := mustJSON(t, ev); strings.Contains(s, leak) {
			t.Fatalf("the event carries record content %q: %s", leak, s)
		}
	}
}

// Hints about a session this driver does not hold, or one the subscription's filter
// excludes, say nothing: the same resolution every other event of this stream gets.
func TestSubscribe_RecordEventFollowsTheFilterAndTheSessionsThisDriverHolds(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q"))
	other := createOne(t, d, "/work/y", "k2")
	f.addMessage(other.ID, tUser("msg_u2", 1, "q"))

	s := subscribe(t, d, driver.SubscribeFilter{Sessions: []string{ref.ID}})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit(growthHint("message.part.delta", "ses_child"))
	f.emit(growthHint("message.updated", other.ID))
	quiet(t, s, "hints about a session that is not this subscription's")

	f.emit(growthHint("message.updated", ref.ID))
	if p := recordPayload(t, next(t, s)); p.Ref.ID != ref.ID {
		t.Fatalf("event for %q, want the filtered session", p.Ref.ID)
	}
}

// A subscription that ends with a window open produces nothing afterwards and holds
// nothing: the timer fires once, finds the stream over, and returns.
func TestSubscribe_ClosingWithAWindowOpenProducesNoEvent(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q"))
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit(growthHint("message.updated", ref.ID))
	// The window is open once the hint has been handled.
	sub := s.(*subscription)
	waitFor(t, "the window to open", func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		t := sub.track[ref.ID]
		return t != nil && t.recordArmed
	})
	_ = s.Close()
	time.Sleep(3 * recordWindow)
	if n := len(sub.out); n != 0 {
		t.Fatalf("%d events were queued after the subscription closed", n)
	}
}

// A session whose record cannot be read at the window's end — gone from the runtime —
// says nothing: there is no cursor to give, and no error leaves the stream.
func TestSubscribe_RecordEventForASessionThatVanishedSaysNothing(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q"))
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus connection", func() bool { return f.busConns() == 1 })

	f.emit(growthHint("message.updated", ref.ID))
	d.forgetSeen(ref.ID) // the driver stopped holding the session inside the window
	quiet(t, s, "a window that ends on a session the driver no longer holds")
}
