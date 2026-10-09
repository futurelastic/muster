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

func mustTime(s string) time.Time {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return v
}
