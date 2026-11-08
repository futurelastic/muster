package tmux

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster#156: the batched capture's TIME wall.
//
// Every test here runs on the REAL clock with a short declared deadline,
// unlike newTestDriver's fixed clock. The behaviour under test is how
// invocations share one call's budget, and a clock that never moves has no
// budget to share. The fixed clock is also already in the past in real time,
// so its bounded context has expired before a call even starts. fakeMux
// ignores the context, which is the only reason that does not matter
// anywhere else.

// budgetMux wraps fakeMux so a test can script how each batched capture
// invocation behaves, while everything else (listing, classification input)
// still comes from the ordinary fake.
type budgetMux struct {
	*fakeMux
	mu sync.Mutex
	// stall decides, per capture invocation (0-based), whether it hangs
	// until its context is done — the multiplexer failing to answer.
	stall func(n int) bool
	// delay is added to every capture invocation that does not stall, to
	// model a slow but successful multiplexer.
	delay time.Duration
	// onCapture runs at the start of every capture invocation, before it
	// stalls or answers (for example, to cancel the caller mid-call).
	onCapture func(n int)
	// stallOver, when positive, hangs every capture invocation that names more
	// than this many panes: a multiplexer that cannot answer a large batch in
	// time and can answer a small one (muster#428).
	stallOver int
	// stallPane, when set, hangs every capture invocation that names this pane:
	// one pane whose capture never returns.
	stallPane string
	// partialFirst, when positive, makes capture invocation 0 print the output
	// of its first N panes and then hang until killed: what a killed
	// invocation hands back (muster#428).
	partialFirst int
	captures     int
	deadlines    []time.Time // each capture invocation's context deadline, if any
	started      []time.Time
	panes        []int // how many panes each capture invocation named
}

func (b *budgetMux) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "display-message" {
		return b.fakeMux.exec(ctx, name, args...)
	}
	np := namedPanes(args)
	b.mu.Lock()
	n := b.captures
	b.captures++
	dl, _ := ctx.Deadline()
	b.deadlines = append(b.deadlines, dl)
	b.started = append(b.started, time.Now())
	b.panes = append(b.panes, np)
	b.mu.Unlock()

	if b.onCapture != nil {
		b.onCapture(n)
	}
	partial := b.partialFirst > 0 && n == 0
	if partial || (b.stall != nil && b.stall(n)) || (b.stallOver > 0 && np > b.stallOver) ||
		(b.stallPane != "" && namesPane(args, b.stallPane)) {
		var out []byte
		if partial {
			out, _ = b.fakeMux.exec(ctx, name, firstPanes(args, b.partialFirst)...)
		}
		<-ctx.Done()
		// Exactly what exec.CommandContext reports for a process it had to
		// kill mid-run, or for one whose context was already cancelled
		// before it started.
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, context.Canceled
		}
		return out, errors.New("signal: killed")
	}
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	return b.fakeMux.exec(ctx, name, args...)
}

// namedPanes is how many panes one batched capture invocation reads.
func namedPanes(args []string) int {
	n := 0
	for _, a := range args {
		if a == "display-message" {
			n++
		}
	}
	return n
}

func namesPane(args []string, pane string) bool {
	for _, a := range args {
		if a == pane {
			return true
		}
	}
	return false
}

// firstPanes cuts a batched capture's arguments down to its first n panes.
func firstPanes(args []string, n int) []string {
	seen := 0
	for i, a := range args {
		if a != "display-message" {
			continue
		}
		seen++
		if seen == n+1 {
			return args[:i-1] // drop the ";" that joined this pane to the last
		}
	}
	return args
}

// paneCounts is a copy of how many panes each capture invocation named, in order.
func (b *budgetMux) paneCounts() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.panes...)
}

func (b *budgetMux) captureCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.captures
}

// capturedDeadlines is a copy of every capture invocation's context deadline,
// in invocation order. A zero time means that invocation ran with no deadline.
func (b *budgetMux) capturedDeadlines() []time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]time.Time(nil), b.deadlines...)
}

func newBudgetDriver(b *budgetMux, deadline time.Duration) *Driver {
	return newBudgetDriverAt(b, deadline, time.Now)
}

// newBudgetDriverAt is newBudgetDriver with the driver's own clock supplied.
// The clock is what the driver derives the call's deadline and every slice
// from; the timers that actually stop a stalled invocation stay on the real
// clock, because they belong to the context package.
func newBudgetDriverAt(b *budgetMux, deadline time.Duration, now func() time.Time) *Driver {
	return New("testbox",
		withExec(b.exec),
		withNonce(func() string { return testNonce }),
		withClock(now),
		WithDeadline(deadline),
	)
}

// captureLog sends the standard logger to a buffer for the test's lifetime.
// Not parallel-safe, so no test in this file calls t.Parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

func bigFleet(n int) *fakeMux {
	f := twoSessions()
	for i := 0; i < n; i++ {
		id := "%" + intToStr(300+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "s" + intToStr(i), paneID: id, cwd: "/w", pid: 2000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("s" + intToStr(i))
	}
	return f
}

func unknownCount(items []fleet.Session) int {
	n := 0
	for _, s := range items {
		if s.State.Status == fleet.StatusUnknown {
			n++
		}
	}
	return n
}

// The failure #156 reported: one invocation stops answering. Before the fix
// it took the call's whole deadline, every pane in the chunk read as a
// driver malfunction, and there was no budget left to ask again. Now it
// gets a slice, is killed at the end of it, and its panes are asked for again
// in smaller chunks (muster#428; #156 asked for the same chunk once more, which
// is still what a one-pane chunk gets).
func TestCaptureChunkStuckIsRetriedOnceWithinCallBudget(t *testing.T) {
	logs := captureLog(t)
	const deadline = 900 * time.Millisecond
	b := &budgetMux{fakeMux: twoSessions(), stall: func(n int) bool { return n == 0 }}
	d := newBudgetDriver(b, deadline)

	start := time.Now()
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := unknownCount(got.Items()); n != 0 {
		t.Errorf("%d session(s) came back unknown; the retry should have recovered every screen", n)
	}
	if src := got.Sources(); len(src) != 1 || src[0].Status != fleet.SourceOK {
		t.Errorf("source should read ok after a recovered retry, got %+v", src)
	}
	if got, want := b.paneCounts(), []int{2, 1, 1}; !equalInts(got, want) {
		t.Errorf("capture invocations named %v pane(s), want %v (the stalled chunk, then its panes one at a time)", got, want)
	}
	if elapsed >= deadline {
		t.Errorf("List took %v, at or past the %v declared deadline; the stall should have "+
			"been cut off at its slice, leaving budget for the retry", elapsed, deadline)
	}
	// The stalled invocation must not have been allowed more than half of
	// the budget. That is the invariant keeping the retry, and the verb's own
	// work after it, fundable.
	if limit := b.started[0].Add(deadline / 2); b.deadlines[0].IsZero() || b.deadlines[0].After(limit) {
		t.Errorf("first capture's deadline %v exceeds start+deadline/2 (%v)", b.deadlines[0], limit)
	}
	if !strings.Contains(logs.String(), "retry recovered 2/2 pane(s)") {
		t.Errorf("a recovered chunk must still be logged (the failure rate is the signal); log:\n%s", logs)
	}
	if c := d.Counters(); c[counterCaptureChunkFailed] != 1 || c[counterCaptureRetryRecovered] != 1 {
		t.Errorf("counters: want chunk_failed=1 retry_recovered=1, got %v", c)
	}
}

// A stall across the whole server: every invocation hangs. There is exactly
// one recovery pass per enumeration, not one per chunk, the pass is capped at
// captureRecoveryMaxInvocations (muster#428; it was a single same-size retry
// before), and no invocation is allowed to run past the call's declared
// deadline.
//
// That last property used to be asserted as "List returned within the
// deadline plus 150 ms", by wall clock (muster#186). It failed once
// during a full -race run on a loaded machine, then passed on the rerun and
// 8 of 8 times in isolation. Measured on a quiet machine, List takes about
// 400 ms (one chunk) and 510 ms (several) against that 750 ms bound, and its
// own work after the last capture is under 10 ms. So the driver leaves the
// margin alone, and what is left for the assertion to measure is how late the
// host wakes a timer, which nothing in this package controls. Delaying the
// wake of the last invocations by 400 ms reproduces the reported failure
// exactly.
//
// The property lives in the deadlines the driver hands each invocation, so it
// is asserted there. The driver's clock is frozen at the call's start, which
// makes the call's deadline exactly start+deadline instead of something to
// estimate; stalls are still cut short by real timers, so the test still takes
// real time. The frozen clock is only safe because List never polls on it.
func TestCaptureRetryBoundedToOnePerEnumeration(t *testing.T) {
	const deadline = 600 * time.Millisecond
	for _, tc := range []struct {
		name  string
		fleet *fakeMux
		// recovery is how many invocations the one recovery pass starts: two
		// single panes for two panes, and the cap once there are enough panes
		// to keep splitting.
		recovery int
	}{
		{"one chunk", twoSessions(), 2},
		{"several chunks", bigFleet(200), captureRecoveryMaxInvocations},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureLog(t)
			rows := make([]paneRow, len(tc.fleet.sessions))
			for i, s := range tc.fleet.sessions {
				rows[i] = paneRow{paneID: s.paneID}
			}
			chunks := len(chunkPaneRows(rows))

			b := &budgetMux{fakeMux: tc.fleet, stall: func(int) bool { return true }}
			callStart := time.Now()
			d := newBudgetDriverAt(b, deadline, func() time.Time { return callStart })
			got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if c := b.captureCount(); c != chunks+tc.recovery {
				t.Errorf("want %d capture invocations (%d chunk(s) + one recovery pass of %d), got %d",
					chunks+tc.recovery, chunks, tc.recovery, c)
			}
			if n := unknownCount(got.Items()); n != len(tc.fleet.sessions) {
				t.Errorf("every session should read unknown when nothing was captured; %d/%d did",
					n, len(tc.fleet.sessions))
			}
			if src := got.Sources(); len(src) != 1 || src[0].Status != fleet.SourceDegraded {
				t.Errorf("source should read degraded, got %+v", src)
			}
			// Every slice derives from the call's deadline, so no invocation
			// may be handed one that runs past it, and none may run without one.
			callDeadline := callStart.Add(deadline)
			for i, dl := range b.capturedDeadlines() {
				if dl.IsZero() {
					t.Errorf("capture invocation %d ran with no deadline; every slice must derive "+
						"from the call's %v deadline", i, deadline)
				} else if dl.After(callDeadline) {
					t.Errorf("capture invocation %d was given a deadline %v past the call's %v deadline",
						i, dl.Sub(callDeadline), deadline)
				}
			}
		})
	}
}

// "context canceled" x3 in #156's own table: the caller was already gone.
// Retrying would only spend work on an answer nobody will read.
func TestCaptureNotRetriedWhenCallerCancelled(t *testing.T) {
	logs := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &budgetMux{
		fakeMux:   twoSessions(),
		stall:     func(int) bool { return true },
		onCapture: func(int) { cancel() },
	}
	d := newBudgetDriver(b, 2*time.Second)
	_, _ = d.List(ctx, testCaller, driver.ListFilter{})

	if c := b.captureCount(); c != 1 {
		t.Errorf("want exactly 1 capture invocation (no retry for a cancelled caller), got %d", c)
	}
	if !strings.Contains(logs.String(), "cause: "+captureCallerCancelled) {
		t.Errorf("failure line should name the cause %q; log:\n%s", captureCallerCancelled, logs)
	}
}

// The line the issue asks for: enough on it to tell "the multiplexer was slow"
// from "the budget was too small" without working it out again.
func TestCaptureFailureLogCarriesWallTime(t *testing.T) {
	logs := captureLog(t)
	b := &budgetMux{fakeMux: twoSessions(), stall: func(int) bool { return true }}
	d := newBudgetDriver(b, 600*time.Millisecond)
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	out := logs.String()
	for _, want := range []string{
		// The #141 prefix, word for word, so existing searches of the log still match.
		"tmux: batched capture returned nothing parseable for 2 pane(s) (chunk 1/1 of this enumeration)",
		"wall ",
		" slice (call budget left ",
		"listing took ",
		"cause: " + captureSliceExpired,
		"exit error: signal: killed",
		"retry: failed after ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("failure line lacks %q; log:\n%s", want, out)
		}
	}
	if c := d.Counters(); c[counterCaptureChunkFailed] != 1 || c[counterCaptureRetryFailed] != 1 {
		t.Errorf("counters: want chunk_failed=1 retry_failed=1, got %v", c)
	}
}

// A slow invocation that succeeds is logged and counted, so wall times are on
// record before the next failure and not only from failures.
func TestCaptureSlowSuccessIsLogged(t *testing.T) {
	logs := captureLog(t)
	const deadline = 1500 * time.Millisecond // slow line = deadline/15 = 100ms
	b := &budgetMux{fakeMux: twoSessions(), delay: 150 * time.Millisecond}
	d := newBudgetDriver(b, deadline)
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := unknownCount(got.Items()); n != 0 {
		t.Errorf("a slow but successful capture must still classify; %d unknown", n)
	}
	if !strings.Contains(logs.String(), "tmux: slow multiplexer invocation — capture chunk 1/1") {
		t.Errorf("slow success was not logged; log:\n%s", logs)
	}
	if c := d.Counters(); c[counterEnumerateSlowInvocation] != 1 {
		t.Errorf("want enumerate.slow_invocation=1, got %v", c)
	}
	if strings.Contains(logs.String(), "nothing parseable") {
		t.Errorf("a successful capture logged a failure line; log:\n%s", logs)
	}
}

// A healthy, fast capture costs nothing new: no retry, no log line.
func TestCaptureHealthyIsSilent(t *testing.T) {
	logs := captureLog(t)
	b := &budgetMux{fakeMux: twoSessions()}
	d := newBudgetDriver(b, 3*time.Second)
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if c := b.captureCount(); c != 1 {
		t.Errorf("want 1 capture invocation, got %d", c)
	}
	if logs.Len() != 0 {
		t.Errorf("healthy enumeration wrote to the log:\n%s", logs)
	}
}

func TestCaptureSliceNeverExceedsHalfRemaining(t *testing.T) {
	for _, tc := range []struct {
		remaining time.Duration
		pending   int
		want      time.Duration
	}{
		{30 * time.Second, 2, 10 * time.Second}, // one chunk + unspent retry
		{20 * time.Second, 1, 10 * time.Second}, // the retry itself, last invocation
		{30 * time.Second, 4, 6 * time.Second},  // three chunks + unspent retry
		{30 * time.Second, 0, 15 * time.Second}, // clamped: never more than half
		{0, 2, 0},
		{-time.Second, 2, 0},
	} {
		got := captureSlice(tc.remaining, tc.pending)
		if got != tc.want {
			t.Errorf("captureSlice(%v, %d) = %v, want %v", tc.remaining, tc.pending, got, tc.want)
		}
		if tc.remaining > 0 && got > tc.remaining/2 {
			t.Errorf("captureSlice(%v, %d) = %v, more than half of what is left", tc.remaining, tc.pending, got)
		}
	}
}

func equalInts(a, b []int) bool {
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
