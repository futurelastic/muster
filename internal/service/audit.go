package service

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

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
	// refusal is the error the handler wrote, when it wrote one (muster #416).
	// writeError notes it here; the audit line prints its kind and a bounded
	// message, because the status alone says "refused" and never why.
	refusal *errorNote
}

// noteError is how writeError tells the audit writer which error it is about to
// send, so the line logged after the handler returns can carry it.
func (w *auditWriter) noteError(e *fleet.Error) { w.refusal = noteOf(e) }

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

// errorNoter is a response writer that wants to know which error was written to
// it. writeError tells the first one it finds on the writer chain.
type errorNoter interface{ noteError(*fleet.Error) }

// noteError hands e to the audit writer on w's chain, if there is one. A writer
// with no audit writer on it (the routes that log nothing) is left alone.
func noteError(w http.ResponseWriter, e *fleet.Error) {
	for w != nil {
		if n, ok := w.(errorNoter); ok {
			n.noteError(e)
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
}

// auditReasonMax bounds the message an audit line carries, in bytes, before it
// is quoted: a refusal's message can be as long as the caller made it, and a log
// line is not the place that is allowed to grow with it (muster #416).
const auditReasonMax = 200

// errorNote is what an audit line keeps of an error: a copy of its kind and
// message, so a handler that goes on to change or reuse the error cannot change
// a line that was about the one it sent.
type errorNote struct {
	kind    fleet.ErrorKind
	message string
}

func noteOf(e *fleet.Error) *errorNote {
	if e == nil {
		return nil
	}
	return &errorNote{kind: e.Kind, message: e.Message}
}

// fields renders the tokens appended to an audit line whose request was answered
// with an error: ` kind=<kind> reason="<message>"`, and nothing at all for a
// request that was not (a nil note).
//
// The message is quoted, so a newline or a quote in it cannot end the line or
// open a field of its own, and it is cut at auditReasonMax bytes on a rune
// boundary with `...` in the place of what was dropped. The kind is a closed set
// on this machine, but a relayed error carries whatever the peer sent, so a kind
// that is not a plain token is quoted too. An error with no message prints no
// reason rather than an empty one.
//
// The tokens come last on the line, after `took=` and any `phase.*`: a reader of
// those fields stops before a free-text one, and `reason="..."` here is the
// message the caller was shown, never the classifier a few lines carry as the
// bare `reason=record-off` or `reason=local-only`.
func (n *errorNote) fields() string {
	if n == nil {
		return ""
	}
	out := " kind=" + auditToken(string(n.kind))
	if n.message != "" {
		out += " reason=" + strconv.Quote(clipReason(n.message))
	}
	return out
}

// auditToken returns s as it stands when it is a plain token, else quoted.
func auditToken(s string) string {
	if s == "" {
		return `""`
	}
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return strconv.Quote(s)
	}
	return s
}

// clipReason returns s unchanged when it fits in auditReasonMax bytes, else its
// longest prefix that ends on a rune boundary, with `...` after it, the whole
// being auditReasonMax bytes or fewer.
func clipReason(s string) string {
	if len(s) <= auditReasonMax {
		return s
	}
	const mark = "..."
	cut := auditReasonMax - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
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
//
// # kind= and reason= (muster #416)
//
// A request answered with an error ends the line with ` kind=<kind>
// reason="<message>"` — the error the caller was shown, its message cut at
// auditReasonMax bytes (see errorNote.fields). A client refused hundreds of
// times a day left only `outcome=refused status=400`, and the one place every
// refusal passes through was the one that dropped the reason. They come after
// took= and phase.*, so a reader of the line up to there is unaffected.
func logMutation(p Principal, g Grant, target fleet.MachineId, r *http.Request, aw *auditWriter) {
	if g == GrantRead {
		return // reads are not mutations; logging them would drown the signal
	}
	line := fmt.Sprintf("audit: actor=%q verb=%s route=%s target=%s/%s outcome=%s status=%d",
		actorOf(p, r), g, routeOf(r), target, r.PathValue("id"), outcomeOf(aw.status), aw.status)
	if t, ok := timingOf(r); ok {
		line += " took=" + strconv.FormatInt(driver.Millis(time.Since(t.start)), 10)
		if f := t.phases.Fields(); f != "" {
			line += " " + f
		}
	}
	log.Print(line + aw.refusal.fields())
}

// logDenied writes the audit line of a request refused for want of a grant, with
// the error the caller is about to be sent: the denial already says which grant,
// and now the line says so too (muster #416).
func logDenied(p Principal, g Grant, target fleet.MachineId, r *http.Request, why *fleet.Error) {
	log.Printf("audit: actor=%q verb=%s route=%s target=%s/%s outcome=DENIED%s",
		actorOf(p, r), g, routeOf(r), target, r.PathValue("id"), noteOf(why).fields())
}

// logContentRead writes the audit line of a read of session content (muster #441:
// the record route).
//
// # How it is told apart from a state read
//
// A state read (a listing, a single session, the composer's digest) writes no
// line of this shape, and a content read writes exactly this one, with its own
// event name where a mutation carries `verb=`: `grep 'audit: content-read'`
// selects every record read and nothing else. It states who, which session and
// how many entries left, how it ended and how long it took — never what the
// entries said, the cursor, or the model.
//
// A read relayed to a peer writes one line on each machine: the entry machine
// names the caller, the owning machine names the caller it was told about
// ("X via <entry machine>"), the same shape every relayed verb has.
//
// reason is set only for the operator's refusal (`record-off`), so the line says
// why nothing was served. Any other error the handler answered with ends the line
// with ` kind=<kind> reason="<message>"` the way a mutation's line does (muster
// #416). A refusal for a missing grant is logDenied's line, from recordGate, which
// runs before this handler could write one.
func logContentRead(r *http.Request, target fleet.MachineId, id string, entries int, aw *auditWriter, reason string) {
	status := aw.status
	actor := callerFrom(r).Principal
	if p, ok := principalOf(r); ok {
		actor = actorOf(p, r)
	}
	outcome := outcomeOf(status)
	if reason != "" {
		// The operator's refusal answers 501, which outcomeOf reads as a failure.
		// It is a decision, not a fault, and the line should say so.
		outcome = "refused"
	}
	line := fmt.Sprintf("audit: content-read actor=%q route=%s target=%s/%s entries=%d outcome=%s status=%d",
		actor, routeOf(r), target, id, entries, outcome, status)
	if t, ok := timingOf(r); ok {
		line += " took=" + strconv.FormatInt(driver.Millis(time.Since(t.start)), 10)
	}
	if reason != "" {
		line += " reason=" + reason
	} else {
		// Any other refusal says why the same way a mutation's does (muster #416).
		line += aw.refusal.fields()
	}
	log.Print(line)
}
