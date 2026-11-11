package tmux

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// session.record on the event stream (muster #444): this driver says a session's
// record grew, with a cursor and never content.
//
// # Where growth is noticed, and why here
//
// In the stream's own pass, next to the diff that already decides session.state.
// That pass is edge-triggered (a control-mode notification wakes it) and paced
// (coalesceWindow, then the duty-paced gap of #415), so noticing growth here adds
// no timer and no wake-up of its own: an idle fleet still costs nothing (§5.5).
// What it adds is one stat of each watched session's conversation file, and one
// listing of its sub-task directory, per pass.
//
// The service is not the place. It holds no record file; making it poll Record
// would read content in the wrong layer and charge an idle machine for the
// question.
//
// # The window
//
// The window IS the pass. A burst of appends between two passes is one event; two
// events for one session are never closer than a pass apart (at least
// coalesceWindow, longer on a machine whose listing is slow). A record that grows
// behind a screen that never repaints is seen on the next pass any notification
// causes — the limit session.state already has, and not papered over with a
// timer.
//
// # The cursor
//
// It is what Record(since="") would answer as `next` at this moment, minted by
// that very reader (readRecordAt). One code path decides what a cursor means, so
// "pass it as `since` and read only what is recorded after this event" holds by
// construction and cannot drift from the route. The read is a bounded tail scan
// whose content stays inside this function: only `next` leaves it.

// recordMark is what the stream last saw of one session's record: enough to say it
// grew, and nothing a client could read.
type recordMark struct {
	conv string // the conversation: a new one is a change a client holding an old cursor must hear
	main int64  // size of the conversation file
	subs string // "id:size" of the sub-task files, in id order
}

// recordMarkOf reads a session's mark and the path of its conversation file. ok is
// false when this driver cannot say anything about the session's record: no record
// root, no conversation identified yet, or no such file. Such a session is neither
// seeded nor reported on — what it will have to say begins when its record exists.
func (d *Driver) recordMarkOf(sess fleet.Session) (mark recordMark, path string, ok bool) {
	if d.conversations == nil || sess.Conversation == nil || !sess.Conversation.Known {
		return recordMark{}, "", false
	}
	path, ok = d.conversations.recordPathWithin(string(sess.Cwd), sess.Conversation.ID)
	if !ok {
		return recordMark{}, "", false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return recordMark{}, "", false
	}

	// The sub-task files a record read follows (a delegated task's own entries are
	// part of the record). The most recent recordSourcesMax, as a read follows
	// them, so a session that delegated hundreds does not turn a pass into a
	// hundred stats.
	subs := listSubTaskFiles(path)
	if len(subs) > recordSourcesMax {
		sort.SliceStable(subs, func(i, j int) bool { return subs[i].mtime.After(subs[j].mtime) })
		subs = subs[:recordSourcesMax]
	}
	sort.SliceStable(subs, func(i, j int) bool { return subs[i].id < subs[j].id })
	var sb strings.Builder
	for i, s := range subs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(s.id + ":" + strconv.FormatInt(s.size, 10))
	}
	return recordMark{conv: sess.Conversation.ID, main: fi.Size(), subs: sb.String()}, path, true
}

// seedRecordMarks takes the baseline for the sessions Subscribe seeded, before it
// returns, for the reason the session baseline is taken there: growth between
// Subscribe returning and the engine's first reading would otherwise be absorbed
// into the baseline and never reported.
func (s *eventStream) seedRecordMarks(seed map[string]fleet.Session) {
	for id, sess := range seed {
		if m, _, ok := s.d.recordMarkOf(sess); ok {
			s.recordMarks[id] = m
		}
	}
}

// noteRecord compares a session's record with what the previous pass saw and says
// so when it grew. firstSight is true for a session this pass announced as
// session.created: that event is its "look now", so what its record held then is
// the baseline, not news.
//
// Owned by run() alone, like quotaBlock and credentialGen: no lock.
func (s *eventStream) noteRecord(ctx context.Context, sess fleet.Session, firstSight bool) {
	mark, path, ok := s.d.recordMarkOf(sess)
	if !ok {
		return
	}
	prev, seen := s.recordMarks[sess.ID]
	s.recordMarks[sess.ID] = mark
	if firstSight || !seen || prev == mark {
		return
	}
	page, err := readRecordAt(path, "", 1, s.d.now())
	if err != nil {
		// The file went away or could not be read this instant. Keep the old mark
		// so the next pass tries again: the growth is not forgotten, only late.
		s.recordMarks[sess.ID] = prev
		return
	}
	s.emit(ctx, fleet.Event{
		Machine: s.d.machine,
		Kind:    fleet.EventSessionRecord,
		Payload: fleet.SessionRecordPayload{
			Ref:       sess.SessionRef,
			StartedAt: sess.StartedAt,
			Cursor:    page.Next,
		},
	})
}
