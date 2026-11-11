package service

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// GET /v1/machines/{machine}/sessions/{id}/record (muster #441) — one session's
// record: what was said in it, by both sides, in the kinds the runtime's reader
// admits.
//
// The route is the wider sibling of turns (turns.go) and is deliberately a
// different route, on a different grant, with a different audit line and an
// operator switch of its own (docs/adr/441-a-sessions-full-record-is-read-under-its-own-grant.md).
// The boundary on WHAT may be returned is enforced where the record is read —
// each driver's allow-list over the runtime's own entries — and nothing here
// widens it. This file decides WHO may ask (recordGate), whether this machine
// serves the route at all (the switch), validates the query, and leaves the
// content-read audit line.

// isRecordPath reports whether a request path is the record route's. The route's
// last segment is literally "record", but a session may be named "record" too,
// so a suffix test would take GET …/sessions/record for it: only the exact
// /v1/machines/{machine}/sessions/{id}/record shape counts.
func isRecordPath(path string) bool {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	return len(seg) == 6 && seg[0] == "v1" && seg[1] == "machines" && seg[3] == "sessions" && seg[5] == "record"
}

// recordGate gates the record route behind the `record` grant.
//
// With a principal table the caller needs `record` and nothing else stands in
// for it: not `read`, not `send` (which guards turns), not `relay`. It
// deliberately does NOT also require relay for a peer target, the same ruling
// #81 made for reads; the owning machine applies its own table to the asserted
// caller (§13). Without a principal table the legacy model has no read/mutate
// split to consult and a valid token is the credential, exactly as for turns —
// which is why the operator switch (Config.RefuseRecord) exists.
//
// The grant is checked BEFORE the switch (handleRecord), so a caller without it
// learns nothing about how this machine is configured.
func recordGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principalOf(r); ok && !p.Allows(GrantRecord) {
			target := fleet.MachineId(r.PathValue("machine"))
			logDenied(p, GrantRecord, target, r)
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnauthorized,
				Message: "principal " + p.Name + " does not hold the record grant (§6), which reading a session's " +
					"record requires. It is its own grant: holding send, which guards turns, does not imply it, and " +
					"nobody holds it by default. This is expected until an operator adds record to this " +
					"principal's grants, not a bug.",
				Machine: target,
			})
			return
		}
		next(w, r)
	}
}

func handleRecord(svc *Service, refuse bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		query := r.URL.Query()

		// The clock starts when the read is admitted, so the audit line can say
		// how long it took.
		r = withTiming(r)

		// Every read of a session's record leaves a content-read line — the
		// caller, the session and how many entries left — whatever the outcome.
		// Never the content, a cursor or a model name.
		aw := &auditWriter{ResponseWriter: w}
		w = aw
		entries := 0
		reason := ""
		defer func() { logContentRead(r, machine, id, entries, aw.status, reason) }()

		if refuse {
			reason = fleet.ReasonRecordOff
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Reason:  fleet.ReasonRecordOff,
				Machine: svc.Self(),
				Message: "this machine does not serve session records: its operator set \"refuseRecordReads\": true " +
					"in the config file (#441). Nothing is wrong with the session or your grants; the operator of " +
					string(svc.Self()) + " decides this.",
			})
			return
		}

		q := fleet.RecordQuery{Since: query.Get("since")}
		if raw := query.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > fleet.MaxRecordLimit {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
					Message: "limit must be an integer from 1 to " + strconv.Itoa(fleet.MaxRecordLimit) +
						"; it is refused rather than clamped so a short page is never mistaken for a short record"})
				return
			}
			q.Limit = n
		}

		req := requestFrom(r)
		// A startedAt that does not parse is refused here, not silently dropped:
		// for a read of another session's words, dropping the check the caller
		// asked for is worse than refusing (the stance turns takes).
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
		reader, ok := d.(driver.RecordReader)
		if !ok {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot report a session's record",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		page, err := reader.Record(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, q)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		if page.Messages == nil {
			page.Messages = []fleet.RecordMessage{}
		}
		// The format is the answering driver's to state — a relay carries the
		// peer's — so it is filled in only where a driver left it unset.
		if page.Format == 0 {
			page.Format = fleet.RecordFormat
		}
		entries = len(page.Messages)
		// A record is read on purpose and is not something a cache should hold.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, page)
	}
}
