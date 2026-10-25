package service

import (
	"context"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// recordingDriver streams nothing and remembers the filter of every
// subscription it was asked for, so a test can read what the hub pushed down to
// a driver and how many times it restarted one.
type recordingDriver struct {
	stub.Driver

	mu      sync.Mutex
	filters []driver.SubscribeFilter
}

func newRecordingDriver() *recordingDriver {
	return &recordingDriver{Driver: stub.Driver{DeadlineMs: 200}}
}

func (d *recordingDriver) Subscribe(ctx context.Context, _ fleet.Request, f driver.SubscribeFilter) (driver.EventStream, error) {
	d.mu.Lock()
	d.filters = append(d.filters, f)
	d.mu.Unlock()
	return &feedStream{events: make(chan fleet.Event)}, nil
}

// Filters returns the filter of each subscription so far, oldest first.
func (d *recordingDriver) Filters() []driver.SubscribeFilter {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]driver.SubscribeFilter(nil), d.filters...)
}

// waitSubscribes blocks until d has been subscribed to at least want times.
func waitSubscribes(t *testing.T, d *recordingDriver, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(d.Filters()) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(d.Filters()); got < want {
		t.Fatalf("driver subscribed %d times, want at least %d", got, want)
	}
}

// settle gives a pump goroutine that should NOT subscribe a moment to do so, so
// a count that stays put means something.
func settle() { time.Sleep(60 * time.Millisecond) }

func pushdownService(t *testing.T) (*Service, *recordingDriver, *recordingDriver) {
	t.Helper()
	svc := New("testbox")
	svc.events.linger = 0
	local, peer := newRecordingDriver(), newRecordingDriver()
	if err := svc.RegisterLocalDriver("fake", local); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	if err := svc.RegisterPeerDriver("peerbox", peer); err != nil {
		t.Fatalf("RegisterPeerDriver: %v", err)
	}
	svc.SetPeerCredential("peer-token")
	return svc, local, peer
}

func subscribe(t *testing.T, svc *Service, scope Scope, f driver.SubscribeFilter) func() {
	t.Helper()
	_, _, _, cancel := svc.Events(context.Background(), scope, f, 0, "")
	t.Cleanup(cancel)
	return cancel
}

// The point of #372: with every subscriber asking by directory, the peer is
// asked for that directory — and the local drivers are not, because they apply
// the prefix themselves to frames whose directory they may not know (#361).
func TestPeerPumpIsOpenedWithTheDirectoryPrefix(t *testing.T) {
	svc, local, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/a"})
	waitSubscribes(t, local, 1)
	waitSubscribes(t, peer, 1)

	if got := local.Filters()[0]; got.CwdPrefix != "" || len(got.Sessions) != 0 {
		t.Errorf("local driver filter = %+v, want no restriction", got)
	}
	if got := peer.Filters()[0]; got.CwdPrefix != "/work/a" || len(got.Sessions) != 0 {
		t.Errorf("peer filter = %+v, want cwdPrefix /work/a", got)
	}
}

// Sessions and directory travel together to the peer, each as the union of what
// its subscribers asked.
func TestPeerPumpCarriesSessionsAndPrefix(t *testing.T) {
	svc, _, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{Sessions: []string{"x"}, CwdPrefix: "/work/a"})
	waitSubscribes(t, peer, 1)

	got := peer.Filters()[0]
	if got.CwdPrefix != "/work/a" || len(got.Sessions) != 1 || got.Sessions[0] != "x" {
		t.Errorf("peer filter = %+v, want session x under /work/a", got)
	}
}

// A subscriber that arrives asking by directory — and one that leaves — must not
// restart the local pumps for everybody else: a restart takes a fresh baseline,
// which can absorb a change that happened in between (defaultLinger). Its
// filter for the local lane does not change, so neither does the lane.
func TestDirectoryOnlySubscriberNeverRestartsLocalPumps(t *testing.T) {
	svc, local, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/a"})
	waitSubscribes(t, local, 1)
	waitSubscribes(t, peer, 1)

	cancel := subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/b"})
	waitSubscribes(t, peer, 2) // the peers must now serve both directories
	cancel()
	settle()

	if n := len(local.Filters()); n != 1 {
		t.Errorf("local pumps subscribed %d times, want 1: a directory-only subscriber changes nothing for them", n)
	}
}

// The peers are asked for the common prefix of the two directories, and once
// the wider subscriber leaves nothing is restarted: a pump that is wider than
// needed is still correct, since the hub filters per subscriber.
func TestPeerPumpWidensForANewcomerAndStaysPutWhenItLeaves(t *testing.T) {
	svc, _, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/app"})
	waitSubscribes(t, peer, 1)

	cancel := subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/api"})
	waitSubscribes(t, peer, 2)
	if got := peer.Filters()[1].CwdPrefix; got != "/work/ap" {
		t.Errorf("widened peer prefix = %q, want /work/ap", got)
	}

	cancel()
	settle()
	if n := len(peer.Filters()); n != 2 {
		t.Errorf("peer pumps subscribed %d times, want 2: a departure must not restart them", n)
	}
}

// A newcomer whose need is already inside what the peers stream costs nobody a
// restart.
func TestPeerPumpIsNotRestartedForACoveredSubscriber(t *testing.T) {
	svc, _, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work"})
	waitSubscribes(t, peer, 1)

	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/a"})
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/b", Sessions: []string{"x"}})
	settle()
	if n := len(peer.Filters()); n != 1 {
		t.Errorf("peer pumps subscribed %d times, want 1: both newcomers were already covered", n)
	}
}

// A subscriber that wants every directory leaves nothing for the peers to
// narrow by.
func TestPeerPumpIsWidenedToEverythingByAnUnfilteredSubscriber(t *testing.T) {
	svc, _, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/a"})
	waitSubscribes(t, peer, 1)

	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{})
	waitSubscribes(t, peer, 2)
	if got := peer.Filters()[1]; got.CwdPrefix != "" || len(got.Sessions) != 0 {
		t.Errorf("peer filter = %+v, want no restriction", got)
	}
}

// A local-scoped subscriber is never sent a peer's frames, so it must not cost
// the peer link anything.
func TestLocalScopedSubscriberDoesNotWidenThePeerPump(t *testing.T) {
	svc, _, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/a"})
	waitSubscribes(t, peer, 1)

	subscribe(t, svc, ScopeLocal, driver.SubscribeFilter{})
	settle()
	if n := len(peer.Filters()); n != 1 {
		t.Errorf("peer pumps subscribed %d times, want 1", n)
	}
}

// Once everybody has gone and the stream is torn down, the next start is as
// narrow as its subscribers need, not as wide as the widest earlier one.
func TestPeerPumpNarrowsAgainAfterTeardown(t *testing.T) {
	svc, local, peer := pushdownService(t)
	cancelA := subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/a"})
	waitSubscribes(t, peer, 1)
	cancelB := subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{})
	waitSubscribes(t, peer, 2)
	cancelA()
	cancelB()
	waitForStream(t, svc, false)

	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/c"})
	waitSubscribes(t, local, 2)
	last := func() driver.SubscribeFilter { f := peer.Filters(); return f[len(f)-1] }
	deadline := time.Now().Add(3 * time.Second)
	for last().CwdPrefix != "/work/c" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := last(); got.CwdPrefix != "/work/c" {
		t.Errorf("peer filter after a fresh start = %+v, want /work/c", got)
	}
}

// A subscriber that wants the fleet and arrives while only local-scoped ones
// hold the stream still gets the peers: they were never started, and nothing
// else would start them until the stream next restarted.
func TestFleetSubscriberStartsPeerPumpsOnARunningLocalStream(t *testing.T) {
	svc, local, peer := pushdownService(t)
	subscribe(t, svc, ScopeLocal, driver.SubscribeFilter{})
	waitSubscribes(t, local, 1)
	settle()
	if n := len(peer.Filters()); n != 0 {
		t.Fatalf("peer subscribed %d times for a local-scoped subscriber, want 0 (§13.1)", n)
	}

	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{})
	waitSubscribes(t, peer, 1)
	if n := len(local.Filters()); n != 1 {
		t.Errorf("local pumps subscribed %d times, want 1: the peers' arrival is not a reason to restart them", n)
	}
}

// A change to the set of peers restarts what it always did, both lanes (#347).
func TestPeersChangedRestartsBothLanes(t *testing.T) {
	svc, local, peer := pushdownService(t)
	subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/a"})
	waitSubscribes(t, local, 1)
	waitSubscribes(t, peer, 1)

	svc.PeersChanged()
	waitSubscribes(t, local, 2)
	waitSubscribes(t, peer, 2)
}

// A change to the set of peers drops the old peer pumps even when nobody wants
// the fleet any more, as the whole-stream restart used to: they would otherwise
// go on streaming from a peer that was just removed.
func TestPeersChangedDropsThePeerPumpsNobodyWantsAnymore(t *testing.T) {
	svc, local, peer := pushdownService(t)
	subscribe(t, svc, ScopeLocal, driver.SubscribeFilter{})
	cancelFleet := subscribe(t, svc, ScopeFleet, driver.SubscribeFilter{})
	waitSubscribes(t, local, 1)
	waitSubscribes(t, peer, 1)
	cancelFleet()

	peerOn := func() bool {
		svc.events.mu.Lock()
		defer svc.events.mu.Unlock()
		return svc.events.peerOn
	}
	if !peerOn() {
		t.Fatal("the peer lane should outlive the fleet subscriber until the next restart")
	}
	svc.PeersChanged()
	if peerOn() {
		t.Error("the peer lane survived a change to the set of peers with nobody wanting the fleet")
	}
	settle()
	if n := len(peer.Filters()); n != 1 {
		t.Errorf("peer subscribed %d times, want 1: nothing wants the fleet", n)
	}
}
