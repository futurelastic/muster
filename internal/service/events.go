package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// The event plane: §7.3's cursor and epoch, §13's multiplexing, and the
// retention that makes a gap announceable instead of silent.
//
// # Who assigns what
//
// A driver produces events and leaves Cursor and Epoch unset, because §7.3
// assigns them per *service instance* and two drivers under one service must
// not mint competing sequences. The hub stamps them on the way through. That
// split is why a driver's event is not directly deliverable and why this type
// exists at all.
//
// # Retention exists so a gap can be announced
//
// A subscriber reconnects with the last cursor it saw. If that cursor is older
// than what is retained, or the epoch differs because this service restarted,
// the subscriber is told `resync_required` rather than quietly resumed from
// the oldest event still held. §7.3 is explicit about why: silently resuming
// produces a subscriber that believes it has a complete history and does not.
// Announced gaps are recoverable; silent ones are not.
//
// # Watching costs what the subscribers asked for
//
// The hub keeps ONE driver subscription per driver, not one per client — two
// streams over the same driver would produce two events for one change, with
// different cursors, and deliver both to everyone. The subscription has two
// lanes, because the two kinds of driver cannot be asked the same question: the
// local lane (this machine's drivers) and the peer lane (one stream to each
// peer).
//
// Each lane's filter is the union of the active clients' filters, recomputed
// whenever a client arrives or leaves. That is D4's lesson applied one level
// up: if every client names the sessions it cares about, the union names them
// too and a driver attaches one connection per named session.
//
// The local lane widens to everything as soon as one client names no sessions:
// a local driver applies Matches itself, to frames whose directory it may not
// know, and a prefix pushed down to it could drop one the hub would have
// delivered (#361). The peer lane is computed per dimension instead, over the
// clients that can receive a peer's frames at all (scope=fleet): the sessions
// when every one of them names some, the longest common directory prefix when
// every one of them has one. A peer serves the filter through its own hub, which
// delivers a frame whose directory it cannot place, so the prefix is safe to send
// and the peer's whole fleet is not streamed here just to be discarded (#372).
// Either way a lane's filter only has to cover every client; the hub's
// per-subscriber filtering stays the exact answer.

const defaultRetention = 512

// defaultLinger is how long the driver-side stream stays up after the last
// subscriber leaves.
//
// # Why an idle stream is worth paying for
//
// Tearing down on the last departure looks like the frugal choice, and for a
// resident SSE subscriber it never fires. For the long-poll transport it fires
// on EVERY cycle: a poll returns, its subscriber is released, the stream is
// cancelled, and the next poll re-opens it — whereupon the driver takes a fresh
// baseline reading and absorbs everything that changed in between. That is not
// a slow feed, it is a silent hole per cycle, and nothing anywhere can notice
// it afterwards.
//
// The window is a compromise with §5.5's "an idle fleet costs nothing": one
// minute is long enough to bridge a poll cycle and a client restart, short
// enough that a machine nobody is watching stops holding connections to a
// multiplexer other tools share.
const defaultLinger = 60 * time.Second

// pumpBackoff bounds how fast a failed driver subscription is retried, and how
// slowly it settles. The first retry is quick because the common failure is a
// machine that was momentarily empty; the ceiling exists because the other
// common failure is a peer that is down for the afternoon.
const (
	pumpBackoffMin = 500 * time.Millisecond
	pumpBackoffMax = 30 * time.Second
)

// hub multiplexes driver events, stamps them per §7.3, retains a window, and
// fans out to subscribers.
type hub struct {
	epoch     string
	self      fleet.MachineId
	retention int

	mu     sync.Mutex
	cursor int64
	ring   []fleet.Event
	// ringFacts runs parallel to ring: what each retained event was about, as
	// resolved when it was published. A backlog replay filters on these, not
	// on a fresh lookup, so a session that closed since (and left the index)
	// is still replayed to a cwdPrefix subscriber exactly as it was delivered
	// live.
	ringFacts []eventFact
	subs      map[int]*subscriber
	nextID    int

	// cwds is the one place a state, close or labels frame can learn which
	// directory its session runs in: those payloads name the session and
	// nothing else. See cwdindex.go.
	cwds     map[sessionKey]string
	cwdTried map[sessionKey]time.Time

	// persist records the cursor high-water mark so a restart continues the
	// sequence rather than reusing numbers (§7.3). Called under the lock and
	// therefore kept cheap; nil when running without durable state.
	persist func(int64)

	// driver-side stream, kept alive while somebody is listening and for
	// linger after the last of them leaves. The local lane's state is the
	// stream* fields; the peer lane has its own so a change that only concerns
	// one lane restarts only that one (see ensureStreamForce).
	streamCancel context.CancelFunc
	streamFilter driver.SubscribeFilter
	streamOn     bool
	peerCancel   context.CancelFunc
	peerFilter   driver.SubscribeFilter
	peerOn       bool
	linger       time.Duration
	lingerTimer  *time.Timer

	// ensure serialises the whole of ensureStreamForce: reading what the lanes
	// are doing, deciding, and recording the lanes it starts. mu alone cannot do
	// that — it is released between the read and the record, which is how two
	// subscribers arriving together each saw a cold stream and each started one,
	// the second overwriting the first's cancel (#375). Taken before mu, never
	// after, and never held across anything but short bookkeeping: the pumps it
	// starts run under `go`.
	ensure sync.Mutex

	// rename is muster #103's own bookkeeping — see
	// rename_corroboration.go. A separate lock from mu above, deliberately:
	// resolving a pending rename recurses into publish (to announce the
	// follow-up), and mu is not reentrant.
	rename renamePlane
}

type subscriber struct {
	id     int
	ch     chan fleet.Event
	filter driver.SubscribeFilter
	// scope decides whether this subscriber's existence justifies streaming
	// from peers. A peer asking us for scope=local must NOT cause us to open
	// streams to our own peers — that is §13.1 in the event plane, and
	// violating it makes two mutually-configured machines each hold an open
	// stream to the other forever.
	scope Scope
	// dropped records that this subscriber fell behind. Its next read gets
	// a resync rather than a hole it cannot detect.
	dropped bool
}

func newHub(self fleet.MachineId, epoch string) *hub {
	return &hub{
		epoch:     epoch,
		self:      self,
		retention: defaultRetention,
		linger:    defaultLinger,
		subs:      map[int]*subscriber{},
		cwds:      map[sessionKey]string{},
		cwdTried:  map[sessionKey]time.Time{},
		rename:    newRenamePlane(),
	}
}

// publish stamps an event and fans it out. Called by whatever is pumping a
// driver's stream.
func (h *hub) publish(ev fleet.Event) {
	h.mu.Lock()

	h.cursor++
	ev.Cursor = h.cursor
	ev.Epoch = h.epoch
	if h.persist != nil {
		h.persist(h.cursor)
	}

	fact := h.factFor(ev)
	h.ring = append(h.ring, ev)
	h.ringFacts = append(h.ringFacts, fact)
	if len(h.ring) > h.retention {
		h.ring = h.ring[len(h.ring)-h.retention:]
		h.ringFacts = h.ringFacts[len(h.ringFacts)-h.retention:]
	}

	for _, s := range h.subs {
		// A local-scoped subscriber must not receive a peer's events —
		// otherwise "scope=local" would be a description of what this
		// service asks for rather than of what it answers with, and a
		// proxied subscription would carry the fleet back to the peer.
		if s.scope == ScopeLocal && ev.Machine != "" && ev.Machine != h.self {
			continue
		}
		if !matchesFact(s.filter, ev, fact) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			// Full. §7a listed backpressure as open; this is the answer
			// the rest of the design forces: never drop silently. The
			// subscriber is marked, and its stream ends with a resync so
			// it refetches rather than continuing with a hole it has no
			// way to notice.
			s.dropped = true
		}
	}

	// After the fan-out, never before: the close frame itself must still be
	// matched against the cwd this session is leaving the index with.
	h.forget(ev)
	h.mu.Unlock()

	// Deliberately unlocked above rather than deferred: muster #103's
	// corroboration bookkeeping lives under its own lock (h.rename, never
	// h.mu — see renamePlane's doc), and a rename that just resolved
	// recurses into publish to announce the follow-up. h.mu is not
	// reentrant, so that recursion must happen with it already released.
	for _, followUp := range h.observeRenameCorroboration(ev) {
		h.publish(followUp)
	}
}

// add registers a subscriber resuming from (cursor, epoch). It returns the
// backlog to replay and whether the subscriber must resync first.
func (h *hub) add(scope Scope, filter driver.SubscribeFilter, fromCursor int64, fromEpoch string) (*subscriber, []fleet.Event, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	s := &subscriber{id: h.nextID, ch: make(chan fleet.Event, 64), filter: filter, scope: scope}
	h.nextID++
	h.subs[s.id] = s

	// A fresh subscriber (no cursor) starts from now and needs no resync:
	// it is not claiming to have history.
	if fromCursor == 0 && fromEpoch == "" {
		return s, nil, false
	}
	// Different instance: every cursor it holds refers to another sequence.
	if fromEpoch != h.epoch {
		return s, nil, true
	}
	// A cursor we have not stamped. The epoch matches, so the caller is not
	// confused about which sequence it means — it is simply ahead of ours, and
	// this service cannot supply what it has not assigned. Announced, like
	// every other gap, rather than answered with a wait that would never end.
	if fromCursor > h.cursor {
		return s, nil, true
	}
	var backlog []fleet.Event
	oldest := int64(0)
	if len(h.ring) > 0 {
		oldest = h.ring[0].Cursor
	}
	if fromCursor < oldest-1 {
		return s, nil, true // fell off the retained window
	}
	for i, ev := range h.ring {
		if ev.Cursor > fromCursor && matchesFact(filter, ev, h.ringFacts[i]) {
			backlog = append(backlog, ev)
		}
	}
	return s, backlog, false
}

func (h *hub) remove(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(s.ch)
	}
}

// currentCursor reports the high-water mark this instance has stamped.
func (h *hub) currentCursor() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cursor
}

func (h *hub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// streamLive reports whether a driver-side subscription is currently running —
// including through a linger window, where nothing is listening but everything
// is still being observed.
//
// It is read by the snapshot endpoint, which may only publish feed coordinates
// when they are a usable resume point. While no stream runs, this service's
// cursor is frozen: the world keeps changing and the sequence does not advance,
// so a cursor handed out then looks resumable and is not. Absent is the honest
// answer there (§5.7), and it is a very different one from zero.
func (h *hub) streamLive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streamOn
}

// armLinger schedules the driver-side stream's teardown rather than performing
// it, so a subscriber arriving in the meantime keeps one continuous
// subscription. See defaultLinger for what a per-cycle teardown costs.
func (h *hub) armLinger() {
	h.mu.Lock()
	if h.linger > 0 {
		if h.lingerTimer != nil {
			h.lingerTimer.Stop()
		}
		h.lingerTimer = time.AfterFunc(h.linger, h.lingerExpired)
		h.mu.Unlock()
		return
	}
	// Linger disabled: tear down on the spot, which is what this did before
	// the window existed. Tests set it to zero to keep that behaviour
	// observable rather than having to wait a minute for it.
	cancels := h.stopLanesLocked()
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// stopLanesLocked marks both lanes stopped and returns their cancels for the
// caller to run once it has released h.mu. A lane that was never started
// contributes none.
func (h *hub) stopLanesLocked() []context.CancelFunc {
	var cancels []context.CancelFunc
	if h.streamCancel != nil {
		cancels = append(cancels, h.streamCancel)
	}
	if h.peerCancel != nil {
		cancels = append(cancels, h.peerCancel)
	}
	h.streamOn, h.streamCancel = false, nil
	h.peerOn, h.peerCancel = false, nil
	return cancels
}

func (h *hub) lingerExpired() {
	h.mu.Lock()
	h.lingerTimer = nil
	if len(h.subs) > 0 {
		// Somebody came back between the timer firing and this lock. Keeping
		// the stream is the whole point of the window.
		h.mu.Unlock()
		return
	}
	cancels := h.stopLanesLocked()
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// disarmLinger cancels a pending teardown because somebody is listening again.
func (h *hub) disarmLinger() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lingerTimer != nil {
		h.lingerTimer.Stop()
		h.lingerTimer = nil
	}
}

// wantsPeers reports whether any subscriber asked for a fleet-wide view. See
// subscriber.scope for why this gates peer streams rather than always running
// them.
func (h *hub) wantsPeers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if s.scope == ScopeFleet {
			return true
		}
	}
	return false
}

// streamFilters computes what each lane of the driver-side stream must watch
// to satisfy everyone who can receive from it.
//
// local is the sessions' union and nothing else: if every subscriber names
// sessions the drivers watch exactly those, and one that asks descriptively —
// a directory prefix, or nothing — widens it to everything (see the file
// comment for why a prefix is not pushed to a local driver).
//
// peer is computed over the fleet-scoped subscribers only, since a
// local-scoped one is never sent a peer's frames, and per dimension: Sessions
// is the union of the named ids when every one of them names some, CwdPrefix is
// the longest common prefix when every one of them has one. A dimension some
// subscriber leaves open stays open. Each dimension over-approximates every
// member, so the hub's per-subscriber filtering is still what decides.
func (h *hub) streamFilters() (local, peer driver.SubscribeFilter) {
	h.mu.Lock()
	defer h.mu.Unlock()

	local.Sessions = unionSessions(h.subs, func(*subscriber) bool { return true })
	fleetSubs := func(s *subscriber) bool { return s.scope == ScopeFleet }
	peer.Sessions = unionSessions(h.subs, fleetSubs)
	peer.CwdPrefix = commonCwdPrefix(h.subs, fleetSubs)
	return local, peer
}

// unionSessions is the union of the sessions the selected subscribers name, or
// nil when any of them names none (or none is selected).
func unionSessions(subs map[int]*subscriber, selected func(*subscriber) bool) []string {
	seen := map[string]bool{}
	var ids []string
	for _, s := range subs {
		if !selected(s) {
			continue
		}
		if len(s.filter.Sessions) == 0 {
			return nil // somebody wants every session
		}
		for _, id := range s.filter.Sessions {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// commonCwdPrefix is the longest string prefix shared by the prefixes of the
// selected subscribers, or "" when any of them has none (or none is selected).
// It is a string prefix, not a path one, because that is what Matches tests.
func commonCwdPrefix(subs map[int]*subscriber, selected func(*subscriber) bool) string {
	common, any := "", false
	for _, s := range subs {
		if !selected(s) {
			continue
		}
		p := s.filter.CwdPrefix
		if p == "" {
			return "" // somebody wants every directory
		}
		if !any {
			common, any = p, true
			continue
		}
		n := 0
		for n < len(common) && n < len(p) && common[n] == p[n] {
			n++
		}
		common = common[:n]
	}
	return common
}

// matchesEvent applies a subscriber's filter to an event on what its payload
// alone says. The hub itself uses matchesFact with the cwd it resolved when the
// event was published (cwdindex.go); this is the same rule without that index.
func matchesEvent(f driver.SubscribeFilter, ev fleet.Event) bool {
	id, cwd, has := eventSubject(ev)
	return matchesFact(f, ev, eventFact{id: id, cwd: cwd, cwdKnown: has})
}

// matchesFact applies a subscriber's filter to an event about the session
// fact describes.
func matchesFact(f driver.SubscribeFilter, ev fleet.Event, fact eventFact) bool {
	// Control events are never filtered out by session: a subscriber that
	// filtered away its own resync notice would be told nothing precisely
	// when it most needs telling.
	if ev.Kind == fleet.EventControlResync || ev.Kind == fleet.EventSourceStatus {
		return true
	}
	if fact.id == "" {
		return true // not about one session: machine.*, session.renamed
	}
	if fact.cwdKnown {
		return f.Matches(fact.id, fact.cwd)
	}
	// The session's directory is not known. A cwdPrefix cannot be applied to
	// it, and dropping the frame would be the silent hole §7.3 forbids: a
	// subscriber that never sees its session go idle has no way to tell that
	// from the session staying busy. An extra frame is recoverable — the
	// client can ignore an id it does not recognise. So the id test alone
	// decides. (The service asks the source for the directory before it
	// publishes — resolveCwd — so this is the residue, not the rule.)
	f.CwdPrefix = ""
	return f.Matches(fact.id, "")
}

// eventTarget reports which session an event is about and the directory its
// payload carries, if any.
func eventTarget(ev fleet.Event) (id, cwd string) {
	id, cwd, _ = eventSubject(ev)
	return id, cwd
}

// eventSubject reads the session an event is about out of its payload, and the
// session's directory when the payload carries one — only a whole session does.
//
// A frame relayed from a peer has been through JSON, and its Payload is the
// generic map that decoding into `any` produces, not the Go type the peer's
// driver published. It has to be read as such: an id of "" admits the frame to
// every subscriber, which is how cwdPrefix came to pass a peer's entire fleet
// through (#361).
func eventSubject(ev fleet.Event) (id, cwd string, hasCwd bool) {
	switch p := ev.Payload.(type) {
	case fleet.Session:
		return p.ID, string(p.Cwd), true
	case fleet.SessionStatePayload:
		return p.Ref.ID, "", false
	case fleet.SessionLabelsPayload:
		return p.Ref.ID, "", false
	case map[string]any:
		switch ev.Kind {
		case fleet.EventSessionCreated:
			id, _ = p["id"].(string)
			cwd, hasCwd = p["cwd"].(string)
			return id, cwd, hasCwd && id != ""
		case fleet.EventSessionState, fleet.EventSessionClosed, fleet.EventSessionLabels:
			ref, _ := p["ref"].(map[string]any)
			id, _ = ref["id"].(string)
			return id, "", false
		}
	}
	return "", "", false
}

// ensureStream starts, restarts or stops the driver-side subscription so that
// it watches exactly the union of what subscribers want.
//
// Restarting on every filter change is deliberate crudeness: the alternative
// is mutating a live subscription's filter, which the Driver interface does
// not offer and which would need its own resync story to be correct. A restart
// is visible, cheap at these scales, and cannot silently half-apply.
func (s *Service) ensureStream(ctx context.Context) { s.ensureStreamForce(ctx, false) }

// ensureStreamForce is ensureStream, optionally restarting a running
// driver-side stream even though its filter is unchanged. A change to the set of
// peers needs that: the peer pumps are started with the stream, so a stream
// already running would otherwise never hear from a peer added after it began
// (muster #347). Subscribers keep their connections; only the pumps restart.
//
// # Two lanes, restarted separately
//
// A restart takes a fresh baseline, which can absorb a change that happened in
// between (see defaultLinger), so a restart is worth avoiding for a pump whose
// filter did not change. The local and peer pumps therefore run under separate
// contexts and each decides for itself:
//
//   - The local lane restarts when its filter changes, as the whole stream did
//     before. A subscriber that only differs by directory never changes it.
//   - The peer lane restarts when the peers must be reached afresh (force), when
//     it is not running and somebody wants the fleet, or when the filter it runs
//     with no longer COVERS what is wanted (see covers) — a new subscriber
//     needs frames the stream was opened to leave out. A narrowing never
//     restarts it: a pump that is wider than needed is still correct, because
//     the hub filters per subscriber, and a departing subscriber is the common
//     case. It narrows again the next time the lane starts.
//
// # One caller at a time
//
// The whole function runs under h.ensure, from the read of what the lanes are
// doing to the record of what it started. Callers that arrive together queue:
// the first starts the lanes, the next sees them running and, finding its
// filter already covered, leaves them alone. Without that, each of them read a
// cold stream, each started its own pumps — the driver subscribed to two or
// three times, every event published once per lane — and the later record of a
// lane replaced the earlier one's cancel, so the earlier lane could never be
// stopped again (#375). The wait is short: the pumps start under `go`.
func (s *Service) ensureStreamForce(ctx context.Context, force bool) {
	h := s.events
	h.ensure.Lock()
	defer h.ensure.Unlock()

	wantLocal, wantPeer := h.streamFilters()
	listeners := h.subscriberCount()

	h.mu.Lock()
	on, cur, cancel := h.streamOn, h.streamFilter, h.streamCancel
	peerOn, peerCur, peerCancel := h.peerOn, h.peerFilter, h.peerCancel
	h.mu.Unlock()

	switch {
	case listeners == 0 && on:
		// Not cancelled here. The stream is left running for a window, so a
		// long-poll's next cycle rides the same subscription instead of
		// re-seeding a baseline that swallows whatever happened in between.
		h.armLinger()
		return
	case listeners == 0:
		return
	}

	// Somebody is listening again; any pending teardown is off.
	h.disarmLinger()

	if !on || force || !sameFilter(cur, wantLocal) {
		if on {
			cancel()
		}
		streamCtx, newCancel := context.WithCancel(context.WithoutCancel(ctx))
		h.mu.Lock()
		h.streamOn, h.streamFilter, h.streamCancel = true, wantLocal, newCancel
		h.mu.Unlock()

		for _, d := range s.localDrivers() {
			go s.pump(streamCtx, s.self, d, fleet.SystemRequest(), wantLocal)
		}
	}

	// Peers only when somebody actually asked for the fleet. See
	// subscriber.scope: a peer's own scope=local subscription must not make
	// us open streams back to our peers (§13.1).
	wantPeers := h.wantsPeers()
	if force || (wantPeers && (!peerOn || !covers(peerCur, wantPeer))) {
		if peerOn {
			peerCancel()
			h.mu.Lock()
			h.peerOn, h.peerCancel = false, nil
			h.mu.Unlock()
		}
		if wantPeers {
			peerCtx, newCancel := context.WithCancel(context.WithoutCancel(ctx))
			h.mu.Lock()
			h.peerOn, h.peerFilter, h.peerCancel = true, wantPeer, newCancel
			h.mu.Unlock()

			for m, d := range s.peerDrivers() {
				// Peers need an authority a local driver does not: see
				// Service.peerCredential for why this is the service's own and
				// not some subscriber's.
				go s.pump(peerCtx, m, d, s.peerRequest(), wantPeer)
			}
		}
	}
}

// pump feeds one driver's events into the hub, and keeps feeding them.
//
// # What the one-shot version did, and why it was the worst available failure
//
// This used to call Subscribe once and return on any error. A driver that
// cannot stream does say so once — ErrUnsupported is a statement about the
// substrate, and retrying it in a loop would be asking the same question
// forever. But every OTHER failure was treated the same way, including the
// transient ones, and the consequence was invisible from outside: the pump
// returned, nothing else ever tried, and the subscriber went on holding a
// stream that was open, healthy-looking and permanently empty.
//
// A stream delivering nothing is indistinguishable from a fleet in which
// nothing is happening. The driver's own lifecycle client already refuses that
// bargain — it re-attaches, and reports itself degraded while it cannot; this
// layer did not, and one reachable path made it ordinary rather than exotic: a
// machine with no sessions has nothing to attach a control client to, so a
// subscriber that connected to an idle machine never heard about the first
// session that machine started.
//
// So: retry with backoff, and ANNOUNCE. Both directions are announced, on the
// transition only, the way machine.quota is — a subscriber told it is deaf can
// re-list, and one told the feed is back knows to, because the gap it just
// lived through was never in the sequence to be replayed.
func (s *Service) pump(ctx context.Context, machine fleet.MachineId, d driver.Driver, req fleet.Request, filter driver.SubscribeFilter) {
	backoff := pumpBackoffMin
	gapAnnounced := false

	for {
		if ctx.Err() != nil {
			return
		}
		stream, err := d.Subscribe(ctx, req, filter)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !gapAnnounced {
				s.announceFeedGap(machine, err)
				gapAnnounced = true
			}
			// ErrUnsupported is the one error asking again cannot change — it
			// describes the substrate, not the moment. It is still announced,
			// once: "this machine reports no events" and "this machine has
			// nothing to report" are different facts (§5.7), and a subscriber
			// that cannot tell them apart waits forever on the wrong one.
			if errors.Is(err, driver.ErrUnsupported) {
				return
			}
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		if gapAnnounced {
			s.announceFeedRestored(machine)
			gapAnnounced = false
		}
		backoff = pumpBackoffMin

		err = s.drainStream(ctx, stream, func(ev fleet.Event) {
			s.resolveCwd(ctx, machine, d, req, ev)
		})
		_ = stream.Close()
		if ctx.Err() != nil {
			return
		}
		// The stream ended while somebody was still listening. That is a gap
		// whatever ended it, and the subscription that replaces it will start
		// from a fresh baseline.
		if !gapAnnounced {
			s.announceFeedGap(machine, err)
			gapAnnounced = true
		}
		if !s.sleep(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// drainStream forwards a live stream into the hub until it ends. resolve is
// given each event before it is published, and may learn what the hub needs to
// filter it (resolveCwd).
func (s *Service) drainStream(ctx context.Context, stream driver.EventStream, resolve func(fleet.Event)) error {
	for {
		ev, err := stream.Next(ctx)
		if err != nil {
			return err
		}
		resolve(ev)
		// A LOCAL driver's session.created carries no labels — the driver
		// never sees them (muster #153). Attach whatever this service
		// already holds. A create's own labels may land a moment after this
		// event; session.labels is what guarantees a subscriber sees them.
		if ev.Kind == fleet.EventSessionCreated && (ev.Machine == "" || ev.Machine == s.self) {
			if sess, ok := ev.Payload.(fleet.Session); ok {
				sess.Labels = s.labels.get("", sess.ID, sess.StartedAt)
				ev.Payload = sess
			}
		}
		// A local session gone from the stream is either an end or a rename's
		// retired id; a re-listing tells them apart and records the end with
		// a timely closedAt (#179, history.go).
		if ev.Kind == fleet.EventSessionClosed && (ev.Machine == "" || ev.Machine == s.self) {
			s.requestSweep()
		}
		s.events.publish(ev)
	}
}

// announceFeedGap says that this machine's changes are no longer being
// observed. It is a SourceStatus rather than a bare log line for the reason
// §9 gives: a source that failed contributes a status, never a silent absence.
func (s *Service) announceFeedGap(machine fleet.MachineId, cause error) {
	reason := "the event stream for this machine ended"
	if cause != nil {
		reason = "the event stream for this machine is not running: " + cause.Error()
	}
	s.events.publish(fleet.Event{
		Machine: machine,
		Kind:    fleet.EventSourceStatus,
		Payload: fleet.SourceStatus{
			Machine:    machine,
			Status:     fleet.SourceDegraded,
			Error:      reason,
			ObservedAt: time.Now(),
		},
	})
}

// announceFeedRestored says the stream is running again — and, separately,
// that what happened while it was not is not recoverable from the sequence.
//
// Two events rather than one, because they are two facts and a client acts on
// them differently. The status says this machine is observed again; the resync
// says the mirror it holds has a hole in it. A client that received only the
// first would resume from its cursor and never learn that the events it is
// resuming past were never stamped in the first place.
func (s *Service) announceFeedRestored(machine fleet.MachineId) {
	s.events.publish(fleet.Event{
		Machine: machine,
		Kind:    fleet.EventSourceStatus,
		Payload: fleet.SourceStatus{
			Machine:    machine,
			Status:     fleet.SourceOK,
			ObservedAt: time.Now(),
		},
	})
	s.events.publish(fleet.Event{
		Machine: machine,
		Kind:    fleet.EventControlResync,
		Payload: fleet.ControlResyncPayload{Reason: fleet.ResyncFeedGap},
	})
}

// sleep waits out a backoff, reporting false when the stream was cancelled
// instead.
func (s *Service) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > pumpBackoffMax {
		return pumpBackoffMax
	}
	return d
}

func sameFilter(a, b driver.SubscribeFilter) bool {
	if a.CwdPrefix != b.CwdPrefix || len(a.Sessions) != len(b.Sessions) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a.Sessions {
		seen[x] = true
	}
	for _, y := range b.Sessions {
		if !seen[y] {
			return false
		}
	}
	return true
}

// covers reports whether a stream opened with have delivers every frame one
// opened with want would, so that keeping it running loses nothing for the
// subscribers want was computed from. Each dimension is checked on its own, which
// is sufficient for the AND the two selectors compose with (it is not necessary:
// two narrower dimensions can be disjoint, and then the answer is a harmless
// "no").
func covers(have, want driver.SubscribeFilter) bool {
	if have.CwdPrefix != "" && !strings.HasPrefix(want.CwdPrefix, have.CwdPrefix) {
		return false
	}
	if len(have.Sessions) == 0 {
		return true
	}
	if len(want.Sessions) == 0 {
		return false
	}
	held := make(map[string]bool, len(have.Sessions))
	for _, id := range have.Sessions {
		held[id] = true
	}
	for _, id := range want.Sessions {
		if !held[id] {
			return false
		}
	}
	return true
}
