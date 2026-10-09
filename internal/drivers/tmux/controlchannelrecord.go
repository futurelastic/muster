package tmux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Reading the runtime's own record for WHY a control channel failed (#69) —
// the record-side counterpart to controlchannel.go's footer read.
//
// # Why this exists, and why it waited
//
// controlchannel.go's footer gives a closed-set STATE — active, connecting,
// reconnecting, failed — read from chrome an agent cannot write into. It
// never carried a REASON, because the runtime's disconnection notice prints
// into the transcript, and the transcript is the forgeable region: the
// incident that motivated the footer-only rule was a supervisor grepping
// panes for that exact notice and classifying itself as disconnected because
// its own tool output contained the string it was searching for.
//
// Measured since (#69): the same notice also reaches the runtime's own
// durable record, structured rather than rendered —
// `{"type":"system","subtype":"informational","content":"Remote Control
// disconnected …"}` — which is the same class of artefact runtimerecord.go
// already reads for refusals (#56).
//
// # The filter is the safety property
//
// A substring search over the record is unsound: the store has at least one
// hit that is a USER-role entry holding captured command output, a region an
// agent's own actions populate. So Type and Subtype are checked BEFORE
// Content is ever compared against the phrase — that ordering is the whole
// property, exactly parallel to #56 keying on `isApiErrorMessage` rather
// than on message text. A reader that matched the phrase across entry types
// would reintroduce forgeability through a different door than the one the
// footer rule closed.
//
// # What is still NOT done here
//
// No transient/terminal classification. controlchannel.go's own comment
// already declined that inference once (#65) — the close codes are read out
// of the runtime binary and recorded there, but which ones are retryable was
// never measured. This file carries the runtime's own sentence, code number
// included when the runtime put one in it, and stops: the caller decides.

// controlDisconnectPhrase anchors the runtime's own disconnection notice.
// Matching this alone would not be safe — the type/subtype check in
// latestControlDisconnect is the actual safety property. The phrase only
// narrows a match, among everything else a system/informational entry can
// carry, to the ones that are actually about a disconnection.
const controlDisconnectPhrase = "Remote Control disconnected"

// controlDisconnectRecordEntry is the subset of one JSONL line this driver
// reads to decide whether it is the runtime's own disconnection notice.
//
// Content is decoded as a bare top-level string deliberately: only a
// `system`/`informational` entry is shaped that way in this store. A
// `user`-role entry carrying the same phrase inside captured command output
// nests it several levels down (`message.content[].content`), which does
// not unmarshal into this field at all — so on that entry Content decodes
// empty and the Type check below rejects it before Content is even
// considered. Failing to populate Content is therefore itself evidence the
// line is not one of these, not a reason to try some other shape.
type controlDisconnectRecordEntry struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// controlDisconnectFact is what the record's own words say about why a
// control channel failed.
type controlDisconnectFact struct {
	// text is the runtime's own sentence, in full.
	text string
	// at is the notice's own timestamp, as the runtime wrote it.
	at time.Time
}

// reasonText bounds the fact's text for display — a length cap only, never a
// sentence cut. apiErrorFact.reasonSentence() cuts at the first period
// because the load-bearing clause of an API error is reliably the first one;
// this notice does not have that shape — controlchannel.go's own doc comment
// quotes two real forms ("Couldn't reconnect … Retry, or start a fresh
// session without --resume" and "this session was ended or archived from
// another device or app (code 4090)"), and in each the detail that matters —
// the retry instruction, the close code — sits after where a sentence cut
// would land. Cutting there would silently throw away the one thing #69
// asked this field to carry.
func (f controlDisconnectFact) reasonText() string {
	const maxReasonBytes = 300
	s := strings.TrimSpace(f.text)
	if len(s) > maxReasonBytes {
		s = s[:maxReasonBytes]
	}
	return s
}

// latestControlDisconnect reads the tail of one runtime record and reports
// the most recent runtime-written disconnection notice it can find — the
// record-side counterpart to controlChannelOf's footer read.
//
// Walks backward like latestAPIError (#56): the answer sought is always the
// most recent matching line, and in practice that is within the first few
// lines scanned. Unlike latestAPIError, this does not stop at the first
// entry of any type it meets — it skips past everything that is not itself
// a matching system/informational entry, because there is no "history
// superseded by this one" hazard to guard against here: this function is not
// deciding a session's CURRENT state (controlChannelOf's footer read already
// did that), only explaining a state already decided.
func latestControlDisconnect(path string) (controlDisconnectFact, bool) {
	lines, _, ok := recordTail(path)
	if !ok {
		return controlDisconnectFact{}, false
	}

	inspected := 0
	for i := len(lines) - 1; i >= 0 && inspected < recordTailCandidates; i-- {
		inspected++
		var entry controlDisconnectRecordEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			// A torn or half-written line is not a reason to give up on the
			// ones before it (the same allowance latestAPIError and
			// conversation.go's readRecordEntry already make).
			continue
		}
		// The region check, ahead of the content check on purpose: an entry
		// that is not the runtime's own system/informational kind is
		// skipped before its Content is even compared against the phrase.
		// This ordering is the safety property #69 measured the need for.
		if entry.Type != "system" || entry.Subtype != "informational" {
			continue
		}
		if !strings.Contains(entry.Content, controlDisconnectPhrase) {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			// Found the right entry but not a fact we can act on: honestly
			// unresolvable rather than a guessed time (the same call
			// latestAPIError makes for the same shape of gap).
			return controlDisconnectFact{}, false
		}
		return controlDisconnectFact{text: entry.Content, at: ts}, true
	}
	return controlDisconnectFact{}, false
}

// controlReasonFor asks the runtime's own record why one session's control
// channel failed — reusing its already-resolved Conversation the way
// recordFactFor does for #56, rather than looking one up again. Returns
// false whenever no record store is configured, this session's conversation
// could not be matched, or the record has no matching entry; every caller
// must leave ControlChannel.Reason empty in exactly those cases, never a
// guess.
func (d *Driver) controlReasonFor(s fleet.Session) (controlDisconnectFact, bool) {
	if d.conversations == nil || s.Conversation == nil || !s.Conversation.Known {
		return controlDisconnectFact{}, false
	}
	return latestControlDisconnect(d.conversations.recordPath(string(s.Cwd), s.Conversation.ID))
}

// upgradeControlChannelFromRecord applies #69's Reason upgrade to a bare
// SessionState that has no pre-resolved Conversation to reuse — State's own
// shape, unlike List's Session, the same split upgradeLastTurnFromRecord
// already makes for #56.
//
// A no-op whenever st.ControlChannel is nil or not Failed (nothing to
// explain), or no record store is configured, or the record cannot be
// matched, or it has no matching entry — in every one of those cases Reason
// is left exactly as classify.go built it: empty.
func (d *Driver) upgradeControlChannelFromRecord(ctx context.Context, st fleet.SessionState, cwd, name string, created time.Time, paneID string, pid int) fleet.SessionState {
	if st.ControlChannel == nil || st.ControlChannel.State != fleet.ControlChannelFailed || d.conversations == nil {
		return st
	}
	ref := d.conversations.lookup(conversationKey{pane: paneID, created: created}, cwd, name, created,
		processGeneration{pid: pid}, d.liveConversationSource(ctx, pid, cwd))
	if ref == nil || !ref.Known {
		return st
	}
	if fact, ok := latestControlDisconnect(d.conversations.recordPath(cwd, ref.ID)); ok {
		channel := *st.ControlChannel
		channel.Reason = fact.reasonText()
		st.ControlChannel = &channel
	}
	return st
}

// Reading the runtime's own record for whether remote control is ON or OFF
// (muster #269) — the state the footer label cannot carry.
//
// # Why the label is not enough
//
// A session without remote control renders no label, so a label can never say
// "off". Worse, on the runtime version this was measured against (2.1.293) the
// label for an ACTIVE channel is drawn in the header banner, not the footer
// row the reader above looks at, and a banner scrolls away as the transcript
// grows. The footer reader is therefore blind to a healthy channel there. What
// the runtime does leave, scroll-independent and written by itself, is a
// structured entry each time the state changes:
//
//   - `system` / `bridge_status`, content "/remote-control is active · …" — the
//     channel came up (written whether it was enabled at launch or by the
//     slash command);
//   - `system` / `local_command` with commandRun.command "remote-control" and
//     stdout "Remote Control disconnected." — the user chose Disconnect.
//
// # The filter is the safety property, again
//
// Exactly as latestControlDisconnect above: Type and Subtype (and, for the
// disconnect, the command name) are checked BEFORE Content is compared with
// anything. A `user`-role entry carrying captured command output that happens
// to contain either phrase nests it several levels down and never reaches the
// comparison. A reader that matched the phrases across entry types would
// reintroduce the forgeable region the footer rule exists to stay out of.
//
// # What is deliberately NOT claimed
//
//   - The runtime's own disconnection notice (system/informational, #69) is not
//     read as `failed` the moment it is written. The record cannot tell a
//     channel the runtime is about to bring back from one that is gone: measured
//     over a large set of records, a notice about an account change was
//     followed by a fresh enable entry within two minutes every time it
//     recovered by itself, while a notice about a failed session creation was
//     followed by one only after hours, when somebody intervened. So the reader
//     reports the notice and its time, and resolveControlChannel decides after
//     a settle window (controlNoticeSettle). Before the window ends it makes no
//     claim, and never `reconnecting`, which no record entry carries.
//   - `connecting` and `reconnecting` have no record entry at all (none in any
//     record measured), and the banner they would be drawn in scrolls away and
//     is not chrome an agent cannot write once it has. They are read from the
//     footer when the runtime draws them there and are otherwise absent.
//   - Entries older than the process's launch are ignored: a resumed
//     conversation's record holds the previous process's bridge history.
//   - The record is folded from its start and the answer remembered per session
//     (#271), so a long conversation keeps it. Only a line too long to decode is
//     a gap, and a gap makes "no entry found" NOT evidence there is none.

const (
	controlEnabledPhrase  = "/remote-control is active"
	controlCommandName    = "remote-control"
	controlBridgeSubtype  = "bridge_status"
	controlLocalSubtype   = "local_command"
	controlInformationalS = "informational"
)

type controlRecordEntry struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	// URL is the link the runtime printed when the channel came up, on a
	// bridge_status entry (#276). Its last path segment is the bridge's id.
	URL        string `json:"url"`
	CommandRun *struct {
		Command string `json:"command"`
	} `json:"commandRun"`
}

// controlRecordKind is what one record line says about the control channel.
type controlRecordKind int

const (
	// controlRecordNone is a system entry that says nothing about the channel.
	controlRecordNone controlRecordKind = iota
	// controlRecordActive is the runtime's "channel came up" entry.
	controlRecordActive
	// controlRecordOff is the user's own Disconnect.
	controlRecordOff
	// controlRecordNotice is the runtime's own disconnection notice (#69).
	controlRecordNotice
)

// controlRecordEvent is one classified record line.
type controlRecordEvent struct {
	kind controlRecordKind
	// at is the entry's own timestamp; atOK is false when it did not parse.
	at   time.Time
	atOK bool
	// text is the entry's content, set for a notice only.
	text string
	// bridgeID is the bridge's id, set for an active entry that carried a link
	// (#276). Empty is "the runtime wrote none", never an error.
	bridgeID string
}

// classifyControlRecordLine classifies one JSONL line. system is false for a
// line that is not a decodable `system` entry, which every caller skips. It is
// a pure function of the line so a per-session fold (#271) can reuse it.
//
// The order is the safety property: Type, then Subtype (and the command name
// for a local command), and only then Content.
func classifyControlRecordLine(line string) (ev controlRecordEvent, system bool) {
	var e controlRecordEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return controlRecordEvent{}, false // torn or half-written
	}
	if e.Type != "system" {
		return controlRecordEvent{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	ev = controlRecordEvent{at: ts, atOK: err == nil}
	switch {
	case e.Subtype == controlBridgeSubtype && strings.HasPrefix(e.Content, controlEnabledPhrase):
		ev.kind = controlRecordActive
		ev.bridgeID = bridgeIDFromURL(e.URL)
	case e.Subtype == controlLocalSubtype && e.CommandRun != nil && e.CommandRun.Command == controlCommandName &&
		strings.Contains(e.Content, controlDisconnectPhrase):
		ev.kind = controlRecordOff
	case e.Subtype == controlInformationalS && strings.Contains(e.Content, controlDisconnectPhrase):
		ev.kind = controlRecordNotice
		ev.text = e.Content
	}
	return ev, true
}

// bridgeIDFromURL is the identifier a bridge link ends in: its last path segment,
// with any query, fragment or trailing slash dropped. It returns "" unless that
// segment is a plain token (letters, digits, '_' and '-'), so an entry whose link
// is missing or shaped some other way yields no id rather than a guess, and an id
// that reaches a caller is always safe to put back into a URL path unescaped.
//
// This reads only the `url` field of a `system`/`bridge_status` entry, which the
// caller has already filtered on type and subtype; it never looks at a message
// body, the forgeable region the rest of this file stays out of.
func bridgeIDFromURL(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimRight(raw, "/")
	seg := raw[strings.LastIndex(raw, "/")+1:]
	if seg == "" || len(seg) > 128 {
		return ""
	}
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return ""
		}
	}
	return seg
}

// controlRecordRead is what folding one record established.
type controlRecordRead struct {
	// exists is false when the record file could not be opened at all — a fresh
	// session the runtime has not written to yet.
	exists bool
	// state is active or off when the newest relevant entry since launch said
	// so; empty otherwise.
	state fleet.ControlChannelState
	// bridgeID is the id carried by the enable entry that state=active rests on
	// (#276); empty for any other state, and for an enable that carried none.
	bridgeID string
	// notice is set when the runtime's own disconnection notice is the newest
	// relevant entry since launch. Whether it means `failed` depends on how
	// long ago it was written, which a cached read cannot know — see
	// resolveControlChannel.
	notice *controlDisconnectFact
	// blocked is true when the record is readable but may not be claimed from
	// (an entry whose time cannot be read): neither may anything that falls
	// back on its silence.
	blocked bool
	// covered is true when the fold saw everything since launch, so finding no
	// entry means there is none.
	covered bool
}

// Remembering the answer per session (muster #271).
//
// #269 first read the answer from the last recordTailBytes of the record. The
// enable entry is written once, at the start, so in a long conversation it
// fell out of that window and the field went absent for exactly the sessions
// that live longest. The reader below keeps what it has folded instead: the
// byte offset already consumed and the newest relevant entry found so far.
// Each read consumes only what was appended since.
//
//   - The first read of a record (and the first after a service restart) is a
//     full read from byte 0; every later one starts where the last stopped.
//   - Only complete lines are consumed. A trailing fragment with no newline is
//     the runtime mid-write: it is classified for THIS answer but not consumed,
//     so the next read decodes it again whole.
//   - The memory is keyed by record path (which carries the conversation id)
//     and the process's launch time, and is dropped when either changes, when
//     the file is replaced (a different file identity) or when it shrinks.
//   - A line over recordLineLimit is skipped, never decoded, and remembered as
//     a gap: absence of an entry is then not evidence, so `covered` stays false
//     and the launch cannot claim `off` from it. The entries this reader keeps
//     are a few hundred bytes; nothing it wants is lost to the skip.
//   - A line that does not contain the bytes "system" cannot be a `system`
//     entry, so it is not decoded at all. That is only a speed rule for a full
//     read of a large record: it can skip a line, never make one match.
type controlRecordFold struct {
	mu sync.Mutex

	path  string
	since time.Time
	// ident is the file identity the offset refers to; offset is the first byte
	// not yet consumed.
	ident  os.FileInfo
	offset int64
	// size and mod are the stat the last answer was computed against: an
	// unchanged stat is answered from memory without opening the file.
	size int64
	mod  time.Time

	state controlFoldState
	// gap is true once a line was skipped for its length.
	gap  bool
	read controlRecordRead
	init bool
}

// controlFoldState is the newest relevant entry since launch, the only thing a
// fold has to remember. It is a value so a mid-write fragment can be applied to
// a copy.
type controlFoldState struct {
	newest  controlRecordEvent
	have    bool
	blocked bool
}

// apply folds one decoded system entry into the state, exactly as a walk from
// the newest entry backwards would have decided: the newest relevant entry
// wins, and an entry from before the launch ends the history that counts.
func (s *controlFoldState) apply(ev controlRecordEvent, since time.Time) {
	if ev.atOK && ev.at.Before(since) {
		*s = controlFoldState{}
		return
	}
	switch ev.kind {
	case controlRecordActive, controlRecordOff, controlRecordNotice:
		if !ev.atOK {
			*s = controlFoldState{blocked: true}
			return
		}
		*s = controlFoldState{newest: ev, have: true}
	}
}

func (s controlFoldState) result(gap bool) controlRecordRead {
	res := controlRecordRead{exists: true}
	switch {
	case s.blocked:
		res.blocked = true
	case !s.have:
		res.covered = !gap
	case s.newest.kind == controlRecordNotice:
		res.notice, res.covered = &controlDisconnectFact{text: s.newest.text, at: s.newest.at}, true
	case s.newest.kind == controlRecordOff:
		res.state, res.covered = fleet.ControlChannelOff, true
	default:
		res.state, res.covered = fleet.ControlChannelActive, true
		res.bridgeID = s.newest.bridgeID
	}
	return res
}

func (f *controlRecordFold) reset(path string, since time.Time) {
	// Not *f = …: that would overwrite the mutex advance is holding.
	f.path, f.since = path, since
	f.ident, f.offset, f.size, f.mod = nil, 0, 0, time.Time{}
	f.state, f.gap, f.read, f.init = controlFoldState{}, false, controlRecordRead{}, false
}

// advance brings the fold up to the end of the record and returns the answer.
func (f *controlRecordFold) advance(path string, since time.Time) controlRecordRead {
	f.mu.Lock()
	defer f.mu.Unlock()

	fi, err := os.Stat(path)
	if err != nil {
		f.reset(path, since)
		if os.IsNotExist(err) {
			return controlRecordRead{} // not written yet: a real answer
		}
		// Present but unreadable is not "not written yet", and must not be
		// allowed to fall through to a launch-based claim.
		return controlRecordRead{exists: true, blocked: true}
	}
	if !f.init || f.path != path || !f.since.Equal(since) || f.ident == nil ||
		!os.SameFile(f.ident, fi) || fi.Size() < f.offset {
		f.reset(path, since)
		f.ident = fi
		f.init = true
	}
	if fi.Size() == f.size && fi.ModTime().Equal(f.mod) && f.size != 0 {
		return f.read
	}

	file, err := os.Open(path)
	if err != nil {
		f.reset(path, since)
		return controlRecordRead{exists: true, blocked: true}
	}
	defer file.Close()
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		f.reset(path, since)
		return controlRecordRead{exists: true, blocked: true}
	}
	r := bufio.NewReaderSize(file, 64*1024)
	var fragment []byte
	for {
		line, n, complete, rerr := readBoundedLine(r, recordLineLimit)
		if !complete {
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				f.reset(path, since)
				return controlRecordRead{exists: true, blocked: true}
			}
			if n > 0 && n <= recordLineLimit {
				buf := make([]byte, n)
				if got, _ := file.ReadAt(buf, f.offset); got == n {
					fragment = buf
				}
			}
			break
		}
		f.offset += int64(n)
		if line == nil {
			f.gap = true
			continue
		}
		if !bytes.Contains(line, systemMarker) {
			continue
		}
		if ev, system := classifyControlRecordLine(string(line)); system {
			f.state.apply(ev, since)
		}
	}
	state := f.state
	if len(fragment) > 0 && bytes.Contains(fragment, systemMarker) {
		if ev, system := classifyControlRecordLine(string(fragment)); system {
			state.apply(ev, since)
		}
	}
	f.size, f.mod = fi.Size(), fi.ModTime()
	f.read = state.result(f.gap)
	return f.read
}

var systemMarker = []byte("system")

// controlChannelFromRecord folds one runtime record from its start for the
// newest remote-control entry written at or after since (the process's launch).
// It remembers nothing; the driver's per-session memory is controlRecordCache.
func controlChannelFromRecord(path string, since time.Time) controlRecordRead {
	var f controlRecordFold
	return f.advance(path, since)
}
