package compat

// Spec describes one check: what it asserts and which driver code depends on
// it. It is the single source for three things that must not drift apart —
// the "relied on by" suffix in every report detail, the additive reliedOn
// array in the report, and the catalogue table in docs/compat.md. Tests hold
// the three together.
type Spec struct {
	// ID is stable once shipped. Renaming or removing one is a schema bump.
	ID   string
	Gate Gate
	// Asserts says, in one sentence, what the check asserts about the
	// candidate.
	Asserts string
	// ReliedOn lists the driver code that depends on the behaviour, as
	// "<repo-relative file>#<top-level identifier>". A test resolves each
	// entry against the source, so a rename fails the build.
	ReliedOn []string
}

// catalogue is in report order. Add rows together with the check that
// evaluates them and the matching row in docs/compat.md.
var catalogue = []Spec{
	{
		ID:       "F-LIMIT",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the usage-limit notice wording the screen classifier recognises. Static text only: the screen cannot be produced on demand.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#usageLimit"},
	},
	{
		ID:       "F-APIERR",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the API-error wording the classifier reads to tell a failed turn from a finished one. Static text only: the screen cannot be produced on demand.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#lastTurnFailed"},
	},
	{
		ID:       "H-RC",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the four remote-control footer labels the control-channel reader maps. Static text only: a check never attaches a bridge, and the wording being present does not prove where the runtime draws it.",
		ReliedOn: []string{"internal/drivers/tmux/controlchannel.go#controlStates"},
	},
	{
		ID:       "H-RCREC",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the wording of the entries the control-channel reader takes from the runtime's own record: the one written when the channel comes up, the one written when it is disconnected, and the record's subtype for the first. Static text only: a check never attaches a bridge, and the wording being present does not prove where the runtime draws a label.",
		ReliedOn: []string{"internal/drivers/tmux/controlchannelrecord.go#classifyControlRecordLine"},
	},
	{
		ID:       "F-FEEDBACK",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the wording of the feedback-draft card's states the driver recognises: the send confirmation, the send error, the question about turning drafts off, the queued count and the /feedback panel's title. Static text only: no probe can make an agent draft feedback.",
		ReliedOn: []string{"internal/drivers/tmux/feedbackcard.go#parseFeedbackStatus"},
	},
	{
		ID:       "F-MODE",
		Gate:     GateWarn,
		Asserts:  "The driver reads the permission mode a live session shows: a session started in the default mode reads as default and one started in bypass-permissions mode reads as bypass, and the candidate still contains the wording of the accept-edits, plan and auto indicator rows. The other three modes cannot be entered without pressing keys in a session other checks share, so their wording is checked statically.",
		ReliedOn: []string{"internal/drivers/tmux/permissionmode.go#permissionModeOf"},
	},
	{
		ID:       "C1",
		Gate:     GateMust,
		Asserts:  "A working directory the driver seeds as trusted starts without the folder-trust dialog, and without the external-imports dialog although its instruction file imports a file from outside it, so a session created there reaches its composer on its own.",
		ReliedOn: []string{"internal/trustseed/trustseed.go#Seeder", "internal/trustseed/trustseed.go#seededKeys"},
	},
	{
		ID:       "F-TRUST",
		Gate:     GateMust,
		Asserts:  "A directory outside the trust root shows the folder-trust dialog, which the driver classifies as such, reads as an unnumbered menu, and can find exactly one affirmative option in. Observed only: the dialog is never answered.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#classifyPromptKind", "internal/drivers/tmux/tmux.go#affirmativeOption"},
	},
	{
		ID:       "F-IMPORTS",
		Gate:     GateMust,
		Asserts:  "A directory that is trusted but was never approved to import from outside itself, and whose instruction file does, shows the external-imports dialog, which the driver classifies as such, reads as an unnumbered menu with the decline highlighted, and can find exactly one affirmative option in. Observed only: the dialog is never answered.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#classifyPromptKind", "internal/drivers/tmux/tmux.go#consentableKinds", "internal/drivers/tmux/tmux.go#affirmativeOption"},
	},
	{
		ID:       "B5",
		Gate:     GateMust,
		Asserts:  "A session started in bypass-permissions mode reaches its composer with no acceptance screen in the way, given the user setting that suppresses it.",
		ReliedOn: []string{"internal/drivers/tmux/tmux.go#claudeCodeCommand"},
	},
	{
		ID:       "F-BYPASS",
		Gate:     GateWarn,
		Asserts:  "The bypass-acceptance screen, when it can be produced, is classified as such; otherwise the wording of its two options is still present in the candidate. Observed only: it is never answered.",
		ReliedOn: []string{"internal/drivers/tmux/tmux.go#acceptanceScreen"},
	},
	{
		ID:       "D1",
		Gate:     GateMust,
		Asserts:  "The runtime's per-process session record appears within fifteen seconds of launch carrying the fields this service reads, with the expected types and values.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#processSessionRecord"},
	},
	{
		ID:       "D3",
		Gate:     GateMust,
		Asserts:  "The record's process start time (UTC text, or on Linux clock ticks since boot) corroborates the running process, so the record can be trusted to belong to that process and not to a recycled pid.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#parseProcessSessionRecordStartTime"},
	},
	{
		ID:       "D4",
		Gate:     GateMust,
		Asserts:  "With remote control off, the record carries no bridge id and the screen shows no control-channel label. The negative half only: the positive half needs a bridge, which a check never creates.",
		ReliedOn: []string{"internal/drivers/tmux/controlchannel.go#controlChannelOf"},
	},
	{
		ID:       "F-COMPOSER",
		Gate:     GateMust,
		Asserts:  "The composer is the prompt glyph between two rules, an empty composer reads as empty even when a dim placeholder is painted in it, and a typed draft reads back exactly.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#composerText"},
	},
	{
		ID:       "F-MLDRAFT",
		Gate:     GateMust,
		Asserts:  "A multi-line draft pasted into the composer reads back as the text that was pasted.",
		ReliedOn: []string{"internal/drivers/tmux/composertext.go#composerMatchesText"},
	},
	{
		ID:       "F-PASTEMARK",
		Gate:     GateMust,
		Asserts:  "A long multi-line paste collapses to a [Pasted text #N +M lines] marker and one long line to a bare [Pasted text #N] marker, and both are counted the way delivery confirmation counts them.",
		ReliedOn: []string{"internal/drivers/tmux/tmux.go#markerCounts", "internal/drivers/tmux/tmux.go#composerHoldsCollapsedPaste"},
	},
	{
		ID:       "F-WRAP",
		Gate:     GateMust,
		Asserts:  "A draft longer than a row wraps onto rows of one width, and the wrapped rows read back as the text that was pasted.",
		ReliedOn: []string{"internal/drivers/tmux/composertext.go#composerRegion"},
	},
	{
		ID:       "G6",
		Gate:     GateMust,
		Asserts:  "The prompt-mode characters behave as the input guard assumes: a leading ! in an empty composer enters shell mode, while the same text after a space, and a slash after a space, stay plain prompt text.",
		ReliedOn: []string{"internal/drivers/tmux/inputguard.go#refuseAsRuntimeSyntax"},
	},
	{
		ID:       "G1",
		Gate:     GateMust,
		Asserts:  "A send lands in an idle session and is confirmed as its own user turn by the runtime's transcript, exactly once, for short, long, multi-line and control-byte text, and the session comes back to an empty composer.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#transcriptTailScan"},
	},
	{
		ID:       "G2",
		Gate:     GateMust,
		Asserts:  "Bracketed paste is on at the composer, and a long single line, a long multi-line paste and text with control bytes each arrive whole and exactly once, with the control bytes removed as the sanitiser assumes.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2.go#sanitizeForBracketedPaste"},
	},
	{
		ID:       "E-USER",
		Gate:     GateMust,
		Asserts:  "The user turn the runtime records for a send carries the fields delivery confirmation reads: message content, a human origin, a typed or queued prompt source, and no meta, sidechain or summary marking.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#extractTranscriptCandidate"},
	},
	{
		ID:       "E-PASTE",
		Gate:     GateMust,
		Asserts:  "A long or collapsed paste is recorded as the real text, wrapped or not, and unwraps to exactly the text that was sent; a marker alone is not enough.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#normalizeTranscriptText"},
	},
	{
		ID:       "E-NAME",
		Gate:     GateMust,
		Asserts:  "The transcript is a file named for the session id whose custom-title is the session's name, within the lines the driver reads.",
		ReliedOn: []string{"internal/drivers/tmux/conversation.go#readRecordEntry"},
	},
	{
		ID:       "E-SLUG",
		Gate:     GateMust,
		Asserts:  "A working directory containing a dot, an underscore, a space and a non-ASCII letter is recorded in the directory the driver derives by replacing every non-alphanumeric character.",
		ReliedOn: []string{"internal/drivers/tmux/conversation.go#recordDirFor"},
	},
	{
		ID:       "B2",
		Gate:     GateMust,
		Asserts:  "The -n value names the per-process record and the transcript's title, and the driver joins the session to its conversation by that name, with remote control off.",
		ReliedOn: []string{"internal/drivers/tmux/conversation.go#conversationStore"},
	},
	{
		ID:       "B7a",
		Gate:     GateMust,
		Asserts:  "A system-prompt file passed with --append-system-prompt-file is honoured: an instruction in it shows in the reply.",
		ReliedOn: []string{"internal/drivers/tmux/tmux.go#claudeCodeCommand"},
	},
	{
		ID:       "D2",
		Gate:     GateWarn,
		Asserts:  "The record's status moves off idle while a turn runs and back, with statusUpdatedAt advancing at each change. Nothing in this driver reads it yet, so this is warn-only: it is the record's own liveness signal.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#processSessionRecord"},
	},
	{
		ID:       "H-RENAME",
		Gate:     GateWarn,
		Asserts:  "Whether, and after how long, a programmatic rename — the driver's own Rename followed by its own SyncTitle, delivered via the composer exactly as the service delivers it — is followed by a custom-title transcript entry naming the session's new id. SyncTitle already degrades honestly to pending when this has not happened within its own four-second window; this is warn-only because that degradation, not this finding, is what the driver relies on. It reads for far longer than that window so a runtime that is merely slow to write the entry is not reported the same as one that never does.",
		ReliedOn: []string{"internal/drivers/tmux/titlesync.go#transcriptTitleScan"},
	},
}

// Catalogue returns a copy of the catalogue, in report order.
func Catalogue() []Spec {
	out := make([]Spec, len(catalogue))
	for i, s := range catalogue {
		s.ReliedOn = append([]string(nil), s.ReliedOn...)
		out[i] = s
	}
	return out
}

// Lookup returns the catalogue entry for id.
func Lookup(id string) (Spec, bool) {
	for _, s := range catalogue {
		if s.ID == id {
			s.ReliedOn = append([]string(nil), s.ReliedOn...)
			return s, true
		}
	}
	return Spec{}, false
}
