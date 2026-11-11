package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// # The list snapshot (muster #464)
//
// GET /v1/sessions used to enumerate every session from every driver, build
// every row and encode the whole envelope on every call, so its cost scaled
// with sessions × callers × poll rate whether or not anything had changed
// between two reads. This keeps the last listing it built, as the encoded body,
// and answers a read with nothing changed since from that — and, because the
// body carries a validator, a conditional read (If-None-Match) from a client
// that already holds it with no body at all.
//
// # What "nothing changed" is allowed to mean
//
// The issue that asked for this proposed invalidating on "the same change
// notifications that drive /v1/sessions/watch". That is the right place to
// start and not enough on its own, because the event plane does not observe
// every way a row changes:
//
//   - It observes the drivers only while somebody is watching (plus a linger),
//     and the cursor does not move while nobody is. A service nobody watches
//     has no notion of "unchanged" to offer, so it offers none (coversLocal).
//   - A driver emits some changes only when they are material, or only for the
//     sessions it holds a client for, and a session-level field (conversation,
//     model, …) can change with no event at all. A snapshot's AGE is therefore
//     bounded (listSnapshotMaxAge), and the documentation says what that costs.
//   - Some service writes publish nothing: a close, a create with no labels. So
//     every mutating request bumps a generation of the service's own, before
//     its response reaches the client (Service.mutating).
//   - A runtime that registers, or changes retry state, publishes nothing: the
//     generation is bumped there too, and a lane that was not started with the
//     runtime is not trusted to watch it (hub.coversLocal).
//
// So a snapshot is valid while the event cursor, the lane generation and the
// service generation are all the ones it was built under, the lane is still
// covering every local runtime, and it is younger than the age bound. Any of
// those failing is not an error: the read enumerates, exactly as it always did.
//
// # Why scope=fleet with a peer is not cached
//
// A peer's rows are fetched with the CALLER's own credential — proxying does
// not launder authority (§13) — so one snapshot cannot be shared between
// callers without showing one of them rows it was not entitled to, or
// reporting an unauthorized source it never asked. And the local hub only hears
// a peer's changes while a fleet-scoped watcher exists, on a lane that can be
// narrowed, so no validator this side holds could promise to move when a peer's
// row does. A fleet read with no peers configured is the local read, and is
// keyed as one.
const (
	// listSnapshotMaxAge bounds how stale a served snapshot can be in the fields
	// no event reports. Five seconds is the figure the single-session read (#339)
	// and the relist gap (#415) already use for the same reason.
	listSnapshotMaxAge = 5 * time.Second

	// listSnapshotMaxEntries bounds the cache: filters are the caller's choice,
	// so the number of distinct questions is not ours to assume.
	listSnapshotMaxEntries = 16
)

// listRevision is everything a snapshot's validity is compared on. The three
// counters only ever increase while a service runs, so a value is never reused.
type listRevision struct {
	cursor  int64  // the hub's event cursor: any published event moves it
	laneGen uint64 // the local lane starting or stopping
	gen     uint64 // the service's own writes and registrations
}

// listKey identifies one question: the effective scope and the parsed filter.
// It is built from the PARSED filter, never the raw query, so a parameter the
// list does not read cannot split the cache.
type listKey string

func newListKey(scope Scope, f driver.ListFilter) listKey {
	var b strings.Builder
	field := func(v string) { fmt.Fprintf(&b, "%d:%s|", len(v), v) }
	field(string(scope))
	field(string(f.Status))
	field(string(f.Agent))
	field(f.CwdPrefix)
	field(f.ID)
	keys := make([]string, 0, len(f.Labels))
	for k := range f.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		field(k)
		field(f.Labels[k])
	}
	return listKey(b.String())
}

// listEntry is one built answer. etag is empty when the answer must not be
// offered a validator (a driver failed to answer): such an entry is served once
// to whoever built it and never stored.
type listEntry struct {
	rev     listRevision
	key     listKey
	body    []byte
	etag    string
	builtAt time.Time
}

type listFlight struct {
	rev   listRevision
	done  chan struct{}
	entry *listEntry // set before done closes; nil when the build failed or was not storable
}

type flightRole int

const (
	flightLeader   flightRole = iota // this caller builds, and others may wait on it
	flightFollower                   // someone building the same revision is ahead of us
	flightAlone                      // someone building a DIFFERENT revision is; build without joining
)

type listSnapshots struct {
	// nonce makes validators unique to this process. The event epoch and cursor
	// can continue across a restart (#454) while the world moves on, so a
	// validator derived only from them could match one a previous run handed out
	// for a different state; this cannot.
	nonce string

	gen      atomic.Uint64 // the service's own writes and registrations
	buildSeq atomic.Uint64 // every build, so an age-expiry rebuild gets a new validator
	now      func() time.Time

	mu      sync.Mutex
	entries map[listKey]*listEntry
	flights map[listKey]*listFlight

	served, built, bypassed atomic.Int64
}

// setClock replaces the clock the age bound is read from, for a test that must
// cross the bound without waiting for it.
func (c *listSnapshots) setClock(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func (c *listSnapshots) clock() time.Time {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	return now()
}

func newListSnapshots() *listSnapshots {
	var raw [8]byte
	nonce := ""
	if _, err := rand.Read(raw[:]); err == nil {
		nonce = hex.EncodeToString(raw[:])
	} else {
		nonce = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return &listSnapshots{
		nonce:   nonce,
		now:     time.Now,
		entries: make(map[listKey]*listEntry),
		flights: make(map[listKey]*listFlight),
	}
}

// invalidate says the service changed something a listing shows (or could show)
// that the event plane did not stamp.
func (c *listSnapshots) invalidate() { c.gen.Add(1) }

func (c *listSnapshots) generation() uint64 { return c.gen.Load() }

// get returns the stored entry for key if it is still valid at rev.
func (c *listSnapshots) get(key listKey, rev listRevision) *listEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || e.rev != rev || c.now().Sub(e.builtAt) >= listSnapshotMaxAge {
		return nil
	}
	return e
}

func (c *listSnapshots) join(key listKey, rev listRevision) (*listFlight, flightRole) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur := c.flights[key]; cur != nil {
		if cur.rev == rev {
			return cur, flightFollower
		}
		return nil, flightAlone
	}
	fl := &listFlight{rev: rev, done: make(chan struct{})}
	c.flights[key] = fl
	return fl, flightLeader
}

func (c *listSnapshots) finish(key listKey, fl *listFlight, entry *listEntry) {
	c.mu.Lock()
	if c.flights[key] == fl {
		delete(c.flights, key)
	}
	fl.entry = entry
	c.mu.Unlock()
	close(fl.done)
}

// store keeps entry if it is still current: an answer that was already stale
// when it was finished is useless, and keeping it would only crowd out a good
// one. Entries built under any other revision are dropped on the way, since no
// read can ever match them again; what remains is bounded by
// listSnapshotMaxEntries, oldest first.
func (c *listSnapshots) store(entry *listEntry, current listRevision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.rev != current {
			delete(c.entries, k)
		}
	}
	if entry.rev != current {
		return
	}
	c.entries[entry.key] = entry
	for len(c.entries) > listSnapshotMaxEntries {
		var oldestKey listKey
		var oldest time.Time
		first := true
		for k, e := range c.entries {
			if k == entry.key {
				continue
			}
			if first || e.builtAt.Before(oldest) {
				oldestKey, oldest, first = k, e.builtAt, false
			}
		}
		delete(c.entries, oldestKey)
	}
}

func (c *listSnapshots) entryCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// counters is what /v1/health reports, so a deployment can read its own hit
// rate instead of inferring it.
func (c *listSnapshots) counters() map[string]int64 {
	return map[string]int64{
		"served":   c.served.Load(),
		"built":    c.built.Load(),
		"bypassed": c.bypassed.Load(),
	}
}

// etagFor derives a strong validator from everything the body depends on. It
// is opaque: a client must treat it as a token to send back, not read it.
func (c *listSnapshots) etagFor(epoch string, key listKey, rev listRevision, seq uint64) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d", c.nonce, epoch, key, rev.cursor, rev.laneGen, rev.gen, seq)
	return `"` + base64.RawURLEncoding.EncodeToString(sum.Sum(nil)[:16]) + `"`
}

// listServed is what the handler writes: a body and the validator it carries,
// or just the validator when the client already holds the body.
type listServed struct {
	body        []byte
	etag        string
	notModified bool
}

func servedFrom(e *listEntry, ifNoneMatch string) *listServed {
	if e.etag != "" && ifNoneMatchHits(ifNoneMatch, e.etag) {
		return &listServed{etag: e.etag, notModified: true}
	}
	return &listServed{body: e.body, etag: e.etag}
}

// ifNoneMatchHits reports whether an If-None-Match header names etag, with the
// weak comparison RFC 9110 §13.1.2 prescribes for it: a W/ prefix is ignored, a
// comma separates members, and * matches any current representation.
func ifNoneMatchHits(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, member := range strings.Split(header, ",") {
		member = strings.TrimPrefix(strings.TrimSpace(member), "W/")
		if member != "" && member == want {
			return true
		}
	}
	return false
}

// listRevisionNow reads, in one step, what a snapshot is stamped with and
// whether the event plane can vouch for it. The two registries the answer
// depends on (the registered runtimes and the generation) are read under one
// lock, because registering a runtime moves both and a reader must see the set
// and the generation from the same moment.
//
// It is the capture a build must take BEFORE it enumerates — the same rule
// FeedPosition states: a change that lands during the enumeration then moves
// the revision past the one the entry was built under, and the entry is never
// matched, rather than being served as current with the change missing from it.
func (s *Service) listRevisionNow() (rev listRevision, peers int, covered bool) {
	s.mu.RLock()
	gen := s.lists.generation()
	registered := make([]fleet.RuntimeId, 0, len(s.local))
	for rt := range s.local {
		registered = append(registered, rt)
	}
	peers = len(s.peers)
	s.mu.RUnlock()
	cursor, laneGen, ok := s.events.coversLocal(registered)
	return listRevision{cursor: cursor, laneGen: laneGen, gen: gen}, peers, ok
}

// listFromSnapshot answers a list read from the snapshot when it may, and
// reports (nil, nil) when it may not — the caller then does exactly what it did
// before this existed. ifNoneMatch is the request's If-None-Match, joined.
func (s *Service) listFromSnapshot(ctx context.Context, req fleet.Request, scope Scope, filter driver.ListFilter, deadline time.Duration, ifNoneMatch string) (*listServed, error) {
	c := s.lists
	rev, peers, covered := s.listRevisionNow()
	if !covered || (scope == ScopeFleet && peers > 0) {
		c.bypassed.Add(1)
		return nil, nil
	}
	if scope == ScopeFleet {
		scope = ScopeLocal // no peer configured: the same question, the same answer
	}
	key := newListKey(scope, filter)

	if e := c.get(key, rev); e != nil {
		c.served.Add(1)
		return servedFrom(e, ifNoneMatch), nil
	}

	fl, role := c.join(key, rev)
	if role == flightFollower {
		select {
		case <-fl.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if fl.entry != nil {
			c.served.Add(1)
			return servedFrom(fl.entry, ifNoneMatch), nil
		}
		// The leader failed, or its answer was not one to share (a short deadline
		// that cost it a driver). Enumerate for ourselves rather than inherit it.
		c.bypassed.Add(1)
		return nil, nil
	}

	var shared *listEntry
	if role == flightLeader {
		defer func() { c.finish(key, fl, shared) }()
		// A build that finished between our look and our joining has already
		// stored what we were about to compute.
		if e := c.get(key, rev); e != nil {
			shared = e
			c.served.Add(1)
			return servedFrom(e, ifNoneMatch), nil
		}
	}
	entry, storable, err := s.buildListEntry(ctx, req, scope, filter, deadline, rev, key)
	if err != nil {
		return nil, err
	}
	if storable {
		shared = entry
	}
	return servedFrom(entry, ifNoneMatch), nil
}

// buildListEntry enumerates once and encodes the answer. Only an answer in which
// every driver that was asked answered ok is given a validator and kept: a
// degraded read is a statement about a moment, not about the fleet, and nothing
// the revision tracks would ever correct it.
func (s *Service) buildListEntry(ctx context.Context, req fleet.Request, scope Scope, filter driver.ListFilter, deadline time.Duration, rev listRevision, key listKey) (*listEntry, bool, error) {
	c := s.lists
	c.built.Add(1)
	col, driversOK, err := s.listSessions(ctx, req, scope, filter, deadline)
	if err != nil {
		return nil, false, err
	}
	// The lane was covering when rev was captured, so the cursor in rev IS a
	// resume point (the same fact FeedPosition reports as `resumable`).
	col = col.WithFeed(rev.cursor, s.epoch)
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(col); err != nil {
		return nil, false, err
	}
	entry := &listEntry{rev: rev, key: key, body: buf.Bytes(), builtAt: c.clock()}
	if !driversOK {
		return entry, false, nil
	}
	entry.etag = c.etagFor(s.epoch, key, rev, c.buildSeq.Add(1))
	current, _, _ := s.listRevisionNow()
	c.store(entry, current)
	return entry, true, nil
}

// writeListServed writes a snapshot answer. A validator brings its caching
// headers with it: `no-cache` says a stored copy must be revalidated before use
// (that is the whole point of the validator), `private` that it is not for a
// shared cache, and Vary names the request header the answer can depend on — a
// principal pinned to this machine is answered locally for the same URL
// (scopeFor).
func writeListServed(w http.ResponseWriter, sv *listServed) {
	w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
	if sv.etag != "" {
		w.Header().Set("ETag", sv.etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Add("Vary", "Authorization")
	}
	if sv.notModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sv.body)
}
