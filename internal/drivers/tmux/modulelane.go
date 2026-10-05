package tmux

// Optional external delivery modules (#185): this driver's half.
//
// The seam (internal/delivery), the child-process client (modclient) and the
// wire protocol are documented where they live. This file is what only THIS
// driver can know: which sessions it launched itself, what lane each one
// holds, and what to do with a lane across a session's whole life — create,
// rename, restart, close.
//
// # One lane per session, chosen at create
//
// A module can only be offered to a session this driver LAUNCHES: the module's
// environment has to be in the agent process before it starts (prepare-launch
// returns it; it rides the staged-env file, never a command line). So a lane is
// decided once, at create, by asking the enabled modules in the operator's
// order of preference and taking the first that answers. A session created
// while no module was enabled — or adopted, or launched by someone else — has
// no record at all and stays on the built-in lane, which is not an error.
//
// # The record is durable, the answer is not
//
// The record ({module, laneKey, pid}) survives a restart, because the module
// keeps its own on-disk state and can re-attach to it. Whether the lane is LIVE
// is never persisted as a fact: it is recomputed from the module's current
// health, the lane's last attach result, and whether that attach happened
// under the module process that is running now (its generation). A module
// that restarted has forgotten every connection; until each lane is
// re-attached, it is not live.
//
// # Lock order
//
// d.mu, then moduleHost.mu. List reads a session's lane while holding d.mu.
// Nothing here may take d.mu while holding moduleHost.mu.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/delivery/modclient"
	"github.com/futurelastic/muster/internal/driver"
)

const (
	lanesFileName = "delivery-lanes"

	defaultAttachWait       = 10 * time.Second
	defaultAttachWindow     = 2 * time.Minute
	defaultAttachRetryEvery = 2 * time.Second
	defaultLaneTick         = 30 * time.Second

	// maxLaneKeyBytes bounds the opaque handle a module returns. It is stored,
	// logged nowhere and put in no path; the bound only keeps a hostile or
	// broken module from growing the state file.
	maxLaneKeyBytes = 128
	// maxModuleEnvValueBytes bounds one environment value a module asks to be
	// set. They are paths; the staged-env file is line-oriented.
	maxModuleEnvValueBytes = 4096
	maxModuleEnvEntries    = 32
)

// Lane states a module reports (spec §4.1). Anything else is read as not live.
const (
	laneStatePrepared   = "prepared"
	laneStateConnecting = "connecting"
	laneStateLive       = "live"
	laneStateDegraded   = "degraded"
	laneStateGone       = "gone"
)

// Attach reasons that mean "the agent has not written its record yet" — a
// first-run trust dialog can hold it back for a long time — so the attach is
// polled rather than given up on.
var retryableAttachReasons = map[string]bool{
	"session-record-missing": true,
	"socket-missing":         true,
}

const (
	// attachRefusedClass is the module error code for an attach it will not
	// take: the lane is bound to a different process than the one offered.
	attachRefusedClass = "refused"
	// laneReasonReplaced is the reason a lane is marked gone when its agent
	// process was replaced behind it.
	laneReasonReplaced = "agent process replaced"
)

// ModulesConfig configures the external delivery modules of one driver.
type ModulesConfig struct {
	// Enabled is every module name the operator enabled, in order of
	// preference — installed or not. A name with no matching Clients entry is
	// enabled but absent: a session cannot be given its lane, a forced route to
	// it is refused, and it reserves whatever prefixes were recorded the last
	// time it ran.
	Enabled []string

	// Clients holds one configuration per enabled module whose executable was
	// found. Counters, logging and the ready callback are supplied by the
	// driver; a caller sets Name, Path, Env and, in tests, the Launcher and the
	// timings.
	Clients []modclient.Config

	// AttachWait is how long one attach asks the module to wait for the agent
	// to come up. AttachWindow bounds the whole poll for a fresh session;
	// AttachRetryEvery paces it. LaneTick paces the background pass that
	// re-attaches degraded lanes and closes lanes whose session is gone.
	AttachWait, AttachWindow, AttachRetryEvery, LaneTick time.Duration

	// Logf is the driver's logger; nil discards.
	Logf func(format string, args ...any)
}

// WithDeliveryModules enables external delivery modules for this driver. A
// config naming none is the same as not calling it.
func WithDeliveryModules(cfg ModulesConfig) Option {
	return func(d *Driver) { d.modCfg = &cfg }
}

// laneRecord is one session's lane. The persisted fields are the durable
// half; the unexported ones are recomputed and never trusted across a restart.
type laneRecord struct {
	// Module is the module that owns the lane, or "" when the session was
	// launched while modules were configured but none could take it — the
	// record then exists only to say why.
	Module  string `json:"module"`
	LaneKey string `json:"laneKey,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Cwd     string `json:"cwd,omitempty"`

	// State is the lane state the module last reported; Live is its last
	// attach answer. Neither says the lane is usable NOW — see usableLocked.
	State        string `json:"state,omitempty"`
	Live         bool   `json:"live,omitempty"`
	PeerVerified *bool  `json:"peerVerified,omitempty"`

	// Reason is why the session is not (yet) on the module: a prepare-launch
	// refusal, an attach reason, a degradation. Prose from an untrusted
	// module is sanitised before it lands here.
	Reason string `json:"reason,omitempty"`

	// Surfaced is the lane the session read as when it last changed, and
	// Since is when — the DeliveryLane the read path publishes.
	Surfaced string    `json:"surfaced,omitempty"`
	Since    time.Time `json:"since,omitempty"`

	// Closing marks a lane whose session is gone (or being closed) and whose
	// `close` the module has not acknowledged yet. It is flushed the next time
	// the module is ready, so a module that was down at teardown still hears
	// about it.
	Closing bool `json:"closing,omitempty"`

	// gen is the module-process generation the last successful attach ran
	// under; zero after a restart of this service. attaching stops two polls
	// running for one session.
	gen       uint64
	attaching bool
	// stalls counts consecutive sends the module saw enqueued on an idle session
	// without a turn ever starting (#264). In memory only: a restart of this
	// service starts the count again, which errs toward trying the lane once more.
	// A confirmed send or a fresh live attach resets it.
	stalls int
	// flushing marks a `close` in flight, so the several places that can
	// notice a closing lane (the close itself, the ready callback, the
	// background pass) send it once, not once each.
	flushing bool
}

type lanesFile struct {
	Lanes map[string]*laneRecord `json:"lanes"`
	// Prefixes are the environment prefixes each module declared it reserves,
	// remembered so the reserved-env guard holds on a machine where the module
	// is not installed or not running right now (spec §5.1 step 1).
	Prefixes map[string][]string `json:"prefixes,omitempty"`
}

// moduleHost owns every enabled module's client and every session's lane.
type moduleHost struct {
	d       *Driver
	cfg     ModulesConfig
	order   []string
	clients map[string]*modclient.Client

	mu       sync.Mutex
	lanes    map[string]*laneRecord
	prefixes map[string][]string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	tickCh chan struct{}
}

// initModules builds the host after every option has run, so a configured state
// store and clock are both in effect. A driver with no modules keeps a nil host
// and every entry point below is a no-op on nil: byte-for-byte what it was.
func (d *Driver) initModules() {
	cfg := d.modCfg
	if cfg == nil || len(cfg.Enabled) == 0 {
		return
	}
	h := &moduleHost{
		d:        d,
		cfg:      *cfg,
		clients:  map[string]*modclient.Client{},
		lanes:    map[string]*laneRecord{},
		prefixes: map[string][]string{},
		tickCh:   make(chan struct{}, 1),
	}
	if h.cfg.AttachWait <= 0 {
		h.cfg.AttachWait = defaultAttachWait
	}
	if h.cfg.AttachWindow <= 0 {
		h.cfg.AttachWindow = defaultAttachWindow
	}
	if h.cfg.AttachRetryEvery <= 0 {
		h.cfg.AttachRetryEvery = defaultAttachRetryEvery
	}
	if h.cfg.LaneTick <= 0 {
		h.cfg.LaneTick = defaultLaneTick
	}
	seen := map[string]bool{}
	for _, n := range cfg.Enabled {
		if delivery.ValidModuleName(n) && !seen[n] {
			seen[n] = true
			h.order = append(h.order, n)
		}
	}
	if len(h.order) == 0 {
		return
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	var f lanesFile
	if d.store != nil {
		if _, err := d.store.Load(lanesFileName, &f); err != nil {
			h.logf("tmux: delivery lanes: %v (starting with none)", err)
			f = lanesFile{}
		}
	}
	for id, rec := range f.Lanes {
		if rec != nil {
			// Never trust a persisted "live": it says what the module said
			// under a process that may be gone.
			rec.Live, rec.State, rec.attaching = false, "", false
			h.lanes[id] = rec
		}
	}
	for m, p := range f.Prefixes {
		var kept []string
		for _, x := range p {
			if modclient.ValidReservedPrefix(x) {
				kept = append(kept, x)
			}
		}
		h.prefixes[m] = kept
	}
	for _, cc := range cfg.Clients {
		cc := cc
		if !delivery.ValidModuleName(cc.Name) || !seen[cc.Name] {
			continue
		}
		name := cc.Name
		cc.Count = func(n string) { d.counters.incr("module." + name + "." + n) }
		cc.Logf = h.cfg.Logf
		cc.OnReady = func(gen uint64) { h.onReady(name, gen) }
		h.clients[name] = modclient.New(cc)
	}
	d.mods = h
}

func (h *moduleHost) logf(format string, args ...any) {
	if h != nil && h.cfg.Logf != nil {
		h.cfg.Logf(format, args...)
	}
}

func (h *moduleHost) count(module, name string) {
	h.d.counters.incr("module." + module + "." + name)
}

// StartDeliveryModules starts every enabled module's child process and the
// background lane pass. It never blocks: a module that is slow to start costs
// nothing but a create that lands before it is ready, which takes the built-in
// lane with the reason recorded. Call it once, after Reconcile and
// ReconcileLanes.
func (d *Driver) StartDeliveryModules() {
	h := d.mods
	if h == nil {
		return
	}
	h.once.Do(func() {
		for _, name := range h.order {
			if c := h.clients[name]; c != nil {
				c.Start()
			}
		}
		h.wg.Add(1)
		go h.laneLoop()
	})
}

// StopDeliveryModules stops the modules and the lane pass. Closing a module's
// stdin makes it drop its connections but keep its on-disk state, so the next
// start can re-attach.
func (d *Driver) StopDeliveryModules() {
	h := d.mods
	if h == nil {
		return
	}
	h.cancel()
	for _, name := range h.order {
		if c := h.clients[name]; c != nil {
			c.Stop()
		}
	}
	h.wg.Wait()
}

// ReservedEnvPrefixes implements driver.ReservedEnvPrefixReporter: the prefixes
// every enabled module reserves, from what each declared when it last ran. It
// answers whether or not a module is running or even installed, because the
// guard it feeds must hold on every create.
func (d *Driver) ReservedEnvPrefixes() []string {
	h := d.mods
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	add := func(list []string) {
		for _, p := range list {
			if !seen[p] && modclient.ValidReservedPrefix(p) {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	for _, name := range h.order {
		add(h.prefixes[name])
		if c := h.clients[name]; c != nil {
			add(c.ReservedEnvPrefixes())
		}
	}
	sort.Strings(out)
	return out
}

var _ driver.ReservedEnvPrefixReporter = (*Driver)(nil)

// deliveryModuleNames lists the enabled module names, in preference order.
func (d *Driver) deliveryModuleEnabled(name string) bool {
	h := d.mods
	if h == nil {
		return false
	}
	for _, n := range h.order {
		if n == name {
			return true
		}
	}
	return false
}

// moduleStatuses is the capabilities view of the enabled modules.
func (d *Driver) moduleStatuses() []fleet.DeliveryModuleStatus {
	h := d.mods
	if h == nil {
		return nil
	}
	h.mu.Lock()
	counts := map[string]map[string]int{}
	for _, rec := range h.lanes {
		if rec.Module == "" || rec.Closing || rec.LaneKey == "" {
			continue
		}
		if counts[rec.Module] == nil {
			counts[rec.Module] = map[string]int{}
		}
		st := rec.State
		if st == "" {
			st = laneStatePrepared
		}
		counts[rec.Module][st]++
	}
	h.mu.Unlock()

	out := make([]fleet.DeliveryModuleStatus, 0, len(h.order))
	for _, name := range h.order {
		s := fleet.DeliveryModuleStatus{Name: name, Lanes: counts[name]}
		c := h.clients[name]
		if c == nil {
			s.Status = fleet.DeliveryModuleUnavailable
			s.Reason = "the module's executable is not installed on this machine"
			out = append(out, s)
			continue
		}
		st := c.Status()
		switch st.State {
		case modclient.StateAvailable:
			s.Status = fleet.DeliveryModuleAvailable
			if st.Suspect {
				s.Status = fleet.DeliveryModuleUnavailable
				s.Reason = "a request missed its deadline; waiting on a health probe"
			}
		case modclient.StateDisabled:
			s.Status = fleet.DeliveryModuleDisabled
			s.Reason = sanitizeModuleText(st.Reason, 200)
		case modclient.StateStarting:
			s.Status = fleet.DeliveryModuleStarting
		default:
			s.Status = fleet.DeliveryModuleUnavailable
			s.Reason = sanitizeModuleText(st.Reason, 200)
		}
		if st.Hello != nil {
			s.Protocol = st.Hello.Protocol
			s.Version = sanitizeModuleText(st.Hello.Version, 64)
		}
		if st.Health != nil {
			s.Platform = sanitizeModuleText(st.Health.Platform, 64)
			pc := st.Health.PeerCheck
			s.PeerCheck = &pc
			if s.Version == "" {
				s.Version = sanitizeModuleText(st.Health.Version, 64)
			}
		}
		out = append(out, s)
	}
	return out
}

// sanitizeModuleText makes a string a module produced safe to keep and to show:
// printable runes only, bounded. A module is a same-user local child, but its
// text is still input, never a path and never trusted.
func sanitizeModuleText(s string, max int) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError, r < 0x20, r == 0x7f:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
		if b.Len() >= max {
			break
		}
	}
	out := b.String()
	if len(out) > max {
		out = strings.ToValidUTF8(out[:max], "") + "…"
	}
	return out
}

// --- lane records --------------------------------------------------------

func (h *moduleHost) saveLocked() {
	if h.d.store == nil {
		return
	}
	f := lanesFile{Lanes: h.lanes, Prefixes: h.prefixes}
	if err := h.d.store.Save(lanesFileName, f); err != nil {
		h.logf("tmux: delivery lanes: saving: %v", err)
	}
}

func (h *moduleHost) snapshot(id string) (laneRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.lanes[id]
	if rec == nil {
		return laneRecord{}, false
	}
	return *rec, true
}

// peerOK reports whether the module has told us it can verify who is on the
// other end of its channel. The maintainer's ruling: a module reporting
// peerCheck:false is not live, whatever else it says. A module that has not
// answered health yet has not said, which is also not live.
func peerOK(c *modclient.Client) bool {
	hh, ok := c.LastHealth()
	return ok && hh.PeerCheck
}

// usableLocked says why the record's lane cannot carry a send right now, or ""
// when it can. Caller holds h.mu.
func (h *moduleHost) usableLocked(rec *laneRecord) string {
	switch {
	case rec.Module == "":
		if rec.Reason != "" {
			return rec.Reason
		}
		return "no delivery module holds a lane for this session"
	case rec.Closing:
		return "the session's lane is being closed"
	case rec.LaneKey == "":
		return "no lane was prepared for this session"
	}
	c := h.clients[rec.Module]
	if c == nil {
		return fmt.Sprintf("module %q is enabled but not installed on this machine", rec.Module)
	}
	if !c.Usable() {
		st := c.Status()
		why := string(st.State)
		if st.Suspect {
			why = "suspect (a request missed its deadline)"
		} else if st.Reason != "" {
			why += ": " + sanitizeModuleText(st.Reason, 120)
		}
		return fmt.Sprintf("module %q is not available (%s)", rec.Module, why)
	}
	if !peerOK(c) {
		return fmt.Sprintf("module %q reports it cannot verify its peer (peerCheck), so it is treated as not live", rec.Module)
	}
	if rec.PeerVerified != nil && !*rec.PeerVerified {
		return fmt.Sprintf("module %q reports the lane's peer was not verified", rec.Module)
	}
	if !rec.Live || rec.State == laneStateDegraded || rec.State == laneStateGone {
		st := rec.State
		if st == "" {
			st = "not attached"
		}
		if rec.Reason != "" {
			return fmt.Sprintf("the lane is %s: %s", st, rec.Reason)
		}
		return fmt.Sprintf("the lane is %s", st)
	}
	if rec.gen != c.Generation() {
		return fmt.Sprintf("module %q restarted and the lane has not been re-attached yet", rec.Module)
	}
	return ""
}

// laneView is the session's `delivery` field: nil when nothing is recorded,
// otherwise which lane an `auto` send would take right now and why.
func (h *moduleHost) laneView(id string) *fleet.DeliveryLane {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.lanes[id]
	if rec == nil {
		return nil
	}
	lane, evidence, connected := fleet.DeliveryLaneTerminal, "", false
	if why := h.usableLocked(rec); why == "" {
		lane, connected = rec.Module, true
		evidence = "the module reports the session's lane live"
	} else {
		evidence = why
	}
	if rec.Surfaced != lane || rec.Since.IsZero() {
		rec.Surfaced, rec.Since = lane, h.d.now()
		h.saveLocked()
	}
	return &fleet.DeliveryLane{
		Lane: lane, ClientConnected: connected,
		Evidence: sanitizeModuleText(evidence, 240), Since: rec.Since,
	}
}

// liveLane is the send path's question: may this session's next send go to a
// module, and which. name is the forced module, or "" for `auto`.
func (h *moduleHost) liveLane(id, name string) (module, laneKey string, c *modclient.Client, why string) {
	if h == nil {
		return "", "", nil, "no delivery module is enabled"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.lanes[id]
	if rec == nil {
		return "", "", nil, "this session has no delivery lane: it was not launched by this service while a delivery module was enabled"
	}
	if name != "" && rec.Module != name {
		if rec.Module == "" {
			return "", "", nil, fmt.Sprintf("module %q holds no lane for this session (%s)", name, rec.Reason)
		}
		return "", "", nil, fmt.Sprintf("this session's lane belongs to module %q, not %q", rec.Module, name)
	}
	if why := h.usableLocked(rec); why != "" {
		return "", "", nil, why
	}
	return rec.Module, rec.LaneKey, h.clients[rec.Module], ""
}

// laneStallLimit is how many consecutive enqueued-but-never-started sends on an
// idle session a lane is given before it is treated as not live (#264). One is
// not enough: a runtime can be slow to begin a turn.
const laneStallLimit = 2

// noteStall records one send that the module saw enqueued while the session sat
// idle and stayed idle, and reports the run length and whether the lane has now
// been degraded. The caller says what the receipt should tell the sender.
func (h *moduleHost) noteStall(id string) (n int, degraded bool) {
	if h == nil {
		return 0, false
	}
	h.mu.Lock()
	rec := h.lanes[id]
	if rec == nil {
		h.mu.Unlock()
		return 0, false
	}
	rec.stalls++
	n = rec.stalls
	h.mu.Unlock()
	if n < laneStallLimit {
		return n, false
	}
	h.degrade(id, fmt.Sprintf("the module enqueued %d sends in a row on an idle session and no turn started", n), false)
	return n, true
}

// clearStalls ends a run of stalls: a send the runtime accepted proves the lane
// delivers.
func (h *moduleHost) clearStalls(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if rec := h.lanes[id]; rec != nil {
		rec.stalls = 0
	}
	h.mu.Unlock()
}

// degrade marks a lane not usable until a later attach reports it live. gone
// says the module no longer knows the lane at all.
func (h *moduleHost) degrade(id, reason string, gone bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.lanes[id]
	if rec == nil {
		return
	}
	rec.Live = false
	rec.State = laneStateDegraded
	if gone {
		rec.State = laneStateGone
	}
	rec.Reason = sanitizeModuleText(reason, 200)
	h.saveLocked()
}

// --- create --------------------------------------------------------------

var moduleEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// offerLane runs step 1–4 of the create integration (spec §5.1): ask the first
// usable module for a lane and return the environment to launch with. It
// returns env unchanged, with offered=false, whenever the session ends up on
// the built-in lane — which is a normal outcome, recorded with its reason, and
// never an error: a module can only ever add a lane, never stop a create.
//
// The record is written BEFORE the process is launched, so a crash between the
// two leaves something the next start can find and close instead of a lane no
// session will ever use.
func (h *moduleHost) offerLane(ctx context.Context, id, cwd string, env map[string]string, canCarryEnv bool) (map[string]string, bool) {
	if h == nil {
		return env, false
	}
	record := func(rec laneRecord) {
		rec.Cwd = cwd
		rec.Since = h.d.now()
		rec.Surfaced = fleet.DeliveryLaneTerminal
		h.mu.Lock()
		old := h.lanes[id]
		h.lanes[id] = &rec
		h.saveLocked()
		h.mu.Unlock()
		// A stale record under this name (a lane whose close never landed)
		// must not be overwritten silently: the module still holds it.
		if old != nil && old.Module != "" && old.LaneKey != "" && old.LaneKey != rec.LaneKey {
			if oc := h.clients[old.Module]; oc != nil {
				h.closeAsync(old.Module, oc, old.LaneKey)
			}
		}
	}
	if !canCarryEnv {
		record(laneRecord{Reason: "this driver launches without the login-shell wrap, which is the only channel a module's environment can travel"})
		return env, false
	}
	var firstWhy string
	for _, name := range h.order {
		c := h.clients[name]
		if c == nil {
			if firstWhy == "" {
				firstWhy = fmt.Sprintf("module %q is enabled but not installed", name)
			}
			continue
		}
		if !c.Usable() {
			if firstWhy == "" {
				st := c.Status()
				if st.State == modclient.StateDisabled {
					firstWhy = fmt.Sprintf("module %q is disabled: %s", name, sanitizeModuleText(st.Reason, 120))
				} else {
					firstWhy = fmt.Sprintf("module %q is not ready yet (%s)", name, st.State)
				}
			}
			continue
		}
		// No claudeVersion: this service does not resolve the agent binary
		// (the login shell does), and a wrong answer would make the module
		// refuse a runtime build it could have served. The module's own PATH
		// is the better witness. Recorded in the ADR.
		res, err := c.PrepareLaunch(ctx, modclient.PrepareLaunchArgs{})
		if err != nil {
			h.count(name, "prepare_refused")
			if firstWhy == "" {
				firstWhy = fmt.Sprintf("module %q refused prepare-launch (%s)", name, moduleErrorClass(err))
			}
			continue
		}
		launchEnv, why := h.acceptPrepared(name, c, res, env)
		if why != "" {
			h.count(name, "prepare_refused")
			// The lane exists module-side; it is never going to be used.
			h.closeAsync(name, c, res.LaneKey)
			if firstWhy == "" {
				firstWhy = why
			}
			continue
		}
		h.count(name, "prepare_ok")
		record(laneRecord{Module: name, LaneKey: res.LaneKey, State: laneStatePrepared, Reason: "waiting for the agent process to come up"})
		return launchEnv, true
	}
	if firstWhy == "" {
		firstWhy = "no delivery module could take this session"
	}
	record(laneRecord{Reason: firstWhy})
	return env, false
}

// acceptPrepared validates a prepare-launch answer — every field is untrusted —
// and returns the environment to launch with. why is non-empty when the answer
// must be refused; the session then takes the built-in lane.
func (h *moduleHost) acceptPrepared(name string, c *modclient.Client, res modclient.PrepareLaunchResult, env map[string]string) (map[string]string, string) {
	if res.LaneKey == "" || len(res.LaneKey) > maxLaneKeyBytes || !printableASCII(res.LaneKey) {
		return nil, fmt.Sprintf("module %q returned a lane key this service will not keep", name)
	}
	if len(res.Env) > maxModuleEnvEntries {
		return nil, fmt.Sprintf("module %q asked for %d environment variables (the limit is %d)", name, len(res.Env), maxModuleEnvEntries)
	}
	prefixes := c.ReservedEnvPrefixes()
	out := make(map[string]string, len(env)+len(res.Env))
	for k, v := range env {
		out[k] = v
	}
	for k, v := range res.Env {
		if !moduleEnvName.MatchString(k) {
			return nil, fmt.Sprintf("module %q asked to set %q, which is not a variable name", name, sanitizeModuleText(k, 40))
		}
		// A module's environment is the ONLY way a reserved name gets set, and
		// only the names it declared reserved. Anything else is a module
		// reaching outside its own namespace.
		if !delivery.HasReservedPrefix(k, prefixes) {
			return nil, fmt.Sprintf("module %q asked to set %q, which is outside the prefixes it reserved", name, k)
		}
		if len(v) > maxModuleEnvValueBytes || strings.ContainsAny(v, "\n\r\x00") {
			return nil, fmt.Sprintf("module %q gave %q a value the staged-env file cannot carry", name, k)
		}
		out[k] = v
	}
	return out, ""
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// moduleErrorClass names an error for a counter-free log line: the module's own
// stable code, or which side of the pipe it died on.
func moduleErrorClass(err error) string {
	var me *modclient.Error
	switch {
	case errors.As(err, &me):
		return sanitizeModuleText(me.Code, 40)
	case errors.Is(err, modclient.ErrLost):
		return "no answer"
	case errors.Is(err, modclient.ErrNotSent):
		return "unavailable"
	}
	return "error"
}

// abandon undoes an offer whose launch did not happen: the lane is closed
// module-side and the record dropped, so a failed create leaves nothing behind.
func (h *moduleHost) abandon(id string) {
	if h == nil {
		return
	}
	h.closeLane(id)
}

// launched starts the asynchronous attach for a session whose process now
// exists. It never blocks the create response (spec §5.1 step 5).
func (h *moduleHost) launched(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	rec := h.lanes[id]
	ok := rec != nil && rec.Module != "" && rec.LaneKey != "" && !rec.Closing && !rec.attaching
	if ok {
		rec.attaching = true
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	if h.ctx.Err() != nil {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.attachPoll(id)
	}()
}

// panePID finds the session's pane process. found=false with ok=true means the
// multiplexer answered and the session is not there.
func (h *moduleHost) paneRows(ctx context.Context) (map[string]paneRow, bool) {
	ctx, cancel := h.d.bounded(ctx)
	defer cancel()
	rows, _, err := h.d.enumerate(ctx)
	if err != nil {
		return nil, false
	}
	out := make(map[string]paneRow, len(rows))
	for _, r := range rows {
		out[r.session] = r
	}
	return out, true
}

// attachPoll runs the attach for a fresh session: repeat while the module says
// the agent's record or socket is not there yet, up to the window, because a
// first-run trust dialog can hold the agent back for minutes.
func (h *moduleHost) attachPoll(id string) {
	defer func() {
		h.mu.Lock()
		if rec := h.lanes[id]; rec != nil {
			rec.attaching = false
		}
		h.mu.Unlock()
	}()
	// Real time, not the driver's injectable clock: this bounds how long the
	// poll actually waits, which a frozen test clock must not stretch forever.
	deadline := time.Now().Add(h.cfg.AttachWindow)
	sleep := func() bool {
		select {
		case <-h.ctx.Done():
			return false
		case <-time.After(h.cfg.AttachRetryEvery):
			return !time.Now().After(deadline)
		}
	}
	for {
		rec, ok := h.snapshot(id)
		if !ok || rec.Closing || rec.Module == "" || rec.LaneKey == "" {
			return
		}
		c := h.clients[rec.Module]
		if c == nil {
			return
		}
		if !c.Usable() {
			// Still starting, or down: the ready callback re-attaches every
			// lane when it comes up, so there is nothing to poll for.
			return
		}
		rows, _ := h.paneRows(h.ctx)
		row, found := rows[id]
		if !found || row.pid == 0 {
			// Not listed yet (a session takes a moment to appear) or gone: wait
			// out the window. A session that never appears is closed by the
			// background pass, which compares lanes with the live set.
			if !sleep() {
				return
			}
			continue
		}
		gen := c.Generation()
		res, err := c.Attach(h.ctx, modclient.AttachArgs{
			LaneKey: rec.LaneKey, PID: row.pid, Cwd: rec.Cwd,
			TimeoutMs: int(h.cfg.AttachWait / time.Millisecond),
		})
		h.applyAttach(id, rec.Module, gen, row.pid, res, err)
		if err != nil {
			return
		}
		if res.Live || !retryableAttachReasons[res.Reason] {
			return
		}
		if !sleep() {
			return
		}
	}
}

// applyAttach records one attach answer. Only under the module process it ran
// against: an answer that arrives after the module restarted describes a
// connection that no longer exists.
func (h *moduleHost) applyAttach(id, module string, gen uint64, pid int, res modclient.AttachResult, err error) {
	c := h.clients[module]
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.lanes[id]
	if rec == nil || rec.Module != module || rec.Closing {
		return
	}
	if c == nil || c.Generation() != gen {
		return
	}
	// PID is the process the lane is bound to, so it moves only when an attach
	// takes (or when nothing was ever recorded). A refused attach against a
	// different pid must not overwrite it: that is the only record that the
	// process behind the lane has changed.
	replaced := err != nil && pid != 0 && rec.PID != 0 && pid != rec.PID &&
		moduleErrorClass(err) == attachRefusedClass
	if pid != 0 && (rec.PID == 0 || (err == nil && res.Live)) {
		rec.PID = pid
	}
	switch {
	case replaced:
		// The agent process was replaced outside this service. The module
		// bound the lane to the old one and will refuse this process every
		// time, so retrying is noise that hides real attach failures. The lane
		// is marked gone (never retried by the background pass) with the
		// reason on the session's delivery field; a create or resume through
		// the service takes a fresh lane and closes this one module-side.
		h.count(module, "attach_replaced")
		h.logf("tmux: delivery module %s: lane of session %s refused a replaced agent process; not retrying", module, id)
		rec.Live = false
		rec.State = laneStateGone
		rec.Reason = laneReasonReplaced
	case err != nil:
		rec.Live = false
		rec.Reason = "attach failed: " + moduleErrorClass(err)
	case res.Live:
		h.count(module, "attach_live")
		rec.Live, rec.gen = true, gen
		rec.stalls = 0
		rec.State = res.State
		if rec.State == "" || rec.State == laneStatePrepared || rec.State == laneStateConnecting {
			rec.State = laneStateLive
		}
		rec.PeerVerified = res.PeerVerified
		rec.Reason = ""
	default:
		h.count(module, "attach_not_live")
		rec.Live = false
		rec.State = res.State
		rec.Reason = sanitizeModuleText(res.Reason, 200)
		if rec.Reason == "" {
			rec.Reason = "the module reported the lane not live"
		}
	}
	h.saveLocked()
}

// --- teardown, restart, background pass -----------------------------------

// closeLane tears a session's lane down: mark it closing (durably), then tell
// the module. The built-in Close remains the kill authority and never waits on
// this; a failure here is logged and retried when the module is next ready.
func (h *moduleHost) closeLane(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	rec := h.lanes[id]
	if rec == nil {
		h.mu.Unlock()
		return
	}
	rec.Closing, rec.Live = true, false
	module, key := rec.Module, rec.LaneKey
	if module == "" || key == "" {
		delete(h.lanes, id)
	}
	h.saveLocked()
	h.mu.Unlock()
	if module == "" || key == "" {
		return
	}
	c := h.clients[module]
	if c == nil || !c.Usable() || h.ctx.Err() != nil {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.flushClose(id, module, key, c)
	}()
}

// flushClose sends `close` for one lane and, once the module has answered —
// with success or with a refusal — forgets it. Only "the module could not be
// reached" keeps the record, so it is retried.
func (h *moduleHost) flushClose(id, module, key string, c *modclient.Client) {
	h.mu.Lock()
	rec := h.lanes[id]
	if rec == nil || rec.flushing || rec.LaneKey != key {
		h.mu.Unlock()
		return
	}
	rec.flushing = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if cur := h.lanes[id]; cur != nil && cur.LaneKey == key {
			cur.flushing = false
		}
		h.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	_, err := c.CloseLane(ctx, modclient.CloseArgs{LaneKey: key})
	var me *modclient.Error
	if err != nil && !errors.As(err, &me) {
		h.logf("tmux: delivery module %s: close failed, will retry when it is ready: %s", module, moduleErrorClass(err))
		return
	}
	h.count(module, "closed")
	h.mu.Lock()
	if rec := h.lanes[id]; rec != nil && rec.Closing && rec.LaneKey == key {
		delete(h.lanes, id)
		h.saveLocked()
	}
	h.mu.Unlock()
}

// closeAsync closes a lane that never got a record (a refused prepare-launch).
func (h *moduleHost) closeAsync(module string, c *modclient.Client, key string) {
	if key == "" || len(key) > maxLaneKeyBytes || h.ctx.Err() != nil {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		if _, err := c.CloseLane(ctx, modclient.CloseArgs{LaneKey: key}); err == nil {
			h.count(module, "closed")
		}
	}()
}

// rekey carries a lane across a session rename: the record is keyed by the
// session's name and the lane belongs to the process, not the name.
func (h *moduleHost) rekey(from, to string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec, ok := h.lanes[from]; ok {
		delete(h.lanes, from)
		h.lanes[to] = rec
		h.saveLocked()
	}
}

// ReconcileLanes applies §5.3 to lanes after Reconcile: a lane whose session
// vanished is marked closing (the module hears about it when it is ready); a
// lane whose session was adopted stays, to be re-attached when its module is
// ready. Records for sessions Reconcile named in neither list are left to the
// background pass, which compares them with the live set itself.
func (d *Driver) ReconcileLanes(rec Reconciliation) {
	h := d.mods
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	for _, s := range rec.Vanished {
		if lr := h.lanes[s.ID]; lr != nil && !lr.Closing {
			lr.Closing, lr.Live = true, false
			changed = true
		}
	}
	for id, lr := range h.lanes {
		if lr.Closing && (lr.Module == "" || lr.LaneKey == "") {
			delete(h.lanes, id)
			changed = true
		}
	}
	if changed {
		h.saveLocked()
	}
}

// onReady runs whenever a module completes its handshake and first health
// probe: remember the prefixes it reserves, tell it about lanes that closed
// while it was away, and re-attach every lane it holds.
func (h *moduleHost) onReady(name string, gen uint64) {
	c := h.clients[name]
	if c == nil || h.ctx.Err() != nil {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.recordPrefixes(name, c)
		h.flushClosing(name, c)
		h.reattachAll(name, c, gen)
		h.kick()
	}()
}

func (h *moduleHost) recordPrefixes(name string, c *modclient.Client) {
	got := c.ReservedEnvPrefixes()
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Join(h.prefixes[name], "\x00") == strings.Join(got, "\x00") {
		return
	}
	h.prefixes[name] = append([]string(nil), got...)
	h.saveLocked()
}

func (h *moduleHost) flushClosing(name string, c *modclient.Client) {
	type item struct{ id, key string }
	var todo []item
	h.mu.Lock()
	for id, rec := range h.lanes {
		if rec.Module == name && rec.Closing && rec.LaneKey != "" {
			todo = append(todo, item{id, rec.LaneKey})
		}
	}
	h.mu.Unlock()
	for _, it := range todo {
		h.flushClose(it.id, name, it.key, c)
	}
}

func (h *moduleHost) reattachAll(name string, c *modclient.Client, gen uint64) {
	type item struct {
		id  string
		rec laneRecord
	}
	var todo []item
	h.mu.Lock()
	for id, rec := range h.lanes {
		if rec.Module == name && !rec.Closing && rec.LaneKey != "" {
			todo = append(todo, item{id, *rec})
		}
	}
	h.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	rows, enumerated := h.paneRows(h.ctx)
	for _, it := range todo {
		if h.ctx.Err() != nil {
			return
		}
		row, found := rows[it.id]
		switch {
		case enumerated && !found:
			// The session is gone: nothing to re-attach to.
			h.closeLane(it.id)
			continue
		case found && it.rec.PID != 0 && row.pid != it.rec.PID:
			// The agent process was replaced. The lane belongs to the old
			// process; a resume relaunch goes through create and gets its own.
			h.closeLane(it.id)
			continue
		}
		pid := it.rec.PID
		if found && row.pid != 0 {
			pid = row.pid
		}
		h.count(name, "reattach_on_restart")
		res, err := c.Attach(h.ctx, modclient.AttachArgs{
			LaneKey: it.rec.LaneKey, PID: pid, Cwd: it.rec.Cwd,
			TimeoutMs: int(h.cfg.AttachWait / time.Millisecond),
		})
		if err != nil || !res.Live {
			h.count(name, "reattach_failed")
		}
		h.applyAttach(it.id, name, gen, pid, res, err)
	}
}

// kick asks the lane pass to run soon.
func (h *moduleHost) kick() {
	select {
	case h.tickCh <- struct{}{}:
	default:
	}
}

// laneLoop is the background pass: close lanes whose session no longer exists,
// flush closes a module missed, and re-attach lanes that degraded (the only
// way a lane leaves `degraded`).
func (h *moduleHost) laneLoop() {
	defer h.wg.Done()
	t := time.NewTicker(h.cfg.LaneTick)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		case <-h.tickCh:
		}
		h.lanePass()
	}
}

func (h *moduleHost) lanePass() {
	rows, enumerated := h.paneRows(h.ctx)
	type item struct {
		id  string
		rec laneRecord
	}
	var gone, degraded []item
	h.mu.Lock()
	for id, rec := range h.lanes {
		switch {
		case rec.Module == "" || rec.LaneKey == "":
			if enumerated {
				if _, ok := rows[id]; !ok {
					gone = append(gone, item{id, *rec})
				}
			}
		case rec.Closing:
			gone = append(gone, item{id, *rec})
		case enumerated && !hasRow(rows, id):
			gone = append(gone, item{id, *rec})
		case !rec.Live && !rec.attaching && retryableLaneState(rec.State):
			// Degraded lanes return to service only through a later attach;
			// a lane whose first attach never got an answer (the module was
			// not ready, or the agent had not come up) is waiting for one.
			degraded = append(degraded, item{id, *rec})
		}
	}
	h.mu.Unlock()
	for _, it := range gone {
		if it.rec.Module == "" || it.rec.LaneKey == "" {
			h.mu.Lock()
			delete(h.lanes, it.id)
			h.saveLocked()
			h.mu.Unlock()
			continue
		}
		if it.rec.Closing {
			if c := h.clients[it.rec.Module]; c != nil && c.Usable() {
				h.flushClose(it.id, it.rec.Module, it.rec.LaneKey, c)
			}
			continue
		}
		h.closeLane(it.id)
	}
	for _, it := range degraded {
		c := h.clients[it.rec.Module]
		if c == nil || !c.Usable() {
			continue
		}
		row := rows[it.id]
		gen := c.Generation()
		pid := it.rec.PID
		if row.pid != 0 {
			pid = row.pid
		}
		if pid == 0 {
			continue // the first attach of a lane needs the agent's pid
		}
		res, err := c.Attach(h.ctx, modclient.AttachArgs{
			LaneKey: it.rec.LaneKey, PID: pid, Cwd: it.rec.Cwd,
			TimeoutMs: int(h.cfg.AttachWait / time.Millisecond),
		})
		h.applyAttach(it.id, it.rec.Module, gen, pid, res, err)
	}
}

func hasRow(rows map[string]paneRow, id string) bool { _, ok := rows[id]; return ok }

// retryableLaneState says a lane that is not live is worth attaching again on
// the background pass: it has not been refused, only not connected. A lane the
// module reports `gone` is not: it has lost it, and only a resume relaunch gets
// a new one.
func retryableLaneState(st string) bool {
	switch st {
	case "", laneStatePrepared, laneStateConnecting, laneStateDegraded:
		return true
	}
	return false
}
