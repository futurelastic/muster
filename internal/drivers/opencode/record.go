package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Reading back a session's record (muster #443, on the route of #441 and the kinds
// of #442).
//
// # The boundary, and where it is enforced
//
// What may be read is decided in recordclassify.go and nowhere downstream: this file
// walks and pages what that allow-list returns, and the service, the relay and the
// wire type carry it without looking.
//
// # The cursor
//
// The runtime pages its message list from the tail with `limit` and backwards with an
// opaque `before`; it has no "after". So `since` cannot be an offset: it names a
// position inside one message, and a read finds that message again by widening a tail
// window until it is inside it (the way turns does, turns.go). The cursor carries the
// session id and its start, so one presented against another conversation is told
// apart from one whose message the record no longer holds; both are the caller's stale
// belief, and both are ErrAmbiguousTarget.
//
// The position is: the message, how many of its parts are consumed, how many of the
// entries of the next part were sent (a completed call that returned images is one
// part and several entries, and a page may end between them), and the calls of that
// message that went out `running` and have not gone out again.
//
// # A message still being written
//
// An assistant message without a completion time is in flight. Unlike turns, which
// returns it whole once it is done, the record returns its SETTLED PARTS as they
// settle, so a viewer of a live session sees what the agent said and is doing now. A
// part settles when the runtime has written its end or has moved on to the next part;
// nothing past an unsettled part is read.
//
// A tool call goes out as soon as the runtime says it is running, with status
// "running", and its id is kept in the cursor. When a later read finds the call
// finished, the page carries that same message again, complete: the same `tool.id`,
// the call's own `at`, in front of whatever is new. The rule for a client is the one
// ADR 442 gives: a `tool` message whose `tool.id` you already hold replaces it. The
// cursor never leaves a message while one of its calls is pending, so the set it
// carries is the running calls of one model round and cannot grow without bound.
//
// A message the runtime never finished (a cut-off turn, muster #282) is passed over
// only when a LATER assistant message exists: the runtime is sequential within a
// session, so the earlier one can then only be a remnant. A call in it that never got
// an outcome goes out `error`, with no result, which is what an interrupted call is
// everywhere else in this record. A trailing unfinished message stays held, which
// fails closed and needs no status read.

const (
	recordCursorVersion = "o1"

	// recordMaxPending bounds the calls one cursor may carry, and so how much a
	// presented cursor can make a read do.
	recordMaxPending = 64
)

var (
	_ driver.RecordReader      = (*Driver)(nil)
	_ driver.RecordMediaReader = (*Driver)(nil)
)

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
	srv, created, err := d.openRead(ctx, req, ref, "record")
	if err != nil {
		return fleet.RecordPage{}, err
	}
	return d.recordPage(ctx, srv, ref, created, q, limit)
}

// recordHead is the cursor Record(since="") answers as `next` right now, read the
// way Record reads it and without a caller's request: what the stream mints a
// session.record event's cursor from (muster #444). One code path decides what a
// cursor means, so the event's cursor resumes after the last entry by construction.
// The page it reads is discarded; only the cursor and the session's start leave.
func (d *Driver) recordHead(ctx context.Context, ref fleet.SessionRef) (cursor string, started time.Time, err error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	srv, created, err := d.openRead(ctx, fleet.Request{}, ref, "record")
	if err != nil {
		return "", time.Time{}, err
	}
	page, err := d.recordPage(ctx, srv, ref, created, fleet.RecordQuery{}, 1)
	if err != nil {
		return "", time.Time{}, err
	}
	return page.Next, created, nil
}

// recordPage is the read Record and recordHead share, after the session has been
// opened and corroborated and the limit validated.
func (d *Driver) recordPage(ctx context.Context, srv *server, ref fleet.SessionRef, created time.Time, q fleet.RecordQuery, limit int) (fleet.RecordPage, error) {
	sc := recordScope{session: ref.ID, created: created.UnixMilli()}

	var cur recordCursor
	if q.Since != "" {
		c, err := readRecordCursor(q.Since, sc)
		if err != nil {
			return fleet.RecordPage{}, err
		}
		cur = c
	}

	recs, err := d.readRecordMessages(ctx, srv, sc, q.Since != "", cur.M, limit)
	if err != nil {
		return fleet.RecordPage{}, err
	}

	var (
		msgs []fleet.RecordMessage
		next recordCursor
	)
	if q.Since != "" {
		msgs, next, err = walkRecord(recs, cur, limit, sc)
		if err != nil {
			return fleet.RecordPage{}, err
		}
	} else {
		msgs, next, _ = walkRecord(recs, recordCursor{P: -1}, -1, sc)
		if len(msgs) > limit {
			msgs = msgs[len(msgs)-limit:]
		}
	}
	if msgs == nil {
		msgs = []fleet.RecordMessage{}
	}
	return fleet.RecordPage{Messages: msgs, Next: next.encode(sc), Format: fleet.RecordFormat}, nil
}

// openRead is what a turns read, a record read and a media read all do before they
// read anything: find the session this driver holds, refuse one that is parked or not
// running, and — §5.4 — read it live so that a recycled id does not hand one
// session's words to a caller who meant another's. The caller bounds ctx.
func (d *Driver) openRead(ctx context.Context, req fleet.Request, ref fleet.SessionRef, what string) (*server, time.Time, error) {
	known, ok := d.wasSeen(ref.ID)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if err := d.requireLive(known); err != nil {
		return nil, time.Time{}, err
	}
	if d.phaseOf(known.srv) == phaseParked {
		// muster #421: the history lives in the runtime's store, which only its process
		// reads, and a read does not start one (a poll would keep the session awake).
		return nil, time.Time{}, d.parkedError(what)
	}
	srv := known.srv

	var sess wireSession
	if err := d.do(ctx, srv, "GET", "/session/"+url.PathEscape(ref.ID), nil, &sess); err != nil {
		if isNotFound(err) {
			return nil, time.Time{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
		}
		return nil, time.Time{}, err
	}
	created := time.UnixMilli(sess.Time.Created)
	if want := req.Expect.StartedAt; want != nil && !created.Equal(*want) {
		return nil, time.Time{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			fleet.ErrAmbiguousTarget, ref.ID, created.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	return srv, created, nil
}

// readRecordMessages reads the smallest tail of the session's message record that
// answers the query, classified. For a read from a cursor that is a tail holding the
// cursor's message; for a read of the most recent entries it is a tail holding enough
// of them; a read from the start of the record needs all of it. The window widens until
// it does, and a record that does not hold the cursor's message even when read whole
// is the caller's stale belief.
func (d *Driver) readRecordMessages(ctx context.Context, srv *server, sc recordScope, fromCursor bool, anchor string, limit int) ([]recordMessage, error) {
	window := turnsWindowStart
	if fromCursor && anchor == "" {
		window = 0 // "from the start" is the whole record
	}
	for {
		path := "/session/" + url.PathEscape(sc.session) + "/message"
		if window > 0 {
			path += "?limit=" + strconv.Itoa(window)
		}
		var raw []json.RawMessage
		if err := d.do(ctx, srv, "GET", path, nil, &raw); err != nil {
			if isNotFound(err) {
				return nil, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, sc.session)
			}
			return nil, err
		}
		recs := make([]recordMessage, 0, len(raw))
		for _, r := range raw {
			recs = append(recs, classifyRecordMessage(r, sc))
		}
		whole := window == 0 || len(raw) < window

		switch {
		case fromCursor && anchor != "":
			if indexOfRecord(recs, anchor) >= 0 {
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
			if msgs, _, _ := walkRecord(recs, recordCursor{P: -1}, -1, sc); len(msgs) >= limit {
				return recs, nil
			}
		}

		window *= turnsWindowGrowth
		if window > turnsWindowMax {
			window = 0
		}
	}
}

// indexOfRecord is the position of the message with this id, or -1.
func indexOfRecord(recs []recordMessage, id string) int {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].id == id {
			return i
		}
	}
	return -1
}

// recordCursor is a position in the record: everything before it has been sent.
type recordCursor struct {
	// M is the message the position is in. Empty means before the first message.
	M string `json:"m"`
	// P is the number of M's parts consumed; -1 means M is done and the position is
	// just after it.
	P int `json:"p"`
	// K is how many of the entries of part P were sent already.
	K int `json:"k,omitempty"`
	// W is the calls of M that went out `running` and have not gone out complete.
	W []recordPending `json:"w,omitempty"`
}

// recordPending is a call that went out running. K is how many entries of its
// finished form (the call, then its images) went out already.
type recordPending struct {
	ID string `json:"i"`
	K  int    `json:"k,omitempty"`
}

type recordCursorWire struct {
	V string `json:"v"`
	S string `json:"s"`
	C int64  `json:"c"`
	recordCursor
}

func (c recordCursor) encode(sc recordScope) string {
	b, _ := json.Marshal(recordCursorWire{V: recordCursorVersion, S: sc.session, C: sc.created, recordCursor: c})
	return base64.RawURLEncoding.EncodeToString(b)
}

// readRecordCursor returns the position a cursor names, provided it was issued
// against this same session, started at this same time. A value that is not a record
// cursor — including a turns cursor — is a plain error, which the service maps to
// `invalid`.
func readRecordCursor(cursor string, sc recordScope) (recordCursor, error) {
	invalid := errors.New("since is not a cursor this API issued for the record")
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return recordCursor{}, invalid
	}
	var w recordCursorWire
	if json.Unmarshal(raw, &w) != nil || w.V != recordCursorVersion {
		return recordCursor{}, invalid
	}
	c := w.recordCursor
	if c.P < -1 || c.K < 0 || len(c.W) > recordMaxPending || (c.M == "" && (c.P != -1 || c.K != 0 || len(c.W) != 0)) {
		return recordCursor{}, invalid
	}
	for _, p := range c.W {
		if p.ID == "" || len(p.ID) > maxCallIDBytes || p.K < 0 {
			return recordCursor{}, invalid
		}
	}
	if w.S != sc.session || w.C != sc.created {
		return recordCursor{}, fmt.Errorf(
			"%w: the cursor was issued for another conversation; this id now holds a different session. Read without `since` to start again",
			fleet.ErrAmbiguousTarget)
	}
	return c, nil
}

// stale is the error for a position the record does not hold any more.
func staleRecordCursor() error {
	return fmt.Errorf(
		"%w: the cursor points at a position this session's record no longer holds (a message was edited since); read without `since` to start again",
		fleet.ErrAmbiguousTarget)
}

// walkRecord returns the entries of recs from the position `from`, oldest first, at
// most max of them (max < 0 means all of them), and the position after the last entry
// it returned. A message still being written stops the walk after its settled parts.
func walkRecord(recs []recordMessage, from recordCursor, max int, sc recordScope) ([]fleet.RecordMessage, recordCursor, error) {
	w := recordWalker{max: max}
	pos := from

	start := 0
	if from.M != "" {
		i := indexOfRecord(recs, from.M)
		if i < 0 {
			return nil, from, staleRecordCursor()
		}
		if from.P == -1 {
			start = i + 1
		} else {
			start = i
		}
	}

	lastAssistant := -1
	for i := range recs {
		if recs[i].assistant {
			lastAssistant = i
		}
	}

	for i := start; i < len(recs); i++ {
		m := &recs[i]
		if m.id == "" {
			continue
		}
		if pos.M != m.id {
			pos = recordCursor{M: m.id}
		}
		// An unfinished assistant message is a remnant when a later assistant message
		// exists, and in flight when it is the last one.
		final := m.finished() || i < lastAssistant
		done, full, err := w.message(m, final, &pos)
		if err != nil {
			return nil, from, err
		}
		if full || !done {
			break
		}
		pos = recordCursor{M: m.id, P: -1}
	}
	if w.out == nil {
		w.out = []fleet.RecordMessage{}
	}
	return w.out, pos, nil
}

// recordWalker accumulates the entries of one page.
type recordWalker struct {
	out []fleet.RecordMessage
	max int
}

// room reports whether another entry fits.
func (w *recordWalker) room() bool { return w.max < 0 || len(w.out) < w.max }

// emit appends the entries of group from index k on while there is room and returns
// the new k.
func (w *recordWalker) emit(group []fleet.RecordMessage, k int) int {
	for k < len(group) && w.room() {
		w.out = append(w.out, group[k])
		k++
	}
	return k
}

// message walks one message from the position in pos, which it advances as it goes.
// done reports that the message is finished and fully consumed; full that the page
// ran out of room.
func (w *recordWalker) message(m *recordMessage, final bool, pos *recordCursor) (done, full bool, err error) {
	if pos.P > len(m.parts) {
		return false, false, staleRecordCursor()
	}

	// Calls that went out running and are still owed: where one has finished, it goes
	// out again, complete, in front of what is new.
	for i := 0; i < len(pos.W); {
		pend := &pos.W[i]
		part := findCall(m, pend.ID)
		if part == nil {
			pos.W = append(pos.W[:i], pos.W[i+1:]...) // the record no longer holds it: let it go
			continue
		}
		var group []fleet.RecordMessage
		switch {
		case part.terminal() && m.admitted:
			group = part.group()
		case final || !m.admitted:
			group = []fleet.RecordMessage{part.interrupted()}
		default:
			i++ // still running
			continue
		}
		if !w.room() {
			return false, true, nil
		}
		pend.K = w.emit(group, pend.K)
		if pend.K < len(group) {
			return false, true, nil
		}
		pos.W = append(pos.W[:i], pos.W[i+1:]...)
	}

	if !m.admitted {
		// Not readable, and nothing more is owed. One the runtime is still writing is
		// held, as turns holds it, until it is known what it is.
		return final, false, nil
	}

	for pos.P < len(m.parts) {
		part := &m.parts[pos.P]
		if part.kind == partSkip {
			pos.P, pos.K = pos.P+1, 0
			continue
		}
		if !part.settled && !final {
			return false, false, nil // the runtime is still writing it
		}
		if !w.room() {
			return false, true, nil
		}
		switch part.kind {
		case partEntry:
			w.emit([]fleet.RecordMessage{part.entry}, 0)
		case partTool:
			switch {
			case part.terminal():
				group := part.group()
				if pos.K > len(group) {
					return false, false, staleRecordCursor()
				}
				pos.K = w.emit(group, pos.K)
				if pos.K < len(group) {
					return false, true, nil
				}
			case final:
				w.emit([]fleet.RecordMessage{part.interrupted()}, 0)
			case part.status == "running":
				if len(pos.W) >= recordMaxPending {
					return false, false, nil // more calls at once than a cursor can carry: wait
				}
				w.emit([]fleet.RecordMessage{part.running()}, 0)
				pos.W = append(pos.W, recordPending{ID: part.callID})
			default:
				return false, false, nil // pending: its input is still arriving
			}
		}
		pos.P, pos.K = pos.P+1, 0
	}
	// Every part is consumed. A message the runtime has not finished stays the
	// position, and so does one with a call still owed.
	return final && len(pos.W) == 0, false, nil
}

// findCall returns the tool part of m with this call id, or nil.
func findCall(m *recordMessage, id string) *recordPart {
	for i := range m.parts {
		if m.parts[i].kind == partTool && m.parts[i].callID == id {
			return &m.parts[i]
		}
	}
	return nil
}
