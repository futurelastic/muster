package opencode

import (
	"context"
	"fmt"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// What an idle server costs (muster #419).
//
// An opencode server that nobody talks to is cheap: measured on one machine, one
// with no client costs about 1 % of a core, and about 1.5–2 % with this driver's
// event stream attached and a finished turn behind it. What made seven idle
// sessions cost most of a core between them was traffic. Every listing the
// service makes — one per dashboard refresh, one per supervisor poll — asks every
// live server for GET /session/status, so the cost of one listing is one request
// per server, and a server paid roughly 2 % of a core for every request per second
// it was asked (the same endpoint at 2 and at 10 requests per second: about 5 %
// and 10 %). A steady 1.5 listings per second kept every idle server at several
// percent for as long as it ran.
//
// The status map is therefore read once per window and shared:
//
//   - A read that is fresh enough is served from memory.
//   - Callers that arrive while a read is in flight wait for that read instead of
//     starting their own, so N concurrent listings make one request per server.
//   - A read that failed is never kept: an error is reported to every caller that
//     was waiting on it and the next call asks again (§5.7: a failed read stays an
//     error, it is not turned into the last good answer).
//
// How long "fresh enough" is depends on whether anything would tell this driver
// the answer changed. With an event stream attached to the server (a subscription
// holds one; subscribe.go) every change of status, every permission ask and every
// message record arrives as an event, and the event drops what was read — so a
// change the runtime makes on its own is seen at once, and the window is only a
// ceiling that bounds the damage of a stream that went quiet while staying open
// (the risk subscribe.go accepts). With no stream attached nothing says anything
// changed, so the window is short.
//
// Writes this driver makes drop what was read too, and hold the cache closed for a
// moment (writeSettle): the runtime accepts a prompt before it reports the session
// working, and a read inside that gap would otherwise be kept for the whole window.
//
// What a reader can lose: a status read through this cache may be older than the
// window by the length of one request. A caller that needs a fresh answer about a
// change this driver did not cause and no stream announced waits at most the
// window for it — 2 s with no stream, 30 s with a silent one.
const (
	// statusTTLBus is the longest a status read is reused while an event stream is
	// attached to the server.
	statusTTLBus = 30 * time.Second
	// statusTTLBare is the longest it is reused with no stream attached.
	statusTTLBare = 2 * time.Second
	// writeSettle is how long after a write of this driver's own nothing is kept.
	writeSettle = 2 * time.Second
)

// statusTTLs are the two windows. Fields of the driver, not constants, so a test
// that flips the fake runtime's status without any write or event still reads
// what it flipped (withStatusTTL).
type statusTTLs struct {
	bus, bare time.Duration
}

// withStatusTTL sets the two windows. Unexported: a test seam, and the zero value
// turns the cache off while keeping its single flight.
func withStatusTTL(bus, bare time.Duration) Option {
	return func(d *Driver) { d.statusTTL = statusTTLs{bus: bus, bare: bare} }
}

// statusCache is what one server's reads are kept in. The zero value is ready.
// mu is a leaf lock: nothing that can block (a request, another lock) is done
// while it is held.
type statusCache struct {
	mu sync.Mutex
	// gen counts drops. A read stores its result only if the count is the one it
	// started under, so a read that raced a drop cannot put an old answer back.
	gen uint64
	// buses is how many event streams are attached to this server.
	buses int
	// holdUntil is when the last write's settling ends; nothing is stored before.
	holdUntil time.Time

	status readCell[statusMap]
	asks   readCell[[]wirePermissionAsk]
	idle   map[string]*idleMemo
}

// readCell is one kept read of one server, with the read in flight for it.
type readCell[T any] struct {
	have   bool
	at     time.Time
	val    T
	flight *readFlight[T]
}

// readFlight is a read in progress. done is closed once val and err are set.
type readFlight[T any] struct {
	done chan struct{}
	val  T
	err  error
}

func (c *readCell[T]) reset() {
	var zero T
	c.have, c.val, c.flight = false, zero, nil
}

// idleMemo is what confirming one idle session read: the session's own record, the
// newest message's verdict, and the usage figures State attached. All of it is
// valid only for the generation it was read under.
type idleMemo struct {
	gen       uint64
	at        time.Time
	sess      wireSession
	lastTurn  *fleet.TurnEnd
	usage     *fleet.Usage
	haveUsage bool
}

// ttlLocked is the window in force now.
func (c *statusCache) ttlLocked(w statusTTLs) time.Duration {
	if c.buses > 0 {
		return w.bus
	}
	return w.bare
}

// drop forgets everything read from the server. A read in flight is let finish
// for the callers already waiting on it, but its result is not kept and a caller
// arriving afterwards starts a read of its own.
func (c *statusCache) drop() {
	c.gen++
	c.status.reset()
	c.asks.reset()
	c.idle = nil
}

// invalidate drops what was read because the runtime said something changed.
func (c *statusCache) invalidate() {
	c.mu.Lock()
	c.drop()
	c.mu.Unlock()
}

// invalidateAfterWrite drops what was read because this driver changed something,
// and keeps nothing for writeSettle: the runtime may not report the change yet.
func (c *statusCache) invalidateAfterWrite(now time.Time) {
	c.mu.Lock()
	c.drop()
	c.holdUntil = now.Add(writeSettle)
	c.mu.Unlock()
}

// attached and detached count event streams on the server. Either drops what was
// read: a read from before the stream attached may have missed changes the stream
// would have announced, and one from while it ran may have missed what happened
// as it ended; neither is entitled to the long window.
func (c *statusCache) attached() {
	c.mu.Lock()
	c.buses++
	c.drop()
	c.mu.Unlock()
}

func (c *statusCache) detached() {
	c.mu.Lock()
	if c.buses > 0 {
		c.buses--
	}
	c.drop()
	c.mu.Unlock()
}

// invalidateStatus is invalidate for the server of a driver's own write.
func (d *Driver) invalidateStatus(srv *server) {
	if srv != nil {
		srv.status.invalidateAfterWrite(d.now())
		// muster #421: every write of this driver's own is activity of the session.
		srv.touch(d.now())
	}
}

// busEventDrops reports whether a bus event of this type means a read of the
// server may be out of date. Unknown names drop nothing: the cost is a stale
// answer for at most the window, never a wrong one that stays.
func busEventDrops(typ string) bool {
	switch typ {
	case "session.status", "session.idle", "session.error", "session.deleted", "session.updated",
		"message.updated", "message.removed",
		"permission.asked", "permission.updated", "permission.replied":
		return true
	}
	return false
}

// noAnswer is the error a caller gets when its own context ends while it waits for
// a shared read: the same one a request that never completed would have produced.
func (d *Driver) noAnswer(err error) error {
	return &fleet.Error{
		Kind:      fleet.ErrorUnreachable,
		Message:   fmt.Sprintf("no answer from the local opencode server: %v", err),
		Machine:   d.machine,
		Retryable: true,
	}
}

// readThrough serves one kept read of srv: from memory while it is fresh, from the
// read already in flight if there is one, from a new read otherwise. cell picks the
// slot; fetch makes the request.
//
// The read runs detached from the caller that starts it, under the driver's own
// deadline: if it ran under that caller's context, one caller giving up would fail
// every caller waiting on the same read. Each caller still honours its own context
// while it waits.
func readThrough[T any](ctx context.Context, d *Driver, srv *server, cell func(*statusCache) *readCell[T], fetch func(context.Context) (T, error)) (T, error) {
	c := &srv.status
	now := d.now()
	c.mu.Lock()
	cl := cell(c)
	if ttl := c.ttlLocked(d.statusTTL); cl.have && ttl > 0 && now.Sub(cl.at) < ttl {
		v := cl.val
		c.mu.Unlock()
		return v, nil
	}
	f := cl.flight
	if f == nil {
		f = &readFlight[T]{done: make(chan struct{})}
		cl.flight = f
		g0 := c.gen
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.deadline)
			defer cancel()
			v, err := fetch(fctx)
			at := d.now()
			c.mu.Lock()
			cl := cell(c)
			if err == nil && c.gen == g0 && !at.Before(c.holdUntil) {
				cl.have, cl.at, cl.val = true, at, v
			}
			if cl.flight == f {
				cl.flight = nil
			}
			f.val, f.err = v, err
			c.mu.Unlock()
			close(f.done)
		}()
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		var zero T
		return zero, d.noAnswer(ctx.Err())
	}
}

// statusOf is GET /session/status for srv, shared (see the package notes above).
// The map is shared too: a caller reads it and never writes to it.
func (d *Driver) statusOf(ctx context.Context, srv *server) (statusMap, error) {
	return readThrough(ctx, d, srv,
		func(c *statusCache) *readCell[statusMap] { return &c.status },
		func(ctx context.Context) (statusMap, error) {
			var m statusMap
			err := d.do(ctx, srv, "GET", "/session/status", nil, &m)
			return m, err
		})
}

// asksOf is GET /permission for srv, shared the same way: a session parked on an
// ask is listed as working, and without this every listing would read the asks once
// per such session.
func (d *Driver) asksOf(ctx context.Context, srv *server) ([]wirePermissionAsk, error) {
	return readThrough(ctx, d, srv,
		func(c *statusCache) *readCell[[]wirePermissionAsk] { return &c.asks },
		func(ctx context.Context) ([]wirePermissionAsk, error) {
			var asks []wirePermissionAsk
			err := d.do(ctx, srv, "GET", "/permission", nil, &asks)
			return asks, err
		})
}

// generation is the count a read about to start must remember, so that what it
// reads is kept only if nothing dropped the cache meanwhile.
func (c *statusCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// idleFor returns the kept confirmation of an idle session, if it is still fresh.
func (d *Driver) idleFor(srv *server, id string) (idleMemo, bool) {
	c := &srv.status
	now := d.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.idle[id]
	if ttl := c.ttlLocked(d.statusTTL); m == nil || m.gen != c.gen || ttl <= 0 || now.Sub(m.at) >= ttl {
		return idleMemo{}, false
	}
	return *m, true
}

// keepIdle stores a confirmation read under gen, unless a drop happened since.
func (d *Driver) keepIdle(srv *server, id string, gen uint64, m idleMemo) {
	c := &srv.status
	now := d.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen || now.Before(c.holdUntil) {
		return
	}
	if c.idle == nil {
		c.idle = map[string]*idleMemo{}
	}
	m.gen, m.at = gen, now
	c.idle[id] = &m
}

// keepIdleUsage adds the usage figures to a confirmation already kept.
func (d *Driver) keepIdleUsage(srv *server, id string, gen uint64, u *fleet.Usage) {
	c := &srv.status
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.idle[id]; m != nil && m.gen == gen && c.gen == gen {
		m.usage, m.haveUsage = u, true
	}
}

// forgetIdle drops what is kept of one session, for a session that is gone.
func (d *Driver) forgetIdle(srv *server, id string) {
	if srv == nil {
		return
	}
	c := &srv.status
	c.mu.Lock()
	delete(c.idle, id)
	c.mu.Unlock()
}

// copyUsage returns a copy a caller may hold: the kept one is shared.
func copyUsage(u *fleet.Usage) *fleet.Usage {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}
