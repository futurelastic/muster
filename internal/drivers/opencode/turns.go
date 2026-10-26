package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Reading back what a session's agent wrote (muster #258, #369).
//
// # The boundary, and where it is enforced
//
// docs/adr/258-assistant-turns-read.md narrows the #82 refusal to exactly one
// data class: the text the session's own agent produced. The line is drawn
// HERE, in assistantTurn, and nowhere downstream: the service, the relay and the
// wire type carry whatever this returns without looking at it, so a category
// that reaches them has already been let through. It is an allow-list that
// fails closed — a message is a turn only when the runtime's own fields say it
// is a finished, error-free, top-level assistant message from a real model, and
// only its plain text parts are kept. Reasoning, tool calls and results, files,
// patches, snapshots, step records, the runtime's synthetic and ignored text,
// compaction summaries, errored messages, user messages and any part type this
// code has never seen contribute nothing. Anything it cannot classify with
// certainty is left out, never guessed at.
//
// This file is also the ONLY place in the package that decodes a part's text.
// wireMessageInfo and wirePart (client.go) deliberately do not, which is what
// keeps state and usage unable to read content by construction; the wire types
// below are separate for that reason, not by accident.
//
// # One turn per assistant message
//
// A turn is one assistant MESSAGE of the runtime's record, its text parts joined
// by a blank line in the order the runtime wrote them. That is the unit ADR 258
// names ("one top-level assistant entry") and the one the terminal driver
// returns, so a consumer reads both runtimes alike. It is not one turn per user
// prompt: measured against a live runtime, a prompt that uses a tool is answered
// by TWO assistant messages (one per model round), and each is a turn here. The
// last turn is still the final report, and a caller that wants the whole reply
// concatenates.
//
// # The cursor
//
// The runtime pages its message list from the tail with `limit` and backwards
// with an opaque `before`; it has no "after". So `since` cannot be an offset: it
// names the last message already consumed, and a read finds that message again by
// widening a tail window until it is inside it. The token carries the session id
// and the session's own start time as well, so a cursor presented against another
// conversation is told apart from one whose anchor the record no longer holds.
// Both are the caller's stale belief, and both are ErrAmbiguousTarget.
//
// # A message still being written
//
// An assistant message without a completion time is in flight. It is never
// returned partial: the scan stops before it, `next` stays where it was, and the
// message is returned once, whole, by a later read. It is passed over (consumed,
// never returned) only when a LATER assistant message exists — the runtime is
// sequential within a session, so the earlier one can then only be a remnant of a
// cut-off turn (muster #282). A trailing remnant therefore stays held until the
// next turn happens, which fails closed and needs no status read, so there is no
// race between reading the messages and reading the status.

const (
	// turnsCursorVersion lets a later cursor shape be told from this one rather
	// than misread by it.
	turnsCursorVersion = "1"

	// turnsWindowStart is the first tail window a read asks the runtime for, in
	// messages. It grows by turnsWindowGrowth until it holds what the read needs;
	// past turnsWindowMax the whole record is read once (the same first-read cost
	// usage.go already pays), because the wire has no way to page forward.
	turnsWindowStart  = 32
	turnsWindowGrowth = 4
	turnsWindowMax    = 2048
)

// Advertised the way the terminal driver advertises it: the service type-asserts
// this interface, and a runtime that does not implement it answers `unsupported`.
var _ driver.TurnReader = (*Driver)(nil)

// Turns implements driver.TurnReader.
func (d *Driver) Turns(ctx context.Context, req fleet.Request, ref fleet.SessionRef, q fleet.TurnsQuery) (fleet.TurnsPage, error) {
	limit := q.Limit
	if limit == 0 {
		limit = fleet.DefaultTurnsLimit
	}
	if limit < 0 || limit > fleet.MaxTurnsLimit {
		return fleet.TurnsPage{}, fmt.Errorf("limit must be between 1 and %d", fleet.MaxTurnsLimit)
	}

	known, ok := d.wasSeen(ref.ID)
	if !ok {
		return fleet.TurnsPage{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if err := d.requireLive(known); err != nil {
		return fleet.TurnsPage{}, err
	}
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	srv := known.srv

	// §5.4, before anything is read: a recycled id must not hand one session's
	// words to a caller who meant another's. The session is read live, not from
	// the cache, because the cursor is bound to its start time too.
	var sess wireSession
	if err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(ref.ID), nil, &sess); err != nil {
		if isNotFound(err) {
			return fleet.TurnsPage{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return fleet.TurnsPage{}, err
	}
	created := time.UnixMilli(sess.Time.Created)
	if want := req.Expect.StartedAt; want != nil && !created.Equal(*want) {
		return fleet.TurnsPage{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			fleet.ErrAmbiguousTarget, ref.ID, created.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	var anchor string
	if q.Since != "" {
		a, err := readTurnsCursor(q.Since, ref.ID, created.UnixMilli())
		if err != nil {
			return fleet.TurnsPage{}, err
		}
		anchor = a
	}

	recs, err := d.readTurnRecords(ctx, srv, ref.ID, q.Since != "", anchor, limit)
	if err != nil {
		return fleet.TurnsPage{}, err
	}

	var turns []fleet.Turn
	var next string
	if q.Since != "" {
		from := 0
		if anchor != "" {
			from = indexOfRec(recs, anchor) + 1
		}
		turns, next = scanTurns(recs, from, limit, anchor)
	} else {
		turns, next = scanTurns(recs, 0, 0, "")
		if len(turns) > limit {
			turns = turns[len(turns)-limit:]
		}
	}
	if turns == nil {
		turns = []fleet.Turn{}
	}
	return fleet.TurnsPage{Turns: turns, Next: turnsCursor(ref.ID, created.UnixMilli(), next)}, nil
}

// readTurnRecords reads the smallest tail of the session's message record that
// answers the query, classified. For a read from a cursor that is a tail holding
// the anchor message; for a read of the most recent turns it is a tail holding
// enough of them; a read from the start of the record needs all of it. The window
// widens until it does, and a record that does not hold the cursor's anchor even
// when read whole is the caller's stale belief.
func (d *Driver) readTurnRecords(ctx context.Context, srv *server, id string, fromCursor bool, anchor string, limit int) ([]turnRec, error) {
	window := turnsWindowStart
	if fromCursor && anchor == "" {
		window = 0 // "from the start" is the whole record
	}
	for {
		path := "/session/" + url.PathEscape(id) + "/message"
		if window > 0 {
			path += "?limit=" + strconv.Itoa(window)
		}
		var raw []json.RawMessage
		if err := d.do(ctx, srv, "GET", path, nil, &raw); err != nil {
			if isNotFound(err) {
				return nil, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, id)
			}
			return nil, err
		}
		recs := make([]turnRec, 0, len(raw))
		for _, r := range raw {
			recs = append(recs, classifyMessage(r))
		}
		whole := window == 0 || len(raw) < window

		switch {
		case fromCursor && anchor != "":
			if indexOfRec(recs, anchor) >= 0 {
				return recs, nil
			}
			if whole {
				return nil, fmt.Errorf(
					"%w: the cursor points at a message this session's record no longer holds (it was deleted or reverted since); read without `since` to start again",
					fleet.ErrAmbiguousTarget)
			}
		case whole:
			return recs, nil
		case !fromCursor:
			if turns, _ := scanTurns(recs, 0, 0, ""); len(turns) >= limit {
				return recs, nil
			}
		}

		window *= turnsWindowGrowth
		if window > turnsWindowMax {
			window = 0
		}
	}
}

// indexOfRec is the position of the message with this id, or -1.
func indexOfRec(recs []turnRec, id string) int {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].id == id {
			return i
		}
	}
	return -1
}

// turnRec is what a read keeps of one message: enough to place it and, when it
// is a turn, the turn. No other content survives classification.
type turnRec struct {
	id        string // empty when the runtime did not give a readable one
	assistant bool
	held      bool // an assistant message the runtime has not finished
	turn      *fleet.Turn
}

// scanTurns walks recs from index `from` and returns the turns in them, oldest
// first, and the id of the last message it consumed (anchor when it consumed
// none). max > 0 stops after that many turns, with next at the message of the
// last one so the following page resumes exactly after it. A message still being
// written stops the walk before it (see the package comment).
func scanTurns(recs []turnRec, from, max int, anchor string) (turns []fleet.Turn, next string) {
	next = anchor
	lastAssistant := -1
	for i := range recs {
		if recs[i].assistant {
			lastAssistant = i
		}
	}
	for i := from; i < len(recs); i++ {
		r := recs[i]
		if r.held && i == lastAssistant {
			return turns, next
		}
		if r.id != "" {
			next = r.id
		}
		if r.turn != nil {
			turns = append(turns, *r.turn)
			if max > 0 && len(turns) >= max {
				return turns, next
			}
		}
	}
	return turns, next
}

// turnWireInfo is the part of a message's "info" the allow-list reads. Distinct
// from wireMessageInfo on purpose (see the file comment).
type turnWireInfo struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	ModelID string `json:"modelID"`
	// Summary marks the runtime's own compaction summary: written by the runtime
	// to shrink context, not by the agent to be read.
	Summary bool `json:"summary"`
	// Error is kept raw: only its presence matters, and an error's shape is not
	// this reader's to interpret.
	Error json.RawMessage `json:"error"`
	Time  struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// turnWirePart is a text part. Only parts whose type is "text" are ever decoded
// this far.
type turnWirePart struct {
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
}

// classifyMessage reduces one raw message to a turnRec. It is the allow-list.
func classifyMessage(raw json.RawMessage) turnRec {
	// A message whose shape cannot even be read for an id and a role is neither a
	// turn nor an anchor.
	var lenient struct {
		Info struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &lenient); err != nil {
		return turnRec{}
	}
	rec := turnRec{id: lenient.Info.ID, assistant: lenient.Info.Role == "assistant"}
	if !rec.assistant || rec.id == "" {
		return rec
	}

	var m struct {
		Info  turnWireInfo      `json:"info"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return rec
	}
	errored := len(m.Info.Error) > 0 && string(m.Info.Error) != "null"
	if m.Info.Time.Completed == 0 && !errored {
		rec.held = true
		return rec
	}
	if errored || m.Info.Summary || m.Info.ModelID == "" || m.Info.Time.Completed <= 0 {
		return rec
	}

	var texts []string
	for _, p := range m.Parts {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(p, &head); err != nil {
			return rec // a part that cannot be classified: leave the whole message out
		}
		if head.Type != "text" {
			continue
		}
		var tp turnWirePart
		if err := json.Unmarshal(p, &tp); err != nil {
			return rec
		}
		if tp.Synthetic || tp.Ignored || strings.TrimSpace(tp.Text) == "" {
			continue
		}
		texts = append(texts, tp.Text)
	}
	if len(texts) == 0 {
		return rec
	}
	text, truncated := cutTurnText(strings.Join(texts, "\n\n"))
	rec.turn = &fleet.Turn{At: time.UnixMilli(m.Info.Time.Completed).UTC(), Text: text, Truncated: truncated}
	return rec
}

// cutTurnText bounds a turn's text at fleet.MaxTurnTextBytes without splitting a
// character.
func cutTurnText(text string) (string, bool) {
	if len(text) <= fleet.MaxTurnTextBytes {
		return text, false
	}
	cut := fleet.MaxTurnTextBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

func turnsCursor(sessionID string, createdMs int64, anchor string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(
		turnsCursorVersion + "|" + sessionID + "|" + strconv.FormatInt(createdMs, 10) + "|" + anchor))
}

// readTurnsCursor returns the anchor message a cursor names, provided it was
// issued against this same session, started at this same time.
func readTurnsCursor(cursor, sessionID string, createdMs int64) (string, error) {
	invalid := errors.New("since is not a cursor this API issued")
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", invalid
	}
	parts := strings.SplitN(string(raw), "|", 4)
	if len(parts) != 4 || parts[0] != turnsCursorVersion {
		return "", invalid
	}
	if _, err := strconv.ParseInt(parts[2], 10, 64); err != nil {
		return "", invalid
	}
	if parts[1] != sessionID || parts[2] != strconv.FormatInt(createdMs, 10) {
		return "", fmt.Errorf(
			"%w: the cursor was issued for another conversation; this id now holds a different session. Read without `since` to start again",
			fleet.ErrAmbiguousTarget)
	}
	return parts[3], nil
}
