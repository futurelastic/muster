package tmux

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Every fixture here is SYNTHETIC, like turns_test.go's: the entry shapes were
// read off the runtime's own writer, the words were made up for this file.

// user is a human-typed user entry whose content is a plain string.
func user(t *testing.T, i int, text string, extra map[string]any) string {
	t.Helper()
	m := map[string]any{
		"type": "user", "timestamp": tsAt(i),
		"message": map[string]any{"role": "user", "content": text},
	}
	for k, v := range extra {
		m[k] = v
	}
	return jsonLine(t, m)
}

// userBlocks is a user entry whose content is a list of blocks.
func userBlocks(t *testing.T, i int, extra map[string]any, blocks ...map[string]any) string {
	t.Helper()
	m := map[string]any{
		"type": "user", "timestamp": tsAt(i),
		"message": map[string]any{"role": "user", "content": blocks},
	}
	for k, v := range extra {
		m[k] = v
	}
	return jsonLine(t, m)
}

func recordTexts(p fleet.RecordPage) []string {
	out := make([]string, 0, len(p.Messages))
	for _, m := range p.Messages {
		out = append(out, string(m.Role)+":"+m.Text)
	}
	return out
}

// The first oracle: a record holding every category the ruling still keeps out,
// in the runtime's own shapes, returns the text of the conversation in both
// directions and not one marker of anything else. (What #442 admits, a tool call
// with its result, reasoning, a sub-task, a summary and an image, has its own
// tests in recordsources_test.go and recordmedia_test.go.)
func TestReadRecord_ExcludedStayOut(t *testing.T) {
	const leak = "LEAK-MARKER"
	lines := []string{
		user(t, 1, "user-prompt-string", nil),
		userBlocks(t, 2, nil, textBlock("user-prompt-blocks")),
		assistant(t, 3, textBlock("assistant-text")),
		// the assistant's reasoning, redacted or empty: nothing to carry
		assistant(t, 4, map[string]any{"type": "thinking", "thinking": ""}),
		assistant(t, 4, map[string]any{"type": "redacted_thinking", "data": leak}),
		// a tool call with no usable id or name
		assistant(t, 5, map[string]any{"type": "tool_use", "id": "", "name": "Bash", "input": map[string]any{"command": leak}}),
		assistant(t, 5, map[string]any{"type": "tool_use", "id": "t0", "name": "", "input": map[string]any{"command": leak}}),
		// a result line that mixes a result with text
		userBlocks(t, 6, nil,
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": leak + " mixed"}, textBlock(leak+" beside a result")),
		user(t, 7, leak+" meta", map[string]any{"isMeta": true}),
		user(t, 8, leak+" sub-agent prompt", map[string]any{"isSidechain": true}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(9), "isSidechain": true,
			"message": map[string]any{"role": "assistant", "model": "model-x", "content": []map[string]any{textBlock(leak + " sub-agent")}}}),
		user(t, 10, leak+" summary that is also meta", map[string]any{"isCompactSummary": true, "isMeta": true}),
		user(t, 11, leak+" viewer only", map[string]any{"isVisibleInTranscriptOnly": true}),
		user(t, 12, "<command-name>/clear</command-name> "+leak, nil),
		user(t, 13, "<local-command-stdout>"+leak+"</local-command-stdout>", nil),
		user(t, 14, "<bash-input>"+leak+"</bash-input>", nil),
		user(t, 15, "<system-reminder>"+leak+"</system-reminder>", nil),
		user(t, 16, "[Request interrupted by user]"+leak, nil),
		user(t, 17, leak+" from a peer", map[string]any{"origin": map[string]any{"kind": "peer"}}),
		user(t, 18, leak+" other source", map[string]any{"promptSource": "other"}),
		// text beside a block this reader does not read
		userBlocks(t, 19, nil, textBlock(leak+" with a document"), map[string]any{"type": "document", "source": map[string]any{"type": "base64", "data": leak}}),
		jsonLine(t, map[string]any{"type": "system", "subtype": "hook", "timestamp": tsAt(20), "content": leak + " hook"}),
		jsonLine(t, map[string]any{"type": "attachment", "timestamp": tsAt(21), "attachment": map[string]any{"text": leak}}),
		jsonLine(t, map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": tsAt(22), "content": leak + " queued"}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(23), "isApiErrorMessage": true,
			"message": map[string]any{"role": "assistant", "model": "<synthetic>", "content": []map[string]any{textBlock(leak + " api error")}}}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(24),
			"message": map[string]any{"role": "assistant", "model": "model-x", "content": leak + " plain string content"}}),
		`not json at all ` + leak,
		// a user entry whose human origin is explicit and whose source is typed
		user(t, 25, "explicit-human", map[string]any{"origin": map[string]any{"kind": "human"}, "promptSource": "typed"}),
		// a user entry with an origin of an unexpected shape fails closed
		user(t, 26, leak+" odd origin", map[string]any{"origin": "human"}),
		// a person's text whose structured tool result is attached to the line
		user(t, 27, leak+" with a toolUseResult", map[string]any{"toolUseResult": map[string]any{"stdout": leak}}),
	}
	page, err := readRecord(writeTurnsRecord(t, lines...), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"user:user-prompt-string", "user:user-prompt-blocks", "assistant:assistant-text", "user:explicit-human",
	}
	if got := recordTexts(page); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	if page.Format != fleet.RecordFormat {
		t.Errorf("format = %d, want %d", page.Format, fleet.RecordFormat)
	}
	for _, m := range page.Messages {
		if m.Kind != fleet.RecordKindText {
			t.Errorf("kind = %q, want text", m.Kind)
		}
		if m.Tool != nil || m.Parent != "" || m.Media != "" {
			t.Errorf("a message carries a field nothing here should fill: %+v", m)
		}
		switch m.Role {
		case fleet.RecordRoleAssistant:
			if m.Model != "model-x" {
				t.Errorf("assistant model = %q, want model-x", m.Model)
			}
		case fleet.RecordRoleUser:
			if m.Model != "" {
				t.Errorf("a user entry carries a model: %q", m.Model)
			}
		default:
			t.Errorf("role = %q", m.Role)
		}
	}
	if strings.Contains(string(mustJSON(t, page)), leak) {
		t.Errorf("the page leaks a marker of an excluded category: %s", mustJSON(t, page))
	}
	if page.Next == "" {
		t.Error("next is empty; a poller needs a place to resume")
	}
}

// A page is exactly what turns' predicate says for the assistant half: the two
// routes cannot disagree about what the agent said.
func TestReadRecord_AssistantHalfIsTurnsPredicate(t *testing.T) {
	lines := []string{
		assistant(t, 1, textBlock("one")),
		assistant(t, 2, map[string]any{"type": "thinking", "thinking": "x"}, textBlock("two"), map[string]any{"type": "tool_use", "id": "a", "name": "n", "input": map[string]any{}}),
		assistant(t, 3, textBlock("three")),
	}
	p := writeTurnsRecord(t, lines...)
	rec, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	turns, err := readTurns(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The record also carries the reasoning and the tool call on the second line
	// (#442); what must agree is the agent's text.
	var texts []fleet.RecordMessage
	for _, m := range rec.Messages {
		if m.Kind == fleet.RecordKindText && m.Role == fleet.RecordRoleAssistant {
			texts = append(texts, m)
		}
	}
	if len(texts) != len(turns.Turns) {
		t.Fatalf("record %d assistant text messages, turns %d", len(texts), len(turns.Turns))
	}
	for i := range turns.Turns {
		if texts[i].Text != turns.Turns[i].Text || !texts[i].At.Equal(turns.Turns[i].At) || texts[i].Truncated != turns.Turns[i].Truncated {
			t.Errorf("entry %d differs: record %+v, turn %+v", i, texts[i], turns.Turns[i])
		}
	}
}

func TestReadRecord_CursorResumesExactlyAfterTheLastEntryAcrossAppends(t *testing.T) {
	p := writeTurnsRecord(t,
		user(t, 1, "a", nil), assistant(t, 2, textBlock("b")), user(t, 3, "c", nil), assistant(t, 4, textBlock("d")))

	tail, err := readRecord(p, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(tail), "|"); got != "user:c|assistant:d" {
		t.Fatalf("the most recent two = %q", got)
	}
	caughtUp, err := readRecord(p, tail.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(caughtUp.Messages) != 0 || caughtUp.Next == "" {
		t.Fatalf("caught up: %d messages, next %q; want none and a cursor", len(caughtUp.Messages), caughtUp.Next)
	}

	// Four more, with noise between them: the first read of two takes the first
	// two admitted entries, the next takes exactly the rest.
	for _, l := range []string{
		user(t, 5, "e", nil),
		jsonLine(t, map[string]any{"type": "mode"}),
		assistant(t, 6, textBlock("f")),
		jsonLine(t, map[string]any{"type": "attachment", "timestamp": tsAt(7)}),
		user(t, 8, "g", nil),
		assistant(t, 9, textBlock("h")),
	} {
		appendLine(t, p, l)
	}
	first, err := readRecord(p, caughtUp.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(first), "|"); got != "user:e|assistant:f" {
		t.Fatalf("first page after appends = %q", got)
	}
	second, err := readRecord(p, first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(second), "|"); got != "user:g|assistant:h" {
		t.Fatalf("second page = %q, want exactly the rest, nothing repeated or skipped", got)
	}
	appendLine(t, p, user(t, 10, "i", nil))
	last, err := readRecord(p, second.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(last), "|"); got != "user:i" {
		t.Fatalf("after one more = %q", got)
	}
}

// A line the runtime is still writing is never returned half-parsed, and is
// returned once, whole, after it is finished.
func TestReadRecord_PartialTrailingLineIsNotConsumed(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "done", nil))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	whole := user(t, 2, "later", nil)
	if _, err := f.WriteString(whole[:len(whole)/2]); err != nil {
		t.Fatal(err)
	}
	first, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(first), "|"); got != "user:done" {
		t.Fatalf("with a partial last line = %q", got)
	}
	if _, err := f.WriteString(whole[len(whole)/2:] + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	next, err := readRecord(p, first.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(next), "|"); got != "user:later" {
		t.Fatalf("after the line was finished = %q", got)
	}
}

func TestReadRecord_ForeignCursorIsConflictGarbageIsInvalid(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "a", nil))

	_, err := readRecord(p, offsetCursor(recordCursorVersion, "some-other-conversation", 0), 10)
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor for another conversation: err = %v, want ErrAmbiguousTarget", err)
	}
	_, err = readRecord(p, offsetCursor(recordCursorVersion, "conv-1", 1<<20), 10)
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a cursor past the end: err = %v, want ErrAmbiguousTarget", err)
	}
	for name, bad := range map[string]string{
		"a turns cursor":  turnsCursor("conv-1", 0),
		"garbage":         "Zm9v",
		"invalid base64":  "!!!",
		"two parts only":  base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion + "|conv-1")),
		"offset not int":  base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion + "|conv-1|abc")),
		"negative offset": offsetCursor(recordCursorVersion, "conv-1", -1),
	} {
		_, err := readRecord(p, bad, 10)
		if err == nil || errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("%s: err = %v, want a plain refusal that is not a conflict", name, err)
		}
	}
	// and the other way round: a record cursor is not a turns cursor
	rec, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readTurns(p, rec.Next, 10); err == nil || errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("a record cursor given to turns: err = %v, want a plain refusal", err)
	}
}

func TestReadRecord_TruncatesAt64KiBOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", fleet.MaxRecordFieldBytes) // two bytes each: well over the cap
	p := writeTurnsRecord(t,
		user(t, 1, long, nil),
		assistant(t, 2, textBlock(long)),
		user(t, 3, "short", nil))
	page, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("%d messages, want 3", len(page.Messages))
	}
	for i, m := range page.Messages[:2] {
		if !m.Truncated {
			t.Errorf("message %d not marked truncated", i)
		}
		if len(m.Text) > fleet.MaxRecordFieldBytes {
			t.Errorf("message %d is %d bytes, over the cap", i, len(m.Text))
		}
		if !strings.HasSuffix(m.Text, "é") {
			t.Errorf("message %d was cut inside a rune", i)
		}
	}
	if page.Messages[2].Truncated {
		t.Error("a short message is marked truncated")
	}

	// A line over the reader's own limit is skipped whole, without an error.
	huge := user(t, 4, strings.Repeat("x", recordEntryLineLimit+1), nil)
	p2 := writeTurnsRecord(t, user(t, 1, "before", nil), huge, user(t, 5, "after", nil))
	page, err = readRecord(p2, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(page), "|"); got != "user:before|user:after" {
		t.Errorf("around an oversized line = %q", got)
	}
}

func TestReadRecord_EmptyAndMissing(t *testing.T) {
	page, err := readRecord(writeTurnsRecord(t, `{"type":"mode"}`), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Messages == nil || len(page.Messages) != 0 || page.Next == "" || page.Format != fleet.RecordFormat {
		t.Errorf("empty record: %+v, want non-nil empty messages, a cursor and the format", page)
	}
	if _, err := readRecord(filepath.Join(t.TempDir(), "absent.jsonl"), "", 5); !errors.Is(err, fleet.ErrNoTurnRecord) {
		t.Errorf("missing file: err = %v, want ErrNoTurnRecord", err)
	}
}

func TestDriverRecord_ResolvesCorroboratesAndRefuses(t *testing.T) {
	d, _, ref, _, conv := titleSyncSession(t)
	ctx := t.Context()
	appendLine(t, conv, user(t, 1, "hello alpha", nil))
	appendLine(t, conv, assistant(t, 2, textBlock("hi from alpha")))
	appendLine(t, conv, assistant(t, 3, map[string]any{"type": "tool_use", "id": "t", "name": "n", "input": map[string]any{}}))

	page, err := d.Record(ctx, fleet.Request{}, ref, fleet.RecordQuery{})
	if err != nil {
		t.Fatal(err)
	}
	// The third line is a tool call: a `tool` message with no text (#442).
	if got := strings.Join(recordTexts(page), "|"); got != "user:hello alpha|assistant:hi from alpha|assistant:" {
		t.Fatalf("record = %q", got)
	}
	if last := page.Messages[len(page.Messages)-1]; last.Kind != fleet.RecordKindTool || last.Tool == nil || last.Tool.Status != fleet.RecordToolRunning {
		t.Fatalf("the last message is %+v, want a running tool call", last)
	}
	appendLine(t, conv, user(t, 4, "and later", nil))
	later, err := d.Record(ctx, fleet.Request{}, ref, fleet.RecordQuery{Since: page.Next})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recordTexts(later), "|"); got != "user:and later" {
		t.Fatalf("after the cursor = %q", got)
	}

	if _, err := d.Record(ctx, withStartedAt(fleet.Request{}, startedAtFixture()), ref, fleet.RecordQuery{}); err != nil {
		t.Errorf("matching startedAt: %v", err)
	}
	_, err = d.Record(ctx, withStartedAt(fleet.Request{}, startedAtFixture().Add(time.Hour)), ref, fleet.RecordQuery{})
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("recycled id: err = %v, want ErrAmbiguousTarget", err)
	}
	_, err = d.Record(ctx, fleet.Request{}, fleet.SessionRef{Machine: "testbox", ID: "nobody"}, fleet.RecordQuery{})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("unknown session: err = %v, want ErrNoSuchSession", err)
	}
	_, err = d.Record(ctx, fleet.Request{}, fleet.SessionRef{Machine: "testbox", ID: "beta"}, fleet.RecordQuery{})
	if !errors.Is(err, fleet.ErrNoTurnRecord) {
		t.Errorf("session with no record: err = %v, want ErrNoTurnRecord", err)
	}
	if _, err := d.Record(ctx, fleet.Request{}, ref, fleet.RecordQuery{Limit: fleet.MaxRecordLimit + 1}); err == nil {
		t.Error("an over-cap limit was accepted")
	}
}

func TestDriverRecord_CapabilityFollowsTurnsAndInterfaceIsImplemented(t *testing.T) {
	d, _, _, _, _ := titleSyncSession(t)
	if _, ok := any(d).(driver.RecordReader); !ok {
		t.Fatal("the terminal driver does not implement driver.RecordReader")
	}
	caps := d.Capabilities()
	if caps.ReadsRecord == nil || caps.ReadsTurns == nil || *caps.ReadsRecord != *caps.ReadsTurns {
		t.Errorf("ReadsRecord = %v, ReadsTurns = %v; the two read one file and are declared alike", caps.ReadsRecord, caps.ReadsTurns)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
