package fleet

import (
	"encoding/json"
	"fmt"
)

// Status is a session's lifecycle state (§2.3, §8). It is a closed set:
// decoding an unrecognised or empty string is an error, never a silent
// default. A silently-defaulted zero value would be indistinguishable from
// an explicit answer — precisely the confusion §5.2 ("uncertainty travels")
// and §5.7 ("absence and failure are different answers") exist to forbid,
// applied here to a single field rather than a plural response.
type Status string

const (
	StatusStarting     Status = "starting"
	StatusWorking      Status = "working"
	StatusWaitingInput Status = "waiting_input"
	StatusIdle         Status = "idle"
	StatusQuotaBlocked Status = "quota_blocked"
	StatusDead         Status = "dead"
	// StatusUnknown is a valid answer, not an error (§2.3): the driver
	// could not determine the session's state and says so, rather than
	// guessing.
	StatusUnknown Status = "unknown"
)

func (s Status) valid() bool {
	switch s {
	case StatusStarting, StatusWorking, StatusWaitingInput, StatusIdle,
		StatusQuotaBlocked, StatusDead, StatusUnknown:
		return true
	default:
		return false
	}
}

// MarshalJSON rejects an invalid Status rather than emitting one silently.
// StatusUnknown included: it is the real value "unknown", never an empty
// string.
func (s Status) MarshalJSON() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid Status", string(s))
	}
	return json.Marshal(string(s))
}

// UnmarshalJSON rejects any value outside the closed set in §2.3, including
// the empty string — an absent or empty status is not the same fact as
// "unknown" and must not be silently coerced into it.
func (s *Status) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := Status(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid Status", raw)
	}
	*s = v
	return nil
}

// Confidence separates knowing from guessing (§2.3). Also a closed set, for
// the same reason as Status.
type Confidence string

const (
	// ConfidenceObserved means a driver read a structured status from an
	// API.
	ConfidenceObserved Confidence = "observed"
	// ConfidenceInferred means a driver guessed from terminal output,
	// process tables, or file mtimes. Both are legitimate; collapsing the
	// distinction is how a precise runtime's answer gets flattened to an
	// imprecise one's (§2.3) — the interface would then destroy the exact
	// advantage it exists to expose.
	ConfidenceInferred Confidence = "inferred"
)

func (c Confidence) valid() bool {
	return c == ConfidenceObserved || c == ConfidenceInferred
}

func (c Confidence) MarshalJSON() ([]byte, error) {
	if !c.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid Confidence", string(c))
	}
	return json.Marshal(string(c))
}

func (c *Confidence) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := Confidence(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid Confidence", raw)
	}
	*c = v
	return nil
}

// SessionState is a driver's answer to "what is this session doing" (§2.3).
//
// Since is the time the status was first observed to hold, not the time it
// began (§8) — for inferred states those differ, sometimes by a lot. Nil
// means the driver has no opinion on when the status started; a driver that
// knows must say so, not synthesize a value it doesn't have (§5.2 again,
// applied to a single field).
// WaitingReason is the closed vocabulary for WHY a caller is being made to
// wait, when the driver can tell. It has two homes, gated differently:
//
//   - SessionState.WaitingOn, below — populated only when Status is
//     `waiting_input`; empty for every other status. Unchanged by #126.
//   - PromptDelivery.WaitingOn (delivery.go, muster #126) — populated
//     while a create's initial prompt is still pending, independent of the
//     session's own Status: a session correctly reading `idle` or `starting`
//     can still be carrying an undelivered prompt, which is exactly why that
//     field cannot simply reuse this one (see PromptDelivery's own doc for
//     the measured harm that would cause).
//
// # Why this had to exist the moment there was a second reason
//
// `waiting_input` began meaning one thing — blocked on a question — and a
// caller could branch on `prompt` being present. Adding usage limits gave it a
// third meaning with no prompt attached, and at that moment the status became
// ambiguous in a way only the evidence prose resolved:
//
//	waiting_input, no prompt → holds unsent text? or something else?
//
// A quota block briefly landed in that ambiguity too, before it was moved to
// the `quota_blocked` status the spec had defined all along (F52). What
// remains is the distinction that genuinely belongs here: a question to answer
// versus text nobody submitted. They need opposite handling — one wants a
// choice, the other must NOT be sent to — and a caller that cannot tell them
// apart does the wrong one about half the time.
//
// Evidence cannot be the discriminator: §2.3 says it is prose for humans and
// must not be parsed, and this project has already paid twice for matching
// sentences that later changed.
//
// # muster #126: a third value, for a cause `waiting_input` never covered
//
// #126 measured seven distinct things that stop a session doing its work, on
// a live fleet, all reading identically through this API as "the session did
// not start". Two of the three classes the issue asked to be told apart at
// minimum were already this type's job: a dialog on screen is WaitingPrompt,
// and `SessionPrompt.Kind` already tells a folder-trust dialog from a
// tool-permission one apart in practice — kept exactly as it was, because
// #126 explicitly asked for that discipline to continue rather than be
// replaced. The third — the interface has not painted a composer AT ALL yet
// — had no equivalent for PromptDelivery.WaitingOn: `StatusStarting` already
// names the identical fact on SessionState as a whole, but that Status can
// read anything (a session can be `idle` with a prompt still pending, per
// PromptDelivery's own doc), so the create-time diagnosis needed its own
// value. WaitingStarting, below, closes that gap — and only for
// PromptDelivery.WaitingOn; it is never set on SessionState.WaitingOn, which
// already has StatusStarting for the same fact and would be redundant with
// it there.
//
// Not claimed to be exhaustive — #126's own issue says so explicitly: seven
// measured causes is evidence the number is greater than two, not a closed
// catalogue. Two of the remaining four are already answered by other,
// existing fields rather than folded in here: a mid-turn runtime failure is a
// fact about the LAST TURN, not about what a caller is waiting on right now,
// and already has TurnEnd for exactly that reason; a composer occupied by
// THIS driver's own earlier stranded delivery is not split out from
// WaitingUnsentInput on PromptDelivery, because at the point that field is
// computed — before this driver has ever delivered anything into the
// brand-new session a create's own prompt is addressed to — text already
// sitting in the composer cannot yet be this driver's own residue, so the two
// causes are not yet confusable the way they could be for a session already
// delivered to. The remaining two (a size-rejected send, and a control
// surface that never reaches this service at all) are, respectively, already
// fixed at a different boundary (#114) and a different layer's failure to
// classify, not this type's.
//
// # Absent means unclassified, not "no reason"
//
// §5.7 again. A driver that knows why says so; one that does not leaves this
// empty, and a caller must treat empty as "go look" rather than as any
// particular cause.
type WaitingReason string

const (
	// WaitingPrompt: a question is on screen. `Prompt` carries it.
	WaitingPrompt WaitingReason = "prompt"
	// WaitingUnsentInput: the composer holds text nobody submitted. `Since`
	// is the age, and the age is what separates somebody mid-thought from
	// text nobody is coming back for.
	WaitingUnsentInput WaitingReason = "unsent-input"
	// WaitingStarting (muster #126): the runtime has not painted an
	// interface capable of receiving input at all yet — no composer, no
	// dialog, nothing there to be blocked on. Distinct from both values
	// above: nothing is waiting on a human's answer and no text is sitting
	// unsent, because there is simply nothing painted yet to hold either.
	// Used on PromptDelivery.WaitingOn only — see that type's own doc for
	// why SessionState.WaitingOn does not need it.
	WaitingStarting WaitingReason = "starting"
)

// QuotaBlock describes an account-level refusal that outlives the screen that
// announced it.
//
// # Why a remembered fact rather than a screen read
//
// A usage limit is not a property of a session. It is a property of the
// ACCOUNT every session on the machine shares, it lasts days, and it appears on
// screen for only as long as nothing else prints. Measured on a live fleet
// hours after one began: 51 sessions, the notice still visible in 2 panes, and
// every other session showing an ordinary idle screen it would happily accept
// work into.
//
// Reading it from the screen therefore answers a different question than the
// one a caller is asking. "Is this session showing a limit notice right now" is
// a flicker; "can this machine do any work" is the state, and only the second
// prevents a supervisor dispatching to 48 sessions that cannot run.
//
// # How it clears, and why that is not a timer
//
// One session observed WORKING clears it, because a running turn is proof the
// account works — direct evidence, from the same reads already being made.
//
// ResetHint is not used to clear it. It is scraped prose ("Aug 10 at 12am"),
// the runtime is free to reword it, and a supervisor next door parsed the same
// line into "Aug 10 at 12am (Asia/Tokyo)      /usage-" with the next widget
// glued on. A hint is worth showing a human and is not worth acting on.
type QuotaBlock struct {
	// Since is when a limit notice was first seen on this machine.
	Since Timestamp `json:"since"`
	// ResetHint is the runtime's own words about when it lifts, when it said
	// anything. Display it; do not parse it.
	ResetHint string `json:"resetHint,omitempty"`
}

// TurnEnd says how the most recent turn FINISHED, when the screen says
// anything about it.
//
// # Why this is not a status
//
// A session whose turn died on a transient server error looks exactly like one
// that finished its work: the error prints, the spinner settles into its
// finished form, the composer empties. Both are `idle`, and `idle` is honest —
// the session is up and will accept input.
//
// What is missing is not the current state but a fact about the LAST TURN, and
// no status member carries it without lying about the present. `waiting_input`
// is the tempting hack and is wrong twice: nothing is being asked, and no human
// is needed — any caller resumes the session by sending anything at all.
//
// So the status stays `idle` and gains a footnote. A supervisor can then tell
// "finished, ready for the next thing" from "its work died and nobody noticed",
// which is the distinction `idle` had been collapsing.
//
// # Why not read the evidence string
//
// `Evidence` is prose for humans and explicitly not to be parsed. A caller that
// must ACT on this needs a field, or it ends up pattern-matching sentences this
// project keeps rewriting.
type TurnEnd struct {
	// Outcome is "failed" when the screen shows the turn ending in an error.
	// Absent otherwise: "it worked" is the unremarkable case, and recording it
	// would make every session carry a field nobody reads.
	Outcome string `json:"outcome"`

	// Reason is the runtime's own words, trimmed. For humans and logs; do not
	// branch on it.
	Reason string `json:"reason,omitempty"`

	// Retryable is true when the runtime itself called the failure temporary —
	// the difference between "poke it and the work continues" and "somebody
	// needs to look". Taken from what the screen SAYS, not inferred from an
	// error code we decided to interpret.
	Retryable bool `json:"retryable,omitempty"`
}

// WarningKind names what a footer notice (see Warning) is ABOUT, when this
// driver recognises its wording.
//
// # Why a named type with exactly one member today
//
// Unlike Status or PermissionModeState, this is not a survey of every shape
// the runtime's footer can take — nobody has looked at more than one notice
// yet. It exists so the *one* cause worth branching on today
// (muster#229, #230) has a name a caller can compare against instead of
// a substring it has to keep matching against prose the runtime is free to
// reword, and so a second cause found later has somewhere to go without
// widening Warning.Text's contract. More members arrive as they are
// measured, the same way WaitingReason's own doc says its set is not closed
// by having only three members.
//
// # Empty is a real answer, not an error — and not the same absence Status uses
//
// A Warning already existing in SessionState.Warnings is this driver's
// positive claim "a notice-shaped footer line is here" (see Warning's own
// doc for the structural test). Kind empty on that Warning says this driver
// does not yet recognise what the notice is about — the line is still
// surfaced, with Text carrying the runtime's own words, rather than being
// silently dropped for lack of a name. That is a different, milder absence
// than Status or ControlChannelState reject at the wire (those two are
// always-answered classifications where "" would mean nothing was decided);
// here "" coexists with a populated, meaningful Text on the same value, so
// there is no MarshalJSON rejection to match theirs — omitempty alone is
// enough to keep an unrecognised Kind off the wire.
type WarningKind string

const (
	// WarningTranscriptUnreliable: the runtime's own footer says its writes
	// to this session's transcript are failing — recent turns may not have
	// been recorded, independent of whether the turn itself is otherwise
	// healthy (muster#229's incident, #230's field). Matched on the
	// runtime's own wording ("Transcript writes are failing"), which is
	// exactly as durable as ControlChannelState's anchor and no more: it
	// holds until the runtime rewords the notice, at which point the line
	// still surfaces (see WarningKind's own doc) with Kind merely reverting
	// to empty rather than the notice disappearing outright.
	WarningTranscriptUnreliable WarningKind = "transcript-unreliable"
)

// Warning is one footer notice this driver read below the composer's closing
// rule — chrome the runtime redraws, never transcript the session under
// watch can write into (muster#229's boundary; see the tmux driver's
// footerLines).
//
// # Why this exists, and why it very nearly did not need to
//
// muster#229 found that this exact class of notice can satisfy the
// structural test spinner() uses for a real turn-status line — a single
// non-ASCII symbol, a space, a capitalised word, a tense marker — and fixed
// the misread by bounding spinner()'s backward scan at the composer's
// closing rule, so the notice is never read as a turn status again. That fix
// throws the line away once it has been ruled out as a spinner. This type is
// the other half #229 explicitly left open (see its own Background): a
// session whose transcript is not being recorded is worth a caller knowing
// about on its own terms, not only worth NOT misreading as "working".
//
// # Independent of Status, the same way ControlChannel and PermissionMode are
//
// A transcript-write failure changes nothing else SessionState reports — the
// composer, Status, Prompt all read exactly as they would without it — so a
// caller that only watches Status never learns that this session's most
// recent turns may be unrecorded. Folding it into Status or Evidence would
// repeat the precedence mistake §2.3's own history already names once for
// Quota; this is deliberately its own field for the same reason
// ControlChannel and PermissionMode are theirs.
type Warning struct {
	// Kind is the closed-so-far vocabulary a caller may branch on. See
	// WarningKind's own doc for why empty here is a real, expected answer
	// and not a decode failure.
	Kind WarningKind `json:"kind,omitempty"`

	// Text is the runtime's own words, trimmed — often truncated by the
	// runtime itself with a trailing ellipsis, the very shape that let #229's
	// incident happen. Prose for humans and logs; do not branch on it, the
	// same discipline TurnEnd.Reason and ControlChannel.Reason already hold
	// themselves to.
	Text string `json:"text"`
}

type SessionState struct {
	Status     Status     `json:"status"`
	Confidence Confidence `json:"confidence"`
	Evidence   string     `json:"evidence"`
	Since      *Timestamp `json:"since,omitempty"`

	// Prompt is the question this session is blocked on, when it is blocked
	// on one (§2.3, answered via §3's respond). Nil otherwise.
	//
	// It lives on the state rather than beside it so that every path which
	// reports state also reports the question: a single-session read, a
	// listing, and — the one that matters most — an event. A subscriber
	// learns that a session became blocked AND what it is asking in the same
	// message, instead of having to turn around and ask.
	//
	// Evidence names the highlighted option in prose; this is the structured
	// form a client can render as buttons and submit by index.
	Prompt *SessionPrompt `json:"prompt,omitempty"`

	// ComposerDigest fingerprints the unsent text, and is present only when
	// WaitingOn is WaitingUnsentInput.
	//
	// It exists so a caller can DISCARD that text without racing a human. The
	// text itself is deliberately not published: a listing that carried pane
	// content would turn every read into a transcript leak, and the caller
	// does not need the words — it needs to prove it is destroying the thing
	// it looked at. A digest does that and nothing else.
	//
	// Same discipline as `close` quoting `startedAt` back: the caller states
	// what it believes, and the driver refuses if the world has moved.
	ComposerDigest string `json:"composerDigest,omitempty"`

	// StrandedDelivery is true only while WaitingOn is WaitingUnsentInput and
	// the driver's own memory proves the text in the composer is a message IT
	// delivered into this session — one it reported `queued` and that the
	// runtime has since handed back to the composer unsent (#240). It is the
	// receiving side's half of what the sender was told: the sender may finish
	// the delivery by sending the same text (and the same `from`) again with
	// resumeIfStranded, with no composer digest and no screen reading.
	//
	// Absent is not "a person typed this". It also covers a driver with no
	// such memory — a restarted one, a peer built before the field existed, a
	// delivery older than the memory's retention — and the text may still be
	// anyone's; ComposerDigest and discard remain the way to deal with it.
	StrandedDelivery bool `json:"strandedDelivery,omitempty"`

	// WaitingOn says why the session is `waiting_input`, when the driver can
	// tell. Empty for every other status, and empty on waiting_input means
	// unclassified — see WaitingReason.
	WaitingOn WaitingReason `json:"waitingOn,omitempty"`

	// ControlChannel is what the runtime says about its own remote-control
	// connection, when it says anything (see ControlChannel).
	//
	// # Why a session-state field and not somebody else's problem
	//
	// A dead control channel raises no prompt, blocks nothing, and changes no
	// status. The session sits at an empty composer with a healthy status line
	// and reads, through every other field here, as an ordinary live session —
	// which is precisely what it is, except that nothing outside the machine
	// can reach it any more. Measured: 37 of 67 sessions came back from a
	// fleet-wide recovery in that state and not one of them was distinguishable
	// through this API.
	//
	// The supervisor that had to find them read pane text instead, and that is
	// the cost worth naming. Grepping panes for a disconnection notice
	// self-contaminates: the session doing the grepping printed the same
	// strings into its own transcript and classified ITSELF as broken. A field
	// read from the runtime's own status region is not forgeable that way,
	// because the transcript is not where it is rendered.
	//
	// # Independent of Status, always
	//
	// It never rewrites Status, the way Quota legitimately does. A session with
	// no control channel is still running, still holding its context, and still
	// perfectly able to work — it simply cannot be driven from elsewhere. Those
	// are different facts about different things, and folding one into the
	// other is the precedence mistake #10's findings already named once.
	//
	// Nil is a real answer and never means "connected" — see ControlChannel.
	ControlChannel *ControlChannel `json:"controlChannel,omitempty"`

	// PermissionMode is the permission mode the runtime shows the session to be
	// in, when the driver can read it (muster #194; see
	// PermissionModeState for the closed set and what each value means).
	//
	// # Why it is on the state
	//
	// `keys` can press BTab, which cycles the mode, and the receipt for a press
	// says only that the screen changed under it (docs/adr/188). A busy session
	// repaints for other reasons, and the runtime's cycle order decides which
	// mode a press lands in, so the receipt cannot say where the session is now.
	// A client that wants a named mode needs to READ it: press, read this, stop
	// when it matches. Reading it any other way meant a direct handle on the
	// terminal multiplexer, which is the thing `keys` exists to make
	// unnecessary.
	//
	// # Where it is read from, and why not the machine-local index
	//
	// From the runtime's own indicator row, below the composer's closing fence —
	// chrome the runtime redraws and nothing the agent prints can reach, the
	// same region and the same reason as ControlChannel. The machine-local
	// session index (#148) was measured as a source and set aside: it carries a
	// two-value class (bypass or prompting) written once when the session is
	// launched, so it cannot tell four prompting modes apart and it goes stale on
	// the first press of the key this field exists to serve.
	//
	// # Absent is not unknown
	//
	// Empty means nothing was read (the driver does not look —
	// DriverCapabilities.ObservesPermissionMode — or a dialog owns the screen).
	// "unknown" means the indicator area was read and named no mode; a client
	// cycling toward a target stops on it. Neither is ever a guess, and neither
	// is evidence about which mode the session is in.
	PermissionMode PermissionModeState `json:"permissionMode,omitempty"`

	// Warnings lists footer notices this driver read below the composer's
	// closing rule (muster#230) — chrome the runtime redraws, the same
	// region ControlChannel and PermissionMode are read from, and for the
	// same reason: independent of Status, and otherwise invisible through
	// every other field here (see Warning's own doc).
	//
	// Nil means no notice-shaped footer line was found on this read — a
	// positive finding (§5.7), not merely "not observed"; only one driver
	// reads footers at all today and it always looks, so there is no
	// separate capability flag the way ControlChannel and PermissionMode
	// have one — see those two fields' own DriverCapabilities entries before
	// adding a second driver that reads footers differently.
	Warnings []Warning `json:"warnings,omitempty"`

	// ScreenDigest fingerprints the whole screen this state was read from.
	//
	// It is the corroboration token for a RAW KEY (api-http.md §3.3, POST
	// …/keys): a caller quotes back the digest of the screen it looked at, and
	// the driver refuses if the screen has moved on. A key event has no
	// SessionPrompt.Nonce to check itself against — the screens that need arrow
	// keys are precisely the ones the classifier did not recognise — so this
	// takes the nonce's place, and it is the same discipline `close` uses with
	// `startedAt` and `discard` with ComposerDigest: the caller states what it
	// believes, and the driver refuses if the world has moved.
	//
	// A digest and never the text. The pane holds a conversation; a read that
	// published it would turn every listing into a transcript leak, and the
	// caller does not need the words — it needs to prove it is acting on the
	// thing it saw. Same trade ComposerDigest already makes.
	//
	// Not comparable ACROSS drivers or across restarts of one: it is whatever
	// fingerprint the driver that produced this state uses. Quote it back;
	// never compute one yourself and expect a match.
	//
	// Deliberately NOT a material change (see MateriallyDiffers). A screen
	// repaints on every character an agent prints, so a feed that fired on this
	// would emit an event per keystroke — the same reason Evidence is excluded,
	// and a much more expensive mistake because this field changes even when
	// the prose does not.
	ScreenDigest string `json:"screenDigest,omitempty"`

	// Quota carries the account-level block when Status is quota_blocked —
	// including a reset hint in the runtime's own words, so a caller need not
	// scrape it out of the evidence prose the way every consumer of this
	// screen has had to.
	Quota *QuotaBlock `json:"quota,omitempty"`

	// LastTurn reports how the most recent turn ended, when the screen says
	// anything about it. Nil is the ordinary case and means nothing was said —
	// NOT that the turn succeeded (§5.7: absence is not a finding).
	LastTurn *TurnEnd `json:"lastTurn,omitempty"`

	// Turns is how many agent turns have completed since this session's most
	// recent prompt delivery (muster #111) — the fact that tells "the
	// agent ran and produced nothing" apart from "the agent never ran at
	// all", which look identical through every other field: both read
	// `status: idle`, `screenDigest`/`composerDigest` empty, no pending
	// prompt. `turns: 0` alongside `idle` is that distinction, in one
	// comparison.
	//
	// # Why this is a liveness fact, not #82's result channel
	//
	// §5.8 permits "the runtime's own structured account of its own
	// condition" and forbids "content the session produced". This is the
	// first, not the second, on the same grounds `screenDigest` already
	// stands on: it is a count of runtime-written turn-boundary markers, not
	// a function of what was said — strictly less revealing than a
	// fingerprint of the whole screen, which §5.8 already permits. See
	// docs/adr/111-turns-is-a-liveness-fact-not-a-result-channel.md for the
	// full argument; do not re-derive it here.
	//
	// Pointer, not a bare int, and that is load-bearing (§5.7): `0` is a
	// POSITIVE finding — a delivery was made and nothing has completed since
	// — and absence is a DIFFERENT fact — this driver has no delivery mark
	// for this session, or could not read far back enough in the runtime's
	// own record to count honestly. A bare int would collapse those two
	// answers into one, which is the exact confusion §5.7 forbids. A driver
	// must never report 0 merely because it could not resolve a count.
	//
	// The denominator is "since the most recent delivery THIS DRIVER made
	// into this session's composer" — not the session's lifetime, and not
	// reset by resumeIfStranded finishing an earlier delivery (that
	// completes the SAME delivery, not a new one).
	Turns *int `json:"turns,omitempty"`

	// CredentialGeneration is the local credential store's own modification
	// time, as read at the moment this state was produced (#12).
	//
	// # What it answers, and what it does not
	//
	// It answers "which identity was locally in force when this state was
	// read" — nothing more. Paired with the session's own StartedAt (on
	// Session, one level up), a caller can evaluate the predicate a rebind
	// supervisor actually needs without this driver ever asserting an
	// account identity: startedAt before this value means the session began
	// under a credential that is no longer the one in force.
	//
	// It is NOT a claim that the session's runtime is still bound to
	// whatever it authenticated as, and must never be read as one. Every
	// local source this project measured — the runtime's own on-screen
	// status, this driver's classification, a supervisor's dispatch record —
	// reported a healthy binding for a session whose binding had gone stale,
	// because all of them ultimately quote the same process's announcement
	// about itself. Nothing added here changes that; it only makes the one
	// fact this driver CAN answer — which generation — askable, instead of
	// silently absent.
	//
	// # Why unconditional, not folded into Status
	//
	// Deliberately independent of Status and never rewrites it, unlike
	// Quota's effect on `idle`/`unknown`/`starting`. A credential transition
	// says nothing about what a session's own screen shows right now, and
	// smearing an account-level fact into session state is the exact
	// precedence mistake #10's own findings already named — repeating it
	// here for a second account-level fact would be the same defect twice.
	// The session genuinely remains locally dispatchable; only the
	// generation it started under may no longer be current.
	//
	// # Nil is a real answer
	//
	// Nil means no credential store is configured, or the stat failed —
	// §5.7's rule applied here: this driver looked and could not tell, which
	// is a different fact from "the generation is unknown to have changed".
	CredentialGeneration *Timestamp `json:"credentialGeneration,omitempty"`
}

// ObservedState constructs a SessionState a driver reports from a
// structured read (§2.3). Prefer this and InferredState over a bare struct
// literal: a hand-filled literal can typo Confidence into a value that lies
// about how the driver actually learned the status, and these two
// constructors are the only places that decision needs to be made
// correctly.
func ObservedState(status Status, evidence string, since *Timestamp) SessionState {
	return SessionState{Status: status, Confidence: ConfidenceObserved, Evidence: evidence, Since: since}
}

// InferredState constructs a SessionState a driver guessed at (§2.3). See
// ObservedState.
func InferredState(status Status, evidence string, since *Timestamp) SessionState {
	return SessionState{Status: status, Confidence: ConfidenceInferred, Evidence: evidence, Since: since}
}

// UnknownState constructs the §2.3 "a real answer, not an error" state. A
// driver that could not determine the session's status calls this rather
// than guessing at StatusIdle or StatusDead.
//
// Confidence is still a parameter here, not fixed: a driver whose API
// explicitly returned "I don't know" is ConfidenceObserved about its own
// ignorance; a driver that merely timed out trying to infer is
// ConfidenceInferred. The spec does not say StatusUnknown implies either —
// see the package doc's findings list.
func UnknownState(confidence Confidence, evidence string) SessionState {
	return SessionState{Status: StatusUnknown, Confidence: confidence, Evidence: evidence}
}

// MateriallyDiffers reports whether two readings of the same session differ in
// a way a subscriber must be told about (§7.3, api-http.md §4).
//
// # Why a status comparison was not enough
//
// The event plane began by emitting `session.state` whenever Status changed,
// and only then. Everything else on this struct changes underneath a silent
// feed: WaitingOn, Prompt — including its Nonce — ComposerDigest, Quota,
// LastTurn, CredentialGeneration. All of them move without Status moving.
//
// A client maintaining a mirror off that feed therefore holds a nonce for a
// question that is no longer on screen, and SessionPrompt.Nonce exists
// precisely to stop an answer being applied to a question the caller never
// read. A feed that under-reports does not merely go stale: it manufactures
// the failure the read path was built to refuse.
//
// # What is deliberately NOT material
//
// Evidence. §2.3 says it is prose for humans and must not be parsed, the
// runtime rewords it freely, and it changes on nearly every screen repaint —
// so including it would emit an event per keystroke while telling a subscriber
// nothing it may act on. A mirror's Evidence is therefore as fresh as the last
// material change, which is a real limitation and is stated rather than left
// to be discovered.
//
// Since on its own, for the same reason in a milder form: a driver re-stamping
// when a status was first observed is not a change in what the session is
// doing. It travels with every event that does fire.
//
// PermissionMode (#194) is material by that rule: a client cycling toward a
// named mode branches on it, and a mode change moves nothing else here — same
// status, same composer, same prompt — so a feed that did not fire on it would
// leave a mirror showing the mode the session had before somebody pressed the
// key. It changes only when the mode does, so it cannot produce a storm.
//
// Warnings (#230) is material for the identical reason ControlChannel is: a
// transcript-write failure appearing or clearing moves nothing else here, so
// a feed that did not fire on it would leave a mirror believing this
// session's transcript is fine long after the footer said otherwise — the
// same invisibility ControlChannel's own doc measured, one field over. It
// changes only when the footer's own set of notices changes, which is not a
// per-keystroke event, so it cannot produce a storm the way ScreenDigest
// would.
//
// The rule for adding a field to this comparison: if a caller may branch on
// it, it is material; if the documentation says not to parse it, it is not.
func (s SessionState) MateriallyDiffers(other SessionState) bool {
	switch {
	case s.Status != other.Status,
		s.Confidence != other.Confidence,
		s.WaitingOn != other.WaitingOn,
		s.ComposerDigest != other.ComposerDigest,
		s.StrandedDelivery != other.StrandedDelivery,
		s.PermissionMode != other.PermissionMode:
		return true
	}
	if !samePrompt(s.Prompt, other.Prompt) {
		return true
	}
	if !sameQuota(s.Quota, other.Quota) {
		return true
	}
	if !sameTurnEnd(s.LastTurn, other.LastTurn) {
		return true
	}
	if !sameControlChannel(s.ControlChannel, other.ControlChannel) {
		return true
	}
	if !sameTurns(s.Turns, other.Turns) {
		return true
	}
	if !sameWarnings(s.Warnings, other.Warnings) {
		return true
	}
	return !sameStamp(s.CredentialGeneration, other.CredentialGeneration)
}

// sameTurns compares #111's liveness count. A caller branches on it (that is
// the entire point of the field), and it changes at most once per completed
// turn — the driver's own memoising latch (see tmux's deliveryMark) is what
// keeps a flaky record read from turning this into a per-poll storm, the same
// discipline ScreenDigest's exclusion from this function protects against for
// a different, per-keystroke reason.
func sameTurns(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// samePrompt compares the parts of a prompt a caller answers by.
//
// Nonce alone would very nearly do — it changes whenever the prompt changes —
// but "very nearly" is the wrong standard for the field whose whole job is to
// make a stale answer detectable, and a driver that recycles a nonce would
// silently disable the comparison. Question is left out: it is best-effort
// prose above the options, and the options are the load-bearing part.
func samePrompt(a, b *SessionPrompt) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Nonce != b.Nonce || a.Kind != b.Kind || a.Selected != b.Selected {
		return false
	}
	if len(a.Options) != len(b.Options) {
		return false
	}
	for i := range a.Options {
		if a.Options[i] != b.Options[i] {
			return false
		}
	}
	return true
}

// sameQuota compares an account-level block. ResetHint is scraped prose the
// runtime is free to reword (see QuotaBlock) and is not compared; the block
// beginning or ending, and the moment it began, are what a caller acts on.
func sameQuota(a, b *QuotaBlock) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Since.Equal(b.Since)
}

// sameTurnEnd compares how the last turn finished. Reason is the runtime's own
// words and is explicitly not to be branched on, but it is compared here
// anyway: unlike Evidence it is written once when a turn ends rather than
// repainted continuously, so it cannot produce a storm, and a second failure
// with a different message is a second event worth having.
func sameTurnEnd(a, b *TurnEnd) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Outcome == b.Outcome && a.Reason == b.Reason && a.Retryable == b.Retryable
}

// sameControlChannel compares what the runtime says about its own control
// channel.
//
// Material, and it is the one field here whose materiality is the entire point:
// a channel going down changes nothing else on this struct — same status, same
// confidence, same composer, same prompt — so a feed that did not fire on it
// would reproduce, one layer up, exactly the invisibility the field was added
// to end. The recovery direction matters just as much: a supervisor never told
// the channel came back goes on believing the session is unreachable.
//
// BridgeID is compared as well (#276): the identifier is written once, with the
// channel coming up, so a change in it is a different channel — a caller that
// builds a viewer link from it must hear that the link it holds is stale.
//
// Reason is compared too, the same call sameTurnEnd already makes for its own
// Reason: it is written once, from a record entry the runtime appends when the
// channel fails (#69), not repainted continuously the way Evidence is — so
// comparing it cannot produce a storm, and a second failure explained
// differently from the first is a second event worth having.
func sameControlChannel(a, b *ControlChannel) bool {
	if a == nil || b == nil {
		return a == b
	}
	if (a.BridgeID == nil) != (b.BridgeID == nil) || (a.BridgeID != nil && *a.BridgeID != *b.BridgeID) {
		return false
	}
	return a.State == b.State && a.Reason == b.Reason
}

// sameWarnings compares the footer notices read below the composer.
//
// Order matters: both sides are built by the identical top-to-bottom scan
// over the same footer region (see the tmux driver's warningsOf), so two
// reads that found the same notices in a different order are evidence the
// footer itself reordered them — a fact worth an event, not noise to
// suppress by sorting it away.
func sameWarnings(a, b []Warning) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind || a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}

func sameStamp(a, b *Timestamp) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// ComposerRead is what an on-demand read of a session's composer returns
// (muster #276): the unsent text itself, and the digest `discard` accepts for it.
//
// It exists because SessionState.ComposerDigest says WHETHER text is sitting
// unsent and lets a caller destroy it safely, but not WHAT it is, and a
// supervisor that must report stranded operator text verbatim so a person can
// decide whether to resubmit it had no way to say more than "unsent, text
// unavailable".
//
// The text is deliberately served by its own route and never carried on a
// listing, a single-session read or the event stream: it can hold anything an
// operator typed, and those surfaces reach every principal that holds `read`
// without asking for it. Reading it is gated by `read` all the same (ruled
// 2026-10-09): every principal holding `read` can read what was left typed.
//
// Text and ComposerDigest are both empty when the composer holds nothing; the
// read then succeeds, since an empty composer is a real answer and not a fault.
// The digest is the SAME value State publishes, so a caller can pass it straight
// to `discard` as the expected digest.
type ComposerRead struct {
	Text           string `json:"text"`
	ComposerDigest string `json:"composerDigest"`
}
