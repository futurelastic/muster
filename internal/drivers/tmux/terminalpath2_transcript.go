package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
)

// Terminal path v2, item c / D6: "screen-based confirmation is the weak
// link". confirmSubmitted (tmux.go) only ever asks the SCREEN whether the
// composer emptied or a marker cleared — evidence that the multiplexer
// redrew the pane, not that the runtime PROCESS treated the paste as a
// submitted turn. The runtime already writes that fact, unasked, to its own
// transcript (conversation.go documents the same file family this reads):
// a `"type":"user"` entry when it was idle, or a `"type":"queue-operation"`
// enqueue when it was busy. Measured for #180: nothing at all is
// written for a STRANDED delivery (29 of 29), the asymmetry this file turns into a positive signal instead
// of only a negative one.
//
// # Why this is a SEPARATE file from conversation.go rather than an addition to it
//
// conversation.go answers "which record belongs to this session" and
// deliberately reads nothing else from the file ("This whole path answers
// 'which record' without ever answering 'what is in it'" — its own doc
// comment). This file is the first thing in the package that DOES open a
// record and read message content, because confirming a submit is exactly
// the question conversation.go declined to answer. Keeping the two apart
// keeps that boundary legible in the diff, not just in prose.
//
// # The honest decision on Outcome (task requirement: decide, don't dodge)
//
// A transcript-confirmed send still reports OutcomeQueued, never
// OutcomeSubmitted, and ConfirmsDelivery stays false for it — unchanged from
// every other terminal-path outcome (docs/api.md's own note: "'submitted' is
// never returned by the terminal Send path"). The reasoning: a `type:user`
// transcript entry proves the RUNTIME PROCESS accepted the text as a turn —
// substantially stronger evidence than "the composer redrew empty", which is
// why it gets its own counter and is tried first — but it is still not an
// acknowledgement from the ADDRESSED PARTY (the agent) that it received or
// acted on the message, which is what this fleet reserves OutcomeSubmitted
// for (the inbox path's own receipt). Upgrading the outcome on the strength
// of this evidence would be exactly the "guess dressed as a reading" this
// driver's own §5.6 discipline exists to refuse. If that bar is ever revised,
// it should be revised for the whole fleet's Outcome vocabulary at once, not
// smuggled in through one driver's one confirmation path.
//
// # Fixtures are synthetic
//
// Every unit test against this file uses SYNTHETIC transcript lines its own
// tests construct, never a real transcript: a real one is somebody's
// conversation. The entry shapes they model were read from the runtime's
// own writer, and the live gate for #180 exercised them end to end.

// pastedTextMarkerPattern recognises a transcript entry whose content is
// ENTIRELY Claude Code's own collapsed-paste placeholder — the same textual
// shape the composer renders on screen for a paste too large to echo
// (markerCounts, tmux.go), which round-1's own background names as also
// appearing in the transcript for a large paste ("account for Claude Code's
// pasted-content wrapper for >800 chars / 5+ lines"). Anchored (^...$) after
// trimming: a marker embedded inside other prose is a false positive this
// driver has no way to attribute, and is deliberately left unmatched by this
// pattern (falling through to the plain substring/equality checks instead).
var pastedTextMarkerPattern = regexp.MustCompile(`^\[Pasted text #\d+(?: \+(\d+) lines?)?\]$`)

// parsePastedTextMarker reports the line count a collapsed-paste placeholder
// claims, when s (already trimmed) is ENTIRELY that placeholder. ok=false
// covers both "not a marker at all" and "a marker with no line count printed"
// — the second is legitimate (see markerCounts' own numberAfter, which
// treats a missing count as 0 rather than as absent) and is reported here as
// lines=0, ok=true, matching that same convention.
func parsePastedTextMarker(s string) (lines int, ok bool) {
	m := pastedTextMarkerPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	if m[1] == "" {
		return 0, true
	}
	n := 0
	for _, r := range m[1] {
		n = n*10 + int(r-'0')
	}
	return n, true
}

// pastedContentWrapperPattern strips the ONE wrapper the runtime is known to
// add around a pasted block it stores in full inside a transcript entry:
// `<pasted_content id="...">...</pasted_content>`, keeping the inner text in
// place. Unlike the composer's on-screen "[Pasted text #N]" placeholder
// (parsePastedTextMarker, below — a rendering artefact this driver has no
// proof the transcript ever repeats verbatim), the runtime needs the actual
// bytes in the transcript to act on them, so the plausible shape for a large
// paste there is the REAL text wrapped in an identifying tag, not a
// placeholder standing in for it.
var pastedContentOpenTag = regexp.MustCompile(`<pasted_content\s+id="[^"]*">`)

// pastedContentCloseTag: review fix. The runtime is measured (runtime 2.1.281,
// its own writer) to close this wrapper with the
// SAME id attribute repeated on the closing tag —
// `</pasted_content id="c702">`, not the plain `</pasted_content>` this
// pattern used to require. A closing tag missing that attribute is also
// accepted (never independently measured, but strictly weaker to allow, and
// nothing about matching it can make a real transcript match WORSE) — see
// TestConfrevRealPastedContentWrapperMatches / …LongPasteRealWrapperConfirmedByTranscript
// for the fixture this was verified against. Before this fix, EVERY
// transcript entry carrying a wrapped paste failed to normalise to its bare
// text at all (the trailing `</pasted_content id="c702">` survived intact,
// so an otherwise byte-identical turn never compared equal), which meant D6
// did nothing for exactly the messages most likely to strand — the ones long
// enough to trigger the wrapper in the first place — and every one of them
// fell through the full submitConfirmWindow to the weaker screen signal.
var pastedContentCloseTag = regexp.MustCompile(`</pasted_content(?:\s+id="[^"]*")?>`)

// normalizeTranscriptText prepares a transcript entry's own text for an
// EXACT comparison against sent, after stripping the specific, known extras
// the runtime is measured (or plausibly documented — see this function's own
// doc comment for what is which) to add on top of the bytes this driver
// actually delivered:
//
//   - the <pasted_content id="..."> wrapper tags, content kept;
//   - a tab expanded to 4 spaces (composerText and the runtime's own
//     rendering both do this; unverified whether the TRANSCRIPT does too, but
//     harmless to allow — a real 4-space run this driver's own text already
//     had collapses back to identical either way once whitespace is later
//     stripped by normalizeForMatch);
//   - a single trailing space — the wake key (Space, tmux.go) is pressed as
//     a real keystroke into the composer immediately before Enter, so the
//     runtime receives sent+" ", not sent, on every ordinary submit.
//
// normalizeForMatch's own NFC-compose-and-strip-whitespace pass runs last, so
// none of the above needs to worry about composing-form or interior
// whitespace differences — that is a separate, already-solved axis.
func normalizeTranscriptText(s string) string {
	s = pastedContentOpenTag.ReplaceAllString(s, "")
	s = pastedContentCloseTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.TrimSuffix(s, " ")
	return normalizeForMatch(s)
}

// transcriptTurnMatches reports whether a transcript entry's own extracted
// text corresponds to sent — the SAME text this driver pasted (already
// through paneLabelled's sender-label prefix, if any: Send always calls this
// with the LABELLED text, so a caller here never needs its own tolerance for
// a missing label — see the review fix below for why that used to be
// conflated with a much more dangerous tolerance).
//
// # Review fix: exact match after normalisation, not "contains"
//
// This used to accept ANY recorded turn containing normSent as a substring,
// with no minimum length and no check on which KIND of transcript entry it
// was reading from (extractTranscriptText's own isMeta/isCompactSummary/
// isSidechain/origin/promptSource filters, added by this same review pass,
// close the second half of that). The substring rule's own justification —
// "covers the sender-label line paneLabelled prepends" — was already moot
// for every real call in this package: Send always passes the LABELLED text
// as sent (see terminalpath2_transcript.go's callers in tmux.go), so
// recordedText and sent already carry the same label on both sides whenever
// this is called from production code, and a substring rule bought nothing
// there. What it did buy: a skill's transcript body containing the sent word
// "go" as a substring confirmed it; a compaction summary quoting an earlier,
// unrelated turn confirmed it; a bare "[Pasted text #1]" with no line count
// confirmed ANY sent text at all, because parsePastedTextMarker's own
// ok=true/lines=0 fell through the OLD "lines == 0 || ..." check
// unconditionally rather than only for a length-agreeing paste. Requiring
// exact equality (after the specific, named extras normalizeTranscriptText
// allows) removes all of those at once, at the cost of the one shape that
// was never real in the first place.
func transcriptTurnMatches(recordedText, sent string) bool {
	normSent := normalizeForMatch(sent)
	if normSent == "" {
		return false
	}
	if normalizeTranscriptText(recordedText) == normSent {
		return true
	}
	// The bare collapsed-paste-marker fallback: kept because whether the
	// transcript ever ACTUALLY stores this literal placeholder instead of
	// the real bytes is unverified (see this file's own top comment), not
	// because it is known to happen. Scoped tightly — the recorded text must
	// be ENTIRELY the marker (parsePastedTextMarker's own anchored pattern),
	// and a PRINTED line count must agree with sent's own newline count. A
	// marker with NO printed count (lines==0, ok==true) no longer matches
	// unconditionally: it now requires sent to carry no newline either — a
	// bare "[Pasted text #7]" is Claude Code's own summary for a paste with
	// nothing further to say about line count, which is what a single-line
	// paste's own marker looks like (muster round-1 background); it is
	// not a wildcard for "however many lines a completely unrelated send
	// happened to have".
	if lines, ok := parsePastedTextMarker(recordedText); ok {
		sentLines := strings.Count(sent, "\n")
		return lines == sentLines
	}
	return false
}

// extractTranscriptText parses one JSONL line and reports, when it is a
// candidate turn at all, which family it is ("user" or "queue-operation")
// and the plain text it carries. Deliberately tolerant of more than one
// content shape — see this file's own top comment for why the exact
// "queue-operation" shape is not independently verified here.
//
// A third family, "command", is a slash command the runtime recorded rather than
// a message: a user entry wrapped in <command-name> tags (#180 M3), or a
// `system`/`local_command` entry (#187, localCommandLine). Its text is
// "/name args".
//
// # Review fix: the real queue-operation shape, and what is NOT a candidate
//
// Claude Code's own writer for a queue-operation entry (read from the
// runtime's own writer) puts the queued text in a top-level "content" field, only
// on operation=="enqueue" — never in a top-level "text" field, and never on
// "dequeue"/"remove"/"popAll". The old check matched on type containing
// "queue" (case-insensitive) with no operation check at all, and read only a
// top-level "text" — so a REAL enqueue entry was never recognised, and this
// driver could not distinguish an enqueue from Claude Code discarding the
// same queued text later under "remove" (the review's own probes: an
// enqueue and dequeue can both carry the same text; treating either as
// confirmation would fabricate a match at the wrong moment, or worse, at
// exactly the moment a queued instruction was WITHDRAWN rather than
// accepted). "content" is read FIRST now, as the verified real shape; the
// old top-level "text" stays as a second, tolerant fallback — costs nothing,
// and the one existing test exercising it stays honest about what it is
// (see TestExtractTranscriptTextQueueOperationTopLevelText's own comment).
//
// A "user" candidate is now also rejected outright when isMeta,
// isCompactSummary or isSidechain is true, or when an `origin` object names
// a kind other than "human", or when `promptSource` is present and names
// something other than "typed" or "queued" (round-1's own measurement: real
// human/agent sends carry origin.kind=human, promptSource=typed/queued) —
// closing the other half of the review's "loose text matching" finding: a
// skill body, a compaction summary quoting an earlier turn, or a
// cross-session peer message are all `type:"user"` entries in the same file,
// and none of them is a candidate for "did OUR delivery get accepted" no
// matter how its text compares.
func extractTranscriptText(line []byte) (kind, text string, ok bool) {
	kind, text, _, ok = extractTranscriptCandidate(line)
	return kind, text, ok
}

// extractTranscriptCandidate is extractTranscriptText plus the one extra
// field #180 review's anchoring fix needs: promptSource, verbatim
// off a "user" candidate ("typed", "queued", or "" when the field is
// absent — round-1's own measurement is that a real entry always carries
// one of the first two, but a caller must not assume that of every
// transcript this driver will ever read). A "queue-operation" candidate
// always reports promptSource "" — the field belongs to a "user" entry, not
// an enqueue.
//
// Kept as the internal, richer function with extractTranscriptText as a thin
// wrapper (rather than widening extractTranscriptText's own signature)
// because every existing test and caller already depends on the narrower
// three-value shape, and this driver's own copy-and-own discipline (§5.x)
// says do not reshape a signature everything already agrees on merely to
// grow one caller a field nothing else needs.
func extractTranscriptCandidate(line []byte) (kind, text, promptSource string, ok bool) {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return "", "", "", false
	}
	if truthy(obj["isMeta"]) || truthy(obj["isCompactSummary"]) || truthy(obj["isSidechain"]) {
		return "", "", "", false
	}
	t, _ := obj["type"].(string)
	if t == "system" {
		// #187: the one system entry that is a candidate at all — a command the
		// runtime ran itself. Everything else it writes as "system" is not a
		// turn and never was.
		if sub, _ := obj["subtype"].(string); sub == "local_command" {
			if line, ok := localCommandLine(obj); ok {
				return "command", line, "", true
			}
		}
		return "", "", "", false
	}
	isUser := t == "user"
	operation, _ := obj["operation"].(string)
	isEnqueue := t == "queue-operation" && operation == "enqueue"
	if !isUser && !isEnqueue {
		return "", "", "", false
	}
	if isUser {
		if origin, ok := obj["origin"].(map[string]any); ok {
			if k, _ := origin["kind"].(string); k != "" && k != "human" {
				return "", "", "", false
			}
		}
		if ps, ok := obj["promptSource"].(string); ok {
			promptSource = ps
			if ps != "" && ps != "typed" && ps != "queued" {
				return "", "", "", false
			}
		}
		// Shape 1 (documented, used by conversation.go's own readRecordEntry
		// for other fields on this same family of file): message.content,
		// either a plain string or an array of content blocks each carrying
		// "type" and "text".
		if msg, ok := obj["message"].(map[string]any); ok {
			if role, _ := msg["role"].(string); role != "" && role != "user" {
				// A message present but not authored as "user" is not this
				// delivery's own turn (an assistant turn logged under a
				// different type key would not reach here at all, but a
				// tolerant reader costs nothing to double-check).
				return "", "", "", false
			}
			switch content := msg["content"].(type) {
			case string:
				text = content
			case []any:
				var b strings.Builder
				for _, blockAny := range content {
					block, ok := blockAny.(map[string]any)
					if !ok {
						continue
					}
					if bt, _ := block["type"].(string); bt != "" && bt != "text" {
						continue
					}
					if s, ok := block["text"].(string); ok {
						b.WriteString(s)
					}
				}
				text = b.String()
			}
		}
	} else {
		// isEnqueue: the verified real shape reads a top-level "content"
		// first; "text" is kept only as a tolerant fallback for a shape this
		// driver has never actually observed (see this function's own doc
		// comment).
		if s, ok := obj["content"].(string); ok {
			text = s
		}
		if text == "" {
			if s, ok := obj["text"].(string); ok {
				text = s
			}
		}
	}
	if text == "" {
		return "", "", "", false
	}
	if isUser {
		// #180 M3: the runtime records a slash command as a user entry
		// wrapped in <command-name>…</command-name> tags, and a command's
		// own output or a bash-mode line as <local-command-…> / <bash-…>
		// entries. None of those is a message turn. A command entry is kept,
		// as kind "command", so a sent slash command can be confirmed by
		// it; the others are not candidates at all.
		trimmed := strings.TrimSpace(text)
		switch {
		case isCommandMarkup(trimmed):
			return "command", commandLine(trimmed), promptSource, true
		case strings.HasPrefix(trimmed, "<local-command-"), strings.HasPrefix(trimmed, "<bash-"):
			return "", "", "", false
		}
	}
	return t, text, promptSource, true
}

// differentTurnComposerEmptied opens the evidence of a submit the transcript
// contradicted after the composer had emptied: the caller must not describe
// that text as sitting in the composer.
const differentTurnComposerEmptied = "the composer emptied, but the runtime's own transcript " +
	"recorded a DIFFERENT turn instead of this text, so whether this text arrived is unknown"

// isCommandMarkup reports whether an entry's text is slash-command markup.
//
// The runtime leads with a different tag depending on the command's kind,
// measured on real transcripts: a built-in command (/context, /rename) leads
// with <command-name>, a skill command (/wrap, /code-wrap) leads with
// <command-message> and carries <command-name> after it (#262). Both are the
// same human-typed slash command, so both open command markup; commandLine
// reads the tags by name and does not care which came first. An entry that
// leads with neither (a <local-command-…> output, a <bash-…> line) is not.
func isCommandMarkup(trimmed string) bool {
	return strings.HasPrefix(trimmed, "<command-name>") || strings.HasPrefix(trimmed, "<command-message>")
}

// commandLine rebuilds "/name args" from a command entry's tags.
func commandLine(entry string) string {
	name := between(entry, "<command-name>", "</command-name>")
	args := strings.TrimSpace(between(entry, "<command-args>", "</command-args>"))
	if args == "" {
		return strings.TrimSpace(name)
	}
	return strings.TrimSpace(name) + " " + args
}

// localCommandLine reads the slash command a `system`/`local_command` entry
// names, as "/name args", and reports false when the entry names none (#187).
//
// # Measured, not assumed
//
// Read off real transcripts written by runtime builds 2.1.273 through 2.1.281
// (34 commands, every one of the two forms below present in every build): a
// command the runtime runs itself — a session-management command such as
// /rename — does NOT write the `<command-name>` user entry #180 M3 was built
// on. It writes entries with `"type":"system","subtype":"local_command"`,
// `"isMeta":false`, in two forms:
//
//   - an echo, first, whose `content` is the same `<command-name>` /
//     `<command-message>` / `<command-args>` markup a user entry carries,
//     leading slash included, and which carries no `commandRun`;
//   - a result, immediately after it (adjacent in all 34 pairs), whose
//     `content` is the command's `<local-command-stdout>` and which carries
//     `commandRun: {"command": "rename", "args": "<the args>"}` — the name WITHOUT
//     its slash, and args equal to the echo's in all 34 pairs.
//
// Both name the command, so either confirms it, and taking both is not
// redundancy. The echo is the earliest evidence, written at dispatch, so a
// command that takes seconds to run still confirms then and not at completion.
// The result is the one entry every measured local command wrote: a command that
// records the user-entry form instead (M3, above) has no echo, only that user
// entry and then this result. Reading the result too means a build that drops
// one of the earlier entries costs no confirmation.
//
// The stdout of the result is never read: it is the command's output, which can
// be anything, and it is not needed — the name and the arguments are already
// structured.
func localCommandLine(obj map[string]any) (string, bool) {
	if run, ok := obj["commandRun"].(map[string]any); ok {
		name, _ := run["command"].(string)
		name = strings.TrimPrefix(strings.TrimSpace(name), "/")
		if name == "" {
			return "", false
		}
		args, _ := run["args"].(string)
		if args = strings.TrimSpace(args); args != "" {
			return "/" + name + " " + args, true
		}
		return "/" + name, true
	}
	content, _ := obj["content"].(string)
	trimmed := strings.TrimSpace(content)
	if !isCommandMarkup(trimmed) {
		return "", false
	}
	if line := commandLine(trimmed); strings.HasPrefix(line, "/") {
		return line, true
	}
	return "", false
}

func between(s, open, shut string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	s = s[i+len(open):]
	if j := strings.Index(s, shut); j >= 0 {
		return s[:j]
	}
	return ""
}

// commandMatches reports whether a recorded command line is the slash
// command sent: same command name, and the same arguments when the entry
// recorded any.
func commandMatches(recorded, sent string) bool {
	sent = collapseSpace(sent)
	recorded = collapseSpace(recorded)
	if !strings.HasPrefix(sent, "/") || recorded == "" {
		return false
	}
	sentName, sentArgs, _ := strings.Cut(sent, " ")
	recName, recArgs, _ := strings.Cut(recorded, " ")
	if sentName != recName {
		return false
	}
	return recArgs == "" || recArgs == sentArgs
}

// truthy reports whether v — one field pulled out of a decoded JSON object —
// is JSON `true`. Every other shape (absent, false, a string, a number) is
// not, which is the right default for a field this driver reads only to
// EXCLUDE a candidate: absence must never itself exclude something, only an
// explicit true may.
func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// transcriptScanResult is transcriptTailScan's own three-way verdict — see
// its doc comment for what each value means and why "no match" was not
// enough on its own (#180 review's own finding).
type transcriptScanResult int

const (
	// transcriptScanSilent: nothing after offset was even a CANDIDATE turn
	// for this delivery — the ordinary, common case while a poll is still
	// waiting for the runtime to write anything at all.
	transcriptScanSilent transcriptScanResult = iota
	// transcriptScanMatched: a candidate turn attributable to THIS delivery
	// (see transcriptTailScan's own doc comment on the enqueue/dequeue
	// anchoring this requires) matched sent exactly.
	transcriptScanMatched
	// transcriptScanDifferentTurn: a candidate turn was recorded, attributable
	// to this delivery's own confirmation window, and it did NOT match sent —
	// evidence something else was submitted (a truncated echo, a merged
	// keystroke, ...), which must never be treated the same as "nothing
	// happened yet" (transcriptScanSilent): the caller's fallback for silence
	// is the screen-based signal, and treating a genuine disagreement as
	// silence is exactly what let that fallback report `queued` for text
	// that, per the transcript's OWN account, was never actually submitted.
	transcriptScanDifferentTurn
)

// transcriptTailMatches is transcriptTailScan collapsed to the boolean shape
// its own (pre-review) callers and tests already depend on — kept as a thin
// wrapper for exactly the same "do not reshape an agreed signature" reason
// extractTranscriptText wraps extractTranscriptCandidate.
func transcriptTailMatches(path string, offset int64, sent string) (bool, error) {
	result, err := transcriptTailScan(path, offset, sent)
	return result == transcriptScanMatched, err
}

// transcriptTailScan scans path from byte offset to EOF for a candidate turn
// matching sent, exactly once per call — the caller polls. offset is the
// file size recorded BEFORE the submit keystroke (resolveTranscriptSource /
// its caller), which is what makes "identical earlier text must not count"
// structural rather than a rule this function has to enforce itself: text
// already in the file before offset is never read at all.
//
// # Review fix: a dequeue is only attributed to THIS delivery if an enqueue
// for the same text was ALSO seen after offset
//
// A queue-operation "enqueue" and the "user"/promptSource=="queued" entry
// that later dequeues it can carry byte-identical text (round-1's own
// measurement: an ordinary re-ping, a repeated "continue"/"yes", or
// a consumer.s automatic resumeIfStranded retry all produce exactly this
// duplicate-content shape). Before this fix, a dequeue entry matched sent
// with no regard for WHICH enqueue it belonged to — so an identical message
// enqueued BEFORE this delivery's own offset, whose dequeue happened to land
// inside THIS delivery's confirmation window, confirmed a submit that never
// actually registered (this delivery's own Enter having been swallowed), and
// the caller's documented remedy then delivered a duplicate.
//
// queueDepth is the fix: it only ever counts enqueues seen strictly AFTER
// offset, and a dequeue may only be attributed to this delivery by consuming
// one of those. A dequeue seen while queueDepth is zero belongs to a turn
// that was already queued before this delivery even started — informative
// about something else, not about this delivery — so it is skipped
// entirely: neither a match nor a disagreement.
func transcriptTailScan(path string, offset int64, sent string) (transcriptScanResult, error) {
	result, _, err := transcriptTailScanKind(path, offset, sent)
	return result, err
}

// transcriptTailScanKind is transcriptTailScan plus WHICH entry matched
// (#240). A match on a queue-operation enqueue says the runtime accepted the
// text into its queue — not that it started a turn on it — and a queue can be
// handed back to the composer. A match on a command entry or on a user entry
// says a turn exists. viaEnqueue is true only for the first.
func transcriptTailScanKind(path string, offset int64, sent string) (result transcriptScanResult, viaEnqueue bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return transcriptScanSilent, false, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return transcriptScanSilent, false, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	queueDepth := 0
	sawDifferent := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		kind, text, promptSource, ok := extractTranscriptCandidate(line)
		if !ok {
			continue
		}
		if kind == "command" {
			// #180 M3: a command entry confirms a sent slash command and is
			// otherwise not a turn at all — never a "different turn".
			if commandMatches(text, sent) {
				return transcriptScanMatched, false, nil
			}
			continue
		}
		matches := transcriptTurnMatches(text, sent)
		if kind == "queue-operation" {
			// Direct evidence a busy send registered — the fast path, tried
			// first, matching the pre-review behaviour exactly (an enqueue
			// this driver can see happened strictly after offset is, by
			// construction, a NEW event; there is no earlier-turn ambiguity
			// to anchor against here the way there is for its dequeue).
			if matches {
				return transcriptScanMatched, true, nil
			}
			queueDepth++
			continue
		}
		// kind == "user".
		if promptSource == "queued" {
			if queueDepth <= 0 {
				// This dequeue cannot be attributed to an enqueue this
				// delivery's own window has seen — see this function's own
				// doc comment. Not evidence about this delivery either way.
				continue
			}
			queueDepth--
		}
		if matches {
			return transcriptScanMatched, false, nil
		}
		sawDifferent = true
	}
	if err := sc.Err(); err != nil {
		return transcriptScanSilent, false, err
	}
	if sawDifferent {
		return transcriptScanDifferentTurn, false, nil
	}
	return transcriptScanSilent, false, nil
}

// transcriptUserTurnSince reports whether the transcript, after offset, holds
// a user turn carrying exactly this text — the runtime having started a turn
// on it (#240). An enqueue does not count: that is the very evidence a hand-back
// leaves behind. A transcript that cannot be read reports false, which is the
// safe direction for its one caller: nothing is then shown to have consumed
// the text, and the draft rule's other requirements still apply.
func transcriptUserTurnSince(path string, offset int64, sent string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		kind, text, _, ok := extractTranscriptCandidate(line)
		if !ok || kind != "user" {
			continue
		}
		if transcriptTurnMatches(text, sent) {
			return true
		}
	}
	return false
}

// processSessionRecord is the shape of one `~/.claude/sessions/<pid>.json`
// file — read for exactly four fields, the ones D6's fallback needs; every
// other field the real file carries (round-1 measured it also carries
// `startedAt`, `version`, `kind`, `entrypoint`, `pidDomain`, `tmux`,
// `messagingSocketPath`, `name`, `agent`, `status`, ...) is left unparsed on
// purpose — reading a field this driver does not use would be one more
// place a future rename of that field silently breaks something.
type processSessionRecord struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	// ProcStart is measured (for #180) to already be
	// rendered in exactly `ps -o lstart=`'s TEXTUAL layout — but, unlike
	// genuine `ps` output, this field crosses a process boundary (this file
	// is written by the runtime's own Node.js process, not read live from
	// `ps` by this driver), so it is exactly the case ParseProcessStartTime's
	// own doc comment names and warns against: "never for a value that may
	// have crossed a process, a machine, or a serialization boundary" —
	// muster #147 already found this same mistake once, for a
	// different field. Round-1 measured it directly for THIS field too: a
	// pid file's own procStart reads "Thu Sep 24 07:38:57 2026" for the same
	// process whose `ps -o lstart=` (parsed as local time, correctly) reads
	// "Thu Sep 24 16:38:57 2026" — a 9-hour gap consistent with the writer
	// rendering in UTC while this machine's local zone is UTC+9, not with
	// two different processes. Reusing ParseProcessStartTime (time.Local) on
	// THIS field silently mis-parsed it as local, so `recordedStart` never
	// equalled `identity.StartedAt` for ANY live pid on such a machine —
	// every send fell back to the screen-based signal, and D6's whole
	// pid-identity path never actually fired. See
	// parseProcessSessionRecordStartTime (terminalpath2_transcript.go),
	// which parses this field with the correct (UTC) zone instead.
	ProcStart string `json:"procStart"`
}

// parseProcessSessionRecordStartTime parses processSessionRecord.ProcStart —
// see that field's own doc comment for why ParseProcessStartTime (time.Local)
// is the wrong parser for it, even though the two share an identical textual
// layout.
//
// The runtime writes this field in one of two shapes, by platform. The textual
// layout above is what it writes where it reads the start time from `ps`. On
// Linux it writes the raw `starttime` field of /proc/<pid>/stat instead — a
// bare decimal count of clock ticks since boot (measured: "343765263" for a
// process `ps -o lstart=` reports as starting at boot + 3437652 s). Rejecting
// that shape left the per-process record unusable on every Linux machine, so
// a session's conversation stayed unknown until its transcript existed — and
// a freshly spawned session with no turn yet has none.
//
// The ticks are converted exactly the way procps renders `lstart`: boot time
// plus whole seconds of ticks, truncated. Both sides are then whole seconds
// derived from the same two kernel values, so corroborateProcessRecord's
// exact-equality check keeps its meaning — a recycled pid still has a
// different starttime and still fails it.
func parseProcessSessionRecordStartTime(s string) (time.Time, error) {
	if ticks, err := strconv.ParseUint(s, 10, 64); err == nil {
		boot, err := linuxBootTime()
		if err != nil {
			return time.Time{}, fmt.Errorf("procStart %q is a Linux tick count, but boot time is unavailable: %w", s, err)
		}
		return boot.Add(time.Duration(ticks/linuxUserHZ) * time.Second), nil
	}
	return time.ParseInLocation(psStartTimeLayout, s, time.UTC)
}

// linuxUserHZ is the unit of /proc/<pid>/stat's starttime. The kernel exports
// every tick-valued /proc field in USER_HZ, which is fixed at 100 as userspace
// ABI regardless of the kernel's internal HZ — the same constant procps divides
// by. Go's standard library has no sysconf(_SC_CLK_TCK) to ask for it.
const linuxUserHZ = 100

// linuxBootTime reads the `btime` line of /proc/stat — the boot instant, in
// whole seconds since the epoch, that procps adds a process's starttime to.
// A variable so a test can pin boot time without a real /proc; off Linux the
// read fails, which leaves a tick-shaped procStart unparsed exactly as before.
var linuxBootTime = func() (time.Time, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("/proc/stat btime %q: %w", v, err)
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, errors.New("/proc/stat has no btime line")
}

// processSessionRecordBytes returns the raw bytes of one process-sessions file,
// or false when this driver has no such root configured or the file cannot be
// read. It is the ONE place that maps a pid to that file, so a caller that needs
// more of the record than the four fields parsed below (muster compat
// checks the record's whole shape, #183) reads the same bytes rather than
// building a second reader that could disagree about where the file is.
func (d *Driver) processSessionRecordBytes(pid int) ([]byte, bool) {
	if d.processSessionsRoot == "" {
		return nil, false
	}
	b, err := os.ReadFile(d.processSessionRecordPath(pid))
	if err != nil {
		return nil, false
	}
	return b, true
}

// processSessionRecordPath is where one pid's record lives: the ONE place that
// maps a pid to its file, used by the reader that says why it declined (#182)
// and by the raw-bytes reader below it, so the two cannot disagree.
func (d *Driver) processSessionRecordPath(pid int) string {
	return filepath.Join(d.processSessionsRoot, fmt.Sprintf("%d.json", pid))
}

// readProcessSessionRecord reads and parses one process-sessions file. ok is
// false for anything short of a complete, well-formed record — a caller with
// a partial answer has no safe use for it (§5.4: never resume/verify
// identity on a guess).
func (d *Driver) readProcessSessionRecord(pid int) (processSessionRecord, bool) {
	rec, why := d.readProcessSessionRecordWhy(pid)
	return rec, why == ""
}

// readProcessSessionRecordWhy is readProcessSessionRecord that says why it
// declined (#182): why is empty exactly when the record is complete and
// well-formed. The sentence carries no filesystem path — it ends up in a
// session's conversation evidence, which a client reads.
func (d *Driver) readProcessSessionRecordWhy(pid int) (processSessionRecord, string) {
	rec, why, rejected := d.readProcessSessionRecordQuiet(pid)
	if rejected {
		d.counters.incr(counterProcessSessionRecordRejected)
	}
	return rec, why
}

// readProcessSessionRecordQuiet is readProcessSessionRecordWhy that counts
// nothing and says whether it refused a session id for its shape (#203). The
// check that runs on every memo hit in a listing reads a record it has no
// decision to make about, and a counter of refusals that ticked once per listing
// per session for one bad file would measure the listing rate, not the files.
// Everything else — what it reads, what it refuses, the sentence it gives — is
// the same code, so the two cannot disagree about a record.
func (d *Driver) readProcessSessionRecordQuiet(pid int) (rec processSessionRecord, why string, rejected bool) {
	if d.processSessionsRoot == "" {
		return processSessionRecord{}, "no per-process record root is configured", false
	}
	b, err := os.ReadFile(filepath.Join(d.processSessionsRoot, fmt.Sprintf("%d.json", pid)))
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		if errors.Is(err, fs.ErrNotExist) {
			return processSessionRecord{}, fmt.Sprintf("the runtime has written no per-process record for pid %d", pid), false
		}
		return processSessionRecord{}, fmt.Sprintf("the per-process record for pid %d could not be read: %v", pid, err), false
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return processSessionRecord{}, fmt.Sprintf("the per-process record for pid %d is not readable JSON", pid), false
	}
	if rec.PID != pid || rec.SessionID == "" || rec.CWD == "" || rec.ProcStart == "" {
		return processSessionRecord{}, fmt.Sprintf("the per-process record for pid %d is incomplete or names another process", pid), false
	}
	// #180 L5: the session id becomes part of a path. Anything but the
	// runtime's own UUID shape is refused, so "../../elsewhere" never
	// resolves outside the record root.
	if !uuidShaped(rec.SessionID) {
		return processSessionRecord{}, fmt.Sprintf("the per-process record for pid %d carries a conversation id that is not UUID-shaped", pid), true
	}
	return rec, "", false
}

// processRecordFor is the half of the per-process check that needs no
// question put to the OS: the record for pid is read, and must name the
// working directory this session is in (#182). why is empty exactly when the
// record may go on to be corroborated.
//
// Split from corroborateProcessRecord so a caller holding many sessions can
// stop at a missing record without paying for the `ps` that corroboration
// needs — most of a fleet's listing cost is such questions.
func (d *Driver) processRecordFor(pid int, cwd string) (processSessionRecord, string) {
	rec, why := d.readProcessSessionRecordWhy(pid)
	if why != "" {
		return processSessionRecord{}, why
	}
	if rec.CWD != cwd {
		// §5.4: a record naming a different working directory is not
		// evidence about THIS session, whatever else it says — refuse
		// rather than guess it is merely stale.
		return processSessionRecord{}, fmt.Sprintf("the per-process record for pid %d names a different working directory than this session's", pid)
	}
	return rec, ""
}

// corroborateProcessRecord is the other half: the record's own start time
// must equal the start time of the process that is running now under that
// pid. Exact equality, because both sides are whole-second text in the same
// layout — the record's own resolution is one second, so there is nothing
// finer for a tolerance to absorb, and any wider tolerance would only be a
// window in which a reused pid's stale record could pass. why is empty
// exactly when it corroborated.
//
// parseProcessSessionRecordStartTime, NOT ParseProcessStartTime — see
// processSessionRecord.ProcStart's own doc comment for the review fix this is
// (the field crosses a process boundary and is measured to be rendered in UTC;
// parsing it as local silently broke this whole check on any machine not
// itself running in UTC).
func corroborateProcessRecord(rec processSessionRecord, identity ProcessIdentity) (liveProcessIdentity, string) {
	recordedStart, err := parseProcessSessionRecordStartTime(rec.ProcStart)
	if err != nil {
		return liveProcessIdentity{}, fmt.Sprintf("the per-process record for pid %d carries a start time that does not parse", identity.PID)
	}
	if !recordedStart.Equal(identity.StartedAt) {
		// The pid was recycled since this file was written (or the file is
		// simply malformed) — VerifyProcessIdentity's own reasoning, applied
		// here instead of duplicated as a second check against the OS.
		return liveProcessIdentity{}, fmt.Sprintf("the per-process record for pid %d was written by a process that started at a "+
			"different time than the one running under that pid now (a reused pid, or a record an earlier process left)", identity.PID)
	}
	return liveProcessIdentity{
		sessionID: rec.SessionID,
		evidence: fmt.Sprintf("process-sessions file for pid %d (verified same process generation), "+
			"sessionId %s", identity.PID, rec.SessionID),
		process: processGeneration{pid: identity.PID, startedAt: identity.StartedAt},
	}, ""
}

// transcriptSource is what resolveTranscriptSource found: enough to poll a
// transcript file from a fixed starting offset, plus the evidence a receipt
// can quote when this signal is the one that decided the outcome.
type transcriptSource struct {
	path     string
	offset   int64
	evidence string
}

// liveProcessIdentity is what resolveLiveProcessSessionID established: the
// runtime's OWN, currently-running claim of which conversation this pane
// holds, verified against the same process generation #116's
// ResolveProcessIdentity already checks.
type liveProcessIdentity struct {
	sessionID string
	evidence  string
	// process is the run the record was corroborated against (#202) — already
	// measured here, so the conversation store can be handed it for free.
	process processGeneration
}

// resolveLiveProcessSessionID is D6's per-process identity fallback,
// factored out of resolveTranscriptSource so it can ALSO be used to
// cross-check a cached record-root lookup — see resolveTranscriptSource's
// own doc comment on the review's "stale transcript after /clear" finding
// for why a cache hit alone is no longer trusted unconditionally.
//
// ok=false covers every reason this driver could not corroborate a live
// sessionId for target right now: not configured, the process could not be
// resolved, its own session file disagrees on cwd, or its own recorded
// start time no longer matches the running process's generation (a
// recycled pid, or simply a malformed file) — §5.4 throughout: a partial
// answer is not a safe one to act on.
func (d *Driver) resolveLiveProcessSessionID(ctx context.Context, ref fleet.SessionRef, target *paneRow) (liveProcessIdentity, bool) {
	if d.processSessionsRoot == "" {
		return liveProcessIdentity{}, false
	}
	identity, err := d.ResolveProcessIdentity(ctx, ref)
	if err != nil {
		return liveProcessIdentity{}, false
	}
	rec, why := d.processRecordFor(identity.PID, target.cwd)
	if why != "" {
		return liveProcessIdentity{}, false
	}
	live, why := corroborateProcessRecord(rec, identity)
	if why != "" {
		return liveProcessIdentity{}, false
	}
	return live, true
}

// resolveTranscriptSource is D6's own two-step identity chain: try the
// record-root lookup every List already uses (d.conversations, keyed on the
// session's own NAME — conversation.go's own "reading our own input back out
// of a file somebody else wrote"); if that comes back unresolved — the
// ~18/75 "resumed sessions" gap round-1 measured, recorded as
// conversation.known=false — fall back to the runtime's own per-process
// identity file, corroborated against #116's process-identity verification
// (ResolveProcessIdentity) so a recycled pid's stale file is refused rather
// than trusted (§5.4).
//
// # Review fix: a cache hit is cross-checked, not trusted forever
//
// conversationStore's own "resolved" cache is keyed on (pane, created) for
// the WHOLE LIFE of a session (conversation.go's own doc comment: "resolved
// caches per SESSION, and only successes"). It was built to stop re-deriving
// an identity that had already been established — it was never built to
// notice the runtime regenerating its OWN session id under an otherwise
// unchanged tmux pane, which `/clear` and an in-pane runtime restart both do
// (round-1's own background note). Nothing about a cache keyed on the PANE
// changes when that happens, so every send after a `/clear` kept reading a
// stale, pre-clear transcript file forever — silently unknown, then a
// resume duplicating into the NEW conversation, matching the review's own
// reproduction.
//
// The fix does not remove the cache (it is still what makes most sends not
// pay ResolveProcessIdentity's cost of re-listing panes and running `ps`) —
// it cross-checks it, whenever the live per-process path CAN be resolved,
// against what the RUNNING PROCESS reports right now. Agreement is the
// common case and costs one extra, already-cheap file read; disagreement
// means the cache is stale, and the live identity wins.
//
// # Since #202 the cache remembers which process it was established against
//
// An in-pane runtime restart, one of the two cases named above, changes the
// pane's pid, and the store now drops an answer on seeing that (conversation.go,
// processGeneration) — for the listing as much as for this path. What it cannot
// see is `/clear`: the runtime regenerates its id under the SAME process, the
// pid and its start time do not move, and only the per-process record says so.
// So this cross-check is still what covers `/clear` here. The listing has its
// own since #203 — a memo hit peeks at the same record, without the `ps` — so
// the two answer alike whether or not a listing ran between the `/clear` and this
// send.
//
// # Since #182 the lookup consults the per-process record too
//
// The lookup in the first step is no longer name-only: handed the per-process
// answer resolved just above, it reconciles the two (conversationprocess.go).
// A session the name cannot answer now resolves THERE, and one where the two
// sources name different conversations comes back unresolved — which lands in
// the fallback below, on the live process's own answer. The stale-cache check
// above is unchanged: it is about a success remembered BEFORE the runtime
// regenerated its id, which a first lookup cannot be.
//
// ok=false means neither step produced a file this driver could confirm is
// the right one — the caller's own responsibility from there is to fall back
// to the screen-based confirmSubmitted, per this file's own top comment.
func (d *Driver) resolveTranscriptSource(ctx context.Context, ref fleet.SessionRef, target *paneRow) (transcriptSource, bool) {
	live, liveOK := d.resolveLiveProcessSessionID(ctx, ref, target)

	if d.conversations != nil {
		key := conversationKey{pane: target.paneID, created: target.created}
		// The run this send is about: the one just measured when the live path
		// resolved, else what the multiplexer said about the pane (#202).
		process := processGeneration{pid: target.pid}
		if liveOK {
			process = live.process
		}
		if conv := d.conversations.lookup(key, target.cwd, ref.ID, target.created, process,
			d.liveConversationFrom(live, liveOK)); conv != nil && conv.Known {
			if !liveOK || live.sessionID == "" || live.sessionID == conv.ID {
				p := d.conversations.recordPath(target.cwd, conv.ID)
				if info, err := os.Stat(p); err == nil {
					return transcriptSource{
						path:     p,
						offset:   info.Size(),
						evidence: fmt.Sprintf("record-root lookup (%s): %s", conv.Source, conv.Evidence),
					}, true
				}
			} else {
				// The cache resolved to a DIFFERENT conversation id than the
				// runtime's own live identity reports right now — stale,
				// per this function's own doc comment. Fall through to the
				// live identity below rather than trusting the cache.
				d.counters.incr(counterTranscriptCacheStaleAfterClear)
			}
		}
	}

	// Fallback (or cache-was-stale override): the runtime's own per-process
	// identity, resolved above. Both the record root (to turn a sessionId
	// into a file path) and the process-sessions root (to learn the
	// sessionId at all) must be configured for this to produce anything —
	// see WithProcessSessionsRoot and WithRecordRoot's own off-by-default
	// contracts.
	if d.conversations == nil || !liveOK {
		return transcriptSource{}, false
	}
	p := d.conversations.recordPath(target.cwd, live.sessionID)
	info, err := os.Stat(p)
	if err != nil {
		return transcriptSource{}, false
	}
	return transcriptSource{path: p, offset: info.Size(), evidence: live.evidence}, true
}

// confirmSubmittedFromSource is confirmSubmittedV2's review-fixed
// replacement, split into two calls so the CALLER (Send, tmux.go) can
// resolve src BEFORE pressing the submit key rather than after — see this
// file's own doc comment on resolveTranscriptSource's caller contract below
// for why that ordering matters. src/srcOK is exactly
// resolveTranscriptSource's own return, taken at whatever point the caller
// resolved it.
//
// # Review fix: a resolved-but-silent transcript now falls back to the screen
//
// The exact "queue-operation" shape this driver parses (extractTranscriptText)
// is measured against real transcripts as of this review pass, but the field
// this driver is not independently certain about — whether EVERY busy-session
// enqueue is written inside submitConfirmWindow, versus this driver's own
// window simply being too short for a slow write — means a resolved
// transcript that stays silent through the whole poll is not, on its own,
// proof the delivery never registered. The OLD behaviour treated that
// silence as final and told the caller "unknown"; the caller's own
// documented remedy (resumeIfStranded) then re-pasted into a composer the
// runtime had ALREADY cleared (because it accepted the message and queued
// it) — the busy-session duplicate-delivery defect the review measured live,
// twice, on two different builds. Falling back to the pre-existing
// screen-based confirmSubmitted ONLY after the transcript's own window
// closes keeps the transcript as the FIRST, preferred signal (still tried
// exclusively for the first submitConfirmWindow, still what confirms most
// real sends per counterSubmitConfirmedByTranscript) while giving the
// composer-emptied/marker-cleared signal the chance to catch exactly the gap
// the transcript's own unverified shape might be missing —
// counterSubmitConfirmedByScreenAfterSilentTranscript is how a live rate for
// that gap becomes visible instead of staying a duplicate-delivery incident.
//
// # Review fix: a scanner error is "cannot tell", never silently "no match"
//
// transcriptTailMatches can fail outright (not merely reach EOF unmatched) —
// one transcript line exceeding recordLineLimit is bufio.Scanner's own
// "token too long". The old code discarded that error (`matched, _ :=`),
// making an unreadable transcript line indistinguishable from one that was
// read in full and simply disagreed. Counting it separately
// (counterTranscriptScannerUnreadable) does not change what this loop does
// with it — there is nothing safer to try than keep polling until the
// window closes, the same as an ordinary non-match — but it stops silently
// misfiling "this driver could not read what it was looking at" as if it
// were positive evidence of disagreement.
//
// Returns the same bool confirmSubmitted always has, plus which family of
// evidence decided it, for the receipt to quote.
func (d *Driver) confirmSubmittedFromSource(ctx context.Context, target *paneRow, sent string, key pasteKey, atCount int, src transcriptSource, srcOK bool) (bool, string, delivery.Signal) {
	confirmed, evidence, signal, _ := d.confirmSubmittedFromSourceTurn(ctx, target, sent, key, atCount, src, srcOK)
	return confirmed, evidence, signal
}

// confirmSubmittedFromSourceTurn is confirmSubmittedFromSource plus whether the
// evidence shows a TURN exists for this text (#240). Only a transcript entry
// of a started turn (a user entry, or a command entry) does. The runtime
// queueing the text (an enqueue), the composer emptying, or a paste marker
// clearing each show the text left the composer — and a runtime that queued it
// can hand it back. Callers keep a provisional record for a confirmation that
// is not turnProven.
func (d *Driver) confirmSubmittedFromSourceTurn(ctx context.Context, target *paneRow, sent string, key pasteKey, atCount int, src transcriptSource, srcOK bool) (confirmed bool, evidence string, signal delivery.Signal, turnProven bool) {
	if !srcOK {
		confirmed := d.confirmSubmitted(ctx, target.paneID, key, atCount)
		signal := delivery.SignalNone
		if confirmed {
			signal = delivery.SignalScreen
		}
		return confirmed, "no transcript could be resolved for this session; fell back to the screen-based signal (composer emptying or this delivery's own paste marker clearing)", signal, false
	}

	start := d.now()
	deadline := start.Add(submitConfirmWindow)
	for {
		result, viaEnqueue, err := transcriptTailScanKind(src.path, src.offset, sent)
		if err != nil {
			d.counters.incr(counterTranscriptScannerUnreadable)
		}
		switch result {
		case transcriptScanMatched:
			d.recordConfirmed(counterSubmitConfirmedByTranscript, d.now().Sub(start))
			if viaEnqueue {
				// #240: queued is not consumed. The runtime accepted the text into
				// its queue; a queue can be handed back to the composer.
				d.counters.incr(counterSubmitConfirmedByEnqueue)
			}
			return true, "the runtime's own transcript recorded this exact text as a turn (" + src.evidence + ")", delivery.SignalTranscript, !viaEnqueue
		case transcriptScanDifferentTurn:
			// Review fix: a transcript that recorded a DIFFERENT turn is not
			// silence, and must not be handed to the screen-based fallback
			// below — that fallback exists for "the transcript said nothing
			// at all", and a composer emptying for an unrelated reason (a
			// truncated echo of this same paste, or a person's own keystroke
			// merging in) would then be reported as "queued" for text the
			// transcript's own account says never arrived. Stop polling; more
			// time will not turn a recorded disagreement back into silence.
			d.counters.incr(counterTranscriptDifferentTurnRecorded)
			// #180 M3: still not a confirmation — but look at the screen once,
			// so the receipt says what is true. If the composer emptied, the
			// text is not "sitting there unsent"; whether it arrived is what
			// is unknown.
			if sc, ok := d.captureForClassify(ctx, target.paneID); ok {
				if pending, scan := composerText(sc); scan == composerFound && pending == "" {
					return false, differentTurnComposerEmptied + " (" + src.evidence + ")", delivery.SignalNone, false
				}
			}
			return false, "a transcript was resolved (" + src.evidence + ") but recorded a DIFFERENT " +
				"turn instead of this delivery's own text within the confirmation window — not " +
				"falling back to the screen signal, which could report this as queued for text " +
				"that, per the transcript's own account, never arrived", delivery.SignalNone, false
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			d.counters.incr(counterSubmitConfirmTimeout)
			return false, "context cancelled while waiting for the transcript to confirm this delivery", delivery.SignalNone, false
		case <-time.After(submitConfirmInterval):
		}
	}

	// The transcript's own window closed with nothing matching. Rather than
	// reporting unknown outright — see this function's own doc comment on
	// why that used to cause a duplicate delivery — give the pre-existing
	// screen-based signal one look before giving up: it costs one more
	// capture, and it is exactly the signal that would have confirmed this
	// delivery before D6 existed.
	if confirmed := d.confirmSubmitted(ctx, target.paneID, key, atCount); confirmed {
		d.recordConfirmed(counterSubmitConfirmedByScreenAfterSilentTranscript, d.now().Sub(start))
		return true, "a transcript was resolved (" + src.evidence + ") but recorded no matching turn " +
			"within the confirmation window; confirmed instead by the composer emptying or this " +
			"delivery's own paste marker clearing", delivery.SignalScreen, false
	}
	d.counters.incr(counterSubmitConfirmTimeout)
	return false, "a transcript was resolved (" + src.evidence + ") but recorded no matching turn " +
		"within the confirmation window, and the screen shows neither the composer emptying nor this " +
		"delivery's own paste marker clearing either", delivery.SignalNone, false
}

// uuidShaped reports whether s is 8-4-4-4-12 hexadecimal, the shape of the
// runtime's session ids (#180 L5).
func uuidShaped(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
	}
	return true
}
