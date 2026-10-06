package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Outcome is the result of a Send call (§2.4). Delivery is not
// fire-and-forget and not a boolean.
type Outcome string

const (
	// OutcomeSubmitted means the driver confirmed the agent received the
	// input.
	OutcomeSubmitted Outcome = "submitted"
	// OutcomeQueued means the driver accepted the input but submission is
	// unconfirmed.
	OutcomeQueued Outcome = "queued"
	// OutcomeRefused means the driver declined; Reason explains why. This
	// is the important one (§2.4): a driver is expected to protect a
	// session from input that would corrupt it, and that protection
	// belongs in the contract, not in each caller's memory of a past
	// incident.
	OutcomeRefused Outcome = "refused"
	// OutcomeUnknown means the input was sent but the outcome is
	// unverifiable.
	OutcomeUnknown Outcome = "unknown"

	// The five values below are muster #119's own: a target's own
	// inbox — reached only when a driver capability-detects one and
	// delivers over it instead of the terminal surface (§2.4's four values
	// above are the terminal-surface vocabulary; these are distinct rather
	// than reused, because #119 was explicit that a richer vocabulary must
	// not be mapped down onto the existing three just because current
	// callers expect three values). OutcomeRefused above IS reused for an
	// inbox refusal — same word, same shape (Reason explains why, the
	// decision was made rather than left unresolved) — only these five
	// have no honest existing analogue.

	// OutcomeDelivered means the target's own inbox confirmed the message
	// reached the session as a genuine turn.
	OutcomeDelivered Outcome = "delivered"
	// OutcomeHeld means the target's own inbox is holding the message for
	// a human-approval step before it reaches the session. This is that
	// runtime's own last human checkpoint in the delivery path; a caller
	// must see it as held, never as a flattened success.
	OutcomeHeld Outcome = "held"
	// OutcomeDenied means the target's own inbox rejected the message
	// outright. Distinct from OutcomeRefused: a refusal is this driver's
	// own pre-write guard, decided before anything was sent; a denial is
	// the remote side's decision, reached after this driver already
	// attempted delivery.
	OutcomeDenied Outcome = "denied"
	// OutcomeExpired means the target's own inbox accepted the message and
	// it aged out unconsumed.
	OutcomeExpired Outcome = "expired"
	// OutcomeDropped means the target's own inbox discarded the message
	// for a reason it attributed to neither denial nor expiry.
	OutcomeDropped Outcome = "dropped"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeSubmitted, OutcomeQueued, OutcomeRefused, OutcomeUnknown,
		OutcomeDelivered, OutcomeHeld, OutcomeDenied, OutcomeExpired, OutcomeDropped:
		return true
	default:
		return false
	}
}

func (o Outcome) MarshalJSON() ([]byte, error) {
	if !o.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid Outcome", string(o))
	}
	return json.Marshal(string(o))
}

func (o *Outcome) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := Outcome(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid Outcome", raw)
	}
	*o = v
	return nil
}

// DeliveryReceipt is the result of send() (§2.4). Reason is populated on
// OutcomeRefused explaining why (e.g. "prompt holds unsent input") and is
// otherwise informational.
//
// A refusal is carried as an ordinary 200 response on the wire
// (api-http.md §3.3), never an HTTP error: it is an expected domain outcome
// carrying structured information, not a fault. Mapping it to 4xx would
// train clients to treat it as an exception and retry — exactly the
// behaviour the refusal exists to prevent.
type DeliveryReceipt struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`

	// Delivery names the path that made this receipt (#184). Nil means the
	// receipt names no path — an early refusal that never reached one, a
	// respond, a driver with a single path, or a peer built before the field
	// existed. It is never a guess: a consumer that needs to know which path
	// delivered reads it, and treats nil as "not stated".
	Delivery *DeliveryPath `json:"delivery,omitempty"`

	// SessionIds is set on a refusal whose address was not a session id but
	// the conversation id of live sessions (muster #268): the ids of those
	// sessions, so a caller retries with one of them instead of parsing
	// Reason. Absent on every other receipt, including the refusal for an id
	// that matches nothing at all.
	SessionIds []string `json:"sessionIds,omitempty"`
}

// Route names a delivery path, and — on the request side — the caller's choice
// of one (POST …/input's `route`, #184).
//
// Two of the values name a path a delivery can actually take. RouteAuto is
// only ever a REQUEST: it asks for the session's live delivery lane when it
// has one and the terminal otherwise (never the inbox, #257), and a receipt
// never carries it, because a receipt reports what happened.
type Route string

const (
	// RouteAuto asks the service to choose. It is the default — the zero
	// value of the Go field and an absent or empty `route` on the wire.
	RouteAuto Route = "auto"
	// RouteTerminal is the terminal module: the text is typed into the
	// session's composer and arrives as the user's own turn (labelled, unless
	// the sender is a human relay). Asked for by name on a session with a live
	// delivery lane it is refused (#257).
	RouteTerminal Route = "terminal"
	// RouteInbox is the runtime's own messaging socket: the text arrives as a
	// cross-session peer message, marked by the runtime as not from the user.
	RouteInbox Route = "inbox"
	// RouteModule is the RECEIPT's name for a delivery an optional external
	// delivery module carried (#185): the text arrived as the user's own turn,
	// like RouteTerminal, over a lane the module holds instead of the
	// composer. It is only ever a receipt value, and it is always paired with
	// DeliveryPath.Module, which names WHICH module — the module's own name is
	// what a caller writes in a request's `route` to force it, so the request
	// side has no fixed spelling and is validated against the machine's
	// enabled list, not against this constant.
	RouteModule Route = "module"
)

// Valid reports whether r is a route a caller may request from the fixed set.
// The zero value is not one: callers treat empty as auto before asking. An
// enabled delivery module's name is also a route a caller may request; that
// set is per-machine configuration, so the service checks it (#185), and
// RouteModule itself is a receipt value and is not requestable.
func (r Route) Valid() bool {
	return r == RouteAuto || r == RouteTerminal || r == RouteInbox
}

// IsPath reports whether r names a path a delivery can take — the values a
// receipt may carry.
func (r Route) IsPath() bool {
	return r == RouteTerminal || r == RouteInbox || r == RouteModule
}

// DeliveryPath is the receipt's account of how a send was carried out. It is
// an object, not a bare string, so a later field (a delivery module's name,
// #185) can join it without breaking a consumer.
type DeliveryPath struct {
	Route Route `json:"route"`

	// Module names the delivery module that carried the send (#185). It is
	// present exactly when Route is RouteModule and absent otherwise.
	Module string `json:"module,omitempty"`
}

// MarshalJSON refuses a path it cannot name, the same discipline Outcome
// applies: a receipt that claimed a route outside the set would decode on the
// other end as an unnamed fourth state. A module route with no module name,
// or a module name on any other route, is the same kind of incoherence.
func (p DeliveryPath) MarshalJSON() ([]byte, error) {
	if !p.Route.IsPath() {
		return nil, fmt.Errorf("fleet: %q is not a route a delivery can take", string(p.Route))
	}
	if p.Route == RouteModule && p.Module == "" {
		return nil, fmt.Errorf("fleet: a %q delivery path must name its module", string(RouteModule))
	}
	if p.Route != RouteModule && p.Module != "" {
		return nil, fmt.Errorf("fleet: only a %q delivery path may name a module", string(RouteModule))
	}
	type wire DeliveryPath
	return json.Marshal(wire(p))
}

// UnmarshalJSON on the RECEIPT is lenient about the route: a value this build
// does not know is dropped to "the receipt names no path" rather than failing
// the whole receipt. A newer peer adding a route (#185) must not make an older
// machine unable to read the outcome of a send that already happened — the
// outcome is the part that matters, and refusing it would invite a retry of a
// send that may have delivered.
func (r *DeliveryReceipt) UnmarshalJSON(b []byte) error {
	type wire struct {
		Outcome  Outcome `json:"outcome"`
		Reason   string  `json:"reason,omitempty"`
		Delivery *struct {
			Route  Route  `json:"route"`
			Module string `json:"module,omitempty"`
		} `json:"delivery,omitempty"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*r = DeliveryReceipt{Outcome: w.Outcome, Reason: w.Reason}
	if w.Delivery != nil && w.Delivery.Route.IsPath() {
		switch {
		case w.Delivery.Route == RouteModule && w.Delivery.Module != "":
			r.Delivery = &DeliveryPath{Route: RouteModule, Module: w.Delivery.Module}
		case w.Delivery.Route != RouteModule:
			r.Delivery = &DeliveryPath{Route: w.Delivery.Route}
		}
		// A module route naming no module is a path nobody could act on; it
		// reads as "not stated", like a value this build does not know.
	}
	return nil
}

// WithRoute returns r stating the path that made it. A route that is not one a
// delivery can take leaves the receipt unchanged, so a caller cannot stamp
// "auto" onto a receipt by accident. Naming a module is WithModule's job.
func (r DeliveryReceipt) WithRoute(route Route) DeliveryReceipt {
	// RouteModule is not stampable here: it is meaningless without the
	// module's name, and WithModule is the one place that supplies both.
	if route.IsPath() && route != RouteModule {
		r.Delivery = &DeliveryPath{Route: route}
	}
	return r
}

// WithModule returns r stating that the delivery module named module carried
// it (#185). An empty name leaves the receipt unchanged.
func (r DeliveryReceipt) WithModule(module string) DeliveryReceipt {
	if module != "" {
		r.Delivery = &DeliveryPath{Route: RouteModule, Module: module}
	}
	return r
}

// ModuleOf reports the delivery module r names, or "" when it names none.
func (r DeliveryReceipt) ModuleOf() string {
	if r.Delivery == nil || r.Delivery.Route != RouteModule {
		return ""
	}
	return r.Delivery.Module
}

// RouteOf reports the path r names, or "" when it names none.
func (r DeliveryReceipt) RouteOf() Route {
	if r.Delivery == nil {
		return ""
	}
	return r.Delivery.Route
}

// PromptDelivery is what became of a prompt a create carried (§2.1's initial
// prompt), reported on the session rather than in the create's own response
// because it cannot be known when that response is written.
//
// # Why the create response could not carry a DeliveryReceipt
//
// send() returns one synchronously because delivery has finished by the time
// it answers. A create's prompt is delivered AFTER the process starts and
// after the runtime has painted a composer to receive it — Create returns
// 201 while that is still ahead of it. So the receipt has to resolve later,
// which is the shape ConversationRef and ResumeOutcome already exist for.
//
// # The ambiguity this closes, which was measured
//
// A session created with a prompt, polled about twelve seconds later, read
// `status: idle, evidence: "interface painted, composer empty, no turn
// yet"`. That classification is exactly right for "up and waiting for work
// with nothing sent" — and indistinguishable from "up and waiting, and an
// accepted prompt has not been delivered yet". The caller concluded the
// prompt was lost and re-sent it four times. `starting` does not help; it
// names an earlier shape, a session too young to have painted a composer at
// all. And the natural client loop — create, then poll until idle or
// waiting_input — returns on exactly this window by construction
// (muster #86).
//
// # Three states (§5.7)
//
//	the field absent       this create carried no prompt. Never a claim that
//	                       one was carried and delivered.
//	Outcome nil + Evidence a prompt was accepted at creation and its delivery
//	                       has not resolved. A real, temporary answer — while
//	                       this state holds, `idle` is not evidence of loss
//	                       and a caller must not re-send. See SessionState.Turns
//	                       (muster #111): a turn observed to complete
//	                       AFTER this delivery is independent, driver-side
//	                       corroboration that it was received, on a substrate
//	                       where Outcome would otherwise sit here forever.
//	Outcome set + Evidence resolved, in the same closed set send() answers
//	                       with.
//
// # Why no Reason field beside Outcome
//
// DeliveryReceipt carries Outcome + Reason because a refusal needs
// explaining and a success does not. This type needs prose in every state —
// including the unresolved one, which has no receipt to draw a Reason from —
// so it carries the family's Evidence instead, and a driver holding a
// DeliveryReceipt copies that receipt's Reason into it. One prose field, not
// two.
//
// # WaitingOn: muster #126's machine-readable half of the pending diagnosis
//
// Evidence is prose no caller may parse (§2.3). #125 made that prose LIVE —
// it changes as the reason a prompt has not landed yet changes — and #126 is
// the finding that prose was, until this field existed, the ONLY answer:
// seven distinct causes measured on a live fleet all read identically
// through this API as "the session did not start", tellable apart only by a
// human reading a dialog, a transcript, or a config file by hand.
//
// WaitingOn carries the same closed vocabulary WaitingReason already gives
// SessionState.WaitingOn: a dialog is on screen (WaitingPrompt — Prompt still
// carries the specific question, exactly as it already does for an ordinary
// waiting_input session), the composer already holds text nobody has
// submitted (WaitingUnsentInput), or the interface has not painted a
// composer at all yet (WaitingStarting). It is set from the SAME
// classification that produces Evidence, in the SAME call
// (notePromptPending on the tmux driver), so the two can never drift into
// disagreeing about the same wait.
//
// Deliberately its OWN field rather than a reuse of SessionState.WaitingOn:
// that field is documented to mean something only when Status is
// waiting_input, and the measured harm this type's own doc already states —
// a session correctly reading `idle` while its prompt is still pending — is
// exactly the shape where writing SessionState.WaitingOn would misrepresent
// the present status. The same reasoning gave TurnEnd its own field rather
// than folding it into Status.
//
// Empty means unclassified (§5.7) — before the very first readiness poll has
// run, or a wait a driver has not taught this field about yet — never a
// guess. Meaningful only while Outcome is nil; once delivery resolves this
// driver leaves it at its zero value, the same way Evidence itself changes
// meaning from live diagnosis to terminal receipt without a separate field
// marking the switch.
type PromptDelivery struct {
	// Outcome is nil until delivery resolves. Once set it is the same value
	// send() would have returned for the same delivery.
	Outcome *Outcome `json:"outcome"`

	// Evidence is prose for humans, present in every state. Do not parse it
	// (§2.3); branch on Outcome being nil or set.
	Evidence string `json:"evidence"`

	// WaitingOn is #126's machine-readable class for Evidence, meaningful
	// only while Outcome is nil — see this type's own doc above.
	WaitingOn WaitingReason `json:"waitingOn,omitempty"`
}

// PromptDelivered states a definite answer: delivery has resolved to
// outcome.
func PromptDelivered(outcome Outcome, evidence string) *PromptDelivery {
	return &PromptDelivery{Outcome: &outcome, Evidence: evidence}
}

// PromptPending records that a prompt was accepted at creation and its
// delivery has not resolved yet — a real, temporary answer, never a
// stand-in for "no prompt was sent" (the absent field) or for a loss.
// waitingOn is #126's machine-readable class for evidence; empty is a
// legitimate "unclassified" (see PromptDelivery.WaitingOn), never a guess.
func PromptPending(waitingOn WaitingReason, evidence string) *PromptDelivery {
	return &PromptDelivery{WaitingOn: waitingOn, Evidence: evidence}
}

// ErrPromptDeliveryIncoherent is returned when a PromptDelivery would state
// something it cannot support — an outcome outside the closed set, or no
// evidence at all.
var ErrPromptDeliveryIncoherent = errors.New(
	"fleet: a PromptDelivery must carry evidence in every state, and a valid outcome when one is set")

func (p PromptDelivery) coherent() bool {
	if p.Evidence == "" {
		return false
	}
	return p.Outcome == nil || p.Outcome.valid()
}

type promptDeliveryWire PromptDelivery

// MarshalJSON refuses an incoherent value rather than emitting one — the
// same reasoning ConversationRef and ResumeOutcome both give: an outcome
// outside the closed set would silently decode as a fourth, unnamed state on
// the other end.
func (p PromptDelivery) MarshalJSON() ([]byte, error) {
	if !p.coherent() {
		return nil, ErrPromptDeliveryIncoherent
	}
	return json.Marshal(promptDeliveryWire(p))
}

// UnmarshalJSON applies the same rule to what a peer sends. A federated read
// is not a more trustworthy source than our own encoder.
func (p *PromptDelivery) UnmarshalJSON(b []byte) error {
	var wire promptDeliveryWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	got := PromptDelivery(wire)
	if !got.coherent() {
		return ErrPromptDeliveryIncoherent
	}
	*p = got
	return nil
}
