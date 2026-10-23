package fleet

import "time"

// SessionPrompt is a question a session is blocked on, exposed so a caller can
// present the choices and answer one (§2.3, §3's respond).
//
// # Why the options are enumerated and not just described
//
// A supervisor's job here is to put the question in front of a human — on a
// phone, in a console, wherever — and submit what they pick. Evidence prose
// naming only the highlighted option is enough to explain a state and not
// enough to act on it.
//
// It matters more than it sounds. Two boot prompts observed on the same fleet:
//
//	❯ 1. Yes, I trust this folder        ❯ 1. No, exit
//	  2. No, continue without these        2. Yes, I accept
//
// Same shape, same footer, and the safe answer is at a different index. A
// caller that accepted the highlighted default would proceed in one case and
// kill the session in the other. Enumerating is what makes an answer a choice
// rather than a guess.

// PromptKind names what a prompt is ASKING, when the driver can recognise it.
//
// # Why this exists, and why it is advisory
//
// A caller that wants to answer prompts safely needs to distinguish "the
// question I know how to answer" from "a question I have never seen". Without
// a kind, every caller does that by matching the option text itself — and that
// is the fragility this project has paid for twice already, once on a footer
// and once on an animated glyph. Duplicated into N clients it is N places to
// be wrong.
//
// So the matching is done ONCE, here, and it is deliberately quarantined:
//
//   - **It is advisory.** Nothing in the service behaves differently because
//     of it. Options and Selected remain the answer; Kind only says what the
//     driver thinks is being asked.
//   - **It fails to empty, never to a guess.** An unrecognised prompt has no
//     kind, which is §5.7 again: not "no kind", but "not classified".
//   - **Empty is not permission.** A caller may auto-answer a kind it knows.
//     It must NEVER treat an unclassified prompt as safe — the default option
//     on a real prompt in the wild is "No, exit", and a client that answers
//     what it cannot read will eventually kill the session it meant to rescue.
//
// That last rule is the whole reason policy stays with the caller. This type
// says what is being asked; deciding what to answer is a supervisor's job, and
// a session service that made that decision would have become one (§1).
type PromptKind string

const (
	// PromptResumeChooser: the runtime asking how to resume a prior session.
	// Every session restored after a crash meets this one, and until it is
	// answered the agent does nothing at all.
	PromptResumeChooser PromptKind = "resume-chooser"
	// PromptFolderTrust: "do you trust the files in this folder".
	//
	// # Seeding is not this layer answering on a supervisor's behalf
	//
	// SessionSpec.Consents (and its predecessor TrustCwd) let a CALLER
	// answer this in advance, scoped to the one session it is creating —
	// the caller named the directory in its own request, so the decision is
	// still the caller's, this layer only executes it. Most sessions on a
	// real fleet are not created through this service at all, so no create
	// request exists for that consent to travel on, and internal/trustseed
	// exists for that gap: it writes the runtime's own answer to this
	// question — projects["<dir>"].hasTrustDialogAccepted, in the runtime's
	// own state file — ahead of time, for every directory under a fixed set
	// of operator-configured roots, so the question is never asked in the
	// first place.
	//
	// That is still not this layer deciding what to answer. The decision —
	// which directories are trusted — is made exactly once, by the
	// operator, in the machine's own configuration; trustseed only carries
	// that decision out, the same relationship Create already has to a
	// caller's per-request consent, extended from "one request" to
	// "standing policy". What would cross the line this type's own package
	// doc draws is this SERVICE choosing, at runtime, that some directory
	// should count as trusted because of something it observed — a prompt
	// it read, a session it is asked to start. trustseed never does that:
	// every root it seeds was named by an operator before any session
	// existed to ask about it, and it refuses (rather than guesses) a
	// directory that resolves outside every configured root. See
	// internal/trustseed's package doc for the mechanism and muster
	// issue #47 for where this line was drawn.
	PromptFolderTrust PromptKind = "folder-trust"
	// PromptExternalImports: "allow external CLAUDE.md file imports" — the
	// runtime's second boot question about a directory, raised when the
	// instruction files its working directory chain loads import a file from
	// OUTSIDE that directory. It holds a new session exactly as folder trust
	// does, before any bridge or conversation id exists, so a host cannot even
	// link to the session (muster #211). It is answered per project, and
	// the runtime's own default highlight is the DECLINE, so an answer must go
	// by index, never by the highlight.
	//
	// # Recognised from the options, and consentable
	//
	// Its two options are fixed strings the runtime emits ("No, disable
	// external imports" / "Yes, allow external imports"), which is the only
	// material classification may read. Like folder trust it is answered in two
	// standing ways that are both the OPERATOR's decision rather than this
	// layer's: internal/trustseed writes the runtime's own answer ahead of time
	// for every directory under the configured trust roots, and a create may
	// carry it in SessionSpec.Consents.
	//
	// The consent is a wider agreement than folder trust's, and the difference
	// is worth stating. A caller who names a directory can vouch for the
	// directory; it did not list the files that directory imports, and the
	// question's own warning is never to allow this for a repository the caller
	// does not own. So the consent is scoped exactly as narrowly as the rest of
	// the table — this one question, this one session, nothing standing — and it
	// takes the same `send` grant on top of `create` that every consent takes.
	PromptExternalImports PromptKind = "external-imports"
	// PromptSettingsTrust: an ADMINISTRATOR's managed-policy payload asking to
	// be approved. Not a working directory's own settings at all — read out of
	// the runtime binary, its neighbours are `policySettings`,
	// `managed-settings.json`, `managed-settings.d`, `/etc/claude-code` and the
	// per-user/device Managed Preferences directory, which are sources an
	// administrator controls, not sources a working directory carries.
	// (`projectSettings` is a different source in that same enumeration, and
	// raises no dialog at all.) The screen re-arms whenever that payload
	// changes, which is why it is its own kind rather than folded into
	// PromptFolderTrust.
	//
	// It is deliberately NOT consentable, and for a different reason than
	// PromptFolderTrust: a create-time consent here would let a
	// session-creating caller accept an ADMINISTRATOR's policy change on
	// behalf of an operator who never saw it — categorically outside what a
	// caller of this layer can speak for. Folder trust is not that: the caller
	// genuinely does own the directory it just named in its own request.
	//
	// (Read from one installed build; this repository's README pins the span
	// it is tested against, and that build sits outside it — a fact read from
	// one build is evidence about that build, not a guarantee across the span.)
	PromptSettingsTrust PromptKind = "settings-trust"
	// PromptToolPermission: a tool asking to run something.
	PromptToolPermission PromptKind = "tool-permission"
	// PromptBypassAcceptance: the permission-mode acceptance screen.
	//
	// # This kind is never produced by option matching, and that is deliberate
	//
	// It was, in intent, for a long time — by a rule requiring "bypass" and
	// "permissions" in one option. That rule could not fire. Read out of the
	// runtime's own binary rather than sampled from a screen capture, the
	// options it ships for that screen are "Yes, I accept" and "No, exit".
	// The identifying words live in the question, and the question is the one
	// thing option matching must not read (see the doc above: a ship decision
	// was once labelled with THIS kind because an agent typed "No auth bypass"
	// into its own prompt).
	//
	// So a driver may emit this kind only when it has evidence the screen text
	// cannot give it — for example, having started the session in that mode
	// itself. A caller must therefore treat its ABSENCE as uninformative here
	// in a way that does not apply to the other kinds: an unclassified boot
	// screen may well be this one.
	PromptBypassAcceptance PromptKind = "bypass-permissions"
	// PromptFeedbackReview: the runtime's own card offering a feedback draft
	// the agent wrote ("1 to review · 2 to send · 0 to dismiss"), muster
	// #215.
	//
	// # A notice, reported only while it is in the way
	//
	// The card is not a dialog. It sits above a live composer, owns no focus, and
	// its three keys act only while the composer is empty, so a session showing
	// it can still take a message. What it does is take rows: on the short pane a
	// fleet-created session has, it pushes the composer's closing rule off the
	// bottom, the composer can no longer be read, and nothing can be delivered or
	// confirmed. That is the one case this kind is reported in — the card over a
	// composer this driver cannot read whole — because only there is "waiting on
	// a person" true from a caller's side. On a taller pane the session is idle
	// and no prompt is reported.
	//
	// # The options are the runtime's verbs; the keys are not their positions
	//
	// Options are ["review", "send", "dismiss"], and a Choice is delivered as the
	// card's own key: 1, 2, and 0 for the third. There is no highlight, so there
	// is no default to accept, and Cancel is refused (Escape on an empty composer
	// dismisses the draft, and may be followed by a question about turning drafts
	// off). Choosing "send" does not send: the runtime first asks to confirm, in a
	// state this driver does not yet recognise.
	//
	// # Never answered on a client's behalf
	//
	// Like every kind it is advisory, and unlike the boot questions it is
	// deliberately absent from consentableKinds. Whether to review, send or
	// dismiss feedback is a person's decision about what leaves their machine.
	// A client puts this prompt in front of a person and does nothing else with
	// it.
	PromptFeedbackReview PromptKind = "feedback-review"
)

// # A known gap: the tool-server ("MCP") trust dialog has no kind yet
//
// Passing SessionSpec.McpConfig at creation can raise a boot question of its
// own — "do you trust the servers named in this configuration" — distinct
// from PromptToolPermission (which asks about one RUNTIME call, not a
// configuration file read at boot) and from PromptFolderTrust (which asks
// about the working directory, not a file the caller pointed at
// separately). It has stranded sessions on two machines, prompt undelivered,
// looking to an operator exactly like a broken spawn (muster #128).
//
// It is deliberately NOT one of the kinds above. Every kind above was added
// from the runtime's own OPTION text, read off an installed build — see
// classifyPromptKind's package doc for why option text is the only safe
// source, and why the question itself must never be read. No such reading
// has been taken for this dialog: the sessions that hit it were not
// captured, and inventing option-text needles from the question alone would
// repeat the exact mistake that once mislabelled a ship decision as
// bypass-permissions. A kind guessed into existence would let a caller's
// Consents auto-answer a screen this package has never actually seen —
// worse than leaving it unclassified, because unclassified at least reads
// as "unknown" rather than as false confidence.
//
// Add it the same way every other kind was: capture the real screen from an
// installed build, add the needle to classifyPromptKind, and — only if its
// affirmative option is as unambiguous as folder-trust's — an entry to
// consentableKinds. Never the reverse order.

// PromptTabState says where one tab of a tabbed question dialog stands.
type PromptTabState string

const (
	// PromptTabAnswered: the tab's question has been answered (the runtime
	// paints it ☒) and is not the one on screen.
	PromptTabAnswered PromptTabState = "answered"
	// PromptTabCurrent: the tab on screen — the one State.Prompt's Question
	// and Options describe. It is current even if it was answered earlier and
	// the dialog was walked back to it.
	PromptTabCurrent PromptTabState = "current"
	// PromptTabPending: not answered yet (painted ☐) and not on screen.
	PromptTabPending PromptTabState = "pending"
)

// PromptTab is one question tab of a tabbed dialog's bar.
type PromptTab struct {
	// Header is the tab's label as the runtime paints it, without its
	// answered/pending glyph. It is the agent's own text.
	Header string         `json:"header"`
	State  PromptTabState `json:"state"`
}

// PromptOptionDetail is what the runtime draws with one option besides its
// label: the description the agent wrote under it and, for the highlighted
// option of a dialog with a preview pane, the pane's text.
type PromptOptionDetail struct {
	// Label repeats the option's text, so an entry reads on its own.
	Label string `json:"label"`
	// Description is the row or rows painted under the option. Rows a wrapped
	// description spans are joined with a single space. Empty when the option
	// has none.
	Description string `json:"description,omitempty"`
	// Preview is the preview pane's text, rows joined with a newline. It is
	// reported on the highlighted option only — the runtime draws one pane, for
	// whichever option is highlighted — so a client must not read its absence on
	// another option as "that option has no preview".
	Preview string `json:"preview,omitempty"`
}

type SessionPrompt struct {
	// Question is the text above the options, best effort. It may be empty
	// when the prompt is terse; the options are the load-bearing part. It is
	// for showing to a person, not for parsing, and a driver bounds it: a long
	// one may be reported from part-way through.
	Question string `json:"question,omitempty"`

	// Options in the order shown, 1-based when referenced by Response.Choice.
	Options []string `json:"options"`

	// Selected is the 1-based index of the highlighted option — what a bare
	// confirmation would accept. Never assume it is the safe one.
	Selected int `json:"selected,omitempty"`

	// OptionDetails carries what each option is drawn with besides its label
	// (muster#324): the description under it and the highlighted option's
	// preview. It is parallel to Options — the same length and order when
	// present — and Options is unchanged, so a client that has never heard of
	// it reads exactly what it always did.
	//
	// It matters because a question's own prose is not in the transcript until
	// it has been answered: while it is open, the screen is the only source.
	//
	// It fails to absent: omitted when no option has a description or a preview,
	// and on a peer built before it existed. It is NOT part of Nonce — a preview
	// follows the highlight, and a nonce that moved with it would refuse an
	// answer to a question that had not changed.
	OptionDetails []PromptOptionDetail `json:"optionDetails,omitempty"`

	// MultiSelect reports that this is a multi-select question: its first
	// options are checkboxes, painted with their tick state in the option
	// text ("[ ] …" clear, "[✔] …" ticked, on the one runtime measured), and
	// a single Choice would flip one box rather than answer anything. Answer
	// it with Response.Choices instead.
	//
	// False means "not recognised as multi-select", not "known to be
	// single-select": a driver sets it only on a shape it can corroborate,
	// and a caller must never send Choices to a prompt without it. An older
	// peer that has never heard of Choices never reports this field either,
	// so the field doubles as the capability signal — a caller that follows
	// the rule cannot send Choices to a driver that would ignore it.
	MultiSelect bool `json:"multiSelect,omitempty"`

	// FreeText reports that this question offers the runtime's free-text row
	// ("Type something") and that a driver can answer through it: send
	// Response.Text rather than picking a listed option. The row is still one
	// of Options — at its own index, so the numbering a caller already relies
	// on does not move — but it is not one of the agent's choices, and a
	// caller drawing the options as a list should draw it as an input.
	//
	// False means "not recognised as answerable this way", not "known to
	// have none": a driver sets it only on a shape it has measured, and a
	// caller must never send Text to a prompt without it. An older peer that
	// has never heard of Text never reports the field either, so — as with
	// MultiSelect — the field is also the capability signal: a caller that
	// follows the rule cannot send Text to a driver that would ignore it and
	// read the rest of the body as "accept the highlighted option".
	//
	// It is false again once the row holds text: the row can only be found by
	// its placeholder, and after an answer has been typed there is nothing
	// left to find it by.
	FreeText bool `json:"freeText,omitempty"`

	// Tabs is the tab bar of a tabbed question dialog, one entry per question
	// in the order the bar shows them (the runtime's own "Submit" tab is not a
	// question and is not listed), and Tab the 0-based index of the one this
	// prompt is. Together they let a caller draw progress — "question 2 of 4",
	// what the others are called, which are answered — where Question and
	// Options describe the current tab alone (muster#242).
	//
	// Both fail to absent, never to a guess. They are absent on a dialog with
	// fewer than two questions (a lone question has no bar worth drawing), on
	// a bar whose current tab could not be read from the screen, and on a peer
	// built before the field existed — so the field is its own capability
	// signal, as MultiSelect and FreeText are. Tab is also absent while the
	// bar is shown but the highlight is on its Submit tab (the review screen):
	// the tabs are all there, and none of them is current.
	//
	// The tab bar is what the screen shows and nothing more: it carries the
	// tab headers, never another tab's question or options, which the
	// runtime does not draw until that tab is current.
	Tabs []PromptTab `json:"tabs,omitempty"`
	Tab  *int        `json:"tab,omitempty"`

	// Kind is what the driver thinks is being asked, or empty when it does
	// not recognise the question. Advisory — see PromptKind. Empty must never
	// be read as "safe to answer".
	Kind PromptKind `json:"kind,omitempty"`

	// AutoDenyAt is when the runtime will deny this request by itself, for a
	// dialog that carries an unattended-session countdown ("will automatically
	// deny this request in 1:13", muster#353). It is computed from the
	// countdown when the screen is read, to the second, so two reads of one
	// dialog agree to within the second they were taken in — and a client can
	// show "auto-denied in 1:13", or tell a denial by the clock from an answer
	// someone gave.
	//
	// The countdown row itself is not in Question and not in Nonce: it changes
	// every second while the dialog does not, and a nonce that moved with it
	// refused every answer. Fails to absent: omitted on a dialog with no
	// countdown, and on a peer that predates it. Absent is not "never denied".
	AutoDenyAt *time.Time `json:"autoDenyAt,omitempty"`

	// Nonce changes whenever the prompt changes.
	//
	// A caller reads a prompt, shows it to a human, and answers some seconds
	// or minutes later. In between, the session may have moved on and be
	// showing a different question at the same place on screen — and an
	// answer meant for the old one would be applied to the new one, silently
	// and by index. The nonce turns that into a refusal.
	//
	// This is §5.4's "a proxy for identity is not identity" arriving in a
	// third place: an option index is not an option.
	Nonce string `json:"nonce"`
}
