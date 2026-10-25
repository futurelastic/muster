// Package service is the machine-local muster instance:
// session-abstraction.md §13's "one service per machine, proxying to
// peers." Service owns local drivers (one per runtime, §4.1) and peer
// drivers (one per configured remote machine, §4.2), and NewMux wires the
// HTTP surface of docs/spec/api-http.md over it.
//
// Nothing here is a "real driver" — see internal/drivers/stub, the only
// driver wired up by cmd/muster today. This package is the routing,
// deadline, idempotency, auth, and error-mapping skeleton the task asked
// for: real request parsing and real envelope construction, over a
// deliberately fake backend.
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/state"
)

// eventSequence is what §7.3 needs to survive a restart.
//
// Persisting the epoch is only honest if the cursor continues with it. An
// epoch says "this is the same sequence"; a service that kept its epoch while
// restarting its cursor at zero would hand subscribers numbers it had already
// used, which is worse than announcing a new instance.
//
// The retained event WINDOW is deliberately not persisted. A subscriber
// resuming from an old cursor therefore gets resync_required — correct, and
// announced as cursor_expired rather than epoch_changed, which is the truthful
// reason: the sequence continued, this service simply cannot replay that far
// back. Persisting the window would buy transparent restarts at the cost of
// durably storing every event, which is a much larger mechanism than the
// problem justifies. The one question a lost window leaves a consumer unable
// to answer — which sessions ended, and when — has its own small record
// instead (history.go, muster #179).
type eventSequence struct {
	Epoch  string `json:"epoch"`
	Cursor int64  `json:"cursor"`
}

// Scope selects how far a plural query reaches (api-http.md §3.2).
type Scope string

const (
	// ScopeLocal restricts a query to this machine's own drivers only, and
	// must never recurse into peers (§13.1). This is what a peer receiving
	// a proxied call answers with — "a proxied request asks for the
	// peer's LOCAL view only."
	ScopeLocal Scope = "local"
	// ScopeFleet fans out to every configured peer, one hop deep, and
	// merges their answers (§13). The default for external clients
	// (api-http.md §3.2).
	ScopeFleet Scope = "fleet"
)

// Service is one machine's muster instance. A peer driver is, from
// this package's point of view, just another driver.Driver — the whole
// point of §4.2 is that federation needs no separate interface, only a
// second kind of registration.
type Service struct {
	self      fleet.MachineId
	epoch     string
	startedAt time.Time
	// build is read once at construction: it describes the binary, which
	// cannot change while the binary runs.
	build fleet.Build

	// peerCredential authorizes this service's OWN long-lived reads from
	// peers — specifically the event subscriptions the hub multiplexes.
	//
	// This is deliberately not the confused deputy D1 removed, and the
	// difference is worth being precise about. A unary proxied call has
	// exactly one caller, so it must present that caller's authority, and a
	// proxy holding its own credential could silently substitute it. A
	// multiplexed subscription has MANY callers at once and outlives any of
	// them: there is no single original caller whose authority it could
	// carry, so pretending otherwise would mean picking one subscriber's
	// credential and serving everyone else under it — which is worse than
	// acting openly as the service.
	//
	// So the service subscribes as itself. §6 permits reads broadly and
	// gates mutations, and a subscription is a read; nothing mutating ever
	// uses this. The residual widening is real and recorded as §14 D9: with
	// one shared token, "as itself" and "as any caller" are the same
	// authority, and only per-peer identity can separate them.
	peerCredential string

	state *state.Store

	// labels is where caller-supplied session labels live (muster
	// #153) — see labels.go for why the service stores them, not a driver.
	labels *labelStore

	// history keeps one record per ended local session for a bounded time
	// (muster #179) — see history.go.
	history *historyStore

	// inputAudit keeps one record per /input call for a bounded time (muster
	// #330) — see inputaudit.go.
	inputAudit *inputAuditStore

	// peerKeys is the rotation overlay for principals that sign (muster #350),
	// and sigReplay the nonces of the signatures already accepted.
	peerKeys  *peerKeyStore
	sigReplay *peerkey.ReplayCache

	// sweepMu guards the background re-listing a local session.closed event
	// asks for (requestSweep).
	sweepMu    sync.Mutex
	sweeping   bool
	sweepAgain bool

	// events is the service-wide event plane: §7.3's cursor and epoch live
	// here because they are per service instance, not per driver.
	events *hub

	// principalTable is the live principal table the mux was built over, if any
	// (muster #347); long-lived handlers bind their streams to it.
	principalTable atomic.Pointer[PrincipalTable]

	mu    sync.RWMutex
	local map[fleet.RuntimeId]driver.Driver
	peers map[fleet.MachineId]driver.Driver

	// unavailable holds the runtimes the operator enabled that have no working
	// driver right now (muster #297; runtimes.go). Guarded by mu and always
	// disjoint from local: RegisterLocalDriver moves a runtime out of here in
	// the same critical section that puts its driver in, so a reader sees the
	// runtime exactly once — never zero rows, never two.
	unavailable map[fleet.RuntimeId]unavailableRuntime

	// defaultRuntime is the machine-local tiebreak resolveSessionDriver
	// falls back to once existence-first resolution has genuinely nothing
	// to route a bare id to (muster issue #60, ⚖ ruling). Empty means
	// exactly what cmd/muster/config.go's own doc comment says an
	// absent setting always means in that file: "the older behaviour" —
	// bare-id addressing among more than one local runtime is refused
	// (ErrAmbiguousSession) rather than guessed.
	//
	// Set only through SetDefaultRuntime, which refuses a value naming a
	// runtime not already registered — guardrail 1 of #60: a typo here
	// must fail once, at startup, rather than turn into a fleet-wide
	// not_found on every bare-id call that looks exactly like sessions
	// having disappeared.
	defaultRuntime fleet.RuntimeId

	// maxInputBytes is the effective limit this machine enforces on
	// `prompt` (create) and `text` (input) — muster #130. Initialized
	// to defaultMaxInputBytes (http.go) in newService and therefore never
	// zero: an "unconfigured" instance still has an effective limit, it is
	// simply the shipped default, exactly the behaviour #130 requires an
	// unconfigured deployment to keep. Changed only through
	// SetMaxInputBytes, meant to be called at most once, at startup, the
	// same one-shot-config rule defaultRuntime above follows.
	maxInputBytes int

	// deliveryModuleRoutes is the set of names a caller may write as a send's
	// `route` to force an external delivery module (#185): exactly the modules
	// this machine's operator enabled, whether or not their executables are
	// present right now. A name is a route because the caller means THAT
	// module, and an unknown name is refused as the request's own shape rather
	// than discovered per session. Nil/empty means none are enabled — the
	// route vocabulary is then exactly what it was before #185. Set once at
	// startup by SetDeliveryModuleRoutes, the same one-shot-config rule
	// defaultRuntime and maxInputBytes follow.
	deliveryModuleRoutes []string

	// selfGrants evaluates this machine's own authorization model for a
	// credential — installed by NewMux, which is where the model lives. Nil
	// for a Service used without a mux, whose self standing then reads
	// assumed rather than inventing an answer (muster #154).
	selfGrants func(token string) []string

	// verifyMu single-flights GET /v1/machines?verify=1: a caller holding
	// only read must not be able to multiply probes to every peer.
	verifyMu sync.Mutex
}

// setSelfGrantResolver installs how this service answers "what does my own
// table grant this credential". See Service.selfGrants.
func (s *Service) setSelfGrantResolver(f func(token string) []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfGrants = f
}

// selfStanding is the self item's PeerStanding (muster #154): this
// machine trivially lists itself, and grantsToMe is what its own table grants
// the credential it presents to peers — so the same fact is comparable when
// read from either machine.
func (s *Service) selfStanding() fleet.PeerStanding {
	s.mu.RLock()
	resolve, tok := s.selfGrants, s.peerCredential
	s.mu.RUnlock()
	yes := true
	if resolve == nil {
		st := fleet.AssumedPeerStanding()
		st.ListsMeBack = &yes
		return st
	}
	now := time.Now()
	return fleet.PeerStanding{ListsMeBack: &yes, GrantsToMe: resolve(tok), Source: fleet.CapabilitiesObserved, ObservedAt: &now}
}

// RefreshPeers re-probes every peer that can be probed, concurrently, each
// bounded by its effective deadline — GET /v1/machines?verify=1, for an
// operator who has just edited a configuration and will not wait for the
// cached cycle (muster #154). Concurrent callers are serialized.
func (s *Service) RefreshPeers(ctx context.Context, callerDeadline time.Duration) {
	s.verifyMu.Lock()
	defer s.verifyMu.Unlock()
	req := s.peerRequest()
	var wg sync.WaitGroup
	for _, d := range s.peerDrivers() {
		refresher, ok := d.(driver.CapabilityRefresher)
		if !ok {
			continue
		}
		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, deadline)
			defer cancel()
			_ = refresher.RefreshCapabilities(callCtx, req)
		}()
	}
	wg.Wait()
}

// New constructs a Service without durable state. See NewWithState.
func New(self fleet.MachineId) *Service {
	svc, _ := NewWithState(self, nil)
	return svc
}

// NewWithState constructs a Service that remembers across restarts (§7.3,
// §10, §12). A nil store means in-memory only, which is a legitimate
// configuration and not a degraded one.
//
// self is this machine's own identity, used to stamp SourceStatus entries this
// service produces directly — as opposed to adopting a peer's own self-report
// (§13.2).
func NewWithState(self fleet.MachineId, st *state.Store) (*Service, error) {
	svc := newService(self)
	svc.state = st
	if st == nil {
		return svc, nil
	}
	if err := svc.labels.load(st); err != nil {
		return nil, err
	}
	if err := svc.history.load(st); err != nil {
		return nil, err
	}
	if err := svc.inputAudit.load(st); err != nil {
		return nil, err
	}
	if err := svc.peerKeys.load(st); err != nil {
		return nil, err
	}

	var seq eventSequence
	found, err := st.Load("events", &seq)
	if err != nil {
		return nil, err
	}
	if found && seq.Epoch != "" {
		svc.epoch = seq.Epoch
		svc.events.epoch = seq.Epoch
		svc.events.cursor = seq.Cursor
		svc.events.persist = func(c int64) {
			_ = st.Save("events", eventSequence{Epoch: seq.Epoch, Cursor: c})
		}
		return svc, nil
	}
	epoch := svc.epoch
	svc.events.persist = func(c int64) {
		_ = st.Save("events", eventSequence{Epoch: epoch, Cursor: c})
	}
	return svc, st.Save("events", eventSequence{Epoch: epoch})
}

func newService(self fleet.MachineId) *Service {
	now := time.Now()
	return &Service{
		self: self,
		// epoch identifies this service instance (§7.3): it changes on
		// every restart, exactly what a subscriber's stale epoch needs to
		// compare against. Derived from startedAt rather than a random id
		// — no dependency needed for a UUID, and it is human-legible in
		// GET /v1/health.
		epoch:      now.UTC().Format(time.RFC3339Nano),
		startedAt:  now,
		build:      fleet.SelfBuild(),
		local:      make(map[fleet.RuntimeId]driver.Driver),
		peers:      make(map[fleet.MachineId]driver.Driver),
		events:     newHub(self, now.UTC().Format(time.RFC3339Nano)),
		labels:     newLabelStore(),
		history:    newHistoryStore(self),
		inputAudit: newInputAuditStore(),
		peerKeys:   newPeerKeyStore(),
		sigReplay:  peerkey.NewReplayCache(maxSignedNonces),
		// #130: every instance starts with the shipped default in force,
		// not a zero value a caller could mistake for "no limit" — see the
		// field's own doc comment.
		maxInputBytes: defaultMaxInputBytes,
	}
}

// Events opens a subscription on this service's event plane (§7.3, §13).
//
// from* carry what the caller last saw. A caller resuming with a cursor this
// service can no longer supply, or with another instance's epoch, is told to
// resync rather than resumed from an arbitrary point — the whole of §7.3.
//
// The returned cancel must be called; it releases the subscriber and, when the
// last one leaves, stops the driver-side stream so an idle service watches
// nothing.
func (s *Service) Events(ctx context.Context, scope Scope, filter driver.SubscribeFilter, fromCursor int64, fromEpoch string) (<-chan fleet.Event, []fleet.Event, bool, func()) {
	sub, backlog, needResync := s.events.add(scope, filter, fromCursor, fromEpoch)
	s.ensureStream(ctx)
	return sub.ch, backlog, needResync, func() {
		s.events.remove(sub.id)
		s.ensureStream(ctx)
	}
}

// SetPeerCredential supplies the authority for this service's own peer
// subscriptions. See the field comment; without it, peer streams are refused
// and the fleet's event plane is silently local-only.
func (s *Service) SetPeerCredential(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerCredential = tok
}

func (s *Service) peerRequest() fleet.Request {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fleet.Request{Caller: fleet.Caller{
		Principal:  "system:" + string(s.self),
		Credential: s.peerCredential,
	}}
}

// Epoch reports this instance's epoch (§7.3).
func (s *Service) Epoch() string { return s.epoch }

// FeedPosition reports where the event sequence stands, and whether that
// number is a usable resume point.
//
// The third return is not a formality. This service's cursor advances only
// while it is actually observing a driver, so with nothing subscribed the
// sequence is frozen while the fleet keeps moving. A snapshot stamped with a
// frozen cursor looks resumable and is not: a client watching from it would
// silently skip everything that happened before the first subscription. So the
// caller is told whether the number means anything, and the wire omits it
// entirely when it does not (fleet.FeedPosition, §5.7).
func (s *Service) FeedPosition() (cursor int64, epoch string, resumable bool) {
	return s.events.currentCursor(), s.epoch, s.events.streamLive()
}

// Self reports this machine's own id. The HTTP layer needs it to tell a
// request about this machine from one destined for a peer — two different
// permissions (§6, §14 D6).
func (s *Service) Self() fleet.MachineId { return s.self }

// RegisterLocalDriver adds a driver for one runtime on this machine. It
// rejects a driver whose declared capabilities fail Validate (§4.4) — an
// undeadlined driver is refused at registration, not discovered by a
// req blocking forever.
func (s *Service) RegisterLocalDriver(runtime fleet.RuntimeId, d driver.Driver) error {
	if err := d.Capabilities().Validate(); err != nil {
		return fmt.Errorf("service: registering local driver %q: %w", runtime, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.local[runtime] = d
	delete(s.unavailable, runtime)
	return nil
}

// RegisterPeerDriver adds a driver fronting one peer machine (§4.2; §7.2 —
// peers are statically configured, never discovered or announced).
func (s *Service) RegisterPeerDriver(machine fleet.MachineId, d driver.Driver) error {
	if err := d.Capabilities().Validate(); err != nil {
		return fmt.Errorf("service: registering peer driver %q: %w", machine, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peers[machine] = d
	return nil
}

// UnregisterPeerDriver removes the driver fronting one peer machine, returning
// it (nil when there was none) so the caller can stop it (muster #347).
func (s *Service) UnregisterPeerDriver(machine fleet.MachineId) driver.Driver {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.peers[machine]
	delete(s.peers, machine)
	return d
}

// PeersChanged tells the event plane that the set of peer drivers changed, so a
// running stream restarts its peer pumps against the new set. Subscribers stay
// connected. Call it once after a batch of Register/UnregisterPeerDriver.
func (s *Service) PeersChanged() {
	s.ensureStreamForce(context.Background(), true)
}

// SetDefaultRuntime configures this machine's tiebreak for bare-id session
// resolution once more than one local driver is registered (muster
// issue #60, ⚖ ruling). runtime must already be a REGISTERED local driver —
// refused otherwise, so an operator's typo in the config file is a startup
// failure read once, never a fleet-wide not_found that reads exactly like
// every session having disappeared (guardrail 1).
//
// Call it only after every RegisterLocalDriver this instance will ever make.
// There is no mechanism here to revalidate against a driver registered
// afterwards, the same way cmd/muster/main.go's trust-root and peer
// wiring are one-shot startup steps rather than something reconciled later.
//
// An empty runtime is accepted and clears any default previously set — the
// zero value already means "no default configured," so this is a no-op
// against a Service that has never had one, and a caller need not special-
// case that.
func (s *Service) SetDefaultRuntime(runtime fleet.RuntimeId) error {
	if runtime == "" {
		s.mu.Lock()
		s.defaultRuntime = ""
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.local[runtime]; !ok {
		// An enabled runtime that is still being retried is a valid default
		// (muster #297): refusing it would make a slow probe at startup fatal
		// to the whole service. A typo, or a runtime that needs somebody's
		// hands (not installed), is still refused — guardrail 1.
		if u, retrying := s.unavailable[runtime]; retrying && u.retrying {
			s.defaultRuntime = runtime
			return nil
		}
		known := make([]string, 0, len(s.local))
		for rt := range s.local {
			known = append(known, string(rt))
		}
		sort.Strings(known)
		return fmt.Errorf("service: default runtime %q is not a registered local driver (have: %s)",
			runtime, strings.Join(known, ", "))
	}
	s.defaultRuntime = runtime
	return nil
}

// DefaultRuntime reports the configured default, or "" when none is set.
func (s *Service) DefaultRuntime() fleet.RuntimeId {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaultRuntime
}

// maxInputBytesCeiling bounds a configured limit from above — muster
// #130: "a value large enough to be meaningless should be refused with a
// clear reason rather than silently honoured." A composer is for a prompt,
// not a document; #128 already established that detailed briefing material
// belongs in a file the agent reads deliberately, and #114's own rationale
// names that same case as far below any value this cap should ever need to
// allow. 1 MiB is comfortably past any legitimate prompt while still small
// enough to catch the likely mistake this validation exists for — a byte
// count where a kilobyte or megabyte count was meant.
const maxInputBytesCeiling = 1 << 20 // 1 MiB

// SetMaxInputBytes configures this machine's limit on `prompt` (create) and
// `text` (input) — muster #130. Meant to be called at most once, at
// startup, before this instance serves any request — the same one-shot rule
// SetDefaultRuntime documents above: an invalid value is a message an
// operator reads once at boot, never a refusal manufactured per request.
//
// n must be strictly positive and strictly below maxInputBytesCeiling;
// anything else is refused with a reason naming both the value and the
// bound it broke, following this repo's posture of refusing loudly rather
// than substituting a default (#114's own rationale for the limit itself).
func (s *Service) SetMaxInputBytes(n int) error {
	if n <= 0 {
		return fmt.Errorf("service: maxInputBytes must be positive, got %d (#130)", n)
	}
	if n >= maxInputBytesCeiling {
		return fmt.Errorf(
			"service: maxInputBytes %d is at or above the %d-byte ceiling (#130): "+
				"detailed content belongs in a file the agent reads deliberately (#128), not a composer",
			n, maxInputBytesCeiling)
	}
	s.mu.Lock()
	s.maxInputBytes = n
	s.mu.Unlock()
	return nil
}

// SetDeliveryModuleRoutes tells the service which external delivery module
// names a caller may force with a send's `route` (#185). Meant to be called at
// most once, at startup. A name outside the module-name grammar, or one that
// would shadow a fixed route ("auto", "terminal", "inbox", "module"), is
// refused: a typo in an operator's list is a message read once at boot, never
// a route that silently means something else.
func (s *Service) SetDeliveryModuleRoutes(names []string) error {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !delivery.ValidModuleName(n) {
			return fmt.Errorf("service: %q is not a legal delivery module name (#185): lowercase "+
				"letters, digits and hyphens, starting with a letter, at most 32 bytes, and never "+
				"one of the fixed routes", n)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	s.mu.Lock()
	s.deliveryModuleRoutes = out
	s.mu.Unlock()
	return nil
}

// deliveryModuleRouteNames returns the module names a caller may force, in the
// operator's configured order of preference. The slice is never mutated after
// SetDeliveryModuleRoutes, so it is safe to return without copying.
func (s *Service) deliveryModuleRouteNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deliveryModuleRoutes
}

// isDeliveryModuleRoute reports whether name is one of this machine's enabled
// delivery modules.
func (s *Service) isDeliveryModuleRoute(name string) bool {
	for _, n := range s.deliveryModuleRouteNames() {
		if n == name {
			return true
		}
	}
	return false
}

// MaxInputBytes reports this machine's effective limit on `prompt` (create)
// and `text` (input) — muster #130. Always positive: constructed with
// defaultMaxInputBytes in force, and only ever replaced by SetMaxInputBytes
// with another positive value, so there is no unset state a caller could
// read as "no limit."
func (s *Service) MaxInputBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxInputBytes
}

func (s *Service) localDrivers() []driver.Driver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]driver.Driver, 0, len(s.local))
	for _, d := range s.local {
		out = append(out, d)
	}
	return out
}

func (s *Service) peerDrivers() map[fleet.MachineId]driver.Driver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[fleet.MachineId]driver.Driver, len(s.peers))
	for m, d := range s.peers {
		out[m] = d
	}
	return out
}

// effectiveDeadline is api-http.md §3.3's rule: "a req may shorten a
// driver's declared deadlineMs, never extend it."
func effectiveDeadline(driverMs int64, req time.Duration) time.Duration {
	d := time.Duration(driverMs) * time.Millisecond
	if req > 0 && req < d {
		return req
	}
	return d
}

// ListSessions answers GET /v1/sessions (api-http.md §3.2). ScopeLocal
// never touches peers (§13.1); ScopeFleet asks every local driver plus
// every peer driver and merges the results into one envelope.
func (s *Service) ListSessions(ctx context.Context, req fleet.Request, scope Scope, filter driver.ListFilter, callerDeadline time.Duration) (fleet.Collection[fleet.Session], error) {
	var items []fleet.Session
	var sources []fleet.SourceStatus

	// Local drivers are walked with their runtime, because a session's labels
	// are keyed by (runtime, id) — muster #153.
	s.mu.RLock()
	local := make(map[fleet.RuntimeId]driver.Driver, len(s.local))
	for rt, d := range s.local {
		local[rt] = d
	}
	pending := s.retryingRuntimesLocked()
	s.mu.RUnlock()
	for rt, d := range local {
		listedAt := time.Now()
		its, srcs := s.callList(ctx, req, s.self, d, filter, callerDeadline)
		// Only a complete, unfiltered answer may prune: anything narrower
		// proves nothing about which sessions are gone (§5.7).
		complete := filter.IsZero() && allSourcesOK(srcs)
		if complete {
			s.labels.retain(rt, its, listedAt)
		}
		// muster #235: a driver capable of it already removed these
		// sessions from its runtime, in the same call that just answered —
		// an exact, positive fact that holds whether or not THIS particular
		// read was filtered, unlike absence, which only a complete read may
		// conclude (§5.7). So this drains on every call, filtered or not.
		var exits []driver.CapturedExit
		if er, ok := d.(driver.ExitReporter); ok {
			exits = er.DrainExits()
		}
		// Closed-session records (#179) take the same rule: only a complete
		// answer may end anything by absence, but every answer is a
		// sighting, and a captured exit ends something outright.
		s.history.observe(rt, its, complete, listedAt, exits)
		for i := range its {
			its[i].Labels = s.labels.get(rt, its[i].ID, its[i].StartedAt)
		}
		items = append(items, its...)
		sources = append(sources, srcs...)
	}

	// An enabled runtime still being retried has no driver, so nothing above
	// asked it — and whatever sessions it keeps are not in `items`. Saying so
	// is this service's job: a driver cannot report on a runtime that has not
	// registered, and without a source here the envelope reads as a complete
	// answer from a machine with nothing more to say (muster #300).
	sources = append(sources, pendingRuntimeSources(s.self, pending, time.Now())...)

	if scope == ScopeFleet {
		for machine, d := range s.peerDrivers() {
			its, srcs := s.callList(ctx, req, machine, d, filter, callerDeadline)
			items = append(items, its...)
			sources = append(sources, srcs...)
		}
	}

	// Applied to EVERY item, peers' included. A peer on a build that
	// predates labels ignores the parameter and answers with everything it
	// has; the remote driver already turns that into a degraded source, and
	// filtering again here is what keeps any such answer from ever being
	// counted as a match.
	if len(filter.Labels) > 0 {
		kept := items[:0]
		for _, it := range items {
			if fleet.MatchesLabels(it.Labels, filter.Labels) {
				kept = append(kept, it)
			}
		}
		items = kept
	}

	if len(sources) == 0 {
		// No drivers registered at all for this scope. Still must not
		// return a bare, sourceless Collection (§9) — the service itself
		// is the one source, honestly answering "nothing here."
		sources = []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: time.Now()}}
	}

	return fleet.NewCollection(items, sources)
}

// allSourcesOK reports whether a listing's sources all answered ok.
func allSourcesOK(srcs []fleet.SourceStatus) bool {
	if len(srcs) == 0 {
		return false
	}
	for _, src := range srcs {
		if src.Status != fleet.SourceOK {
			return false
		}
	}
	return true
}

// publishLabels announces a session's complete label map (session.labels,
// muster #153).
func (s *Service) publishLabels(machine fleet.MachineId, sess fleet.Session) {
	ref := sess.SessionRef
	if ref.Machine == "" {
		ref.Machine = machine
	}
	// The labels frame names the session and not its directory, and can be
	// published before the driver's own session.created arrives. This is a
	// whole session, so say where it is.
	s.events.observeSessions(machine, []fleet.Session{sess})
	s.events.publish(fleet.Event{
		Machine: machine,
		Kind:    fleet.EventSessionLabels,
		Payload: fleet.SessionLabelsPayload{
			Ref: ref, StartedAt: sess.StartedAt, Labels: fleet.CopyLabels(sess.Labels),
		},
	})
}

// callList invokes one driver's List and folds its result into the
// aggregate envelope ListSessions is building.
//
// A local driver's own Collection already carries a SourceStatus (see
// driver.Driver.List's doc comment); for a peer's remote driver, that
// inner SourceStatus is the peer's own self-report and is adopted
// verbatim here — never re-synthesized as a fresh "ok" (§13.2). This
// function only manufactures a SourceStatus itself for the case a driver
// returns a bare Go error instead of ever reaching a Collection: deadline
// exceeded, unsupported, or anything else that short-circuited before
// building an envelope of its own.
func (s *Service) callList(ctx context.Context, req fleet.Request, machine fleet.MachineId, d driver.Driver, filter driver.ListFilter, callerDeadline time.Duration) ([]fleet.Session, []fleet.SourceStatus) {
	deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
	callCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	col, err := d.List(callCtx, req, filter)
	now := time.Now()

	if err == nil {
		// Every listing is a sighting of (machine, id, cwd), whatever filter it
		// was asked with; the event plane needs the directory to apply a
		// cwdPrefix to frames that do not carry one (cwdindex.go).
		s.events.observeSessions(machine, col.Items())
		if len(col.Sources()) > 0 {
			return col.Items(), col.Sources()
		}
		// Belt-and-braces: NewCollection already makes a sourceless
		// Collection hard to construct, but a driver could still return
		// one out of Collection[T]{}'s zero value without going through
		// it. Treat that as this driver's own report rather than
		// silently dropping it.
		return col.Items(), []fleet.SourceStatus{{Machine: machine, Status: fleet.SourceOK, ObservedAt: now}}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return nil, []fleet.SourceStatus{{
			Machine:    machine,
			Status:     fleet.SourceUnreachable,
			Error:      fmt.Sprintf("no answer within %s", deadline),
			ObservedAt: now,
		}}
	}

	// Anything else — including driver.ErrUnsupported — means this source
	// answered, just not usefully. SourceState's closed set (§9) has no
	// member for "reachable but this operation isn't supported"; degraded
	// is the nearest honest fit. Recorded as a finding (see the root
	// package's doc comment), not silently decided.
	return nil, []fleet.SourceStatus{{
		Machine:    machine,
		Status:     fleet.SourceDegraded,
		Error:      err.Error(),
		ObservedAt: now,
	}}
}

// ListMachines answers GET /v1/machines (api-http.md §3.1).
//
// The Driver interface (§3) has no health/ping operation — List is the
// cheapest existing operation to reuse as a liveness probe: if a driver
// answers within its deadline at all, whether with sessions or with
// ErrUnsupported, the machine it fronts is reachable. This is a real,
// working mechanism (not a fabricated one) built from an operation the
// interface already has, and it is a genuine limitation: a driver that
// always errs fast still reads as reachable here, because reachability and
// capability are different facts and this endpoint only ever asks about
// the first. See the root package's doc comment findings list.
func (s *Service) ListMachines(ctx context.Context, req fleet.Request, callerDeadline time.Duration) (fleet.Collection[fleet.MachineInfo], error) {
	now := time.Now()
	items := []fleet.MachineInfo{{
		Machine: s.self, Self: true, Status: fleet.SourceOK, ObservedAt: now,
		Build: s.build, MaxInputBytes: s.MaxInputBytes(), Peer: s.selfStanding(),
	}}
	sources := []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: now}}

	for machine, d := range s.peerDrivers() {
		status := fleet.SourceOK
		errText := ""
		// A peer that has already stopped answering is reported from what
		// its driver remembers, never dialled again on this request's path
		// (muster #237): the answer is already known, and asking would
		// cost the full deadline to learn it a second time.
		if down, ok := peerDownOf(d); ok {
			status = fleet.SourceUnreachable
			errText = down
		} else {
			deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
			callCtx, cancel := context.WithTimeout(ctx, deadline)
			col, err := d.List(callCtx, req, driver.ListFilter{})
			cancel()
			switch {
			case err != nil && errors.Is(err, context.DeadlineExceeded):
				status = fleet.SourceUnreachable
				errText = fmt.Sprintf("no answer within %s", deadline)
			case err == nil:
				// A remote driver folds "the peer did not answer" into the
				// envelope's own sources rather than returning an error, so
				// a nil error is not proof of contact. Reading the sources is
				// what keeps an unreachable peer from being listed as ok.
				for _, src := range col.Sources() {
					if src.Machine == machine && src.Status == fleet.SourceUnreachable {
						status, errText = fleet.SourceUnreachable, src.Error
						break
					}
				}
			}
		}
		observed := time.Now()
		// A peer's build is whatever the last successful probe learned —
		// driver.BuildReporter, not a fresh call on this request's path (the
		// probe that populates it rides RefreshCapabilities/reconcile, same
		// as Runtime()). A driver that never implements it — every local
		// driver, and any peer this service has never reached — reports the
		// zero value, which is Known: false: muster #121 requires this
		// read as unknown, never as a default that looks like an answer.
		build := fleet.Build{}
		if reporter, ok := d.(driver.BuildReporter); ok {
			build = reporter.Build()
		}
		// A peer's effective input limit, learned the same way and stale in
		// the same manner as its build (muster #130): a driver that
		// has never implemented or never probed this reports the zero
		// value, which — unlike Build's Known flag — needs no separate
		// marker, because a real effective limit is never zero (see
		// MachineInfo.MaxInputBytes).
		var maxInputBytes int
		if reporter, ok := d.(driver.MaxInputBytesReporter); ok {
			maxInputBytes = reporter.MaxInputBytes()
		}
		// This service's own standing there, learned on the same probe
		// (muster #154). A driver that cannot report one reads as the
		// floor — assumed, never "not listed".
		peer := fleet.AssumedPeerStanding()
		if reporter, ok := d.(driver.PeerStandingReporter); ok {
			peer = reporter.PeerStanding()
		}
		address := ""
		if reporter, ok := d.(driver.AddressReporter); ok {
			if a, ok := reporter.Address(); ok {
				address = a
			}
		}
		items = append(items, fleet.MachineInfo{
			Machine: machine, Self: false, Status: status, ObservedAt: observed,
			Build: build, MaxInputBytes: maxInputBytes, Address: address, Peer: peer,
		})
		sources = append(sources, fleet.SourceStatus{Machine: machine, Status: status, Error: errText, ObservedAt: observed})
	}

	return fleet.NewCollection(items, sources)
}

// peerDownOf reports, from a driver's own memory and without a network call,
// whether the peer it fronts is marked down, with the text to show for it.
func peerDownOf(d driver.Driver) (string, bool) {
	r, ok := d.(driver.PeerDownReporter)
	if !ok {
		return "", false
	}
	down, since, failures := r.PeerDown()
	if !down {
		return "", false
	}
	return fmt.Sprintf("marked down: no answer since %s (%d consecutive failures); re-probing in the background",
		since.UTC().Format(time.RFC3339), failures), true
}

// ListRuntimes answers GET /v1/runtimes (api-http.md §3.1), local drivers
// only — the wire example does not show a scope parameter for this
// endpoint, and this skeleton does not invent fanning it out to peers.
func (s *Service) ListRuntimes(ctx context.Context) (fleet.Collection[fleet.RuntimeInfo], error) {
	now := time.Now()
	var items []fleet.RuntimeInfo
	sources := []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: now}}

	// A registered driver whose agent program is missing is listed after the
	// ones that work, with its cause (muster #342).
	// Enabled runtimes with no driver follow the registered ones (muster
	// #297); localRuntimeRows owns that order.
	items = append(items, s.localRuntimeRows(ctx)...)
	s.mu.RLock()
	peers := make(map[fleet.MachineId]driver.Driver, len(s.peers))
	for m, d := range s.peers {
		peers[m] = d
	}
	s.mu.RUnlock()

	// Peers are included, and they are the whole reason this endpoint
	// matters. api-http.md §3.1 tells clients they MUST consult this before
	// relying on a capability — a rule that was unfollowable for peer
	// runtimes while this listed only local ones, which is exactly the case
	// where a caller cannot simply know.
	//
	// No network call is made. A peer driver answers from whatever it has
	// cached, and says so: §4.3's `source` reports `assumed` when nobody has
	// told it anything and `observed` when the peer has. That distinction is
	// what makes reporting a cache honest rather than misleading, and it is
	// why this endpoint can stay cheap.
	for m, d := range peers {
		caps := d.Capabilities()
		var rt fleet.RuntimeId
		if r, ok := d.(interface{ Runtime() fleet.RuntimeId }); ok {
			rt = r.Runtime()
		}
		st := fleet.SourceOK
		if caps.Source != fleet.CapabilitiesObserved {
			// Nothing was reached to produce this row. §5.7: that is not
			// the same as a peer that answered, and the envelope must not
			// present it as one.
			st = fleet.SourceDegraded
		}
		// muster #67: the fallback row for a peer nobody has heard
		// from — `source: assumed` with no runtime learned yet — used to be
		// reported anyway, empty runtime id and all, as a placeholder
		// nobody removed. That is the one option that misleads a client
		// keying its capability table by (machine, runtime): it reads as a
		// runtime literally named "" on that machine, rather than as "no
		// runtime known there yet". The degraded SourceStatus below already
		// says "nothing is known" about this peer; omitting the item row
		// instead of naming a runtime that was never learned is the option
		// that does not invent a fact. A peer that HAS answered but
		// happens to report an empty runtime id is a different, genuine
		// case this endpoint has no basis to hide.
		if rt != "" || st == fleet.SourceOK {
			row := fleet.RuntimeInfo{Machine: m, Runtime: rt, Capabilities: caps, Status: fleet.RuntimeAvailable}
			if caps.Source != fleet.CapabilitiesAssumed {
				ps := fleet.PostureSupportFrom(caps)
				row.Posture = &ps
			}
			items = append(items, row)
		}
		sources = append(sources, fleet.SourceStatus{
			Machine: m, Status: st, ObservedAt: now,
			Error: map[bool]string{true: "", false: "capabilities not yet obtained from this peer"}[st == fleet.SourceOK],
		})
	}
	return fleet.NewCollection(items, sources)
}

// driverSummary is one row of GET /v1/health's `drivers`: an enabled runtime
// on this machine. Status, Cause and Reason mean what they mean on a
// GET /v1/runtimes row (muster #297, #300): an enabled runtime with no working
// driver is listed too, with its cause, rather than being absent — absence on
// this surface reads as "never enabled".
type driverSummary struct {
	Machine      fleet.MachineId          `json:"machine"`
	Runtime      fleet.RuntimeId          `json:"runtime"`
	Capabilities fleet.DriverCapabilities `json:"capabilities"`
	Status       fleet.RuntimeStatus      `json:"status,omitempty"`
	Cause        fleet.UnavailableCause   `json:"cause,omitempty"`
	Reason       string                   `json:"reason,omitempty"`
}

// driverSummaries lists registered drivers first, then enabled runtimes with
// no driver, each group in runtime-id order — the order GET /v1/runtimes uses,
// so a reader that takes the first row never takes an unavailable one.
func (s *Service) driverSummaries(ctx context.Context) []driverSummary {
	rows := s.localRuntimeRows(ctx)
	out := make([]driverSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, driverSummary{
			Machine: row.Machine, Runtime: row.Runtime, Capabilities: row.Capabilities,
			Status: row.Status, Cause: row.Cause, Reason: row.Reason,
		})
	}
	return out
}

// counterSnapshot answers #9: a driver's own counters, read through
// driver.CounterReporter, keyed by runtime rather than merged into one flat
// map. Two local drivers are already possible (RegisterLocalDriver has no
// limit of one), and a name collision between them silently merging two
// different facts into one count would be a worse bug than the one this
// registry exists to prevent — see counters.go's own reasoning for why a
// count that cannot be told apart from another is not a count.
//
// A driver that does not implement the interface is left out entirely,
// never given a zeroed entry: an empty map would claim "nothing happened"
// about a driver that was never asked, which is not the same statement as
// #9's own distinction between "not measured" and "cannot be measured".
func (s *Service) counterSnapshot() map[fleet.RuntimeId]map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[fleet.RuntimeId]map[string]int64, len(s.local))
	for rt, d := range s.local {
		reporter, ok := d.(driver.CounterReporter)
		if !ok {
			continue
		}
		out[rt] = reporter.Counters()
	}
	return out
}

// ErrAmbiguousSession documents the finding recorded in the root package's
// doc comment: the single-session URL shape (api-http.md §3.3) carries no
// runtime segment, but SessionRef.ID is scoped to (machine, runtime), not
// machine alone (session-abstraction.md §2.2). Two runtimes on one machine
// can legally reuse the same id.
//
// It is also the OLDER behaviour a configured default runtime exists to
// relieve callers of (muster issue #60): absent a default, this is
// still exactly what a caller gets whenever resolution genuinely cannot
// name one driver — the config file's own "nothing here has a default; an
// absent value means the older behaviour" carried one level down.
var ErrAmbiguousSession = errors.New("service: more than one local runtime is registered; pass ?runtime= to disambiguate (api-http.md §3.3)")

// runtimeResolution records HOW resolveSessionDriver picked a driver, so a
// caller that named its own runtime and a caller that got the machine's
// configured default are not rendered alike (§5.7; muster issue #60
// guardrail 2). Only resolvedDefault is a genuine guess wearing a
// configuration's authority — the other three are exactly as trustworthy as
// a caller naming its own runtime, because each is either the caller's own
// word or a fact this machine just confirmed.
type runtimeResolution string

const (
	resolvedExplicit  runtimeResolution = "explicit"  // caller passed ?runtime=
	resolvedSole      runtimeResolution = "sole"      // exactly one local driver is registered at all
	resolvedExistence runtimeResolution = "existence" // exactly one registered driver affirmed it holds this id
	resolvedDefault   runtimeResolution = "default"   // configured default, used only as a genuine-miss tiebreak
	resolvedPeer      runtimeResolution = "peer"      // routed to a peer machine — the default never applies (§13.1)
)

// resolveSessionDriver finds the Driver responsible for an existing or
// about-to-be-created session addressed by (machine, id, runtimeHint).
//
// id is the session's id when one is already being addressed — every
// endpoint but create. It is empty for create, where nothing exists yet to
// search local drivers FOR. Threading it through is what makes
// existence-first resolution possible at all: a version of this function
// that never saw the id (the shape this had before muster issue #60)
// had nothing to check registered drivers against, and could only guess or
// refuse.
//
//   - machine == a configured peer: that peer's single remote driver
//     handles it — the peer resolves its own runtimes locally, and this
//     service never recurses into asking the peer to disambiguate (§13.1).
//     The machine-local default configured here NEVER reaches this branch:
//     applying it to a peer would make one bare id mean different sessions
//     depending on which machine answered, exactly the fork §7.1's
//     (machine, id) addressing exists to prevent.
//
//   - machine == self, runtimeHint given: routes directly to that local
//     driver. The caller named its runtime; nothing here second-guesses it.
//
//   - machine == self, runtimeHint empty, exactly one local driver
//     registered: that driver, unambiguously — the case that held for this
//     whole repository's life before a second local driver existed to
//     register.
//
//   - machine == self, runtimeHint empty, more than one local driver
//     registered: EXISTENCE FIRST, DEFAULT AS TIEBREAK (muster #60).
//     A nonempty id is probed against every registered local driver's own
//     State() — ErrNoSuchSession is a driver's affirmative "I have never
//     had this id" (errors.go) — and:
//
//     exactly one driver affirms it     -> that driver, full stop,
//     REGARDLESS of the configured
//     default (guardrail 3: a default
//     naming runtime A must never
//     steer a bare id that plainly
//     belongs to runtime B into a
//     false not_found).
//     more than one driver affirms it   -> refused, naming every runtime
//     that claims the id. §5.4
//     already requires consensus
//     before acting on a recycled id;
//     two runtimes claiming the same
//     one is exactly that case, and it
//     is surfaced rather than
//     silently resolved.
//     any driver's probe is inconclusive
//     (fails for a reason other than
//     "never had this id") and nothing
//     else affirms it                   -> refused rather than guessed. An
//     inconclusive driver might be the
//     one actually holding the id, and
//     defaulting past it is the same
//     false-absence guardrail 3
//     forbids.
//     every driver affirmatively
//     confirms absence                  -> a genuine miss, the same shape
//     create's own ambiguous case has
//     (there is equally nothing here
//     to route TO) — falls to the
//     configured default, or
//     ErrAmbiguousSession absent one.
//
//     id == "" (create) skips the probe outright and goes straight to that
//     same genuine-miss handling: the configured default when ambiguous, or
//     ErrAmbiguousSession absent one.
func (s *Service) resolveSessionDriver(ctx context.Context, req fleet.Request, machine fleet.MachineId, id string, runtimeHint fleet.RuntimeId, callerDeadline time.Duration) (driver.Driver, fleet.RuntimeId, runtimeResolution, *fleet.Error) {
	if machine != s.self && machine != "" {
		s.mu.RLock()
		d, ok := s.peers[machine]
		s.mu.RUnlock()
		if !ok {
			return nil, "", "", &fleet.Error{Kind: fleet.ErrorNotFound, Message: "unknown machine", Machine: machine}
		}
		return d, "", resolvedPeer, nil
	}

	s.mu.RLock()
	if runtimeHint != "" {
		d, ok := s.local[runtimeHint]
		u, down := s.unavailable[runtimeHint]
		s.mu.RUnlock()
		if !ok {
			if down {
				// Enabled, and not usable right now: say so, rather than the
				// "unknown runtime" an id nobody configured gets (muster #297).
				return nil, "", "", unavailableError(machine, runtimeHint, u)
			}
			return nil, "", "", &fleet.Error{Kind: fleet.ErrorNotFound, Message: "unknown runtime", Machine: machine}
		}
		return d, runtimeHint, resolvedExplicit, nil
	}

	// Snapshot under the lock, then release it before any driver call — a
	// probe below may reach a real substrate, and holding this mutex across
	// that would block every other registration and lookup for as long as
	// the slowest candidate driver takes to answer.
	local := make(map[fleet.RuntimeId]driver.Driver, len(s.local))
	for rt, d := range s.local {
		local[rt] = d
	}
	defaultRuntime := s.defaultRuntime
	// Enabled runtimes still being retried count as candidates (muster #297):
	// they cannot be asked and cannot be routed to yet, but routing as if they
	// did not exist would send every create to the one that happens to be up,
	// and move bare ids elsewhere the moment the late runtime answers.
	pending := s.retryingRuntimesLocked()
	s.mu.RUnlock()

	switch {
	case len(local) == 0 && len(pending) == 0:
		return nil, "", "", &fleet.Error{Kind: fleet.ErrorNotFound, Message: "no local drivers registered", Machine: machine}
	case len(local) == 1 && len(pending) == 0:
		for rt, d := range local {
			return d, rt, resolvedSole, nil
		}
	case len(local) == 0:
		for rt, u := range pending {
			return nil, "", "", unavailableError(machine, rt, u)
		}
	}

	// More than one local driver, and the caller named none. A nonempty id
	// means an existing session is being addressed, so it is resolved
	// existence-first before the default is ever consulted.
	if id != "" {
		holders, inconclusive := s.probeHolders(ctx, req, local, id, callerDeadline)
		for rt := range pending {
			// Cannot be asked: not evidence of absence (§7.1a guardrail 3).
			inconclusive[rt] = nil
		}
		switch {
		case len(holders) == 1:
			for rt, d := range holders {
				return d, rt, resolvedExistence, nil
			}
		case len(holders) > 1:
			return nil, "", "", &fleet.Error{
				Kind: fleet.ErrorInvalid,
				Message: "session id is present under more than one local runtime (" +
					strings.Join(runtimeNames(holders), ", ") +
					"); pass ?runtime= to disambiguate (§5.4)",
				Machine: machine,
			}
		case len(inconclusive) > 0:
			return nil, "", "", &fleet.Error{
				Kind: fleet.ErrorInvalid,
				Message: "more than one local runtime is registered and existence could not be " +
					"confirmed against all of them (" + strings.Join(runtimeNames(inconclusive), ", ") +
					"); pass ?runtime= to disambiguate (api-http.md §3.3)",
				Machine: machine,
			}
			// else: confirmed absent from every local driver — a genuine
			// miss, falls through to the same handling create's own
			// ambiguous case gets, below.
		}
	}

	if defaultRuntime != "" {
		if d, ok := local[defaultRuntime]; ok {
			return d, defaultRuntime, resolvedDefault, nil
		}
		if u, ok := pending[defaultRuntime]; ok {
			return nil, "", "", unavailableError(machine, defaultRuntime, u)
		}
		// SetDefaultRuntime refuses a value that does not name a
		// registered driver, so this is defensive rather than reachable in
		// practice.
		return nil, "", "", &fleet.Error{Kind: fleet.ErrorNotFound, Message: "configured default runtime is not registered", Machine: machine}
	}

	return nil, "", "", &fleet.Error{Kind: fleet.ErrorInvalid, Message: ErrAmbiguousSession.Error(), Machine: machine}
}

// probeHolders asks every candidate local driver, individually, whether it
// has ever had id — the affirmative signal ErrNoSuchSession's own doc
// comment names ("a read whose id the machine has never had"). Run with the
// service lock already released by the caller; a probe may reach a real
// substrate.
//
// Returns two disjoint sets. holders affirmatively HAVE the id — their
// State() succeeded. inconclusive could not be asked at all: State() failed
// for a reason OTHER than "never had it" AND other than "can never have it"
// (unreachable, past its deadline, or any other transient failure).
// resolveSessionDriver treats those very differently — see its own doc
// comment — because an inconclusive driver is not evidence of absence,
// only evidence that this probe could not reach a verdict.
//
// A driver answering driver.ErrUnsupported is neither: it is not affirming
// the id (it has no notion of "having" anything), and it is not merely
// unreachable right now — ErrUnsupported's own doc comment (internal/driver)
// says this is a statement about the SUBSTRATE, permanent, true again on
// retry. muster issue #63 measured what conflating the two costs: one
// runtime being temporarily unreachable poisoned resolution for every
// runtime on the machine, including ones that structurally never held
// anything and never will. Such a driver is folded into the same "not this
// one" outcome as ErrNoSuchSession, leaving inconclusive to mean only what
// #60 built it to mean.
//
// This is read from the returned sentinel rather than from
// fleet.DriverCapabilities, and that is a real gap rather than a
// preference: DriverCapabilities has no field for "this driver can never
// hold a session". ObservesState looks like a candidate but is not one — it
// is false for the tmux driver too, where it describes fidelity (inferred
// vs. observed) on a driver whose State() answers plenty of ids, never
// "this operation always fails here". Nothing on DriverCapabilities
// currently distinguishes a driver that answers from one that cannot
// participate in this operation at all, so there is no honest way to know
// this before the call. Adding such a field is future work, out of #63's
// scope, and would let this branch be removed at that call site instead of
// only at this comment.
func (s *Service) probeHolders(ctx context.Context, req fleet.Request, local map[fleet.RuntimeId]driver.Driver, id string, callerDeadline time.Duration) (holders, inconclusive map[fleet.RuntimeId]driver.Driver) {
	holders = make(map[fleet.RuntimeId]driver.Driver)
	inconclusive = make(map[fleet.RuntimeId]driver.Driver)
	ref := fleet.SessionRef{Machine: s.self, ID: id}
	for rt, d := range local {
		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
		callCtx, cancel := context.WithTimeout(ctx, deadline)
		_, err := d.State(callCtx, req, ref)
		cancel()
		switch {
		case err == nil:
			holders[rt] = d
		case errors.Is(err, fleet.ErrNoSuchSession):
			// Affirmatively absent from this driver — belongs to neither
			// set; §5.7's "absence and failure are different answers"
			// applied to a routing decision instead of a read.
		case errors.Is(err, driver.ErrUnsupported):
			// Structurally incapable of ever holding a session (see the
			// doc comment above) — the firm "not mine" ErrNoSuchSession
			// would have meant, had this driver a notion of sessions to
			// have never had one of. Belongs to neither set, same as the
			// case above and for the same reason.
		default:
			inconclusive[rt] = d
		}
	}
	return holders, inconclusive
}

// runtimeNames returns the sorted runtime ids of m, for an error message a
// caller can act on without depending on Go map iteration order.
func runtimeNames(m map[fleet.RuntimeId]driver.Driver) []string {
	out := make([]string, 0, len(m))
	for rt := range m {
		out = append(out, string(rt))
	}
	sort.Strings(out)
	return out
}
