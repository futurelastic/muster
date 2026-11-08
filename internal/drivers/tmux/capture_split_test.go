package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster#428: a batched capture killed at its slice leaves panes unread, and
// those panes are read again in smaller chunks instead of all being classified
// as a driver malfunction from that one answer.
//
// Every test here uses the budgetMux fake and the frozen-clock pattern of
// TestCaptureRetryBoundedToOnePerEnumeration (muster#186): the driver's clock
// stands still at the call's start, so each slice and the recovery pass's
// share of the budget are exact constants, while stalls are still ended by real
// timers. Only the FIRST invocation waits out its slice in real time; every
// later one is handed a deadline that is already past and ends at once. So the
// tests assert the deadlines the driver handed out, never how long anything
// took, and they stay fast.
//
// The deadline is 1.5 s: the first slice is a third of it (500 ms of real
// waiting) and the recovery pass's share ends at 750 ms, which leaves 250 ms
// of real time for work the fake does instantly.
const splitTestDeadline = 1500 * time.Millisecond

// runSplitList runs one List under the frozen clock and returns what it saw.
func runSplitList(t *testing.T, b *budgetMux) (items []fleet.Session, src []fleet.SourceStatus, callStart time.Time, d *Driver) {
	t.Helper()
	callStart = time.Now()
	d = newBudgetDriverAt(b, splitTestDeadline, func() time.Time { return callStart })
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return got.Items(), got.Sources(), callStart, d
}

// assertDeadlinesInside checks the property the whole design leans on: no
// capture invocation, whichever pass started it, was handed a deadline past the
// call's, and every one after the first (the recovery pass's) stays inside the
// half of the budget the pass is allowed.
func assertDeadlinesInside(t *testing.T, b *budgetMux, callStart time.Time, recoveryFrom int) {
	t.Helper()
	callDeadline := callStart.Add(splitTestDeadline)
	passDeadline := callStart.Add(splitTestDeadline / 2)
	for i, dl := range b.capturedDeadlines() {
		switch {
		case dl.IsZero():
			t.Errorf("capture invocation %d ran with no deadline", i)
		case dl.After(callDeadline):
			t.Errorf("capture invocation %d was given a deadline %v past the call's", i, dl.Sub(callDeadline))
		case i >= recoveryFrom && dl.After(passDeadline):
			t.Errorf("recovery invocation %d was given a deadline %v past the pass's half of the budget",
				i, dl.Sub(passDeadline))
		}
	}
}

// The issue's own done-when: a multiplexer that cannot answer a large batch in
// its slice and answers small ones. Every pane gets its real state, nothing is
// classified as a malfunction, and the call stays inside its deadline.
func TestCaptureSplitRecoversWhenOnlyLargeInvocationsStall(t *testing.T) {
	logs := captureLog(t)
	f := bigFleet(62) // 64 panes: exactly one chunk
	b := &budgetMux{fakeMux: f, stallOver: 32}

	items, src, callStart, d := runSplitList(t, b)

	if len(items) != 64 {
		t.Fatalf("want 64 sessions, got %d", len(items))
	}
	if n := unknownCount(items); n != 0 {
		t.Errorf("%d session(s) classified unknown; every pane should have its real state", n)
	}
	if len(src) != 1 || src[0].Status != fleet.SourceOK {
		t.Errorf("source should read ok after a recovered chunk, got %+v", src)
	}
	if got, want := b.paneCounts(), []int{64, 32, 32}; !equalInts(got, want) {
		t.Errorf("capture invocations named %v pane(s), want %v (the stalled chunk, then its two halves)", got, want)
	}
	out := logs.String()
	if strings.Contains(out, "nothing parseable") {
		t.Errorf("a recovered chunk must not be logged as lost; log:\n%s", out)
	}
	if !strings.Contains(out, "retry recovered 64/64 pane(s)") {
		t.Errorf("a recovered chunk is still logged (the failure rate is the signal); log:\n%s", out)
	}
	if c := d.Counters(); c[counterCaptureChunkFailed] != 1 || c[counterCaptureRetryRecovered] != 1 ||
		c[counterCaptureSplitInvocation] != 2 || c[counterCaptureRetryFailed] != 0 {
		t.Errorf("counters: want chunk_failed=1 retry_recovered=1 split_invocation=2 retry_failed=0, got %v", c)
	}
	assertDeadlinesInside(t, b, callStart, 1)
}

// One pane whose capture never returns costs that pane its state and no other.
// The split isolates it: two invocations per level, six levels.
func TestCaptureSplitIsolatesAStuckPane(t *testing.T) {
	logs := captureLog(t)
	f := bigFleet(62)
	b := &budgetMux{fakeMux: f, stallPane: "%340"}

	items, src, callStart, d := runSplitList(t, b)

	if n := unknownCount(items); n != 1 {
		t.Errorf("want exactly the stuck pane's session unknown, got %d unknown", n)
	}
	for _, s := range items {
		if s.State.Status == fleet.StatusUnknown && s.ID != "s40" {
			t.Errorf("session %q is unknown but only the one holding pane %%340 should be", s.ID)
		}
	}
	if len(src) != 1 || src[0].Status != fleet.SourceDegraded {
		t.Errorf("source should read degraded while a pane is unread, got %+v", src)
	}
	if c := b.captureCount(); c != 13 {
		t.Errorf("want 13 capture invocations (the chunk + 12 to isolate one pane among 64), got %d: %v",
			c, b.paneCounts())
	}
	if c := b.captureCount(); c > 1+captureRecoveryMaxInvocations {
		t.Errorf("%d invocations is past the cap of %d for a recovery pass", c, captureRecoveryMaxInvocations)
	}
	if out := logs.String(); !strings.Contains(out, "left 1 of 64 pane(s) unread") {
		t.Errorf("the pane that stayed unread must be on the log; log:\n%s", out)
	}
	if c := d.Counters(); c[counterCaptureRetryRecovered] != 1 || c[counterCaptureSplitInvocation] != 12 {
		t.Errorf("counters: want retry_recovered=1 split_invocation=12, got %v", c)
	}
	assertDeadlinesInside(t, b, callStart, 1)
}

// A killed invocation hands back what it printed first. Only the panes it did
// not reach are asked for again; the ones it did are kept.
func TestCaptureSplitReReadsOnlyThePanesAKilledInvocationMissed(t *testing.T) {
	logs := captureLog(t)
	b := &budgetMux{fakeMux: bigFleet(62), partialFirst: 27}

	items, src, callStart, d := runSplitList(t, b)

	if n := unknownCount(items); n != 0 {
		t.Errorf("%d session(s) classified unknown; the 37 unread panes should have been read again", n)
	}
	if len(src) != 1 || src[0].Status != fleet.SourceOK {
		t.Errorf("source should read ok after the unread panes were recovered, got %+v", src)
	}
	if got, want := b.paneCounts(), []int{64, 18, 19}; !equalInts(got, want) {
		t.Errorf("capture invocations named %v pane(s), want %v (the chunk, then only the 37 unread, halved)", got, want)
	}
	if out := logs.String(); !strings.Contains(out, "short, 37 of 64 pane(s) unread") ||
		!strings.Contains(out, "retry recovered 37/37 pane(s)") {
		t.Errorf("a short chunk should be logged as short, with what the pass recovered; log:\n%s", out)
	}
	if c := d.Counters(); c[counterCaptureChunkPartial] != 1 || c[counterCaptureChunkFailed] != 0 ||
		c[counterCaptureRetryRecovered] != 1 {
		t.Errorf("counters: want chunk_partial=1 chunk_failed=0 retry_recovered=1, got %v", c)
	}
	assertDeadlinesInside(t, b, callStart, 1)
}

// A stall across the whole server stays bounded: one recovery pass for the
// whole enumeration, capped, and not a pass per chunk.
func TestCaptureSplitIsBoundedWhenEveryInvocationStalls(t *testing.T) {
	captureLog(t)
	f := bigFleet(200) // 202 panes: chunks of 64, 64, 64 and 10
	b := &budgetMux{fakeMux: f, stall: func(int) bool { return true }}

	items, _, callStart, _ := runSplitList(t, b)

	if n := unknownCount(items); n != len(f.sessions) {
		t.Errorf("every session should read unknown when nothing was captured; %d/%d did", n, len(f.sessions))
	}
	got := b.paneCounts()
	if want := 4 + captureRecoveryMaxInvocations; len(got) != want {
		t.Errorf("want %d capture invocations (4 chunks + a pass capped at %d), got %d: %v",
			want, captureRecoveryMaxInvocations, len(got), got)
	}
	if len(got) >= 4 {
		if tail := got[len(got)-3:]; !equalInts(tail, []int{64, 64, 10}) {
			t.Errorf("the chunks after the first should run once each, without a pass of their own; tail %v", tail)
		}
	}
	assertDeadlinesInside(t, b, callStart, 1)
}

// A machine with one pane has nothing to split. The pass asks for that pane
// once more, as the single same-size retry it replaces did (muster#156).
func TestCaptureSplitRetriesAOnePaneChunkAsIs(t *testing.T) {
	captureLog(t)
	f := twoSessions()
	f.sessions = f.sessions[:1]
	b := &budgetMux{fakeMux: f, stall: func(n int) bool { return n == 0 }}

	items, _, callStart, _ := runSplitList(t, b)

	if n := unknownCount(items); n != 0 {
		t.Errorf("%d session(s) classified unknown; the retry should have read the pane", n)
	}
	if got, want := b.paneCounts(), []int{1, 1}; !equalInts(got, want) {
		t.Errorf("capture invocations named %v pane(s), want %v", got, want)
	}
	assertDeadlinesInside(t, b, callStart, 1)
}

// A caller that throws the captures away must not pay for recovering them: it
// keeps exactly the single same-size retry it had.
func TestEnumerateRowsOnlyCallersKeepTheSingleSameSizeRetry(t *testing.T) {
	captureLog(t)
	f := bigFleet(200)
	b := &budgetMux{fakeMux: f, stall: func(int) bool { return true }}
	callStart := time.Now()
	d := newBudgetDriverAt(b, splitTestDeadline, func() time.Time { return callStart })

	ctx, cancel := d.bounded(context.Background())
	defer cancel()
	rows, _, err := d.enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(rows) != len(f.sessions) {
		t.Errorf("the rows are the point of this caller and must all be there, got %d/%d", len(rows), len(f.sessions))
	}
	if got, want := b.paneCounts(), []int{64, 64, 64, 64, 10}; !equalInts(got, want) {
		t.Errorf("capture invocations named %v pane(s), want %v (each chunk once, and the first chunk once more)", got, want)
	}
	if c := d.Counters(); c[counterCaptureSplitInvocation] != 0 {
		t.Errorf("a rows-only caller started %d recovery invocations", c[counterCaptureSplitInvocation])
	}
}

func TestSplitHelpers(t *testing.T) {
	rows := func(n int) []indexedPaneRow {
		out := make([]indexedPaneRow, n)
		for i := range out {
			out[i].index = i
		}
		return out
	}
	if got := halveRows(rows(5)); len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 3 {
		t.Errorf("halveRows(5) = %d pieces, want 2 and 3 panes", len(got))
	}
	if got := halveRows(rows(1)); len(got) != 1 || len(got[0]) != 1 {
		t.Errorf("halveRows of one pane should return it whole, got %v", got)
	}
	read := map[string]paneCapture{"0": {}, "2": {}}
	if left := unreadRows(rows(4), read); len(left) != 2 || left[0].index != 1 || left[1].index != 3 {
		t.Errorf("unreadRows = %v, want indexes 1 and 3", left)
	}
}
