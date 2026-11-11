package tmux

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// session.record on the terminal runtime's stream (muster #444). Growth is noticed
// in the stream's own pass, the window is that pass, and the cursor is what the
// route itself would answer as `next` — so what these tests hold is: one event per
// window, a cursor that resumes after the last entry, and no content in the payload.

// appendRecord adds lines to a conversation file, as the runtime does.
func appendRecord(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// recordEventRig is a driver whose record root holds one conversation for the
// session "alpha💬", so the stream resolves it the way a real pass does.
type recordEventRig struct {
	t    *testing.T
	mux  *fakeMux
	r    *ctlRegistry
	d    *Driver
	root string
	path string
}

const recordEventConv = "rec-alpha-444"

func newRecordEventRig(t *testing.T) *recordEventRig {
	t.Helper()
	rig := &recordEventRig{t: t, mux: twoSessions(), r: &ctlRegistry{}, root: t.TempDir()}
	writeRecord(t, rig.root, "/work/alpha", recordEventConv, "alpha💬", sessionStart.Add(4*time.Second))
	rig.path = filepath.Join(rig.root, recordDirFor("/work/alpha"), recordEventConv+".jsonl")
	rig.d = New("testbox",
		withExec(rig.mux.exec),
		withCtlDialer(rig.r.dial),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(rig.root),
	)
	return rig
}

// subscribe opens the stream on alpha only and returns it.
func (rig *recordEventRig) subscribe() driver.EventStream {
	rig.t.Helper()
	s, err := rig.d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{Sessions: []string{"alpha💬"}})
	if err != nil {
		rig.t.Fatal(err)
	}
	rig.t.Cleanup(func() { _ = s.Close() })
	return s
}

func (rig *recordEventRig) nextRecordEvent(s driver.EventStream, within time.Duration) (fleet.SessionRecordPayload, bool) {
	rig.t.Helper()
	deadline := time.Now().Add(within)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return fleet.SessionRecordPayload{}, false
		}
		ev, ok := nextWithin(rig.t, s, left)
		if !ok {
			return fleet.SessionRecordPayload{}, false
		}
		if ev.Kind != fleet.EventSessionRecord {
			continue
		}
		p, isPayload := ev.Payload.(fleet.SessionRecordPayload)
		if !isPayload {
			rig.t.Fatalf("payload type %T, want SessionRecordPayload", ev.Payload)
		}
		return p, true
	}
}

// The first oracle: an append produces exactly one event per window, carrying a
// cursor that resumes after the last entry.
func TestSubscribeEmitsOneRecordEventPerWindowWithACursorAfterTheLastEntry(t *testing.T) {
	rig := newRecordEventRig(t)
	s := rig.subscribe()
	alpha := rig.r.contentFor("alpha💬")

	// A burst: three lines and several triggers inside one window.
	appendRecord(t, rig.path,
		user(t, 1, "first question", nil),
		assistant(t, 2, textBlock("first answer")),
		user(t, 3, "second question", nil))
	for i := 0; i < 5; i++ {
		fire(alpha, "output")
	}

	p, ok := rig.nextRecordEvent(s, 3*time.Second)
	if !ok {
		t.Fatal("no session.record event after the record grew")
	}
	if p.Ref.ID != "alpha💬" || p.Cursor == "" {
		t.Fatalf("payload = %+v, want alpha💬 and a cursor", p)
	}
	if p.StartedAt == nil {
		t.Error("the payload must carry startedAt so a recycled id can be told apart")
	}
	if extra, again := rig.nextRecordEvent(s, 700*time.Millisecond); again {
		t.Fatalf("a burst inside one window is one event, got a second: %+v", extra)
	}

	// The cursor resumes after the last entry: nothing is recorded after it yet…
	page, err := readRecordAt(rig.path, p.Cursor, 20, time.Unix(1785760000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 0 {
		t.Fatalf("the event's cursor must resume AFTER the last entry; the route answered %v", recordTexts(page))
	}
	// …and the next append is exactly what the route then returns.
	appendRecord(t, rig.path, assistant(t, 4, textBlock("second answer")))
	page, err = readRecordAt(rig.path, p.Cursor, 20, time.Unix(1785760000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recordTexts(page), []string{"assistant:second answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the event's cursor the route returned %v, want %v", got, want)
	}

	// That append is the next window's event.
	fire(alpha, "output")
	if _, ok := rig.nextRecordEvent(s, 3*time.Second); !ok {
		t.Fatal("a later append produced no event: one per window, not one per subscription")
	}
}

// A pass that finds the record unchanged says nothing: the stream's passes are
// woken by the screen, and most screens change with no new entry.
func TestSubscribeEmitsNoRecordEventWhenTheRecordDidNotGrow(t *testing.T) {
	rig := newRecordEventRig(t)
	s := rig.subscribe()

	rig.mux.setCapture("%1", fixtureWorking) // a real state change, so the pass really ran
	fire(rig.r.contentFor("alpha💬"), "output")

	sawState := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sawState {
		ev, ok := nextWithin(t, s, 500*time.Millisecond)
		if !ok {
			continue
		}
		switch ev.Kind {
		case fleet.EventSessionRecord:
			t.Fatalf("a session.record event with no growth: %+v", ev.Payload)
		case fleet.EventSessionState:
			sawState = true
		}
	}
	if !sawState {
		t.Fatal("the pass never ran, so this proved nothing")
	}
	if p, again := rig.nextRecordEvent(s, 500*time.Millisecond); again {
		t.Fatalf("a session.record event with no growth: %+v", p)
	}
}

// What a pass compares, one case at a time, without a multiplexer.
func TestNoteRecordSaysOnlyWhatChanged(t *testing.T) {
	root := t.TempDir()
	d := New("testbox", WithRecordRoot(root), withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	path := filepath.Join(root, recordDirFor("/work/alpha"), "conv-a.jsonl")
	writeRecord(t, root, "/work/alpha", "conv-a", "alpha💬", sessionStart)

	sess := fleet.Session{
		SessionRef:   fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		Cwd:          "/work/alpha",
		Conversation: &fleet.ConversationRef{Known: true, ID: "conv-a"},
	}
	s := &eventStream{d: d, out: make(chan fleet.Event, 16), recordMarks: map[string]recordMark{}}
	ctx := context.Background()
	drained := func() []fleet.SessionRecordPayload {
		var out []fleet.SessionRecordPayload
		for {
			select {
			case ev := <-s.out:
				if p, ok := ev.Payload.(fleet.SessionRecordPayload); ok {
					out = append(out, p)
				}
			default:
				return out
			}
		}
	}

	// First sight only seeds: what the record held when the stream met the session
	// is the baseline, not news.
	s.noteRecord(ctx, sess, false)
	if got := drained(); len(got) != 0 {
		t.Fatalf("the first sight of a session reported %v; it only seeds", got)
	}
	// Unchanged: nothing.
	s.noteRecord(ctx, sess, false)
	if got := drained(); len(got) != 0 {
		t.Fatalf("an unchanged record reported %v", got)
	}
	// Growth of the conversation file: one event.
	appendRecord(t, path, user(t, 1, "a question", nil))
	s.noteRecord(ctx, sess, false)
	if got := drained(); len(got) != 1 {
		t.Fatalf("growth produced %d events, want 1", len(got))
	}
	// Growth of a sub-task's own file is growth of the record.
	subDir := subTaskDir(path)
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subFile := filepath.Join(subDir, "agent-sub1.jsonl")
	if err := os.WriteFile(subFile, []byte(user(t, 2, "a delegated task", nil)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.noteRecord(ctx, sess, false)
	if got := drained(); len(got) != 1 {
		t.Fatalf("a new sub-task file produced %d events, want 1", len(got))
	}
	appendRecord(t, subFile, assistant(t, 3, textBlock("its answer")))
	s.noteRecord(ctx, sess, false)
	if got := drained(); len(got) != 1 {
		t.Fatalf("growth of a sub-task file produced %d events, want 1", len(got))
	}
	// A conversation replaced under the same session id (a /clear): a client that
	// holds a cursor of the old one must hear, because its next read is a conflict.
	writeRecord(t, root, "/work/alpha", "conv-b", "alpha💬", sessionStart.Add(time.Minute))
	replaced := sess
	replaced.Conversation = &fleet.ConversationRef{Known: true, ID: "conv-b"}
	s.noteRecord(ctx, replaced, false)
	got := drained()
	if len(got) != 1 {
		t.Fatalf("a replaced conversation produced %d events, want 1", len(got))
	}
	if page, err := readRecordAt(filepath.Join(root, recordDirFor("/work/alpha"), "conv-b.jsonl"), got[0].Cursor, 20, time.Unix(1785760000, 0)); err != nil || len(page.Messages) != 0 {
		t.Fatalf("the replaced conversation's cursor must be one of ITS record, resuming after its last entry: %+v %v", page, err)
	}

	// A session this pass announced as created is baseline, whatever its record holds.
	fresh := sess
	fresh.ID = "beta"
	s.noteRecord(ctx, fresh, true)
	if got := drained(); len(got) != 0 {
		t.Fatalf("a session announced as created reported %v; session.created is its look-now", got)
	}
}

// A session whose record this driver cannot identify has nothing to report and is
// not reported on: no record root, no conversation yet, or no file.
func TestNoteRecordIsSilentWhereTheRecordCannotBeSeen(t *testing.T) {
	root := t.TempDir()
	withRoot := New("testbox", WithRecordRoot(root))
	withoutRoot := New("testbox")
	sess := fleet.Session{
		SessionRef:   fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		Cwd:          "/work/alpha",
		Conversation: &fleet.ConversationRef{Known: true, ID: "conv-a"},
	}
	for name, tc := range map[string]struct {
		d    *Driver
		sess fleet.Session
	}{
		"no record root":       {withoutRoot, sess},
		"no conversation":      {withRoot, fleet.Session{SessionRef: sess.SessionRef, Cwd: sess.Cwd}},
		"conversation unknown": {withRoot, fleet.Session{SessionRef: sess.SessionRef, Cwd: sess.Cwd, Conversation: &fleet.ConversationRef{Known: false}}},
		"no file for the id":   {withRoot, sess},
		"an id that is a path": {withRoot, fleet.Session{SessionRef: sess.SessionRef, Cwd: sess.Cwd, Conversation: &fleet.ConversationRef{Known: true, ID: "../escape"}}},
	} {
		if _, _, ok := tc.d.recordMarkOf(tc.sess); ok {
			t.Errorf("%s: recordMarkOf reported a mark", name)
		}
	}
}

// The mark is names and sizes, nothing a client could read: pin that it carries
// no content by holding its shape.
func TestRecordMarkHoldsOnlyIdentifiersAndSizes(t *testing.T) {
	typ := reflect.TypeOf(recordMark{})
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, typ.Field(i).Name+":"+typ.Field(i).Type.String())
	}
	sort.Strings(fields)
	want := []string{"conv:string", "main:int64", "subs:string"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("recordMark fields = %v, want %v", fields, want)
	}
}
