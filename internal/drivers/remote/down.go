package remote

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Peer down state (muster #237).
//
// # The defect this closes
//
// A peer that sleeps overnight stops answering, and until this state existed
// every read that reached it re-dialled a machine already known to be gone and
// waited the full bound (the dialer's own timeout, ~30s). The whole fleet read
// therefore took ~30s, and so did anything with a shorter HTTP timeout — which
// just failed. Nothing remembered that the last thousand calls had failed.
//
// # The rule
//
// After downAfterFailures CONSECUTIVE genuine transport failures the peer is
// marked down. While it is down, a call that would have dialled it fails at
// once with the same `unreachable` a timeout produces — the wire shape is
// unchanged, only the wait is gone — and the error names when the peer began
// failing. A background probe (GET /v1/health) re-asks on a doubling backoff
// and clears the state on the FIRST answer of any kind. Any answer from the
// peer clears it too, whichever path it arrived on.
//
// The backoff has the shape of internal/delivery/modclient's supervisor
// (double each miss, capped), reused rather than reinvented; the constants
// differ because a peer that wakes should be noticed within a minute, not
// treated like a crashing child.
//
// # What counts as a failure — and what deliberately does not
//
//   - A caller that hung up (context.Canceled) says nothing about the peer.
//   - A call the CALLER shortened below this driver's own bound (§4.4's
//     Fleet-Deadline-Ms) and that then missed that shorter deadline says
//     nothing either: the peer may well have been answering, just not inside
//     a budget someone else chose (#174). Counting those would let one
//     impatient client mark a healthy, slow peer down for everyone.
//   - Any HTTP response, whatever its status, is an answer: the peer is up.
//
// # Callers that never wait the full bound (#302)
//
// Those two exclusions are each right and, together, left a hole: a peer that
// went dark was marked down only after K callers each waited out the whole
// bound, so a fleet whose callers all time out sooner (a watch script, a CLI
// at 15s, a dashboard at 3s) never marked it down and every read stayed slow
// for as long as the peer stayed dark.
//
// A miss that is not evidence — a hang-up, or a caller-shortened deadline —
// therefore starts the driver's OWN check: one GET /v1/health under
// downProbeTimeout, at most one in flight per peer, and ITS outcome counts as
// a genuine failure (or as an answer, which clears the streak). The verdict
// rests on a budget this driver chose, not on the impatient caller's, so the
// #174 guarantee holds: a slow peer that answers inside downProbeTimeout is
// never marked down, whoever is timing out against it.

// # What it costs, said plainly
//
// While a peer is marked down, calls to it — mutations included — fail fast
// instead of being attempted. A false positive therefore blocks a send for at
// most downBackoffMax. It is the right trade for a peer that stops answering
// for hours, and the failure it produces is the honest, retryable
// `unreachable` a timeout would have produced anyway, only sooner and without
// the ambiguity of a request that might have been half-delivered.
const (
	// downAfterFailures is K: consecutive genuine transport failures before
	// the peer is marked down. Three, because one is a blip and two can be a
	// restart racing a probe; three in a row on calls that each waited out a
	// bound is a peer that is not there.
	downAfterFailures = 3
	// downBackoffMin/Max bound the delay between background probes. The first
	// probe follows quickly (a restarting peer is back within seconds), then
	// the gap doubles up to a ceiling so a peer asleep all night costs one
	// small request per downBackoffMax.
	downBackoffMin = 2 * time.Second
	downBackoffMax = 30 * time.Second
	// downProbeTimeout caps one probe. It is bounded by the driver's own
	// floor when that is shorter.
	downProbeTimeout = 5 * time.Second
)

// downState is the driver's memory of how its peer has been answering.
type downState struct {
	mu sync.Mutex
	// fails counts consecutive genuine transport failures; failingSince is
	// when the first of them began.
	fails        int
	failingSince time.Time
	// down is set when fails reaches the threshold and cleared by any answer.
	down      bool
	nextProbe time.Time
	probing   bool
	// checking is set while a miss-triggered check (probeAfterMiss) is in
	// flight: the single-flight guard, separate from probing, which belongs to
	// the loop that runs only once the peer is already down.
	checking bool
	// closed is set by Shutdown so no new check starts after it.
	closed   bool
	stop     chan struct{}
	stopOnce sync.Once
}

// downPolicy is the tunable side of the state, so a test can shrink the clock
// without waiting out real backoffs. The defaults are the constants above.
type downPolicy struct {
	after      int
	backoffMin time.Duration
	backoffMax time.Duration
}

func defaultDownPolicy() downPolicy {
	return downPolicy{after: downAfterFailures, backoffMin: downBackoffMin, backoffMax: downBackoffMax}
}

// WithDownPolicy overrides when a peer is marked down and how often it is
// re-probed: after consecutive failures, then probes spaced from backoffMin
// doubling to backoffMax. Zero or negative values keep the defaults.
func WithDownPolicy(after int, backoffMin, backoffMax time.Duration) Option {
	return func(d *Driver) {
		if after > 0 {
			d.downPol.after = after
		}
		if backoffMin > 0 {
			d.downPol.backoffMin = backoffMin
		}
		if backoffMax > 0 {
			d.downPol.backoffMax = backoffMax
		}
		if d.downPol.backoffMax < d.downPol.backoffMin {
			d.downPol.backoffMax = d.downPol.backoffMin
		}
	}
}

// PeerDown implements driver.PeerDownReporter: from memory, never from the
// network.
func (d *Driver) PeerDown() (down bool, since time.Time, failures int) {
	d.down.mu.Lock()
	defer d.down.mu.Unlock()
	if !d.down.down {
		return false, time.Time{}, d.down.fails
	}
	return true, d.down.failingSince, d.down.fails
}

var _ driver.PeerDownReporter = (*Driver)(nil)

// Shutdown stops the background probe, if one is running. The daemon never
// calls it (a peer driver lives as long as the process); it exists so a test
// that marks a peer down does not leave a goroutine dialling after it ends.
func (d *Driver) Shutdown() {
	d.down.stopOnce.Do(func() {
		d.down.mu.Lock()
		d.down.closed = true
		if d.down.stop == nil {
			d.down.stop = make(chan struct{})
		}
		close(d.down.stop)
		d.down.mu.Unlock()
	})
}

// admit is the gate every outgoing call passes first. It returns nil unless
// the peer is marked down, in which case it returns the fast `unreachable`
// that stands in for the timeout the call would otherwise have waited out.
func (d *Driver) admit() *fleet.Error {
	d.down.mu.Lock()
	defer d.down.mu.Unlock()
	if !d.down.down {
		return nil
	}
	next := time.Until(d.down.nextProbe).Round(time.Second)
	if next < 0 {
		next = 0
	}
	return &fleet.Error{
		Kind: fleet.ErrorUnreachable,
		Message: fmt.Sprintf("%s is marked down: no answer since %s (%d consecutive failures); "+
			"not dialled — re-probing in the background, next in ~%s",
			d.machine, d.down.failingSince.UTC().Format(time.RFC3339), d.down.fails, next),
		Machine:   d.machine,
		Retryable: true,
	}
}

// countsAsPeerFailure decides whether a failed call is evidence about the
// PEER — see the rules above. budget is the deadline the call actually ran
// under ("none" callers pass 0 and are treated as unbounded by this driver's
// own bound).
func (d *Driver) countsAsPeerFailure(deadlineKind bool, budget time.Duration) bool {
	if !deadlineKind {
		return true // a dial/transport error from the network itself
	}
	// A deadline that fired: only this driver's own bound proves anything
	// about the peer. A shorter, caller-chosen budget does not.
	bound := d.effectiveDeadline()
	return budget <= 0 || budget >= bound-bound/10
}

// noteFailure records one genuine transport failure and, on the K-th
// consecutive one, marks the peer down and starts the background probe.
// req is kept for the probe: with an identity the probe presents it, in
// shared-token mode it presents the credential of the call that tripped it —
// the same authority the failing call was already using.
func (d *Driver) noteFailure(req fleet.Request, started time.Time) {
	d.down.mu.Lock()
	if d.down.fails == 0 {
		d.down.failingSince = started
	}
	d.down.fails++
	trip := !d.down.down && d.down.fails >= d.downPol.after
	if trip {
		d.down.down = true
		d.down.nextProbe = d.now().Add(d.downPol.backoffMin)
	}
	fails, since := d.down.fails, d.down.failingSince
	start := trip && !d.down.probing
	if start {
		d.down.probing = true
		if d.down.stop == nil {
			d.down.stop = make(chan struct{})
		}
	}
	stop := d.down.stop
	d.down.mu.Unlock()

	if trip {
		// One line per transition, never per call: the incident that motivated
		// this state logged ~30,000 lines for a single sleeping peer.
		log.Printf("remote: peer %s marked down after %d consecutive failures (failing since %s); "+
			"calls to it fail fast and it is re-probed in the background",
			d.machine, fails, since.UTC().Format(time.RFC3339))
	}
	if start {
		go d.probeLoop(req, stop)
	}
}

// noteReached records that the peer answered — any HTTP response — and clears
// the down state. It reports whether the peer had been marked down.
func (d *Driver) noteReached() bool {
	d.down.mu.Lock()
	if d.down.fails == 0 && !d.down.down {
		d.down.mu.Unlock()
		return false
	}
	wasDown, since := d.down.down, d.down.failingSince
	d.down.fails, d.down.down = 0, false
	d.down.failingSince = time.Time{}
	d.down.mu.Unlock()
	if wasDown {
		log.Printf("remote: peer %s answered again after being down since %s",
			d.machine, since.UTC().Format(time.RFC3339))
	}
	return wasDown
}

// probeLoop re-asks the peer until it answers, doubling the gap between asks.
// It talks to the peer directly rather than through do(): do() is gated by
// admit, and the probe is the one caller that must get through.
func (d *Driver) probeLoop(req fleet.Request, stop <-chan struct{}) {
	wait := d.downPol.backoffMin
	for {
		timer := time.NewTimer(wait)
		select {
		case <-stop:
			timer.Stop()
			d.endProbe(true)
			return
		case <-timer.C:
		}
		if d.probeOnce(req) {
			// Refresh what the peer says about itself off the back of the
			// answer, as any ordinary successful contact does.
			d.noteSuccessfulContact(req)
		}
		// Exit only when the peer is no longer down, decided under the same
		// lock that clears `probing`: a peer that re-trips between an answer
		// and this check must keep its probe, or it would stay marked down
		// with nothing left to ever clear it.
		if d.endProbe(false) {
			return
		}
		wait = min(wait*2, d.downPol.backoffMax)
		d.down.mu.Lock()
		d.down.nextProbe = d.now().Add(wait)
		d.down.mu.Unlock()
	}
}

// endProbe retires the probe goroutine if it should end — always when forced,
// otherwise only once the peer is no longer marked down. It reports whether
// the goroutine should return.
func (d *Driver) endProbe(force bool) bool {
	d.down.mu.Lock()
	defer d.down.mu.Unlock()
	if force || !d.down.down {
		d.down.probing = false
		return true
	}
	return false
}

// probeOnce asks GET /v1/health and reports whether the peer answered at all.
// A 401 is an answer: the peer is up and refusing this credential, which is
// a different problem from a peer that is not there. Marks the peer reachable
// on any answer.
func (d *Driver) probeOnce(req fleet.Request) bool {
	answered, _ := d.askHealth(req)
	if answered {
		d.noteReached()
	}
	return answered
}

// askHealth is the one place a probe talks to the peer: GET /v1/health under
// this driver's own budget, never a caller's. answered reports whether any
// HTTP response came back; asked is false when no request could be made at
// all (no credential to present), which says nothing about the peer either
// way and must not be counted as a miss.
func (d *Driver) askHealth(req fleet.Request) (answered, asked bool) {
	token, behalf, ok := d.bearerFor(req)
	if !ok {
		return false, false
	}
	timeout := downProbeTimeout
	if d.deadline > 0 && d.deadline < timeout {
		timeout = d.deadline
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, d.current()+"/v1/health", nil)
	if err != nil {
		return false, false
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if behalf != "" {
		httpReq.Header.Set("Fleet-On-Behalf-Of", behalf)
	}
	resp, err := d.roundTrip(httpReq, "/v1/health")
	if err != nil {
		return false, true
	}
	_ = resp.Body.Close()
	return true, true
}

// probeAfterMiss is called for a failed call that is NOT evidence about the
// peer — a caller that hung up, or one that shortened its own deadline and
// missed it. Such a miss cannot count itself (#174), but it is a reason to
// ask: it starts one check on the driver's own budget and counts that
// outcome. At most one check is in flight per peer, so a burst of impatient
// callers costs the peer one small request at a time, not one per caller.
//
// It does nothing for a peer already marked down (the background probe is
// already asking) or after Shutdown.
func (d *Driver) probeAfterMiss(req fleet.Request) {
	d.down.mu.Lock()
	if d.down.down || d.down.checking || d.down.closed {
		d.down.mu.Unlock()
		return
	}
	d.down.checking = true
	d.down.mu.Unlock()
	go d.checkPeer(req)
}

// checkPeer runs one miss-triggered check and records its outcome: an answer
// of any kind clears the streak, a miss is one genuine failure of the K.
func (d *Driver) checkPeer(req fleet.Request) {
	started := d.now()
	answered, asked := d.askHealth(req)
	d.down.mu.Lock()
	closed := d.down.closed
	d.down.mu.Unlock()
	switch {
	case !asked || closed:
		// Nothing was asked, or the driver is shutting down: no evidence.
	case answered:
		d.noteReached()
	default:
		d.noteFailure(req, started)
	}
	d.down.mu.Lock()
	d.down.checking = false
	d.down.mu.Unlock()
}
