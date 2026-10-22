package fleet

// MachineInfo is one entry of GET /v1/machines (api-http.md §3.1).
type MachineInfo struct {
	Machine    MachineId   `json:"machine"`
	Self       bool        `json:"self"`
	Status     SourceState `json:"status"`
	ObservedAt Timestamp   `json:"observedAt"`
	// Build identifies the code this machine is running — muster #121.
	// For self it is always known (fleet.SelfBuild(), read once at startup).
	// For a peer it is whatever the last successful probe learned; absent a
	// probe yet, or a peer driver that cannot report one, this reads as the
	// zero value (Known: false) — never a plausible-looking default. See
	// fleet.Build's own doc comment for why an unknown build must never
	// compare equal to anything, including itself.
	Build Build `json:"build"`
	// MaxInputBytes is the effective limit this machine enforces on
	// `prompt` (create) and `text` (input) — muster #130, the same
	// ask-do-not-infer move #121 made for Build above: a caller sizing a
	// dispatch brief should be able to ask rather than discover the
	// boundary by exceeding it, and that matters more once the value is
	// machine-local and can differ across the fleet.
	//
	// For self this is always known and positive — every deployment has an
	// effective limit, configured or defaulted (cmd/muster/config.go,
	// internal/service.Service.MaxInputBytes). For a peer it is whatever
	// the last successful probe learned; absent a probe yet, or a peer
	// driver that cannot report one, this reads as the zero value.
	//
	// Unlike Build, no separate Known flag is needed to tell "unknown" from
	// "a real answer": a real effective limit is never zero (SetMaxInputBytes
	// refuses non-positive values), so zero is unambiguous on its own —
	// §5.7's "absence and failure are different answers" holds here by
	// construction rather than by an extra field.
	MaxInputBytes int `json:"maxInputBytes,omitempty"`
	// Address is the address this service is currently using to reach that
	// machine — the last one that answered — and is present only when the peer
	// is configured with more than one (muster #335), so a reader can tell
	// which connection a machine that is known by several is on. Absent for
	// self and for a peer with a single address. Until some address has
	// answered it is the first one configured.
	Address string `json:"address,omitempty"`
	// Peer is this service's own standing on that machine — whether it lists
	// this service back and what it grants the credential this service
	// presents there (muster #154). Federation is two hand-kept halves
	// of configuration, and before this the only way to learn they disagreed
	// was a 403 at the moment of need.
	//
	// Always present. See PeerStanding for how "not listed" and "nobody
	// could tell" stay different answers.
	Peer PeerStanding `json:"peer"`
}

// PeerStanding is this service's registration on one machine, as that
// machine itself reported it (muster #154).
//
// It is gathered on the peer probe that already learns build and
// maxInputBytes, by asking the peer's whoami about the credential this
// service presents there. Three answers must not collapse into each other:
//
//   - observed, listsMeBack false — the peer answered and does not list us.
//   - observed, grantsToMe [] — the peer answered and our credential holds
//     nothing there, or matches no principal at all. A real negative.
//   - assumed — nobody could tell: the peer was never reached, answered too
//     long ago (the same staleness bound as capabilities), runs a build that
//     predates this read, or this service presents no credential of its own
//     to it. listsMeBack is null and grantsToMe is [] — a floor, exactly as an
//     unreached peer's capabilities are, never a claim of absence.
type PeerStanding struct {
	// ListsMeBack is whether that machine's own peer roster names this
	// service. Null when it did not say — including a peer credential without
	// `read` there, since the roster is what `read` guards.
	ListsMeBack *bool `json:"listsMeBack"`
	// GrantsToMe is every grant that machine attaches to the credential this
	// service presents to it. Never another principal's.
	GrantsToMe []string `json:"grantsToMe"`
	// Source is CapabilitySource's provenance, reused for the same fact.
	Source CapabilitySource `json:"source"`
	// ObservedAt is when the answer arrived, on this machine's clock. Absent
	// under "assumed".
	ObservedAt *Timestamp `json:"observedAt,omitempty"`
	// Auth is how this service authenticates to that machine: PeerAuthKey (it
	// signs each request, no reusable secret crosses the wire) or PeerAuthToken
	// (a bearer credential). A fact of this machine's own configuration, so it
	// is present whatever Source says; absent only on the self item (muster
	// #350).
	Auth string `json:"auth,omitempty"`
}

// How a service authenticates to a peer (PeerStanding.Auth).
const (
	PeerAuthKey   = "key"
	PeerAuthToken = "token"
)

// AssumedPeerStanding is the conservative floor: nothing confirmed.
func AssumedPeerStanding() PeerStanding {
	return PeerStanding{GrantsToMe: []string{}, Source: CapabilitiesAssumed}
}

// RuntimeStatus is whether a runtime an operator enabled can take work right
// now (api-http.md §3.1, muster #297).
type RuntimeStatus string

const (
	// RuntimeAvailable: a driver is registered and answering.
	RuntimeAvailable RuntimeStatus = "available"
	// RuntimeUnavailable: the operator enabled the runtime but no working
	// driver exists for it at this moment. RuntimeInfo.Cause says why.
	RuntimeUnavailable RuntimeStatus = "unavailable"
)

// UnavailableCause is the machine-readable reason an enabled runtime has no
// working driver. A client branches on it instead of parsing Reason.
//
// The set is open on read: a decoder must accept a value it does not know and
// treat it as "unavailable, reason unstated" — a later build may name a cause
// this one never heard of.
type UnavailableCause string

const (
	// CauseNotInstalled: the runtime's binary was not found.
	CauseNotInstalled UnavailableCause = "not-installed"
	// CauseNotExecutable: something is there, and it cannot be run.
	CauseNotExecutable UnavailableCause = "not-executable"
	// CauseProbeTimeout: the version probe did not answer within its
	// deadline. The only cause the service retries on its own — a binary
	// that is slow under load is the same binary a minute later.
	CauseProbeTimeout UnavailableCause = "probe-timeout"
	// CauseProbeFailed: the probe ran and failed some other way.
	CauseProbeFailed UnavailableCause = "probe-failed"
	// CauseStartFailed: the runtime was refused for a reason that is not the
	// binary — its configuration, or this platform.
	CauseStartFailed UnavailableCause = "start-failed"
)

// Retryable reports whether the service tries again on its own. Everything
// else needs somebody to change something first.
func (c UnavailableCause) Retryable() bool { return c == CauseProbeTimeout }

// RuntimeInfo is one entry of GET /v1/runtimes (api-http.md §3.1).
//
// Status, Cause and Reason were added by muster #297. A row from a build that
// predates them has none of the three, and that reads as available: such a
// build only ever listed runtimes with a working driver. Use Available, not a
// comparison against RuntimeAvailable, so the absent value is read the same
// way everywhere.
type RuntimeInfo struct {
	Machine      MachineId          `json:"machine"`
	Runtime      RuntimeId          `json:"runtime"`
	Capabilities DriverCapabilities `json:"capabilities"`
	// Status is "available" or "unavailable". This build sets it on every row
	// it emits.
	Status RuntimeStatus `json:"status,omitempty"`
	// Cause is set only when the runtime is unavailable.
	Cause UnavailableCause `json:"cause,omitempty"`
	// Reason is a sentence for people, set only when the runtime is
	// unavailable. It never carries the resolved path of the runtime's binary.
	Reason string `json:"reason,omitempty"`
	// Posture is what this runtime can enforce of a create's `posture`
	// (muster #344), derived from its capabilities. Absent on an unavailable
	// runtime and on a peer whose capabilities have not been obtained: nobody
	// has said, which is "unknown", never "nothing enforceable".
	Posture *PostureSupport `json:"posture,omitempty"`
}

// Available reports whether the row describes a runtime with a working
// driver. An absent Status is available.
func (r RuntimeInfo) Available() bool { return r.Status != RuntimeUnavailable }
