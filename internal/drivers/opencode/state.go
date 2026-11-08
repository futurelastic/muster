package opencode

import (
	"context"
	"fmt"
	"net/url"
	"time"

	fleet "github.com/futurelastic/muster"
)

// classify turns one session's entry in a SUCCESSFULLY-read status map —
// or its demonstrated absence from one — into a fleet.SessionState.
//
// present must be true only when the read that produced this map is known
// to have succeeded AND the session is known to exist (checked by the
// caller before this is reached, e.g. via a prior 200 on GET /session/{id}
// or membership in a GET /session listing). classify never decides
// existence; it only decides busy/retry/idle GIVEN existence, which is the
// shape #55's ruling asks for: "present ⇒ busy/retry; absent ⇒ idle ONLY
// IF THE READ ITSELF DEMONSTRABLY SUCCEEDED." A failed read must never
// reach this function at all — see ops.go, which returns the read's error
// directly instead of calling classify.
func classify(present bool, st wireStatus) fleet.SessionState {
	if !present {
		return fleet.ObservedState(fleet.StatusIdle,
			"absent from the runtime's status map: no active turn reported", nil)
	}
	switch st.Type {
	case "busy":
		return fleet.ObservedState(fleet.StatusWorking, "runtime reports an active turn", nil)
	case "retry":
		// muster issue #52's direction note, repeated in #55: the
		// runtime's own "retry" has no member on fleet.Status and must
		// not get one. It maps onto working plus TurnEnd.Retryable —
		// "poke-it-and-it-continues", not a status of its own. Outcome is
		// deliberately left empty: the turn has not ended, so "failed"
		// would be a claim this state does not make.
		s := fleet.ObservedState(fleet.StatusWorking,
			fmt.Sprintf("retrying turn (attempt %d): %s", st.Attempt, st.Message), nil)
		s.LastTurn = &fleet.TurnEnd{Retryable: true, Reason: st.Message}
		return s
	case "idle":
		// Measured behaviour (#55) omits idle sessions from the map
		// entirely, so this arm should be unreachable in practice. It is
		// kept as a defensive, honest no-op rather than falling into
		// "unrecognised" below, in case a future release starts reporting
		// it explicitly — the classification is the same either way.
		return fleet.ObservedState(fleet.StatusIdle, "runtime explicitly reported idle", nil)
	default:
		// A status type this driver does not recognise is §2.3's "carry,
		// don't flatten" rule with nothing to carry it in: fleet.Status is
		// a closed set (§2.3, F52's own lesson) and must not be extended
		// per-driver. Reporting unknown, honestly, is the only answer
		// that does not silently misclassify a future runtime release.
		return fleet.UnknownState(fleet.ConfidenceObserved,
			fmt.Sprintf("runtime reported an unrecognised status type %q", st.Type))
	}
}

// turnRead is one read of the runtime's newest message: how the turn it belongs
// to ended, and whether that message settles the question.
type turnRead struct {
	// turn is the failure the message records, nil when it records none or there
	// was nothing to read.
	turn *fleet.TurnEnd
	// settled is true when the newest message is the runtime's own reply, so the
	// read says how the newest turn ended. A user message at the tail, or none at
	// all, says nothing yet: the turn it starts has not been recorded, or is still
	// running (muster #426).
	settled bool
	// err is a read that failed. It says nothing about the turn, and is never kept.
	err error
}

// lastTurnFailure asks the runtime's own message record whether the most
// recent turn ended in a provider-side error that GET /session/status never
// carries (muster #77). Measured live: a turn the provider refused
// with HTTP 402 recorded the refusal on its own assistant-message record
// and then simply left the status map — the session reads idle, at
// `confidence: observed`, forever after, indistinguishable from one that
// finished normally.
//
// Returns nil whenever there is nothing to report: no message history
// yet, the newest message is not an assistant reply (a user message with
// no reply is #55's own "busy" territory, not this function's job), or
// that reply carries no error. A read failure here also returns nil
// rather than propagating — the same best-effort-enrichment shape the tmux
// driver's own upgradeLastTurnFromRecord already takes: the status read
// that got this driver to "absent, therefore idle" already succeeded, and
// this is enrichment on top of a real answer, not the source of it. A
// caller that must tell a failed read from an empty one uses turnVerdict.
//
// Bounded to the single newest message (?limit=1, confirmed live against a
// real server to return the TAIL of the conversation, not the head) —
// constant cost regardless of how long the session's history is — and
// never decodes "parts", the field carrying actual conversation content
// (see wireMessageInfo). State calls it for every idle read, and List for a
// read about ONE session (ListFilter.ID, muster #363); an unfiltered List asks
// at most once per finished turn, through the record (lastturn.go).
func (d *Driver) lastTurnFailure(ctx context.Context, srv *server, id string) *fleet.TurnEnd {
	return d.turnVerdict(ctx, srv, id).turn
}

// turnVerdict is lastTurnFailure with what it leaves out: whether the read
// failed, and whether the message it read settles how the newest turn ended.
func (d *Driver) turnVerdict(ctx context.Context, srv *server, id string) turnRead {
	var msgs []wireMessage
	if err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(id)+"/message?limit=1", nil, &msgs); err != nil {
		return turnRead{err: err}
	}
	if len(msgs) == 0 {
		return turnRead{}
	}
	info := msgs[0].Info
	if info.Role != "assistant" {
		return turnRead{}
	}
	r := turnRead{settled: true}
	// muster #293: a turn that ran on a provider outside the operator's set
	// is not an unremarkable idle, whatever its own outcome was. The runtime's
	// words about an error on that turn, if it has one, ride along.
	if reason, off := d.offPolicyTurn(info.ProviderID, info.ModelID); off {
		if info.Error != nil {
			if te := d.turnFailure(ctx, srv, info.Error, time.Now(), false); te != nil {
				reason += "; the runtime also reported: " + te.Reason
			}
		}
		r.turn = &fleet.TurnEnd{Outcome: "failed", Reason: reason}
		return r
	}
	if info.Error == nil {
		return r
	}

	// The name is the runtime's own, and where the API gave only its generic
	// placeholder, the one its log gave (muster #284) — see turnFailure.
	at := time.Now()
	if info.Time.Created > 0 {
		at = time.UnixMilli(info.Time.Created)
	}
	r.turn = d.turnFailure(ctx, srv, info.Error, at, false)
	return r
}

// offPolicyTurn reports whether a turn ran on a provider or model the policy
// does not allow, and says why in words the turn's outcome can carry. A record
// that names no provider is no evidence either way; one that names a provider
// and no model is judged on the provider alone.
func (d *Driver) offPolicyTurn(provider, model string) (string, bool) {
	if provider == "" {
		return "", false
	}
	v, rule := permitted, ""
	if model == "" {
		if !d.providers.permits(provider) {
			v = providerNotAllowed
		}
	} else {
		v, rule = d.providers.judge(provider, model)
	}
	switch v {
	case providerNotAllowed:
		return fmt.Sprintf("the turn ran on provider %q (model %q), which this machine does not allow (allowed: %s)",
			provider, model, d.providers.describe()), true
	case modelDenied:
		return fmt.Sprintf("the turn ran on provider %q (model %q), which this machine does not allow: denied by %s (allowed: %s)",
			provider, model, rule, d.providers.describe()), true
	case modelNotAllowed:
		return fmt.Sprintf("the turn ran on provider %q (model %q), which this machine does not allow: no allowed model on it matches (allowed: %s)",
			provider, model, d.providers.describe()), true
	}
	return "", false
}
