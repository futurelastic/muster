package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Everything in this file is synthetic: fake drivers and invented session ids
// (muster #465).

// lateDriver is a feedDriver that also keeps the context and filter of every
// subscription it was given, so a test can tell what the lane pushed down to a
// runtime that joined it and whether the lane's end reached that runtime's pump.
type lateDriver struct {
	feedDriver

	lmu     sync.Mutex
	ctxs    []context.Context
	filters []driver.SubscribeFilter
}

func newLateDriver(failFirst int, failWith error) *lateDriver {
	return &lateDriver{feedDriver: *newFeedDriver(failFirst, failWith)}
}

func (d *lateDriver) Subscribe(ctx context.Context, req fleet.Request, f driver.SubscribeFilter) (driver.EventStream, error) {
	d.lmu.Lock()
	d.ctxs = append(d.ctxs, ctx)
	d.filters = append(d.filters, f)
	d.lmu.Unlock()
	return d.feedDriver.Subscribe(ctx, req, f)
}

func (d *lateDriver) seen() (ctxs []context.Context, filters []driver.SubscribeFilter) {
	d.lmu.Lock()
	defer d.lmu.Unlock()
	return append([]context.Context(nil), d.ctxs...), append([]driver.SubscribeFilter(nil), d.filters...)
}

// nextMatching reads ch until pred accepts an event and returns it, failing the
// test if none does in time. Events it skips are returned in skipped, so a test
// can also say what must NOT have been announced.
func nextMatching(t *testing.T, ch <-chan fleet.Event, what string, pred func(fleet.Event) bool) (got fleet.Event, skipped []fleet.Event) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("the subscription closed while waiting for %s", what)
			}
			if pred(ev) {
				return ev, skipped
			}
			skipped = append(skipped, ev)
		case <-deadline:
			t.Fatalf("no %s within 5s; saw %d other events: %v", what, len(skipped), eventKinds(skipped))
		}
	}
}

func eventKinds(evs []fleet.Event) []fleet.EventKind {
	out := make([]fleet.EventKind, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

func isStatus(want fleet.SourceState) func(fleet.Event) bool {
	return func(ev fleet.Event) bool {
		st, ok := ev.Payload.(fleet.SourceStatus)
		return ev.Kind == fleet.EventSourceStatus && ok && st.Status == want
	}
}

func isResync(ev fleet.Event) bool {
	p, ok := ev.Payload.(fleet.ControlResyncPayload)
	return ev.Kind == fleet.EventControlResync && ok && p.Reason == fleet.ResyncFeedGap
}

func isSessionID(id string) func(fleet.Event) bool {
	return func(ev fleet.Event) bool {
		p, ok := ev.Payload.(fleet.SessionStatePayload)
		return ev.Kind == fleet.EventSessionState && ok && p.Ref.ID == id
	}
}

// watchedService is a service with one runtime ("early"), already being watched
// by one unfiltered local subscriber and with its pump attached.
func watchedService(t *testing.T) (*Service, *lateDriver, <-chan fleet.Event) {
	t.Helper()
	svc := New("testbox")
	early := newLateDriver(0, nil)
	if err := svc.RegisterLocalDriver("early", early); err != nil {
		t.Fatal(err)
	}
	ch, _, _, cancel := svc.Events(context.Background(), ScopeLocal, driver.SubscribeFilter{}, 0, "")
	t.Cleanup(cancel)
	waitCovered(t, svc)
	return svc, early, ch
}

// #465 — the oracle. A runtime that registers after a watcher attached is
// pumped: an event published on its stream reaches the watcher, and the watcher
// is told, before it, that its view holds none of the sessions the runtime
// already had.
func TestRuntimeRegisteredAfterTheLaneStartedIsPumped(t *testing.T) {
	svc, early, ch := watchedService(t)

	late := newLateDriver(0, nil)
	if err := svc.RegisterLocalDriver("late", late); err != nil {
		t.Fatal(err)
	}

	// Told first: this machine is observed again, and what the watcher holds is
	// not the whole of it.
	_, skipped := nextMatching(t, ch, "source.status ok", isStatus(fleet.SourceOK))
	if len(skipped) != 0 {
		t.Fatalf("announced something before the restoration: %v", eventKinds(skipped))
	}
	if _, skipped := nextMatching(t, ch, "control.resync feed_gap", isResync); len(skipped) != 0 {
		t.Fatalf("announced something between the status and the resync: %v", eventKinds(skipped))
	}

	// And then it is heard.
	late.events <- sessionEvent("late-1", fleet.StatusWorking)
	nextMatching(t, ch, "the late runtime's session.state", isSessionID("late-1"))

	// The pump took the lane's own filter, and the runtimes already pumped were
	// left alone: a restart would have given them a fresh baseline that absorbs a
	// change without stamping it.
	_, lateFilters := late.seen()
	if len(lateFilters) != 1 || len(lateFilters[0].Sessions) != 0 || lateFilters[0].CwdPrefix != "" {
		t.Fatalf("late runtime subscribed with %+v, want one unfiltered subscription", lateFilters)
	}
	if n := early.Attempts(); n != 1 {
		t.Fatalf("the runtime that was already pumped subscribed %d times, want 1 (no restart)", n)
	}
	// The early runtime is still heard too.
	early.events <- sessionEvent("early-1", fleet.StatusIdle)
	nextMatching(t, ch, "the early runtime's session.state", isSessionID("early-1"))
}

// A pump that cannot attach yet behaves like any other that cannot: the machine
// is announced degraded, and the restoration — with the resync that says the
// watcher's view has a hole — arrives when it does attach, once.
func TestLateRuntimeThatCannotSubscribeYetIsAnnouncedWhenItCan(t *testing.T) {
	svc, _, ch := watchedService(t)

	late := newLateDriver(1, errors.New("the runtime is not answering yet"))
	if err := svc.RegisterLocalDriver("late", late); err != nil {
		t.Fatal(err)
	}

	nextMatching(t, ch, "source.status degraded", isStatus(fleet.SourceDegraded))
	// The retry is one backoff away; one restoration, not two.
	_, skipped := nextMatching(t, ch, "source.status ok", isStatus(fleet.SourceOK))
	if len(skipped) != 0 {
		t.Fatalf("announced something between the degradation and its restoration: %v", eventKinds(skipped))
	}
	nextMatching(t, ch, "control.resync feed_gap", isResync)

	late.events <- sessionEvent("late-2", fleet.StatusWorking)
	_, skipped = nextMatching(t, ch, "the late runtime's session.state", isSessionID("late-2"))
	for _, ev := range skipped {
		if ev.Kind == fleet.EventControlResync || ev.Kind == fleet.EventSourceStatus {
			t.Fatalf("a second restoration was announced for one joining runtime: %v", eventKinds(skipped))
		}
	}
}

// Nobody watching means no lane: a runtime registered then is seated by the
// lane the next watcher starts, with every other, and nothing is announced.
func TestRuntimeRegisteredWithNoLaneIsSeatedByTheNextLane(t *testing.T) {
	svc := New("testbox")
	early, late := newLateDriver(0, nil), newLateDriver(0, nil)
	if err := svc.RegisterLocalDriver("early", early); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("late", late); err != nil {
		t.Fatal(err)
	}
	settle()
	if early.Attempts() != 0 || late.Attempts() != 0 {
		t.Fatalf("subscribed with nobody watching: early %d, late %d", early.Attempts(), late.Attempts())
	}

	ch, _, _, cancel := svc.Events(context.Background(), ScopeLocal, driver.SubscribeFilter{}, 0, "")
	t.Cleanup(cancel)
	waitCovered(t, svc)
	settle()
	if early.Attempts() != 1 || late.Attempts() != 1 {
		t.Fatalf("the new lane subscribed early %d, late %d times, want 1 each", early.Attempts(), late.Attempts())
	}
	select {
	case ev := <-ch:
		t.Fatalf("a lane that starts with every runtime registered announced %s", ev.Kind)
	default:
	}
}

// A runtime registered a second time is not pumped a second time: two pumps on
// one driver publish every event twice (#375).
func TestRegisteringAnAlreadyPumpedRuntimeAgainDoesNotPumpItTwice(t *testing.T) {
	svc, early, _ := watchedService(t)
	if err := svc.RegisterLocalDriver("early", early); err != nil {
		t.Fatal(err)
	}
	settle()
	if n := early.Attempts(); n != 1 {
		t.Fatalf("the runtime was subscribed %d times, want 1", n)
	}
}

// The joined pump belongs to the lane: when the last watcher leaves and the
// linger runs out, it ends with the others.
func TestLaneEndingEndsTheJoinedPump(t *testing.T) {
	svc := New("testbox")
	svc.events.linger = 0
	early := newLateDriver(0, nil)
	if err := svc.RegisterLocalDriver("early", early); err != nil {
		t.Fatal(err)
	}
	_, _, _, cancel := svc.Events(context.Background(), ScopeLocal, driver.SubscribeFilter{}, 0, "")
	waitCovered(t, svc)

	late := newLateDriver(0, nil)
	if err := svc.RegisterLocalDriver("late", late); err != nil {
		t.Fatal(err)
	}
	waitCovered(t, svc)

	cancel()
	ctxs, _ := late.seen()
	if len(ctxs) != 1 {
		t.Fatalf("late runtime subscribed %d times, want 1", len(ctxs))
	}
	select {
	case <-ctxs[0].Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the lane ended and the joined pump was left running")
	}
}

// Registration and the lane starting race: whichever wins, the runtime is
// pumped exactly once — seated by the lane or joined to it, never both and
// never neither. Run in fresh services with both released from one gate, since
// the window is a few instructions wide.
func TestRegistrationRacingTheLaneStartPumpsTheRuntimeOnce(t *testing.T) {
	const rounds = 30
	for round := 0; round < rounds; round++ {
		svc := New("testbox")
		svc.events.linger = 0
		early, late := newLateDriver(0, nil), newLateDriver(0, nil)
		if err := svc.RegisterLocalDriver("early", early); err != nil {
			t.Fatal(err)
		}

		gate := make(chan struct{})
		var wg sync.WaitGroup
		var cancel func()
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			if err := svc.RegisterLocalDriver("late", late); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-gate
			_, _, _, cancel = svc.Events(context.Background(), ScopeLocal, driver.SubscribeFilter{}, 0, "")
		}()
		close(gate)
		wg.Wait()

		waitCovered(t, svc)
		settle()
		if n := late.Attempts(); n != 1 {
			t.Fatalf("round %d: late runtime subscribed %d times, want 1", round, n)
		}
		if n := early.Attempts(); n != 1 {
			t.Fatalf("round %d: early runtime subscribed %d times, want 1", round, n)
		}
		cancel()
	}
}
