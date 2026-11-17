package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Reading the runtime's own record of a REFUSAL, rather than the screen it
// painted about one (#56).
//
// # What the record says, measured rather than assumed
//
// A refusal is not a distinct event type in the record. It is an ordinary
// `type: "assistant"` entry the runtime writes IN PLACE of a real reply, with
// four extra fields set: `isApiErrorMessage: true`, `error` (a closed set
// measured on a live fleet: "rate_limit", "server_error",
// "authentication_failed", "oauth_org_not_allowed"), `apiErrorStatus` (the
// HTTP status, when the failure came from one), and a `message.content[0].text`
// carrying the runtime's own prose — the SAME sentence the screen renders
// ("You've hit your session limit · resets 9:50pm (Asia/Saigon)" for
// `rate_limit`; "API Error: 529 Overloaded ... usually temporary — try again
// in a moment" for a retryable `server_error`).
//
// `rate_limit` is specifically the account-level condition §2.3 calls
// `quota_blocked`. The other three categories are turn failures of a
// different kind — real, worth `LastTurn`, but never `Quota`.
//
// # Written at the refusal, not deferred to some later completion
//
// Measured across sampled records: the `system/turn_duration` entry that
// closes out a turn lands one to five milliseconds after the error entry —
// the error entry effectively IS the turn's terminal write, not a fact
// buffered until later. A refusal that ends the session is therefore not at
// risk of going unrecorded, which was this issue's own "measurement first"
// worry.
//
// # No native retryable field
//
// There is nothing in the record that says "this is temporary" as a
// boolean — only the same prose the screen already carries. So this file
// does not invent a status-code-based inference (lastTurnFailed's own
// comment already explains why that would be the wrong move); it applies
// the SAME word rule (retryableWords) to the record's text instead of a
// live, window-bounded screen scan. The source moves; the rule does not.

const (
	// recordTailBytes bounds how much of a record file this driver reads
	// looking for the most recent assistant entry. Deliberately smaller than
	// conversation.go's identity read: that one reads the top of a file
	// once and caches it forever; this one may run every read cycle for
	// every session currently reporting quota_blocked or a failed last
	// turn, so it stays cheap by design rather than by luck. A record whose
	// final line does not fit in this window is honestly unresolvable
	// (recordUnavailable) rather than silently wrong — the driver already
	// has a screen-derived fallback for exactly that case.
	recordTailBytes = 256 << 10 // 256 KiB

	// recordTailCandidates bounds how many decoded lines within the tail
	// window this driver is willing to inspect before giving up. The
	// answer is always the LAST assistant-type line, which in practice is
	// within the first few lines scanned backward; this is a defensive
	// ceiling, not an expected depth.
	recordTailCandidates = 200
)

// recordVerdict is what latestAPIError concluded about a record's most
// recent assistant-role entry.
type recordVerdict int

const (
	// recordUnavailable: the record could not be opened, or nothing
	// decodable was found in the tail window. Not a claim about the
	// session — §5.7's rule applied to this source the way it already
	// applies to a failed pane capture: absence of a usable read is a
	// different fact from a positive answer either way, and a caller
	// must fall back rather than treat this as "no error".
	recordUnavailable recordVerdict = iota

	// recordCleanTurn: the most recent assistant entry carries no error.
	// This is POSITIVE evidence the account is not refusing work and the
	// last turn did not fail — the durable counterpart to `sawWorking`,
	// and the signal #56 asks for to take a session OUT of quota_blocked
	// without waiting for some other pane to be caught mid-spinner.
	recordCleanTurn

	// recordAPIError: the most recent assistant entry is the runtime's own
	// report of a refusal. apiErrorFact carries what it said.
	recordAPIError
)

// apiErrorFact is what the record says about its most recent refusal.
type apiErrorFact struct {
	// category is the runtime's own error string ("rate_limit" is quota;
	// anything else is a turn failure of a different kind).
	category string
	// text is the runtime's own prose, in full — deliberately NOT trimmed
	// to one sentence here. lastTurnFailed's Retryable check reads
	// retryableWords against the WHOLE joined screen text, and a 529's
	// "usually temporary" clause sits after the first period the sentence
	// cut would stop at ("API Error: 529 Overloaded. ... usually temporary
	// — try again in a moment."). Trimming here would silently make every
	// retryable server error read as not-retryable. Callers trim for
	// display (trimToSentence) and check retryableWords on this, in that
	// order — never the reverse.
	text string
	// at is the refusal's own timestamp, as the runtime wrote it — not
	// when this driver happened to read the file. For a clean turn read by
	// latestServedEntry it is that turn's timestamp, zero when it has none.
	at time.Time
}

// reasonSentence is fact.text cut to one sentence for display — the record
// counterpart to lastTurnFailed's own trim, applied at the same point
// (after retryableWords has already looked at the full text).
func (f apiErrorFact) reasonSentence() string {
	return trimToSentence(f.text, 200)
}

// retryable applies the same word rule lastTurnFailed uses on the screen,
// to this fact's full text.
func (f apiErrorFact) retryable() bool {
	return retryableWords(strings.ToLower(f.text))
}

// apiErrorRecordEntry is the subset of one JSONL line this driver reads to
// decide whether it is a refusal. Everything else in the line — model
// details, usage counters, request ids — is deliberately not decoded: this
// answers "did the runtime report a failure, and what did it say", nothing
// about the conversation's content.
type apiErrorRecordEntry struct {
	Type              string `json:"type"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
	Timestamp         string `json:"timestamp"`
	Message           struct {
		// Model is who wrote the entry: a model's name for an answer the
		// account served, syntheticModel for text the runtime composed itself.
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// syntheticModel is the model name the runtime writes on an assistant entry it
// composed itself — its own report of a refusal, or a placeholder such as "no
// response requested". Nothing served by the account wrote it, so it is no
// evidence that the account answered anything.
const syntheticModel = "<synthetic>"

// latestAPIError reads the tail of one runtime record and reports what its
// most recent assistant-role entry says.
//
// The most recent assistant entry answers both questions this driver ever
// asks of a record: whether the account is STILL refusing work (only true
// while that entry is itself an unanswered `rate_limit` refusal — anything
// after it, success or a different kind of failure, means something has
// happened since), and how the session's LAST TURN ended.
// recordTail reads the last recordTailBytes of one runtime record and
// returns it as decoded lines, oldest first — the same window and the same
// torn-first-line allowance every reader of this store needs, factored out
// so latestAPIError (#56) and latestControlDisconnect (#69) cannot drift
// apart on it. ok is false whenever the file could not be opened or stat'd;
// an empty record (ok true, zero lines) is a different, legitimate fact from
// that.
//
// torn is true whenever the file was larger than this window and the read
// therefore started mid-file — muster #111's turnsSince is the first
// caller that needs this itself (its own honesty rule depends on knowing
// whether the window could possibly have reached back far enough); every
// other caller here answers "what does the MOST RECENT entry say" and does
// not care whether earlier history was cut off, so torn is a plain second
// return value rather than a new sibling function they would all have had
// to switch to.
func recordTail(path string) (lines []string, torn bool, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, false
	}
	start := int64(0)
	if info.Size() > recordTailBytes {
		start = info.Size() - recordTailBytes
		torn = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, false
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if torn && len(lines) > 0 {
		// The first line after a mid-file seek may be a torn fragment of
		// whatever line straddled the seek point. The record is
		// append-only, so dropping one candidate costs nothing a real
		// answer would have needed — the line we actually want is later
		// (closer to EOF), never this one.
		lines = lines[1:]
	}
	return lines, torn, true
}

func latestAPIError(path string) (apiErrorFact, recordVerdict) {
	return latestAssistantEntry(path, false)
}

// latestServedEntry is latestAPIError for the one question "did the account
// serve a turn, and when" (#489): the newest assistant entry that is either a
// refusal or a real model answer. An entry the runtime composed itself that is
// not a refusal ("no response requested") says nothing either way, so it is
// stepped over rather than read as a clean turn — one of those landing after a
// refusal must not read as the account serving again.
//
// A recordCleanTurn from here carries the entry's own timestamp in fact.at,
// zero when the entry has none or it cannot be parsed. A turn that cannot be
// dated proves nothing about WHEN the account served, and every caller treats a
// zero time as no proof.
func latestServedEntry(path string) (apiErrorFact, recordVerdict) {
	return latestAssistantEntry(path, true)
}

func latestAssistantEntry(path string, answersOnly bool) (apiErrorFact, recordVerdict) {
	lines, _, ok := recordTail(path)
	if !ok {
		return apiErrorFact{}, recordUnavailable
	}

	inspected := 0
	for i := len(lines) - 1; i >= 0 && inspected < recordTailCandidates; i-- {
		inspected++
		var entry apiErrorRecordEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			// A torn or half-written line is not a reason to give up on
			// the ones before it (conversation.go's readRecordEntry makes
			// the same allowance for the same reason: the runtime appends,
			// and a reader may arrive mid-write).
			continue
		}
		if entry.Type != "assistant" {
			continue
		}
		if answersOnly && !entry.IsAPIErrorMessage && entry.Message.Model == syntheticModel {
			continue
		}
		// The most recent assistant-role entry, whichever it is, answers
		// the question. Stop here regardless of what it says — an OLDER
		// assistant entry is history superseded by this one, exactly the
		// replay hazard usageLimit and lastTurnFailed already guard
		// against on the screen side.
		if !entry.IsAPIErrorMessage {
			at, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
			return apiErrorFact{at: at}, recordCleanTurn
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			// The record found the right line but not a fact we can act
			// on: honestly unresolvable rather than a guessed time.
			return apiErrorFact{}, recordUnavailable
		}
		var text string
		if len(entry.Message.Content) > 0 {
			text = entry.Message.Content[0].Text
		}
		return apiErrorFact{
			category: entry.Error,
			text:     text,
			at:       ts,
		}, recordAPIError
	}
	return apiErrorFact{}, recordUnavailable
}

// resetHint extracts the runtime's own reset time out of this fact's text,
// the same rule usageLimit applies to a screen line — display it, never
// parse it further.
func (f apiErrorFact) resetHintText() string {
	h, _ := resetHintIn(strings.ToLower(f.text))
	return h
}

// turnRecordEntry is the subset of one JSONL line turnsSince decodes: just
// enough to recognise a turn-boundary marker and its timestamp, nothing
// about what the turn said or produced (muster #111). The runtime
// writes this entry unasked, as an ordinary structural marker between
// turns — never something an agent authors — which is the provenance
// docs/adr/111-turns-is-a-liveness-fact-not-a-result-channel.md rests the
// whole field on.
type turnRecordEntry struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Timestamp string `json:"timestamp"`
}

// turnsSince counts turn_duration markers in one runtime record that landed
// strictly after `since` — the timestamp of the delivery a caller wants the
// count relative to.
//
// ok is false whenever this driver cannot answer HONESTLY, not merely
// whenever the count comes out zero (§5.7): the record could not be opened,
// or the tail window this driver is willing to read does not demonstrably
// reach back past `since`. Reporting 0 in either of those cases would
// manufacture "the prompt never landed" out of a read failure — worse than
// the gap #111 exists to close, because it would look like a positive
// answer instead of an honest absence.
//
// Deliberately does NOT apply recordTailCandidates: that ceiling exists for
// "find the newest matching line and stop looking", the shape
// latestAPIError needs. This needs the whole window, because a turn that
// happened five turns ago is exactly as countable as the most recent one.
func turnsSince(path string, since time.Time) (count int, ok bool) {
	lines, torn, readOK := recordTail(path)
	if !readOK {
		return 0, false
	}

	reachesBack := !torn
	for _, line := range lines {
		var entry turnRecordEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			// A torn or half-written line: skip it, the same allowance
			// latestAPIError makes for the same reason (the runtime
			// appends; a reader may arrive mid-write). Neither confirms
			// nor denies reaching back past `since`.
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			continue
		}
		if !ts.After(since) {
			// A decodable line at or before the delivery mark: proof this
			// window reaches back far enough to answer honestly, whether
			// or not the file was torn at the byte level.
			reachesBack = true
			continue
		}
		if entry.Type == "system" && entry.Subtype == "turn_duration" {
			count++
		}
	}
	if !reachesBack {
		return 0, false
	}
	return count, true
}

// turnsSinceOffset counts turn_duration markers appended to a runtime
// record strictly after byte offset `from` — the incremental counterpart to
// turnsSince, used once a checkpoint (deliveryMark.Size) has already been
// established. Unlike turnsSince, this needs no honesty-window proof against
// a timestamp: `from` IS the boundary already vouched for by a previous
// successful count (it is always a prior info.Size() taken right after a
// full JSONL line was flushed, so it always lands exactly on a line
// boundary), so reading forward from it to EOF is provably complete however
// large the gap has grown — which is exactly what turnsSince's own
// window-honesty rule (correctly, by its own contract) cannot promise once
// the tail window no longer reaches back to the ORIGINAL delivery mark
// (muster #142: a single long turn is large enough to do this between
// polls).
//
// ok is false only when the record cannot be opened/stat'd, or the file is
// now SMALLER than `from` (a truncated or rotated record — the checkpoint no
// longer means anything and the caller must fall back to a fresh baseline
// scan instead of trusting it).
func turnsSinceOffset(path string, from int64) (count int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, false
	}
	if info.Size() < from {
		return 0, false
	}
	if info.Size() == from {
		return 0, true
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, false
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	for sc.Scan() {
		var entry turnRecordEntry
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type == "system" && entry.Subtype == "turn_duration" {
			count++
		}
	}
	return count, true
}

// turnsFor answers muster #111's liveness fact for one session, given
// its already-resolved conversation record — List resolves that in its own
// pending-conversation pass before this runs; State's own call site resolves
// it itself, the same split upgradeLastTurnFromRecord already uses between
// those two callers.
//
// When a delivery mark already has a checkpoint size (mark.Size > 0), the
// count is incremented by reading forward from that size rather than by
// timestamp-deriving a new window (#142); an unchanged checkpoint returns a
// cached value without re-reading the file.
//
// nil is the honest answer whenever this driver cannot count: no delivery
// mark for this session (nothing has been delivered into it that this
// driver remembers), no conversation resolved, or the record's window could
// not be shown to reach back far enough — never a guessed 0 (§5.7).
func (d *Driver) turnsFor(id, cwd string, conv *fleet.ConversationRef) *int {
	if d.conversations == nil || conv == nil || !conv.Known {
		return nil
	}
	mark, ok := d.deliveryMarkFor(id, cwd)
	if !ok {
		return nil
	}
	path := d.conversations.recordPath(cwd, conv.ID)

	info, statErr := os.Stat(path)
	if statErr == nil && mark.Size > 0 && info.Size() == mark.Size {
		// Memo-and-latch: an unchanged file size since the last successful
		// count is a cache read, not a reason to re-parse.
		n := mark.Count
		return &n
	}

	if statErr == nil && mark.Size > 0 && info.Size() > mark.Size {
		// A checkpoint already exists: count only what landed after it.
		// This needs no window-honesty proof against `mark.At` — `mark.Size`
		// is itself the already-vouched-for boundary (muster #142).
		added, addedOK := turnsSinceOffset(path, mark.Size)
		if addedOK {
			n := mark.Count + added
			d.updateDeliveryMarkCount(id, cwd, mark.At, n, info.Size())
			return &n
		}
		// The checkpoint no longer refers to anything real (the record
		// shrank below it — truncated or rotated underneath this driver).
		// Fall through to the original timestamp-anchored path below rather
		// than trust it.
	}

	n, ok := turnsSince(path, mark.At)
	if !ok {
		if mark.Size > 0 {
			// A flaky read this time does not erase a real count already
			// established — report the latch rather than flapping to
			// absent for a reason unrelated to whether the session is
			// alive.
			cached := mark.Count
			return &cached
		}
		return nil
	}
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	d.updateDeliveryMarkCount(id, cwd, mark.At, n, size)
	return &n
}

// upgradeTurnsFromRecord applies #111's turns count to a bare SessionState
// that has no pre-resolved Conversation to reuse — State's own shape, unlike
// List's Session (the same split upgradeLastTurnFromRecord already uses
// between these two callers).
//
// The delivery-mark check runs FIRST, before any conversation lookup, as its
// own cost gate: a session this driver has never delivered into has nothing
// for this to report, so it costs one map read and nothing more — the same
// "only a flagged/marked session ever opens a record" discipline List's own
// turns loop and #56's LastTurn upgrade both already follow.
func (d *Driver) upgradeTurnsFromRecord(ctx context.Context, st fleet.SessionState, cwd, name string, created time.Time, paneID string, pid int) fleet.SessionState {
	if d.conversations == nil {
		return st
	}
	if _, ok := d.deliveryMarkFor(name, cwd); !ok {
		return st
	}
	ref := d.conversations.lookup(conversationKey{pane: paneID, created: created}, cwd, name, created,
		processGeneration{pid: pid}, d.liveConversationSource(ctx, pid, cwd))
	st.Turns = d.turnsFor(name, cwd, ref)
	return st
}

// recordFactFor asks the runtime's own record about one session already
// carried in a fleet.Session — reusing its already-resolved Conversation
// rather than looking one up again (#56). recordUnavailable whenever no
// record store is configured, this session's conversation could not be
// matched, or the matched record could not be read; every caller must keep
// its screen-derived fallback in exactly those cases, not only the ones it
// happens to check for.
func (d *Driver) recordFactFor(s fleet.Session) (apiErrorFact, recordVerdict) {
	if d.conversations == nil || s.Conversation == nil || !s.Conversation.Known {
		return apiErrorFact{}, recordUnavailable
	}
	return latestAPIError(d.conversations.recordPath(string(s.Cwd), s.Conversation.ID))
}

// quotaRecordPath is where one session's runtime record lives, when this
// listing resolved which conversation the session is.
func (d *Driver) quotaRecordPath(s fleet.Session) (string, bool) {
	if d.conversations == nil || s.Conversation == nil || !s.Conversation.Known {
		return "", false
	}
	return d.conversations.recordPath(string(s.Cwd), s.Conversation.ID), true
}

// quotaEvidence settles, for one whole listing, what the evidence says about
// one CREDENTIAL's account (#489, #490): the fact and verdict noteQuotaBlock
// takes, in place of "whichever flagged session the loop met first".
//
// sessions are the sessions of that credential, and only them: a turn another
// credential served proves nothing about this one, and a refusal on another
// credential is not this one's.
//
// recordCleanTurn is returned when the account is shown to serve AFTER the
// newest refusal there is evidence of, which is either of
//
//   - a session whose screen shows a limit notice but whose OWN newest turn
//     in its record is one the account served — the notice is history, as
//     #56 established; or
//   - a turn served later than every refusal the machine shows evidence of:
//     any session's record whose newest turn is a real answer, or a session
//     this driver saw working (noteServed). A turn is positive proof the
//     account serves; it needs no spinner caught on this read, and it does
//     not need the session to be one whose screen happens to show a notice.
//
// "Evidence of a refusal" is, newest first wins: the block's own since; the
// refusal any record dates (a rate_limit entry's own timestamp); and a limit
// notice on a screen whose record dates nothing, which cannot be newer than
// the moment this driver first saw that screen (screenSince). That last one
// is what keeps an old notice, still sitting on an idle screen nobody has
// typed into, from re-creating a block the account has outlived: its screen
// has not changed since before the proof, so it is the old refusal. A NEW
// refusal changes the screen, so it is dated after the proof and blocks.
//
// Before this the record half read only the FIRST session whose screen showed
// a notice. With no notice on any screen, which is how a block outlives the
// notices that announced it, nobody was read; with a refusal nobody ever
// resumed listed first, it answered for every session after it; and a notice
// left on an idle screen put the block back as soon as it cleared.
//
// recordAPIError is the first flagged session's refusal, which noteQuotaBlock
// uses to date a block at the refusal itself; recordUnavailable is "no usable
// record said anything", and the caller keeps its screen-derived fallback.
//
// The cost is paid only by a listing with something to settle: no block and no
// notice on any screen reads nothing at all, and a block costs one stat per
// session (recordProbe), not a read.
func (d *Driver) quotaEvidence(credential string, sessions []fleet.Session) (apiErrorFact, recordVerdict) {
	now := d.now()
	q, observed := d.quotaBlockFor(credential)
	var flagged []int
	for i := range sessions {
		if sessions[i].State.Status == fleet.StatusQuotaBlocked {
			flagged = append(flagged, i)
		}
	}
	if q == nil && len(flagged) == 0 {
		return apiErrorFact{}, recordUnavailable
	}

	var fact apiErrorFact
	verdict := recordUnavailable
	var undated []int // flagged sessions whose record dates no refusal
	for _, i := range flagged {
		path, ok := d.quotaRecordPath(sessions[i])
		if !ok {
			undated = append(undated, i)
			continue
		}
		f, v := d.quotaProbe.latest(path)
		switch {
		case v == recordCleanTurn:
			d.noteServed(credential, f.at, now)
			return f, v
		case v == recordAPIError && verdict == recordUnavailable:
			fact, verdict = f, v
		}
		if !(v == recordAPIError && f.category == "rate_limit") {
			undated = append(undated, i)
		}
	}

	// The time the account is known to have refused work at, which a turn must
	// come after to prove it serves again. A refusal the record dates is that
	// time — unless the block already carries a record-confirmed one that is
	// later. With neither (a block first seen on a screen, no record to date
	// it) it is the moment the block began being remembered, which nothing can
	// precede and so cannot be disproved by a turn recorded before it.
	var refusal time.Time
	switch {
	case verdict == recordAPIError && fact.category == "rate_limit" &&
		(q == nil || !observed || fact.at.After(q.Since)):
		refusal = fact.at
	case q != nil:
		refusal = q.Since
	}
	// A notice no record dates is newest-bounded by when its screen was first
	// seen. A session this driver has never seen adds nothing it can claim.
	for _, i := range undated {
		if t := d.screenSince(sessions[i].ID); t.After(refusal) {
			refusal = t
		}
	}
	if refusal.IsZero() {
		return fact, verdict
	}

	// Newest evidence wins. Every record on the machine is read (a stat each,
	// recordProbe) for the newest refusal it dates and the newest turn it shows
	// served; the account serves again exactly when the turn is the later of
	// the two. A turn proved by an earlier listing stands, whichever sessions
	// this one happens to show.
	proof := d.servedAt(credential)
	paths := make([]string, 0, len(sessions))
	for i := range sessions {
		if path, ok := d.quotaRecordPath(sessions[i]); ok {
			paths = append(paths, path)
		}
	}
	for _, path := range paths {
		f, v := d.quotaProbe.latest(path)
		switch {
		case v == recordAPIError && f.category == "rate_limit" && f.at.After(refusal):
			refusal = f.at
		case v == recordCleanTurn && f.at.After(proof) && !f.at.After(now):
			// A turn dated after now is a record this machine's clock
			// disagrees with; counting one would outrank every refusal
			// recorded before the clock caught up.
			proof = f.at
		}
	}
	if proof.After(refusal) {
		d.noteServed(credential, proof, now)
		return apiErrorFact{at: proof}, recordCleanTurn
	}
	return fact, verdict
}

// screenSince is when this driver first saw session id's screen showing what it
// shows now (observation.digestSince): the latest a notice on it can have
// appeared. Zero for a session never observed.
func (d *Driver) screenSince(id string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.observed[id]
	if !ok {
		return time.Time{}
	}
	if !o.digestSince.IsZero() {
		return o.digestSince
	}
	return o.at
}

// noteServed remembers that credential's account was shown serving a turn at
// at: a record's own timestamp, or the read at which a session on it was seen
// working.
//
// A turn dated after now is a record this machine's clock disagrees with, and
// remembering one would outrank every refusal recorded before it caught up, for
// good — so it is not remembered.
func (d *Driver) noteServed(credential string, at, now time.Time) {
	if at.After(now) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.accountLocked(credential); at.After(a.served) {
		a.served = at
	}
}

// servedAt is the newest turn any record has shown credential's account serving.
func (d *Driver) servedAt(credential string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.quotas[credential]; a != nil {
		return a.served
	}
	return time.Time{}
}

// upgradeLastTurnFromRecord applies #56's LastTurn upgrade to a bare
// SessionState that has no pre-resolved Conversation to reuse — State's own
// shape, unlike List's Session. It performs its own conversation lookup,
// keyed the same way conversation.go keys one (pane + creation time); that
// lookup memoises successes, so calling it again for a session List has
// already resolved this cycle is a cache read, not a rescan.
//
// A no-op whenever st.LastTurn is nil (the screen flagged nothing this
// read) or no record store is configured or the record cannot be matched —
// in every one of those cases the screen-derived TurnEnd, or its absence,
// is left exactly as classify.go built it.
func (d *Driver) upgradeLastTurnFromRecord(ctx context.Context, st fleet.SessionState, cwd, name string, created time.Time, paneID string, pid int) fleet.SessionState {
	if st.LastTurn == nil || d.conversations == nil {
		return st
	}
	ref := d.conversations.lookup(conversationKey{pane: paneID, created: created}, cwd, name, created,
		processGeneration{pid: pid}, d.liveConversationSource(ctx, pid, cwd))
	if ref == nil || !ref.Known {
		return st
	}
	switch fact, verdict := latestAPIError(d.conversations.recordPath(cwd, ref.ID)); verdict {
	case recordAPIError:
		st.LastTurn = &fleet.TurnEnd{
			Outcome:   "failed",
			Reason:    fact.reasonSentence(),
			Retryable: fact.retryable(),
		}
	case recordCleanTurn:
		// The durable record says the last turn actually succeeded — the
		// screen's "api error" match was history a window scan cannot
		// tell from the present. #56's argument for Quota, arriving at
		// LastTurn instead.
		st.LastTurn = nil
	}
	return st
}

// recordProbe answers latestServedEntry for many records on every read, at the
// price of a stat for each one whose record has not grown (#489).
//
// A machine whose account is blocked has to ask every session's record whether
// the account serves again, on every listing, and a listing is taken after each
// output burst. Reading each record's tail that often is the cost
// recordTailBytes was sized to avoid for the few sessions the screen flags;
// here it would be paid by all of them. The answer for a record can only change
// when the record does, and the runtime only appends, so size and modification
// time together name the answer already read.
type recordProbe struct {
	mu   sync.Mutex
	seen map[string]probeRead

	// read is the reader behind the memo; nil means latestServedEntry. A test
	// sets it to count how often a record is really read.
	read func(path string) (apiErrorFact, recordVerdict)
}

type probeRead struct {
	size    int64
	modTime time.Time
	fact    apiErrorFact
	verdict recordVerdict
}

// latest is latestServedEntry(path), re-read only when the record has changed
// since the answer held for it. recordUnavailable whenever the record cannot be
// statted: a missing record is never answered from memory.
func (p *recordProbe) latest(path string) (apiErrorFact, recordVerdict) {
	info, err := os.Stat(path)
	if err != nil {
		return apiErrorFact{}, recordUnavailable
	}
	p.mu.Lock()
	prior, ok := p.seen[path]
	p.mu.Unlock()
	if ok && prior.size == info.Size() && prior.modTime.Equal(info.ModTime()) {
		return prior.fact, prior.verdict
	}
	read := p.read
	if read == nil {
		read = latestServedEntry
	}
	fact, verdict := read(path)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string]probeRead{}
	}
	p.seen[path] = probeRead{size: info.Size(), modTime: info.ModTime(), fact: fact, verdict: verdict}
	return fact, verdict
}

// keep drops every memo whose record is not in live, so the memo cannot
// outgrow the fleet.
func (p *recordProbe) keep(live map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for path := range p.seen {
		if !live[path] {
			delete(p.seen, path)
		}
	}
}
