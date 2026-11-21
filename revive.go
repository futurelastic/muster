package fleet

// Reviving a session a restart left unknown (muster #427).
//
// A session created with caller-supplied environment is never relaunched by a
// restart: the values were never kept (muster #282), and a session started again
// with less than it was created with would be a different session wearing the same
// id. It is listed `unknown`, "needs re-creation", with its conversation intact in
// the runtime's own store and nothing able to reach it but a close.
//
// Revive is the caller giving the environment back. The service keeps no secret to
// do it with and persists nothing; the request carries the same `env` a create takes
// and the driver relaunches the session's own server on the directory it already
// holds. The verb is a create-grant act, because it starts a process with
// caller-supplied environment on this machine, which is what the create grant means.

// ReasonNotRevivable marks the conflict (409, non-retryable) a revive returns when
// the session is not one a revive can bring back: it is already running, or it is
// listed `unknown` for a cause the environment has nothing to do with (its relaunch
// failed for a reason a later start may clear, its record cannot be read, its
// context file is gone), or the runtime no longer holds it. Nothing was started.
// The message says which; a client that lost the response to its own revive reads
// the session, where `idle` with `conversation.known` is the success it missed.
const ReasonNotRevivable = "not-revivable"

// ReviveRequest is the body of POST …/revive.
type ReviveRequest struct {
	// Env is the environment the session was created with, by name and value. It
	// must name exactly the variables the create named — a session relaunched with
	// fewer is a different session, and one with more is too — and may not name a
	// variable the service owns or a delivery module reserves, the same rules a
	// create applies. Required: a revive without environment is refused, because
	// that is a session relaunched with less than it was created with.
	Env map[string]string `json:"env"`
	// ContextRef, when set, must be the file the create named. It exists so a caller
	// that remembers its create says so and is refused when it is wrong; it cannot
	// change the session's context. Absent means "the one it was created with".
	ContextRef AbsolutePath `json:"contextRef,omitempty"`
}
