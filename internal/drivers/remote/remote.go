// Package remote implements driver.Driver as an HTTP client to a peer
// muster instance — session-abstraction.md §4.2, "the remote driver".
//
// This is the entire federation design, and the cheapest available test of
// it. §4.2 states the claim plainly: cross-machine operation is not a
// feature layered on top of the abstraction, it is one implementation of
// it, and "if the interface cannot express 'a session on another machine,'
// the interface is wrong."
//
// It mostly can. Two places it cannot are recorded below, plus one that has
// since been fixed. All three share a shape worth naming up front: the interface was designed against a
// local driver, where certain things are free — knowing your own
// capabilities, knowing who is asking, never being unreachable. None of
// those are free across a network, and the interface has nowhere to put the
// difference.
//
// # FINDING 1: Capabilities() cannot fail, and a remote driver's cannot succeed
//
// Capabilities() is synchronous and infallible: no context, no error. For a
// local driver that is correct — it is describing itself. A remote driver is
// describing somebody else, and the answer lives on the peer
// (GET /v1/runtimes), behind a network that may be down.
//
// There is no honest synchronous answer when the peer has never been
// reached. Reporting the peer's real capabilities is impossible; reporting
// optimistic ones is the §5.6 emulation the field exists to prevent; and
// reporting everything false is a lie of a subtler kind — see FINDING 2.
//
// What this driver does: report a conservative floor until the peer has
// answered, refresh the cache whenever it does, and never claim a capability
// it has not been told about. What it cannot do is tell a req which of
// those two situations it is in.
//
// # FINDING 2: DriverCapabilities has no way to say "I don't know yet"
//
// This is §5.7 — "absence and failure are different answers" — a third
// time, in a third place, and by now that is not a coincidence but a
// property of the design worth stating: every field in this API that can be
// absent needs to distinguish absent-because-no from absent-because-unknown.
//
// A DriverCapabilities of all-false means "this driver supports nothing".
// An unreached peer produces exactly that value, meaning "nobody has told me
// anything". A req consulting GET /v1/runtimes (which api-http.md §3.1
// says it MUST do before relying on a capability) cannot tell a peer that
// genuinely observes nothing from a peer that has not been asked yet. It
// degrades identically in both cases — which is safe, and is why this is
// recorded rather than treated as urgent — but it also means a permanently
// misconfigured peer is indistinguishable from a deliberately minimal one.
//
// # RESOLVED: the caller's authority is now a parameter
//
// This driver previously took the original caller's credentials from a
// context value, because no operation in §3 had anywhere to put them. §13
// requires a proxying service to present the ORIGINAL caller's authority to
// a peer, never its own — "otherwise every machine becomes a confused deputy
// for every other" — and an out-of-band value is exactly the kind a service
// can forget to attach.
//
// The failure mode was the reason this was the most serious defect in the
// design: a remote driver with no req credentials would reach for the one
// credential it certainly had, its own. The request then succeeds. The tests
// pass. The authorization is silently widened, and nothing reports it,
// because the symptom of the bug is that everything works.
//
// fleet.Caller is now a parameter of every operation, and this driver holds
// NO credential of its own. That second half matters more than the first:
// refusing to fall back is a policy a driver can get wrong, whereas having
// nothing to fall back TO is a property of the type. The confused deputy is
// not prevented here; it is unrepresentable.
//
// Reads are included. An earlier revision let List and State fall back to
// the driver's own token on the reasoning that reading is harmless — but
// "which sessions exist, in which directories, on which machine" is exactly
// the reconnaissance an unauthorized req wants, and §6 grants read
// permission broadly rather than universally.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

const defaultDeadlineMs = 3000

// capabilityStaleness bounds how long a cached, previously-`observed`
// capability declaration may still be reported as `observed` without fresh
// evidence from the peer it describes (muster #67).
//
// Nothing before this bound existed: RefreshCapabilities ran exactly once,
// at peer registration, and never again for the life of the process — which
// under a keep-alive supervisor is weeks. Measured directly: restart a peer
// onto a capability-adding revision and the observer's cached answer stays
// labelled `observed` and wrong for as long as it happens to run. That is
// the worse of #67's two bad states — a stale claim that does not admit to
// being stale — so it gets a hard ceiling rather than staying unbounded.
// Capabilities degrades its own answer to `assumed` past this bound (the
// "or degrade the label" half of #67's ask #1); noteSuccessfulContact is
// the "re-probe" half, and in practice keeps a driver seeing any traffic
// well under the ceiling.
const capabilityStaleness = 5 * time.Minute

// ErrNoCallerAuthority is returned when an operation arrives with no
// credential to present to the peer. Every operation checks it, reads
// included: this driver has no credential of its own to substitute, so an
// empty one means the request simply cannot be made (§13).
var ErrNoCallerAuthority = errors.New(
	"remote: req carries no credential; a proxied request presents the " +
		"original caller's authority, and this proxy holds none of its own (§13)")

// Driver is an HTTP client to one peer muster, presented as an
// ordinary driver.Driver. The service registers it with RegisterPeerDriver
// and never learns that it is remote — which is the design's whole claim.
type Driver struct {
	machine  fleet.MachineId
	base     string
	client   *http.Client
	deadline time.Duration
	margin   time.Duration
	now      func() time.Time

	// identity is the credential THIS machine holds ON THIS PEER (§6).
	//
	// An earlier revision deliberately had no credential field at all: with
	// one shared secret, forwarding the caller's token was both possible and
	// correct, and holding one of its own was how a proxy became a confused
	// deputy.
	//
	// Per-peer credentials remove that coincidence. A caller's token means
	// nothing on another machine, so there is nothing to forward, and a
	// proxied request necessarily authenticates as the relaying machine. The
	// caller's identity travels alongside as an assertion (onBehalfOf).
	//
	// What that costs is honest to state: the peer authorizes THIS MACHINE,
	// not the original caller, and trusts our claim about who asked exactly
	// as far as it trusts us. What it cannot do is manufacture authority — a
	// relay never obtains more than it was granted, whatever principal it
	// names. Empty means shared-token mode, where the caller's credential is
	// forwarded as before.
	identity string

	mu       sync.RWMutex
	caps     fleet.DriverCapabilities
	capsSeen bool
	// refreshing is true while a capability probe triggered by
	// noteSuccessfulContact is in flight, so a burst of concurrent traffic
	// triggers at most one probe rather than one per call.
	refreshing bool
	// runtime is the peer's own runtime id, learned on the same probe as
	// caps. Empty until the peer has answered — which is honest, and is
	// distinguishable because the cached capabilities say `assumed` in
	// exactly that case.
	runtime fleet.RuntimeId
	// build is what the peer said it was running, the last time it said
	// anything. Cached beside caps because it is learned the same way and
	// is stale in the same manner: a peer that has restarted onto new code
	// has not told us, and will not until something asks again.
	build fleet.Build
	// maxInputBytes is the peer's own effective input-length limit
	// (muster #130), learned on the same /v1/health probe as build
	// and cached the same way: zero until the peer has answered at least
	// once, which is honest — see driver.MaxInputBytesReporter.
	maxInputBytes int
	// labelLimits is what the peer's /v1/health said about session labels
	// (muster #153), on the same probe as build. Nil means either the
	// peer has not answered yet or it predates labels — see requireLabels,
	// which trusts only a positive answer and re-asks before refusing.
	labelLimits *fleet.LabelLimits
	// supportsConversationId is what the peer's /v1/health said about the
	// conversationId create field (muster #224), on the same probe as
	// build and labelLimits. Nil means either the peer has not answered yet
	// or it predates the field — see requireConversationId, which trusts
	// only a positive answer and re-asks before refusing, the same rule
	// requireLabels already applies to labelLimits.
	supportsConversationId *bool
	// supportsLaunchSettings is what the peer's /v1/health said about the
	// `settings` create field (muster #247), cached and re-asked exactly as
	// supportsConversationId is — see requireLaunchSettings.
	supportsLaunchSettings *bool
	// launchSettingsOutsideBypass is the key list the peer's /v1/health
	// advertised for `settings` on a session NOT in bypass mode (muster #254).
	// Nil means the peer has not answered or predates #254 — whose `settings`
	// is bypass-only — see requireLaunchSettings.
	launchSettingsOutsideBypass []string

	// self is this machine's own id, named to the peer when asking whether it
	// is listed there (muster #154). Empty means the standing probe is
	// not made.
	self fleet.MachineId
	// standing is this machine's registration on the peer as the peer last
	// reported it — see refreshStanding and PeerStanding.
	standing     fleet.PeerStanding
	standingSeen bool

	// down is what this driver remembers about how the peer has been
	// answering, and downPol is its tuning — see down.go (muster #237).
	down    downState
	downPol downPolicy
}

// Option configures a Driver.
type Option func(*Driver)

// WithHTTPClient replaces the HTTP client. The client's own Timeout is left
// alone; deadlines are enforced per-call via context, because §4.4 requires
// a req to be able to shorten a deadline and a client-wide timeout
// cannot express that.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Driver) {
		if c != nil {
			d.client = c
		}
	}
}

// WithDeadline sets the FLOOR for this driver's per-call deadline (§4.4).
//
// It is a floor rather than the value because a proxy's deadline is not its
// own to choose freely — see effectiveDeadline.
//
// It is a floor for this driver's own choice only. A caller may still shorten
// any single call below it with Fleet-Deadline-Ms (§4.4); a call that misses
// such a shortened deadline is logged with both values (budget and bound), so
// a cliff below the floor is traceable to the caller that set it (#174).
func WithDeadline(dur time.Duration) Option {
	return func(d *Driver) {
		if dur > 0 {
			d.deadline = dur
		}
	}
}

// WithTransitMargin sets the allowance added to a peer's declared deadline to
// cover the network. Default 2s.
//
// The margin lengthens how long this driver waits. The transit reserve
// (announcedDeadlineMs) shortens the bound it announces. Both make room for
// the network, from opposite ends of the same call.
func WithTransitMargin(dur time.Duration) Option {
	return func(d *Driver) {
		if dur > 0 {
			d.margin = dur
		}
	}
}

// effectiveDeadline is how long this driver will actually wait.
//
// # A proxy's deadline is not independent of its peer's
//
// §4.4 governs a *caller* shortening a driver's deadline. It says nothing
// about composing deadlines across a hop, and the omission has a sharp edge:
// if a proxy waits less time than the peer has declared it may take, the
// proxy abandons calls the peer would have completed. Every such call is
// reported as "unreachable" — a machine that answered, described as one that
// did not, which is §5.7's confusion produced by a timer.
//
// Measured, which is why this exists: a peer declaring 5s, behind a proxy
// waiting 3s, on a host under enough load that a subprocess spawn took over a
// second. The peer was healthy and answering; the proxy timed out.
//
// So once the peer's capabilities are known, this driver waits at least as
// long as the peer said it might take, plus transit. Before they are known it
// falls back to the configured floor — and that gap is one more consequence
// of §14 D3, capability declaration being unable to say "not yet known".
//
// The same confusion has a second source: announcing to the peer the exact
// bound this driver enforces, so the peer's honest answer is still on the
// wire when the timer fires. announcedDeadlineMs covers that side (#175).
func (d *Driver) effectiveDeadline() time.Duration {
	d.mu.RLock()
	seen, peerMs := d.capsSeen, d.caps.DeadlineMs
	d.mu.RUnlock()
	if seen && peerMs > 0 {
		if peer := time.Duration(peerMs)*time.Millisecond + d.margin; peer > d.deadline {
			return peer
		}
	}
	return d.deadline
}

// WithIdentity sets the credential this machine presents to the peer (§6).
// Without it the driver stays in shared-token mode and forwards the caller's
// credential, which only works while every machine accepts the same secret.
func WithIdentity(token string) Option {
	return func(d *Driver) { d.identity = token }
}

// WithSelf names this machine, so the probe can ask the peer whether it lists
// this machine back (muster #154).
func WithSelf(machine fleet.MachineId) Option {
	return func(d *Driver) { d.self = machine }
}

func withClock(f func() time.Time) Option { return func(d *Driver) { d.now = f } }

// bearerFor decides what a proxied request authenticates with, and whom it
// names. See the identity field for why these are two different things.
func (d *Driver) bearerFor(req fleet.Request) (token, onBehalfOf string, ok bool) {
	if d.identity != "" {
		return d.identity, req.Caller.Principal, true
	}
	if req.Caller.HasCredential() {
		return req.Caller.Credential, req.Caller.Principal, true
	}
	return "", "", false
}

// New builds a driver for the peer at base (e.g. "https://host:PORT").
//
// It deliberately does NOT probe the peer. §7.2 configures peers statically,
// and §5.7 requires an unreachable peer to surface as a source reporting
// unreachable rather than as an absence — a peer that could not be
// registered because it happened to be down at startup would be exactly that
// absence. Registration therefore always succeeds; reachability is a
// per-call fact, reported per call.
func New(machine fleet.MachineId, base string, opts ...Option) *Driver {
	d := &Driver{
		machine:  machine,
		base:     strings.TrimRight(base, "/"),
		client:   &http.Client{},
		deadline: defaultDeadlineMs * time.Millisecond,
		margin:   2 * time.Second,
		now:      time.Now,
		downPol:  defaultDownPolicy(),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

var _ driver.Driver = (*Driver)(nil)

// Capabilities reports what the PEER can do, as last observed — see
// FINDINGS 1 and 2 for why this is the wrong shape for a remote driver and
// what it costs.
//
// DeadlineMs is the one field this driver can always answer honestly,
// because it describes this driver's own transport rather than the peer's
// behaviour. That matters: §4.4 makes DeadlineMs mandatory and
// Validate-checked at registration, so a remote driver must be registrable
// before its peer has ever answered.
func (d *Driver) Capabilities() fleet.DriverCapabilities {
	d.mu.RLock()
	caps, seen := d.caps, d.capsSeen
	d.mu.RUnlock()

	caps.DeadlineMs = d.effectiveDeadline().Milliseconds()
	if !seen {
		// Nobody has told this driver anything about the peer. The flags
		// are a conservative floor, and saying so is the whole of D3: a
		// caller can now tell an unreached peer from a minimal one, which
		// an all-false declaration alone cannot express.
		return caps.Assumed()
	}
	// A cached `observed` is a claim about the peer AS IT WAS the moment it
	// answered. Past capabilityStaleness that claim can no longer be
	// attributed to whatever the peer is currently running — it may have
	// restarted onto a different build any time since — so it degrades
	// rather than keep asserting an observation that may no longer hold
	// (muster #67, ask #1: "a wrong `observed` is worse than a stale
	// `assumed`"). This check runs on every read, so the degrade is not
	// contingent on a background refresh having noticed first.
	//
	// The degraded value is a genuine conservative floor — every flag
	// false, same as an unreached peer — rather than the stale flags
	// relabelled. `source: assumed` already tells a caller not to trust
	// these flags as an answer, but leaving true values sitting there
	// under that label is still a claim this driver has no evidence for
	// any more; a floor is the only value it can still stand behind.
	if caps.ObservedAt != nil && d.now().Sub(*caps.ObservedAt) > capabilityStaleness {
		var floor fleet.DriverCapabilities
		floor.DeadlineMs = caps.DeadlineMs
		return floor.Assumed()
	}
	return caps
}

// RefreshCapabilities asks the peer what it can do and caches the answer.
//
// This exists because Capabilities() cannot: it needs a context, it needs to
// be able to fail, and it needs to be callable again later. A method the
// interface does not know about is a poor substitute for a signature that
// admitted this in the first place (FINDING 1).
func (d *Driver) RefreshCapabilities(ctx context.Context, req fleet.Request) error {
	// Deliberately NOT bounded by this driver's own deadline.
	//
	// The bootstrap is circular: the deadline this driver should enforce is
	// derived from the peer's declared one (see effectiveDeadline), but
	// learning it requires a call — and bounding that call by the floor is
	// exactly the too-short value the derivation exists to correct. Observed
	// directly: the probe timed out at the 3s floor against a loaded peer,
	// so the deadline was never learned and every later call kept using the
	// floor.
	//
	// §4.4 governs *session operations*, whose whole point is that they must
	// not block unboundedly. This is an out-of-band metadata probe made by
	// an operator at startup, and it honours the caller's context — which is
	// where the bound belongs for a call whose purpose is to discover bounds.
	//
	// Standing goes first (muster #154): a credential the peer accepts
	// but grants no read is exactly the misconfiguration it exists to report,
	// and the /v1/runtimes read below would refuse that credential and return
	// before anything after it ran.
	d.refreshStanding(ctx, req)

	var body fleet.Collection[fleet.RuntimeInfo]
	if err := d.do(ctx, req, http.MethodGet, "/v1/runtimes", nil, &body); err != nil {
		return err
	}
	for _, ri := range body.Items() {
		if ri.Machine != d.machine {
			continue
		}
		d.mu.Lock()
		// Adopt what the peer said about itself, then stamp WHEN we heard
		// it — the peer's own timestamp would be on the peer's clock (§11),
		// and freshness here is a local judgement about a local cache.
		d.caps = ri.Capabilities.Observed(d.now())
		d.capsSeen = true
		// The peer named its runtime in the same row. Discarding it and
		// reporting "" was a placeholder that survived: a client cannot use
		// ?runtime= to disambiguate a peer's session if the peer's runtime
		// is never reported.
		d.runtime = ri.Runtime
		d.mu.Unlock()

		// Learn which code the peer is running, and its effective input
		// limit, on the same probe rather than a second one each. Failure
		// here is deliberately not propagated: both are diagnostic, and a
		// peer that answers /v1/runtimes but not /v1/health is still a
		// working peer. Losing the deadline we just learned in order to
		// report a missing diagnostic would trade a correctness property
		// for an informational one.
		if h, err := d.peerHealth(ctx, req); err == nil {
			d.mu.Lock()
			d.build = h.Build
			d.maxInputBytes = h.MaxInputBytes
			d.labelLimits = h.Labels
			d.supportsConversationId = h.SupportsConversationId
			d.supportsLaunchSettings = h.SupportsLaunchSettings
			d.launchSettingsOutsideBypass = h.LaunchSettingsOutsideBypass
			d.mu.Unlock()
		}
		return nil
	}
	return fmt.Errorf("remote: peer %q reported no runtimes for itself", d.machine)
}

// peerHealthBody is the subset of GET /v1/health this driver reads to learn
// facts about the peer it fronts — build identity (#121) and, since #130,
// the peer's own effective input-length limit. One struct and one call for
// both, rather than a probe per fact, because they are learned the same way
// and go stale the same way.
type peerHealthBody struct {
	Build         fleet.Build `json:"build"`
	MaxInputBytes int         `json:"maxInputBytes"`
	// Labels is absent on a peer that predates session labels (#153).
	Labels *fleet.LabelLimits `json:"labels"`
	// SupportsConversationId is absent on a peer that predates the
	// conversationId create field (muster #224) — the same shape as
	// Labels one field up, but a bare flag rather than a limits struct: there
	// is no bound to report, only whether the field is understood at all. A
	// machine that has it always sends true; absence, not the value, is what
	// tells "predates the feature" from "has it".
	SupportsConversationId *bool `json:"supportsConversationId"`
	// SupportsLaunchSettings is absent on a peer that predates the `settings`
	// create field (muster #247); the same bare-flag shape as
	// SupportsConversationId, for the same reason.
	SupportsLaunchSettings *bool `json:"supportsLaunchSettings"`
	// LaunchSettingsOutsideBypass lists the `settings` keys the peer accepts on
	// a session that is not in bypass mode (muster #254). Absent on a peer that
	// carries only #247, where `settings` outside bypass is a 400 — or on a
	// peer older still, where it is dropped.
	LaunchSettingsOutsideBypass []string `json:"launchSettingsOutsideBypass"`
}

// requireLabels refuses a labelled write to a peer that would drop the labels
// (muster #153).
//
// A peer on an older build decodes a create body with encoding/json, which
// ignores a field it does not know: it would start the session, answer 201,
// and keep none of the labels. That is a session bound to nothing that looks
// bound — so the check runs BEFORE any side effect. Only a positive cached
// answer is trusted; anything else is asked again, so a peer that has since
// upgraded is not refused on stale evidence.
func (d *Driver) requireLabels(ctx context.Context, req fleet.Request) error {
	d.mu.RLock()
	limits := d.labelLimits
	d.mu.RUnlock()
	if limits != nil {
		return nil
	}
	h, err := d.peerHealth(ctx, req)
	if err != nil {
		return fmt.Errorf("remote: could not confirm %s carries session labels, so the labelled request was not sent: %w", d.machine, err)
	}
	d.mu.Lock()
	d.build = h.Build
	d.maxInputBytes = h.MaxInputBytes
	d.labelLimits = h.Labels
	d.mu.Unlock()
	if h.Labels == nil {
		return &fleet.Error{
			Kind: fleet.ErrorUnsupported,
			Message: fmt.Sprintf("peer %s does not carry session labels (older build); the request was not sent, "+
				"because that peer would accept it and silently drop the labels", d.machine),
			Machine: d.machine,
		}
	}
	return nil
}

// requireConversationId refuses forwarding a caller-chosen conversation id to
// a peer that would drop it (muster #224) — requireLabels' own pattern
// applied to a second field that predates on some peers.
//
// A peer on an older build decodes a create body with encoding/json, which
// ignores a field it does not know: it would start the session under an id of
// its own choosing, answer 201, and the caller would have no way to tell its
// request was silently dropped one machine away — a session bound to nothing
// that looks bound, the same failure #153 built requireLabels to prevent. Only
// a positive cached answer is trusted; anything else is asked again, so a peer
// that has since upgraded is not refused on stale evidence.
func (d *Driver) requireConversationId(ctx context.Context, req fleet.Request) error {
	d.mu.RLock()
	supported := d.supportsConversationId
	d.mu.RUnlock()
	if supported != nil && *supported {
		return nil
	}
	h, err := d.peerHealth(ctx, req)
	if err != nil {
		return fmt.Errorf("remote: could not confirm %s carries conversationId, so the create was not sent: %w", d.machine, err)
	}
	d.mu.Lock()
	d.build = h.Build
	d.maxInputBytes = h.MaxInputBytes
	d.labelLimits = h.Labels
	d.supportsConversationId = h.SupportsConversationId
	d.supportsLaunchSettings = h.SupportsLaunchSettings
	d.launchSettingsOutsideBypass = h.LaunchSettingsOutsideBypass
	d.mu.Unlock()
	if h.SupportsConversationId == nil || !*h.SupportsConversationId {
		return &fleet.Error{
			Kind: fleet.ErrorUnsupported,
			Message: fmt.Sprintf("peer %s does not carry conversationId (older build); the create was not sent, "+
				"because that peer would accept it and silently start a conversation under an id of its own choosing", d.machine),
			Machine: d.machine,
		}
	}
	return nil
}

// requireLaunchSettings refuses forwarding launch-time CLI settings to a peer
// that would drop them (muster #247) — requireConversationId's pattern for a
// third field that predates on some peers.
//
// A peer on an older build ignores a create-body field it does not know: it
// would start the session in bypass mode, answer 201, and the session would
// lack the setting it was created with — here, a bypass session that silently
// holds inbound cross-session messages. Only a positive cached answer is
// trusted; anything else is asked again, so an upgraded peer is not refused on
// stale evidence.
func (d *Driver) requireLaunchSettings(ctx context.Context, req fleet.Request, spec fleet.SessionSpec) error {
	// muster #254: outside bypass the peer must also carry the keys asked for.
	// A peer that carries only #247 answers `supportsLaunchSettings: true` and
	// then refuses a non-bypass create with a 400; refusing here, before
	// anything crosses, gives the same verdict with the reason named on this
	// side instead of a relayed one.
	var need []string
	if spec.PermissionMode != fleet.PermissionModeBypass {
		need = fleet.LaunchSettingsKeys(spec.Settings)
	}
	ok := func(supported *bool, keys []string) bool {
		if supported == nil || !*supported {
			return false
		}
		if len(need) == 0 {
			return true
		}
		have := make(map[string]bool, len(keys))
		for _, k := range keys {
			have[k] = true
		}
		for _, k := range need {
			if !have[k] {
				return false
			}
		}
		return true
	}
	d.mu.RLock()
	supported, keys := d.supportsLaunchSettings, d.launchSettingsOutsideBypass
	d.mu.RUnlock()
	if ok(supported, keys) {
		return nil
	}
	h, err := d.peerHealth(ctx, req)
	if err != nil {
		return fmt.Errorf("remote: could not confirm %s carries launch settings, so the create was not sent: %w", d.machine, err)
	}
	d.mu.Lock()
	d.build = h.Build
	d.maxInputBytes = h.MaxInputBytes
	d.labelLimits = h.Labels
	d.supportsConversationId = h.SupportsConversationId
	d.supportsLaunchSettings = h.SupportsLaunchSettings
	d.launchSettingsOutsideBypass = h.LaunchSettingsOutsideBypass
	d.mu.Unlock()
	if ok(h.SupportsLaunchSettings, h.LaunchSettingsOutsideBypass) {
		return nil
	}
	if h.SupportsLaunchSettings != nil && *h.SupportsLaunchSettings && len(need) > 0 {
		return &fleet.Error{
			Kind: fleet.ErrorUnsupported,
			Message: fmt.Sprintf("peer %s carries launch settings only for bypass sessions (it predates #254, or does not list %s); "+
				"the create was not sent, because that peer would refuse it or start the session without them", d.machine, strings.Join(need, ", ")),
			Machine: d.machine,
		}
	}
	return &fleet.Error{
		Kind: fleet.ErrorUnsupported,
		Message: fmt.Sprintf("peer %s does not carry launch settings (older build); the create was not sent, "+
			"because that peer would accept it and silently start the session without them", d.machine),
		Machine: d.machine,
	}
}

// peerHealth reads the subset of the peer's health endpoint this driver
// caches.
func (d *Driver) peerHealth(ctx context.Context, req fleet.Request) (peerHealthBody, error) {
	var body peerHealthBody
	if err := d.do(ctx, req, http.MethodGet, "/v1/health", nil, &body); err != nil {
		return peerHealthBody{}, err
	}
	return body, nil
}

// noteSuccessfulContact opportunistically re-probes this peer's capabilities
// off the back of an ordinary operation that just reached it and got a
// domain answer back — muster #67's sharpened ask, which replaces a
// time-based schedule nobody was driving: "probe on first successful
// contact, not only at startup." An ordinary verb succeeding — a create, a
// keypress, even a guarded refusal — is proof the peer is up and speaking
// the protocol, and this is the cheapest moment to use that proof: the round
// trip already happened.
//
// Two situations warrant a refresh, and both are #67's reported bad states:
//
//   - capabilities have never been observed (capsSeen is false): the peer
//     may have been down, or on an older build, the one time this driver
//     probed it at startup, and nothing has asked again since. #67 measured
//     this surviving a full round of successful relayed traffic — a create,
//     a keypress, two refusals, a close — none of which populated the
//     record.
//   - the cached observation is older than capabilityStaleness: still
//     usable, but old enough that Capabilities() has already started
//     degrading it to `assumed` on read (see above); refreshing it is what
//     lets `observed` come back rather than staying degraded indefinitely.
//
// Runs in its own goroutine against its own bounded context: the operation
// that discovered the staleness must not wait on the probe it triggers, and
// must not hand the probe a context this call is about to cancel on return.
// The refreshing flag keeps concurrent traffic to one in-flight probe.
func (d *Driver) noteSuccessfulContact(req fleet.Request) {
	d.mu.Lock()
	stale := !d.capsSeen || d.caps.ObservedAt == nil ||
		d.now().Sub(*d.caps.ObservedAt) > capabilityStaleness
	if !stale || d.refreshing {
		d.mu.Unlock()
		return
	}
	d.refreshing = true
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			d.refreshing = false
			d.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), d.deadline)
		defer cancel()
		_ = d.RefreshCapabilities(ctx, req)
	}()
}

// refreshStanding asks the peer what it makes of THIS machine: whether its
// roster lists us, and what it grants the credential we present (muster
// #154). It asks the peer's whoami — authentication only, exempt from read —
// so a credential the peer accepts but grants nothing still gets an observed
// answer, and a 401 can only mean the credential matches no principal there.
//
// Made only with this machine's own identity. In shared-token mode the
// credential presented is whichever caller's request triggered the probe, and
// its grants say nothing about this machine; standing stays assumed there.
//
// The mixed-version rule is the whole point of reading the raw key: a peer on
// a build that predates the field omits `listsYou`, and that must read as
// assumed — never as false, which would report a registration problem nobody
// has.
func (d *Driver) refreshStanding(ctx context.Context, req fleet.Request) {
	if d.identity == "" || d.self == "" {
		return
	}
	var raw map[string]json.RawMessage
	err := d.do(ctx, req, http.MethodGet, "/v1/whoami?peer="+url.QueryEscape(string(d.self)), nil, &raw)
	now := d.now()
	st := fleet.AssumedPeerStanding()
	var fe *fleet.Error
	switch {
	case err == nil:
		lists, has := raw["listsYou"]
		if !has {
			break // older build: nobody can tell
		}
		var listsMeBack *bool
		var grants []string
		if json.Unmarshal(lists, &listsMeBack) != nil || json.Unmarshal(raw["grants"], &grants) != nil {
			return // a malformed answer is not an observation; keep the last one
		}
		if grants == nil {
			grants = []string{}
		}
		st = fleet.PeerStanding{ListsMeBack: listsMeBack, GrantsToMe: grants,
			Source: fleet.CapabilitiesObserved, ObservedAt: &now}
	case routeMissing(err):
		// A build that predates whoami itself: still nobody can tell.
	case errors.As(err, &fe) && fe.Kind == fleet.ErrorUnauthorized:
		// The peer answered: this credential matches no principal there.
		st = fleet.PeerStanding{GrantsToMe: []string{}, Source: fleet.CapabilitiesObserved, ObservedAt: &now}
	default:
		// Unreached, or a failure that is not an answer: keep what we knew.
		// PeerStanding decays it to assumed once it is too old to stand on.
		return
	}
	d.mu.Lock()
	d.standing, d.standingSeen = st, true
	d.mu.Unlock()
}

// PeerStanding reports this machine's standing on the peer as last observed.
// Implements driver.PeerStandingReporter (muster #154).
//
// An observation older than capabilityStaleness degrades to the assumed floor,
// for #67's reason: a peer may have been reconfigured since, and a stale
// `observed` is worse than an honest `assumed`.
func (d *Driver) PeerStanding() fleet.PeerStanding {
	d.mu.RLock()
	st, seen := d.standing, d.standingSeen
	d.mu.RUnlock()
	if !seen || st.Source != fleet.CapabilitiesObserved ||
		(st.ObservedAt != nil && d.now().Sub(*st.ObservedAt) > capabilityStaleness) {
		return fleet.AssumedPeerStanding()
	}
	st.GrantsToMe = append([]string{}, st.GrantsToMe...)
	return st
}

var _ driver.PeerStandingReporter = (*Driver)(nil)

// Runtime reports the peer's runtime id, empty until the peer has answered.
func (d *Driver) Runtime() fleet.RuntimeId {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.runtime
}

// Build reports what the peer said it was running, if it has ever said.
// Implements driver.BuildReporter (muster #121), which is how
// internal/service.ListMachines surfaces it per peer on GET /v1/machines.
//
// An unknown build and a matching build must not be conflated — see
// fleet.Build.SameAs, which refuses to call anything unknown "the same".
func (d *Driver) Build() fleet.Build {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.build
}

// MaxInputBytes reports what the peer said its own effective input-length
// limit was, if it has ever said. Implements driver.MaxInputBytesReporter
// (muster #130), which is how internal/service.ListMachines surfaces
// it per peer on GET /v1/machines.
//
// Zero means "never answered" — see fleet.MachineInfo.MaxInputBytes for why
// that reading is unambiguous here without a separate Known flag.
func (d *Driver) MaxInputBytes() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.maxInputBytes
}

// CapabilitiesKnown reports whether the peer has ever answered. It is the
// distinction DriverCapabilities itself cannot carry (FINDING 2), exposed
// here so at least a req holding the concrete type can ask.
func (d *Driver) CapabilitiesKnown() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.capsSeen
}

// The transit reserve: how much of the bound this driver enforces is held
// back from the bound it announces to the peer (#175).
//
// transitReserveMax caps it on a long bound, and transitReserveShare caps it
// on a short one (at most 1/share of what remains), so a caller-shortened
// budget keeps most of itself for the peer's work instead of being reduced
// to nothing.
const (
	transitReserveMax   = 250 * time.Millisecond
	transitReserveShare = 5
)

// announcedDeadlineMs is the Fleet-Deadline-Ms to send a peer when this
// driver has `remaining` left before it gives up.
//
// # Why announce less than we enforce
//
// Announcing the full remaining budget makes the peer's local deadline and
// this driver's timer expire at the same instant. A peer whose local source
// is slow would answer honestly — "my source was slow", in its own
// SourceStatus — but that answer is still on the wire when the timer fires,
// so this driver records it as unreachable and logs a miss (#174) that is
// indistinguishable from a peer that is down. That is effectiveDeadline's
// "a machine that answered, described as one that did not", reached from the
// announcing side. Holding a reserve back gives the answer time to travel.
//
// # Why the result is never below 1
//
// A caller may shorten a call below this driver's floor (WithDeadline), so
// the remaining budget can be tiny or already spent. A non-positive value
// would have to be dropped, and a request with no header tells the peer
// there is no bound at all — strictly worse than one that is too small. 1ms
// is the smallest value the peer still reads as a bound (§3.3), so it fails
// fast with its own report.
//
// This is not the transit margin (WithTransitMargin): that is added upward
// to how long this driver waits; this is taken downward from what it
// announces. They are kept separate so neither can cancel the other.
func announcedDeadlineMs(remaining time.Duration) int64 {
	reserve := min(transitReserveMax, remaining/transitReserveShare)
	if ms := (remaining - reserve).Milliseconds(); ms >= 1 {
		return ms
	}
	return 1
}

// announceDeadline sets Fleet-Deadline-Ms from ctx's deadline, if it has
// one. It is the only place that header is set, so every construction site
// holds the same reserve.
func announceDeadline(ctx context.Context, h http.Header) {
	if dl, ok := ctx.Deadline(); ok {
		h.Set("Fleet-Deadline-Ms", strconv.FormatInt(announcedDeadlineMs(time.Until(dl)), 10))
	}
}

// bounded applies this driver's declared deadline, or the caller's if
// shorter (§4.4: "a req may supply a shorter deadline; never a longer
// one").
func (d *Driver) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	own := d.now().Add(d.effectiveDeadline())
	if dl, ok := ctx.Deadline(); ok && dl.Before(own) {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, own)
}

// do performs one request. token, when non-empty, replaces this driver's own
// credential — that is how a proxied call presents the original caller's
// authority (§13).
func (d *Driver) do(ctx context.Context, req fleet.Request, method, path string, body any, out any) error {
	return d.doWithHeaders(ctx, req, method, path, nil, body, out)
}

// doWithHeaders is do with extra request headers — the one call that needs
// them is Send carrying the human-relay assertion (#180 L3).
func (d *Driver) doWithHeaders(ctx context.Context, req fleet.Request, method, path string, headers map[string]string, body any, out any) error {
	token, behalf, ok := d.bearerFor(req)
	if !ok {
		return ErrNoCallerAuthority
	}
	// A peer already known to be down is not dialled: see down.go.
	if ferr := d.admit(); ferr != nil {
		return ferr
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("remote: encoding request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, d.base+path, rdr)
	if err != nil {
		return fmt.Errorf("remote: building request: %w", err)
	}
	// The caller's authority, never this driver's — it has none (§13).
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if behalf != "" {
		// Who asked, for the peer's audit trail (§6 requirement 4). Never
		// a substitute for the credential above.
		httpReq.Header.Set("Fleet-On-Behalf-Of", behalf)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	// Tell the peer a bound slightly inside the one we are enforcing, so it
	// can fail fast rather than working on a request we have already given
	// up on (§3.3) — and so its own answer still reaches us (#175).
	announceDeadline(ctx, httpReq.Header)

	started := d.now()
	resp, err := d.client.Do(httpReq)
	if err != nil {
		// A transport failure is unreachable, never not_found. The peer did
		// not answer, so nothing at all is known about the session
		// (api-http.md §2: "the single most important line in this
		// document").
		return d.transportFailure(ctx, req, method, path, behalf, started, err)
	}
	defer resp.Body.Close()
	d.noteReached()

	// A response is a domain answer, whatever the status: the peer is up
	// and speaking the protocol (muster #67). Excluded here are the two
	// paths capability discovery itself uses — RefreshCapabilities and
	// peerBuild — so a probe triggered below does not immediately trigger
	// another one recursively.
	if path != "/v1/runtimes" && path != "/v1/health" && !strings.HasPrefix(path, "/v1/whoami") {
		d.noteSuccessfulContact(req)
	}

	if resp.StatusCode >= 400 {
		return decodeError(resp, d.machine)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("remote: decoding response from %s: %w", d.machine, err)
	}
	return nil
}

// noEnvelopeFormat is decodeError's message for a status with no envelope.
// Shared with routeMissing, which is the one caller that must recognise it.
const noEnvelopeFormat = "peer returned %d with no error envelope"

// routeMissing reports whether err is a peer's router answering that it has
// no such route at all — a bare 404 or 405 with no error envelope, which is
// what a build that predates an endpoint answers. Every route this service
// defines answers failures WITH an envelope, so a bare one on a known route
// is not "no such session".
func routeMissing(err error) bool {
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		return false
	}
	return fe.Message == fmt.Sprintf(noEnvelopeFormat, http.StatusNotFound) ||
		fe.Message == fmt.Sprintf(noEnvelopeFormat, http.StatusMethodNotAllowed)
}

// decodeError turns a peer's error envelope back into a typed error,
// preserving the kind rather than flattening every failure into "something
// went wrong". The 404/504 distinction in particular must survive the trip.
func decodeError(resp *http.Response, machine fleet.MachineId) error {
	var env fleet.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err == nil && env.Error.Kind != "" {
		e := env.Error
		if e.Machine == "" {
			e.Machine = machine
		}
		return &e
	}
	// A peer that answered with a status but no parseable envelope is
	// misbehaving, not unreachable — it answered. Map by status rather than
	// assuming the worst.
	kind := fleet.ErrorInvalid
	switch resp.StatusCode {
	case http.StatusNotFound:
		kind = fleet.ErrorNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = fleet.ErrorUnauthorized
	case http.StatusConflict:
		kind = fleet.ErrorConflict
	case http.StatusNotImplemented:
		kind = fleet.ErrorUnsupported
	case http.StatusGatewayTimeout:
		kind = fleet.ErrorUnreachable
	}
	return &fleet.Error{
		Kind:    kind,
		Message: fmt.Sprintf(noEnvelopeFormat, resp.StatusCode),
		Machine: machine,
	}
}

// List asks the peer for its LOCAL view only (§13.1) and adopts the
// SourceStatus it returns (§13.2).
//
// Both rules are load-bearing and both are one line of code, which is
// exactly why they are easy to get wrong:
//
//   - scope=local is what keeps fan-out one hop deep. Without it two
//     mutually-configured peers query each other forever, or — worse —
//     double-count and look fine.
//   - the returned envelope's sources are passed through untouched. A peer
//     can answer promptly AND report itself degraded; manufacturing a fresh
//     "ok" from the mere fact that the HTTP call succeeded would flatten
//     that into a confident envelope built on a self-declared unreliable
//     source.
func (d *Driver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	q := url.Values{}
	q.Set("scope", "local") // §13.1 — never "fleet"
	if filter.Status != "" {
		q.Set("status", string(filter.Status))
	}
	if filter.Agent != "" {
		q.Set("agent", string(filter.Agent))
	}
	if filter.CwdPrefix != "" {
		q.Set("cwdPrefix", filter.CwdPrefix)
	}
	labelKeys := make([]string, 0, len(filter.Labels))
	for k := range filter.Labels {
		labelKeys = append(labelKeys, k)
	}
	sort.Strings(labelKeys)
	for _, k := range labelKeys {
		q.Add("label", k+fleet.LabelSeparator+filter.Labels[k])
	}

	var out fleet.Collection[fleet.Session]
	if err := d.do(ctx, req, http.MethodGet, "/v1/sessions?"+q.Encode(), nil, &out); err != nil {
		// §5.7: a failed read is never an empty list. The peer contributes a
		// SourceStatus saying it did not answer.
		src := fleet.SourceStatus{
			Machine:    d.machine,
			Status:     sourceStateFor(err),
			Error:      err.Error(),
			ObservedAt: d.now(),
		}
		return fleet.NewCollection([]fleet.Session{}, []fleet.SourceStatus{src})
	}
	// A peer that predates labels ignores `label=` and answers with every
	// session it has, none carrying the key (muster #153). Those are not
	// matches, and an empty list would claim the peer has none: say instead
	// that this source could not answer the question. A new build always
	// writes `labels`, so a nil map can only come from an old one. An old peer
	// with no sessions at all is indistinguishable — and its true answer is
	// the same zero, since it cannot hold labels.
	if len(filter.Labels) > 0 {
		for _, sess := range out.Items() {
			if sess.Labels == nil {
				return fleet.NewCollection([]fleet.Session{}, []fleet.SourceStatus{{
					Machine:    d.machine,
					Status:     fleet.SourceDegraded,
					Error:      "peer does not carry session labels (older build), so a label filter cannot be answered there",
					ObservedAt: d.now(),
				}})
			}
		}
	}
	// Adopted verbatim: out already carries the peer's own sources, and
	// Collection's decoder recomputed `complete` from them.
	return out, nil
}

// ListClosed reads the peer's own closed-session records (muster #179),
// asking for its LOCAL view only (§13.1) and adopting the SourceStatus it
// returns (§13.2) — List's two rules, for the same reasons.
//
// A peer on a build that predates the route answers with its router's bare
// 404. That is reported as a source that cannot answer this question, never
// as a peer with nothing closed: an empty list from it would be a claim it
// never made.
func (d *Driver) ListClosed(ctx context.Context, req fleet.Request, since time.Time) (fleet.Collection[fleet.ClosedSession], error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	q := url.Values{}
	q.Set("scope", "local") // §13.1 — never "fleet"
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out fleet.Collection[fleet.ClosedSession]
	if err := d.do(ctx, req, http.MethodGet, "/v1/sessions/closed?"+q.Encode(), nil, &out); err != nil {
		src := fleet.SourceStatus{
			Machine:    d.machine,
			Status:     sourceStateFor(err),
			Error:      err.Error(),
			ObservedAt: d.now(),
		}
		if routeMissing(err) {
			src.Status = fleet.SourceDegraded
			src.Error = "peer does not keep closed-session records (older build)"
		}
		return fleet.NewCollection([]fleet.ClosedSession{}, []fleet.SourceStatus{src})
	}
	return out, nil
}

// sourceStateFor maps a transport-level failure onto the closed SourceState
// set (§9). Note that unauthorized is kept distinct from unreachable: "the
// peer refused me" and "the peer never answered" are different operational
// problems with different fixes, and collapsing them sends an operator to
// check the network when the real answer is a token.
func sourceStateFor(err error) fleet.SourceState {
	var fe *fleet.Error
	if errors.As(err, &fe) {
		switch fe.Kind {
		case fleet.ErrorUnauthorized:
			return fleet.SourceUnauthorized
		case fleet.ErrorUnreachable:
			return fleet.SourceUnreachable
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fleet.SourceUnreachable
	}
	return fleet.SourceDegraded
}

// State reads one session from the peer.
func (d *Driver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var out fleet.Session
	path := fmt.Sprintf("/v1/machines/%s/sessions/%s",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if err := d.do(ctx, req, http.MethodGet, path, nil, &out); err != nil {
		return fleet.SessionState{}, err
	}
	return out.State, nil
}

// createBody is the POST /v1/machines/{machine}/sessions payload
// (api-http.md §3.3). Machine is absent deliberately: it travels in the URL.
type createBody struct {
	Runtime    fleet.RuntimeId    `json:"runtime,omitempty"`
	Cwd        fleet.AbsolutePath `json:"cwd"`
	Agent      fleet.AgentId      `json:"agent,omitempty"`
	Model      string             `json:"model,omitempty"`
	Effort     string             `json:"effort,omitempty"`
	Name       string             `json:"name,omitempty"`
	Prompt     string             `json:"prompt,omitempty"`
	ContextRef fleet.AbsolutePath `json:"contextRef,omitempty"`
	// TrustCwd travels because the consent is about the DIRECTORY, and the
	// directory is on the peer. Dropping it here would produce the failure it
	// exists to prevent, one machine away and only there.
	TrustCwd bool `json:"trustCwd,omitempty"`

	// Everything a session needs to be the same KIND of session on a peer as it
	// is locally. A field dropped here does not fail: it produces a session that
	// starts, looks healthy, and is missing something — one machine away, which
	// is the hardest place to notice it.
	Env            map[string]string  `json:"env,omitempty"`
	Resume         string             `json:"resume,omitempty"`
	PermissionMode string             `json:"permissionMode,omitempty"`
	Consents       []fleet.PromptKind `json:"consents,omitempty"`
	// ConversationId travels with the create (muster #224), the same
	// shape as Labels three fields down: Create refuses a peer that has not
	// confirmed it carries the field rather than send it there to be silently
	// dropped by a build that predates it — see requireConversationId.
	ConversationId string `json:"conversationId,omitempty"`
	// McpConfig names PATHS, and the paths are the peer's. Forwarded rather
	// than resolved here for the same reason ContextRef is: this machine's
	// filesystem is not the one the session will read from, and a proxy that
	// checked its own would either refuse a create that is perfectly valid on
	// the peer or approve one that is not.
	McpConfig []fleet.AbsolutePath `json:"mcpConfig,omitempty"`

	// Settings is launch-time CLI configuration (muster #247), forwarded
	// verbatim: the peer validates the bypass-only pairing against its own
	// request, and a peer that predates the field is refused first —
	// see requireLaunchSettings.
	Settings json.RawMessage `json:"settings,omitempty"`

	// Labels travel with the create (muster #153); Create refuses a peer
	// that would drop them rather than send them there to be ignored.
	Labels map[string]string `json:"labels,omitempty"`

	// Marker is the session-type stamp (fleet.SessionSpec.Marker) — dropping
	// it does not fail the create, it just leaves the peer's copy invisible
	// to whatever tooling on THAT machine groups sessions by type.
	Marker string `json:"marker,omitempty"`

	// RemoteControl must stay a *bool, forwarded as the caller's pointer, not
	// copied through a plain bool. fleet.SessionSpec.RemoteControl is a
	// tri-state on purpose: nil ("give me whatever a first-class session
	// gets") and false ("deliberately unreachable") are different requests,
	// and a bool has no zero value that means neither — it is always false,
	// which is indistinguishable from "the caller asked to be unreachable".
	// Encoding that through a plain bool would silently turn every unaware
	// caller's create into an explicit opt-out, on the one substrate
	// (federated) where the caller has no way to notice, which is precisely
	// the defect #20 introduced this field to close.
	//
	// omitempty is safe on the pointer specifically because
	// encoding/json's emptiness check for a pointer is nil-ness, not the
	// pointed-to value: a nil RemoteControl omits the key (peer sees
	// "absent", i.e. give the default), while &false still encodes as
	// `"remoteControl":false` (peer sees an explicit refusal). The same tag
	// on a plain bool would instead test the bool's own zero value, so
	// `false` — whether the caller set it or never touched the field — goes
	// unsent either way, which is exactly the collapse this field exists to
	// prevent.
	RemoteControl *bool `json:"remoteControl,omitempty"`
}

// Create starts a session on the peer, forwarding the caller's idempotency
// key unchanged (§10).
//
// Forwarding rather than regenerating matters more here than anywhere else
// in this driver. §10's disaster is a federated create that times out in
// transit and gets retried, producing two agents in one working directory. A
// proxy that minted its own key per attempt would defeat the mechanism
// precisely when it is needed, since the retry would arrive carrying a
// different key and read as a different request.
func (d *Driver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if key == "" {
		return fleet.Session{}, &fleet.Error{
			Kind:    fleet.ErrorInvalid,
			Message: "create: idempotency key is required (§10)",
			Machine: d.machine,
		}
	}
	body := createBody{
		Runtime: spec.Runtime, Cwd: spec.Cwd, Agent: spec.Agent,
		Model: spec.Model, Effort: spec.Effort, Name: spec.Name,
		Prompt: spec.Prompt, ContextRef: spec.ContextRef,
		TrustCwd: spec.TrustCwd, Env: spec.Env, Resume: spec.Resume,
		PermissionMode: spec.PermissionMode, Consents: spec.Consents,
		McpConfig: spec.McpConfig, Settings: spec.Settings,
		Marker: spec.Marker, RemoteControl: spec.RemoteControl,
		Labels:         spec.Labels,
		ConversationId: spec.ConversationId,
	}
	if len(spec.Labels) > 0 {
		if err := d.requireLabels(ctx, req); err != nil {
			return fleet.Session{}, err
		}
	}
	if spec.ConversationId != "" {
		if err := d.requireConversationId(ctx, req); err != nil {
			return fleet.Session{}, err
		}
	}
	if len(spec.Settings) > 0 {
		if err := d.requireLaunchSettings(ctx, req, spec); err != nil {
			return fleet.Session{}, err
		}
	}
	var out fleet.Session
	path := "/v1/machines/" + url.PathEscape(string(d.machine)) + "/sessions"
	if err := d.doWithKey(ctx, req, http.MethodPost, path, body, key, &out); err != nil {
		return fleet.Session{}, err
	}
	// muster #84/#85/#86: adopt the peer's own answer whole, the same
	// §13.2 rule List already follows for a relayed read — this machine never
	// learns anything about the create beyond what the peer reports, and
	// discarding everything but SessionRef here to let the HTTP handler
	// rebuild a Session from the caller's own request is #84's defect
	// travelling across a federation hop.
	return out, nil
}

// transportFailure turns a call the peer never answered into the error both
// do and doWithKey return, and logs it on the way out (muster #174).
//
// # Why it logs
//
// A failed read is folded into a SourceStatus by List and handed upward as
// data, which is correct for the envelope (§5.7) and left the requesting
// daemon with no record of its own at all. Measured: a consumer counted
// dozens of partial polls an hour while the daemon that produced them logged
// one line in a day — and that one came from the startup capability probe,
// not the read path. The frequency could only be read off the consumers.
//
// So every miss is logged here, once per failed call, with no suppression:
// suppression is exactly what hid it. A response of any status is a domain
// answer and never reaches this function, so a healthy fleet logs nothing.
//
// # What the line carries
//
// after is the measured latency. budget is the deadline that actually fired,
// measured from the same start; bound is this driver's own limit. They are
// printed side by side because they differ in the case that matters: a
// caller may shorten a deadline below this driver's floor (§4.4), and
// "budget=2s bound=32s" says in one line that the cliff is the caller's own
// Fleet-Deadline-Ms, not a constant here to be tuned. on_behalf_of names the
// principal that asked, which is how that caller is found.
//
// A caller that hung up is not a peer miss — the requester went away, the
// peer may well have been answering — so it returns the same error but is
// not logged, rather than logged as the peer's fault.
func (d *Driver) transportFailure(ctx context.Context, req fleet.Request, method, path, behalf string, started time.Time, err error) *fleet.Error {
	after := d.now().Sub(started).Round(time.Millisecond)
	ferr := &fleet.Error{
		Kind:      fleet.ErrorUnreachable,
		Message:   fmt.Sprintf("no answer from %s after %s: %v", d.machine, after, err),
		Machine:   d.machine,
		Retryable: true,
	}
	if errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
		return ferr
	}
	budget := "none"
	var budgetDur time.Duration
	if dl, ok := ctx.Deadline(); ok {
		budgetDur = dl.Sub(started)
		budget = budgetDur.Round(time.Millisecond).String()
	}
	kind := "transport"
	if errors.Is(err, context.DeadlineExceeded) {
		kind = "deadline"
	}
	// Evidence about the peer, not about this caller's patience (#237).
	if d.countsAsPeerFailure(kind == "deadline", budgetDur) {
		d.noteFailure(req, started)
	}
	// The route, never the query: label filters carry caller-supplied values.
	// The transport's own error repeats the whole URL, query included, so
	// only its cause is logged; the route is already in op.
	route, _, _ := strings.Cut(path, "?")
	cause := err
	var uerr *url.Error
	if errors.As(err, &uerr) {
		cause = uerr.Err
	}
	if behalf == "" {
		behalf = "-"
	}
	log.Printf("remote: peer call failed machine=%s op=%q after=%s budget=%s bound=%s on_behalf_of=%s kind=%s err=%v",
		d.machine, method+" "+route, after, budget, d.effectiveDeadline(), behalf, kind, cause)
	return ferr
}

// doWithKey is do() plus the Idempotency-Key header.
func (d *Driver) doWithKey(ctx context.Context, req fleet.Request, method, path string, body any, key string, out any) error {
	token, behalf, ok := d.bearerFor(req)
	if !ok {
		return ErrNoCallerAuthority
	}
	if ferr := d.admit(); ferr != nil {
		return ferr
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("remote: encoding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, d.base+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("remote: building request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if behalf != "" {
		httpReq.Header.Set("Fleet-On-Behalf-Of", behalf)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", key)
	announceDeadline(ctx, httpReq.Header)

	started := d.now()
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return d.transportFailure(ctx, req, method, path, behalf, started, err)
	}
	defer resp.Body.Close()
	d.noteReached()

	// Same reasoning as do(): a response, whatever the status, is a domain
	// answer and proof the peer is up (muster #67). doWithKey's only
	// caller is Create, never a capability-discovery path, so no exclusion
	// is needed here.
	d.noteSuccessfulContact(req)

	if resp.StatusCode >= 400 {
		return decodeError(resp, d.machine)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Send delivers input to a session on the peer.
//
// A refusal comes back as an ordinary 200 with outcome "refused"
// (api-http.md §3.3), so it is returned as a value and not an error. This
// driver must not "helpfully" convert it: mapping a refusal to an error
// would train callers to retry it, which is exactly what the refusal exists
// to stop.
//
// ResumeIfStranded must travel with the rest of opts (#33). Without it, the
// owning daemon evaluates the flag as false and applies §2.4's refusal to a
// delivery the caller was explicitly retrying to resume — the caller's own
// advice from the first, unconfirmed send becomes impossible to follow on
// exactly the path it was given from. This field is easy to forget again:
// it is not part of the wire body's zero-cost fields, it exists solely to
// be turned on, so a caller who never sets it produces no symptom at all.
//
// ReplaceIfStranded (muster #112) is exactly the same trap, one field
// over: it also has no effect unless forwarded, and a caller who never sets
// it sees no symptom at all — the owning daemon just falls back to the same
// §2.4 refusal ResumeIfStranded's own paragraph above describes.
// routeWireValue renders driver.SendOptions.Route as the wire's own closed
// vocabulary (service/http.go's own switch). Auto is sent as NO field at all,
// not as "auto": an owning peer built before #184 knows only "" and "terminal"
// and rejects anything else with a 400, so naming a value it may not know for a
// send that never asked for one would break exactly the callers that changed
// nothing. An explicit "inbox" is sent as itself — and an older peer's 400 is
// the right answer to it, never a silent downgrade to the terminal.
//
// Kept as its own function so the ONE place this type becomes that string is
// visible in a diff, rather than an inline expression easy to re-derive wrong
// at a second call site later.
//
// #185: any other value is an enabled delivery module's own name — the only
// other thing the service lets through — and travels verbatim. The owning
// machine, which holds the lane, makes the real decision; a peer built before
// #185 answers an unknown route with a 400, which is the right answer to a
// forced request and never a silent downgrade. #257: a human relay's auto
// crosses as auto (no field) together with the human-relay assertion, and the
// owner decides. driver.SendOptions.LiveLaneOnly is deliberately NOT forwarded:
// the owner's own service sets it for the /input it receives.
func routeWireValue(r fleet.Route) string {
	if r == "" || r == fleet.RouteAuto {
		return ""
	}
	return string(r)
}

func (d *Driver) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	// #158: From travels too, Machine included — this is the one hop where
	// the machine field is on the wire at all. The entering machine stamped
	// it; the owning daemon accepts it only because this request arrives as a
	// relay (see the service's stampSender).
	//
	// Route (terminal path v2 / D7, review fix; #184): this is the SAME #33 trap
	// this doc comment already names twice over for ResumeIfStranded and
	// ReplaceIfStranded — a field with no effect unless explicitly forwarded,
	// and a caller who forgets sees no symptom AT ALL until the one day it
	// matters. Dropping it here meant a human's terminal-routed send
	// (a human-facing relay, entering on one machine for a session on another) was
	// evaluated for inbox-eligibility on the OWNING machine as if
	// route:"terminal" had never been asked for — exactly what D7 exists to
	// prevent. Since #257 a human relay's auto is NOT rewritten on the entering
	// machine: it crosses as auto with the human-relay assertion header, and the
	// owner (which alone can see the session's lane) decides. "inbox" is
	// forwarded verbatim, so an owner that cannot honour it answers, rather than
	// this driver deciding for it.
	body := struct {
		Text              string             `json:"text"`
		Submit            bool               `json:"submit"`
		ResumeIfStranded  bool               `json:"resumeIfStranded,omitempty"`
		ReplaceIfStranded bool               `json:"replaceIfStranded,omitempty"`
		Expect            string             `json:"expect,omitempty"`
		From              *fleet.MessageFrom `json:"from,omitempty"`
		Route             string             `json:"route,omitempty"`
	}{Text: text, Submit: opts.Submit, ResumeIfStranded: opts.ResumeIfStranded, ReplaceIfStranded: opts.ReplaceIfStranded, Expect: opts.ExpectComposerDigest, From: opts.From, Route: routeWireValue(opts.Route)}

	var out fleet.DeliveryReceipt
	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/input",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	// #180 L3: a human relay established on this machine travels to the
	// owning one as an assertion; the owner trusts it only as far as it
	// trusts this machine as a relay.
	var headers map[string]string
	if opts.HumanRelay {
		headers = map[string]string{"Fleet-Human-Relay": "1"}
	}
	// #272: likewise the finding that the caller holds the remote-control
	// grant here, so the owning machine can apply its gate to the original
	// caller and not to this machine's credential.
	if opts.RemoteControl {
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Fleet-Remote-Control"] = "1"
	}
	if err := d.doWithHeaders(ctx, req, http.MethodPost, path, headers, body, &out); err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	return out, nil
}

// Interrupt asks the peer to interrupt a session (202, intent only).
func (d *Driver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/interrupt",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if err := d.do(ctx, req, http.MethodPost, path, struct{}{}, nil); err != nil {
		return fleet.Ack{}, err
	}
	return fleet.Ack{Accepted: true}, nil
}

// Close asks the peer to destroy a session (202, intent only).
//
// §5.4's corroboration cannot be performed here, and it is worse across a
// network than it was locally. The local driver could at least compare
// against its own recent sighting; this driver has no sightings of its own —
// it has only the id the req handed it, which is the one thing §5.4 says
// is not identification. Whatever corroboration happens must happen on the
// peer, against evidence that has no field to travel in.
//
// This is the same defect recorded in spec §5.4, observed from the side that
// makes its cost obvious.
func (d *Driver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))

	// Forward the caller's expectation, so §5.4's corroboration happens on
	// the machine that can actually see the session — against the caller's
	// belief, not the far driver's own sighting. Dropping it here would
	// silently downgrade every cross-machine destroy to the weak check,
	// which is the failure this whole envelope exists to prevent.
	if want := req.Expect.StartedAt; want != nil {
		path += "?startedAt=" + url.QueryEscape(want.UTC().Format(time.RFC3339Nano))
	}
	if err := d.do(ctx, req, http.MethodDelete, path, nil, nil); err != nil {
		return fleet.Ack{}, err
	}
	return fleet.Ack{Accepted: true}, nil
}

// Discard clears unsent composer text on a session belonging to the peer (§3).
//
// The caller's digest travels, because corroboration has to happen where the
// composer actually is — against what the CALLER saw, not the far driver's own
// later reading.
//
// opts.Force (muster #136) is forwarded on the wire as its own query
// parameter, never folded into or inferred from expect. This is the one
// place in the whole DiscardOptions ripple that is NOT a local signature
// change: dropping Force here would make the escape hatch work on a local
// driver and silently vanish at the federation boundary — the exact class
// of drift classifyCaptureArgs' own single-source-of-truth exists to
// prevent for capture shape, arriving here for the wire shape instead.
func (d *Driver) Discard(ctx context.Context, req fleet.Request, ref fleet.SessionRef, expectDigest string, opts driver.DiscardOptions) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/discard",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	q := url.Values{}
	if expectDigest != "" {
		q.Set("expect", expectDigest)
	}
	if opts.Force {
		q.Set("force", "true")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var ack fleet.Ack
	if err := d.do(ctx, req, http.MethodPost, path, nil, &ack); err != nil {
		return fleet.Ack{}, err
	}
	return ack, nil
}

// Keys delivers one raw key event to a session belonging to the peer
// (driver.KeySender).
//
// Corroboration is forwarded, never re-derived. `expect` is the digest of the
// screen the ORIGINAL caller read, and it is checked where the session actually
// is — this driver has no screen of its own to compare against, and a proxy
// that substituted its own reading would corroborate a caller's belief against
// something the caller never saw. Same rule Close and Discard already follow.
func (d *Driver) Keys(ctx context.Context, req fleet.Request, ref fleet.SessionRef, key fleet.KeyName, expectDigest string) (fleet.DeliveryReceipt, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/keys",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	q := url.Values{}
	if expectDigest != "" {
		q.Set("expect", expectDigest)
	}
	if want := req.Expect.StartedAt; want != nil {
		q.Set("startedAt", want.UTC().Format(time.RFC3339Nano))
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var receipt fleet.DeliveryReceipt
	if err := d.do(ctx, req, http.MethodPost, path, map[string]any{"key": key}, &receipt); err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	return receipt, nil
}

// Rename changes the id of a session belonging to the peer (§3).
//
// The caller's expectation is forwarded for the same reason Close forwards it:
// corroboration has to happen where the session actually is, against what the
// CALLER saw rather than the far driver's own sighting.
func (d *Driver) Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.RenameAck, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/rename",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if want := req.Expect.StartedAt; want != nil {
		path += "?startedAt=" + url.QueryEscape(want.UTC().Format(time.RFC3339Nano))
	}
	body := map[string]string{"name": to}
	// muster #222: the peer already ran its own title-sync step against
	// its own driver before answering, so whatever fleet.RenameAck it sends —
	// title included, or absent for a peer predating this field — is
	// forwarded to OUR caller verbatim. This driver never implements
	// driver.TitleSyncer itself; there is nothing left here to attempt.
	var ack fleet.RenameAck
	if err := d.do(ctx, req, http.MethodPost, path, body, &ack); err != nil {
		return fleet.RenameAck{}, err
	}
	return ack, nil
}

// Labels forwards a label write to the peer (POST …/labels, muster
// #153), corroborated by startedAt the way Rename is. Implements
// driver.LabelRelayer.
//
// A peer that predates the route answers with its router's bare 404, which
// decodeError alone would read as "no such session". It is reported as
// unsupported instead: the session may be perfectly alive, and the peer
// simply cannot store labels.
func (d *Driver) Labels(ctx context.Context, req fleet.Request, ref fleet.SessionRef, patch map[string]*string) (fleet.Session, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/labels",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if want := req.Expect.StartedAt; want != nil {
		path += "?startedAt=" + url.QueryEscape(want.UTC().Format(time.RFC3339Nano))
	}
	body := map[string]map[string]*string{"labels": patch}
	var out fleet.Session
	if err := d.do(ctx, req, http.MethodPost, path, body, &out); err != nil {
		if routeMissing(err) {
			return fleet.Session{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("peer %s has no label route (older build)", d.machine),
				Machine: d.machine,
			}
		}
		return fleet.Session{}, err
	}
	return out, nil
}

var _ driver.LabelRelayer = (*Driver)(nil)

// SetRemoteControl asks the peer to turn a session's remote control on or off
// (202, intent only; muster #269).
//
// The peer applies its own grant and its own capability check, so a refusal for
// either arrives as the peer's own typed error and is adopted as it is. A peer
// that predates the route answers with its router's bare 404, which decodeError
// alone would read as "no such session": it is reported as unsupported instead,
// because the session may be perfectly alive and the peer simply has no such
// verb.
func (d *Driver) SetRemoteControl(ctx context.Context, req fleet.Request, ref fleet.SessionRef, enabled bool) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/remote-control",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if want := req.Expect.StartedAt; want != nil {
		path += "?startedAt=" + url.QueryEscape(want.UTC().Format(time.RFC3339Nano))
	}
	var out fleet.Ack
	if err := d.do(ctx, req, http.MethodPost, path, map[string]bool{"enabled": enabled}, &out); err != nil {
		if routeMissing(err) {
			return fleet.Ack{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("peer %s has no remote-control route (older build)", d.machine),
				Machine: d.machine,
			}
		}
		return fleet.Ack{}, err
	}
	return out, nil
}

var _ driver.RemoteControlSetter = (*Driver)(nil)

// Composer asks the peer what is sitting unsent in a session's composer
// (GET …/composer, muster #276).
//
// The peer applies its own grant to the asserted caller. A peer that predates the
// route answers with its router's bare 404, which decodeError alone would read as
// "no such session"; it is reported as unsupported instead, because the session
// may be perfectly alive and the peer simply has no such read.
func (d *Driver) Composer(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.ComposerRead, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/composer",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if want := req.Expect.StartedAt; want != nil {
		path += "?startedAt=" + url.QueryEscape(want.UTC().Format(time.RFC3339Nano))
	}
	var out fleet.ComposerRead
	if err := d.do(ctx, req, http.MethodGet, path, nil, &out); err != nil {
		if routeMissing(err) {
			return fleet.ComposerRead{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("peer %s has no composer route (older build)", d.machine),
				Machine: d.machine,
			}
		}
		return fleet.ComposerRead{}, err
	}
	return out, nil
}

var _ driver.ComposerReader = (*Driver)(nil)

// Respond answers a prompt on a session belonging to the peer (§3).
//
// Like Send, a refusal comes back as an ordinary 200 carrying an outcome and
// is returned as a value: the peer decided the session was not at a prompt,
// which is information rather than a fault.
func (d *Driver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var out fleet.DeliveryReceipt
	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/respond",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if err := d.do(ctx, req, http.MethodPost, path, resp, &out); err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	return out, nil
}
