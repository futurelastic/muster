package opencode

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #419: an idle server is asked for its status once per window, not once per
// listing. These tests count the requests the fake runtime receives; the cost they
// stand for (about 2 % of a core per request per second, per server) was measured
// against a real server and is in statuscache.go.

// stepClock is a clock a test moves by hand.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock() *stepClock { return &stepClock{t: time.Now()} }

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// cachedDriver is a driver over f with the production windows and a clock the test
// moves. The clock starts past the settle a create leaves behind, so a test that
// does nothing else is looking at a cache that is open.
func cachedDriver(t *testing.T, f *fakeServer) (*Driver, *stepClock) {
	t.Helper()
	clk := newStepClock()
	d, err := newDriverWithOptions(t, f, withClock(clk.now), withStatusTTL(statusTTLBus, statusTTLBare))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, clk
}

// readsOf counts the requests the fake received whose path is exactly path, or
// begins with it when prefix is set.
func readsOf(f *fakeServer, method, path string, prefix bool) int {
	n := 0
	for _, r := range f.requestsSnapshot() {
		if r.method != method {
			continue
		}
		if r.path == path || (prefix && strings.HasPrefix(r.path, path)) {
			n++
		}
	}
	return n
}

func statusReads(f *fakeServer) int { return readsOf(f, "GET", "/session/status", false) }

func listAll(t *testing.T, d *Driver) map[string]fleet.Session {
	t.Helper()
	return listed(t, d)
}

func TestList_AnIdleServerIsAskedForItsStatusOncePerWindow(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	clk.advance(writeSettle + time.Second)

	before := statusReads(f)
	for i := 0; i < 100; i++ {
		if got := listAll(t, d)[ref.ID].State.Status; got != fleet.StatusIdle {
			t.Fatalf("listing %d: status = %q, want idle", i, got)
		}
	}
	if got := statusReads(f) - before; got != 1 {
		t.Errorf("100 listings made %d status reads, want 1 (the window had not ended)", got)
	}

	clk.advance(statusTTLBare + time.Second)
	listAll(t, d)
	if got := statusReads(f) - before; got != 2 {
		t.Errorf("after the window, %d status reads in all, want 2", got)
	}
}

func TestList_ConcurrentListingsShareOneStatusRead(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	createOne(t, d, "/work/x", "key-1")
	clk.advance(writeSettle + time.Second)
	before := statusReads(f)

	hold := make(chan struct{})
	f.mu.Lock()
	f.statusHold = hold
	f.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.List(context.Background(), testReq, driver.ListFilter{}); err != nil {
				t.Errorf("List: %v", err)
			}
		}()
	}
	waitFor(t, "the one read to reach the runtime", func() bool { return statusReads(f)-before >= 1 })
	time.Sleep(50 * time.Millisecond) // let the others find it in flight
	close(hold)
	wg.Wait()
	if got := statusReads(f) - before; got != 1 {
		t.Errorf("20 concurrent listings made %d status reads, want 1", got)
	}
}

// The runtime accepts a prompt before it reports the session working, so a read in
// that gap must not be what the next listings are answered from.
func TestWrites_DropWhatWasReadAndKeepNothingForAMoment(t *testing.T) {
	cases := []struct {
		name string
		// arrange runs with the cache full of the first answer, then changes the
		// runtime without telling the driver.
		arrange func(f *fakeServer, id string)
		write   func(t *testing.T, d *Driver, ref fleet.SessionRef)
		first   fleet.Status
		then    fleet.Status
	}{
		{
			name:    "Send",
			arrange: func(f *fakeServer, id string) { f.setBusy(id) },
			write: func(t *testing.T, d *Driver, ref fleet.SessionRef) {
				if _, err := d.Send(context.Background(), testReq, ref, "go", driver.SendOptions{Submit: true}); err != nil {
					t.Fatalf("Send: %v", err)
				}
			},
			first: fleet.StatusIdle, then: fleet.StatusWorking,
		},
		{
			name:    "Interrupt",
			arrange: func(f *fakeServer, id string) { f.setBusy(id) },
			write: func(t *testing.T, d *Driver, ref fleet.SessionRef) {
				if _, err := d.Interrupt(context.Background(), testReq, ref); err != nil {
					t.Fatalf("Interrupt: %v", err)
				}
			},
			first: fleet.StatusIdle, then: fleet.StatusWorking,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServer(t)
			d, clk := cachedDriver(t, f)
			ref := createOne(t, d, "/work/x", "key-1")
			clk.advance(writeSettle + time.Second)

			if got := listAll(t, d)[ref.ID].State.Status; got != tc.first {
				t.Fatalf("first listing: %q, want %q", got, tc.first)
			}
			tc.arrange(f, ref.ID) // no event, no write: the driver cannot know
			if got := listAll(t, d)[ref.ID].State.Status; got != tc.first {
				t.Fatalf("an unannounced change was seen at once (%q): the answer was not kept, so nothing is proven below", got)
			}

			tc.write(t, d, ref)
			if got := listAll(t, d)[ref.ID].State.Status; got != tc.then {
				t.Errorf("after %s the listing says %q, want %q: the write did not drop what was read", tc.name, got, tc.then)
			}

			// And nothing is kept for a moment: every listing asks while the runtime
			// may still be catching up with the write.
			base := statusReads(f)
			for i := 0; i < 5; i++ {
				listAll(t, d)
			}
			if got := statusReads(f) - base; got != 5 {
				t.Errorf("5 listings right after a write made %d status reads, want 5 (nothing is kept for %s)", got, writeSettle)
			}
			clk.advance(writeSettle + time.Second)
			base = statusReads(f)
			for i := 0; i < 5; i++ {
				listAll(t, d)
			}
			if got := statusReads(f) - base; got != 1 {
				t.Errorf("5 listings after the settle made %d status reads, want 1", got)
			}
		})
	}
}

func TestList_ABusEventDropsWhatWasRead(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	s := subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus to connect", func() bool { return f.busConns() == 1 })
	clk.advance(writeSettle + time.Second)

	if got := listAll(t, d)[ref.ID].State.Status; got != fleet.StatusIdle {
		t.Fatalf("first listing: %q, want idle", got)
	}
	f.setBusy(ref.ID)
	if got := listAll(t, d)[ref.ID].State.Status; got != fleet.StatusIdle {
		t.Fatalf("an unannounced change was seen at once (%q): nothing is proven below", got)
	}

	f.emit("session.status", map[string]any{"sessionID": ref.ID, "status": map[string]any{"type": "busy"}})
	ev := next(t, s)
	if ev.Kind != fleet.EventSessionState {
		t.Fatalf("event kind = %q, want session.state", ev.Kind)
	}
	if got := listAll(t, d)[ref.ID].State.Status; got != fleet.StatusWorking {
		t.Errorf("after the event the listing says %q, want working", got)
	}
}

func TestList_AttachedStreamLengthensTheWindowAndLosingItShortensIt(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	createOne(t, d, "/work/x", "key-1")
	clk.advance(writeSettle + time.Second)

	// No stream: the short window.
	base := statusReads(f)
	listAll(t, d)
	clk.advance(statusTTLBare + time.Second)
	listAll(t, d)
	if got := statusReads(f) - base; got != 2 {
		t.Fatalf("with no stream, two listings %s apart made %d reads, want 2", statusTTLBare+time.Second, got)
	}

	// A stream: the long one.
	subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus to connect", func() bool { return f.busConns() == 1 })
	listAll(t, d) // the attach dropped what was read; this fills it again
	base = statusReads(f)
	clk.advance(10 * time.Second)
	listAll(t, d)
	if got := statusReads(f) - base; got != 0 {
		t.Errorf("with a stream attached, a listing 10 s later made %d reads, want 0", got)
	}
	clk.advance(statusTTLBus)
	listAll(t, d)
	if got := statusReads(f) - base; got != 1 {
		t.Errorf("past the long window, %d reads, want 1 (a silent stream is bounded)", got)
	}

	// The stream ends while the server stays up: back to the short window.
	f.cutBus()
	waitFor(t, "the stream to be dropped", func() bool { return f.busConns() == 0 })
	waitFor(t, "the driver to notice", func() bool {
		srv := d.shared
		srv.status.mu.Lock()
		defer srv.status.mu.Unlock()
		return srv.status.buses == 0
	})
	listAll(t, d) // fills again
	base = statusReads(f)
	clk.advance(statusTTLBare + time.Second)
	listAll(t, d)
	if got := statusReads(f) - base; got != 1 {
		t.Errorf("after the stream ended, a listing %s later made %d reads, want 1", statusTTLBare+time.Second, got)
	}
}

func TestList_AFailedStatusReadIsNeverKept(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	clk.advance(writeSettle + time.Second)

	f.mu.Lock()
	f.statusDown = true
	f.mu.Unlock()
	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := col.Items()[0].State.Status; got != fleet.StatusUnknown {
		t.Fatalf("status with the runtime down = %q, want unknown", got)
	}
	// Still down: the failure is reported again, not replaced by an answer.
	if got := listAll(t, d)[ref.ID].State.Status; got != fleet.StatusUnknown {
		t.Errorf("second listing with the runtime still down = %q, want unknown", got)
	}

	f.mu.Lock()
	f.statusDown = false
	f.mu.Unlock()
	if got := listAll(t, d)[ref.ID].State.Status; got != fleet.StatusIdle {
		t.Errorf("after the runtime came back = %q, want idle at once (a failure is not kept)", got)
	}
}

// A read that was in flight when something dropped the cache answers the callers
// that were already waiting for it, and is not kept for the ones after.
func TestList_AReadThatRacesADropIsNotKept(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	createOne(t, d, "/work/x", "key-1")
	clk.advance(writeSettle + time.Second)
	before := statusReads(f)

	hold := make(chan struct{})
	f.mu.Lock()
	f.statusHold = hold
	f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = d.List(context.Background(), testReq, driver.ListFilter{})
	}()
	waitFor(t, "the read to reach the runtime", func() bool { return statusReads(f)-before >= 1 })

	d.shared.status.invalidate() // the runtime said something changed
	f.mu.Lock()
	f.statusHold = nil
	f.mu.Unlock()
	close(hold)
	<-done

	listAll(t, d)
	if got := statusReads(f) - before; got != 2 {
		t.Errorf("%d status reads, want 2: the read that raced the drop was kept", got)
	}
}

// One caller giving up must not fail the callers waiting on the same read, and
// must itself be answered when its own context ends.
func TestList_ACallerThatGivesUpDoesNotFailTheOthers(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	createOne(t, d, "/work/x", "key-1")
	clk.advance(writeSettle + time.Second)
	before := statusReads(f)

	hold := make(chan struct{})
	f.mu.Lock()
	f.statusHold = hold
	f.mu.Unlock()

	patient := make(chan error, 1)
	go func() {
		_, err := d.List(context.Background(), testReq, driver.ListFilter{})
		patient <- err
	}()
	waitFor(t, "the read to reach the runtime", func() bool { return statusReads(f)-before >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	col, err := d.List(ctx, testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("the impatient List: %v", err)
	}
	if got := col.Items()[0].State.Status; got != fleet.StatusUnknown {
		t.Errorf("the impatient listing says %q, want unknown (its own context ended)", got)
	}

	close(hold)
	if err := <-patient; err != nil {
		t.Errorf("the patient List failed because another caller gave up: %v", err)
	}
	if got := statusReads(f) - before; got != 1 {
		t.Errorf("%d status reads, want 1", got)
	}
}

func TestState_AnIdleSessionIsConfirmedOncePerWindow(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	clk.advance(writeSettle + time.Second)

	counts := func() (status, session, messages int) {
		return statusReads(f),
			readsOf(f, "GET", "/session/"+ref.ID, false),
			readsOf(f, "GET", "/session/"+ref.ID+"/message", false)
	}
	s0, e0, m0 := counts()
	for i := 0; i < 50; i++ {
		st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: ref.ID})
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if st.Status != fleet.StatusIdle {
			t.Fatalf("State %d: %q, want idle", i, st.Status)
		}
	}
	s1, e1, m1 := counts()
	if s1-s0 != 1 || e1-e0 != 1 {
		t.Errorf("50 States made %d status and %d session reads, want 1 and 1", s1-s0, e1-e0)
	}
	// The newest message (the last turn's verdict) and the usage history: one read
	// of each, however many polls.
	if m1-m0 > 2 {
		t.Errorf("50 States made %d message reads, want at most 2", m1-m0)
	}

	clk.advance(statusTTLBare + time.Second)
	if _, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: ref.ID}); err != nil {
		t.Fatalf("State: %v", err)
	}
	s2, e2, _ := counts()
	if s2-s1 != 1 || e2-e1 != 1 {
		t.Errorf("a State after the window made %d status and %d session reads, want 1 and 1", s2-s1, e2-e1)
	}
}

// A failed turn is how a session reads idle with a lastTurn; the event that ends a
// turn drops the confirmation, so the failure is not hidden behind a kept one.
func TestState_AFailedTurnIsSeenAfterItsEvent(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	subscribe(t, d, driver.SubscribeFilter{})
	waitFor(t, "the bus to connect", func() bool { return f.busConns() == 1 })
	clk.advance(writeSettle + time.Second)

	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: ref.ID})
	if err != nil || st.LastTurn != nil {
		t.Fatalf("first State: %+v, %v; want idle with no lastTurn", st, err)
	}

	f.addMessage(ref.ID, map[string]any{
		"info": map[string]any{"id": "msg_2", "role": "assistant", "sessionID": ref.ID,
			"time":  map[string]any{"created": time.Now().UnixMilli()},
			"error": map[string]any{"name": "APIError", "data": map[string]any{"message": "refused", "statusCode": 402}}},
		"parts": []any{},
	})
	// Not announced yet: the kept confirmation is still what answers.
	st, _ = d.State(context.Background(), testReq, fleet.SessionRef{ID: ref.ID})
	if st.LastTurn != nil {
		t.Fatalf("an unannounced failure was seen at once; nothing is proven below")
	}
	f.emit("message.updated", map[string]any{"info": map[string]any{"id": "msg_2", "sessionID": ref.ID}})
	waitFor(t, "the failure to be seen", func() bool {
		st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: ref.ID})
		return err == nil && st.LastTurn != nil && st.LastTurn.Outcome == "failed"
	})
}

func TestState_AParkedAskIsReadOncePerWindow(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)
	f.mu.Lock()
	f.permissions = []wirePermissionAsk{{ID: "per_1", SessionID: ref.ID, Permission: "bash", Patterns: []string{"ls"}}}
	f.mu.Unlock()
	clk.advance(writeSettle + time.Second)

	before := readsOf(f, "GET", "/permission", false)
	for i := 0; i < 50; i++ {
		got := listAll(t, d)[ref.ID].State
		if got.Status != fleet.StatusWaitingInput {
			t.Fatalf("listing %d: %q, want waiting_input", i, got.Status)
		}
	}
	if n := readsOf(f, "GET", "/permission", false) - before; n != 1 {
		t.Errorf("50 listings of a parked session read the asks %d times, want 1", n)
	}
}

// Answering an ask is done against the ask that exists now, never a remembered one.
func TestRespond_ReadsTheAsksFreshAndDropsWhatWasRead(t *testing.T) {
	f := newFakeServer(t)
	d, clk := cachedDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)
	f.mu.Lock()
	f.permissions = []wirePermissionAsk{{ID: "per_1", SessionID: ref.ID, Permission: "bash", Patterns: []string{"ls"}}}
	f.mu.Unlock()
	clk.advance(writeSettle + time.Second)
	listAll(t, d) // keeps the asks

	before := readsOf(f, "GET", "/permission", false)
	rcpt, err := d.Respond(context.Background(), testReq, ref, fleet.Response{Choice: 1, Nonce: "per_1"})
	if err != nil || rcpt.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("Respond: %+v, %v", rcpt, err)
	}
	if n := readsOf(f, "GET", "/permission", false) - before; n != 1 {
		t.Errorf("Respond read the asks %d times, want 1 (fresh, not from memory)", n)
	}
	if got := listAll(t, d)[ref.ID].State.Status; got != fleet.StatusIdle {
		t.Errorf("after the answer the listing says %q, want idle", got)
	}
}
