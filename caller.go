package fleet

// Caller is the authority an operation is performed on behalf of (§6, §13).
//
// # Why this is a parameter and not a context value
//
// §13 requires that a service proxying a request to a peer present the
// ORIGINAL caller's authority, never its own — "otherwise every machine
// becomes a confused deputy for every other." An earlier revision carried
// this out of band, in a context value, because no operation in §3 had
// anywhere to put it.
//
// That failed in the specific way security defects fail: the natural
// fallback for a remote driver with no caller credentials is to use the one
// credential it certainly has — its own. The request then succeeds, the
// tests pass, and the authorization is silently widened. Nothing reports it,
// because the symptom of the bug is that everything works.
//
// Making it a parameter is the entire fix. A driver cannot compile without
// deciding what to do with it, and a service cannot forget to supply it. The
// out-of-band version could be omitted by accident; this one cannot be
// omitted at all.
//
// # The two fields answer two different questions
//
// Principal answers "who is asking", and exists for §6 requirement 4: every
// remote-originated mutation is logged with actor, verb, target and outcome.
// It is the audit trail that replaces "it could only ever have been me."
//
// Credential answers "with what authority", and exists for §13: it is what a
// remote driver presents to a peer so the peer authorizes the principal who
// initiated the request rather than the machine that relayed it.
//
// A local driver typically uses the first and ignores the second. A remote
// driver needs both. Neither may be invented by a driver that was not given
// one.
type Caller struct {
	// Principal names who is asking, for the audit trail. Safe to log.
	//
	// While a fleet authenticates with a single shared token (§7.2's
	// statically-configured peers), this cannot be a true identity —
	// nothing distinguishes one bearer of that token from another. A
	// service should populate it with the most specific honest value it
	// has, typically the request's origin, and callers should read it as
	// provenance rather than identity. Per-peer identity is §6's future
	// work, and this field is where it will land when it exists.
	Principal string

	// Credential is the authority to present onward when proxying (§13).
	//
	// NEVER log this, and never substitute a different one. A driver that
	// finds it empty must refuse the operation rather than fall back to its
	// own credentials — the fallback is the confused-deputy bug, and it
	// fails by succeeding.
	Credential string

	// Person is the human the caller says it acts for (muster #349), or nil.
	// It is attribution only — never an input to authorization. A service sets
	// it only after deciding the claim may be trusted (the calling principal
	// holds the acting-person grant), and a remote driver forwards it to the
	// peer, which decides again for itself.
	Person *Person
}

// Person names a human on whose behalf a request is made. The ID is opaque to
// this service: it means something only to the Issuer that minted it, which is
// why the two travel and are recorded together.
type Person struct {
	ID     string
	Issuer string
}

// String is the form recorded in an audit line: "<id>@<issuer>".
func (p Person) String() string { return p.ID + "@" + p.Issuer }

// SystemCaller is the authority for work a service performs on its own
// behalf rather than for a request — startup reconciliation (§12), for
// example, which no client asked for.
//
// It carries no credential deliberately. Anything that tries to use it to
// reach a peer will be refused, which is correct: a service reconciling its
// own machine has no business acting on another one, and §12's rule 4
// ("never destroy anything during reconciliation") means it should not be
// acting destructively anywhere.
func SystemCaller() Caller {
	return Caller{Principal: "system:self"}
}

// HasCredential reports whether this caller can present authority to a peer.
// A remote driver checks this before any operation and refuses when it is
// false; see the Credential field.
func (c Caller) HasCredential() bool { return c.Credential != "" }
