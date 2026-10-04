package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

// Closed-session records — muster #179. See fleet.ClosedSession for what
// a record promises; this file is how the service keeps them.
//
// # Two maps, one document
//
// A tombstone has to carry what the session looked like, and by the time a
// session is gone nothing can be asked about it any more. So the service keeps
// a small "seen" record per live local session (runtime, id, name, cwd,
// startedAt, conversation, last sighting), refreshed from every create and
// every listing it answers, and turns that record into a tombstone when the
// session ends. Both halves live in one state document, so a restart keeps
// both: a session that ended while the service was down is found missing by
// the first complete read after start-up, and still has its metadata.
//
// # Where an end is learned
//
//   - A close through this service that the driver accepted: exact.
//   - A complete, unfiltered listing of a runtime that no longer contains a
//     seen session: an upper bound (fleet.ClosedByAbsent). A filtered or
//     partial listing proves nothing about absence (§5.7) and never ends
//     anything — the same rule the label store's retain follows.
//   - A driver that can capture a session's own exit before removing it
//     (muster #235, driver.ExitReporter) reports it alongside its
//     listing: exact, like a close, because the driver observed the pane
//     itself rather than merely noticing it missing later.
//   - A local session.closed event does NOT write a tombstone by itself: a
//     rename also retires the old id on the stream. It asks for a local
//     re-listing instead (requestSweep), which settles the question with the
//     rename-carry rule below and gives a timely closedAt whenever a stream
//     is live.
//
// # Renames and recycled ids
//
// A session missing under its old id and present under a new one is the same
// run, not an end: carried when exactly one newly-listed session of the same
// runtime has the missing record's startedAt. A seen id that now names a
// session with a DIFFERENT startedAt is §5.4's recycled id: the old occupant
// ended, and gets its tombstone.
//
// # Retention
//
// Tombstones older than the retention period are pruned on every write AND
// filtered on every read, so an idle service does not keep answering with
// records past their window. A seen record not sighted within the retention
// period is dropped too — it can only belong to a runtime no longer listed.
//
// A ClosedByExit tombstone (#235) also names a file under this store's own
// exit-screens/ directory. muster #236: that file follows the SAME
// retention as the record naming it — one rule, not two — so pruning a
// tombstone here removes its file too, and a start-up sweep removes any file
// left behind with no record at all (one saved by saveExitScreen just before
// a crash prevented the tombstone from ever being written, or one whose
// record was pruned by an older build that predates this file-aware prune).
// A "keep the newest N files" cap was considered and rejected: it is a
// second rule that could drift from the store's own retention instead of
// reusing it.

// historyStateName is this store's document in the state directory.
const historyStateName = "session-history"

// DefaultClosedRetention is how long a closed-session record is kept when the
// operator configures nothing.
const DefaultClosedRetention = 14 * 24 * time.Hour

// seenFlushInterval bounds how stale a persisted lastSeenAt may get. Writing
// on every listing would make every read a write; writing only on change
// would let a restart report a lastSeenAt from hours ago. Neither extreme is
// wrong, only wasteful or loose — this is the middle.
const seenFlushInterval = 5 * time.Minute

type seenRecord struct {
	Runtime      fleet.RuntimeId        `json:"runtime,omitempty"`
	ID           string                 `json:"id"`
	Name         string                 `json:"name,omitempty"`
	Cwd          fleet.AbsolutePath     `json:"cwd,omitempty"`
	StartedAt    *fleet.Timestamp       `json:"startedAt,omitempty"`
	Conversation *fleet.ConversationRef `json:"conversation,omitempty"`
	LastSeen     time.Time              `json:"lastSeen"`

	// ObsMode is the permission mode this session last reported
	// (state.permissionMode), kept only when the driver actually read one.
	// muster #256: the half of a launch record that a listing teaches.
	ObsMode string `json:"obsMode,omitempty"`
	// Launch is what this service launched the session with, when it
	// launched it. Absent for a session started some other way.
	Launch *launchFacts `json:"launch,omitempty"`
}

// launchFacts is what a create through this service gave a session at launch
// (muster #256) — the half of a launch record that only the create knows.
type launchFacts struct {
	// Mode is the permission mode the create asked for: "bypass" or "".
	Mode string `json:"mode,omitempty"`
	// Settings is the compact launch-time settings object the create carried.
	Settings json.RawMessage `json:"settings,omitempty"`
	// SendAuth records that whoever made this launch held the `send` grant,
	// which is what a bypass mode or a settings object needs on top of
	// `create`. A later resume by a principal without it may carry them
	// forward only because this is true.
	SendAuth bool `json:"sendAuth,omitempty"`
	// Conversation is the conversation the create named (resume or
	// conversationId), known before the session's own record shows up.
	Conversation string `json:"conversation,omitempty"`
}

// launchRecord is what a conversation's last session launched with and last
// reported (muster #256), keyed by runtime and conversation id. It outlives
// the session and its tombstone's seen record: a restart-resume asks for it
// after the old process is gone, which is the whole point.
type launchRecord struct {
	Runtime      fleet.RuntimeId `json:"runtime,omitempty"`
	Conversation string          `json:"conversation"`
	// Mode is the permission mode the conversation's last session reported,
	// or — when no listing has yet read one — the mode it was launched with.
	Mode     string          `json:"mode,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
	SendAuth bool            `json:"sendAuth,omitempty"`
	At       time.Time       `json:"at"`
}

type historyDoc struct {
	Seen     []seenRecord          `json:"seen"`
	Closed   []fleet.ClosedSession `json:"closed"`
	Launches []launchRecord        `json:"launches,omitempty"`
}

type historyStore struct {
	mu        sync.Mutex
	self      fleet.MachineId
	seen      map[labelKey]*seenRecord
	launches  map[labelKey]*launchRecord
	closed    []fleet.ClosedSession
	retention time.Duration
	st        *state.Store
	savedAt   time.Time
	now       func() time.Time
}

func newHistoryStore(self fleet.MachineId) *historyStore {
	return &historyStore{
		self:      self,
		seen:      map[labelKey]*seenRecord{},
		launches:  map[labelKey]*launchRecord{},
		retention: DefaultClosedRetention,
		now:       time.Now,
	}
}

// load adopts a persisted document. A document that will not parse is an
// error the operator sees — state.Store's own rule.
func (h *historyStore) load(st *state.Store) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.st = st
	var doc historyDoc
	found, err := st.Load(historyStateName, &doc)
	if err != nil || !found {
		// muster #236: even a first run (found == false) or a store
		// with nothing to load may have exit-screens/ files left over from
		// a previous process — e.g. a crash between saveExitScreen and the
		// tombstone write. h.closed is empty either way, so this call
		// removes every file the directory holds; that is correct, not
		// aggressive, because there is by construction no live record for
		// any of them yet.
		h.sweepOrphanExitScreensLocked()
		return err
	}
	for i := range doc.Seen {
		r := doc.Seen[i]
		if r.ID == "" {
			continue
		}
		h.seen[labelKey{r.Runtime, r.ID}] = &r
	}
	h.closed = doc.Closed
	for i := range doc.Launches {
		l := doc.Launches[i]
		if l.Conversation == "" {
			continue
		}
		// The state store writes indented JSON; settings are compared and put on
		// argv compact, so they are read back compact.
		var buf bytes.Buffer
		if json.Compact(&buf, l.Settings) == nil {
			l.Settings = buf.Bytes()
		}
		h.launches[labelKey{l.Runtime, l.Conversation}] = &l
	}
	h.savedAt = h.now()
	h.sweepOrphanExitScreensLocked()
	return nil
}

// exitScreensDirLocked names the directory the tmux driver's saveExitScreen
// writes to (muster #235) — the same state.Store, so the same Dir(), as
// the one this history store persists into (cmd/muster wires both from
// one *state.Store). Empty when there is no store at all (a throwaway
// instance, a test): every caller here already treats that as "nothing to
// do", the same convention state.Store.Dir() itself uses.
func (h *historyStore) exitScreensDirLocked() string {
	dir := h.st.Dir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "exit-screens")
}

// removeExitScreenLocked deletes the file a pruned ClosedByExit tombstone
// named, if any (muster #236). Best-effort: a closed-session record is
// already gone from h.closed by the time this runs, and a leftover file a
// failed remove could not clear is exactly what the start-up sweep in load()
// exists to catch on the next restart.
func (h *historyStore) removeExitScreenLocked(c fleet.ClosedSession) {
	if c.Exit == nil || c.Exit.ScreenPath == "" {
		return
	}
	_ = os.Remove(c.Exit.ScreenPath)
}

// sweepOrphanExitScreensLocked removes every file under exit-screens/ that no
// live closed-session record names (muster #236). Run once at load —
// not on every prune, where removeExitScreenLocked already deletes a file the
// instant its own record ages out; this is only for a file whose record never
// made it into h.closed at all, or already left it by some path other than
// pruneLocked. A directory that does not exist yet, or cannot be read, is
// silently left alone — sweeping is a cleanliness pass, never load-bearing
// for correctness.
func (h *historyStore) sweepOrphanExitScreensLocked() {
	dir := h.exitScreensDirLocked()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	live := make(map[string]bool, len(h.closed))
	for _, c := range h.closed {
		if c.Exit != nil && c.Exit.ScreenPath != "" {
			live[filepath.Base(c.Exit.ScreenPath)] = true
		}
	}
	for _, e := range entries {
		if e.IsDir() || live[e.Name()] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// setRetention changes how long tombstones are kept. Non-positive restores
// the default.
func (h *historyStore) setRetention(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d <= 0 {
		d = DefaultClosedRetention
	}
	h.retention = d
}

func (h *historyStore) pruneLocked(now time.Time) bool {
	cut := now.Add(-h.retention)
	changed := false
	kept := h.closed[:0]
	for _, c := range h.closed {
		if c.ClosedAt.Before(cut) {
			changed = true
			// muster #236: this record is the only thing naming this
			// file (ScreenPath is never reused across records); once it
			// ages out here, nothing else will ever remove the file.
			h.removeExitScreenLocked(c)
			continue
		}
		kept = append(kept, c)
	}
	h.closed = kept
	for k, r := range h.seen {
		if r.LastSeen.Before(cut) {
			delete(h.seen, k)
			changed = true
		}
	}
	for k, l := range h.launches {
		if l.At.Before(cut) {
			delete(h.launches, k)
			changed = true
		}
	}
	return changed
}

// saveLocked prunes and writes the whole document. A failed write is not
// fatal to the request that caused it — the record is in force for this
// process, and the next successful write carries it.
func (h *historyStore) saveLocked(now time.Time) {
	h.pruneLocked(now)
	h.savedAt = now
	if h.st == nil {
		return
	}
	doc := historyDoc{Seen: make([]seenRecord, 0, len(h.seen)), Closed: h.closed}
	for _, l := range h.launches {
		doc.Launches = append(doc.Launches, *l)
	}
	sort.Slice(doc.Launches, func(i, j int) bool {
		if doc.Launches[i].Runtime != doc.Launches[j].Runtime {
			return doc.Launches[i].Runtime < doc.Launches[j].Runtime
		}
		return doc.Launches[i].Conversation < doc.Launches[j].Conversation
	})
	if doc.Closed == nil {
		doc.Closed = []fleet.ClosedSession{}
	}
	for _, r := range h.seen {
		doc.Seen = append(doc.Seen, *r)
	}
	sort.Slice(doc.Seen, func(i, j int) bool {
		if doc.Seen[i].Runtime != doc.Seen[j].Runtime {
			return doc.Seen[i].Runtime < doc.Seen[j].Runtime
		}
		return doc.Seen[i].ID < doc.Seen[j].ID
	})
	_ = h.st.Save(historyStateName, doc)
}

func knownConversation(c *fleet.ConversationRef) *fleet.ConversationRef {
	if c == nil || !c.Known {
		return nil
	}
	cp := *c
	return &cp
}

// sightLocked records one live sighting. It reports whether anything a
// tombstone would carry changed — the only reason a sighting alone is worth
// a write.
func (h *historyStore) sightLocked(rt fleet.RuntimeId, s fleet.Session, at time.Time) bool {
	k := labelKey{rt, s.ID}
	conv := knownConversation(s.Conversation)
	mode := observedMode(s)
	r, had := h.seen[k]
	if !had {
		r = &seenRecord{Runtime: rt, ID: s.ID, Name: s.Name, Cwd: s.Cwd,
			StartedAt: s.StartedAt, Conversation: conv, LastSeen: at, ObsMode: mode}
		h.seen[k] = r
		h.syncLaunchLocked(r, at)
		return true
	}
	changed := false
	if mode != "" && mode != r.ObsMode {
		r.ObsMode, changed = mode, true
	}
	if s.Name != "" && s.Name != r.Name {
		r.Name, changed = s.Name, true
	}
	if s.Cwd != "" && s.Cwd != r.Cwd {
		r.Cwd, changed = s.Cwd, true
	}
	if s.StartedAt != nil && (r.StartedAt == nil || !r.StartedAt.Equal(*s.StartedAt)) {
		r.StartedAt, changed = s.StartedAt, true
	}
	if conv != nil && (r.Conversation == nil || r.Conversation.ID != conv.ID) {
		r.Conversation, changed = conv, true
	}
	if at.After(r.LastSeen) {
		r.LastSeen = at
	}
	if h.syncLaunchLocked(r, at) {
		changed = true
	}
	return changed
}

// observedMode is the permission mode a listing read for s, or "" when the
// driver read none (absent) or could not name it (unknown). Only a mode the
// driver actually read may overwrite what the launch asked for — silence is
// not a report (§5.7).
func observedMode(s fleet.Session) string {
	m := s.State.PermissionMode
	if m == "" || m == fleet.PermissionModeUnknown {
		return ""
	}
	return string(m)
}

// syncLaunchLocked folds a seen record into the launch record of the
// conversation it belongs to (muster #256), and reports whether that record
// changed. A session whose conversation is not yet known — neither resolved
// nor named by its create — contributes nothing; the next sighting tries again.
//
// The mode is the last one the session reported, falling back to the one it
// was launched with while no listing has read one, and finally to what the
// conversation's previous record said. Settings and the send flag are facts
// only a create knows, so a session this service did not launch (Launch nil)
// leaves the conversation's earlier ones standing instead of erasing them.
func (h *historyStore) syncLaunchLocked(r *seenRecord, at time.Time) bool {
	conv := ""
	switch {
	case r.Conversation != nil && r.Conversation.ID != "":
		conv = r.Conversation.ID
	case r.Launch != nil:
		conv = r.Launch.Conversation
	}
	if conv == "" {
		return false
	}
	k := labelKey{r.Runtime, conv}
	prev := h.launches[k]
	next := launchRecord{Runtime: r.Runtime, Conversation: conv, At: at}
	if prev != nil {
		next.Mode, next.Settings, next.SendAuth = prev.Mode, prev.Settings, prev.SendAuth
	}
	if r.Launch != nil {
		next.Mode, next.Settings, next.SendAuth = r.Launch.Mode, r.Launch.Settings, r.Launch.SendAuth
	}
	if r.ObsMode != "" {
		next.Mode = r.ObsMode
	}
	if prev != nil && prev.Mode == next.Mode && prev.SendAuth == next.SendAuth &&
		string(prev.Settings) == string(next.Settings) {
		// Nothing a resume would read has changed, so this is not a reason to
		// write — but the conversation is still being seen, and its record must
		// not age out of retention underneath a live session.
		if at.After(prev.At) {
			prev.At = at
		}
		return false
	}
	h.launches[k] = &next
	return true
}

// launchFor returns the launch record of a conversation, if one is held and
// still inside the retention period.
func (h *historyStore) launchFor(rt fleet.RuntimeId, conversation string) (launchRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.launches[labelKey{rt, conversation}]
	if !ok || l.At.Before(h.now().Add(-h.retention)) {
		return launchRecord{}, false
	}
	return *l, true
}

func (h *historyStore) tombstoneLocked(r *seenRecord, at time.Time, by fleet.ClosureKind, evidence string) {
	name := r.Name
	if name == "" {
		name = r.ID
	}
	h.closed = append(h.closed, fleet.ClosedSession{
		SessionRef:   fleet.SessionRef{Machine: h.self, ID: r.ID, Name: name},
		Runtime:      r.Runtime,
		Cwd:          r.Cwd,
		StartedAt:    r.StartedAt,
		Conversation: r.Conversation,
		LastSeenAt:   r.LastSeen,
		ClosedAt:     at,
		ClosedBy:     by,
		Evidence:     evidence,
	})
}

// tombstoneExitLocked writes the tombstone for a session an ExitReporter
// driver captured and already removed (muster #235). ClosedAt is the
// driver's own capture moment (e.Exit.At), not `now`: the capture happened
// synchronously inside the List call this observe is folding in, and using
// that moment keeps ClosedByExit exact the same way ClosedByClose's is,
// rather than backdating it to whenever this fold-in happened to run.
func (h *historyStore) tombstoneExitLocked(r *seenRecord, e driver.CapturedExit) {
	name := r.Name
	if name == "" {
		name = r.ID
	}
	exit := e.Exit
	h.closed = append(h.closed, fleet.ClosedSession{
		SessionRef:   fleet.SessionRef{Machine: h.self, ID: r.ID, Name: name},
		Runtime:      r.Runtime,
		Cwd:          r.Cwd,
		StartedAt:    r.StartedAt,
		Conversation: r.Conversation,
		LastSeenAt:   r.LastSeen,
		ClosedAt:     exit.At,
		ClosedBy:     fleet.ClosedByExit,
		Evidence:     fmt.Sprintf("captured its own exit status (%d) before removing the session", exit.Status),
		Exit:         &exit,
	})
}

// created records a session this service just started, so a close that
// arrives before any listing still has something to describe.
//
// launch is what this create gave the session (muster #256), recorded so a later
// resume of the conversation can carry it. Nil for a caller with nothing to
// record.
func (h *historyStore) created(rt fleet.RuntimeId, s fleet.Session, launch *launchFacts) {
	if s.ID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.sightLocked(rt, s, now)
	if launch != nil {
		if r, ok := h.seen[labelKey{rt, s.ID}]; ok {
			r.Launch = launch
			h.syncLaunchLocked(r, now)
		}
	}
	h.saveLocked(now)
}

// renamed moves a seen record to its new id (a rename accepted through this
// service). A rename that later reverts is settled by observe's carry rule.
func (h *historyStore) renamed(rt fleet.RuntimeId, from, to string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.seen[labelKey{rt, from}]
	if !ok || from == to {
		return
	}
	delete(h.seen, labelKey{rt, from})
	r.ID, r.Name = to, to
	h.seen[labelKey{rt, to}] = r
	h.saveLocked(h.now())
}

// closedByRequest writes the tombstone for a close this service relayed to a
// local driver and the driver accepted.
func (h *historyStore) closedByRequest(rt fleet.RuntimeId, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	k := labelKey{rt, id}
	r, ok := h.seen[k]
	if !ok {
		// Never sighted by this service (it started before any listing and
		// was closed before one ran). The end is still a fact worth keeping;
		// only the metadata is thin, and it says so by being absent.
		r = &seenRecord{Runtime: rt, ID: id}
	}
	delete(h.seen, k)
	h.tombstoneLocked(r, now, fleet.ClosedByClose, "closed through this service")
	h.saveLocked(now)
}

// observe folds one local runtime's listing in. complete is true only for an
// unfiltered listing every source answered ok; only such a listing may end a
// session. listedAt is when the listing was requested: a record sighted after
// it (a create that finished while the listing ran) is not missing, merely
// later than the snapshot. exits is whatever an ExitReporter driver (#235)
// captured and already removed in the SAME call that produced items — every
// id in it is guaranteed absent from items, by construction of the driver
// side of this contract.
func (h *historyStore) observe(rt fleet.RuntimeId, items []fleet.Session, complete bool, listedAt time.Time, exits []driver.CapturedExit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	changed := false

	// muster #235: settle every captured exit before the ordinary
	// present/absent bookkeeping below ever sees its id. The driver already
	// removed these sessions, so none of them can appear in items this round —
	// without this they would fall through to the complete-scan's generic
	// "absent from a listing" tombstone below, the very thing an ExitReporter
	// exists to do better than.
	for _, e := range exits {
		k := labelKey{rt, e.ID}
		r, ok := h.seen[k]
		if !ok {
			// Never sighted by this service — its own process exited before
			// any listing reached it. The end is still a fact worth keeping
			// with the exit the driver captured; only the rest of the
			// metadata is thin, and it says so by being absent, same as
			// closedByRequest's identical fallback.
			r = &seenRecord{Runtime: rt, ID: e.ID}
		} else {
			delete(h.seen, k)
		}
		h.tombstoneExitLocked(r, e)
		changed = true
	}

	present := make(map[string]bool, len(items))
	fresh := make([]fleet.Session, 0)
	for _, s := range items {
		present[s.ID] = true
		// A close accepted by the driver that did not take: the session is
		// still here, so its tombstone was premature. Withdrawn rather than
		// left to contradict the live list.
		if h.withdrawLocked(rt, s) {
			changed = true
		}
		k := labelKey{rt, s.ID}
		r, had := h.seen[k]
		if !had {
			fresh = append(fresh, s)
			continue
		}
		if r.StartedAt != nil && s.StartedAt != nil && !r.StartedAt.Equal(*s.StartedAt) {
			// §5.4: the id now names a session that started at a different
			// time. Any listing showing that proves the old occupant ended —
			// complete or not — and the new one starts a record of its own.
			h.tombstoneLocked(r, now, fleet.ClosedByAbsent,
				"id is now held by a session that started at a different time")
			delete(h.seen, k)
			changed = true
		}
		if h.sightLocked(rt, s, now) {
			changed = true
		}
	}

	if complete {
		for k, r := range h.seen {
			if k.runtime != rt || present[k.id] || !r.LastSeen.Before(listedAt) {
				continue
			}
			if i := carryIndex(r, fresh); i >= 0 {
				// Same run under a new id — a rename, not an end.
				delete(h.seen, k)
				r.ID, r.Name = fresh[i].ID, fresh[i].Name
				h.seen[labelKey{rt, r.ID}] = r
				fresh = append(fresh[:i], fresh[i+1:]...)
				changed = true
				continue
			}
			h.tombstoneLocked(r, now, fleet.ClosedByAbsent,
				"absent from a complete listing of its runtime; it ended after lastSeenAt")
			delete(h.seen, k)
			changed = true
		}
	}

	for _, s := range fresh {
		h.sightLocked(rt, s, now)
		changed = true
	}

	if changed || now.Sub(h.savedAt) >= seenFlushInterval {
		h.saveLocked(now)
	}
}

// carryIndex finds the one newly-listed session that is r's run under a new
// id. Ambiguity carries nothing: two candidates with r's startedAt could each
// be it, and guessing would attribute one session's history to another.
func carryIndex(r *seenRecord, fresh []fleet.Session) int {
	if r.StartedAt == nil {
		return -1
	}
	found := -1
	for i, s := range fresh {
		if s.StartedAt != nil && s.StartedAt.Equal(*r.StartedAt) {
			if found >= 0 {
				return -1
			}
			found = i
		}
	}
	return found
}

// withdrawLocked removes a ClosedByClose tombstone for a session that is
// demonstrably still running (same runtime, id and startedAt).
func (h *historyStore) withdrawLocked(rt fleet.RuntimeId, s fleet.Session) bool {
	if s.StartedAt == nil {
		return false
	}
	for i := len(h.closed) - 1; i >= 0; i-- {
		c := h.closed[i]
		if c.ClosedBy == fleet.ClosedByClose && c.Runtime == rt && c.ID == s.ID &&
			c.StartedAt != nil && c.StartedAt.Equal(*s.StartedAt) {
			h.closed = append(h.closed[:i], h.closed[i+1:]...)
			return true
		}
	}
	return false
}

// list returns the retained tombstones that closed at or after since (zero
// means everything retained), newest first.
func (h *historyStore) list(since time.Time) []fleet.ClosedSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	cut := h.now().Add(-h.retention)
	out := make([]fleet.ClosedSession, 0, len(h.closed))
	for _, c := range h.closed {
		if c.ClosedAt.Before(cut) || c.ClosedAt.Before(since) {
			continue
		}
		out = append(out, c)
	}
	sortClosed(out)
	return out
}

func sortClosed(items []fleet.ClosedSession) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].ClosedAt.After(items[j].ClosedAt) })
}

// ListClosedSessions answers GET /v1/sessions/closed. ScopeLocal reads this
// machine's own records only (§13.1); ScopeFleet adds every peer that can
// answer, and a peer that cannot contributes a SourceStatus saying so — never
// a silent absence (§9).
func (s *Service) ListClosedSessions(ctx context.Context, req fleet.Request, scope Scope, since time.Time, callerDeadline time.Duration) (fleet.Collection[fleet.ClosedSession], error) {
	items := s.history.list(since)
	sources := []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: time.Now()}}

	if scope == ScopeFleet {
		for machine, d := range s.peerDrivers() {
			cl, ok := d.(driver.ClosedLister)
			if !ok {
				sources = append(sources, fleet.SourceStatus{
					Machine: machine, Status: fleet.SourceDegraded,
					Error: "this peer's driver cannot report closed sessions", ObservedAt: time.Now(),
				})
				continue
			}
			deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
			callCtx, cancel := context.WithTimeout(ctx, deadline)
			col, err := cl.ListClosed(callCtx, req, since)
			cancel()
			if err != nil {
				sources = append(sources, fleet.SourceStatus{
					Machine: machine, Status: fleet.SourceUnreachable,
					Error: err.Error(), ObservedAt: time.Now(),
				})
				continue
			}
			items = append(items, col.Items()...)
			sources = append(sources, col.Sources()...)
		}
		sortClosed(items)
	}
	return fleet.NewCollection(items, sources)
}

// SetClosedRetention configures how long closed-session records are kept
// (#179). Call it at startup; non-positive restores the default.
func (s *Service) SetClosedRetention(d time.Duration) { s.history.setRetention(d) }

// SweepLocal lists every local runtime once, unfiltered, so the history store
// can notice sessions that ended unobserved. cmd/muster calls it at
// start-up (sessions that ended while the service was down); requestSweep
// calls it when a local event stream reports a session gone.
func (s *Service) SweepLocal(ctx context.Context) {
	_, _ = s.ListSessions(ctx, fleet.SystemRequest(), ScopeLocal, driver.ListFilter{}, 0)
}

// requestSweep runs SweepLocal in the background, at most one at a time; a
// request arriving while one runs schedules exactly one more, so the last
// change is always read after it happened.
func (s *Service) requestSweep() {
	s.sweepMu.Lock()
	if s.sweeping {
		s.sweepAgain = true
		s.sweepMu.Unlock()
		return
	}
	s.sweeping = true
	s.sweepMu.Unlock()
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.SweepLocal(ctx)
			cancel()
			s.sweepMu.Lock()
			if !s.sweepAgain {
				s.sweeping = false
				s.sweepMu.Unlock()
				return
			}
			s.sweepAgain = false
			s.sweepMu.Unlock()
		}
	}()
}
