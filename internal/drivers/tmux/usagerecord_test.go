package tmux

import (
	"os"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Synthetic fixtures, shaped like the runtime's own writer (see usagerecord.go).

func usageEntry_(t *testing.T, i int, msgID string, in, out, cr, cw int64) string {
	t.Helper()
	return jsonLine(t, map[string]any{
		"type": "assistant", "timestamp": tsAt(i),
		"message": map[string]any{
			"id": msgID, "role": "assistant", "model": "model-x",
			"content": []map[string]any{textBlock("x")},
			"usage": map[string]any{
				"input_tokens": in, "output_tokens": out,
				"cache_read_input_tokens": cr, "cache_creation_input_tokens": cw,
			},
		},
	})
}

func turnMarker(t *testing.T, i int) string {
	return jsonLine(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": tsAt(i)})
}

// The oracle the issue adds: a multi-step turn whose LAST step differs from the
// sum. The block reports the sum, the repeated entries of one message count
// once, and the turn's own figures are what lie between two markers.
func TestUsage_SumsEveryStepNotTheLast(t *testing.T) {
	p := writeTurnsRecord(t,
		usageEntry_(t, 1, "m1", 10, 100, 1000, 50), // first block of m1
		usageEntry_(t, 2, "m1", 10, 100, 1000, 50), // second block of m1: the SAME request
		usageEntry_(t, 3, "m2", 20, 200, 2000, 0),
		usageEntry_(t, 4, "m3", 1, 5, 7, 0), // last step is tiny
		turnMarker(t, 5),
	)
	var r usageReader
	u := r.read(p)
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 31 || u.Output != 305 || u.CacheRead != 3007 || u.CacheWrite != 50 {
		t.Fatalf("cumulative = %+v, want the sum over distinct messages (31/305/3007/50)", u)
	}
	if u.Reasoning != nil || u.Cost != nil {
		t.Errorf("figures the record does not carry must be absent, got reasoning=%v cost=%v", u.Reasoning, u.Cost)
	}
	if u.Source != fleet.ConfidenceInferred {
		t.Errorf("source = %q, want inferred", u.Source)
	}
	if !u.AsOf.Equal(mustTime(tsAt(4))) {
		t.Errorf("asOf = %v, want the newest counted entry %s", u.AsOf, tsAt(4))
	}
	if u.LastTurn == nil || u.LastTurn.Input != 31 || u.LastTurn.Output != 305 || !u.LastTurn.At.Equal(mustTime(tsAt(5))) {
		t.Fatalf("lastTurn = %+v", u.LastTurn)
	}
}

func TestUsage_TurnFiguresAreTheSpanBetweenMarkers(t *testing.T) {
	p := writeTurnsRecord(t,
		usageEntry_(t, 1, "m1", 10, 100, 0, 0),
		turnMarker(t, 2),
		usageEntry_(t, 3, "m2", 1, 2, 3, 4),
		usageEntry_(t, 4, "m3", 1, 2, 3, 4),
		turnMarker(t, 5),
		usageEntry_(t, 6, "m4", 9, 9, 9, 9), // a turn still running
	)
	var r usageReader
	u := r.read(p)
	if u.Input != 21 || u.Output != 113 {
		t.Fatalf("cumulative includes the running turn: %+v", u)
	}
	if u.LastTurn == nil || u.LastTurn.Input != 2 || u.LastTurn.Output != 4 || u.LastTurn.CacheWrite != 8 {
		t.Fatalf("lastTurn is the most recent COMPLETED turn: %+v", u.LastTurn)
	}
}

// Incremental: a read after an append adds only the tail, and a message whose
// later entry carries a larger streamed count contributes the difference.
func TestUsage_IncrementalReadAddsOnlyTheTail(t *testing.T) {
	p := writeTurnsRecord(t, usageEntry_(t, 1, "m1", 10, 5, 0, 0))
	var r usageReader
	if u := r.read(p); u == nil || u.Output != 5 {
		t.Fatalf("first read: %+v", u)
	}
	appendLine(t, p, usageEntry_(t, 2, "m1", 10, 40, 0, 0)) // the same message, now with its final output count
	appendLine(t, p, usageEntry_(t, 3, "m2", 1, 1, 0, 0))
	u := r.read(p)
	if u.Input != 11 || u.Output != 41 {
		t.Fatalf("after append = %+v, want input 11 output 41", u)
	}
	// A half-written trailing line is the runtime mid-append: not counted yet.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"type":"assistant","message":{"id":"m3","usage":{"input_t`)
	f.Close()
	if u := r.read(p); u.Input != 11 {
		t.Fatalf("a partial line was counted: %+v", u)
	}
}

func TestUsage_AbsentIsNotZero(t *testing.T) {
	var r usageReader
	if u := r.read("/nonexistent/conv.jsonl"); u != nil {
		t.Errorf("missing record: %+v", u)
	}
	// A record with no usage-bearing entry yet is "not yet known".
	p := writeTurnsRecord(t, assistantNoUsage(t), turnMarker(t, 2))
	if u := r.read(p); u != nil {
		t.Errorf("record with no usage entries: %+v", u)
	}
	// Synthetic and error entries are the runtime speaking for itself, not spend.
	p2 := writeTurnsRecord(t,
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(1), "isApiErrorMessage": true,
			"message": map[string]any{"id": "e", "usage": map[string]any{"input_tokens": 5}}}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(2),
			"message": map[string]any{"id": "s", "model": "<synthetic>", "usage": map[string]any{"input_tokens": 5}}}),
	)
	if u := r.read(p2); u != nil {
		t.Errorf("synthetic/error entries were counted: %+v", u)
	}
}

func assistantNoUsage(t *testing.T) string { return assistant(t, 1, textBlock("hi")) }

func TestUsage_TruncatedRecordStartsOver(t *testing.T) {
	p := writeTurnsRecord(t, usageEntry_(t, 1, "m1", 100, 100, 0, 0), usageEntry_(t, 2, "m2", 100, 100, 0, 0))
	var r usageReader
	r.read(p)
	if err := os.WriteFile(p, []byte(usageEntry_(t, 3, "m9", 7, 7, 0, 0)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if u := r.read(p); u == nil || u.Input != 7 {
		t.Fatalf("after replacement = %+v, want a fresh sum of 7", u)
	}
}

// A first read of a record over the synchronous bound happens in the
// background: absent until it lands, then the full sum.
func TestUsage_LargeFirstReadIsBackgroundAndAbsentUntilDone(t *testing.T) {
	big := []string{usageEntry_(t, 1, "m1", 3, 4, 5, 6)}
	pad := strings.Repeat("x", 1<<20)
	for i := 0; i < 9; i++ {
		big = append(big, jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(2), "message": map[string]any{"content": pad}}))
	}
	p := writeTurnsRecord(t, big...)
	var r usageReader
	if u := r.read(p); u != nil {
		t.Fatalf("a big first read answered synchronously: %+v", u)
	}
	deadline := 200
	for ; deadline > 0; deadline-- {
		if u := r.read(p); u != nil {
			if u.Input != 3 || u.Output != 4 {
				t.Fatalf("background sum = %+v", u)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background scan never completed")
}

// Through the driver: State and List both carry the block, and the capability
// that makes an absent block answerable is declared.
func TestDriverState_CarriesUsageFromTheSessionsRecord(t *testing.T) {
	d, _, ref, _, conv := titleSyncSession(t)
	if !d.Capabilities().ReportsUsage {
		t.Fatal("the tmux driver reads usage; it must say so")
	}
	appendLine(t, conv, usageEntry_(t, 1, "m1", 10, 20, 30, 40))
	appendLine(t, conv, usageEntry_(t, 2, "m2", 1, 2, 3, 4))
	appendLine(t, conv, turnMarker(t, 3))

	st, err := d.State(t.Context(), fleet.Request{}, ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.Usage == nil || st.Usage.Input != 11 || st.Usage.Output != 22 || st.Usage.LastTurn == nil || st.Usage.LastTurn.CacheWrite != 44 {
		t.Fatalf("State().Usage = %+v", st.Usage)
	}
	if !d.Capabilities().ReportsContext {
		t.Error("the tmux driver reads the newest request's figures; it must say so")
	}
	if c := st.Usage.Context; c == nil || c.Tokens != 1+3+4 || c.Window != nil || c.Model != "model-x" {
		t.Errorf("State().Usage.Context = %+v, want m2's 8 tokens and a null window", c)
	}

	col, err := d.List(t.Context(), fleet.Request{}, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, s := range col.Items() {
		if s.ID == ref.ID {
			seen = true
			if s.State.Usage == nil || s.State.Usage.Input != 11 {
				t.Errorf("List() usage for %s = %+v", s.ID, s.State.Usage)
			}
		} else if s.State.Usage != nil {
			t.Errorf("session %s has no record and must report no usage, got %+v", s.ID, s.State.Usage)
		}
	}
	if !seen {
		t.Fatal("session not listed")
	}
}

// #313: the context size is what the NEWEST distinct request read — its
// uncached input plus cache read plus cache write — not any sum. The oracle is
// a multi-step turn whose newest request differs from every sum, and a message
// written as several entries that counts once.
func TestUsage_ContextIsTheNewestRequestNotASum(t *testing.T) {
	p := writeTurnsRecord(t,
		usageEntry_(t, 1, "m1", 10, 100, 1000, 50),  // 1060
		usageEntry_(t, 2, "m1", 10, 100, 1000, 50),  // the SAME request, second block
		usageEntry_(t, 3, "m2", 20, 200, 90000, 0),  // 90020
		usageEntry_(t, 4, "m3", 1, 5, 142000, 1999), // 144000: the newest
		usageEntry_(t, 4, "m3", 1, 9, 142000, 1999), // same message again; counts once
		turnMarker(t, 5),
	)
	var r usageReader
	u := r.read(p)
	if u == nil || u.Context == nil {
		t.Fatalf("usage = %+v, want a context block", u)
	}
	c := u.Context
	if c.Tokens != 144000 {
		t.Errorf("tokens = %d, want 144000 (input+cacheRead+cacheWrite of m3 once)", c.Tokens)
	}
	if c.Model != "model-x" {
		t.Errorf("model = %q, want the model the request ran on", c.Model)
	}
	if !c.At.Equal(mustTime(tsAt(4))) {
		t.Errorf("at = %v, want the entry's own timestamp %s", c.At, tsAt(4))
	}
	if c.Window != nil {
		t.Errorf("window = %v: the record states no window, so it must be null", *c.Window)
	}
	if sum := u.Input + u.CacheRead + u.CacheWrite; c.Tokens == sum {
		t.Errorf("context equals the cumulative sum %d: it must be the newest request's figure", sum)
	}
}

// A multi-entry message whose later entry carries the larger input is still one
// request, and a later message replaces it.
func TestUsage_ContextFollowsTheTailIncrementally(t *testing.T) {
	p := writeTurnsRecord(t, usageEntry_(t, 1, "m1", 10, 5, 100, 0))
	var r usageReader
	if u := r.read(p); u == nil || u.Context == nil || u.Context.Tokens != 110 {
		t.Fatalf("first read: %+v", u)
	}
	appendLine(t, p, usageEntry_(t, 2, "m2", 5, 5, 400, 100))
	u := r.read(p)
	if u == nil || u.Context == nil || u.Context.Tokens != 505 || !u.Context.At.Equal(mustTime(tsAt(2))) {
		t.Fatalf("after append: %+v", u.Context)
	}
	// The newest request can be SMALLER than an earlier one (the runtime
	// compacted): the figure follows the newest, never a running maximum.
	appendLine(t, p, usageEntry_(t, 3, "m3", 7, 5, 50, 0))
	if u := r.read(p); u.Context == nil || u.Context.Tokens != 57 {
		t.Fatalf("after a compaction-sized request: %+v", u.Context)
	}
}

// Absent is not zero (#313): nothing read, nothing carrying figures, and a
// request with no input at all each leave the context off rather than at 0.
func TestUsage_ContextAbsentIsNotZero(t *testing.T) {
	var r usageReader
	if u := r.read("/nonexistent/conv.jsonl"); u != nil {
		t.Errorf("missing record: %+v", u)
	}
	if u := r.read(writeTurnsRecord(t, assistantNoUsage(t))); u != nil {
		t.Errorf("record with no usage entries: %+v", u)
	}
	// Output-only usage is spend, but it is no context size.
	p := writeTurnsRecord(t, usageEntry_(t, 1, "m1", 0, 9, 0, 0))
	u := r.read(p)
	if u == nil || u.Output != 9 {
		t.Fatalf("usage = %+v, want the output counted", u)
	}
	if u.Context != nil {
		t.Errorf("context = %+v for a request that read nothing, want absent", u.Context)
	}
	// An all-zero entry after a real one does not erase it.
	p2 := writeTurnsRecord(t, usageEntry_(t, 1, "m1", 10, 5, 100, 0), usageEntry_(t, 2, "m2", 0, 0, 0, 0))
	if u := r.read(p2); u == nil || u.Context == nil || u.Context.Tokens != 110 {
		t.Errorf("a zero-figure entry replaced the context: %+v", u)
	}
}

// A delegated agent's own request is spend of the session but not the size of
// the context the session carries.
func TestUsage_ContextIgnoresSidechainRequests(t *testing.T) {
	side := jsonLine(t, map[string]any{
		"type": "assistant", "timestamp": tsAt(3), "isSidechain": true,
		"message": map[string]any{
			"id": "side", "role": "assistant", "model": "model-y",
			"usage": map[string]any{"input_tokens": 999999, "output_tokens": 1},
		},
	})
	p := writeTurnsRecord(t, usageEntry_(t, 1, "m1", 10, 5, 100, 0), side)
	var r usageReader
	u := r.read(p)
	if u == nil || u.Context == nil || u.Context.Tokens != 110 || u.Context.Model != "model-x" {
		t.Fatalf("context = %+v, want the main thread's request", u.Context)
	}
	if u.Input != 10+999999 {
		t.Errorf("the sidechain's spend must still be counted: input = %d", u.Input)
	}
}

func mustTime(s string) time.Time {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return v
}

// #338: a listing runs after every output burst and reads every session's
// usage. A record nothing was appended to costs a stat, never an open.
func TestUsage_UnchangedRecordIsNotOpened(t *testing.T) {
	p := writeTurnsRecord(t, usageEntry_(t, 1, "m1", 10, 5, 0, 0))
	opens := 0
	prev := openRecord
	openRecord = func(name string) (*os.File, error) { opens++; return prev(name) }
	t.Cleanup(func() { openRecord = prev })

	var r usageReader
	first := r.read(p)
	if first == nil || opens != 1 {
		t.Fatalf("first read: usage=%+v opens=%d, want a result from exactly one open", first, opens)
	}
	for i := 0; i < 5; i++ {
		u := r.read(p)
		if u == nil || u.Output != first.Output {
			t.Fatalf("idle read %d = %+v, want the memoised %+v", i, u, first)
		}
	}
	if opens != 1 {
		t.Fatalf("5 reads of an unchanged record opened it %d more times, want 0", opens-1)
	}
	appendLine(t, p, usageEntry_(t, 2, "m2", 1, 1, 0, 0))
	if u := r.read(p); u == nil || u.Output != 6 || opens != 2 {
		t.Fatalf("after an append: usage=%+v opens=%d, want output 6 from one more open", u, opens)
	}
}
