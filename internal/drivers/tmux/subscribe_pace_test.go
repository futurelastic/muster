package tmux

import (
	"context"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// #415: the gap after a listing is proportional to what the listing cost, so a
// big fleet is paced and a small one is left exactly as it was.
func TestRelistGapIsProportionalToTheCostAndBounded(t *testing.T) {
	cases := []struct {
		cost, want time.Duration
	}{
		{0, 0},
		{-time.Second, 0},
		{10 * time.Millisecond, 20 * time.Millisecond},
		{700 * time.Millisecond, 1400 * time.Millisecond}, // the measured 80-session pass
		{10 * time.Second, relistMaxGap},                  // a pathological one does not stretch it unboundedly
	}
	for _, c := range cases {
		if got := relistGap(c.cost); got != c.want {
			t.Errorf("relistGap(%v) = %v, want %v", c.cost, got, c.want)
		}
	}
	// 26 ms is what a 22-session listing was measured at. Its gap must be
	// shorter than the coalesce window the loop already waits, or a small
	// machine's feed would be slower than it was.
	if got := relistGap(26 * time.Millisecond); got >= coalesceWindow {
		t.Errorf("a 26 ms listing earns a %v gap, which exceeds the %v coalesce window: small fleets would be paced", got, coalesceWindow)
	}
}

func listPanesCalls(f *fakeMux) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) > 1 && c[0] == "list-panes" && c[1] == "-a" {
			n++
		}
	}
	return n
}

// The measured shape: one session keeps printing, the fleet's listing is
// expensive, and the stream used to list back to back for as long as it
// printed. Counted over a fixed burst, against what the unpaced loop does in
// the same time (a pass every coalesce window plus its own cost: about eleven
// here), and with a real state change in the middle that must still arrive.
func TestSustainedOutputOnAnExpensiveFleetCostsBoundedListings(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}

	const listCost = 50 * time.Millisecond
	slow := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "list-panes" && args[1] == "-a" {
			time.Sleep(listCost)
		}
		return f.exec(ctx, name, args...)
	}
	d := New("testbox",
		withExec(slow),
		withCtlDialer(r.dial),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	)
	d.relistGap = func(cost time.Duration) time.Duration { return 12 * cost } // 600 ms here

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	baseline := listPanesCalls(f)
	content := r.contentFor("alpha💬")

	// One session keeps printing the whole time; alpha changes state once, a
	// second in. Events are collected on the side so the burst is a fixed length.
	const burst = 2400 * time.Millisecond
	ctx, cancelRead := context.WithCancel(context.Background())
	events := make(chan fleet.Event, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			ev, err := s.Next(ctx)
			if err != nil {
				return
			}
			select {
			case events <- ev:
			default:
			}
		}
	}()
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				fire(content, "output")
			}
		}
	}()
	time.AfterFunc(time.Second, func() { f.setCapture("%1", fixtureWorking) })

	time.Sleep(burst)
	close(stop)
	cancelRead()
	wg.Wait()

	gotWorking := false
	for len(events) > 0 {
		ev := <-events
		if p, ok := ev.Payload.(fleet.SessionStatePayload); ok && p.Ref.ID == "alpha💬" && p.State.Status == fleet.StatusWorking {
			gotWorking = true
		}
	}

	passes := listPanesCalls(f) - baseline
	if passes < 2 {
		t.Fatalf("only %d listings in %v: the stream stopped reading", passes, burst)
	}
	if passes > 6 {
		t.Errorf("%d full listings in %v of continuous output on a fleet whose listing costs %v — "+
			"a pass every coalesce window is ~11; the gap after each listing is not being waited out",
			passes, burst, listCost)
	}
	if !gotWorking {
		t.Error("alpha's state change never arrived while the stream was being paced: pacing may delay an edge, never lose one")
	}
}
