package fleet

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorKind is the closed set of wire error kinds (api-http.md §2).
type ErrorKind string

const (
	ErrorInvalid      ErrorKind = "invalid"
	ErrorUnauthorized ErrorKind = "unauthorized"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorConflict     ErrorKind = "conflict"
	ErrorUnsupported  ErrorKind = "unsupported"
	// ErrorUnreachable means the machine did not answer; nothing is known.
	// Must never be conflated with ErrorNotFound (api-http.md §2): one
	// means the fleet knows the session does not exist, the other means
	// the fleet knows nothing at all. A client that treats a 504 as a 404
	// will confidently report work as gone while it is running fine on an
	// unreachable host.
	ErrorUnreachable ErrorKind = "unreachable"
)

// ErrAmbiguousTarget is returned by a destructive operation whose target
// could not be corroborated (§5.4): the caller's expectation disagrees with
// the live session, or nothing was supplied to corroborate against at all.
//
// It lives in this package rather than in a driver so the service can map it
// to a wire kind without importing one — the mapping is part of the contract,
// not of any substrate.
//
// It maps to "conflict", not "invalid". The request was well formed; what
// failed is that the caller's belief about the world and the world itself
// disagree, which is exactly what 409 means. Reporting it as 400 tells a
// caller to fix its syntax when what it should do is re-read and decide.
var ErrAmbiguousTarget = errors.New("fleet: destructive operation on an uncorroborated target (§5.4)")

// ErrNoSuchSession is returned by a read whose id the machine has never had.
//
// # Why this is not "dead"
//
// A single-session read used to answer `dead` for any id it could not find,
// on the reasoning that §8 makes dead terminal and a session that is gone is
// gone. That reasoning is right for a session the machine ONCE HAD, and wrong
// for every other string a caller can type.
//
// `dead` is a claim about a session's history: it existed, and it ended. For
// an id that was never seen, the service has no such history, and manufacturing
// one is §5.7 inverted — reporting a fact about the world that is really the
// reporter's ignorance. A caller that mistypes an id would be told its session
// had died, which is a far more alarming answer than "no such thing", and it
// invites exactly the wrong follow-up.
//
// The distinction is available because a driver remembers what it has seen
// (that memory is also what §8's `since` and §12's reconciliation are built
// from). Seen before and absent now is `dead`; never seen is this error, and
// it maps to not_found.
//
// Like ErrAmbiguousTarget, it lives here so the service can map it to a wire
// kind without importing a driver.
var ErrNoSuchSession = errors.New("fleet: no session with that id has been observed on this machine")

// DefaultHTTPStatus is the api-http.md §2 table, kept next to the kind it
// describes so a Go client never has to hardcode it separately from the
// server that emits it.
func (k ErrorKind) DefaultHTTPStatus() int {
	switch k {
	case ErrorInvalid:
		return 400
	case ErrorUnauthorized:
		return 401
	case ErrorNotFound:
		return 404
	case ErrorConflict:
		return 409
	case ErrorUnsupported:
		return 501
	case ErrorUnreachable:
		return 504
	default:
		return 500
	}
}

// Error is the wire error body (api-http.md §2), wrapped in {"error": ...}
// by ErrorEnvelope. It implements the standard error interface so it can
// travel through ordinary Go error-handling paths inside the service.
type Error struct {
	Kind      ErrorKind `json:"kind"`
	Message   string    `json:"message"`
	Machine   MachineId `json:"machine,omitempty"`
	Retryable bool      `json:"retryable"`

	// Reason further classifies some Kind values beyond what the fixed Kind
	// enum distinguishes, so a caller can branch on the specific condition
	// without parsing Message — see ReasonReplayOfEndedSession below for the
	// one case that sets it today. Absent unless a case documents one.
	Reason string `json:"reason,omitempty"`

	// Session and ClosedAt are set together with Reason ==
	// ReasonReplayOfEndedSession (muster #234): an Idempotency-Key
	// replay whose recorded session no longer exists. Session is that
	// session's own ref — never the replaying caller's — and ClosedAt is
	// when this machine observed the absence, an upper bound on the true end,
	// the same rule ClosedSession.ClosedAt follows for ClosedByAbsent
	// (closed.go).
	Session  *SessionRef `json:"session,omitempty"`
	ClosedAt *Timestamp  `json:"closedAt,omitempty"`

	// SessionIds is set together with Reason == ReasonConversationId
	// (muster #268): the ids of the live sessions whose conversation id the
	// caller addressed. A client reads it instead of parsing Message.
	SessionIds []string `json:"sessionIds,omitempty"`
}

// ReasonConversationId marks the ErrorNotFound a session-keyed read returns
// when the id it was given is not a session id but IS the conversation id of a
// live session (api-http.md, muster #268). The session exists; the caller
// addressed it by the wrong kind of id. Error.SessionIds carries the right
// ones.
const ReasonConversationId = "conversation-id"

// ConversationIdError is what a driver returns, in place of a bare
// ErrNoSuchSession, when the id it was asked about matches no session id but
// equals the conversation id of one or more live sessions (muster #268).
//
// It still answers errors.Is(err, ErrNoSuchSession) — the id is not a session
// id, and a service probing drivers for ownership must read it as "not mine"
// — but its message says what the id actually is, and never reads like "the
// session is gone".
type ConversationIdError struct {
	ID         string   // the id the caller addressed
	SessionIds []string // ids of the live sessions holding it, sorted
}

func (e *ConversationIdError) Error() string { return ConversationIdReason(e.ID, e.SessionIds) }

// Is makes the error a refinement of ErrNoSuchSession rather than a sibling.
func (e *ConversationIdError) Is(target error) bool { return target == ErrNoSuchSession }

// ConversationIdReason is the one sentence every route uses for this refusal,
// so the receipt reason and the error message cannot drift apart. The text is
// for people; the ids a program should use travel in a structured field
// (DeliveryReceipt.SessionIds, Error.SessionIds).
func ConversationIdReason(id string, sessionIds []string) string {
	if len(sessionIds) == 1 {
		return fmt.Sprintf("%q is a conversation id, not a session id: it is the conversation of the live session whose id is %q — address that id instead", id, sessionIds[0])
	}
	return fmt.Sprintf("%q is a conversation id, not a session id: it is the conversation of %d live sessions, whose ids are %s — address one of those ids instead", id, len(sessionIds), strings.Join(quoteAll(sessionIds), ", "))
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// ReasonReplayOfEndedSession marks the ErrorConflict a create's
// Idempotency-Key replay returns when the key's recorded session is no
// longer present (api-http.md §3.3, muster #234). The key itself is
// untouched by this — it stays spent for its normal retention
// (idemStore.sweepLocked) — so "same key, same answer" (§10) still holds;
// the caller's fix is to mint a FRESH key, not to retry this one.
const ReasonReplayOfEndedSession = "replay-of-ended-session"

func (e *Error) Error() string {
	if e.Machine != "" {
		return fmt.Sprintf("fleet: %s (%s, machine=%s)", e.Message, e.Kind, e.Machine)
	}
	return fmt.Sprintf("fleet: %s (%s)", e.Message, e.Kind)
}

// ErrorEnvelope is the top-level JSON body of every non-2xx response
// (api-http.md §2).
type ErrorEnvelope struct {
	Error Error `json:"error"`
}
