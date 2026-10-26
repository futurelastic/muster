package opencode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Synthetic fixtures shaped like the runtime's message record, built as the
// runtime would send them (map form), because the production wire types cannot
// carry a part's text on purpose. The shapes follow what a live runtime was
// measured to send: a user message with one text part; an assistant message
// whose parts are step-start, reasoning, text, tool and step-finish; one prompt
// that uses a tool answered by TWO assistant messages.

func tText(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func tPart(typ string, extra map[string]any) map[string]any {
	p := map[string]any{"type": typ}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func tUser(id string, created int64, text string) map[string]any {
	return map[string]any{
		"info":  map[string]any{"id": id, "role": "user", "time": map[string]any{"created": created}, "agent": "build"},
		"parts": []any{tText(text)},
	}
}

// tAsst is a finished assistant message from a real model; completed 0 leaves
// the completion time off, like a message the runtime is still writing.
func tAsst(id string, created, completed int64, parts ...map[string]any) map[string]any {
	tm := map[string]any{"created": created}
	if completed != 0 {
		tm["completed"] = completed
	}
	ps := make([]any, 0, len(parts))
	for _, p := range parts {
		ps = append(ps, p)
	}
	return map[string]any{
		"info":  map[string]any{"id": id, "role": "assistant", "modelID": "big-pickle", "providerID": "opencode", "time": tm},
		"parts": ps,
	}
}

func withInfo(m map[string]any, k string, v any) map[string]any {
	m["info"].(map[string]any)[k] = v
	return m
}

func getTurns(t *testing.T, d *Driver, ref fleet.SessionRef, since string, limit int) fleet.TurnsPage {
	t.Helper()
	page, err := d.Turns(context.Background(), fleet.RequestFrom(fleet.Caller{}), ref, fleet.TurnsQuery{Since: since, Limit: limit})
	if err != nil {
		t.Fatalf("Turns(since=%q, limit=%d): %v", since, limit, err)
	}
	if page.Turns == nil {
		t.Fatal("Turns returned a nil slice; an empty page is [] on the wire")
	}
	return page
}

func texts(p fleet.TurnsPage) []string {
	out := make([]string, 0, len(p.Turns))
	for _, tr := range p.Turns {
		out = append(out, tr.Text)
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// addTurn scripts one finished question-and-answer pair.
func addTurn(f *fakeServer, id string, n int, answer string) {
	f.addMessage(id, tUser(fmt.Sprintf("msg_u%04d", n), int64(n*10), fmt.Sprintf("question %d", n)))
	f.addMessage(id, tAsst(fmt.Sprintf("msg_a%04d", n), int64(n*10+1), int64(n*10+2), tText(answer)))
}

// The oracle of the issue: two prompts, each answered by a multi-part assistant
// message, yield two turns whose text is the text parts, in order — and nothing
// the message held besides text.
func TestTurns_OneTurnPerAssistantMessageWithItsTextPartsJoined(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")

	f.addMessage(ref.ID, tUser("msg_u1", 1000, "LEAK-prompt-1"))
	f.addMessage(ref.ID, tAsst("msg_a1", 1001, 1900,
		tPart("step-start", nil),
		tPart("reasoning", map[string]any{"text": "LEAK-reasoning-1"}),
		tText("alpha"),
		tPart("tool", map[string]any{"tool": "read", "state": map[string]any{"status": "completed", "output": "LEAK-tool-output-1"}}),
		tText("beta"),
		tPart("step-finish", map[string]any{"reason": "stop"}),
	))
	f.addMessage(ref.ID, tUser("msg_u2", 2000, "LEAK-prompt-2"))
	f.addMessage(ref.ID, tAsst("msg_a2", 2001, 2900,
		tPart("step-start", nil),
		tText("gamma"),
		tPart("reasoning", map[string]any{"text": "LEAK-reasoning-2"}),
		tText("delta"),
		tPart("step-finish", map[string]any{"reason": "stop"}),
	))

	page := getTurns(t, d, ref, "", 0)
	if got, want := texts(page), []string{"alpha\n\nbeta", "gamma\n\ndelta"}; !eqStrings(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
	if got := page.Turns[0].At; !got.Equal(time.UnixMilli(1900)) || got.Location() != time.UTC {
		t.Errorf("at = %v, want the message's completion time in UTC", got)
	}
	for _, tr := range page.Turns {
		if strings.Contains(tr.Text, "LEAK") {
			t.Errorf("a turn carries content outside the allow-list: %q", tr.Text)
		}
	}
}

// The shape measured against a live runtime: one prompt that uses a tool is
// answered by two assistant messages, and each is a turn, in order — as the
// terminal driver returns the same interaction.
func TestTurns_ASecondModelRoundIsASecondTurn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")

	f.addMessage(ref.ID, tUser("msg_u1", 1, "read the file"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 3,
		tPart("step-start", nil), tPart("reasoning", map[string]any{"text": "LEAK"}), tText("first"),
		tPart("tool", map[string]any{"state": map[string]any{"output": "LEAK-out"}}), tPart("step-finish", nil)))
	f.addMessage(ref.ID, tAsst("msg_a2", 4, 5, tPart("step-start", nil), tText("second\n\nThe file contains: hello"), tPart("step-finish", nil)))

	got := texts(getTurns(t, d, ref, "", 0))
	if want := []string{"first", "second\n\nThe file contains: hello"}; !eqStrings(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
}

// ADR 258's boundary, one category at a time: every message below holds a
// sentinel, one message holds the only real turn, and no page for any limit may
// contain a sentinel. A class this reader cannot classify with certainty is left
// out, so the shapes it has never seen are in the table too.
func TestTurns_BoundaryLeavesEverythingElseOut(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")

	f.addMessage(ref.ID, tUser("msg_01", 1, "LEAK-user-text"))
	f.addMessage(ref.ID, tAsst("msg_02", 2, 3, tPart("reasoning", map[string]any{"text": "LEAK-reasoning-only"})))
	f.addMessage(ref.ID, tAsst("msg_03", 4, 5, tPart("tool", map[string]any{"state": map[string]any{"output": "LEAK-tool-only"}})))
	f.addMessage(ref.ID, tAsst("msg_04", 6, 7, tPart("text", map[string]any{"text": "LEAK-synthetic", "synthetic": true})))
	f.addMessage(ref.ID, tAsst("msg_05", 8, 9, tPart("text", map[string]any{"text": "LEAK-ignored", "ignored": true})))
	f.addMessage(ref.ID, withInfo(tAsst("msg_06", 10, 11, tText("LEAK-errored-message")), "error",
		map[string]any{"name": "APIError", "data": map[string]any{"message": "LEAK-error-message"}}))
	f.addMessage(ref.ID, withInfo(tAsst("msg_07", 12, 13, tText("LEAK-compaction-summary")), "summary", true))
	f.addMessage(ref.ID, withInfo(tAsst("msg_08", 14, 15, tText("LEAK-no-model")), "modelID", ""))
	f.addMessage(ref.ID, tAsst("msg_09", 16, 17, tPart("text", map[string]any{"text": "LEAK-odd-flag", "synthetic": "yes"})))
	f.addMessage(ref.ID, tAsst("msg_10", 18, 19, tPart("future-part", map[string]any{"text": "LEAK-unknown-part"})))
	f.addMessage(ref.ID, tAsst("msg_11", 20, 21, tPart("file", map[string]any{"filename": "LEAK-file", "text": "LEAK-file-text"}), tPart("patch", map[string]any{"hash": "LEAK-patch"})))
	f.addMessage(ref.ID, tAsst("msg_12", 22, 23, tText("   \n"))) // blank is not a turn
	f.addMessage(ref.ID, map[string]any{"info": "not an object", "parts": []any{tText("LEAK-garbled")}})
	f.addMessage(ref.ID, map[string]any{"parts": []any{tText("LEAK-no-info")}})
	f.addMessage(ref.ID, tAsst("msg_13", 24, 25, tText("the only turn")))

	for _, limit := range []int{1, 2, 100} {
		page := getTurns(t, d, ref, "", limit)
		if got, want := texts(page), []string{"the only turn"}; !eqStrings(got, want) {
			t.Fatalf("limit %d: turns = %q, want %q", limit, got, want)
		}
	}
	// And from the very beginning of the record.
	f2 := newFakeServer(t)
	d2 := newTestDriver(t, f2)
	ref2 := createOne(t, d2, "/work/x", "k2")
	start := getTurns(t, d2, ref2, "", 0).Next
	for _, raw := range f.messages[ref.ID] {
		f2.mu.Lock()
		f2.messages[ref2.ID] = append(f2.messages[ref2.ID], raw)
		f2.mu.Unlock()
	}
	page := getTurns(t, d2, ref2, start, 100)
	if got, want := texts(page), []string{"the only turn"}; !eqStrings(got, want) {
		t.Fatalf("from the start: turns = %q, want %q", got, want)
	}
}

func TestTurns_MostRecentFirstThenForwardFromNext(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	for n := 1; n <= 5; n++ {
		addTurn(f, ref.ID, n, fmt.Sprintf("answer %d", n))
	}

	p := getTurns(t, d, ref, "", 2)
	if got, want := texts(p), []string{"answer 4", "answer 5"}; !eqStrings(got, want) {
		t.Fatalf("most recent 2 = %q, want %q", got, want)
	}
	if p.Next == "" {
		t.Fatal("no next cursor")
	}

	// Nothing new: an empty page, and a cursor that still works.
	idle := getTurns(t, d, ref, p.Next, 0)
	if len(idle.Turns) != 0 || idle.Next != p.Next {
		t.Fatalf("an idle poll = %+v, want no turns and the same next", idle)
	}

	addTurn(f, ref.ID, 6, "answer 6")
	more := getTurns(t, d, ref, p.Next, 0)
	if got, want := texts(more), []string{"answer 6"}; !eqStrings(got, want) {
		t.Fatalf("since next = %q, want %q", got, want)
	}
	if more.Next == p.Next {
		t.Error("next did not move after a turn was read")
	}

	// Default limit is 20, and the most recent are what comes back.
	f2 := newFakeServer(t)
	d2 := newTestDriver(t, f2)
	ref2 := createOne(t, d2, "/work/x", "k2")
	for n := 1; n <= 30; n++ {
		addTurn(f2, ref2.ID, n, fmt.Sprintf("answer %d", n))
	}
	big := getTurns(t, d2, ref2, "", 0)
	if len(big.Turns) != fleet.DefaultTurnsLimit || big.Turns[0].Text != "answer 11" || big.Turns[19].Text != "answer 30" {
		t.Fatalf("default page = %d turns %q..%q, want the most recent 20", len(big.Turns), big.Turns[0].Text, big.Turns[len(big.Turns)-1].Text)
	}
}

// Walking a conversation page by page returns every turn exactly once, in order,
// and an empty record is a page, not an error.
func TestTurns_WalkingByPagesReturnsEveryTurnOnce(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")

	empty := getTurns(t, d, ref, "", 0)
	if len(empty.Turns) != 0 || empty.Next == "" {
		t.Fatalf("an empty record = %+v, want no turns and a usable next", empty)
	}

	for n := 1; n <= 7; n++ {
		addTurn(f, ref.ID, n, fmt.Sprintf("answer %d", n))
		if n == 3 { // a message that is not a turn between turns
			f.addMessage(ref.ID, tAsst("msg_tool3", 31, 32, tPart("tool", map[string]any{"state": map[string]any{"output": "x"}})))
		}
	}
	var got []string
	cursor := empty.Next
	for pages := 0; pages < 10; pages++ {
		p := getTurns(t, d, ref, cursor, 2)
		if len(p.Turns) > 2 {
			t.Fatalf("a page of %d turns for limit 2", len(p.Turns))
		}
		got = append(got, texts(p)...)
		if len(p.Turns) == 0 {
			break
		}
		cursor = p.Next
	}
	want := []string{"answer 1", "answer 2", "answer 3", "answer 4", "answer 5", "answer 6", "answer 7"}
	if !eqStrings(got, want) {
		t.Fatalf("walked %q, want %q", got, want)
	}
}

func TestTurns_StaleCursorsAreRefused(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	a := createOne(t, d, "/work/x", "ka")
	b := createOne(t, d, "/work/y", "kb")
	for n := 1; n <= 3; n++ {
		addTurn(f, a.ID, n, fmt.Sprintf("a%d", n))
		addTurn(f, b.ID, n, fmt.Sprintf("b%d", n))
	}
	pa := getTurns(t, d, a, "", 0)
	req := fleet.RequestFrom(fleet.Caller{})

	// Another conversation's cursor.
	if _, err := d.Turns(context.Background(), req, b, fleet.TurnsQuery{Since: pa.Next}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor from another session: %v, want ErrAmbiguousTarget", err)
	}
	// The same id, a different start: a recycled id.
	forged := turnsCursor(a.ID, f.sessions[a.ID].Time.Created+1, "msg_a0001")
	if _, err := d.Turns(context.Background(), req, a, fleet.TurnsQuery{Since: forged}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor from an earlier incarnation of the id: %v, want ErrAmbiguousTarget", err)
	}
	// The anchor is gone: the record was rewritten under the caller.
	f.deleteMessage(a.ID, "msg_a0003")
	if _, err := d.Turns(context.Background(), req, a, fleet.TurnsQuery{Since: pa.Next}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor whose message was deleted: %v, want ErrAmbiguousTarget", err)
	}
	// Not a cursor at all is a malformed request, not a stale belief.
	for _, junk := range []string{"not-a-cursor", "%%%", turnsCursor("", 0, "")[:3]} {
		_, err := d.Turns(context.Background(), req, a, fleet.TurnsQuery{Since: junk})
		if err == nil || errors.Is(err, fleet.ErrAmbiguousTarget) || !strings.Contains(err.Error(), "cursor") {
			t.Errorf("since=%q: %v, want a plain refusal naming the cursor", junk, err)
		}
	}
}

// A cursor far behind is found by widening the tail window, and the window grows
// from a small first read rather than reading the record whole.
func TestTurns_FindsAnAnchorBeyondTheFirstWindow(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	for n := 1; n <= 3; n++ {
		addTurn(f, ref.ID, n, fmt.Sprintf("answer %d", n))
	}
	cursor := getTurns(t, d, ref, "", 0).Next
	for n := 4; n <= 150; n++ { // 294 further messages
		addTurn(f, ref.ID, n, fmt.Sprintf("answer %d", n))
	}
	before := len(f.requestsSnapshot())

	p := getTurns(t, d, ref, cursor, 100)
	if len(p.Turns) != 100 || p.Turns[0].Text != "answer 4" || p.Turns[99].Text != "answer 103" {
		t.Fatalf("page = %d turns, %q..%q; want answer 4..103", len(p.Turns), p.Turns[0].Text, p.Turns[len(p.Turns)-1].Text)
	}
	var limits []string
	for _, r := range f.requestsSnapshot()[before:] {
		if strings.HasSuffix(r.path, "/message") {
			limits = append(limits, r.query)
		}
	}
	if got, want := strings.Join(limits, " "), "limit=32 limit=128 limit=512"; got != want {
		t.Errorf("message reads = %q, want the window widening %q", got, want)
	}
	// And the page after it carries on exactly.
	rest := getTurns(t, d, ref, p.Next, 100)
	if len(rest.Turns) != 47 || rest.Turns[0].Text != "answer 104" || rest.Turns[46].Text != "answer 150" {
		t.Fatalf("the following page = %d turns from %q", len(rest.Turns), rest.Turns[0].Text)
	}
}

// A message the runtime is still writing is never returned partial, never
// skipped, and never returned twice.
func TestTurns_AMessageStillBeingWrittenIsHeldThenReturnedOnce(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	addTurn(f, ref.ID, 1, "answer 1")
	f.addMessage(ref.ID, tUser("msg_u2", 20, "question 2"))
	f.addMessage(ref.ID, tAsst("msg_a2", 21, 0, tText("half of an answ"))) // in flight

	p := getTurns(t, d, ref, "", 0)
	if got, want := texts(p), []string{"answer 1"}; !eqStrings(got, want) {
		t.Fatalf("while in flight = %q, want %q", got, want)
	}
	again := getTurns(t, d, ref, p.Next, 0)
	if len(again.Turns) != 0 || again.Next != p.Next {
		t.Fatalf("polling past an in-flight message = %+v, want nothing and the same next", again)
	}

	// A prompt queued behind it does not release it: nothing assistant follows.
	f.addMessage(ref.ID, tUser("msg_u3", 30, "question 3"))
	queued := getTurns(t, d, ref, p.Next, 0)
	if len(queued.Turns) != 0 || queued.Next != p.Next {
		t.Fatalf("with a prompt queued behind it = %+v, want nothing and the same next", queued)
	}

	// It finishes: returned once, whole.
	f.deleteMessage(ref.ID, "msg_u3")
	f.deleteMessage(ref.ID, "msg_a2")
	f.addMessage(ref.ID, tAsst("msg_a2", 21, 25, tText("a whole answer")))
	done := getTurns(t, d, ref, p.Next, 0)
	if got, want := texts(done), []string{"a whole answer"}; !eqStrings(got, want) {
		t.Fatalf("after completion = %q, want %q", got, want)
	}
	if rest := getTurns(t, d, ref, done.Next, 0); len(rest.Turns) != 0 {
		t.Fatalf("returned twice: %q", texts(rest))
	}

	// A remnant of a cut-off turn followed by a later assistant message is passed
	// over: it is never returned and does not block what comes after.
	g := newFakeServer(t)
	d2 := newTestDriver(t, g)
	ref2 := createOne(t, d2, "/work/x", "k2")
	start := getTurns(t, d2, ref2, "", 0).Next
	g.addMessage(ref2.ID, tUser("msg_u1", 1, "q1"))
	g.addMessage(ref2.ID, tAsst("msg_a1", 2, 0, tText("LEAK-cut-off")))
	g.addMessage(ref2.ID, tUser("msg_u2", 3, "q2"))
	g.addMessage(ref2.ID, tAsst("msg_a2", 4, 5, tText("after the restart")))
	got := getTurns(t, d2, ref2, start, 0)
	if want := []string{"after the restart"}; !eqStrings(texts(got), want) {
		t.Fatalf("past a cut-off remnant = %q, want %q", texts(got), want)
	}
}

func TestTurns_CorroboratesStartedAtBeforeReadingAnything(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	addTurn(f, ref.ID, 1, "answer 1")
	started := time.UnixMilli(f.sessions[ref.ID].Time.Created)

	before := len(f.requestsSnapshot())
	wrong := started.Add(time.Hour)
	req := fleet.RequestFrom(fleet.Caller{})
	req.Expect.StartedAt = &wrong
	if _, err := d.Turns(context.Background(), req, ref, fleet.TurnsQuery{}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Fatalf("a startedAt that is not this session's: %v, want ErrAmbiguousTarget", err)
	}
	for _, r := range f.requestsSnapshot()[before:] {
		if strings.HasSuffix(r.path, "/message") {
			t.Fatalf("the message record was read (%s) before the startedAt was corroborated", r.path)
		}
	}

	req.Expect.StartedAt = &started
	page, err := d.Turns(context.Background(), req, ref, fleet.TurnsQuery{})
	if err != nil || len(page.Turns) != 1 {
		t.Fatalf("a matching startedAt: %v, %+v", err, page)
	}
}

func TestTurns_RefusalsAndAbsences(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	req := fleet.RequestFrom(fleet.Caller{})

	if _, err := d.Turns(context.Background(), req, fleet.SessionRef{ID: "ses_never_seen"}, fleet.TurnsQuery{}); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("an id this driver never saw: %v, want ErrNoSuchSession", err)
	}
	for _, limit := range []int{-1, fleet.MaxTurnsLimit + 1} {
		if _, err := d.Turns(context.Background(), req, ref, fleet.TurnsQuery{Limit: limit}); err == nil {
			t.Errorf("limit %d was accepted; an over-large page is refused, never clamped", limit)
		}
	}

	// Seen once, gone from the runtime since.
	f.mu.Lock()
	delete(f.sessions, ref.ID)
	f.mu.Unlock()
	if _, err := d.Turns(context.Background(), req, ref, fleet.TurnsQuery{}); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("a session the runtime no longer holds: %v, want ErrNoSuchSession", err)
	}

	// Listed but not running: the same conflict every other verb gives.
	d.markSeen("ses_listed", knownSession{unknown: "needs re-creation"})
	_, err := d.Turns(context.Background(), req, fleet.SessionRef{ID: "ses_listed"}, fleet.TurnsQuery{})
	if kindOfErr(err) != fleet.ErrorConflict {
		t.Errorf("a session listed but not running: %v, want a conflict naming the reason", err)
	}
}

func TestTurns_ATurnOverTheBoundIsCutOnACharacterAndMarked(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	long := strings.Repeat("あ", fleet.MaxTurnTextBytes/3+10) // 3 bytes each, over the bound
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 3, tText(long)))
	f.addMessage(ref.ID, tAsst("msg_a2", 4, 5, tText("short")))

	p := getTurns(t, d, ref, "", 0)
	if len(p.Turns) != 2 {
		t.Fatalf("turns = %d", len(p.Turns))
	}
	if !p.Turns[0].Truncated || len(p.Turns[0].Text) > fleet.MaxTurnTextBytes || !utf8.ValidString(p.Turns[0].Text) {
		t.Errorf("long turn: truncated=%v len=%d valid=%v", p.Turns[0].Truncated, len(p.Turns[0].Text), utf8.ValidString(p.Turns[0].Text))
	}
	if p.Turns[1].Truncated {
		t.Error("a short turn was marked truncated")
	}
}

// The service finds this capability by asserting the interface; a driver that
// loses it silently answers `unsupported` again.
func TestTurns_IsAdvertisedByImplementingTurnReader(t *testing.T) {
	f := newFakeServer(t)
	var d driver.Driver = newTestDriver(t, f)
	if _, ok := d.(driver.TurnReader); !ok {
		t.Fatal("the opencode driver does not implement driver.TurnReader; /turns would answer unsupported")
	}
}
