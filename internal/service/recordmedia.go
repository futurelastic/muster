package service

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// GET /v1/machines/{machine}/sessions/{id}/record/media/{ref} (muster #442) — one
// image of a session's record, by the reference an `image` entry of the record
// carries.
//
// It is the record route's own sibling and nothing else: the same grant
// (recordGate), the same operator switch (Config.RefuseRecord), the same
// content-read audit line, the same startedAt corroboration. Holding `record` is
// exactly what reading an image costs; there is no grant for images alone. What
// may be served is decided where the record is read, by the same allow-list that
// issued the reference (docs/adr/442-…); this file decides who may ask, whether
// this machine serves the route at all, and what the answer looks like.

// isRecordMediaPath reports whether a request path is the media route's. Only the
// exact /v1/machines/{machine}/sessions/{id}/record/media/{ref} shape counts, as
// for isRecordPath: a session may be named "media" or "record".
func isRecordMediaPath(path string) bool {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	return len(seg) == 8 && seg[0] == "v1" && seg[1] == "machines" && seg[3] == "sessions" &&
		seg[5] == "record" && seg[6] == "media" && seg[7] != ""
}

func handleRecordMedia(svc *Service, refuse bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		ref := r.PathValue("ref")
		query := r.URL.Query()

		r = withTiming(r)

		// A media read is a content read like a record read: a line with the caller,
		// the session and how many images left (one, or none), whatever the outcome.
		// The reference is never in it: routeOf names the route's pattern, not the
		// path that was asked for.
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

		req := requestFrom(r)
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
		reader, ok := d.(driver.RecordMediaReader)
		if !ok {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot report an image of a session's record",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		media, err := reader.RecordMedia(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, ref)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		// Whatever the driver is — a relay carries a peer's answer — what leaves
		// here is an image of a type this API names, within its size cap, and
		// nothing else. A viewer is handed these bytes under a grant, so the
		// service does not trust the answer's label.
		if !servableMedia(media) {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
				Message: "the machine that owns this session answered with something that is not an image this API serves"})
			return
		}
		entries = 1
		h := w.Header()
		h.Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
		h.Set("Content-Type", media.ContentType)
		h.Set("Content-Length", strconv.Itoa(len(media.Data)))
		// A record is read on purpose and is not something a cache should hold, and
		// the bytes are never to be sniffed into anything a browser could run.
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(media.Data)
	}
}

// servableMedia reports whether an answer is an image this route may serve.
func servableMedia(m fleet.RecordMedia) bool {
	if len(m.Data) == 0 || len(m.Data) > fleet.MaxRecordMediaBytes {
		return false
	}
	for _, t := range fleet.RecordMediaTypes {
		if m.ContentType == t {
			return true
		}
	}
	return false
}
