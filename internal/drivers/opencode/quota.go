package opencode

import (
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// A sign-in the provider keeps refusing (muster #458).
//
// Every session on the subscription shares one plan and its usage windows, so when
// the window runs out it runs out for all of them at once and they recover together
// when it resets. Measured on the real runtime (docs/adr/458-*), that does NOT arrive
// as a failed turn: a usage-limit answer (HTTP 429) is retried with a growing delay and never recorded as a failure, and the
// session sits in the runtime's `retry` status with the provider's own message, an
// attempt count that keeps growing and the time of the next try. Seen per session it
// is a turn that is slow to finish; seen across the machine it is one state of one
// account, and this file is where it is said once.
//
// # What the signal is, and is not
//
// A subscription session in `retry` at attempt limitAttempt or later has been refused
// repeatedly, which is not a blip. That is all the runtime states: it does not say
// WHY, and this driver does not read the cause out of the message (the discipline
// every classification here keeps). An exhausted plan window is the case this exists
// for; any other persistent refusal of the sign-in reads the same, and the provider's
// message, passed through unparsed, says which. Calibrating against a real hit is a
// row on #273.
//
// # Where it is kept
//
// The live view (which sessions, since when) is in memory and answers GET /v1/runtimes.
// The offline doctor cannot ask a service, so the episode is also written to the grant
// store, through the one party allowed to write it (the refresher): on the first
// refused session, again as a heartbeat while any remain, and cleared when none do. A
// start clears a mark an earlier process left, because nothing here can say it is still
// true; the retry loop shows it again within seconds if it is.

const (
	// limitAttempt is the retry attempt from which a session counts as refused.
	limitAttempt = 3
	// limitHeartbeat is how often a still-true episode is written again.
	limitHeartbeat = time.Minute
	// limitStale is how long an observation holds an episode open without being seen
	// again. A refused session is read on every listing and event, so one nobody has
	// seen for this long is gone (its server died) and must not keep the grant limited.
	limitStale = 10 * time.Minute
	// limitMessageMax bounds the provider's message as it is kept and passed on.
	limitMessageMax = 200
)

// LimitSink is where the driver records the episode. *grant.Refresher is one. It is
// optional: a GrantSource that is not a LimitSink (a test fake, a future source) is
// simply not told, and the live view still answers.
type LimitSink interface {
	NoteLimit(since time.Time, message string) error
	ClearLimit() error
}

// limitWatch is the live view of the sessions on the subscription that are refused.
type limitWatch struct {
	mu       sync.Mutex
	refused  map[string]refusal // session id -> its last sighting
	since    time.Time          // start of the episode; zero when there is none
	seen     time.Time          // the last observation that kept it true
	message  string
	wrote    time.Time // when the sink was last told
	sinkLock sync.Mutex
}

// refusal is one session's last sighting in the retry loop.
type refusal struct {
	message string
	at      time.Time
}

// limitAction is what a transition asks of the sink, decided under the lock and
// done after it is released.
type limitAction struct {
	note  bool
	clear bool
	since time.Time
	msg   string
}

// observeStatus is told what the runtime reported for one session: present with its
// status, or absent from the status map (idle). Only a session on the subscription
// counts.
func (d *Driver) observeStatus(srv *server, id string, present bool, st wireStatus) {
	if srv == nil || srv.credential != fleet.CredentialSubscription {
		return
	}
	now := d.now()
	refused := present && st.Type == "retry" && st.Attempt >= limitAttempt
	d.limit.mu.Lock()
	if refused {
		if d.limit.refused == nil {
			d.limit.refused = map[string]refusal{}
		}
		d.limit.refused[id] = refusal{message: cutMessage(st.Message), at: now}
		d.limit.message = d.limit.refused[id].message
	} else {
		delete(d.limit.refused, id)
	}
	act := d.limit.decide(now)
	d.limit.mu.Unlock()
	d.runLimitAction(act)
}

// forgetRefused drops a session that is gone, so a closed session cannot hold an
// episode open.
func (d *Driver) forgetRefused(id string) {
	d.limit.mu.Lock()
	_, was := d.limit.refused[id]
	delete(d.limit.refused, id)
	act := limitAction{}
	if was {
		act = d.limit.decide(d.now())
	}
	d.limit.mu.Unlock()
	d.runLimitAction(act)
}

// decide moves the episode on and says what the sink must be told. Called with mu held.
func (w *limitWatch) decide(now time.Time) limitAction {
	w.prune(now)
	if len(w.refused) == 0 {
		if w.since.IsZero() {
			return limitAction{}
		}
		w.since, w.seen, w.wrote = time.Time{}, time.Time{}, time.Time{}
		return limitAction{clear: true}
	}
	w.seen = now
	if w.since.IsZero() {
		w.since = now
	}
	if w.wrote.IsZero() || now.Sub(w.wrote) >= limitHeartbeat {
		w.wrote = now
		return limitAction{note: true, since: w.since, msg: w.message}
	}
	return limitAction{}
}

// prune drops sightings nobody has repeated for limitStale. Called with mu held.
func (w *limitWatch) prune(now time.Time) {
	for id, r := range w.refused {
		if now.Sub(r.at) > limitStale {
			delete(w.refused, id)
		}
	}
}

// runLimitAction tells the sink, one call at a time. A failed write is said once
// per episode edge by the caller's logger and never fails a read.
func (d *Driver) runLimitAction(a limitAction) {
	if !a.note && !a.clear {
		return
	}
	sink, ok := d.grants.(LimitSink)
	if !ok {
		return
	}
	d.limit.sinkLock.Lock()
	defer d.limit.sinkLock.Unlock()
	var err error
	if a.note {
		err = sink.NoteLimit(a.since, a.msg)
	} else {
		err = sink.ClearLimit()
	}
	if err != nil {
		d.say("opencode: could not record the openai sign-in's usage state: %v", err)
	}
}

// clearStaleLimit removes a mark an earlier process left: nothing here can say it is
// still true, and the retry loop shows it again within seconds if it is.
func (d *Driver) clearStaleLimit() {
	if sink, ok := d.grants.(LimitSink); ok {
		_ = sink.ClearLimit()
	}
}

// liveQuota is the quota state as this process sees it now, nil when no session on
// the subscription is refused.
func (d *Driver) liveQuota() *fleet.GrantQuota {
	d.limit.mu.Lock()
	defer d.limit.mu.Unlock()
	d.limit.prune(d.now())
	n := len(d.limit.refused)
	if n == 0 || d.limit.since.IsZero() {
		return nil
	}
	return &fleet.GrantQuota{
		State:      fleet.QuotaLimited,
		Since:      d.limit.since.UTC().Truncate(time.Second),
		ObservedAt: d.limit.seen.UTC().Truncate(time.Second),
		Sessions:   &n,
		Message:    d.limit.message,
	}
}

// cutMessage keeps the provider's message to one short line.
func cutMessage(m string) string {
	m = strings.Join(strings.Fields(m), " ")
	if r := []rune(m); len(r) > limitMessageMax {
		m = string(r[:limitMessageMax-1]) + "…"
	}
	return m
}
