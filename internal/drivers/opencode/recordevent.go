package opencode

import (
	"context"
	"time"

	fleet "github.com/futurelastic/muster"
)

// session.record on this driver's stream (muster #444): a session's record grew,
// said with a cursor and never content.
//
// # Where growth is noticed
//
// In the subscription, off the runtime's own bus, which pushes the change itself.
// Five events mean the record moved: message.updated, message.removed,
// message.part.updated, message.part.removed and message.part.delta — every one
// carries the session under properties.sessionID (read off a real 1.18.3 server's
// OpenAPI document). They are hints, not payloads: nothing about a message is
// decoded here, so a release that reshapes a part costs nothing.
//
// # The window
//
// The first hint for a session opens a window of recordWindow; further hints
// inside it add nothing; at its end one event is minted. So a model streaming
// tokens is one event per window, never one per token and never starved by a
// debounce that keeps being reset. Two events for one session are never closer
// than a window apart, and a client must not rely on the number.
//
// # The cursor
//
// What Record(since="") answers as `next` at the end of the window, read by
// recordHead, the same code Record runs. The runtime's record is read in the
// driver and discarded; only the cursor leaves. This cursor is deterministic (it
// is a position in the message list, with no clock in it), so an event whose
// cursor equals the last one this subscription sent for the session is dropped:
// a token streaming into a part that has not settled changes nothing a client
// could read, and a client told to pull would pull an empty page. The first window
// of a session in a subscription has no earlier cursor to compare with and is
// always sent.
//
// A session that is parked, gone or unreadable at the window's end produces no
// event: there is no cursor to give, and the next hint opens the next window.

// recordWindow is the longest a session's growth waits to be said. It is short
// enough that a viewer follows a live session, and long enough that a burst of
// part updates is one read.
const recordWindow = 250 * time.Millisecond

// recordGrew opens a session's window if none is open.
func (s *subscription) recordGrew(ref fleet.SessionRef) {
	s.mu.Lock()
	t := s.track[ref.ID]
	if t == nil {
		t = &sessionTrack{}
		s.track[ref.ID] = t
	}
	if t.recordArmed {
		s.mu.Unlock()
		return
	}
	t.recordArmed = true
	s.mu.Unlock()
	time.AfterFunc(recordWindow, func() { s.recordWindowEnd(ref) })
}

// recordWindowEnd closes a session's window: it mints the cursor and says so.
func (s *subscription) recordWindowEnd(ref fleet.SessionRef) {
	// Disarmed BEFORE the read, so a hint that arrives while the runtime is being
	// asked opens the next window instead of being absorbed into one whose read
	// may already have missed it.
	s.mu.Lock()
	if t := s.track[ref.ID]; t != nil {
		t.recordArmed = false
	}
	s.mu.Unlock()

	if s.ctx.Err() != nil {
		return
	}
	if _, ok := s.d.wasSeen(ref.ID); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, busReadTimeout)
	cursor, started, err := s.d.recordHead(ctx, ref)
	cancel()
	if err != nil || cursor == "" || s.ctx.Err() != nil {
		return
	}

	s.mu.Lock()
	t := s.track[ref.ID]
	if t == nil {
		t = &sessionTrack{}
		s.track[ref.ID] = t
	}
	if t.recordLast == cursor {
		s.mu.Unlock()
		return
	}
	t.recordLast = cursor
	s.mu.Unlock()

	s.push(fleet.Event{
		Machine: s.d.machine,
		Kind:    fleet.EventSessionRecord,
		Payload: fleet.SessionRecordPayload{Ref: ref, StartedAt: &started, Cursor: cursor},
	})
}
