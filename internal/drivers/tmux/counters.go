package tmux

import (
	"sync"
	"time"
)

// counterSet is a tiny, generalisable named-counter registry.
//
// It exists because of a distinction #44 drew explicitly: a retry that
// silently clears a strand still needs to be COUNTED, or the rate — the
// actual signal, per #44's measurement of three consecutive strands on one
// busy machine — is laundered away the moment it stops being a failure. A
// one-off `log.Printf` at the call site would have recorded that this
// happened; it would not have given anything a number to read later, and
// #44 asks for a counter, not a line that scrolls off.
//
// This is deliberately the smallest thing that satisfies that: a name and a
// count, nothing wired to an HTTP surface yet. #9 ("the service cannot see
// itself") already describes the larger shape this is one piece of, and
// names its own reason for staying unscheduled — no corpus yet to compare a
// surface against. Building that surface here, for one caller's two
// counters, would be answering a question #9 has not asked yet. What #44
// owes #9 is a registry the next fact can join without a new struct field
// and a new accessor — only a new name — and that is what this is.
type counterSet struct {
	mu sync.Mutex
	n  map[string]int64
}

func (c *counterSet) incr(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int64{}
	}
	c.n[name]++
}

// Snapshot returns a copy safe for a caller to read without racing further
// increments. Exported on the struct (unlike incr) because the day
// something reads this — a test, a health endpoint, a future #9 surface —
// it should not have to move packages to reach it.
func (c *counterSet) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

// Names a future reader looks for. Kept beside the registry rather than
// scattered at each call site — the same discipline WaitingReason's const
// block applies to the strings a caller might branch on.
const (
	// counterInitialPromptRetried counts every time §2.1's initial-prompt
	// delivery needed the one in-window retry #44 added, whether or not
	// that retry went on to succeed. Incrementing it on a successful retry
	// too is the point: the count is what says a busy machine is racing
	// this keystroke, and a retry that quietly fixes it every time would
	// hide exactly that.
	counterInitialPromptRetried = "initial_prompt.delivery_retried"
	// counterInitialPromptStranded counts every time that retry was not
	// enough and the prompt was still unsent when deliverInitialPrompt gave
	// up on it — the case that used to reach nobody at all.
	counterInitialPromptStranded = "initial_prompt.delivery_stranded"
	// counterInitialPromptLaneUnconfirmed counts every time the session's
	// delivery lane wrote the create-time prompt and could not confirm it (#368).
	// There is no retry for it, so it is neither retried nor stranded; the
	// prompt is resolved unknown with the lane's own reason.
	counterInitialPromptLaneUnconfirmed = "initial_prompt.delivery_lane_unconfirmed"
	// counterInitialPromptSessionGone counts every time settleNewSession gave
	// up because the session itself was confirmed gone (muster #125),
	// never because a timer expired — there is no timer any more. A nonzero
	// rate here says sessions are dying before their initial prompt lands,
	// which is a different signal from delivery_stranded and worth telling
	// apart from it.
	counterInitialPromptSessionGone = "initial_prompt.delivery_session_gone"

	// muster #104: confirmSubmitted's own doc comment already names two
	// INDEPENDENT confirming signals — the composer reading fully empty, or
	// this delivery's own attributed marker count falling below what it was
	// pasted at — added in that order because the second one was found
	// necessary AFTER the first shipped (residue on the composer line can
	// keep it non-empty forever). #104's suspicion is that this history could
	// repeat in the other direction: nothing proves both branches still fire
	// in practice, so "confirmation rests on two signals" could quietly be
	// "confirmation rests on one, and the other is dead code nobody removed."
	// A live capture can't answer that — an agent session is refused from
	// driving the multiplexer directly, by design (#104's own text) — but
	// which branch decided a given confirmation is knowable at the moment it
	// is decided, inside this driver's own call. Counting it here turns an
	// unobservable question into a rate anyone can read afterwards: if
	// counterSubmitConfirmedByMarkerCleared stays at zero across real
	// traffic, that is the dead branch #104 suspects, found without a single
	// capture — the
	// same idiom #116 used for counterIdentityContested's sibling question.
	counterSubmitConfirmedByComposerEmpty = "submit_confirm.by_composer_empty"
	counterSubmitConfirmedByMarkerCleared = "submit_confirm.by_marker_cleared"
	// counterSubmitConfirmTimeout counts a confirmSubmitted call that never
	// saw either signal inside submitConfirmWindow. This is not new
	// information — the caller already turns this into an `unknown` receipt
	// and a stranded record — but it gives the timeout itself a rate,
	// independent of what the caller went on to do with it.
	counterSubmitConfirmTimeout = "submit_confirm.timeout"

	// #104's second question: submitConfirmWindow (4s) was inherited, never
	// derived from how long a real submit actually takes to show. A live
	// capture would answer that by watching one pane; this answers it the
	// same way as the branch question above — by having the service notice,
	// on every call it already makes, how long confirmation actually took.
	// Bucketed rather than summed/averaged: this repo takes no third-party
	// dependency (no histogram library), and a mean would hide exactly the
	// tail this question is about — a window near the tail bucket is a
	// budget worth revisiting even if the mean looks comfortable. Buckets
	// are exclusive (each confirmation lands in exactly one), the top one
	// capped at submitConfirmWindow itself since nothing here confirms past
	// it — a call that reaches the window without either signal firing is
	// counterSubmitConfirmTimeout instead, not a "latency" of any length.
	counterSubmitConfirmLatencyUnder250ms = "submit_confirm.latency_under_250ms"
	counterSubmitConfirmLatencyUnder500ms = "submit_confirm.latency_under_500ms"
	counterSubmitConfirmLatencyUnder1s    = "submit_confirm.latency_under_1s"
	counterSubmitConfirmLatencyUnder2s    = "submit_confirm.latency_under_2s"
	counterSubmitConfirmLatencyUnder4s    = "submit_confirm.latency_under_4s"

	// muster#156: the batched capture's TIME wall. chunk_failed counts
	// every capture chunk that came back with nothing parseable, whatever
	// the cause and whether or not a retry then recovered it. The rate of
	// failures is the signal, and a retry that quietly fixes each one would
	// hide it (same argument as counterInitialPromptRetried). retry_recovered
	// and retry_failed split the retries that were attempted, and their ratio
	// answers whether retrying is worth doing. A stall across the whole server
	// shows up as retry_failed, and more retrying will not fix that.
	// slow_invocation counts every multiplexer invocation, listing included,
	// that SUCCEEDED but took longer than slowInvocationLine. It gives a wall
	// time distribution before the next failure, which the failure line
	// alone cannot.
	counterCaptureChunkFailed      = "capture.chunk_failed"
	counterCaptureRetryRecovered   = "capture.retry_recovered"
	counterCaptureRetryFailed      = "capture.retry_failed"
	counterEnumerateSlowInvocation = "enumerate.slow_invocation"

	// muster#150: the inbox send path's exits. sendViaInbox reports
	// every fallback as the same ok=false, and a fallback costs only
	// latency, so nothing outside this driver can tell the reasons apart —
	// the same laundering #44 names. attempted counts every call that had a
	// resolver to try; each such call then ends in exactly one of the ten
	// exits below it, so attempted equals their sum and a health read checks
	// itself. A machine with no inbox configured counts nothing at all: a
	// zero there would claim a measurement that was never taken.
	//
	// written, not delivered: nothing observes delivery on this path (#144).
	counterInboxAttempted                  = "inbox.attempted"
	counterInboxFallbackIdentityUnresolved = "inbox.fallback_identity_unresolved"
	counterInboxErrorIdentityResolve       = "inbox.error_identity_resolve"
	counterInboxFallbackResolverError      = "inbox.fallback_resolver_error"
	counterInboxFallbackResolverDeclined   = "inbox.fallback_resolver_declined"
	counterInboxRefusedIdentityUnverified  = "inbox.refused_identity_unverified"
	counterInboxFallbackNoModeClass        = "inbox.fallback_no_mode_class"
	counterInboxFallbackBodyUnattestable   = "inbox.fallback_body_unattestable"
	counterInboxFallbackDialFailed         = "inbox.fallback_dial_failed"
	counterInboxFallbackWriteFailed        = "inbox.fallback_write_failed"
	counterInboxWritten                    = "inbox.written"

	// muster#184: two more exits, and what "written" now splits into.
	//
	// no_transcript: the inbox needs the receiver's transcript to CONFIRM a
	// write, so a session whose transcript this driver cannot locate cannot
	// have an inbox delivery it can stand behind — the same "half a capability
	// is none of it" rule as no_mode_class (ADR 119, ADR 148). A fallback, taken
	// before anything is dialled.
	//
	// unknown_partial_write: a write that put SOME bytes on the socket and then
	// failed. Unlike write_failed (which now means ZERO bytes) this is never a
	// fallback: the receiver may have been given a message, and the same text is
	// not sent down the other path. It ends `unknown`.
	//
	// written is still an exit — a complete write — and is now split into
	// confirmed and unconfirmed by whether the receiver's own transcript
	// recorded the message. written = confirmed + unconfirmed, so a read checks
	// itself. unconfirmed / written is the rate an operator watches: it is what
	// a silently-held message (#148) looks like now that there is something to
	// confirm against. confirmed_by_envelope / confirmed_by_origin_body say
	// which transcript shape the runtime actually writes for a peer message,
	// which no offline test can tell.
	counterInboxFallbackNoTranscript  = "inbox.fallback_no_transcript"
	counterInboxUnknownPartialWrite   = "inbox.unknown_partial_write"
	counterInboxConfirmed             = "inbox.confirmed"
	counterInboxUnconfirmed           = "inbox.unconfirmed"
	counterInboxConfirmedByEnvelope   = "inbox.confirmed_by_envelope"
	counterInboxConfirmedByOriginBody = "inbox.confirmed_by_origin_body"
	counterInboxLedgerEntryWritten    = "inbox.unconfirmed_ledger_written"

	// muster#184: one family per Send, whatever path it took, so an
	// operator can split deliveries by route. Every Send is counted exactly
	// once in route.decided.<requested>.<taken>, where <taken> is the path the
	// receipt names (inbox | terminal) or "none" when the receipt names no path
	// (a refusal made before any path was chosen) or "error" when Send returned
	// an error. route.outcome.<path>.<outcome> is the same delivery by outcome.
	//
	// (#257: the auto-to-inbox fallback, and with it route.auto_fallback, is
	// retired: the inbox is tried only when a caller names it.)
	//
	// refused.lane_live (#257) counts a /input refused because the session's
	// delivery lane is live and the call asked for a path that would bypass it
	// (an explicit terminal route, submit:false, resumeIfStranded,
	// replaceIfStranded) or because the lane holds text this driver stranded.
	// refused.lane_live.<field> splits it by what was asked for (terminal,
	// submit_false, resume, replace, stranded_text); a call asking for several
	// things counts once in the total and once per field. A deploy watches it
	// for callers still asking for the terminal. refused.lane_lost counts a send
	// whose lane read live at the gate and stopped being usable before a byte was
	// written: refused, not sent to the terminal.
	//
	// The guard counters are the cross-path ledger's (docs/adr/184-route-by-sender.md):
	// inbox_unconfirmed — a follow-up was answered from an earlier unconfirmed
	// inbox write instead of being sent again; terminal_unconfirmed — the inbox
	// was skipped (auto) or refused (route inbox) because the same text is
	// stranded in the composer; late_confirmed — a follow-up found the earlier
	// inbox write recorded after all.
	counterRouteRefusedLaneLive        = "route.refused.lane_live"
	counterRouteRefusedLaneLivePrefix  = "route.refused.lane_live."
	counterRouteRefusedLaneLost        = "route.refused.lane_lost"
	counterRouteGuardInboxUnconfirmed  = "route.guard.inbox_unconfirmed"
	counterRouteGuardTerminalUnconfirm = "route.guard.terminal_unconfirmed"
	counterRouteGuardLateConfirmed     = "route.guard.late_confirmed"
	counterModuleLedgerEntryWritten    = "module.unconfirmed_ledger_written"

	// The number #150's decision reads. attest_checked counts every call
	// that reached attestation; attest_body_lookalike counts those whose
	// body the current rule refuses, WHETHER OR NOT the class was also
	// missing. Attest checks the class first, so while an index still omits
	// classes a body refusal hides behind the class one, and
	// fallback_body_unattestable alone would read as a clean rate that is
	// only a masked one. See docs/adr/150-count-inbox-fallbacks-before-widening.md.
	counterInboxAttestChecked       = "inbox.attest_checked"
	counterInboxAttestBodyLookalike = "inbox.attest_body_lookalike"

	// muster#149: every refusal of a composer taller than the capture
	// window (#134's composerClipped), one name per verb. ADR 149 found no
	// evidence inside this driver that makes acting on such a composer safe,
	// so these refusals are permanent for as long as the state lasts. What
	// the ADR can still use is a rate: how often real traffic reaches this
	// state, and through which verb. That is the measurement its reopen
	// condition names. Counted at the refusal, not at the classification: a
	// read that sees a clipped composer and does nothing destroys nothing.
	counterComposerClippedRefusedDiscard = "composer_clipped.refused_discard"
	counterComposerClippedRefusedKeys    = "composer_clipped.refused_keys"
	counterComposerClippedRefusedSend    = "composer_clipped.refused_send"
	// muster#215: refusals that named the runtime's feedback-draft card
	// instead of the generic reasons they used to fall into (no composer
	// painted, selection menu), one name per verb, and the lone-digit message
	// the card would have read as its own shortcut. Counted at the refusal.
	counterFeedbackCardRefusedSend    = "feedback_card.refused_send"
	counterFeedbackCardRefusedKeys    = "feedback_card.refused_keys"
	counterFeedbackCardRefusedDiscard = "feedback_card.refused_discard"
	counterFeedbackCardRefusedDigit   = "feedback_card.refused_lone_digit"
	// muster#169: the subset of those refusals that happened ONLY
	// because the composer's opening fence sat above the visible pane, in the
	// history margin — the same rows without the pane boundary would have
	// read as a found composer. Incremented alongside the per-verb counter,
	// never instead of it. #169 asked that the rule not be adopted before
	// checking it leaves ordinary tall composers readable; this is that check
	// kept running, since a snapshot of one fleet cannot speak for every
	// runtime. See docs/adr/169-a-composer-is-read-from-the-visible-pane.md.
	counterComposerClippedAboveVisiblePane = "composer_clipped.fence_above_visible_pane"
	// muster#216: the subset of those refusals that happened because the
	// composer's closing rule was cut off by the bottom of the pane — a prompt
	// row on the pane's last row, opened by a rule. This shape read as absent
	// before, so it was refused elsewhere or not at all; the rate at which real
	// traffic now reaches the clipped refusals through this rule is this counter.
	// Incremented alongside the per-verb counter, never instead of it.
	counterComposerClippedBottomCut = "composer_clipped.bottom_cut"

	// Terminal path v2 (muster round-1 research, item f). Two families:
	//
	// land_confirm.* names which signal confirmLandedV2 (terminalpath2.go)
	// used to decide text had rendered — by_composer_match for the new
	// structural, wrap-tolerant composer-region comparison (D1's fix),
	// by_marker for the pre-existing collapsed-paste marker attribution
	// (unchanged from confirmLanded, kept for multi-line pastes the TUI
	// summarises rather than echoes), timeout for neither ever firing inside
	// submitConfirmWindow. Exactly one fires per confirmLandedV2 call that
	// returns landed=true; by_marker and by_composer_match are mutually
	// exclusive within one call because the loop returns on the first hit.
	//
	// submit_confirm.by_transcript is the new signal alongside the two
	// confirmSubmitted already had (by_composer_empty, by_marker_cleared,
	// counted under the SAME counterSubmitConfirmTimeout on a miss) — see
	// terminalpath2_transcript.go's confirmSubmittedV2 for what "by
	// transcript" means and why it is tried FIRST.
	counterLandConfirmByComposerMatch = "land_confirm.by_composer_match"
	counterLandConfirmByMarker        = "land_confirm.by_marker"
	counterLandConfirmTimeout         = "land_confirm.timeout"
	// counterLandConfirmByLegacyNeedle counts confirmLandedV2 falling back
	// to confirmLanded's own pre-existing tail-needle substring match — see
	// confirmLandedV2's own doc comment (terminalpath2.go) for why this
	// fallback exists at all and what a nonzero live rate for it would mean.
	counterLandConfirmByLegacyNeedle = "land_confirm.by_legacy_needle"
	// counterLandConfirmByClippedTail: a fresh send's composer grew past the
	// visible top and its visible rows rendered a tail of the text (#149,
	// decided by #180). counterLandConfirmByOwnMarker: a resume finished the
	// driver's own collapsed paste by its recorded marker (#180 H1).
	counterLandConfirmByClippedTail = "land_confirm.by_clipped_tail"
	counterLandConfirmByOwnMarker   = "land_confirm.by_own_marker"
	// counterLandConfirmPreempted (#180 M4): a send gave up this session's
	// lock before Enter because a respond was waiting for it.
	counterLandConfirmPreempted = "land_confirm.preempted"

	counterSubmitConfirmedByTranscript = "submit_confirm.by_transcript"

	// #240: a submit confirmed by evidence that does not show a turn started,
	// and what became of the text afterwards. by_enqueue counts a transcript
	// confirmation that was only the runtime queueing the text;
	// provisional_kept counts a confirmation (of either weak kind: enqueue, or
	// the screen) that left a provisional record behind;
	// provisional_handed_back counts the composer later holding that text again
	// with the provisional record as the only proof it is the driver's own;
	// unexplained_labelled_composer counts an unsent composer that opens with a
	// sender label this driver writes and that nothing it remembers explains —
	// diagnostic only, never proof, and a nonzero rate after #240 means a hop
	// other than the one #240 closed.
	counterSubmitConfirmedByEnqueue         = "submit_confirm.by_enqueue"
	counterStrandedProvisionalKept          = "stranded.provisional_kept"
	counterStrandedProvisionalHandedBack    = "stranded.provisional_handed_back"
	counterStrandedUnexplainedLabelledInput = "stranded.unexplained_labelled_composer"

	// counterBracketPasteFlagUnknown/Off count pasteBracketed's two refusal
	// reasons separately (terminalpath2.go) — the query itself failing is a
	// different fact from the query answering "no", and round-1 measured
	// only the "yes" case live (see pasteBracketed's own doc comment), so
	// telling these apart is what would show whether the "no" branch is
	// ever actually exercised in the field.
	counterBracketPasteFlagUnknown = "bracket_paste.flag_unknown"
	counterBracketPasteFlagOff     = "bracket_paste.flag_off"
	// counterBracketPasteFlagUnsupported counts the third refusal: the query
	// was answered with an empty string, which is what a tmux older than 3.7
	// prints for a format variable it does not have (#333). It is kept apart
	// from flag_off because the two want different fixes — a pane that
	// declined bracketed paste is the runtime's doing, an unsupported variable
	// is the multiplexer's.
	counterBracketPasteFlagUnsupported = "bracket_paste.flag_unsupported"
	// counterBracketPasteUnreadablePre37 counts a paste sent on a multiplexer that
	// reports the release it is and is older than 3.7 (#334): the flag cannot be
	// read there, so the paste went out as `paste-buffer -p` with no pre-check and
	// the multiplexer alone decided whether to bracket it. A rate here is how many
	// deliveries rest on that rather than on a flag read.
	counterBracketPasteUnreadablePre37 = "bracket_paste.unreadable_pre37"

	// counterComposerLockContended counts every composer-touching call
	// (Send, Respond, Discard, Keys — terminalpath2_lock.go) that had to
	// wait for another call on the SAME session id to release the per-
	// session lock before it could start. Zero across real traffic would
	// say the race D4 named (two concurrent /input calls merged into one
	// user turn) was never actually being hit in the field, which is worth
	// knowing independently of having fixed it.
	counterComposerLockContended = "composer_lock.contended"
	// counterComposerLockDeadlineExceeded counts a composer-touching call
	// whose OWN caller deadline ran out while waiting for another call on
	// the same session id to release the lock — the review fix for D4
	// ignoring the caller's context entirely. Distinct from
	// counterComposerLockContended, which fires on every wait regardless of
	// how it ends; this one is the subset that timed out rather than
	// eventually acquiring.
	counterComposerLockDeadlineExceeded = "composer_lock.deadline_exceeded"

	// counterTranscriptCacheStaleAfterClear counts resolveTranscriptSource
	// discovering that its own cached record-root lookup (conversationStore,
	// keyed on pane+created for the life of the daemon) names a DIFFERENT
	// conversation id than the runtime's own live per-process identity file
	// reports right now — the signature of Claude Code regenerating its
	// session id under an unchanged tmux pane (a `/clear`, or an in-pane
	// runtime restart) without this driver's cache ever being told. A
	// nonzero live rate says this actually happens on real sessions, not
	// only in the review's synthetic reproduction.
	counterTranscriptCacheStaleAfterClear = "transcript_source.cache_stale_after_clear"

	// counterTranscriptScannerUnreadable counts transcriptTailMatches'
	// bufio.Scanner failing outright (not merely reaching EOF with no match)
	// — e.g. one transcript line exceeding recordLineLimit. The review's own
	// finding: this used to be silently folded into "no match", which is
	// indistinguishable from a transcript that was read in full and simply
	// disagreed. Both degrade to the same caller-visible outcome (this
	// signal cannot confirm the send), but only one of them is this driver
	// admitting it could not read what it was looking at.
	counterTranscriptScannerUnreadable = "transcript_source.scanner_unreadable"

	// counterSubmitConfirmedByScreenAfterSilentTranscript counts
	// confirmSubmittedFromSource falling back to the screen-based signal
	// (composer emptying, or this delivery's own marker clearing) AFTER a
	// resolved transcript's own polling window found nothing — the review
	// fix for the busy-session false "unknown": the exact queue-operation
	// shape this driver parses is not independently verified against a real
	// transcript (see terminalpath2_transcript.go's own top comment), so a
	// resolved-but-silent transcript is now treated as "this signal has
	// nothing to say", not as "the runtime never accepted this delivery" —
	// the screen gets a chance to say otherwise before the caller is told
	// unknown. A nonzero live rate is this driver's own admission that its
	// transcript parsing missed a real acceptance; see also
	// counterSubmitConfirmedByTranscript, which fires instead whenever the
	// transcript itself was the one that confirmed it.
	counterSubmitConfirmedByScreenAfterSilentTranscript = "submit_confirm.by_screen_after_silent_transcript"

	// counterSendRefusedDialogRace counts confirmLandedV2 declining to treat
	// a composer or marker match as "landed" because the SAME capture that
	// produced the match also shows a selection menu on screen — the review
	// fix for "pressing Enter after the landed check can approve a dialog":
	// a modal appearing in the gap between this driver's last look and the
	// submit keystroke must not have that keystroke land on "1. Yes"
	// instead of on the message this driver meant to submit.
	counterSendRefusedDialogRace = "land_confirm.refused_dialog_race"

	// counterSendRefusedDialogRacePreSubmit is counterSendRefusedDialogRace's
	// sibling for the SECOND #180 review finding of the same shape: a
	// selection menu appearing not while confirmLandedV2 was polling, but in
	// the extra window resolveTranscriptSource itself opens between the
	// landed check returning and the submit keystroke — list-panes plus a
	// batched capture, then `ps`, then file reads, measured 26.8-52.9ms on a
	// private 25-pane tmux server. The fresh, cheap re-capture this counts is
	// taken immediately before send-keys at both submit sites (Send's
	// first-attempt path and its resumeIfStranded completion), specifically
	// to close that gap.
	counterSendRefusedDialogRacePreSubmit = "land_confirm.refused_dialog_race_presubmit"
	// counterSendRefusedNotHeldPreSubmit (#180 M6): the last look before
	// Enter did not positively show a composer holding this delivery.
	counterSendRefusedNotHeldPreSubmit = "land_confirm.refused_not_held_presubmit"

	// foreground.* (#180 H2): who was in the pane's foreground when this
	// driver was about to paste or press Enter (foreground.go). verified: a
	// foreground process's own session record matched; unverified: nothing
	// contradicted the runtime but nothing proved it either; shell: refused,
	// a shell was in the foreground; unanswered: refused, the multiplexer
	// could not say.
	counterForegroundVerified   = "foreground.verified"
	counterForegroundUnverified = "foreground.unverified"
	counterForegroundShell      = "foreground.refused_shell"
	counterForegroundUnanswered = "foreground.refused_unanswered"

	// counterAgentUnread (#301): a listing's one process-table read failed, so
	// no session in it could report the agent its runtime was started with.
	// The listing's source is marked degraded for the same read.
	counterAgentUnread = "agent.process_table_unread"

	// counterProcessSessionRecordRejected (#180 L5): a runtime per-process
	// record whose session id is not UUID-shaped, refused before it could
	// become part of a path.
	counterProcessSessionRecordRejected = "transcript_source.session_record_rejected"

	// counterSendRefusedResumeNoRecord counts resumeIfStranded (without
	// replaceIfStranded) landing on a busy composer this driver holds no
	// stranded record for — #180 review's own fix: #135 originally treated
	// this the same as replaceIfStranded (clear whatever is there and
	// deliver), which is safe when the caller explicitly means "discard it"
	// but not when the caller only means "finish MY earlier delivery" and the
	// record for it was forgotten out from under them (Discard's own
	// composerFound-empty forget, or a confirmed send elsewhere) — in which
	// case #135's door silently submitted a person's own draft as this
	// call's text. resumeIfStranded now refuses here instead of clearing.
	counterSendRefusedResumeNoRecord = "send.refused_resume_no_record"

	// counterResumeConfirmedByTranscriptOnEmptyComposer counts
	// resumeIfStranded finding the composer ALREADY EMPTY (not busy) for a
	// session with a matching stranded record, and confirming — from that
	// record's own TranscriptPath/TranscriptOffset, resolved at strand time —
	// that the runtime had already accepted the text, rather than silently
	// falling through to the ordinary fresh-paste path and delivering a
	// byte-for-byte duplicate (#180 review's own finding).
	counterResumeConfirmedByTranscriptOnEmptyComposer = "resume.confirmed_by_transcript_on_empty_composer"

	// counterResumeRefusedEmptyComposerUnconfirmed is
	// counterResumeConfirmedByTranscriptOnEmptyComposer's negative sibling:
	// the composer read empty for a matching stranded record, but this
	// driver's own transcript record could not confirm the runtime accepted
	// it either (no transcript resolved at strand time, or it resolved but
	// disagreed/stayed silent). Per the same review fix, this refuses rather
	// than silently pasting the same text again.
	counterResumeRefusedEmptyComposerUnconfirmed = "resume.refused_empty_composer_unconfirmed"
	// counterResumeRepastedNeverRendered (#180 M1): a resume found the
	// composer empty for a delivery that never rendered and never had Enter
	// pressed, the transcript showed nothing, and it was pasted again.
	counterResumeRepastedNeverRendered = "resume.repasted_never_rendered"

	// counterTranscriptDifferentTurnRecorded counts
	// confirmSubmittedFromSource finding transcriptScanDifferentTurn — a
	// candidate turn attributable to this delivery's own confirmation window
	// that did NOT match the sent text — #180 review's own fix for
	// "a transcript that records a different turn is treated as silence, and
	// the screen fallback reports queued".
	counterTranscriptDifferentTurnRecorded = "transcript_source.different_turn_recorded"

	// muster #222: SyncTitle's own outcomes, one exclusive counter per
	// fleet.TitleSyncStatus a tmux SyncTitle call can actually produce (never
	// title_sync.not_applicable — that value is the SERVICE's, for a driver
	// that does not implement TitleSyncer at all, so this driver never
	// increments it).
	counterTitleSyncSynced  = "title_sync.synced"
	counterTitleSyncPending = "title_sync.pending"
	counterTitleSyncFailed  = "title_sync.failed"
	// counterTitleSyncCommandWithoutTitle counts a `pending` specifically
	// where the runtime's own local_command/<command-name> entry confirmed
	// the /rename command ran (muster #187) but no custom-title entry
	// followed it within the confirmation window — the live rate of the one
	// fact this repo has NOT measured about the runtime (#222's own plan:
	// whether/when /rename writes a fresh custom-title at all).
	counterTitleSyncCommandWithoutTitle = "title_sync.command_without_title"
	// counterTitleSyncRecordedOtherTitle counts a `failed` specifically
	// where a custom-title entry DID appear after this delivery's offset,
	// but named a different title than the one this delivery asked for —
	// distinct from every other failed cause, which never got that far.
	counterTitleSyncRecordedOtherTitle = "title_sync.recorded_other_title"
)

// confirmLatencyBucket maps an observed confirm latency onto one of the five
// exclusive buckets declared above. See their doc comment for why buckets,
// and why the top one is capped at submitConfirmWindow rather than open-ended.
func confirmLatencyBucket(elapsed time.Duration) string {
	switch {
	case elapsed < 250*time.Millisecond:
		return counterSubmitConfirmLatencyUnder250ms
	case elapsed < 500*time.Millisecond:
		return counterSubmitConfirmLatencyUnder500ms
	case elapsed < time.Second:
		return counterSubmitConfirmLatencyUnder1s
	case elapsed < 2*time.Second:
		return counterSubmitConfirmLatencyUnder2s
	default:
		return counterSubmitConfirmLatencyUnder4s
	}
}

// countClippedCause records which rule read a refused screen as clipped, beside
// the per-verb counter its caller has already incremented: the visible-pane rule
// (#169) and the pane's bottom edge (#216) each get their own, so the rate of
// each is readable without inferring it from the total.
func (d *Driver) countClippedCause(s screen) {
	if clippedOnlyAboveVisiblePane(s) {
		d.counters.incr(counterComposerClippedAboveVisiblePane)
	}
	if bottomCutComposer(s) {
		d.counters.incr(counterComposerClippedBottomCut)
	}
}
