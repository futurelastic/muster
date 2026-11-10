package service

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// §6 requirement 4: "Log every remote-originated mutation — actor, verb,
// target, outcome. This is the audit trail that replaces 'it could only ever
// have been me.'"
//
// That requirement was unimplementable until principals existed. A shared
// token can name an address, and an audit line reading "something at
// 10.x.y.z closed a session" answers where from while never answering who —
// which is the only question an audit trail is asked.
//
// Denials are logged as well as grants. A refused attempt is the more
// interesting line of the two: permitted work is expected, and a pattern of
// refusals is the first sign that a credential is somewhere it should not be.

// actorOf composes the audit actor through the SAME path that builds
// Request.Caller, rather than reaching for Principal.Name directly.
//
// The first version did reach for the name, and a live relayed mutation logged
// actor="<the relaying machine>" — losing the assertion that exists precisely
// so the peer can record who asked. Two places deriving "who is acting" is one
// too many, and the one nobody reads during a test is the one that drifts.
func actorOf(p Principal, r *http.Request) string {
	c := callerFor(p, r)
	if c.Person != nil {
		// "<person> · <principal> via <peer>" (muster #349): the person leads
		// because it is the answer to "who", with the principal that vouched for
		// them behind it.
		return c.Person.String() + " · " + c.Principal
	}
	return c.Principal
}

// auditWriter records the status a handler actually produced, so the audit
// line can state the OUTCOME rather than the authorization decision.
//
// The first version logged before running the handler, so every attempt read
// `outcome=permitted`. Four discard attempts in one minute — two refused with
// 409, one performed, one an idempotent retry — produced four identical lines.
// §6 requirement 4 asks for "actor, verb, target, outcome", and an audit trail
// that cannot distinguish a refusal from a destruction is not one.
type auditWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// outcomeOf names what happened in words an operator reads at speed.
//
// A refusal is called refused rather than "error": these are mostly §5.4
// corroboration failures, which mean the caller's belief was stale — a working
// safety property, not a fault, and it should not read like one at 3am.
func outcomeOf(status int) string {
	switch {
	case status == 0:
		return "no-response"
	case status >= 200 && status < 300:
		return "performed"
	case status == http.StatusConflict:
		return "refused-stale"
	case status == http.StatusNotFound:
		return "no-such-session"
	case status >= 400 && status < 500:
		return "refused"
	default:
		return "failed"
	}
}

// routeOf names the HTTP route a request actually matched — the method and
// path pattern ServeMux resolved it against (e.g. "POST
// /v1/machines/{machine}/sessions/{id}/input") — a fact the standard library
// itself records when the mux dispatches, not something a caller supplies.
//
// This is what muster#105 asked for: input and respond both require
// GrantSend, so verb=send alone cannot tell them apart in the audit trail,
// and muster#82 made that collision the ordinary case rather than a
// rare one — its endorsed reply-delivery convention has a dispatched worker
// call input on the requester's own session, so a delivered reply and a
// respond-to-a-dialog call now differ only by route, never by grant.
//
// It does NOT recover every collision #105 names. A delivered reply and an
// ordinary operator follow-up both match POST .../input — same method, same
// pattern — so no amount of route detail tells those two apart; #82's ADR
// already declined to add a caller-supplied marker for exactly that
// distinction ("proposed against a harm nothing has yet caused"), and this
// function does not reopen that. Whatever already distinguishes them does so
// through actorOf, when the two calls authenticate as different principals —
// unaffected by, and not a substitute for, this field.
func routeOf(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	// Defensive only: r.Pattern is populated by ServeMux once it has
	// matched, and every route this service logs is reached through NewMux.
	// A coarser fact beats a silently empty field if that ever isn't true.
	return r.Method + " " + r.URL.Path
}

// logActingDrop records an acting-person claim that was not honoured. A claim
// that is silently ignored would look, to the client that made it, exactly like
// one that was recorded.
func logActingDrop(p Principal, r *http.Request) {
	a := actingOf(r)
	if a.Dropped == "" {
		return
	}
	log.Printf("audit: acting-person dropped actor=%q route=%s claimed=%q reason=%q",
		callerFor(p, r).Principal, routeOf(r), a.Claimed, a.Dropped)
}

// requestTiming is what a request that wants its duration audited carries
// (muster #450): when it started, and the phases anything under it noted.
type requestTiming struct {
	start  time.Time
	phases *driver.Phases
}

type timingKey struct{}

// withTiming starts the clock on r and lets drivers note phases under it
// (driver.NotePhase). Called where an audited mutation begins — mutating() and
// the gates that stand in for it — so the line logMutation writes afterwards can
// say how long it took without every handler passing a stopwatch around.
func withTiming(r *http.Request) *http.Request {
	t := &requestTiming{start: time.Now(), phases: &driver.Phases{}}
	ctx := context.WithValue(r.Context(), timingKey{}, t)
	return r.WithContext(driver.WithPhases(ctx, t.phases))
}

func timingOf(r *http.Request) (*requestTiming, bool) {
	t, ok := r.Context().Value(timingKey{}).(*requestTiming)
	return t, ok
}

// logMutation writes the audit line of a mutation that ran.
//
// # took= and phase.* (muster #450)
//
// A create that took 33 s used to log exactly what a create that took 40 ms
// logged, so where the time went could only be estimated from the caller's side.
// The line now ends with `took=<ms>` — whole milliseconds from the moment
// the verb was admitted to the moment the handler returned — and, when a
// handler or a driver noted any, one `phase.<name>=<ms>` token each, in the order
// they finished. Both are APPENDED, so anything that read the line up to
// status= still reads it.
//
// Every mutating verb carries took=, not only create: it is one field, the
// question "was this slow" is the same for all of them, and a line that carried it
// for one verb would read as though the others had been measured and found
// instant. phase.* is only what was actually measured; a verb with no phases
// prints none rather than a zero.
func logMutation(p Principal, g Grant, target fleet.MachineId, r *http.Request, status int) {
	if g == GrantRead {
		return // reads are not mutations; logging them would drown the signal
	}
	line := fmt.Sprintf("audit: actor=%q verb=%s route=%s target=%s/%s outcome=%s status=%d",
		actorOf(p, r), g, routeOf(r), target, r.PathValue("id"), outcomeOf(status), status)
	if t, ok := timingOf(r); ok {
		line += " took=" + strconv.FormatInt(driver.Millis(time.Since(t.start)), 10)
		if f := t.phases.Fields(); f != "" {
			line += " " + f
		}
	}
	log.Print(line)
}

func logDenied(p Principal, g Grant, target fleet.MachineId, r *http.Request) {
	log.Printf("audit: actor=%q verb=%s route=%s target=%s/%s outcome=DENIED",
		actorOf(p, r), g, routeOf(r), target, r.PathValue("id"))
}
