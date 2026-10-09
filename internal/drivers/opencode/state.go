package opencode

import (
	"context"
	"fmt"
	"net/url"

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

// lastTurnFailure asks the runtime's own message record whether the most
// recent turn ended in a provider-side error that GET /session/status never
// carries (muster #77). Measured live: a turn the provider refused
// with HTTP 402 recorded the refusal on its own assistant-message record
// and then simply left the status map — the session reads idle, at
// `confidence: observed`, forever after, indistinguishable from one that
// finished normally.
//
// Returns nil, nil whenever there is nothing to report: no message history
// yet, the newest message is not an assistant reply (a user message with
// no reply is #55's own "busy" territory, not this function's job), or
// that reply carries no error. A read failure here also returns nil, nil
// rather than propagating — the same best-effort-enrichment shape the tmux
// driver's own upgradeLastTurnFromRecord already takes: the status read
// that got this driver to "absent, therefore idle" already succeeded, and
// this is enrichment on top of a real answer, not the source of it. A
// caller wanting the message endpoint's own errors surfaced must read them
// off the returned error value, which this function discards on purpose.
//
// Bounded to the single newest message (?limit=1, confirmed live against a
// real server to return the TAIL of the conversation, not the head) —
// constant cost regardless of how long the session's history is — and
// never decodes "parts", the field carrying actual conversation content
// (see wireMessageInfo). This is why State calls it and List does not:
// List already visits every known session once per call, and one extra
// request per session there is a real, unbounded cost this function's
// single-session caller does not have. Left as a deliberate gap, not an
// oversight — worth its own issue if List needs the same honesty.
func (d *Driver) lastTurnFailure(ctx context.Context, srv *server, id string) *fleet.TurnEnd {
	var msgs []wireMessage
	if err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(id)+"/message?limit=1", nil, &msgs); err != nil {
		return nil
	}
	if len(msgs) == 0 {
		return nil
	}
	info := msgs[0].Info
	if info.Role != "assistant" || info.Error == nil {
		return nil
	}

	reason := info.Error.Name
	if info.Error.Data.Message != "" {
		reason += ": " + info.Error.Data.Message
	}
	// IsRetryable is only ever populated by the runtime for the "APIError"
	// variant (client.go's wireAssistantError doc). Every other Name leaves
	// the zero value, which is correctly read here as "not claimed
	// retryable" — the runtime never said either way for those, and this
	// must not guess yes on their behalf.
	retryable := info.Error.Name == "APIError" && info.Error.Data.IsRetryable
	return &fleet.TurnEnd{Outcome: "failed", Reason: reason, Retryable: retryable}
}
