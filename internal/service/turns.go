package service

import (
	"context"
	"log"
	"net/http"
	"strconv"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// GET /v1/machines/{machine}/sessions/{id}/turns (muster #258) — what the
// session's agent wrote, and only that.
//
// The route is the one narrow reversal of #82's refusal to return content a
// session produced (docs/adr/258-assistant-turns-read.md). The boundary on WHAT
// may be returned is enforced where the record is read (the driver's allow-list
// over the runtime's own entries); this file decides WHO may ask, validates the
// query, and leaves the audit line.

// revealing gates a route that returns content a session produced.
//
// With a principal table the caller needs the `send` grant — the one `input`
// and `respond` already require, so no new grant exists to configure: whoever
// may speak to a session may read what it said back. It deliberately does NOT
// also require `relay` for a peer target, the same ruling #81 made for reads
// (reaching is not changing); the peer applies its own table to the asserted
// caller (§13). Without a principal table the legacy model has no read/mutate
// split to consult and a valid token is the credential, exactly as for every
// other read.
func revealing(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principalOf(r); ok && !p.Allows(GrantSend) {
			target := fleet.MachineId(r.PathValue("machine"))
			logDenied(p, GrantSend, target, r)
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnauthorized,
				Message: "principal " + p.Name + " does not hold the send grant (§6), which reading a session's " +
					"turns requires: the same grant as delivering input. Every grant defaults to denied — " +
					"this is expected until an operator adds send to this principal's grants, not a bug.",
				Machine: target,
			})
			return
		}
		next(w, r)
	}
}

func handleTurns(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		query := r.URL.Query()

		// Every read of session content leaves a line — the caller, the session
		// and how many turns left — whatever the outcome. Never the text.
		aw := &auditWriter{ResponseWriter: w}
		w = aw
		count := 0
		pending := 0
		defer func() {
			log.Printf("audit: actor=%q verb=read-turns route=%s target=%s/%s turns=%d pending=%d outcome=%s status=%d",
				callerFrom(r).Principal, routeOf(r), machine, id, count, pending, outcomeOf(aw.status), aw.status)
		}()

		q := fleet.TurnsQuery{Since: query.Get("since")}
		if raw := query.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > fleet.MaxTurnsLimit {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
					Message: "limit must be an integer from 1 to " + strconv.Itoa(fleet.MaxTurnsLimit) +
						"; it is refused rather than clamped so a short page is never mistaken for a short conversation"})
				return
			}
			q.Limit = n
		}

		req := requestFrom(r)
		// requestFrom ignores a startedAt it cannot parse. For a write that is
		// the weaker guarantee; for a read of another session's words it would
		// silently drop the very check the caller asked for.
		if query.Get("startedAt") != "" && req.Expect.StartedAt == nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
				Message: "startedAt is not an RFC 3339 timestamp"})
			return
		}

		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, fleet.RuntimeId(query.Get("runtime")), parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)
		reader, ok := d.(driver.TurnReader)
		if !ok {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot report a session's turns",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		page, err := reader.Turns(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, q)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		if page.Turns == nil {
			page.Turns = []fleet.Turn{}
		}
		count = len(page.Turns)
		if page.Pending != nil {
			pending = 1
		}
		writeJSON(w, http.StatusOK, page)
	}
}

// handleComposer returns the unsent text in a session's composer together with
// the digest `discard` accepts for it (api-http.md §3.3, muster #276).
//
// # Why its own route, and why `read`
//
// The text can hold anything an operator typed, so it is never carried on the
// listing, on a single-session read or on the event stream — those reach every
// principal holding `read` without anyone asking for the words. A route that is
// asked for, and audited, is the only shape that keeps the text out of them.
//
// The grant is the existing `read`; no new grant exists (ruled 2026-10-09). The
// trade-off was accepted knowingly: every principal that holds `read` can now
// read whatever an operator left typed and unsent, so the only lever for hiding
// it is withholding `read` (docs/api.md, the grant table). It deliberately does
// not also require `relay` for a peer target, the same ruling #81 made for
// reads; the peer applies its own table to the asserted caller (§13).
//
// Every read leaves an audit line — the caller, the session, how many
// characters left — whatever the outcome. Never the text.
func handleComposer(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")

		aw := &auditWriter{ResponseWriter: w}
		w = aw
		chars := 0
		defer func() {
			log.Printf("audit: actor=%q verb=read-composer route=%s target=%s/%s chars=%d outcome=%s status=%d",
				callerFrom(r).Principal, routeOf(r), machine, id, chars, outcomeOf(aw.status), aw.status)
		}()

		req := requestFrom(r)
		// Same stance as turns: a startedAt that does not parse is refused here,
		// not silently dropped, because dropping it removes the one check the
		// caller asked for on a read of another session's words.
		if r.URL.Query().Get("startedAt") != "" && req.Expect.StartedAt == nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
				Message: "startedAt is not an RFC 3339 timestamp"})
			return
		}

		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, fleet.RuntimeId(r.URL.Query().Get("runtime")), parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)
		reader, ok := d.(driver.ComposerReader)
		if !ok {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot report what is sitting unsent in a session's composer",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		out, err := reader.Composer(ctx, req, fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		chars = len([]rune(out.Text))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	}
}
