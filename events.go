package fleet

// EventKind is the closed set of SSE event names (api-http.md §4).
type EventKind string

const (
	EventSessionCreated EventKind = "session.created"
	EventSessionState   EventKind = "session.state"
	EventSessionClosed  EventKind = "session.closed"
	// EventSessionRenamed carries a session whose ID has CHANGED. A subscriber
	// filtering by id must re-key on this or it silently stops matching a
	// session that is still very much alive — which is why a rename cannot be
	// a quiet mutation. Without the event, a rename is indistinguishable from
	// a disappearance, and that is the one thing it must not be mistaken for.
	//
	// One rename produces more than one of these (muster #103): an
	// accept-time event, always, and a later follow-up once this service has
	// something to say about whether it held — see SessionRenamed.Corroboration.
	EventSessionRenamed EventKind = "session.renamed"
	// EventSessionLabels carries a session's labels after they changed
	// (muster #153) — at create when it carried any, and after every
	// POST …/labels. The payload is the WHOLE map, not the patch, so a
	// subscriber that missed an earlier one still converges.
	//
	// It is also the guarantee for a create: a driver's own session.created
	// can be observed before the service has stored the create's labels, so
	// that event may carry `{}` for a session this one then labels.
	EventSessionLabels EventKind = "session.labels"
	// EventSessionRecord says a session's record grew (muster #444). It carries
	// the session and a cursor and NEVER content: a client that holds the record
	// route's cursor (#441) pulls from its own last cursor to read what is new.
	//
	// It is a hint, like the multiplexer driver's notifications are to its own
	// reads: it says "look", never what to see. A burst of appends inside one
	// window is one event, so two events for one session are never closer than
	// that driver's window, and a client must not rely on a number.
	EventSessionRecord EventKind = "session.record"
	EventSourceStatus  EventKind = "source.status"
	// EventMachineQuota reports that this machine's ACCOUNT started or
	// stopped refusing work — a fact about the machine, not about any one
	// session, and the only event here whose subject is not a session.
	//
	// It exists because of how a supervisor learned this without it. When an
	// account hit its weekly limit, 48 autopilot sessions each discovered it
	// separately, by being told to work and stalling; the supervisor recorded
	// 48 stall reasons and never formed the single conclusion that explained
	// all of them. Every one of those discoveries was a session that had
	// already been dispatched — the cost is paid before the fact is learned.
	//
	// One event, at the transition, lets a supervisor stop dispatching
	// instead of finding out N times that it should have. It is the earliest
	// honest signal available: the runtime gives no advance warning, so the
	// first refusal IS the notice (see the package findings on what was
	// searched for and not found).
	EventMachineQuota EventKind = "machine.quota"
	// EventMachineAccount reports that this machine's local credential
	// material changed — a fact about the MACHINE, not about any one
	// session, and (with EventMachineQuota) the second event here whose
	// subject is not a session (#12).
	//
	// Same subject and the same justification as EventMachineQuota: an
	// account-level fact, discovered by a session stalling or not at all,
	// outlives the moment it was learned. This one has a cheaper signal
	// than quota does — the credential store's own modification time dates
	// the change exactly, before anything has to fail for it to be
	// noticed — so it does not need a session to be dispatched first in
	// order to be learned.
	//
	// It reports WHICH generation is now locally in force. It does not
	// report, and must never be read as reporting, that any session's
	// binding to it still works: three independent local sources agreed
	// the fleet was healthy through exactly this kind of transition and
	// all three were wrong, because all three ultimately quote the
	// process's own announcement about itself. See MachineAccountPayload.
	EventMachineAccount EventKind = "machine.account"
	EventControlResync  EventKind = "control.resync"
)

// ResyncReason is control.resync's payload discriminant (api-http.md §4,
// session-abstraction.md §7.3).
type ResyncReason string

const (
	ResyncEpochChanged  ResyncReason = "epoch_changed"
	ResyncCursorExpired ResyncReason = "cursor_expired"
	// ResyncFeedGap means the sequence is intact and continuous, and the
	// OBSERVER of it was not: this service's subscription to a driver stopped
	// and was re-established, so changes during the interval were never
	// stamped into the sequence at all.
	//
	// It is a third reason rather than a reuse of ResyncCursorExpired because
	// the two are opposite statements about where the fault lies. A cursor
	// expires when the caller has fallen behind what is retained; a feed gap
	// is this service admitting it stopped watching. Telling a caller its
	// cursor is too old, when the cursor is perfectly current and the hole is
	// ours, sends it looking for a slowness problem it does not have.
	//
	// The action is the same as for the other two — refetch and resubscribe —
	// which is why it can be added without changing what a client DOES on
	// control.resync, only what it can say about why.
	ResyncFeedGap ResyncReason = "feed_gap"
)

// SessionStatePayload is session.state's and session.closed's payload shape
// (api-http.md §4: "ref + SessionState", "ref + final state").
type SessionStatePayload struct {
	Ref   SessionRef   `json:"ref"`
	State SessionState `json:"state"`
}

// SessionLabelsPayload is session.labels' payload (muster #153): the
// session, when it started (so a subscriber can tell a recycled id apart,
// §5.4), and its complete label map after the change.
type SessionLabelsPayload struct {
	Ref       SessionRef        `json:"ref"`
	StartedAt *Timestamp        `json:"startedAt,omitempty"`
	Labels    map[string]string `json:"labels"`
}

// SessionRecordPayload is session.record's payload (muster #444): the session,
// when it started, and where its record has reached.
//
// These are the only keys, and a test holds it so: the event exists so a client
// can learn that a record grew WITHOUT the record travelling on the feed, which
// is readable under the `read` grant while the record itself is readable only
// under `record` (docs/adr/441-…). A field added here is a field of content
// risk, and needs its own ruling.
//
// Cursor is the cursor GET …/record would return as `next` at that moment: pass
// it as `since` and the route answers what is recorded AFTER this event, which
// is empty until the next append. To read what the event announced, a client
// reads from the cursor of ITS last read, not from this one. The cursor is
// opaque, exactly as RecordPage.Next is, and is not comparable with one.
type SessionRecordPayload struct {
	Ref SessionRef `json:"ref"`
	// StartedAt is when the session started, so a subscriber can tell a recycled
	// id apart (§5.4), as session.labels carries it. Absent when the driver does
	// not know it.
	StartedAt *Timestamp `json:"startedAt,omitempty"`
	Cursor    string     `json:"cursor"`
}

// MachineQuotaPayload is machine.quota's payload.
//
// Blocked is explicit rather than implied by Quota being nil, so a recovery
// event is a positive statement ("this account works again") and not an
// absence a subscriber has to interpret — §5.7's rule applied to the event
// plane.
type MachineQuotaPayload struct {
	Machine MachineId   `json:"machine"`
	Blocked bool        `json:"blocked"`
	Quota   *QuotaBlock `json:"quota,omitempty"`
}

// ControlResyncPayload is control.resync's payload.
type ControlResyncPayload struct {
	Reason ResyncReason `json:"reason"`
}

// MachineAccountPayload is machine.account's payload (#12).
//
// Generation is the credential store's new modification time — an identity
// marker, not a willingness or health claim. It names WHICH generation is
// now locally in force, so a subscriber can compare it against a session's
// own SessionState.CredentialGeneration or Session.StartedAt to answer "did
// this session start under the credential now in force" — never "is this
// session's binding to it still good". The measured failure this event
// exists to avoid is the same shape as machine.quota's: a supervisor
// learning an account-level fact one stalled session at a time instead of
// once, at the transition.
type MachineAccountPayload struct {
	Machine    MachineId `json:"machine"`
	Generation Timestamp `json:"generation"`
}

// Event is one entry on the stream subscribe() opens (§3, §7.3). Every
// event carries a monotonic Cursor and the Epoch of the service instance
// that assigned it (§7.3) — a subscriber reconnecting with a stale Cursor
// or a changed Epoch gets EventControlResync instead of a silent gap.
//
// Payload's Go type varies by Kind:
//   - EventSessionCreated -> Session
//   - EventSessionState, EventSessionClosed -> SessionStatePayload
//   - EventSessionLabels -> SessionLabelsPayload
//   - EventSessionRecord -> SessionRecordPayload
//   - EventSourceStatus -> SourceStatus
//   - EventMachineQuota -> MachineQuotaPayload
//   - EventMachineAccount -> MachineAccountPayload
//   - EventControlResync -> ControlResyncPayload
//
// The spec does not pin down the exact SSE line framing — whether Kind
// travels as the SSE "event:" field, a JSON "kind" property inside "data:",
// or both (see the package doc's findings list). This type is the
// Go-native shape a driver or the service works with in memory;
// internal/service would own translating it to and from the wire if this
// skeleton implemented streaming, which it does not (see NewMux's
// /v1/events handler).
type Event struct {
	Cursor  int64     `json:"cursor"`
	Epoch   string    `json:"epoch"`
	Machine MachineId `json:"machine"`
	Kind    EventKind `json:"kind"`
	Payload any       `json:"payload"`

	// Origin carries the sequence coordinates the ORIGINATING service gave
	// this event, when it reached the caller through a proxy. Nil for
	// events a service produced itself.
	//
	// Cursor and Epoch above always belong to the service the caller is
	// talking to, so "resume from cursor N" is never ambiguous about whose
	// N. Origin preserves what the peer said about its own sequence, so a
	// caller that later talks to that peer directly can resume there
	// instead of refetching.
	//
	// This is the same split §13.2 uses for source status and F20 for error
	// kinds: adopt what the peer said about itself, add only what the
	// relaying service is uniquely positioned to know — here, the ordering
	// of this event against everything else the caller is receiving.
	//
	// The originating machine is Event.Machine and is not repeated here.
	Origin *EventOrigin `json:"origin,omitempty"`
}

// EventOrigin is a relayed event's coordinates in its originating service's
// own sequence (§7.3, §13).
type EventOrigin struct {
	Cursor int64  `json:"cursor"`
	Epoch  string `json:"epoch"`
}
