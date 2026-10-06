package tmux

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
)

// Reading back what a session's agent wrote (muster #258).
//
// # The boundary, and where it is enforced
//
// docs/adr/258-assistant-turns-read.md narrows the #82 refusal to exactly one
// data class: the text the session's own agent produced. Everything else the
// runtime records in the same file — tool calls and results, the messages sent
// in by a human or another session, system and hook output, reasoning, a
// sub-agent's entries — stays out of every response.
//
// The line is drawn HERE, in assistantTurnText, and nowhere downstream: the
// service, the relay and the wire type all carry whatever this returns without
// looking at it, so a category that reaches them has already been let through.
// The function is therefore an allow-list that fails closed. An entry is kept
// only when it decodes cleanly AND says, in the runtime's own fields, that it is
// a top-level assistant message with a real model and text blocks. Anything
// else — an unfamiliar type, a field of an unexpected shape, a flag this code
// has never seen set — is dropped, never guessed at.
//
// # One turn per entry, not per model message
//
// The runtime writes each content block of one model message as its own line
// (reasoning, then text, then the tool call). A turn here is a line that holds
// the agent's text. A message that said something, called a tool, then said
// something more yields two turns, in order, which is also how the agent's
// words were interleaved with its actions.
//
// # The cursor
//
// An opaque token carrying the conversation it was issued against and a byte
// offset into that conversation's record. The record is append-only, so an
// offset taken at a line boundary stays valid for as long as the file does.
// Both halves are checked on the way back in (readTurnsCursor): a `/clear` or a
// replaced process points the session at another file, and the same offset in
// that file is a different place — the caller is told its belief is stale, as
// for a recycled id, instead of being handed a page from a conversation it
// never saw.

const (
	// turnsCursorVersion lets a later cursor shape be told from this one rather
	// than misread by it.
	turnsCursorVersion = "1"

	// turnsTailStart is the first window tried when reading the most recent
	// turns. It is widened until it holds enough turns or hits turnsTailMax.
	turnsTailStart int64 = 256 << 10

	// turnsTailMax bounds how far back a "most recent N" read goes. A session
	// that has written fewer than N turns inside this window returns what it
	// has, which is the honest answer to "the most recent turns".
	turnsTailMax int64 = 32 << 20

	// turnsScanMax bounds the bytes ONE call reads going forward from a cursor.
	// A cursor far behind a very large record could otherwise make a single
	// request read the whole file; the page it returns still carries a `next`
	// that moves forward, so the caller continues from there.
	turnsScanMax int64 = 32 << 20
)

// Turns implements driver.TurnReader.
func (d *Driver) Turns(ctx context.Context, req fleet.Request, ref fleet.SessionRef, q fleet.TurnsQuery) (fleet.TurnsPage, error) {
	limit := q.Limit
	if limit == 0 {
		limit = fleet.DefaultTurnsLimit
	}
	if limit < 0 || limit > fleet.MaxTurnsLimit {
		return fleet.TurnsPage{}, fmt.Errorf("limit must be between 1 and %d", fleet.MaxTurnsLimit)
	}

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.TurnsPage{}, err
	}
	var target *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		return fleet.TurnsPage{}, d.noSuchSession(ctx, rows, ref.ID)
	}
	// §5.4, before anything is read: a recycled id must not hand one session's
	// words to a caller who meant another's.
	if want := req.Expect.StartedAt; want != nil && !target.created.Equal(*want) {
		return fleet.TurnsPage{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			fleet.ErrAmbiguousTarget, ref.ID, target.created.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	src, ok := d.resolveTranscriptSource(ctx, ref, target)
	if !ok {
		return fleet.TurnsPage{}, fmt.Errorf("%w: %q", fleet.ErrNoTurnRecord, ref.ID)
	}
	page, err := readTurns(src.path, q.Since, limit)
	if err != nil {
		return page, err
	}
	// The record is read BEFORE the screen, on purpose (muster #266): a prompt
	// answered between the two reads then costs this call a pending entry it
	// would have had, where the other order would show the same text twice, once
	// as pending and once as the turn the answer just made the runtime write.
	if scr, ok := d.captureForClassify(ctx, target.paneID); ok {
		if text, truncated, nonce, ok := pendingAgentText(scr); ok {
			page.Pending = &fleet.PendingTurn{
				ObservedAt: d.now().UTC(),
				Text:       text,
				Truncated:  truncated,
				Source:     fleet.PendingSourceScreen,
				Nonce:      nonce,
			}
		}
	}
	return page, nil
}

// readTurns answers one query against one record file. Split from Turns so the
// reading — the part that holds the boundary — is testable without a
// multiplexer.
func readTurns(path, since string, limit int) (fleet.TurnsPage, error) {
	conv := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	f, err := os.Open(path)
	if err != nil {
		return fleet.TurnsPage{}, fmt.Errorf("%w: %v", fleet.ErrNoTurnRecord, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fleet.TurnsPage{}, fmt.Errorf("%w: %v", fleet.ErrNoTurnRecord, err)
	}
	size := info.Size()

	if since != "" {
		off, err := readTurnsCursor(since, conv)
		if err != nil {
			return fleet.TurnsPage{}, err
		}
		if off > size {
			return fleet.TurnsPage{}, fmt.Errorf(
				"%w: the cursor points past the end of this session's record (it was truncated or replaced since); read without `since` to start again",
				fleet.ErrAmbiguousTarget)
		}
		to := size
		if size-off > turnsScanMax {
			to = off + turnsScanMax
		}
		found, consumed, err := scanAssistantTurns(f, off, to, limit)
		if err != nil {
			return fleet.TurnsPage{}, err
		}
		return pageOf(found, conv, consumed), nil
	}

	// The most recent `limit` turns: widen a window back from the end until it
	// holds enough, the start of the file, or the ceiling.
	window := turnsTailStart
	for {
		start := size - window
		if start < 0 {
			start = 0
		}
		start, err = alignToLine(f, start)
		if err != nil {
			return fleet.TurnsPage{}, err
		}
		found, consumed, err := scanAssistantTurns(f, start, size, 0)
		if err != nil {
			return fleet.TurnsPage{}, err
		}
		if len(found) >= limit || start == 0 || window >= turnsTailMax {
			if len(found) > limit {
				found = found[len(found)-limit:]
			}
			return pageOf(found, conv, consumed), nil
		}
		window *= 4
		if window > turnsTailMax {
			window = turnsTailMax
		}
	}
}

func pageOf(found []foundTurn, conv string, consumed int64) fleet.TurnsPage {
	page := fleet.TurnsPage{Turns: make([]fleet.Turn, 0, len(found)), Next: turnsCursor(conv, consumed)}
	for _, t := range found {
		page.Turns = append(page.Turns, t.turn)
	}
	return page
}

type foundTurn struct {
	turn fleet.Turn
	end  int64 // offset just past this turn's line, including its newline
}

// scanAssistantTurns reads whole lines from [from, to) and returns the assistant
// turns among them. consumed is the offset just past the last line it finished
// with: a final line with no newline yet is the runtime mid-append and is left
// for the next read, never half-parsed. When max > 0 it stops after that many
// turns and consumed is the end of the last one, so the next page resumes
// exactly after it.
func scanAssistantTurns(f *os.File, from, to int64, max int) (found []foundTurn, consumed int64, err error) {
	consumed = from
	if to <= from {
		return nil, consumed, nil
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, from, to-from), 64<<10)
	pos := from
	for {
		line, n, complete, rerr := readBoundedLine(r, recordLineLimit)
		if !complete {
			// EOF with no terminating newline (or a read error): not consumed.
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return found, consumed, rerr
			}
			return found, consumed, nil
		}
		pos += int64(n)
		consumed = pos
		if line != nil {
			if at, text, truncated, ok := assistantTurnText(line); ok {
				found = append(found, foundTurn{turn: fleet.Turn{At: at, Text: text, Truncated: truncated}, end: pos})
				if max > 0 && len(found) >= max {
					return found, consumed, nil
				}
			}
		}
	}
}

// readBoundedLine reads one newline-terminated line. n is the number of bytes
// consumed including the newline. line is nil when the line exceeded limit
// (it is drained and skipped, so one enormous entry cannot stall the reader or
// the memory). complete is false at EOF before a newline.
func readBoundedLine(r *bufio.Reader, limit int) (line []byte, n int, complete bool, err error) {
	var buf []byte
	over := false
	for {
		chunk, rerr := r.ReadSlice('\n')
		n += len(chunk)
		if !over {
			if len(buf)+len(chunk) > limit {
				over = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case rerr == nil:
			if over {
				return nil, n, true, nil
			}
			return buf[:len(buf)-1], n, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		default:
			return nil, n, false, rerr
		}
	}
}

// alignToLine moves start forward to the first byte of a line. start is already
// a line start when the byte before it is a newline (or it is 0).
func alignToLine(f *os.File, start int64) (int64, error) {
	if start <= 0 {
		return 0, nil
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], start-1); err != nil {
		return 0, err
	}
	if b[0] == '\n' {
		return start, nil
	}
	r := bufio.NewReader(io.NewSectionReader(f, start, 1<<62-start))
	skipped := int64(0)
	for {
		chunk, err := r.ReadSlice('\n')
		skipped += int64(len(chunk))
		if err == nil {
			return start + skipped, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		// No newline before EOF: no complete line lies ahead.
		fi, serr := f.Stat()
		if serr != nil {
			return 0, serr
		}
		return fi.Size(), nil
	}
}

// assistantTurnText is the allow-list. ok is true only for an entry the runtime
// wrote as a top-level assistant message that holds text blocks.
func assistantTurnText(line []byte) (at time.Time, text string, truncated, ok bool) {
	var e struct {
		Type              string `json:"type"`
		Timestamp         string `json:"timestamp"`
		IsSidechain       bool   `json:"isSidechain"`
		IsMeta            bool   `json:"isMeta"`
		IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
		Message           struct {
			Role    string          `json:"role"`
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return time.Time{}, "", false, false
	}
	if e.Type != "assistant" || e.Message.Role != "assistant" {
		return time.Time{}, "", false, false
	}
	// A sub-agent's entries share the file's shape and are not this session's
	// agent speaking; the runtime's synthetic notices (an API error, a
	// placeholder) carry the model name "<synthetic>" and are the runtime
	// speaking. Neither is in the ruling.
	if e.IsSidechain || e.IsMeta || e.IsAPIErrorMessage || e.Message.Model == "" || e.Message.Model == "<synthetic>" {
		return time.Time{}, "", false, false
	}
	ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return time.Time{}, "", false, false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
		// A plain-string content, or any shape not a list of blocks, is not
		// something this reader has verified as agent text.
		return time.Time{}, "", false, false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return time.Time{}, "", false, false
	}
	text = strings.Join(parts, "\n\n")
	if len(text) > fleet.MaxTurnTextBytes {
		cut := fleet.MaxTurnTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text, truncated = text[:cut], true
	}
	return ts.UTC(), text, truncated, true
}

func turnsCursor(conv string, offset int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(turnsCursorVersion + "|" + conv + "|" + strconv.FormatInt(offset, 10)))
}

// readTurnsCursor returns the offset a cursor names, provided it was issued
// against this same conversation.
func readTurnsCursor(cursor, conv string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, errors.New("since is not a cursor this API issued")
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 || parts[0] != turnsCursorVersion {
		return 0, errors.New("since is not a cursor this API issued")
	}
	off, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || off < 0 {
		return 0, errors.New("since is not a cursor this API issued")
	}
	if parts[1] != conv {
		return 0, fmt.Errorf(
			"%w: the cursor was issued for another conversation; this session's record has been replaced since (a /clear or a relaunch). Read without `since` to start again",
			fleet.ErrAmbiguousTarget)
	}
	return off, nil
}
