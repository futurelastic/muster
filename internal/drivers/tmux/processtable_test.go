package tmux

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/driver"
)

// A fake clock and a fake `ps` the tests can steer: how many times the table
// was read, what it holds, whether it fails, and whether a read is held open.
type tableEnv struct {
	mu    sync.Mutex
	now   time.Time
	out   string
	err   error
	hold  chan struct{} // when non-nil, a read blocks until it is closed or its ctx ends
	calls int32
	d     *Driver
}

func newTableEnv(t *testing.T, table string) *tableEnv {
	t.Helper()
	e := &tableEnv{now: time.Unix(1_800_000_000, 0), out: table}
	e.d = New("testbox",
		withClock(func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }),
		withPSExec(e.ps),
		WithProcessAgent(),
	)
	return e
}

func (e *tableEnv) ps(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	atomic.AddInt32(&e.calls, 1)
	e.mu.Lock()
	out, err, hold := e.out, e.err, e.hold
	e.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []byte(out), err
}

func (e *tableEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *tableEnv) set(out string, err error) {
	e.mu.Lock()
	e.out, e.err = out, err
	e.mu.Unlock()
}

func (e *tableEnv) reads() int { return int(atomic.LoadInt32(&e.calls)) }

// agents asks for the agents of the given sessions, the way a listing does.
func (e *tableEnv) agents(t *testing.T, roots map[string]int) (map[string]string, bool) {
	t.Helper()
	rows := make([]paneRow, 0, len(roots))
	for s, pid := range roots {
		rows = append(rows, paneRow{session: s, pid: pid})
	}
	return e.d.processAgents(context.Background(), rows)
}

const oneAgent = "100 1 100 100 claude --agent x\n"

func TestProcessTableIsSharedInsideItsLifetime(t *testing.T) {
	e := newTableEnv(t, oneAgent)
	roots := map[string]int{"a": 100}
	for i := 0; i < 5; i++ {
		got, ok := e.agents(t, roots)
		if !ok || got["a"] != "x" {
			t.Fatalf("read %d: agents = %v, ok = %v", i, got, ok)
		}
		e.advance(procTableTTL / 10)
	}
	if n := e.reads(); n != 1 {
		t.Fatalf("process table read %d times inside one lifetime, want 1", n)
	}
	// Past the lifetime it is retaken, and the new table is what answers.
	e.set("100 1 100 100 claude --agent y\n", nil)
	e.advance(procTableTTL)
	got, _ := e.agents(t, roots)
	if e.reads() != 2 || got["a"] != "y" {
		t.Fatalf("after the lifetime: reads = %d, agent = %q; want 2 reads and y", e.reads(), got["a"])
	}
}

// The sharing is across callers, not across a listing's own loop: many
// concurrent listings cost one read while it is in flight.
func TestProcessTableConcurrentCallersShareOneRead(t *testing.T) {
	e := newTableEnv(t, oneAgent)
	hold := make(chan struct{})
	e.mu.Lock()
	e.hold = hold
	e.mu.Unlock()

	const callers = 24
	var wg sync.WaitGroup
	results := make(chan string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _ := e.agents(t, map[string]int{"a": 100})
			results <- got["a"]
		}()
	}
	// Let every caller reach the wait, then release the one read.
	deadline := time.Now().Add(5 * time.Second)
	for e.reads() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(hold)
	wg.Wait()
	close(results)
	for a := range results {
		if a != "x" {
			t.Fatalf("a caller got agent %q, want x", a)
		}
	}
	if n := e.reads(); n != 1 {
		t.Fatalf("%d concurrent callers took %d reads, want 1", callers, n)
	}
}

// A pane the snapshot predates is evidence the snapshot is too old for this
// listing — but only once it has some age, so the rule is bounded.
func TestProcessTableRetakenForAPaneItDoesNotContain(t *testing.T) {
	e := newTableEnv(t, oneAgent)
	e.agents(t, map[string]int{"a": 100})

	// A new pane appears at once: the snapshot is too young to retake (the worst
	// case stays one read per procTableMinAge, not one per call).
	e.set(oneAgent+"300 1 300 300 claude --agent z\n", nil)
	got, _ := e.agents(t, map[string]int{"a": 100, "c": 300})
	if e.reads() != 1 || got["c"] != "" {
		t.Fatalf("inside the minimum age: reads = %d, c = %q; want the held snapshot", e.reads(), got["c"])
	}
	e.advance(procTableMinAge)
	got, _ = e.agents(t, map[string]int{"a": 100, "c": 300})
	if e.reads() != 2 || got["c"] != "z" {
		t.Fatalf("past the minimum age: reads = %d, c = %q; want a retake that sees the new pane", e.reads(), got["c"])
	}
	// A pane the table genuinely never contains does not make every call read.
	for i := 0; i < 3; i++ {
		e.agents(t, map[string]int{"a": 100, "ghost": 999})
	}
	if e.reads() > 3 {
		t.Fatalf("a pane absent from the table caused %d reads in one instant", e.reads())
	}
}

func TestProcessTableFailureIsSharedBrieflyThenRetried(t *testing.T) {
	e := newTableEnv(t, oneAgent)
	e.set("", errors.New("ps: boom"))
	for i := 0; i < 4; i++ {
		if _, ok := e.agents(t, map[string]int{"a": 100}); ok {
			t.Fatal("a failed read reported ok")
		}
	}
	if n := e.reads(); n != 1 {
		t.Fatalf("a burst after a failure took %d reads, want 1", n)
	}
	if got := e.d.Counters()[counterAgentUnread]; got != 4 {
		t.Errorf("%s = %d, want 4 (one per degraded listing)", counterAgentUnread, got)
	}
	e.set(oneAgent, nil)
	e.advance(procTableErrTTL)
	got, ok := e.agents(t, map[string]int{"a": 100})
	if !ok || got["a"] != "x" || e.reads() != 2 {
		t.Fatalf("after the failure window: ok = %v, agent = %q, reads = %d", ok, got["a"], e.reads())
	}
}

// A caller that gives up says nothing about the machine: its failed read is not
// remembered, and the next caller reads for itself.
func TestProcessTableACancelledReadIsNotRemembered(t *testing.T) {
	e := newTableEnv(t, oneAgent)
	hold := make(chan struct{})
	e.mu.Lock()
	e.hold = hold
	e.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.d.processAgents(ctx, []paneRow{{session: "a", pid: 100}})
	}()
	for e.reads() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	e.mu.Lock()
	e.hold = nil
	e.mu.Unlock()
	got, ok := e.agents(t, map[string]int{"a": 100})
	if !ok || got["a"] != "x" {
		t.Fatalf("after a cancelled read: ok = %v, agent = %q; want a fresh read to succeed", ok, got["a"])
	}
	if n := e.reads(); n != 2 {
		t.Fatalf("reads = %d, want 2 (the cancelled one was not cached)", n)
	}
}

// A waiter is only as patient as its own context.
func TestProcessTableAWaiterHonoursItsOwnContext(t *testing.T) {
	e := newTableEnv(t, oneAgent)
	hold := make(chan struct{})
	defer close(hold)
	e.mu.Lock()
	e.hold = hold
	e.mu.Unlock()
	go e.agents(t, map[string]int{"a": 100}) // the leader, held open
	for e.reads() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.d.processTable(ctx, map[string]int{"a": 100}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a waiter past its deadline got %v, want its own deadline error", err)
	}
}

// Listings through the public path: many Lists, one read, and the answer is the
// same on the last as on the first.
func TestListsInsideOneLifetimeShareOneProcessTableRead(t *testing.T) {
	f := twoSessions()
	var calls int32
	d := agentTestDriver(f, psTable(agentHostTable, &calls), nil)
	for i := 0; i < 6; i++ {
		col := listWithAgent(t, d, driver.ListFilter{})
		agents := map[string]string{}
		for _, s := range col.Items() {
			agents[s.ID] = string(s.Agent)
		}
		if agents["alpha💬"] != "x" || agents["beta"] != "" {
			t.Fatalf("list %d: agents = %v", i, agents)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("six listings took %d process-table reads, want 1", n)
	}
}
