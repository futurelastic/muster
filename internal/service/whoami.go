package service

import (
	"crypto/subtle"
	"net/http"

	fleet "github.com/futurelastic/muster"
)

// handleWhoAmI answers GET /v1/whoami (muster #106): what the presented
// credential is authorized to do, on the machine named.
//
// # Why this route carries no grant of its own
//
// Every other route here is gated by withAuth (authentication: is this a
// credential this machine recognises at all) and then, for most reads, by
// reading()'s GrantRead check (authorization: may this credential see
// anything). This route deliberately skips the second gate — it sits behind
// withAuth alone.
//
// The reason is the sub-question muster #106 raised rather than
// answered: a principal holding NO grants at all still needs to be able to
// learn that it holds none. Gating this route on GrantRead would make it
// unusable by exactly the caller most likely to need it — the one every
// other route already refuses. Reporting a caller's own authority back to it
// is not the same risk reading() exists to gate: reading() protects OTHER
// principals' data (every session, every working directory, the full event
// stream) from a credential that was never granted read; this route protects
// nothing beyond what the credential already used to authenticate itself,
// and states only that credential's own name and grants — never another
// principal's (§6; see logDenied/logMutation for the audit trail those DO
// carry actor/verb/target/outcome for, which this route does not touch,
// being a read with nothing to log).
//
// # Why a peer machine never gets a real answer
//
// A relayed mutation needs a grant on the machine receiving the call AND a
// different grant on the machine that performs it (session-abstraction.md
// §7.7, muster #82's dispatch-reply convention, muster #68's
// federated-keypress precedent). This route can answer the first directly —
// it has just resolved that credential's own table to serve THIS request.
// It cannot answer the second the same way: unlike DriverCapabilities, which
// this service probes and caches per peer (RefreshCapabilities, GET
// /v1/runtimes), nothing here ever asks a peer what it has granted a given
// credential, and grants are per-machine configuration rather than a runtime
// fact worth building that machinery for. So every peer-machine answer is
// the same conservative floor CapabilitiesAssumed already means elsewhere:
// nothing confirmed, read as "go ask that machine directly," never as this
// service's own observation. Reusing that provenance shape rather than
// inventing a second one for the same "nobody has told me anything" fact is
// deliberate — muster #82 already made this argument once for
// capabilities, and a grant table nobody here has ever seen is the identical
// case.
func handleWhoAmI(svc *Service, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := fleet.MachineId(r.URL.Query().Get("machine"))
		if target == "" {
			target = svc.Self()
		}
		caller := callerFrom(r)

		if target != svc.Self() {
			writeJSON(w, http.StatusOK, fleet.GrantReport{
				Principal: caller.Principal,
				Machine:   target,
				Grants:    []string{},
				Source:    fleet.CapabilitiesAssumed,
			})
			return
		}

		// ?peer= is how a SERVICE probing this machine asks whether it is
		// listed here (muster #154). The roster is this machine's
		// configuration, so only a credential holding read gets an answer;
		// everyone else reads null, never false.
		var listsYou *bool
		if asker := fleet.MachineId(r.URL.Query().Get("peer")); asker != "" && callerHoldsRead(r) {
			_, listed := svc.peerDrivers()[asker]
			listsYou = &listed
		}

		writeJSON(w, http.StatusOK, fleet.GrantReport{
			Principal: caller.Principal,
			Machine:   svc.Self(),
			Grants:    grantsForRequest(cfg, r),
			Source:    fleet.CapabilitiesObserved,
			ListsYou:  listsYou,
		})
	}
}

// grantsForRequest translates this instance's authorization model into the
// vocabulary Grants() defines, for whichever model is actually configured.
//
// One function rather than two copies, so a grant reported here can never
// drift from a grant actually enforced by mutating()/reading() — the same
// lesson muster #80 recorded about a duplicated grant list inside the
// config loader, applied to a second duplicate this route could otherwise
// have grown.
func grantsForRequest(cfg Config, r *http.Request) []string {
	if p, ok := principalOf(r); ok {
		return grantNames(p.Grants)
	}
	return legacyGrants(cfg)
}

func grantNames(gs []Grant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, string(g))
	}
	return out
}

// callerHoldsRead reports whether the request's credential may read this
// machine's configuration. Single-token mode grants read unconditionally.
func callerHoldsRead(r *http.Request) bool {
	if p, ok := principalOf(r); ok {
		return p.Allows(GrantRead)
	}
	return true
}

// grantsForToken is what this machine's own authorization model grants a
// given credential — the self item's grantsToMe on GET /v1/machines
// (muster #154), evaluated for the credential this service presents to
// its peers. Same one-definition rule as grantsForRequest: a credential that
// matches nothing holds nothing.
func grantsForToken(cfg Config, token string) []string {
	if token == "" {
		return []string{}
	}
	if len(cfg.Principals) > 0 {
		for _, p := range cfg.Principals {
			if p.Token != "" && subtle.ConstantTimeCompare([]byte(p.Token), []byte(token)) == 1 {
				return grantNames(p.Grants)
			}
		}
		return []string{}
	}
	if cfg.Token != "" && subtle.ConstantTimeCompare([]byte(cfg.Token), []byte(token)) == 1 {
		return legacyGrants(cfg)
	}
	return []string{}
}

func legacyGrants(cfg Config) []string {

	// Legacy single-token mode has no per-verb table, only the two coarse
	// flags mutating() actually consults, plus reading()'s own documented
	// no-op: "reads may be granted broadly, mutations are opt-in" (see
	// Config.AllowLocalMutations). A caller authenticated under this mode
	// holds read unconditionally, both mutation flags collapsed into one
	// bit each, exactly as mutating() enforces them today — not a finer
	// grain than the model this instance is actually configured with.
	out := []string{string(GrantRead)}
	if cfg.AllowLocalMutations {
		out = append(out,
			string(GrantCreate), string(GrantSend), string(GrantInterrupt),
			string(GrantClose), string(GrantRename), string(GrantDiscard), string(GrantKeys),
			string(GrantLabel), string(GrantRemoteControl))
	}
	if cfg.AllowPeerRelay {
		out = append(out, string(GrantRelay))
	}
	return out
}
