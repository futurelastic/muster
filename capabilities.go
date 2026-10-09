package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// PinSupport is the per-field breakdown of DriverCapabilities.SupportsPin
// (§4.3): a driver may be able to pin a model without being able to pin an
// effort level, or vice versa.
type PinSupport struct {
	Model  bool `json:"model"`
	Effort bool `json:"effort"`
	Agent  bool `json:"agent"`
}

// CapabilitySource is the provenance of a capability declaration.
//
// It exists because §5.7 — absence and failure are different answers — applies
// here too, and this is the fourth place it turned up. A DriverCapabilities
// with every flag false means "this driver supports nothing"; a peer that has
// never answered produces exactly that value, meaning "nobody has told me
// anything". Without provenance the two are one value, and a permanently
// misconfigured peer is indistinguishable from a deliberately minimal one.
//
// The shape is borrowed rather than invented: §2.3 already solved this for
// session state with status + confidence + evidence, and a design that solves
// the same problem two different ways has two things to learn instead of one.
type CapabilitySource string

const (
	// CapabilitiesObserved means the driver these describe reported them.
	// A local driver is always this: it is describing itself.
	CapabilitiesObserved CapabilitySource = "observed"
	// CapabilitiesAssumed means nobody has reported them and these are a
	// conservative floor. A remote driver whose peer has not answered is
	// this, and must never be read as the peer's real answer.
	CapabilitiesAssumed CapabilitySource = "assumed"
)

func (c CapabilitySource) valid() bool {
	return c == CapabilitiesObserved || c == CapabilitiesAssumed
}

func (c CapabilitySource) MarshalJSON() ([]byte, error) {
	if !c.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid CapabilitySource", string(c))
	}
	return json.Marshal(string(c))
}

// UnmarshalJSON rejects the empty string along with anything else outside the
// set. An absent provenance is not the same fact as "assumed" and must not be
// silently coerced into it — the same rule Status and Confidence follow.
func (c *CapabilitySource) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := CapabilitySource(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid CapabilitySource", raw)
	}
	*c = v
	return nil
}

// RemoteControlSupport is what a driver declares about turning a RUNNING
// session's remote control on or off (muster #269).
//
// It is a pointer on DriverCapabilities, absent for a driver that cannot toggle
// at all, so a client reads "is there a control to offer" as the field's
// presence and never has to interpret a pair of falses.
type RemoteControlSupport struct {
	// Toggle reports that POST …/remote-control {enabled:true} is honoured:
	// the driver can turn remote control on, and reconnect a failed channel.
	Toggle bool `json:"toggle"`
	// Off reports that {enabled:false} is honoured. A runtime that can turn
	// remote control on but has no way to turn it off declares false here, and
	// the verb refuses enabled:false as unsupported rather than emulate it.
	Off bool `json:"off"`
}

// DriverCapabilities is what a driver declares about itself (§4.3). Callers
// must consult this (GET /v1/runtimes, api-http.md §3.1) and degrade rather
// than assume (§5.6) — a driver never silently emulates a capability it
// lacks.
type DriverCapabilities struct {
	// ObservesState reports whether the driver can report status without
	// inference.
	ObservesState bool `json:"observesState"`
	// DeliversRawKeys reports whether the driver can deliver a raw key event
	// to a session's screen (§3 `keys`) and populate
	// SessionState.ScreenDigest (§2.3) to corroborate it.
	//
	// Mirrors ObservesState in shape, not in kind. `keys` is the one
	// operation §5.1 asks every operation not to be — it delivers a literal
	// keystroke, not a question — and muster issue #59 ruled that the
	// honest fix is not to hide the mechanism but to make every driver
	// declare it. A driver over a runtime with no screen to capture reports
	// false and answers driver.ErrUnsupported (§5.6) rather than emulate a
	// screen that does not exist. See docs/spec/session-abstraction.md §3 for
	// why this is a declared, bounded asymmetry and not a resolved one.
	DeliversRawKeys bool `json:"deliversRawKeys"`
	// ObservesControlChannel reports whether the driver can say anything about
	// a session's remote-control channel (SessionState.ControlChannel).
	//
	// It exists so a nil ControlChannel is answerable rather than ambiguous.
	// Without it, "this runtime has no control channel", "this driver never
	// looks" and "it looked and the channel is fine" are one indistinguishable
	// absence — §5.7 at the capability level, and the same argument
	// DeliversRawKeys settled: an escape hatch or an observation every driver
	// is silently assumed to make is a fork; one that must be DECLARED is a
	// documented asymmetry.
	//
	// Like every flag here it inherits `source: assumed` from an unreached
	// peer, so a temporarily unreachable machine can never report a bare false
	// and become permanently incapable in a caller's cache (§4.3, D3).
	ObservesControlChannel bool `json:"observesControlChannel"`
	// ObservesPermissionMode reports whether the driver can read the session's
	// current permission mode (SessionState.PermissionMode, muster #194).
	//
	// It exists so an absent PermissionMode is answerable rather than
	// ambiguous: without it, "this driver never looks" and "it looked and a
	// dialog owned the screen" are one indistinguishable absence, and a client
	// that loops on the field cannot tell "read again" from "this will never
	// arrive". The same argument ObservesControlChannel settled one field above.
	//
	// Like every flag here it inherits `source: assumed` from an unreached peer,
	// so a temporarily unreachable machine can never report a bare false and
	// become permanently incapable in a caller's cache (§4.3, D3).
	ObservesPermissionMode bool `json:"observesPermissionMode"`
	// ReportsRuntimeSurface reports whether this driver can say anything
	// about a session's runtime-operated surface (Session.RuntimeSurface,
	// muster #85).
	//
	// It exists so a nil RuntimeSurface is answerable rather than
	// ambiguous. Without it, "this runtime operates no such surface",
	// "this driver never looks" and "it looked and there is none" are one
	// indistinguishable absence — §5.7 at the capability level, the same
	// argument ObservesControlChannel settled one field above. Like every
	// flag here it inherits `source: assumed` from an unreached peer, so a
	// temporarily unreachable machine can never report a bare false and
	// become permanently incapable in a caller's cache (§4.3, D3).
	ReportsRuntimeSurface bool `json:"reportsRuntimeSurface"`
	// IsolatesEnvironment reports whether the driver starts a session's process
	// with a BUILT environment — a small documented base, the machine's
	// sessionEnv entries and the create's env, and nothing inherited from the
	// service (muster #280). A caller that needs it sets
	// SessionSpec.IsolateEnvironment and a driver reporting false refuses that
	// create as unsupported; a caller can also read this flag first. Like every
	// flag here it inherits `source: assumed` from an unreached peer.
	IsolatesEnvironment bool `json:"isolatesEnvironment"`
	// ConfirmsDelivery reports whether the driver can distinguish
	// "submitted" from "queued".
	ConfirmsDelivery bool `json:"confirmsDelivery"`
	// SupportsResume reports whether sessions survive a service restart.
	SupportsResume bool `json:"supportsResume"`
	// DeliversToInbox reports whether this driver has an inbox delivery
	// path wired and reachable for at least some targets (muster
	// #119, #122) — never whether any one call will actually take it,
	// which stays capability-detected per target regardless of this flag.
	//
	// #122 filed the gap this closes: #119 shipped and deployed with this
	// path unreachable, and the only way an operator could tell was
	// reading a receipt's wording and recognising which surface it named.
	// Same argument #121 made for `build` — "a caller should be able to
	// ask" — applied to a different question. True means an
	// InboxResolver is configured; false means every delivery falls
	// through to the pane path unconditionally, the same as before #119
	// existed. Like every flag in this block it is covered by `source`:
	// an unreached peer reports it under `source: assumed`, never as the
	// peer's confirmed answer.
	DeliversToInbox bool `json:"deliversToInbox"`

	SupportsPin PinSupport `json:"supportsPin"`

	// RemoteControl declares whether a running session's remote control can be
	// turned on and off through the API (muster #269). Absent means the driver
	// cannot — and, like every flag in this block, an unreached peer reports it
	// under source: assumed rather than as that peer's confirmed answer.
	RemoteControl *RemoteControlSupport `json:"remoteControl,omitempty"`

	// DeliveryModules reports the optional external delivery modules this
	// machine's driver has enabled and how each is wired (#185; see
	// DeliveryModuleStatus). Absent means none are enabled — the built-in
	// terminal path is the only lane, which is a first-class state and not a
	// fault.
	DeliveryModules []DeliveryModuleStatus `json:"deliveryModules,omitempty"`

	// DeadlineMs is mandatory (§4.4): "a driver that can block without a
	// bound is a specification violation, not a slow driver." Measured
	// directly against a stopped peer, an undeadlined call was still
	// blocked with no result after seven seconds. Zero or negative fails
	// Validate.
	DeadlineMs int64 `json:"deadlineMs"`

	// Source is where this declaration came from (§4.3). Callers that act
	// on a capability must consult it: "assumed" means the values below are
	// a floor nobody confirmed, not an answer.
	Source CapabilitySource `json:"source"`

	// ObservedAt is when the declaration was obtained, or nil if it never
	// was. Freshness is deliberately left to the caller to judge rather
	// than collapsed into a boolean here — the same reasoning as §11, which
	// reports clocks and lets callers compute skew instead of pretending to
	// have decided for them.
	ObservedAt *Timestamp `json:"observedAt,omitempty"`
}

// ErrNoDeadline is returned by Validate when DeadlineMs is not a positive
// number — §4.4's rule made mechanically checkable rather than only stated
// in prose.
var ErrNoDeadline = errors.New("fleet: DriverCapabilities.DeadlineMs must be > 0 (§4.4: every driver declares a deadline)")

// Assumed marks a declaration as an unconfirmed floor. A driver reporting
// values nobody gave it uses this, so a caller can tell the difference between
// a minimal peer and an unreached one.
func (c DriverCapabilities) Assumed() DriverCapabilities {
	c.Source = CapabilitiesAssumed
	c.ObservedAt = nil
	return c
}

// Observed marks a declaration as reported by the driver it describes.
func (c DriverCapabilities) Observed(at Timestamp) DriverCapabilities {
	c.Source = CapabilitiesObserved
	c.ObservedAt = &at
	return c
}

// Validate enforces §4.4. Intended to run once, at driver registration
// (see internal/service.Service.RegisterLocalDriver /
// RegisterPeerDriver), so an undeadlined driver is rejected before it ever
// serves a request rather than discovered by a caller blocking forever.
func (c DriverCapabilities) Validate() error {
	if c.DeadlineMs <= 0 {
		return ErrNoDeadline
	}
	return nil
}
