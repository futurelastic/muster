package fleet

// ClosedSession is the record a service keeps of one session after it ended
// — a tombstone, read through GET /v1/sessions/closed (api-http.md §3.2).
//
// # Why a service keeps these at all
//
// The live list answers "what is running"; the event feed answers "what
// changed while I was watching". Neither answers "which sessions ran here in
// the last N days, and when did each end" — the feed's window is in memory,
// is not persisted, and only advances while something is subscribed. A
// consumer that wants that answer would otherwise have to keep its own copy
// of session state, which is exactly the second copy of the truth this
// service exists to make unnecessary.
//
// This is deliberately much smaller than a persisted event window: one record
// per ended session, kept for a bounded retention period, carrying only the
// metadata the live record already carried. Content is never included.
//
// # closedAt is a bound, and ClosedBy says which kind
//
// A session closed through this service ends at a moment the service
// observed, and closedAt is that moment. A session that ended on its own —
// the process exited, a human killed it from a terminal — is learned about
// the next time the service reads its machine completely: from a live event
// stream, a listing, or its own start-up sweep. Its closedAt is when that
// absence was OBSERVED, and the true end lies somewhere after LastSeenAt and
// no later than closedAt. The two are reported side by side rather than one
// being presented as the other (§5.2: inference is never presented as
// observation).
type ClosedSession struct {
	SessionRef

	Runtime RuntimeId    `json:"runtime,omitempty"`
	Cwd     AbsolutePath `json:"cwd,omitempty"`

	// Agent is the same hint the live record carries (Session.Agent), copied
	// from the last sighting that reported one. Same contract as the live
	// field: a hint, omitted when it was never known, never guessed. A peer
	// whose build predates the field simply omits it.
	Agent AgentId `json:"agent,omitempty"`

	// StartedAt is the same field the live record carries (§5.4's identity
	// pairing with ID), copied from the last sighting. Absent when the
	// runtime never reported it.
	StartedAt *Timestamp `json:"startedAt,omitempty"`

	// Conversation is the runtime's own conversation record as last seen,
	// present only when it was known (ConversationRef.Known).
	Conversation *ConversationRef `json:"conversation,omitempty"`

	// LastSeenAt is the last time this service recorded the session as
	// present. For a session ClosedBy absent, the end lies after it.
	LastSeenAt Timestamp `json:"lastSeenAt"`

	// ClosedAt is when the end was observed — exact for ClosedByClose, an
	// upper bound for ClosedByAbsent.
	ClosedAt Timestamp `json:"closedAt"`

	ClosedBy ClosureKind `json:"closedBy"`

	// Evidence says, in a sentence, how the end was learned.
	Evidence string `json:"evidence"`

	// Exit is present only for ClosedByExit: what a driver captured about the
	// session's own process exiting, in the same pass that removed the
	// session (muster #235). Absent for every other ClosureKind.
	Exit *SessionExit `json:"exit,omitempty"`

	// Usage is what the session had spent when it ended, as its runtime
	// reported it (muster #485): the final cumulative figures, kept because
	// the runtime's own record of them goes with the session's directory when
	// it closes, and a caller that wants spend per session, per day or per
	// task can otherwise only sample sessions while they live. Absent when no
	// figure was ever read — a driver that reports none, a session that ended
	// before any request was recorded — which is "not known", never zero.
	// The session's start and end are the record's own StartedAt and ClosedAt.
	Usage *ClosedUsage `json:"usage,omitempty"`
}

// ClosedUsage is a session's final cumulative usage, as its runtime reported
// it, kept after the session ended (muster #485). It is the same account
// Usage gives while the session lives — the SUM over every request, the
// runtime's own cost in the unit it used, nothing priced here, absent meaning
// "not known" rather than zero — read once more at the end, plus the two facts
// a live block leaves to a separate field: which provider and model the last
// request ran on.
//
// # Figures only
//
// Tokens, a cost, a provider name, a model name and a timestamp. Nothing the
// session said or did: the record of that is content, readable only under its
// own grant, and is exactly what is gone by the time this is read.
//
// # Provider and model are the newest request's
//
// A session that changed model part-way has earlier spend on others. The
// cumulative figures are not split by model; Provider and Model say where the
// newest request ran, so a consumer attributing the whole figure to one model
// knows it is attributing to the last one.
type ClosedUsage struct {
	Input      int64    `json:"input"`
	Output     int64    `json:"output"`
	CacheRead  int64    `json:"cacheRead"`
	CacheWrite int64    `json:"cacheWrite"`
	Reasoning  *int64   `json:"reasoning,omitempty"`
	Cost       *float64 `json:"cost,omitempty"`

	// Credential is which account the figures were spent on (Usage.Credential,
	// muster #458), with the same meaning: the credential this service gave the
	// session, named only while the newest request ran on that provider.
	Credential CredentialKind `json:"credential,omitempty"`

	// Provider and Model are the provider and model the newest request that
	// reported usage ran on, as the runtime resolved them. Absent when the
	// runtime named none.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// Source says how the driver learned the figures, as Usage.Source does.
	Source Confidence `json:"source"`

	// AsOf is the moment of the runtime's own newest entry counted in these
	// figures — not the time the session closed or the time this was read. A
	// session closed long after its last request has an AsOf long before its
	// ClosedAt.
	AsOf Timestamp `json:"asOf"`
}

// Figures returns the arithmetic form of u's cumulative figures.
func (u ClosedUsage) Figures() UsageFigures {
	return UsageFigures{u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Reasoning, u.Cost}
}

// NewClosedUsage builds the final block from summed figures.
func NewClosedUsage(f UsageFigures, source Confidence, asOf Timestamp) *ClosedUsage {
	return &ClosedUsage{
		Input: f.Input, Output: f.Output, CacheRead: f.CacheRead, CacheWrite: f.CacheWrite,
		Reasoning: f.Reasoning, Cost: f.Cost, Source: source, AsOf: asOf,
	}
}

// SessionExit is what a driver captured about a session's own process
// exiting on its own, before the driver removed the session that held it
// (muster #235). Never guessed — present only when a driver capable of
// it (an ExitReporter) actually observed the pane before it was gone.
type SessionExit struct {
	// Status is the pane's own process exit status, as the runtime reported
	// it.
	Status int `json:"status"`

	// At is when the driver captured this — the pane was still readable at
	// that moment, which is what makes ClosedByExit's ClosedAt exact rather
	// than an upper bound.
	At Timestamp `json:"at"`

	// ScreenPath names a file in the service's OWN state directory holding
	// the pane's last lines at exit — never the content itself. Pane text can
	// hold anything the runtime printed, and putting it on the API is a new
	// data-exposure surface this issue deliberately does not open (see the
	// ruling on muster #235); an operator who needs it remotely reads
	// the file on that machine, or a future issue adds a route for it.
	// Empty when nothing was captured (an unconfigured store, or the capture
	// itself failed) — absent evidence, never an empty string standing in
	// for "nothing here".
	ScreenPath string `json:"screenPath,omitempty"`
}

// ClosureKind says how a service learned a session ended.
type ClosureKind string

const (
	// ClosedByClose: a close request through this service was accepted by
	// the driver. ClosedAt is the moment it was accepted.
	ClosedByClose ClosureKind = "close"
	// ClosedByAbsent: the session was missing from a complete read of its
	// runtime (or its id now names a session that started later). ClosedAt
	// is when the absence was observed; the end lies after LastSeenAt.
	ClosedByAbsent ClosureKind = "absent"
	// ClosedByExit: a driver capable of it (ExitReporter) captured the
	// session's own process exit — status and last screen — before removing
	// the session, and reported it in the same pass (muster #235).
	// ClosedAt is exact, like ClosedByClose: it is the moment the driver
	// captured the pane, not an upper bound inferred from a later listing.
	ClosedByExit ClosureKind = "exit"
)
