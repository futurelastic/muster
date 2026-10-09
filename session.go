package fleet

import (
	"encoding/json"
	"time"
)

// Timestamp is a point in time as stamped by one machine's own clock (§11).
// Every response carries the stamping machine's current clock reading
// alongside it (Fleet-Clock, api-http.md §1); callers compute skew rather
// than assume synchronisation. Durations computed by one machine (Since,
// silence timers) are internally consistent even when clocks disagree;
// comparing raw Timestamps across machines is not safe.
type Timestamp = time.Time

// PermissionModeBypass is the one non-default permission posture this fleet's
// runtime has: the agent stops asking before it acts.
//
// A closed set of one, named rather than left as a free string, so an
// unrecognised mode is refused at the boundary instead of being handed to a CLI
// that may or may not understand it — §2.3's rule for statuses, applied to an
// input a caller controls.
const PermissionModeBypass = "bypass"

// SessionSpec is what a caller supplies to start a session (§2.1).
//
// Machine names the target host. The HTTP wire form
// (POST /v1/machines/{machine}/sessions, api-http.md §3.3) carries the same
// value positionally in the URL and does not repeat it in the request body
// — internal/service fills this field in from the path before handing a
// SessionSpec to a Driver. A Driver instance is already scoped to one
// (machine, runtime) pair (§4), so Machine reaching Driver.Create is
// redundant information a driver MAY use to assert consistency, never
// information it needs in order to route.
type SessionSpec struct {
	Machine MachineId    `json:"machine"`
	Runtime RuntimeId    `json:"runtime"`
	Cwd     AbsolutePath `json:"cwd"`

	// Agent, Model and Effort are hints, not guarantees (§2.1). A driver
	// that cannot honour one must say so at creation (via a "refused"-style
	// outcome or an error) rather than silently substituting a default —
	// see §4.3's DriverCapabilities.SupportsPin.
	Agent  AgentId `json:"agent,omitempty"`
	Model  string  `json:"model,omitempty"`
	Effort string  `json:"effort,omitempty"`

	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt,omitempty"`

	// Marker is a session-type stamp appended to the resolved name.
	//
	// It exists because the name is, on some substrates, the only channel
	// there is: it is what every listing, every remote client and every
	// human sees, and tooling that groups sessions by type has nothing else
	// to key on. A session created without one is not broken, it is
	// invisible to that tooling — which is a difference a caller could not
	// previously ask for OR detect.
	//
	// The driver carries a marker, never stacks it: a name that already
	// ends in one keeps the one it has. This holds regardless of what
	// alphabet the marker is drawn from — a marker built only from the
	// same characters as the name body is carried exactly like one that
	// is not. The service assigns no vocabulary of its own — what a
	// marker MEANS is the caller's business.
	Marker string `json:"marker,omitempty"`

	// Labels are caller-supplied facts about what this session is FOR —
	// the unit of work it serves, the working tree it uses, the kind of
	// session it is (muster #153). Opaque to the service exactly as
	// Marker is: the service assigns no vocabulary and attaches no meaning,
	// and two sessions carrying the same pair is not a conflict it reports.
	//
	// They exist so a consumer asking "is any machine already working on X"
	// reads a fact rather than parsing it back out of a name — names are
	// mutable, collide across machines, and a creative marker makes even a
	// session's type unrecoverable from them. Bounded by ValidateLabels.
	Labels map[string]string `json:"labels,omitempty"`

	// RemoteControl requests that the session be reachable by remote
	// clients rather than only from a terminal on the machine running it.
	//
	// Nil means "do whatever a first-class session on this substrate gets",
	// which is what an unaware caller wants and, on the tmux driver, means
	// enabled. It is a tri-state on purpose: the zero value of a bool would
	// silently mean "off", and the entire defect this field closes is that
	// sessions created through the API were quietly second-class — a
	// caller could not ask for the difference and could not detect it
	// afterwards.
	//
	// Like Agent, Model and Effort this is a HINT. A driver on a substrate
	// with no such notion must say so at creation rather than accept the
	// request and produce a session that cannot be reached.
	RemoteControl *bool `json:"remoteControl,omitempty"`

	// ContextRef is a path, never inline content (§5.3). It must never
	// reach a command line — anything that matches processes by argv can
	// otherwise match and terminate a session whose prompt merely contains
	// the string being hunted for.
	ContextRef AbsolutePath `json:"contextRef,omitempty"`

	// Env is variables the session's process must carry.
	//
	// # Why a session needs any, and why the API not having them was disqualifying
	//
	// An agent inside a session has to be able to identify itself to the tooling
	// around it — which session am I, where is the context staged for me, which
	// prior conversation am I re-attaching to. A supervisor answers that by
	// exporting variables into the session at creation. Without a field for it,
	// a session created through this API is a different kind of session from one
	// the supervisor starts itself, which is the whole class of defect §2.1's
	// first-class-session work exists to close.
	//
	// # Values never reach a command line (§5.3)
	//
	// The obvious mechanism — the multiplexer's own per-session `-e NAME=value`
	// flag — puts every value in an argv that any process on the machine can
	// read. That is precisely the rule §5.3 states for prompts and context, and
	// nothing about an environment variable makes it safer; the opposite, since
	// a credential is far likelier to arrive here than in a prompt.
	//
	// So values are staged in a file the session reads and unlinks, exactly as
	// ContextRef is a path rather than content. A driver that cannot deliver
	// them out of band must REFUSE the create rather than start a session
	// missing them — a session that comes up without its identity looks healthy
	// and fails later, somewhere else.
	//
	// # The shape a value may have, and why it is bounded
	//
	// Names must look like environment variable names; values may not contain a
	// newline or a NUL. The bound is honest rather than incidental: the staging
	// format is line-oriented, and a value with an embedded newline would
	// silently become two variables — the same fabrication-out-of-value-content
	// this driver already had to defend against when recording an environment.
	// A refused create says so; a truncated one does not.
	Env map[string]string `json:"env,omitempty"`

	// IsolateEnvironment requires that the session's process carries ONLY the
	// environment this create asked for: a small documented base, this
	// machine's configured sessionEnv entries, and Env — nothing inherited from
	// the service's own environment (muster #280).
	//
	// It is a requirement, not a preference. A driver that cannot deliver it
	// (DriverCapabilities.IsolatesEnvironment is false) REFUSES the create as
	// unsupported rather than start a session that leaks the service's
	// environment into a caller that asked for less (§2.1, §5.6). It is a plain
	// boolean because there is no honest request for the opposite: "please leak"
	// is not something a caller asks for, and a driver that always isolates
	// (opencode) simply honours it without effect.
	//
	// What it does not do: it cannot protect a credential the session needs and
	// was given in Env — the model-provider key lives in the session's own
	// environment — and it does not confine the filesystem (see §4.3).
	IsolateEnvironment bool `json:"isolateEnvironment,omitempty"`

	// Resume names a prior conversation this session continues.
	//
	// Distinct from DriverCapabilities.SupportsResume, which answers a different
	// question — whether sessions survive a service restart. A caller reading
	// that as "I can continue a conversation" would be wrong with nothing to
	// correct it, which is why this is a field of its own rather than a
	// capability flag.
	//
	// It is a HINT like Agent and Model: a runtime with no such notion must say
	// so at creation rather than start a fresh session that merely looks right.
	//
	// A resumed session commonly meets the runtime's resume chooser on the way
	// up. That question is deliberately NOT in Consents — see PromptKind and the
	// driver's own note: the option that means "the conversation I named" cannot
	// be identified from the option text, and a consent that guesses would pick
	// somebody's other session.
	Resume string `json:"resume,omitempty"`

	// ConversationId is a caller-chosen UUID this create asks the runtime to
	// start a NEW conversation under (muster #224), so the id is known
	// from the `201` rather than only after the runtime's own per-process
	// record shows up — measured at about 2s on a fresh session, during
	// which a caller wanting "click → open the session" has nothing to open.
	//
	// Mutually exclusive with Resume: one names a conversation to CONTINUE,
	// the other names one to START. Sending both is refused invalid, because
	// they answer the same question — "which conversation is this session's"
	// — in two incompatible ways.
	//
	// It is a HINT like Resume, Agent and Model: a driver with no way to
	// launch its runtime under a caller-chosen id must refuse the create as
	// unsupported rather than start a session under an id of its own
	// choosing that merely looks right (§2.1). See ValidateConversationId
	// for the shape this must have, and ConversationCaptured for how a
	// driver that honours it reports the id back before anything the
	// runtime wrote can be read.
	ConversationId string `json:"conversationId,omitempty"`

	// PermissionMode requests a runtime permission posture other than the
	// default. The only value this fleet's runtime has is "bypass" — the mode
	// in which the agent stops asking before acting.
	//
	// It is deliberately not a bool. A boolean field named for today's single
	// dangerous mode ages into a lie the moment a second mode exists, and the
	// closed set makes an unrecognised value a refusal rather than a silent
	// default — §2.3's discipline applied to an input.
	//
	// The service requires the `send` grant for this on top of `create`: a
	// session in this mode acts without asking, and the mode also raises an
	// acceptance screen that has to be answered. A principal permitted only to
	// start sessions must not be able to start THAT one.
	PermissionMode string `json:"permissionMode,omitempty"`

	// McpConfig names tool-server configuration files the session should be
	// started with. Each entry is an absolute PATH.
	//
	// # Paths, never inline configuration
	//
	// The same rule ContextRef follows, and for a sharper reason: such a
	// configuration commonly carries the credentials its servers authenticate
	// with, and §5.3 keeps context off command lines precisely so the payload
	// likeliest to be a secret is not the one exception. Inline content here
	// would land in an argv that every process table on the machine can read. A
	// caller holding a configuration in memory writes it to a 0600 file and
	// names the file — the same move Env already forces for the same reason.
	//
	// # A list, because the runtime flag repeats
	//
	// One entry is the ordinary case. The plural shape exists so a caller
	// composing a base configuration with a per-session addition does not have
	// to merge them itself and write a third file.
	//
	// # What this service does NOT do with them
	//
	// Nothing. It does not read, merge, validate or interpret the contents. A
	// service that parsed these would have begun to hold opinions about what a
	// session may talk to, which is a supervisor's judgement and §1's non-goal.
	// A driver checks that each path is absolute and that it can read it —
	// refusing a create rather than starting a session that will come up
	// healthy-looking and missing the tools it was created for, which is the
	// same refusal Env already makes in the same words.
	//
	// It requires the `send` grant on top of `create`, like PermissionMode and
	// for the same shape of reason: these configurations name servers the
	// session will LAUNCH, and between "may start a session" and "may start a
	// session that also starts these", the second is plainly the larger
	// authority.
	McpConfig []AbsolutePath `json:"mcpConfig,omitempty"`

	// Settings carries runtime settings the agent CLI reads only at launch
	// (muster #247) — a JSON object the driver serializes onto argv as the
	// CLI's `--settings '<json>'`.
	//
	// # Why a field, when Env exists
	//
	// The motivating switch is the one that makes a bypass-mode session accept
	// inbound cross-session messages (`{"crossSessionInbound":"accept"}`): the
	// CLI reads it from `--settings` or from its own settings file at boot and
	// from NO environment variable, so Env cannot carry it. Without a field here a
	// client that needs the switch has to start the process itself, and a session
	// started that way misses everything this service wires in at launch.
	//
	// # Scoped by key outside bypass (#254)
	//
	// With PermissionMode "bypass" any key is carried (#247). Without it only
	// the keys on the allow-list ValidateLaunchSettings enforces are — today
	// `crossSessionInbound` — and any other key is refused naming it, so a
	// session that still asks before acting cannot be widened by accident. #247
	// first scoped the whole field to bypass; that left a client relaunching a
	// default-mode session through `resume` unable to reproduce its argv. An
	// allow-list rather than a deny-list, because this service holds no opinion
	// about which of the CLI's settings widen a session: a deny-list is a claim
	// it cannot keep, and it fails open when the CLI adds a key. The
	// alternatives recorded for #247 — writing the value into the machine-wide
	// settings file, or this service emitting the one cross-session switch
	// itself — remain rejected for the reasons given there.
	//
	// It requires the `send` grant on top of `create`, like PermissionMode, in
	// every mode: the grant is by field, not by key.
	//
	// # Not for secrets
	//
	// The compacted JSON is an argv element, readable from any process table on
	// the machine — which is why McpConfig names files. Launch-time switches only.
	//
	// It is a HINT like Env and McpConfig: a driver with no CLI to hand it to
	// refuses the create as unsupported rather than drop it.
	Settings json.RawMessage `json:"settings,omitempty"`

	// Consents lists the boot questions the caller answers in advance, so the
	// driver may clear them instead of leaving a new session parked in front of
	// one.
	//
	// # Why a list, and why the kinds are named
	//
	// TrustCwd (below) shipped first and covers exactly one question. The moment
	// a second arrived — the acceptance screen a non-default PermissionMode
	// raises — it was clear the shape was wrong: one boolean per question means
	// a new field per runtime screen forever, and a caller cannot express "these
	// two, not that third one".
	//
	// Naming the kinds keeps the safety property that matters. A consent is
	// scoped to a question the driver can RECOGNISE (PromptKind), the option is
	// then found by reading the runtime's own option text, and an unrecognised
	// or ambiguously-worded screen is answered not at all. Nothing here is a
	// standing permission to answer whatever appears.
	//
	// Not every kind is consentable: see the driver. A question whose
	// affirmative option cannot be identified from its own text has no safe
	// consent, and offering one would be offering a coin flip.
	Consents []PromptKind `json:"consents,omitempty"`

	// TrustCwd carries the caller's consent to the runtime's own question
	// about Cwd — "is this a project you created or one you trust?" — so the
	// driver may answer it on the caller's behalf instead of leaving the new
	// session parked in front of it.
	//
	// Superseded by Consents, and kept because it shipped: it means exactly
	// `consents: ["folder-trust"]`, and a caller sending both is not in
	// conflict — the union is taken.
	//
	// # Why the consent is a field on the create request
	//
	// A runtime that asks this asks it BEFORE the session can do anything, and
	// it asks nobody in particular: on a fleet there is no human at that
	// terminal. Every client then meets the same wall — the create returns 201,
	// the session boots, and the work never starts. Measured on a live fleet: a
	// session parked on this question for two days, reading as merely
	// `waiting_input`, while the machine it was on had a supervisor watching.
	//
	// The decision is not the service's to take, and prompt.go says why at
	// length: a session service that decided what to answer would have become a
	// supervisor (§1). But it is not the service's to WITHHOLD either. The
	// caller already named this directory in this request — the same caller,
	// the same act, the same blast radius — and it is the only party in the
	// exchange with standing to say "yes, I trust it".
	//
	// So the consent travels WITH the directory it is about. It is scoped to
	// exactly one question (PromptFolderTrust) on exactly one session, the one
	// being created; it is never a standing permission, and it authorizes
	// nothing about the tool-permission or bypass screens, which ask something
	// else entirely and are deliberately not reachable this way.
	//
	// The zero value is the safe one: absent means the driver answers nothing,
	// which is what every existing caller already gets.
	//
	// # It is a HINT, like Agent and Model
	//
	// A runtime with no such question honours this by having nothing to do. A
	// driver that cannot answer prompts at all leaves the session as it found
	// it — the caller learns which it got by reading the session's state, where
	// an unanswered question is still reported in full.
	TrustCwd bool `json:"trustCwd,omitempty"`
}

// ConsentsTo reports whether the caller consented, in this create request, to
// having a question of this kind answered on its behalf.
//
// It is the one place the older TrustCwd boolean and the newer Consents list are
// reconciled, so no driver has to remember that two spellings of one consent
// exist. The union is taken deliberately: a caller sending both said the same
// thing twice, which is agreement, not conflict.
func (s SessionSpec) ConsentsTo(kind PromptKind) bool {
	if kind == PromptFolderTrust && s.TrustCwd {
		return true
	}
	for _, k := range s.Consents {
		if k == kind {
			return true
		}
	}
	return false
}

// SessionRef addresses a session (§2.2). Ids are machine-scoped and
// potentially recyclable (§5.4, §7.1): never treat an id match alone as
// identity, and never act destructively on one without corroborating at
// least one independent attribute (working directory, start time, name).
//
// ID is scoped to (Machine, runtime) — not to Machine alone. See the
// package doc's findings list for the consequence this has for the
// single-session HTTP URL shape, which carries no runtime segment.
type SessionRef struct {
	Machine MachineId `json:"machine"`
	ID      string    `json:"id"`
	Name    string    `json:"name,omitempty"`
}

// SessionRenamed reports that a session's id changed (§3's rename).
//
// Both ids are present and neither is optional. A subscriber holding the old
// one has to recognise the event as being about ITS session, which needs From;
// and it has to re-key, which needs To.
//
// StartedAt is the identity that did NOT change, and it is why a mutable id is
// safe to reason about at all: §5.4 already tells callers an id is not
// identity and must never be acted on alone. A rename is that rule with a
// sharper edge — the id can now change under a caller who was already
// forbidden from trusting it.
//
// Corroboration is why a given (From, To) pair can appear MORE than once on
// the stream (muster #103). A rename that is accepted and then reverts —
// #97's own measured scenario, a `202`, a correct read for roughly half an
// hour, then silently undone — used to emit exactly one of these, at accept
// time, and never revisit it: a subscriber that re-keyed on it, as §3.3 tells
// it to, held a name that had already stopped being true, with nothing on the
// stream saying so. See RenameCorroboration for what each value means and for
// the guarantee that one of its three non-accepted values always follows.
type SessionRenamed struct {
	Machine       MachineId           `json:"machine"`
	From          string              `json:"from"`
	To            string              `json:"to"`
	StartedAt     *Timestamp          `json:"startedAt,omitempty"`
	Corroboration RenameCorroboration `json:"corroboration"`
}

// RenameCorroboration is session.renamed's own truthfulness (muster
// #103): whether From/To is still just what the handler ACCEPTED, or has
// since been checked against an independent, later observation.
//
// A subscriber sees exactly one RenameAccepted for a given rename, at accept
// time, followed — always, never silently omitted — by exactly one of the
// other three once this service has something to say about whether it held.
// That is the fix for #97's actual gap: a `202`, a correct read for a while,
// then a silent revert with nothing on the stream saying so. It is closed by
// guaranteeing the "nothing" case cannot happen on the event plane, not by
// making the accept-time event wait for a confirmation that may be half an
// hour away.
type RenameCorroboration string

const (
	// RenameAccepted is the first session.renamed for a rename: the request
	// succeeded, at the moment it succeeded. This is the same fact the old,
	// single accept-time event carried — named honestly now as provisional,
	// rather than left to be read as a durability claim it never was.
	RenameAccepted RenameCorroboration = "accepted"
	// RenameCorroborated is a later session.renamed for the same rename: an
	// independent, later read found the new id still resolving, with no sign
	// of a revert, through the corroboration window.
	RenameCorroborated RenameCorroboration = "corroborated"
	// RenameContested is a later session.renamed reporting that the new id
	// stopped resolving AND the old id's identity came back — matched by
	// StartedAt, never by name alone (SessionRef's own rule, §5.4) — the
	// exact shape #97 measured: id, name and attach target all reverted.
	RenameContested RenameCorroboration = "contested"
	// RenameUnconfirmed is a later session.renamed reporting that this
	// service cannot say either way: the new id stopped resolving without the
	// old id's identity reappearing to corroborate a revert — as consistent
	// with an ordinary DELETE of the renamed session as with a revert this
	// service could not attribute — or the observation itself had a gap (a
	// feed disconnect, a resync) somewhere in the corroboration window. §5.7's
	// "known false, with evidence" applied to a rename: not a claim that it
	// held, and not a claim that it reverted, said plainly instead of by
	// omission.
	RenameUnconfirmed RenameCorroboration = "unconfirmed"
)

// Session is a SessionRef plus its current details and state — the shape
// GET /v1/machines/{machine}/sessions/{id} returns (api-http.md §3.3), and
// the item type of Collection[Session] returned by List (see
// internal/driver.Driver.List's doc comment for why List must return this
// shape, embedded state included, rather than a bare []SessionRef).
type Session struct {
	SessionRef

	Runtime RuntimeId    `json:"runtime"`
	Cwd     AbsolutePath `json:"cwd"`

	// Agent and Model are the APPLIED values — what the runtime is actually
	// using — reported only when the driver observed them, never an echo of
	// what SessionSpec requested (muster #84). Empty means the driver
	// does not know, the same real-answer rule StartedAt and Attach already
	// follow below; it is not a claim that no pin was requested. What was
	// REQUESTED, and whether it was honoured, is Pins — the two must not be
	// collapsed: a caller that read a request back here would be reading a
	// fabricated success, exactly the failure #84 measured.
	Agent AgentId `json:"agent,omitempty"`
	Model string  `json:"model,omitempty"`

	// Pins states what this session's create asked to pin — Agent, Model,
	// Effort — and, once it can be told, what the runtime actually applied
	// (muster #84; see PinOutcome). Nil means none of the three was
	// requested at creation. A field being nil inside a non-nil PinOutcome
	// means that particular pin was not requested; the other two follow the
	// same rule independently.
	Pins *PinOutcome `json:"pins,omitempty"`

	// StartedAt is when this session began, as observed by the machine
	// running it. It is what a caller sends back in Request.Expect to make
	// a destroy corroborable (§5.4) — without it in the read, the caller
	// has nothing to quote, and the strong guarantee is unreachable.
	//
	// Nil means the driver does not know, which is a real answer: not every
	// substrate records it.
	StartedAt *Timestamp `json:"startedAt,omitempty"`

	// Attach is how a human's terminal reaches this session, if the driver
	// knows (§2.8). Nil means it does not — a real answer, and the one a
	// driver over a substrate with no interactive attachment must give.
	//
	// It is on Session rather than SessionRef because a ref is an address and
	// this is a capability: two sessions with the same shape of id may be
	// attachable by entirely different means.
	Attach *AttachHint `json:"attach,omitempty"`

	// RuntimeSurface names where this session is reachable on a surface the
	// RUNTIME operates, distinct from Attach's local terminal and from
	// Conversation's transcript record (muster #85; see
	// RuntimeSurfaceRef). Nil means nobody looked — a substrate with no such
	// surface, or a driver that does not report one; ask
	// DriverCapabilities.ReportsRuntimeSurface to tell those apart. Non-nil
	// is always a fact this machine established, for the same reason as
	// Attach and Conversation.
	RuntimeSurface *RuntimeSurfaceRef `json:"runtimeSurface,omitempty"`

	// Conversation points at the runtime's own record of this session's
	// conversation — the first source in this service that is not the process
	// describing itself (see ConversationRef).
	//
	// Nil means NOBODY LOOKED: a substrate with no such record, or a driver
	// with lookup unconfigured. It is not a claim that no record exists — that
	// claim is Known false with the evidence for it, and the two must not be
	// collapsed (§5.7). A caller reading nil as "no record" would conclude that
	// a driver which cannot look has proved an absence.
	//
	// Non-nil is always a fact this machine established about this session, so
	// it is on Session rather than on SessionRef for the same reason as Attach:
	// a ref is an address, this is something learned.
	Conversation *ConversationRef `json:"conversation,omitempty"`

	// ResumeOutcome states whether this session's creation asked to resume
	// a conversation and, once it can be told, whether that was honoured
	// (muster #72). Named distinctly from SessionSpec.Resume (the
	// create-time request field, a bare conversation id) on purpose — the
	// two are never in the same message, but one names an intent and the
	// other names a verdict about it, and giving them the same wire name
	// across the API would blur that.
	//
	// Nil means no resume was requested at creation. It is never a claim
	// that one was requested and succeeded — that claim is
	// ResumeOutcome.Honoured true, and the two must not be collapsed
	// (§5.7), for the same reason Conversation's own nil does not mean
	// "no record".
	ResumeOutcome *ResumeOutcome `json:"resumeOutcome,omitempty"`

	// Carried is what this create took from the conversation it resumed
	// because the request itself did not say (muster #256; see
	// CarriedLaunch). Nil means nothing was carried — a create that did not
	// resume, a conversation this service holds no record of, or a request
	// that named everything itself. It is never a claim that the session
	// launched in the runtime's ordinary mode; read state.permissionMode for
	// what the session actually is.
	Carried *CarriedLaunch `json:"carried,omitempty"`

	// PromptDelivery is what became of a prompt this session's create carried
	// (muster #86; see PromptDelivery). Nil means this create carried no
	// prompt — never a claim that one was carried and delivered, the same
	// rule ResumeOutcome's own nil follows for resume.
	PromptDelivery *PromptDelivery `json:"promptDelivery,omitempty"`

	// Delivery is which delivery lane this session's input takes, when an
	// optional external delivery module is configured on the machine that owns
	// the session (#185; see DeliveryLane). Nil means nothing is configured or
	// nothing has been probed for this session — never "terminal".
	Delivery *DeliveryLane `json:"delivery,omitempty"`

	// IdentityAssertion is what this machine last asserted this session's
	// identity to be, and whether the runtime still carries it (muster
	// #97, #102; see IdentityAssertion). Nil means this machine has asserted
	// no identity for this session at all — an adopted, foreign or
	// cold-store session it never named, or a driver with no state store.
	// It is never a claim that the identity agrees; that claim is
	// IdentityAssertion.Drifted false.
	IdentityAssertion *IdentityAssertion `json:"identityAssertion,omitempty"`

	// Marker is the session-type marker this session's create carried and
	// the resolved name ended in (muster #165; see SessionSpec.Marker).
	// It is a fact the driver recorded at the instant it applied the marker,
	// so a consumer grouping sessions by type reads it here instead of
	// running a suffix test on Name — the test #90 and #96 measured as
	// ambiguous whenever a marker shares the name body's alphabet.
	//
	// Read-only. No endpoint writes it, and it is not a label: the label
	// namespace stays entirely the caller's (ADR 153), and a label keyed
	// `marker` is just a label. It survives a rename, because a rename
	// changes what a session is called and not what kind of session it is.
	//
	// Empty means this machine has no such record — no marker was asked for,
	// the name kept a different marker it already carried, the driver does
	// not record markers, or the session predates the field. It is never a
	// claim that the session is untyped.
	Marker string `json:"marker,omitempty"`

	// Labels are the caller-supplied facts attached to this session, at
	// create or later through POST …/labels (muster #153; see
	// SessionSpec.Labels). They are stored by the service, not observed by
	// the driver, which is why they survive a rename.
	//
	// Always present on the wire: a session with none encodes as `{}`, never
	// `null` and never absent (Session.MarshalJSON). "No labels" is a real
	// answer, and a DECODED Session whose map is nil therefore came from a
	// service that predates labels — which is how a relaying service tells an
	// older peer that ignored a label filter from a newer one that applied it.
	Labels map[string]string `json:"labels"`

	State SessionState `json:"state"`
}
