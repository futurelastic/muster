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
	// Models is which models a create on this runtime may name (muster #400).
	// This build sets it on every row it emits; a row from a build that predates
	// it, or from a peer that does, has none, and the service reads that as
	// NoModels() — "nobody said", never "no model works".
	Models *RuntimeModels `json:"models,omitempty"`
	// Grants is the state of each provider sign-in this machine holds for the
	// runtime (muster #451): present only on a runtime that keeps one and only on
	// this machine's own rows. Absent says nobody reported, never "signed out" —
	// a peer's row has none, and so does a runtime that holds no sign-in.
	Grants []RuntimeGrant `json:"grants,omitempty"`
}

// GrantState is where a provider sign-in stands (muster #451). The set is open on
// read: a client must treat a state it does not know as not usable.
type GrantState string

const (
	// GrantOK: signed in and renewing; sessions created now get a working token.
	GrantOK GrantState = "ok"
	// GrantSignedOut: no sign-in is stored.
	GrantSignedOut GrantState = "signed-out"
	// GrantDegraded: the last renewal failed for a reason that is not the
	// sign-in's fault (the provider unreachable or failing) and is being retried.
	// The token in sessions may still be good; accessExpiresAt says until when.
	GrantDegraded GrantState = "degraded"
	// GrantRevoked: the provider refused the stored refresh token. Nothing renews
	// it; a person signs in again. Sessions on the provider fail until then.
	GrantRevoked GrantState = "revoked"
)

// RuntimeGrant is one entry of RuntimeInfo.Grants. It never carries a credential.
type RuntimeGrant struct {
	// Provider is the runtime's own id for the provider ("openai").
	Provider string     `json:"provider"`
	State    GrantState `json:"state"`
	// AccessExpiresAt is when the stored access token stops working; absent when
	// signed out.
	AccessExpiresAt *Timestamp `json:"accessExpiresAt,omitempty"`
	// RefreshedAt is when the sign-in was last renewed, else when it was made.
	RefreshedAt *Timestamp `json:"refreshedAt,omitempty"`
	// Reason is one sentence for a person, present unless State is ok.
	Reason string `json:"reason,omitempty"`
	// Quota is present while the provider is refusing requests on this sign-in
	// (muster #458): the plan's usage window is the case it exists for. It is a
	// state of the GRANT, not of any one session: every session on the
	// subscription is affected at once and they recover together. Absent says
	// nobody reports a limit, never that none exists.
	Quota *GrantQuota `json:"quota,omitempty"`
}

// GrantQuotaState is where a sign-in's usage stands. The set is open on read: a
// client must treat a state it does not know as limited.
type GrantQuotaState string

const (
	// QuotaLimited: sessions on the sign-in are sitting in the runtime's retry loop
	// because the provider keeps refusing their requests. The runtime retries without
	// end, so a session does not fail: it waits, and recovers on its own when the
	// provider lets requests through.
	QuotaLimited GrantQuotaState = "limited"
)

// GrantQuota is the quota state of one sign-in (muster #458).
//
// The signal is what the runtime states, not a cause this service parsed: an
// exhausted plan window and any other persistent refusal look the same to the
// runtime, and Message is the provider's own words for which it was.
type GrantQuota struct {
	State GrantQuotaState `json:"state"`
	// Since is when the first session on the sign-in was seen refused, in this
	// episode; ObservedAt is when the service last saw it still so.
	Since      Timestamp `json:"since"`
	ObservedAt Timestamp `json:"observedAt"`
	// Sessions is how many sessions on the sign-in are refused right now, on the
	// answering machine. Absent when this answer is read from a stored mark
	// rather than from live sessions.
	Sessions *int `json:"sessions,omitempty"`
	// Message is the provider's own message as the runtime reported it, cut to a
	// short length. It is not parsed.
	Message string `json:"message,omitempty"`
}

// ModelsSource says where a RuntimeModels list came from, and so how far a
// client may trust it (api-http.md §3.1, muster #400).
//
// The set is open on read: a client must treat a source it does not know as
// ModelsNone, because a later build may name one this build never heard of.
type ModelsSource string

const (
	// ModelsFromRuntime: the runtime itself listed these, the last time it was
	// asked. A client may offer them as the choices; the create stays the
	// authority.
	ModelsFromRuntime ModelsSource = "runtime"
	// ModelsDeclared: the operator's configuration, or the aliases the runtime
	// documents, named these. The runtime has no list to ask, so a value outside
	// them may still work and a client must keep accepting one.
	ModelsDeclared ModelsSource = "declared"
	// ModelsNone: this runtime cannot say, or has not yet. A client keeps free
	// text.
	ModelsNone ModelsSource = "none"
)

// RuntimeModels is the models block of a RuntimeInfo row (muster #400).
type RuntimeModels struct {
	Source ModelsSource `json:"source"`
	// ReadAt is when the runtime last answered with this list, on the clock of
	// the machine that read it — a peer's value is passed through, not
	// re-stamped. Present only for ModelsFromRuntime: a declared or empty list
	// was not read from anything.
	ReadAt *Timestamp `json:"readAt,omitempty"`
	// IDs are the model ids a create may name, in the runtime's own spelling.
	// Never null; empty when Source is ModelsNone.
	IDs []string `json:"ids"`
	// Default is the id the runtime uses when a create names none, when this
	// machine can say so with certainty; null otherwise.
	Default *string `json:"default"`
}

// NoModels is the answer for a runtime that cannot say which models it takes.
func NoModels() RuntimeModels {
	return RuntimeModels{Source: ModelsNone, IDs: []string{}}
}

// Normalized fixes the two ways a value can be loosely formed: ids that are nil
// become the empty list (the wire never carries null there), and a missing or
// "none" source drops everything else, because a list with no stated origin is
// not one a client may offer.
func (m RuntimeModels) Normalized() RuntimeModels {
	if m.Source == "" || m.Source == ModelsNone {
		return NoModels()
	}
	if m.IDs == nil {
		m.IDs = []string{}
	}
	return m
}

// Available reports whether the row describes a runtime with a working
// driver. An absent Status is available.
func (r RuntimeInfo) Available() bool { return r.Status != RuntimeUnavailable }
