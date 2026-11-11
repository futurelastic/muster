package fleet

// Request is the caller-side context of an operation: everything an operation
// needs to know about *who is asking and what they believe*, as opposed to
// what they are asking about.
//
// # Why this exists as one type
//
// Two defects in this design turned out to be the same defect. §13 required a
// proxying service to present the original caller's authority, and §5.4
// required a destructive operation to corroborate against the caller's own
// observation. Neither was expressible, for the same reason: the operations in
// §3 took domain arguments only, and had nowhere to carry anything the
// *caller* knew.
//
// Both failed in the way that shape of gap always fails — the missing value
// was moved out of band, where it could be forgotten. For authority that
// produced a silent security defect (a proxy substituting its own credential,
// succeeding, and never reporting it). For corroboration it produced a rule
// stated in the specification and enforceable by nobody.
//
// Fixing them one at a time would fix two instances of a recurring problem.
// This type fixes the class: caller-side context is a single parameter with
// room to grow, so the next thing a caller needs to tell an operation is a
// field here rather than another break of every signature in §3.
//
// # What does NOT belong here
//
// Deadlines. §4.4 is enforced through the context, which is where the language
// already puts cancellation and which every driver already consults. A
// DeadlineMs field here would be a second source of truth about the same fact,
// free to disagree with the first — the exact failure this codebase objects to
// elsewhere (§9's `complete`, §13.2's re-synthesized source status).
//
// Anything the *target* is, rather than anything the caller knows. A session
// id, a filter and a payload are arguments; they stay arguments.
type Request struct {
	// Caller is the authority this operation is performed on behalf of
	// (§6, §13). Required for any operation that crosses a machine
	// boundary; a driver that cannot present it refuses rather than
	// substituting.
	Caller Caller

	// Expect is what the caller believes about the target, for operations
	// that must not act on a belief that has gone stale (§5.4).
	Expect Expectation

	// RuntimeHint is the runtime the caller named for a session-addressed
	// call (?runtime=, api-http.md §3.3), empty when it named none.
	//
	// It is a statement the caller made about HOW to resolve the target, not a
	// fact about the target, which is why it is here and not in a SessionRef
	// (a ref is an address, and an id is only unique within a runtime). A
	// local resolution consumes it where the route is handled; this field
	// exists for the one resolution this machine does not perform: a peer
	// resolves its own runtimes (§13.1), so the only way the caller's choice
	// reaches that resolution is for the remote driver to carry it. Empty means
	// the call goes out exactly as it would have before this field existed.
	RuntimeHint RuntimeId
}

// Expectation is the caller's own observation of the target, supplied so a
// driver can refuse when reality has moved (§5.4).
//
// Every field is optional, and that is deliberate: a caller that supplies
// nothing gets a weaker guarantee **explicitly**, rather than a
// strong-sounding rule that silently degrades. A driver must say which of the
// two it applied when it refuses.
type Expectation struct {
	// StartedAt is the session start time the caller observed.
	//
	// §5.4's problem is that ids are recyclable, so matching an id is not
	// identification. A driver can compare a live session against its own
	// last sighting, but that only closes the window between the DRIVER
	// observing and acting. The window that matters is between the CALLER
	// observing and acting — it is longer, it is the one a human is inside
	// of, and it is far longer again across a network, where a round trip
	// separates the two.
	//
	// This field is the caller's half of that comparison. With it, "destroy
	// the session I looked at" is expressible; without it, only "destroy
	// whatever is at this id" is, which is what §5.4 forbids.
	StartedAt *Timestamp
}

// HasExpectation reports whether the caller supplied anything to corroborate
// against. A driver uses this to decide which guarantee it can offer, and to
// say so.
func (e Expectation) HasExpectation() bool { return e.StartedAt != nil }

// RequestFrom builds a Request for a caller with no particular expectation —
// the common case for reads, which corroborate nothing.
func RequestFrom(c Caller) Request { return Request{Caller: c} }

// SystemRequest is work a service performs on its own behalf rather than for
// any client: startup reconciliation (§12), for example. It carries no
// credential, so anything trying to use it to reach a peer is refused — which
// is correct, since a service reconciling its own machine has no business
// acting on another one.
func SystemRequest() Request { return Request{Caller: SystemCaller()} }
