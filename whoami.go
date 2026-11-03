package fleet

// GrantReport answers GET /v1/whoami (api-http.md §5, muster #106): what
// the presented credential is authorized to do, on the machine named.
//
// It exists because every other way to learn this is trial and refusal:
// attempt the call, read whichever precondition failed first, fix it, and
// discover the next one on the next attempt. That cost doubles for a relayed
// operation, which needs one grant on the machine receiving the call and a
// second on the machine performing it (session-abstraction.md §7.7) — this
// route answers the first directly and is honest about why it cannot answer
// the second the same way.
type GrantReport struct {
	// Principal is the caller's own resolved identity, exactly as it
	// appears in the audit trail (Caller.Principal). Never another
	// principal's — this route reports on the credential that presented it,
	// nothing else configured on this machine.
	Principal string `json:"principal"`
	// Machine is the machine this report describes: the one that answered,
	// when Source is "observed"; the one named and never reached, when
	// Source is "assumed".
	Machine MachineId `json:"machine"`
	// Grants is every verb this credential currently holds on Machine. An
	// empty list is a real answer, not an omission — "this credential holds
	// nothing here" is exactly what a principal with no grants needs to be
	// able to learn about itself.
	Grants []string `json:"grants"`
	// Source is CapabilitySource's provenance shape, reused rather than
	// reinvented for a second domain: "observed" means Machine's own table
	// was consulted directly, just now, to produce Grants. "assumed" means
	// Machine names a peer this service has no mechanism to ask — grants are
	// per-machine configuration, not a runtime fact this service probes and
	// caches the way it does DriverCapabilities, so Grants is always empty
	// under "assumed" and must be read the same way an unreached peer's
	// capabilities are: a conservative floor, never that peer's real answer.
	Source CapabilitySource `json:"source"`
	// Scope is how far this credential reaches (muster #395): "local" for a
	// principal pinned to the machine that answered — its fleet-scoped reads
	// are answered locally, a request naming another machine is refused, and
	// GET /v1/machines lists only this machine — and "fleet" for everyone else.
	// Present when the report is about the machine that answered; absent when
	// it names a peer, where this service has no way to know.
	Scope string `json:"scope,omitempty"`
	// ListsYou answers a question only a SERVICE asks (muster #154): is
	// the machine named in `?peer=` in this machine's own roster of peers?
	// A service probing a peer names itself there and reads this back as
	// PeerStanding.ListsMeBack.
	//
	// Always present on a build that has it, and null whenever it was not
	// answered — no `peer` asked, a report about another machine, or a
	// credential without `read`, because the roster is this machine's
	// configuration and `read` is what guards that. So an ABSENT key can
	// only mean a build that predates the field, which is how the prober
	// tells "not listed" (false) from "cannot say" (a peer on older code).
	//
	// The id is asserted by the caller. That is acceptable because this is a
	// report, never authority: nothing is granted or refused on it.
	ListsYou *bool `json:"listsYou"`
}
