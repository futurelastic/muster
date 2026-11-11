package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Synthetic fixtures shaped like the runtime's message record, built as the runtime
// would send them (map form): the part and tool-state shapes are the ones the
// runtime's own OpenAPI document gives (a text part with an optional `time`, a tool
// part whose `state` is pending, running, completed or error, a file part with an
// inline data URL). No real session content appears in any of it.

func rTool(callID, name string, state map[string]any) map[string]any {
	return tPart("tool", map[string]any{"callID": callID, "tool": name, "state": state})
}

func rSettled(text string, start, end int64) map[string]any {
	return tPart("text", map[string]any{"text": text, "time": map[string]any{"start": start, "end": end}})
}

func rFile(mime, url string) map[string]any {
	return tPart("file", map[string]any{"mime": mime, "url": url, "filename": "x"})
}

func rDone(input map[string]any, output string, start, end int64) map[string]any {
	return map[string]any{"status": "completed", "input": input, "output": output, "title": "t",
		"metadata": map[string]any{"LEAK": "LEAK-metadata"}, "time": map[string]any{"start": start, "end": end}}
}

func rRunning(input map[string]any, start int64) map[string]any {
	return map[string]any{"status": "running", "input": input, "time": map[string]any{"start": start}}
}

// Tiny images whose first bytes are the signature of their type, as base64 data URLs.
func pngData() ([]byte, string) {
	b := append([]byte("\x89PNG\r\n\x1a\n"), []byte("not-a-real-picture-just-bytes")...)
	return b, "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

func jpegData() ([]byte, string) {
	b := append([]byte("\xff\xd8\xff\xe0"), []byte("not-a-real-picture-either-bytes")...)
	return b, "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b)
}

func getRecord(t *testing.T, d *Driver, ref fleet.SessionRef, since string, limit int) fleet.RecordPage {
	t.Helper()
	page, err := d.Record(context.Background(), fleet.RequestFrom(fleet.Caller{}), ref, fleet.RecordQuery{Since: since, Limit: limit})
	if err != nil {
		t.Fatalf("Record(since=%q, limit=%d): %v", since, limit, err)
	}
	if page.Messages == nil {
		t.Fatal("Record returned a nil slice; an empty page is [] on the wire")
	}
	if page.Format != fleet.RecordFormat {
		t.Fatalf("format = %d, want %d", page.Format, fleet.RecordFormat)
	}
	return page
}

// shape is "kind/role" with the text, or the tool's id and status, for a compact
// comparison.
func shape(m fleet.RecordMessage) string {
	switch m.Kind {
	case fleet.RecordKindTool:
		return fmt.Sprintf("tool/%s:%s:%s", m.Role, m.Tool.ID, m.Tool.Status)
	case fleet.RecordKindImage:
		return fmt.Sprintf("image/%s", m.Role)
	}
	return fmt.Sprintf("%s/%s:%s", m.Kind, m.Role, m.Text)
}

func shapes(p fleet.RecordPage) []string {
	out := make([]string, 0, len(p.Messages))
	for _, m := range p.Messages {
		out = append(out, shape(m))
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newRecordSession(t *testing.T) (*fakeServer, *Driver, fleet.SessionRef) {
	t.Helper()
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	return f, d, createOne(t, d, "/work/x", "k1")
}

// Text, in both directions: what a person typed and what the agent wrote, in order,
// each with its own side and the model on the agent's.
func TestRecord_TextInBothDirections(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1000, "hello there"))
	f.addMessage(ref.ID, tAsst("msg_a1", 1001, 1900, rSettled("hi, how can I help", 1002, 1850)))

	p := getRecord(t, d, ref, "", 0)
	want := []string{"text/user:hello there", "text/assistant:hi, how can I help"}
	if got := shapes(p); !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
	if u := p.Messages[0]; !u.At.Equal(time.UnixMilli(1000)) || u.At.Location() != time.UTC || u.Model != "" {
		t.Errorf("user entry = %+v; want the message's creation time in UTC and no model", u)
	}
	if a := p.Messages[1]; !a.At.Equal(time.UnixMilli(1850)) || a.Model != "big-pickle" {
		t.Errorf("assistant entry = %+v; want the part's own end time and the model", a)
	}
}

// The oracle of the issue: a prompt that uses a tool is answered by two assistant
// messages, one per model round, and the record reads them as two assistant entries
// with the tool between them — as turns reads the same interaction as two turns.
func TestRecord_TwoModelRoundsAreTwoAssistantEntries(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "read the file"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 3,
		tPart("step-start", nil), tText("first"),
		rTool("call_1", "read", rDone(map[string]any{"path": "/f"}, "hello", 2, 3)), tPart("step-finish", nil)))
	f.addMessage(ref.ID, tAsst("msg_a2", 4, 5, tPart("step-start", nil), tText("second\n\nThe file contains: hello"), tPart("step-finish", nil)))

	want := []string{
		"text/user:read the file",
		"text/assistant:first",
		"tool/assistant:call_1:completed",
		"text/assistant:second\n\nThe file contains: hello",
	}
	if got := shapes(getRecord(t, d, ref, "", 0)); !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
	// The same two assistant texts turns returns.
	if got, want := texts(getTurns(t, d, ref, "", 0)), []string{"first", "second\n\nThe file contains: hello"}; !eqStrings(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
}

func TestRecord_ReasoningIsItsOwnKind(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "why"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 9,
		tPart("reasoning", map[string]any{"text": "weighing it up", "time": map[string]any{"start": 3, "end": 4}}),
		tPart("reasoning", map[string]any{"text": "  ", "time": map[string]any{"start": 5, "end": 6}}), // blank: not written out
		tText("because")))
	p := getRecord(t, d, ref, "", 0)
	if got, want := shapes(p), []string{"text/user:why", "reasoning/assistant:weighing it up", "text/assistant:because"}; !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
	if r := p.Messages[1]; r.Model != "big-pickle" || !r.At.Equal(time.UnixMilli(4)) {
		t.Errorf("reasoning entry = %+v", r)
	}
}

// Tool parts with their state, paired into one `tool` message: the input as the
// runtime recorded it, the result, and a status taken from the runtime's own state.
func TestRecord_ToolPartsCarryTheirStateAndResult(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1000, "go"))
	f.addMessage(ref.ID, tAsst("msg_a1", 1001, 1900,
		rTool("call_1", "bash", rDone(map[string]any{"command": "ls"}, "a\nb", 1100, 1200)),
		rTool("call_2", "read", map[string]any{"status": "error", "input": map[string]any{"path": "/x"},
			"error": "no such file", "time": map[string]any{"start": 1300, "end": 1301}}),
		tText("done")))

	p := getRecord(t, d, ref, "", 0)
	if got, want := shapes(p), []string{"text/user:go", "tool/assistant:call_1:completed", "tool/assistant:call_2:error", "text/assistant:done"}; !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
	one, two := p.Messages[1], p.Messages[2]
	if one.Tool.Name != "bash" || string(one.Tool.Input) != `{"command":"ls"}` || one.Tool.Result != "a\nb" || !one.At.Equal(time.UnixMilli(1100)) || one.Model != "big-pickle" {
		t.Errorf("completed call = %+v (tool %+v)", one, one.Tool)
	}
	if two.Tool.Name != "read" || two.Tool.Result != "no such file" || !two.At.Equal(time.UnixMilli(1300)) {
		t.Errorf("failed call = %+v (tool %+v)", two, two.Tool)
	}
	if strings.Contains(mustJSON(t, p), "LEAK") {
		t.Errorf("the page carries a tool's metadata: %s", mustJSON(t, p))
	}
}

// A call the runtime is still running goes out running, once; the page that reads
// its outcome carries the same message again, complete, with the same id and the
// call's own time, in front of what is new — and nothing goes out twice besides.
func TestRecord_ARunningCallGoesOutOnceAndAgainCompleteByTheSameID(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1000, "build it"))
	f.addMessage(ref.ID, tAsst("msg_a1", 1001, 0,
		rSettled("building", 1002, 1003),
		rTool("call_1", "bash", rRunning(map[string]any{"command": "make"}, 1100))))

	p1 := getRecord(t, d, ref, "", 0)
	if got, want := shapes(p1), []string{"text/user:build it", "text/assistant:building", "tool/assistant:call_1:running"}; !eqStrings(got, want) {
		t.Fatalf("while running = %q, want %q", got, want)
	}
	if p1.Messages[2].Tool.Result != "" {
		t.Errorf("a running call carries a result: %+v", p1.Messages[2].Tool)
	}

	// Polling while it runs: nothing new, and the cursor stays where it was.
	idle := getRecord(t, d, ref, p1.Next, 0)
	if len(idle.Messages) != 0 || idle.Next != p1.Next {
		t.Fatalf("an idle poll = %+v, want nothing and the same next", idle)
	}
	// A prompt queued behind the message does not release it.
	f.addMessage(ref.ID, tUser("msg_u2", 1500, "and then?"))
	if held := getRecord(t, d, ref, p1.Next, 0); len(held.Messages) != 0 || held.Next != p1.Next {
		t.Fatalf("with a prompt queued behind it = %+v", held)
	}

	// The call finishes, and so does the message.
	f.deleteMessage(ref.ID, "msg_u2")
	f.deleteMessage(ref.ID, "msg_a1")
	f.addMessage(ref.ID, tAsst("msg_a1", 1001, 1400,
		rSettled("building", 1002, 1003),
		rTool("call_1", "bash", rDone(map[string]any{"command": "make"}, "built", 1100, 1300)),
		rSettled("it is built", 1310, 1390)))
	f.addMessage(ref.ID, tUser("msg_u2", 1500, "and then?"))

	p2 := getRecord(t, d, ref, p1.Next, 0)
	want := []string{"tool/assistant:call_1:completed", "text/assistant:it is built", "text/user:and then?"}
	if got := shapes(p2); !eqStrings(got, want) {
		t.Fatalf("after it finished = %q, want %q", got, want)
	}
	if m := p2.Messages[0]; m.Tool.Result != "built" || !m.At.Equal(p1.Messages[2].At) {
		t.Errorf("the update = %+v; want the result, and the call's own time %v", m, p1.Messages[2].At)
	}
	if rest := getRecord(t, d, ref, p2.Next, 0); len(rest.Messages) != 0 {
		t.Fatalf("returned twice: %q", shapes(rest))
	}
}

// What the runtime is still writing is not returned: a text part still streaming, and a
// call whose input is still arriving, hold the read; the part is returned once, whole,
// when it settles.
func TestRecord_AnUnsettledPartIsHeldThenReturnedOnce(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 0, tPart("text", map[string]any{"text": "half of an answ", "time": map[string]any{"start": 3}})))

	p := getRecord(t, d, ref, "", 0)
	if got, want := shapes(p), []string{"text/user:q"}; !eqStrings(got, want) {
		t.Fatalf("while streaming = %q, want %q", got, want)
	}
	f.deleteMessage(ref.ID, "msg_a1")
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 0,
		rSettled("a whole answer", 3, 4),
		rTool("call_1", "bash", map[string]any{"status": "pending", "input": map[string]any{}, "raw": "{\"comm"})))
	q := getRecord(t, d, ref, p.Next, 0)
	if got, want := shapes(q), []string{"text/assistant:a whole answer"}; !eqStrings(got, want) {
		t.Fatalf("with a call still arriving = %q, want %q", got, want)
	}
	f.deleteMessage(ref.ID, "msg_a1")
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 9,
		rSettled("a whole answer", 3, 4),
		rTool("call_1", "bash", rDone(map[string]any{"command": "x"}, "ok", 5, 6))))
	r := getRecord(t, d, ref, q.Next, 0)
	if got, want := shapes(r), []string{"tool/assistant:call_1:completed"}; !eqStrings(got, want) {
		t.Fatalf("once finished = %q, want %q (and the text only once)", got, want)
	}
}

// A message the runtime never finished, with a later assistant message after it, is a
// remnant of a cut-off turn: a call in it that has no outcome goes out `error` — and
// replaces the running one the reader already holds.
func TestRecord_ARemnantsUnfinishedCallGoesOutAsAnError(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q1"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 0, rSettled("working", 3, 4), rTool("call_1", "bash", rRunning(map[string]any{"command": "sleep 99"}, 5))))
	p := getRecord(t, d, ref, "", 0)
	if got, want := shapes(p), []string{"text/user:q1", "text/assistant:working", "tool/assistant:call_1:running"}; !eqStrings(got, want) {
		t.Fatalf("in flight = %q, want %q", got, want)
	}

	f.addMessage(ref.ID, tUser("msg_u2", 10, "q2"))
	f.addMessage(ref.ID, tAsst("msg_a2", 11, 12, tText("after the restart")))
	q := getRecord(t, d, ref, p.Next, 0)
	want := []string{"tool/assistant:call_1:error", "text/user:q2", "text/assistant:after the restart"}
	if got := shapes(q); !eqStrings(got, want) {
		t.Fatalf("past the remnant = %q, want %q", got, want)
	}
	if q.Messages[0].Tool.Result != "" || !q.Messages[0].At.Equal(p.Messages[2].At) {
		t.Errorf("the interrupted call = %+v", q.Messages[0])
	}

	// A message that ends in an error after its call went out running is the same:
	// the call is closed as an error, and nothing more of the message is read.
	g, d2, ref2 := newRecordSession(t)
	g.addMessage(ref2.ID, tUser("msg_u1", 1, "q"))
	g.addMessage(ref2.ID, tAsst("msg_a1", 2, 0, rTool("call_1", "bash", rRunning(map[string]any{"command": "x"}, 5))))
	first := getRecord(t, d2, ref2, "", 0)
	g.deleteMessage(ref2.ID, "msg_a1")
	g.addMessage(ref2.ID, withInfo(tAsst("msg_a1", 2, 9, tText("LEAK-after-error"),
		rTool("call_1", "bash", rDone(map[string]any{"command": "x"}, "LEAK-result", 5, 8))), "error",
		map[string]any{"name": "MessageAbortedError", "data": map[string]any{"message": "LEAK-abort"}}))
	second := getRecord(t, d2, ref2, first.Next, 0)
	if got, want := shapes(second), []string{"tool/assistant:call_1:error"}; !eqStrings(got, want) {
		t.Fatalf("after the message errored = %q, want %q", got, want)
	}
	if strings.Contains(mustJSON(t, second), "LEAK") {
		t.Errorf("an errored message's content went out: %s", mustJSON(t, second))
	}
}

// The calls a cursor carries are bounded: a message with more calls running at once
// than that is read up to the bound and waits, sending none of them twice.
func TestRecord_MoreRunningCallsThanACursorCarriesWait(t *testing.T) {
	f, d, ref := newRecordSession(t)
	parts := make([]map[string]any, 0, recordMaxPending+1)
	for i := 0; i < recordMaxPending+1; i++ {
		parts = append(parts, rTool(fmt.Sprintf("call_%03d", i), "bash", rRunning(map[string]any{"n": i}, int64(10+i))))
	}
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 0, parts...))

	p := getRecord(t, d, ref, "", fleet.MaxRecordLimit)
	if len(p.Messages) != recordMaxPending {
		t.Fatalf("entries = %d, want the %d the cursor can carry", len(p.Messages), recordMaxPending)
	}
	if again := getRecord(t, d, ref, p.Next, fleet.MaxRecordLimit); len(again.Messages) != 0 || again.Next != p.Next {
		t.Fatalf("a poll while they all run = %d entries, want none and the same next", len(again.Messages))
	}
}

// A reader that meets a remnant for the first time (no call of it went out before)
// gets its unfinished calls as errors too, never as calls that are running for ever.
func TestRecord_AFreshReadOfARemnantClosesItsUnfinishedCalls(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q1"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 0,
		rSettled("working", 3, 4),
		rTool("call_1", "bash", rRunning(map[string]any{"command": "sleep 99"}, 5)),
		rTool("call_2", "bash", map[string]any{"status": "pending", "input": map[string]any{}, "raw": "{"}),
		tPart("text", map[string]any{"text": "half of a sent", "time": map[string]any{"start": 6}})))
	f.addMessage(ref.ID, tUser("msg_u2", 10, "q2"))
	f.addMessage(ref.ID, tAsst("msg_a2", 11, 12, tText("after")))

	p := getRecord(t, d, ref, "", 0)
	want := []string{"text/user:q1", "text/assistant:working", "tool/assistant:call_1:error", "tool/assistant:call_2:error",
		"text/assistant:half of a sent", "text/user:q2", "text/assistant:after"}
	if got := shapes(p); !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
}

// A compaction summary is its own kind, and nothing else of that message is read.
func TestRecord_ACompactionSummaryIsASummary(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q"))
	f.addMessage(ref.ID, withInfo(tAsst("msg_a1", 2, 3,
		tPart("reasoning", map[string]any{"text": "LEAK-summary-reasoning", "time": map[string]any{"start": 2, "end": 3}}),
		tText("what happened so far"),
		rTool("call_9", "bash", rDone(map[string]any{}, "LEAK-summary-tool", 2, 3))), "summary", true))
	f.addMessage(ref.ID, tAsst("msg_a2", 4, 5, tText("carrying on")))

	p := getRecord(t, d, ref, "", 0)
	if got, want := shapes(p), []string{"text/user:q", "summary/assistant:what happened so far", "text/assistant:carrying on"}; !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
	if p.Messages[1].Model != "big-pickle" || strings.Contains(mustJSON(t, p), "LEAK") {
		t.Errorf("summary entry = %+v", p.Messages[1])
	}
	// turns still leaves it out.
	if got, want := texts(getTurns(t, d, ref, "", 0)), []string{"carrying on"}; !eqStrings(got, want) {
		t.Errorf("turns = %q, want %q", got, want)
	}
}

// Images are references and never bytes, and the reference reads back the image the
// entry named: a person's attachment and one a completed tool returned.
func TestRecord_ImagesAreReferencesAndReadBack(t *testing.T) {
	f, d, ref := newRecordSession(t)
	png, pngURL := pngData()
	jpeg, jpegURL := jpegData()

	f.addMessage(ref.ID, map[string]any{
		"info": map[string]any{"id": "msg_u1", "role": "user", "time": map[string]any{"created": 1000}, "agent": "build"},
		"parts": []any{
			tText("what is this"),
			rFile("image/png", pngURL),
			rFile("image/png", "file:///LEAK/path.png"),                          // a path, not inline
			rFile("image/svg+xml", "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4="), // not a type this API names
			rFile("image/png", jpegURL),                                          // the URL says another type
			rFile("image/png", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("not a png at all, plain text bytes"))),
			rFile("image/png", "data:image/png;base64,AAAA"), // far too short to be one
		},
	})
	f.addMessage(ref.ID, tAsst("msg_a1", 1001, 1900,
		rTool("call_1", "read", map[string]any{"status": "completed", "input": map[string]any{"path": "/pic"}, "output": "Image read successfully",
			"title": "t", "metadata": map[string]any{}, "time": map[string]any{"start": 1100, "end": 1200},
			"attachments": []any{rFile("image/png", pngURL), rFile("image/jpeg", jpegURL), rFile("image/png", "http://LEAK/x.png")}}),
		// An image on an assistant message itself is not a shape this reader emits.
		rFile("image/png", pngURL)))

	p := getRecord(t, d, ref, "", 0)
	want := []string{"text/user:what is this", "image/user", "tool/assistant:call_1:completed", "image/assistant", "image/assistant"}
	if got := shapes(p); !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
	body := mustJSON(t, p)
	for _, leak := range []string{"LEAK", "base64", "iVBOR"} {
		if strings.Contains(body, leak) {
			t.Errorf("the page carries %q: %s", leak, body)
		}
	}

	read := func(media string) (fleet.RecordMedia, error) {
		return d.RecordMedia(context.Background(), fleet.RequestFrom(fleet.Caller{}), ref, media)
	}
	for i, tc := range []struct {
		entry int
		ctype string
		data  []byte
	}{{1, "image/png", png}, {3, "image/png", png}, {4, "image/jpeg", jpeg}} {
		m := p.Messages[tc.entry]
		if m.Media == "" || m.Text != "" || m.Tool != nil {
			t.Fatalf("image entry %d = %+v", i, m)
		}
		got, err := read(m.Media)
		if err != nil || got.ContentType != tc.ctype || string(got.Data) != string(tc.data) {
			t.Errorf("image %d read back = %q %d bytes, %v; want %q %d bytes", i, got.ContentType, len(got.Data), err, tc.ctype, len(tc.data))
		}
		// The reference holds a place, not the picture.
		raw, _ := base64.RawURLEncoding.DecodeString(m.Media)
		if len(m.Media) > 200 || strings.Contains(string(raw), base64.StdEncoding.EncodeToString(tc.data)[:16]) {
			t.Errorf("reference %d holds content: %s", i, raw)
		}
	}
}

// Every way a reference can fail is the same answer — and one that names another
// session, a message that is gone, or a place the allow-list does not admit as an
// image is never read, whatever the runtime holds there.
func TestRecord_AMediaReferenceThatDoesNotNameAnImageIsOneAnswer(t *testing.T) {
	f, d, ref := newRecordSession(t)
	other := createOne(t, d, "/work/y", "k2")
	_, pngURL := pngData()
	for _, id := range []string{ref.ID, other.ID} {
		f.addMessage(id, map[string]any{
			"info":  map[string]any{"id": "msg_u1", "role": "user", "time": map[string]any{"created": 1}, "agent": "build"},
			"parts": []any{tText("see"), rFile("image/png", pngURL), rFile("image/png", "file:///LEAK.png")},
		})
	}
	created := f.sessions[ref.ID].Time.Created
	good := mediaRef{S: ref.ID, C: created, M: "msg_u1", P: 1, A: -1}
	read := func(r string) error {
		_, err := d.RecordMedia(context.Background(), fleet.RequestFrom(fleet.Caller{}), ref, r)
		return err
	}
	if err := read(good.encode()); err != nil {
		t.Fatalf("the genuine reference: %v", err)
	}
	for name, r := range map[string]string{
		"junk":                  "not-a-reference",
		"empty":                 "",
		"a turns cursor":        turnsCursor(ref.ID, created, "msg_u1"),
		"another session":       mediaRef{S: other.ID, C: f.sessions[other.ID].Time.Created, M: "msg_u1", P: 1, A: -1}.encode(),
		"a recycled id":         mediaRef{S: ref.ID, C: created + 1, M: "msg_u1", P: 1, A: -1}.encode(),
		"a message not there":   mediaRef{S: ref.ID, C: created, M: "msg_gone", P: 1, A: -1}.encode(),
		"a text part":           mediaRef{S: ref.ID, C: created, M: "msg_u1", P: 0, A: -1}.encode(),
		"a part not an image":   mediaRef{S: ref.ID, C: created, M: "msg_u1", P: 2, A: -1}.encode(),
		"a part out of range":   mediaRef{S: ref.ID, C: created, M: "msg_u1", P: 9, A: -1}.encode(),
		"an attachment of none": mediaRef{S: ref.ID, C: created, M: "msg_u1", P: 1, A: 0}.encode(),
		"extra fields":          base64.RawURLEncoding.EncodeToString([]byte(`{"v":"om1","s":"` + ref.ID + `","c":` + fmt.Sprint(created) + `,"m":"msg_u1","p":1,"a":-1,"x":1}`)),
	} {
		if err := read(r); !errors.Is(err, fleet.ErrNoSuchMedia) {
			t.Errorf("%s: %v, want ErrNoSuchMedia", name, err)
		}
	}
	// The message is deleted since: the reference stops working.
	f.deleteMessage(ref.ID, "msg_u1")
	if err := read(good.encode()); !errors.Is(err, fleet.ErrNoSuchMedia) {
		t.Errorf("after the message was deleted: %v, want ErrNoSuchMedia", err)
	}
	// startedAt is corroborated before anything is read.
	wrong := time.UnixMilli(created).Add(time.Hour)
	req := fleet.RequestFrom(fleet.Caller{})
	req.Expect.StartedAt = &wrong
	if _, err := d.RecordMedia(context.Background(), req, ref, good.encode()); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a startedAt that is not this session's: %v, want ErrAmbiguousTarget", err)
	}
	if _, err := d.RecordMedia(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: "ses_never_seen"}, good.encode()); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("an id this driver never saw: %v, want ErrNoSuchSession", err)
	}
}

// ADR 441's boundary, one category at a time: every message below holds a sentinel,
// one holds the only real entry, and no page for any limit may contain a sentinel. A
// shape this reader has never seen is left out, so those are in the table too.
func TestRecord_BoundaryLeavesEverythingElseOut(t *testing.T) {
	f, d, ref := newRecordSession(t)
	_, pngURL := pngData()
	bad := func(m map[string]any) map[string]any { return m }

	f.addMessage(ref.ID, map[string]any{"info": map[string]any{"id": "msg_01", "role": "user", "time": map[string]any{"created": 1}},
		"parts": []any{
			tPart("text", map[string]any{"text": "LEAK-synthetic-user", "synthetic": true}),
			tPart("text", map[string]any{"text": "LEAK-ignored-user", "ignored": true}),
			tText("   \n"),
			tPart("text", map[string]any{"text": "LEAK-odd-flag", "synthetic": "yes"}),
			tPart("agent", map[string]any{"name": "LEAK-agent"}),
			tPart("subtask", map[string]any{"prompt": "LEAK-subtask", "description": "LEAK-d", "agent": "x"}),
			tPart("compaction", map[string]any{"auto": true}),
			"LEAK-not-an-object",
			tPart("future-part", map[string]any{"text": "LEAK-unknown-part"}),
		}})
	f.addMessage(ref.ID, tAsst("msg_02", 2, 3,
		tPart("step-start", nil), tPart("step-finish", map[string]any{"reason": "stop"}),
		tPart("snapshot", map[string]any{"snapshot": "LEAK-snapshot"}),
		tPart("patch", map[string]any{"hash": "LEAK-patch", "files": []any{"LEAK-file"}}),
		tPart("retry", map[string]any{"attempt": 1, "error": map[string]any{"name": "LEAK-retry"}}),
		tPart("agent", map[string]any{"name": "LEAK-agent"}),
		tPart("text", map[string]any{"text": "LEAK-synthetic", "synthetic": true}),
		tPart("text", map[string]any{"text": "LEAK-ignored", "ignored": true}),
		tPart("text", map[string]any{"text": "LEAK-odd-flag", "synthetic": "yes"}),
		tPart("future-part", map[string]any{"text": "LEAK-unknown-part"}),
		rFile("image/png", pngURL),
		rTool("call_a", "bash", map[string]any{"status": "weird", "input": map[string]any{}, "output": "LEAK-weird-status"}),
		rTool("", "bash", rDone(map[string]any{}, "LEAK-no-call-id", 1, 2)),
		rTool(strings.Repeat("c", maxCallIDBytes+1), "bash", rDone(map[string]any{}, "LEAK-long-call-id", 1, 2)),
		rTool("call_b", "", rDone(map[string]any{}, "LEAK-no-tool-name", 1, 2)),
		tPart("tool", map[string]any{"callID": "call_c", "tool": "x", "state": "LEAK-not-a-state"}),
	))
	f.addMessage(ref.ID, withInfo(tAsst("msg_03", 4, 5, tText("LEAK-errored-message"), rTool("call_d", "x", rDone(map[string]any{}, "LEAK-errored-tool", 1, 2))), "error",
		map[string]any{"name": "APIError", "data": map[string]any{"message": "LEAK-error-message"}}))
	f.addMessage(ref.ID, withInfo(tAsst("msg_04", 6, 7, tText("LEAK-no-model")), "modelID", ""))
	f.addMessage(ref.ID, map[string]any{"info": "not an object", "parts": []any{tText("LEAK-garbled")}})
	f.addMessage(ref.ID, map[string]any{"parts": []any{tText("LEAK-no-info")}})
	f.addMessage(ref.ID, map[string]any{"info": map[string]any{"id": "msg_05", "role": "system", "time": map[string]any{"created": 8}}, "parts": []any{tText("LEAK-system-role")}})
	f.addMessage(ref.ID, map[string]any{"info": map[string]any{"role": "user", "time": map[string]any{"created": 9}}, "parts": []any{tText("LEAK-no-id")}})
	f.addMessage(ref.ID, bad(map[string]any{"info": map[string]any{"id": "msg_06", "role": "user", "time": map[string]any{"created": 10}}, "parts": "LEAK-parts-not-a-list"}))
	f.addMessage(ref.ID, tAsst("msg_07", 11, 12, tText("the only entry")))

	for _, limit := range []int{1, 2, 100} {
		p := getRecord(t, d, ref, "", limit)
		if got, want := shapes(p), []string{"text/assistant:the only entry"}; !eqStrings(got, want) {
			t.Fatalf("limit %d: record = %q, want %q", limit, got, want)
		}
		if strings.Contains(mustJSON(t, p), "LEAK") {
			t.Fatalf("limit %d: a page carries content outside the allow-list: %s", limit, mustJSON(t, p))
		}
	}
	// And from the very beginning of the record, by cursor.
	g := newFakeServer(t)
	d2 := newTestDriver(t, g)
	ref2 := createOne(t, d2, "/work/x", "k2")
	start := getRecord(t, d2, ref2, "", 0).Next
	g.mu.Lock()
	g.messages[ref2.ID] = append(g.messages[ref2.ID], f.messages[ref.ID]...)
	g.mu.Unlock()
	if got, want := shapes(getRecord(t, d2, ref2, start, 100)), []string{"text/assistant:the only entry"}; !eqStrings(got, want) {
		t.Fatalf("from the start: record = %q, want %q", got, want)
	}
}

// A message whose shape cannot be read beyond an id and a role is passed over, even
// when it is the last assistant message: it never holds back what follows it.
func TestRecord_AGarbledMessageDoesNotBlockWhatFollowsIt(t *testing.T) {
	f, d, ref := newRecordSession(t)
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q1"))
	f.addMessage(ref.ID, map[string]any{"info": map[string]any{"id": "msg_a1", "role": "assistant", "modelID": "m", "time": "LEAK-not-an-object"},
		"parts": []any{tText("LEAK-garbled-assistant")}})
	f.addMessage(ref.ID, tUser("msg_u2", 3, "q2"))
	p := getRecord(t, d, ref, "", 0)
	if got, want := shapes(p), []string{"text/user:q1", "text/user:q2"}; !eqStrings(got, want) {
		t.Fatalf("record = %q, want %q", got, want)
	}
	if strings.Contains(mustJSON(t, p), "LEAK") {
		t.Errorf("a garbled message's content went out: %s", mustJSON(t, p))
	}
}

// Walking a record page by page returns every entry exactly once, in order — also
// when a page ends between a call and the images it returned, and when two calls run
// at once and finish later — and an empty record is a page, not an error.
func TestRecord_WalkingByPagesReturnsEveryEntryOnce(t *testing.T) {
	f, d, ref := newRecordSession(t)
	empty := getRecord(t, d, ref, "", 0)
	if len(empty.Messages) != 0 || empty.Next == "" {
		t.Fatalf("an empty record = %+v, want no messages and a cursor", empty)
	}

	_, pngURL := pngData()
	_, jpegURL := jpegData()
	f.addMessage(ref.ID, tUser("msg_u1", 1, "q1"))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 3, tText("a1"),
		rTool("call_1", "read", map[string]any{"status": "completed", "input": map[string]any{}, "output": "o", "title": "t", "metadata": map[string]any{},
			"time": map[string]any{"start": 2, "end": 3}, "attachments": []any{rFile("image/png", pngURL), rFile("image/jpeg", jpegURL)}}),
		tText("a1 again")))
	f.addMessage(ref.ID, tUser("msg_u2", 4, "q2"))
	f.addMessage(ref.ID, tAsst("msg_a2", 5, 0,
		rTool("call_2", "bash", rRunning(map[string]any{"n": 2}, 5)),
		rTool("call_3", "bash", rRunning(map[string]any{"n": 3}, 6))))

	whole := getRecord(t, d, ref, empty.Next, 100)
	wantWhole := []string{
		"text/user:q1", "text/assistant:a1", "tool/assistant:call_1:completed", "image/assistant", "image/assistant", "text/assistant:a1 again",
		"text/user:q2", "tool/assistant:call_2:running", "tool/assistant:call_3:running",
	}
	if got := shapes(whole); !eqStrings(got, wantWhole) {
		t.Fatalf("whole = %q, want %q", got, wantWhole)
	}

	var walked []string
	cursor := empty.Next
	for i := 0; i < 40; i++ {
		p := getRecord(t, d, ref, cursor, 1)
		if len(p.Messages) > 1 {
			t.Fatalf("limit 1 gave %d entries", len(p.Messages))
		}
		walked = append(walked, shapes(p)...)
		if len(p.Messages) == 0 {
			break
		}
		cursor = p.Next
	}
	if !eqStrings(walked, wantWhole) {
		t.Fatalf("walked one at a time = %q, want %q", walked, wantWhole)
	}

	// Both calls finish: each goes out again, complete, one per page, and once.
	f.deleteMessage(ref.ID, "msg_a2")
	f.addMessage(ref.ID, tAsst("msg_a2", 5, 20,
		rTool("call_2", "bash", rDone(map[string]any{"n": 2}, "two", 5, 10)),
		rTool("call_3", "bash", rDone(map[string]any{"n": 3}, "three", 6, 11))))
	var after []string
	for i := 0; i < 10; i++ {
		p := getRecord(t, d, ref, cursor, 1)
		if len(p.Messages) == 0 {
			break
		}
		after = append(after, shapes(p)...)
		cursor = p.Next
	}
	if want := []string{"tool/assistant:call_2:completed", "tool/assistant:call_3:completed"}; !eqStrings(after, want) {
		t.Fatalf("after both finished = %q, want %q", after, want)
	}
}

func TestRecord_MostRecentFirstThenForwardFromNext(t *testing.T) {
	f, d, ref := newRecordSession(t)
	for n := 1; n <= 5; n++ {
		addTurn(f, ref.ID, n, fmt.Sprintf("answer %d", n))
	}
	p := getRecord(t, d, ref, "", 3)
	if got, want := shapes(p), []string{"text/assistant:answer 4", "text/user:question 5", "text/assistant:answer 5"}; !eqStrings(got, want) {
		t.Fatalf("most recent 3 = %q, want %q", got, want)
	}
	idle := getRecord(t, d, ref, p.Next, 0)
	if len(idle.Messages) != 0 || idle.Next != p.Next {
		t.Fatalf("an idle poll = %+v, want nothing and the same next", idle)
	}
	addTurn(f, ref.ID, 6, "answer 6")
	more := getRecord(t, d, ref, p.Next, 0)
	if got, want := shapes(more), []string{"text/user:question 6", "text/assistant:answer 6"}; !eqStrings(got, want) {
		t.Fatalf("since next = %q, want %q", got, want)
	}

	// The default page is 20, and the most recent are what comes back.
	g, d2, ref2 := newRecordSession(t)
	for n := 1; n <= 30; n++ {
		addTurn(g, ref2.ID, n, fmt.Sprintf("answer %d", n))
	}
	big := getRecord(t, d2, ref2, "", 0)
	if len(big.Messages) != fleet.DefaultRecordLimit || big.Messages[19].Text != "answer 30" || big.Messages[0].Text != "question 21" {
		t.Fatalf("default page = %d entries %q..%q", len(big.Messages), big.Messages[0].Text, big.Messages[len(big.Messages)-1].Text)
	}
}

// A cursor far behind is found by widening the tail window, as turns does it.
func TestRecord_FindsAnAnchorBeyondTheFirstWindow(t *testing.T) {
	f, d, ref := newRecordSession(t)
	for n := 1; n <= 3; n++ {
		addTurn(f, ref.ID, n, fmt.Sprintf("answer %d", n))
	}
	cursor := getRecord(t, d, ref, "", 0).Next
	for n := 4; n <= 150; n++ {
		addTurn(f, ref.ID, n, fmt.Sprintf("answer %d", n))
	}
	before := len(f.requestsSnapshot())
	p := getRecord(t, d, ref, cursor, 100)
	if len(p.Messages) != 100 || p.Messages[0].Text != "question 4" || p.Messages[99].Text != "answer 53" {
		t.Fatalf("page = %d entries, %q..%q", len(p.Messages), p.Messages[0].Text, p.Messages[len(p.Messages)-1].Text)
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
}

// The cursor is bound to the conversation, a stale one is the caller's belief and not a
// malformed request, and the record's cursor and turns' are not interchangeable.
func TestRecord_CursorRules(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	a := createOne(t, d, "/work/a", "ka")
	b := createOne(t, d, "/work/b", "kb")
	addTurn(f, a.ID, 1, "a1")
	addTurn(f, a.ID, 2, "a2")
	addTurn(f, b.ID, 1, "b1")
	req := fleet.RequestFrom(fleet.Caller{})

	pa := getRecord(t, d, a, "", 0)
	if _, err := d.Record(context.Background(), req, b, fleet.RecordQuery{Since: pa.Next}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor from another session: %v, want ErrAmbiguousTarget", err)
	}
	forged := recordCursor{M: "msg_a0002", P: -1}.encode(recordScope{session: a.ID, created: f.sessions[a.ID].Time.Created + 1})
	if _, err := d.Record(context.Background(), req, a, fleet.RecordQuery{Since: forged}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor from an earlier incarnation of the id: %v, want ErrAmbiguousTarget", err)
	}
	// A position past the end of the message it names: the record changed under the caller.
	scope := recordScope{session: a.ID, created: f.sessions[a.ID].Time.Created}
	if _, err := d.Record(context.Background(), req, a, fleet.RecordQuery{Since: recordCursor{M: "msg_a0002", P: 9}.encode(scope)}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a position the message does not have: %v, want ErrAmbiguousTarget", err)
	}
	f.deleteMessage(a.ID, "msg_a0002")
	if _, err := d.Record(context.Background(), req, a, fleet.RecordQuery{Since: pa.Next}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor whose message was deleted: %v, want ErrAmbiguousTarget", err)
	}

	// Not a cursor at all — and a turns cursor is not one — is a plain refusal.
	turnsC := getTurns(t, d, b, "", 0).Next
	junk := []string{"not-a-cursor", "%%%", turnsC,
		recordCursor{M: "", P: 3}.encode(scope),
		recordCursor{M: "m", P: -2}.encode(scope),
		recordCursor{M: "m", P: -1, W: make([]recordPending, recordMaxPending+1)}.encode(scope)}
	for _, since := range junk {
		_, err := d.Record(context.Background(), req, a, fleet.RecordQuery{Since: since})
		if err == nil || errors.Is(err, fleet.ErrAmbiguousTarget) || !strings.Contains(err.Error(), "cursor") {
			t.Errorf("since=%q: %v, want a plain refusal naming the cursor", since, err)
		}
	}
	// And the other way: turns refuses a record cursor.
	if _, err := d.Turns(context.Background(), req, b, fleet.TurnsQuery{Since: getRecord(t, d, b, "", 0).Next}); err == nil || errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("turns accepted a record cursor: %v", err)
	}
}

func TestRecord_CorroboratesStartedAtBeforeReadingAnything(t *testing.T) {
	f, d, ref := newRecordSession(t)
	addTurn(f, ref.ID, 1, "answer 1")
	started := time.UnixMilli(f.sessions[ref.ID].Time.Created)

	before := len(f.requestsSnapshot())
	wrong := started.Add(time.Hour)
	req := fleet.RequestFrom(fleet.Caller{})
	req.Expect.StartedAt = &wrong
	if _, err := d.Record(context.Background(), req, ref, fleet.RecordQuery{}); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Fatalf("a startedAt that is not this session's: %v, want ErrAmbiguousTarget", err)
	}
	for _, r := range f.requestsSnapshot()[before:] {
		if strings.HasSuffix(r.path, "/message") {
			t.Fatalf("the message record was read (%s) before the startedAt was corroborated", r.path)
		}
	}
	req.Expect.StartedAt = &started
	if p, err := d.Record(context.Background(), req, ref, fleet.RecordQuery{}); err != nil || len(p.Messages) != 2 {
		t.Fatalf("a matching startedAt: %v, %+v", err, p)
	}
}

func TestRecord_RefusalsAndAbsences(t *testing.T) {
	f, d, ref := newRecordSession(t)
	req := fleet.RequestFrom(fleet.Caller{})

	if _, err := d.Record(context.Background(), req, fleet.SessionRef{ID: "ses_never_seen"}, fleet.RecordQuery{}); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("an id this driver never saw: %v, want ErrNoSuchSession", err)
	}
	for _, limit := range []int{-1, fleet.MaxRecordLimit + 1} {
		if _, err := d.Record(context.Background(), req, ref, fleet.RecordQuery{Limit: limit}); err == nil {
			t.Errorf("limit %d was accepted; an over-large page is refused, never clamped", limit)
		}
	}
	f.mu.Lock()
	delete(f.sessions, ref.ID)
	f.mu.Unlock()
	if _, err := d.Record(context.Background(), req, ref, fleet.RecordQuery{}); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("a session the runtime no longer holds: %v, want ErrNoSuchSession", err)
	}
	d.markSeen("ses_listed", knownSession{unknown: "needs re-creation"})
	_, err := d.Record(context.Background(), req, fleet.SessionRef{ID: "ses_listed"}, fleet.RecordQuery{})
	if kindOfErr(err) != fleet.ErrorConflict {
		t.Errorf("a session listed but not running: %v, want a conflict naming the reason", err)
	}
}

// A field over the bound is cut on a character and marked: a text, a tool's input
// (which then reads as a JSON string of the leading part of it) and its result.
func TestRecord_AFieldOverTheBoundIsCutOnACharacterAndMarked(t *testing.T) {
	f, d, ref := newRecordSession(t)
	long := strings.Repeat("あ", fleet.MaxRecordFieldBytes/3+10) // 3 bytes each, over the bound
	f.addMessage(ref.ID, tUser("msg_u1", 1, long))
	f.addMessage(ref.ID, tAsst("msg_a1", 2, 3,
		rTool("call_1", "write", rDone(map[string]any{"content": long}, long, 2, 3)),
		tText("short")))

	p := getRecord(t, d, ref, "", 0)
	if len(p.Messages) != 3 {
		t.Fatalf("entries = %d", len(p.Messages))
	}
	if u := p.Messages[0]; !u.Truncated || len(u.Text) > fleet.MaxRecordFieldBytes || !utf8.ValidString(u.Text) {
		t.Errorf("long text: truncated=%v len=%d valid=%v", u.Truncated, len(u.Text), utf8.ValidString(u.Text))
	}
	tool := p.Messages[1]
	var input string
	if err := json.Unmarshal(tool.Tool.Input, &input); err != nil || !strings.HasPrefix(input, `{"content":"`) || !tool.Truncated {
		t.Errorf("a long input must read as a JSON string of its leading part and the message as truncated: %v truncated=%v", err, tool.Truncated)
	}
	if len(tool.Tool.Result) > fleet.MaxRecordFieldBytes || !utf8.ValidString(tool.Tool.Result) {
		t.Errorf("long result: len=%d valid=%v", len(tool.Tool.Result), utf8.ValidString(tool.Tool.Result))
	}
	if p.Messages[2].Truncated {
		t.Error("a short text was marked truncated")
	}
}

// The service finds these capabilities by asserting the interfaces; a driver that loses
// one silently answers `unsupported` again.
func TestRecord_IsAdvertisedByImplementingTheInterfaces(t *testing.T) {
	f := newFakeServer(t)
	var d driver.Driver = newTestDriver(t, f)
	if _, ok := d.(driver.RecordReader); !ok {
		t.Fatal("the opencode driver does not implement driver.RecordReader; /record would answer unsupported")
	}
	if _, ok := d.(driver.RecordMediaReader); !ok {
		t.Fatal("the opencode driver does not implement driver.RecordMediaReader; the media route would answer unsupported")
	}
}
