package tmux

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Every fixture here is SYNTHETIC, like record_test.go's: the entry shapes were
// read off the runtime's own writer, the words were made up for this file, and
// no test reads a real transcript.

func toolUse(t *testing.T, i int, id, name string, input any) string {
	t.Helper()
	return assistant(t, i, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
}

// toolResult is the user-role line the runtime writes for a tool's result.
func toolResult(t *testing.T, i int, id string, content any, isErr bool) string {
	t.Helper()
	b := map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	if isErr {
		b["is_error"] = true
	}
	return userBlocks(t, i, map[string]any{"toolUseResult": map[string]any{"structured": "never read"}, "sourceToolAssistantUUID": "u-1"}, b)
}

func thinkingBlock(s string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": s, "signature": "sig"}
}

// pngData is base64 of a PNG signature and some padding bytes: not a picture, but
// what this reader checks (the signature and the size) is satisfied.
func pngData() string {
	return base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
}

func imageBlockOf(media, data string) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": data}}
}

func subAssistant(t *testing.T, agent string, i int, blocks ...map[string]any) string {
	return jsonLine(t, map[string]any{
		"type": "assistant", "timestamp": tsAt(i), "isSidechain": true, "agentId": agent, "sessionId": "conv-1",
		"message": map[string]any{"role": "assistant", "model": "model-y", "content": blocks},
	})
}

func subUser(t *testing.T, agent string, i int, content any) string {
	return jsonLine(t, map[string]any{
		"type": "user", "timestamp": tsAt(i), "isSidechain": true, "agentId": agent, "sessionId": "conv-1",
		"message": map[string]any{"role": "user", "content": content},
	})
}

func subToolResult(t *testing.T, agent string, i int, id string, content any) string {
	return jsonLine(t, map[string]any{
		"type": "user", "timestamp": tsAt(i), "isSidechain": true, "agentId": agent, "sessionId": "conv-1",
		"toolUseResult": map[string]any{"x": 1},
		"message": map[string]any{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": id, "content": content}}},
	})
}

// writeSub writes one sub-task file beside a conversation file, with the sidecar
// the runtime writes next to it. A zero mtime leaves the file time alone.
func writeSub(t *testing.T, conv, agent, parent string, finished bool, mtime time.Time, lines ...string) string {
	t.Helper()
	dir := subTaskDir(conv)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "agent-"+agent+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"toolUseId": parent, "finished": finished, "agentType": "Explore", "spawnDepth": 1})
	if err := os.WriteFile(filepath.Join(dir, "agent-"+agent+".meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// rec keys one message for comparisons: kind, role, parent, and what identifies it.
func rec(m fleet.RecordMessage) string {
	id := m.Text
	switch {
	case m.Tool != nil:
		id = m.Tool.ID + "/" + m.Tool.Status
	case m.Kind == fleet.RecordKindImage:
		id = "image"
	}
	p := ""
	if m.Parent != "" {
		p = "@" + m.Parent
	}
	return string(m.Kind) + ":" + string(m.Role) + p + ":" + id
}

func recs(p fleet.RecordPage) string {
	out := make([]string, 0, len(p.Messages))
	for _, m := range p.Messages {
		out = append(out, rec(m))
	}
	return strings.Join(out, "|")
}

// The oracle's own fixtures: a record with each kind the issue names.
func TestReadRecord_EachKind(t *testing.T) {
	img := pngData()
	p := writeTurnsRecord(t,
		user(t, 1, "please look", nil),
		assistant(t, 2, thinkingBlock("let me think")),
		assistant(t, 3, textBlock("looking")),
		toolUse(t, 4, "t1", "Bash", map[string]any{"command": "ls"}),
		toolResult(t, 5, "t1", "file-a\nfile-b", false),
		toolUse(t, 6, "t2", "Read", map[string]any{"file_path": "/nope"}),
		toolResult(t, 7, "t2", []map[string]any{{"type": "text", "text": "no such file"}}, true),
		user(t, 8, "This session is being continued from an earlier one. Summary: x.",
			map[string]any{"isCompactSummary": true, "isVisibleInTranscriptOnly": true}),
		userBlocks(t, 9, nil, textBlock("here is a picture"), imageBlockOf("image/png", img)),
		toolUse(t, 10, "t3", "Screenshot", map[string]any{}),
		toolResult(t, 11, "t3", []map[string]any{{"type": "text", "text": "captured"}, imageBlockOf("image/png", img)}, false),
	)
	page, err := readRecord(p, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"text:user:please look",
		"reasoning:assistant:let me think",
		"text:assistant:looking",
		"tool:assistant:t1/completed",
		"tool:assistant:t2/error",
		"summary:assistant:This session is being continued from an earlier one. Summary: x.",
		"text:user:here is a picture",
		"image:user:image",
		"tool:assistant:t3/completed",
		"image:assistant:image",
	}, "|")
	if got := recs(page); got != want {
		t.Fatalf("record =\n %s\nwant\n %s", got, want)
	}
	byID := map[string]fleet.RecordMessage{}
	for _, m := range page.Messages {
		if m.Tool != nil {
			byID[m.Tool.ID] = m
		}
		if m.Kind == fleet.RecordKindImage && m.Media == "" {
			t.Errorf("an image entry has no media reference: %+v", m)
		}
		if m.Kind != fleet.RecordKindImage && m.Media != "" {
			t.Errorf("a %s entry carries a media reference", m.Kind)
		}
	}
	if m := byID["t1"]; m.Tool.Name != "Bash" || string(m.Tool.Input) != `{"command":"ls"}` || m.Tool.Result != "file-a\nfile-b" || !m.At.Equal(mustUTC(tsAt(4))) {
		t.Errorf("t1 = %+v (input %s); want the call's own time and name, input and result", m.Tool, m.Tool.Input)
	}
	if m := byID["t2"]; m.Tool.Result != "no such file" || m.Tool.Status != fleet.RecordToolError {
		t.Errorf("t2 = %+v", m.Tool)
	}
	if m := byID["t3"]; m.Tool.Result != "captured" {
		t.Errorf("t3 result = %q, want the text blocks only", m.Tool.Result)
	}
	if strings.Contains(string(mustJSON(t, page)), img) {
		t.Error("an image's bytes are inline in the page")
	}
}

func mustUTC(s string) time.Time {
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return tm.UTC()
}

func TestReadRecord_UnpairedCallIsRunning(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "go", nil), toolUse(t, 2, "t1", "Bash", map[string]any{"command": "sleep 99"}))
	page, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "text:user:go|tool:assistant:t1/running" {
		t.Fatalf("record = %s", got)
	}
	if m := page.Messages[1]; m.Tool.Result != "" {
		t.Errorf("a running call carries a result: %+v", m.Tool)
	}
}

// A result that arrives after its call was sent as running comes back as the same
// call, complete, under the same id and at the call's own time.
func TestReadRecord_ResultOnLaterPageReplacesTheCallByID(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "go", nil), toolUse(t, 2, "t1", "Bash", map[string]any{"command": "make"}))
	first, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	appendLine(t, p, assistant(t, 3, textBlock("waiting")))
	appendLine(t, p, toolResult(t, 4, "t1", "built", false))
	appendLine(t, p, assistant(t, 5, textBlock("done")))
	second, err := readRecord(p, first.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	// In the order the record holds them: the text, the update (it sits where the
	// result is), the text.
	if got := recs(second); got != "text:assistant:waiting|tool:assistant:t1/completed|text:assistant:done" {
		t.Fatalf("second page = %s", got)
	}
	upd := second.Messages[1]
	if upd.Tool.Result != "built" || upd.Tool.Name != "Bash" || string(upd.Tool.Input) != `{"command":"make"}` {
		t.Errorf("update = %+v, want the whole call with its result", upd.Tool)
	}
	if !upd.At.Equal(first.Messages[1].At) {
		t.Errorf("update at %v, want the call's own %v", upd.At, first.Messages[1].At)
	}
}

// A page that ends between a call and its result sends the call running; a page
// that holds both sends it once, complete.
func TestReadRecord_ResultFoldedOnlyWhenTheCursorPassesIt(t *testing.T) {
	lines := []string{
		toolUse(t, 1, "t1", "Bash", map[string]any{}),
		assistant(t, 2, textBlock("between")),
		toolResult(t, 3, "t1", "r1", false),
		assistant(t, 4, textBlock("after")),
	}
	p := writeTurnsRecord(t, lines...)

	whole, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(whole); got != "tool:assistant:t1/completed|text:assistant:between|text:assistant:after" {
		t.Fatalf("whole = %s", got)
	}

	// The tail read of two ends after "between" would NOT be the case here (the
	// tail takes the last two); read forward with a limit of two from the start of
	// the record instead.
	start := offsetCursor(recordCursorVersion, "conv-1", 0)
	one, err := readRecord(p, start, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(one); got != "tool:assistant:t1/running|text:assistant:between" {
		t.Fatalf("first page = %s; the result line was not passed, so the call is running", got)
	}
	two, err := readRecord(p, one.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(two); got != "tool:assistant:t1/completed|text:assistant:after" {
		t.Fatalf("second page = %s; the update goes out once, then the rest", got)
	}
}

func TestReadRecord_ResultNeverAttachesToTheWrongCall(t *testing.T) {
	p := writeTurnsRecord(t,
		toolUse(t, 1, "t1", "A", map[string]any{}),
		toolUse(t, 2, "t2", "B", map[string]any{}),
		toolResult(t, 3, "t2", "for-b", false),
		toolResult(t, 4, "t1", "for-a", false),
		// a second result for t1, and a result for a call nobody made
		toolResult(t, 5, "t1", "again", false),
		toolResult(t, 6, "t9", "stray", false),
	)
	page, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "tool:assistant:t1/completed|tool:assistant:t2/completed" {
		t.Fatalf("record = %s", got)
	}
	if page.Messages[0].Tool.Result != "for-a" || page.Messages[1].Tool.Result != "for-b" {
		t.Errorf("results = %q, %q; want for-a, for-b", page.Messages[0].Tool.Result, page.Messages[1].Tool.Result)
	}
	if strings.Contains(string(mustJSON(t, page)), "stray") || strings.Contains(string(mustJSON(t, page)), "again") {
		t.Errorf("a result that is not the first for its call, or for no call, is on the page: %s", mustJSON(t, page))
	}

	// The same on a later page: the duplicate and the stray are resolved against
	// the file and refused.
	p2 := writeTurnsRecord(t,
		toolUse(t, 1, "t1", "A", map[string]any{}),
		toolResult(t, 2, "t1", "first", false),
	)
	first, err := readRecord(p2, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	appendLine(t, p2, toolResult(t, 3, "t1", "second", false))
	appendLine(t, p2, toolResult(t, 4, "t7", "stray", false))
	later, err := readRecord(p2, first.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(later.Messages) != 0 {
		t.Errorf("a duplicate or stray result produced %s", recs(later))
	}
}

// A result whose call is far before the window the tail read starts at is still
// paired with it, by widening the search.
func TestReadRecord_TailResolvesAResultWhoseCallIsOutsideTheWindow(t *testing.T) {
	lines := []string{toolUse(t, 1, "t1", "Bash", map[string]any{"command": "long"})}
	pad := strings.Repeat("p", 2000)
	for i := 0; i < 400; i++ { // ~800 KiB between the call and its result
		lines = append(lines, user(t, 2, pad, nil))
	}
	lines = append(lines, toolResult(t, 3, "t1", "late", false))
	p := writeTurnsRecord(t, lines...)
	page, err := readRecord(p, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "tool:assistant:t1/completed" {
		t.Fatalf("tail = %s", got)
	}
	if page.Messages[0].Tool.Result != "late" || string(page.Messages[0].Tool.Input) != `{"command":"long"}` {
		t.Errorf("tool = %+v", page.Messages[0].Tool)
	}
}

func TestReadRecord_ToolInputOverTheCapIsCutAndSaysSo(t *testing.T) {
	big := strings.Repeat("x", fleet.MaxRecordFieldBytes+100)
	p := writeTurnsRecord(t,
		toolUse(t, 1, "t1", "Write", map[string]any{"content": big}),
		toolUse(t, 2, "t2", "Write", map[string]any{"content": "small"}),
		toolResult(t, 3, "t2", big, false))
	page, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	m := page.Messages[0]
	var s string
	if err := json.Unmarshal(m.Tool.Input, &s); err != nil || !m.Truncated || len(s) > fleet.MaxRecordFieldBytes || !strings.HasPrefix(s, `{"content":"xxx`) {
		t.Errorf("an over-cap input: truncated=%v, input is a JSON string = %v, len %d", m.Truncated, err == nil, len(s))
	}
	small := page.Messages[1]
	if string(small.Tool.Input) != `{"content":"small"}` || !small.Truncated || len(small.Tool.Result) > fleet.MaxRecordFieldBytes {
		t.Errorf("small input %s, truncated %v, result %d bytes: the result alone is over the cap", small.Tool.Input, small.Truncated, len(small.Tool.Result))
	}
}

func TestReadRecord_ImageLineOverOneMiBIsRead(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 2<<20)...))
	if len(big) <= recordLineLimit {
		t.Fatal("fixture too small")
	}
	p := writeTurnsRecord(t,
		user(t, 1, "before", nil),
		userBlocks(t, 2, nil, textBlock("the picture"), imageBlockOf("image/png", big)),
		user(t, 3, "after", nil))
	page, err := readRecord(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "text:user:before|text:user:the picture|image:user:image|text:user:after" {
		t.Fatalf("record = %s", got)
	}
	// An image over the size cap, of an unsupported type, or not what it claims to
	// be is not referenced; the text beside it is.
	p2 := writeTurnsRecord(t,
		userBlocks(t, 1, nil, textBlock("t1"), imageBlockOf("image/svg+xml", pngData())),
		userBlocks(t, 2, nil, textBlock("t2"), imageBlockOf("image/png", base64.StdEncoding.EncodeToString([]byte("not a png at all, sorry")))),
		userBlocks(t, 3, nil, textBlock("t3"), map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.invalid/x.png"}}),
		userBlocks(t, 4, nil, imageBlockOf("image/png", pngData())))
	page, err = readRecord(p2, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "text:user:t1|text:user:t2|text:user:t3|image:user:image" {
		t.Fatalf("record = %s", got)
	}
	if page.Messages[0].Media != "" {
		t.Error("a svg was referenced")
	}
}

// ---------------------------------------------------------------------------
// Sub-tasks

func TestReadRecord_SubTaskEntriesCarryTheParentCall(t *testing.T) {
	p := writeTurnsRecord(t,
		user(t, 1, "delegate it", nil),
		toolUse(t, 2, "task-1", "Task", map[string]any{"prompt": "do the thing"}),
		toolResult(t, 20, "task-1", "the sub-task is done", false))
	writeSub(t, p, "a1", "task-1", true, time.Time{},
		subUser(t, "a1", 3, "do the thing"),
		subAssistant(t, "a1", 4, thinkingBlock("hmm")),
		subAssistant(t, "a1", 5, map[string]any{"type": "tool_use", "id": "inner-1", "name": "Grep", "input": map[string]any{"q": "x"}}),
		subToolResult(t, "a1", 6, "inner-1", "found"),
		subAssistant(t, "a1", 7, textBlock("report")))
	// a sub-task started by a call inside the first sub-task
	writeSub(t, p, "b2", "inner-1", true, time.Time{},
		subUser(t, "b2", 8, "go deeper"),
		subAssistant(t, "b2", 9, textBlock("deep answer")))

	page, err := readRecord(p, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"text:user:delegate it",
		"tool:assistant:task-1/completed",
		"text:user@task-1:do the thing",
		"reasoning:assistant@task-1:hmm",
		"tool:assistant@task-1:inner-1/completed",
		"text:assistant@task-1:report",
		"text:user@inner-1:go deeper",
		"text:assistant@inner-1:deep answer",
	}, "|")
	if got := recs(page); got != want {
		t.Fatalf("record =\n %s\nwant\n %s", got, want)
	}
	if page.Messages[2].Model != "" || page.Messages[5].Model != "model-y" {
		t.Errorf("models = %q, %q", page.Messages[2].Model, page.Messages[5].Model)
	}
}

// A sub-task file is read only when everything about it is certain.
func TestReadRecord_SubTaskFilesThatCannotBeVerifiedAreOut(t *testing.T) {
	const leak = "LEAK-MARKER"
	p := writeTurnsRecord(t, user(t, 1, "main", nil))
	dir := subTaskDir(p)

	// no sidecar
	writeSub(t, p, "nometa", "x", true, time.Time{}, subUser(t, "nometa", 2, leak+" no sidecar"))
	if err := os.Remove(filepath.Join(dir, "agent-nometa.meta.json")); err != nil {
		t.Fatal(err)
	}
	// a sidecar that names no call, or a call id with a path in it
	writeSub(t, p, "nocall", "", true, time.Time{}, subUser(t, "nocall", 3, leak+" no call"))
	writeSub(t, p, "badcall", "../../etc/passwd", true, time.Time{}, subUser(t, "badcall", 4, leak+" bad call"))
	// lines that are not this sub-task's own: main-file shape, another agent's, another conversation's
	writeSub(t, p, "mixed", "task-1", true, time.Time{},
		user(t, 5, leak+" a main-shaped line", nil),
		subUser(t, "someone-else", 6, leak+" another agent"),
		jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(7), "isSidechain": true, "agentId": "mixed", "sessionId": "conv-OTHER",
			"message": map[string]any{"role": "user", "content": leak + " another conversation"}}),
		subUser(t, "mixed", 8, "genuine"))
	// a link to a file elsewhere
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(outside, []byte(subUser(t, "linked", 9, leak+" via link")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "agent-linked.jsonl")); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"toolUseId": "task-9", "finished": true})
	if err := os.WriteFile(filepath.Join(dir, "agent-linked.meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	// a name that is not an id
	if err := os.WriteFile(filepath.Join(dir, "agent-a b.jsonl"), []byte(subUser(t, "a b", 10, leak+" odd name")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	page, err := readRecord(p, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "text:user:main|text:user@task-1:genuine" {
		t.Fatalf("record = %s", got)
	}
	if strings.Contains(string(mustJSON(t, page)), leak) {
		t.Errorf("a file that cannot be verified leaked: %s", mustJSON(t, page))
	}
}

// ---------------------------------------------------------------------------
// The cursor

// materialize applies a client's rule to pages read in sequence: a tool message
// whose id it holds replaces the held one. What is left is the record as a client
// shows it.
func materialize(pages ...fleet.RecordPage) []string {
	var order []string
	held := map[string]int{}
	for _, p := range pages {
		for _, m := range p.Messages {
			k := rec(m)
			if m.Tool != nil {
				if i, ok := held[m.Tool.ID]; ok {
					order[i] = k + "=" + m.Tool.Result
					continue
				}
				held[m.Tool.ID] = len(order)
				k += "=" + m.Tool.Result
			}
			order = append(order, k)
		}
	}
	return order
}

// Pages of any size, read in sequence, show the record exactly as one big read
// does: nothing lost, nothing shown twice.
func TestReadRecordCursor_NoLossNoDuplicateAtAnyLimit(t *testing.T) {
	img := pngData()
	p := writeTurnsRecord(t,
		user(t, 1, "start", nil),
		toolUse(t, 2, "t1", "Bash", map[string]any{"c": 1}),
		toolUse(t, 3, "task-1", "Task", map[string]any{"p": 1}),
		assistant(t, 4, textBlock("two calls out")),
		toolResult(t, 9, "t1", "r1", false),
		userBlocks(t, 10, nil, textBlock("pic"), imageBlockOf("image/png", img), imageBlockOf("image/png", img)),
		toolUse(t, 11, "t2", "Read", map[string]any{}),
		toolResult(t, 12, "t2", []map[string]any{{"type": "text", "text": "r2"}, imageBlockOf("image/png", img)}, true),
		toolResult(t, 20, "task-1", "sub done", false),
		assistant(t, 21, textBlock("end")),
	)
	writeSub(t, p, "a1", "task-1", true, time.Time{},
		subUser(t, "a1", 5, "go"),
		subAssistant(t, "a1", 6, map[string]any{"type": "tool_use", "id": "in-1", "name": "G", "input": map[string]any{}}),
		subToolResult(t, "a1", 7, "in-1", "found"),
		subAssistant(t, "a1", 8, textBlock("sub text")),
		subAssistant(t, "a1", 15, textBlock("sub later")))

	now := mustUTC(tsAt(100))
	whole, err := readRecordAt(p, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	want := materialize(whole)
	if len(want) != 14 {
		t.Fatalf("the whole record has %d entries, want 14: %v", len(want), want)
	}

	for limit := 1; limit <= 6; limit++ {
		// Read forward from the very start: main at 0, no sub-task named, a
		// watermark before everything, so each sub-task is new and read from 0.
		cur := recordCursor("conv-1", recPos{}, mustUTC(tsAt(0)).Add(recordWatermarkSlack), nil)
		var pages []fleet.RecordPage
		for n := 0; n < 200; n++ {
			pg, err := readRecordAt(p, cur, limit, now)
			if err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			if len(pg.Messages) > limit {
				t.Fatalf("limit %d: page holds %d messages", limit, len(pg.Messages))
			}
			pages = append(pages, pg)
			if len(pg.Messages) == 0 {
				break
			}
			cur = pg.Next
		}
		got := materialize(pages...)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("limit %d: the pages show\n %s\nthe whole record shows\n %s", limit, strings.Join(got, "\n "), strings.Join(want, "\n "))
		}
	}
}

func cursorNames(t *testing.T, cursor string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(raw), "|", 5)
	if len(parts) != 5 {
		t.Fatalf("cursor %q is not an r2 cursor", raw)
	}
	return parts[4]
}

func TestReadRecordCursor_ASubTaskThatBeginsAfterTheCursorIsReadFromItsStart(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "main", nil))
	now := mustUTC(tsAt(60)) // the watermark sits two minutes before: before the sub-task's first line
	first, err := readRecordAt(p, "", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	writeSub(t, p, "a1", "task-1", false, time.Time{},
		subUser(t, "a1", 200, "the delegated prompt"), subAssistant(t, "a1", 201, textBlock("first words")))
	page, err := readRecordAt(p, first.Next, 10, mustUTC(tsAt(300)))
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "text:user@task-1:the delegated prompt|text:assistant@task-1:first words" {
		t.Fatalf("page = %s", got)
	}
	if !strings.HasPrefix(cursorNames(t, page.Next), "a1:") {
		t.Errorf("a sub-task still being written is not named by the cursor: %q", cursorNames(t, page.Next))
	}
}

func TestReadRecordCursor_AFinishedSubTaskIsDroppedAndNotReadAgain(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "main", nil))
	long := mustUTC(tsAt(0)).Add(-time.Hour)
	writeSub(t, p, "a1", "task-1", true, long, subUser(t, "a1", 2, "p"), subAssistant(t, "a1", 3, textBlock("old news")))

	now := mustUTC(tsAt(1000))
	first, err := readRecordAt(p, "", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(first); got != "text:user:main|text:user@task-1:p|text:assistant@task-1:old news" {
		t.Fatalf("first = %s", got)
	}
	if names := cursorNames(t, first.Next); names != "" {
		t.Errorf("a finished, fully read, old sub-task is named by the cursor: %q", names)
	}
	appendLine(t, p, user(t, 1001, "more main", nil))
	next, err := readRecordAt(p, first.Next, 10, mustUTC(tsAt(1100)))
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(next); got != "text:user:more main" {
		t.Fatalf("next = %s; the finished sub-task was read again", got)
	}
}

func TestReadRecordCursor_ASubTaskResumedFromACutPageIsNotRepeated(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "main", nil))
	sub := writeSub(t, p, "a1", "task-1", false, time.Time{},
		subUser(t, "a1", 2, "s1"), subAssistant(t, "a1", 3, textBlock("s2")), subAssistant(t, "a1", 4, textBlock("s3")))
	now := mustUTC(tsAt(10))
	cur := recordCursor("conv-1", recPos{}, mustUTC(tsAt(0)).Add(recordWatermarkSlack), nil)

	var got []string
	for n := 0; n < 10; n++ {
		pg, err := readRecordAt(p, cur, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pg.Messages {
			got = append(got, rec(m))
		}
		if n == 1 { // the sub-task grows between two pages
			appendLine(t, sub, subAssistant(t, "a1", 5, textBlock("s4")))
		}
		cur = pg.Next
	}
	want := "text:user:main|text:user@task-1:s1|text:assistant@task-1:s2|text:assistant@task-1:s3|text:assistant@task-1:s4"
	if strings.Join(got, "|") != want {
		t.Fatalf("pages show %s\nwant %s", strings.Join(got, "|"), want)
	}
}

// A cursor of the first version names the conversation file only: sub-tasks that
// exist are followed from now, so what they wrote before is not delivered.
func TestReadRecordCursor_AnR1CursorFollowsSubTasksFromNow(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "main", nil))
	sub := writeSub(t, p, "a1", "task-1", false, time.Time{}, subUser(t, "a1", 2, "before"))
	r1 := offsetCursor(recordCursorVersion, "conv-1", 0)
	page, err := readRecordAt(p, r1, 10, mustUTC(tsAt(10)))
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "text:user:main" {
		t.Fatalf("page = %s", got)
	}
	appendLine(t, sub, subAssistant(t, "a1", 11, textBlock("after")))
	page, err = readRecordAt(p, page.Next, 10, mustUTC(tsAt(20)))
	if err != nil {
		t.Fatal(err)
	}
	if got := recs(page); got != "text:assistant@task-1:after" {
		t.Fatalf("page = %s", got)
	}
}

func TestReadRecordCursor_R2ParsingAndConflicts(t *testing.T) {
	p := writeTurnsRecord(t, user(t, 1, "a", nil))
	good, err := readRecordAt(p, "", 5, mustUTC(tsAt(10)))
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"wrong field count":  base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion2 + "|conv-1|0|0")),
		"bad main position":  base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion2 + "|conv-1|x|0|")),
		"bad watermark":      base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion2 + "|conv-1|0|x|")),
		"bad sub-task entry": base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion2 + "|conv-1|0|0|a1")),
		"bad sub-task id":    base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion2 + "|conv-1|0|0|../x:1")),
		"bad inner index":    base64.RawURLEncoding.EncodeToString([]byte(recordCursorVersion2 + "|conv-1|0.0|0|")),
		"unknown version":    base64.RawURLEncoding.EncodeToString([]byte("r9|conv-1|0")),
	} {
		if _, err := readRecordAt(p, bad, 5, mustUTC(tsAt(10))); err == nil || strings.Contains(err.Error(), "another conversation") {
			t.Errorf("%s: err = %v, want a plain refusal", name, err)
		}
	}
	other := strings.Replace(string(mustDecode(t, good.Next)), "|conv-1|", "|conv-2|", 1)
	if _, err := readRecordAt(p, base64.RawURLEncoding.EncodeToString([]byte(other)), 5, mustUTC(tsAt(10))); err == nil ||
		!strings.Contains(err.Error(), "another conversation") {
		t.Errorf("another conversation: err = %v, want the conflict", err)
	}
	pastEnd := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|conv-1|%d|0|", recordCursorVersion2, 1<<20)))
	if _, err := readRecordAt(p, pastEnd, 5, mustUTC(tsAt(10))); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Errorf("past the end: err = %v, want the conflict", err)
	}
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
