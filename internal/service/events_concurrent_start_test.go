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

// lifeDriver is a recordingDriver that also keeps the context of every
// subscription it was given, so a test can tell a lane that was stopped from
// one whose cancel function nobody holds any more.
type lifeDriver struct {
	recordingDriver

	cmu  sync.Mutex
	ctxs []context.Context
}

func newLifeDriver() *lifeDriver {
	return &lifeDriver{recordingDriver: recordingDriver{Driver: stub.Driver{DeadlineMs: 200}}}
}

func (d *lifeDriver) Subscribe(ctx context.Context, r fleet.Request, f driver.SubscribeFilter) (driver.EventStream, error) {
	d.cmu.Lock()
	d.ctxs = append(d.ctxs, ctx)
	d.cmu.Unlock()
	return d.recordingDriver.Subscribe(ctx, r, f)
}

func (d *lifeDriver) contexts() []context.Context {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	return append([]context.Context(nil), d.ctxs...)
}

// #375: the decision to start a lane and the record of the lane it started were
// two separate critical sections, so subscribers arriving together on a cold
// service each read "not running" and each started the pumps. The second one
// also overwrote the first's cancel function, leaving a lane nothing could stop.
//
// The race is a few instructions wide, so one round proves little: it is run in
// fresh services, with the subscribers released from a single gate.
func TestSubscribersArrivingTogetherStartOneLanePerDriver(t *testing.T) {
	const rounds, subscribers = 40, 12

	for round := 0; round < rounds; round++ {
		svc := New("testbox")
		svc.events.linger = 0
		local, peer := newLifeDriver(), newLifeDriver()
		if err := svc.RegisterLocalDriver("fake", local); err != nil {
			t.Fatalf("RegisterLocalDriver: %v", err)
		}
		if err := svc.RegisterPeerDriver("peerbox", peer); err != nil {
			t.Fatalf("RegisterPeerDriver: %v", err)
		}
		svc.SetPeerCredential("peer-token")

		gate := make(chan struct{})
		cancels := make([]func(), subscribers)
		var wg sync.WaitGroup
		for i := range cancels {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				_, _, _, cancels[i] = svc.Events(context.Background(), ScopeFleet, driver.SubscribeFilter{}, 0, "")
			}()
		}
		close(gate)
		wg.Wait()

		waitSubscribes(t, &local.recordingDriver, 1)
		waitSubscribes(t, &peer.recordingDriver, 1)
		time.Sleep(20 * time.Millisecond) // let a duplicate lane, if one was started, subscribe too
		if n := len(local.Filters()); n != 1 {
			t.Fatalf("round %d: local driver subscribed %d times, want 1", round, n)
		}
		if n := len(peer.Filters()); n != 1 {
			t.Fatalf("round %d: peer driver subscribed %d times, want 1", round, n)
		}

		// Everybody leaving must stop every lane that was started — which a lane
		// whose cancel was overwritten cannot.
		for _, cancel := range cancels {
			wg.Add(1)
			go func() { defer wg.Done(); cancel() }()
		}
		wg.Wait()
		for name, d := range map[string]*lifeDriver{"local": local, "peer": peer} {
			for i, ctx := range d.contexts() {
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
					t.Fatalf("round %d: %s lane %d was never stopped after every subscriber left", round, name, i)
				}
			}
		}
	}
}
