package service

import (
	"log"
	"net/http"

	fleet "github.com/futurelastic/muster"
)

// A principal pinned to this machine (muster #395).
//
// A principal holding `read` sees the whole fleet: the default scope of every
// plural read is `fleet`, and a path naming a peer needs only `read` (ruled in
// #81). That left no credential that sees THIS machine's sessions and nothing
// else — a client that never sets `scope` listed a peer's sessions too.
//
// `"scope": "local"` on a principal closes that, for that principal alone. The
// pieces, each in the place the thing it changes already lives:
//
//	authenticated()   refuses a request that names another machine (localOnlyGate)
//	list/closed/watch/events handlers   answer a fleet-scoped read as scope=local (scopeFor)
//	handleMachines    lists this machine and no one else
//	handleWhoAmI      says so
//	PrincipalTable    cuts a fleet-scoped stream when a reload pins its principal
//
// # Clamp the scope, refuse the path
//
// The two asks differ and the answers differ with them.
//
// A fleet-scoped read is the DEFAULT. The client in the measured case never
// asked for the fleet; it got it by not saying. Refusing that request would
// break exactly the client the operator is trying to keep working, so the read
// is answered for this machine. It is answered, not silently shrunk: the
// response of a scope=local read carries this machine's one source, which is
// what the same request would have returned had the client asked for local.
//
// A path that names another machine is a deliberate act: the client chose the
// peer. There is no local answer to give instead, so it is refused, as
// unauthorized with the reason `local-only`.
//
// # One gate, in front of every route
//
// The refusal sits in authenticated(), not in reading() and mutating(). Not
// every route runs through those two: the turns read has its own gate
// (revealing), the machine-list push decides its own grant before reading a
// body, and whoami names its machine in the query. A check in the wrappers
// would be a check on the routes someone remembered to wrap; this one is on all
// of them, including a route added next year.
//
// The answer is the same for a real peer and for a machine id that exists
// nowhere, so the refusal cannot be used to learn which peers there are.

// localOnlyGate refuses a request from a principal pinned to this machine when
// it names any other machine, or is about the fleet's roster. It returns nil
// when the principal is not pinned, or the request names this machine or none.
func (s *Service) localOnlyGate(p Principal, r *http.Request) *fleet.Error {
	if !p.LocalOnly() {
		return nil
	}
	target := fleet.MachineId(r.PathValue("machine"))
	if target == "" && r.URL.Path == "/v1/whoami" {
		target = fleet.MachineId(r.URL.Query().Get("machine"))
	}
	// The owner-signed machine list is the fleet's roster, peers' addresses
	// included, and a push replaces it. It names no machine in its path and is
	// about nothing but the others, so a pinned principal has no business with it.
	fleetWide := r.URL.Path == "/v1/machine-list"
	if !fleetWide && (target == "" || target == s.Self()) {
		return nil
	}
	log.Printf("audit: actor=%q verb=%s route=%s target=%s/%s outcome=DENIED reason=%s",
		actorOf(p, r), grantForVerb(r), routeOf(r), target, r.PathValue("id"), fleet.ReasonLocalOnly)
	return &fleet.Error{
		Kind:   fleet.ErrorUnauthorized,
		Reason: fleet.ReasonLocalOnly,
		Message: "principal " + p.Name + " is pinned to this machine (scope \"local\") and cannot " +
			"reach another one, whatever grants it holds. This is how the principal is configured, " +
			"not a missing grant: an operator removes the scope from the principal to lift it.",
		Machine: target,
	}
}

// scopeFor is the scope a plural read is answered at: what the caller asked,
// except that a principal pinned to this machine is always answered locally.
// Called AFTER the requested value is validated, so a malformed scope is still
// a 400 for everybody.
func scopeFor(r *http.Request, asked Scope) Scope {
	if p, ok := principalOf(r); ok && p.LocalOnly() {
		return ScopeLocal
	}
	return asked
}

// pinnedLocal reports whether the request's principal is pinned to this
// machine. Single-token mode has no principals and so no pin.
func pinnedLocal(r *http.Request) bool {
	p, ok := principalOf(r)
	return ok && p.LocalOnly()
}

// scopeReport is what whoami says about the caller's reach: "local" for a
// pinned principal, "fleet" for everyone else (including single-token mode,
// which has no way to be pinned).
func scopeReport(r *http.Request) string {
	if pinnedLocal(r) {
		return string(ScopeLocal)
	}
	return string(ScopeFleet)
}
