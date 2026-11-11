package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Reading back a session's record (muster #441).
//
// # The boundary, and where it is enforced
//
// docs/adr/441-a-sessions-full-record-is-read-under-its-own-grant.md widens what
// may be read from a session's record, on its own route and under its own grant
// (`record`). This file is where this runtime's share of that is drawn, and
// nowhere downstream: the service, the relay and the wire type carry whatever
// recordMessageOf returns without looking at it.
//
// What this reader admits today is the text of the conversation in both
// directions and nothing else:
//
//   - the assistant's own prose, by the SAME predicate turns uses
//     (assistantEntry, turns.go), so the two routes cannot disagree about what
//     the agent said;
//   - a message a human typed (humanTextEntry below), under an allow-list of its own.
//
// Everything else the record holds — tool calls and results, reasoning, a
// sub-agent's entries, summaries, system and hook output, attachments, images,
// the runtime's own markup — is out, whole. Those kinds exist in the wire type
// (fleet.RecordKind) and a later change fills them; until then they are absent
// from every page, not empty. An entry that cannot be classified with certainty
// is left out, never guessed at: the file belongs to the runtime and its shape
// can change in a release, so drift costs a missing entry, not a leaked
// category.
//
// # The cursor
//
// The same machinery as turns (readOffsetEntries): an offset into the append-only
// conversation file, bound to the conversation. Its version tag differs
// (recordCursorVersion), so a turns cursor handed to this route — or the other
// way round — is refused as a cursor this route did not issue rather than
// silently read as a position in the wrong shape of page.

var _ driver.RecordReader = (*Driver)(nil)

// recordCursorVersion tells this route's cursor from turns': a cursor is a
// position in a route's own page sequence, and the two sequences differ (the
// same offset yields different entries), so neither route accepts the other's.
const recordCursorVersion = "r1"

// Record implements driver.RecordReader.
func (d *Driver) Record(ctx context.Context, req fleet.Request, ref fleet.SessionRef, q fleet.RecordQuery) (fleet.RecordPage, error) {
	limit := q.Limit
	if limit == 0 {
		limit = fleet.DefaultRecordLimit
	}
	if limit < 0 || limit > fleet.MaxRecordLimit {
		return fleet.RecordPage{}, fmt.Errorf("limit must be between 1 and %d", fleet.MaxRecordLimit)
	}

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	src, _, err := d.sessionRecordSource(ctx, req, ref)
	if err != nil {
		return fleet.RecordPage{}, err
	}
	return readRecord(src.path, q.Since, limit)
}

// readRecord answers one query against one record file. Split from Record so
// the reading — the part that holds the boundary — is testable without a
// multiplexer.
func readRecord(path, since string, limit int) (fleet.RecordPage, error) {
	msgs, next, err := readOffsetEntries(path, since, recordCursorVersion, limit, recordMessageOf)
	if err != nil {
		return fleet.RecordPage{}, err
	}
	if msgs == nil {
		msgs = []fleet.RecordMessage{}
	}
	return fleet.RecordPage{Messages: msgs, Next: next, Format: fleet.RecordFormat}, nil
}

// recordMessageOf is the allow-list for this runtime's record: one line in, a
// message out only when the line is an assistant text entry (assistantEntry) or
// a human-typed user text entry (humanTextEntry).
func recordMessageOf(line []byte) (fleet.RecordMessage, bool) {
	if at, text, model, truncated, ok := assistantEntry(line); ok {
		return fleet.RecordMessage{
			At: at, Kind: fleet.RecordKindText, Role: fleet.RecordRoleAssistant,
			Text: text, Model: model, Truncated: truncated,
		}, true
	}
	if at, text, truncated, ok := humanTextEntry(line); ok {
		return fleet.RecordMessage{
			At: at, Kind: fleet.RecordKindText, Role: fleet.RecordRoleUser,
			Text: text, Truncated: truncated,
		}, true
	}
	return fleet.RecordMessage{}, false
}

// runtimeMarkupPrefixes open a user-typed entry that is not a person's message:
// the runtime recording a slash command or its output, a bash-mode line, an
// injected reminder or hook output, or the note that a turn was interrupted.
// That is the runtime speaking in the user's voice, and not in this issue's
// shape. A later kind (fleet.RecordKindSystem) may carry some of it.
var runtimeMarkupPrefixes = []string{
	"<command-name>",
	"<command-message>",
	"<local-command-",
	"<bash-",
	"<system-reminder>",
	"<user-prompt-submit-hook>",
	"[Request interrupted by user",
}

// humanTextEntry is the allow-list for what a person typed. ok is true only for an
// entry the runtime wrote as a top-level user message, authored by a human at
// the keyboard (or queued there), whose content is text and only text.
//
// Every field the check reads must have the shape this reader expects; one that
// does not decode fails the whole entry. Fields it has never seen set are not
// consulted, which is why each category it excludes is named: an unfamiliar
// flag is not a reason to admit.
func humanTextEntry(line []byte) (at time.Time, text string, truncated, ok bool) {
	var e struct {
		Type                      string          `json:"type"`
		Timestamp                 string          `json:"timestamp"`
		IsSidechain               bool            `json:"isSidechain"`
		IsMeta                    bool            `json:"isMeta"`
		IsCompactSummary          bool            `json:"isCompactSummary"`
		IsVisibleInTranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
		ToolUseResult             json.RawMessage `json:"toolUseResult"`
		Origin                    json.RawMessage `json:"origin"`
		PromptSource              string          `json:"promptSource"`
		Message                   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return time.Time{}, "", false, false
	}
	if e.Type != "user" || e.Message.Role != "user" {
		return time.Time{}, "", false, false
	}
	// A sub-agent's prompt, a meta or compaction entry, and anything shown only
	// in the runtime's own viewer are not a person speaking to this session.
	if e.IsSidechain || e.IsMeta || e.IsCompactSummary || e.IsVisibleInTranscriptOnly {
		return time.Time{}, "", false, false
	}
	// A tool's result is recorded as a user entry that carries toolUseResult.
	if !jsonAbsent(e.ToolUseResult) {
		return time.Time{}, "", false, false
	}
	// Another session or the runtime sending in is not a human typing; format 1
	// has no field to attribute it, so it stays out.
	if !jsonAbsent(e.Origin) {
		var origin struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(e.Origin, &origin); err != nil || (origin.Kind != "" && origin.Kind != "human") {
			return time.Time{}, "", false, false
		}
	}
	if e.PromptSource != "" && e.PromptSource != "typed" && e.PromptSource != "queued" {
		return time.Time{}, "", false, false
	}
	ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return time.Time{}, "", false, false
	}

	var body string
	if err := json.Unmarshal(e.Message.Content, &body); err == nil {
		text = body
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(e.Message.Content, &blocks); err != nil || len(blocks) == 0 {
			return time.Time{}, "", false, false
		}
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			// A tool_result, image, document or unknown block drops the whole
			// entry: this issue reads text, and a message that mixes text with
			// another kind is neither one nor the other until that kind is read.
			if b.Type != "text" {
				return time.Time{}, "", false, false
			}
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n\n")
	}

	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return time.Time{}, "", false, false
	}
	for _, prefix := range runtimeMarkupPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return time.Time{}, "", false, false
		}
	}
	text, truncated = capText(text, fleet.MaxRecordFieldBytes)
	return ts.UTC(), text, truncated, true
}

// jsonAbsent reports whether a raw JSON field was missing or null.
func jsonAbsent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}
