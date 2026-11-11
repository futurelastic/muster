package opencode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
)

// What one message of the runtime's record may yield to the record route (muster
// #443, on the boundary of #441 and the kinds of #442).
//
// # The boundary, and where it is enforced
//
// This file is the allow-list for this runtime's record reader and nowhere
// downstream is it widened: record.go and recordmedia.go only walk and page what
// classifyRecordMessage returns, and the service, the relay and the wire type carry
// it without looking. It is an allow-list that fails closed. A part is admitted by
// its own type and the runtime's own flags, and a part this code has never seen, or
// cannot decode, contributes nothing; the part beside it is not affected.
//
// Like turns.go this file decodes part content, and unlike it, more than text: the
// wire types below are separate from wireMessageInfo and wirePart (client.go) for
// the same reason, so that state and usage stay unable to read content.
//
// # One entry per part, not one per message
//
// turns joins an assistant message's text parts into one turn, because a turn is
// the unit ADR 258 names. A record is read while a message is still being written,
// and a joined entry would change under a client that already holds it. So the
// record has one entry per part, in the order the runtime wrote them: a text part
// is a `text`, a reasoning part a `reasoning`, a tool part a `tool`, an image an
// `image`. The two routes agree on WHICH text is the agent's (the same predicate);
// they differ in how it is grouped. A model round that wrote one text part, which
// is nearly every round, reads alike on both.
//
// # What is emitted
//
//	user message       text            a text part a person typed (not synthetic, not ignored)
//	                   image           an inline image the person attached
//	assistant message  text            a text part of a finished or running, error-free message
//	                   reasoning       a reasoning part with text
//	                   tool            a tool part: its input and its state, paired
//	                   image           an inline image a completed tool returned
//	compaction summary summary         the text of an assistant message flagged `summary`
//
// Out, always: step, snapshot, patch, agent, retry, compaction and subtask parts, a
// message that errored, an assistant message from no model, a file part that is not
// an inline image of a type this API names (a link, a path, an SVG), a tool's
// metadata and title, and any part this code cannot classify with certainty.

// recordPartKind says what a part is to the walk in record.go.
type recordPartKind int

const (
	partSkip  recordPartKind = iota // not admitted
	partEntry                       // one entry: text, reasoning, summary, an image
	partTool                        // a tool call, with a state and perhaps images
)

// recordPart is one part of a message, classified.
type recordPart struct {
	kind recordPartKind

	// settled is false while the runtime is still writing the part: a streaming text
	// or reasoning part, or a tool call whose input is still arriving. Nothing past
	// an unsettled part is read until the message is finished.
	settled bool

	// entry is the part's one entry (partEntry).
	entry fleet.RecordMessage

	// A tool part (partTool).
	callID     string
	status     string // wire status: pending, running, completed or error
	tool       fleet.RecordMessage
	result     string
	resultCut  bool
	hasResult  bool
	attachment []fleet.RecordMessage
}

// terminal reports whether the runtime has written the call's outcome.
func (p *recordPart) terminal() bool { return p.status == "completed" || p.status == "error" }

// running returns the call as it goes out while it has no result.
func (p *recordPart) running() fleet.RecordMessage {
	m := p.tool
	t := *p.tool.Tool
	t.Status = fleet.RecordToolRunning
	m.Tool = &t
	return m
}

// interrupted returns the call as it goes out when the runtime will write no
// result for it: it was never finished, and a later message exists.
func (p *recordPart) interrupted() fleet.RecordMessage {
	m := p.tool
	t := *p.tool.Tool
	t.Status = fleet.RecordToolError
	m.Tool = &t
	return m
}

// group returns the entries a terminal call goes out as: the call, complete, then
// the images it returned.
func (p *recordPart) group() []fleet.RecordMessage {
	m := p.tool
	t := *p.tool.Tool
	t.Status = fleet.RecordToolCompleted
	if p.status == "error" {
		t.Status = fleet.RecordToolError
	}
	if p.hasResult {
		t.Result = p.result
		m.Truncated = m.Truncated || p.resultCut
	}
	m.Tool = &t
	return append([]fleet.RecordMessage{m}, p.attachment...)
}

// recordMessage is one message of the record, classified.
type recordMessage struct {
	id        string
	assistant bool
	// admitted is the message-level predicate: whether its parts may be read at all.
	admitted bool
	// completed is the runtime's own completion time of an assistant message, zero
	// while it is being written; errored that it ended in an error.
	completed int64
	errored   bool
	parts     []recordPart
}

// finished reports whether the runtime is done writing the message: it completed,
// or it ended in an error and will write no more.
func (m *recordMessage) finished() bool { return !m.assistant || m.completed > 0 || m.errored }

// recordScope is what binds a reference to the conversation it is issued for.
type recordScope struct {
	session string
	created int64 // the session's start, ms
}

// wireRecordInfo is the part of a message's "info" the allow-list reads.
type wireRecordInfo struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	ModelID string          `json:"modelID"`
	Summary bool            `json:"summary"`
	Error   json.RawMessage `json:"error"`
	Time    struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// wireRecordPart is a part, decoded as far as the allow-list needs. Fields a part
// of another type does not have are simply zero.
type wireRecordPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Synthetic bool            `json:"synthetic"`
	Ignored   bool            `json:"ignored"`
	Time      wireRecordTime  `json:"time"`
	CallID    string          `json:"callID"`
	Tool      string          `json:"tool"`
	State     wireRecordState `json:"state"`
	Mime      string          `json:"mime"`
	URL       dataURLHead     `json:"url"`
}

type wireRecordTime struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// wireRecordState is a tool part's state: one of four shapes, told apart by Status.
type wireRecordState struct {
	Status      string            `json:"status"`
	Input       json.RawMessage   `json:"input"`
	Output      string            `json:"output"`
	Error       string            `json:"error"`
	Time        wireRecordTime    `json:"time"`
	Attachments []json.RawMessage `json:"attachments"`
}

// wireRecordFile is one attachment of a completed tool.
type wireRecordFile struct {
	Type string      `json:"type"`
	Mime string      `json:"mime"`
	URL  dataURLHead `json:"url"`
}

// dataURLHead is a `data:<type>;base64,<payload>` URL reduced to what the page scan
// needs of an image: its type, the length of its payload and the first characters of
// it, which is enough to check the type — not a copy of it. A value of any other
// shape (a link, a path, escapes in the payload) leaves ok false. Decoding into it
// never fails the decode of the part it sits in.
type dataURLHead struct {
	ok   bool
	mime string
	n    int // payload length in base64 characters
	head string
}

// dataURLHeadChars is how much of the payload is kept: 24 characters decode to 18
// bytes, more than any signature http.DetectContentType reads for the types in
// fleet.RecordMediaTypes.
const dataURLHeadChars = 24

func (d *dataURLHead) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return nil
	}
	body := b[1 : len(b)-1]
	rest, ok := bytes.CutPrefix(body, []byte("data:"))
	if !ok {
		return nil
	}
	i := bytes.IndexByte(rest, ',')
	if i < 0 || i > 64 {
		return nil
	}
	mime, ok := strings.CutSuffix(string(rest[:i]), ";base64")
	if !ok || mime == "" {
		return nil
	}
	payload := rest[i+1:]
	// A backslash is a JSON escape, which base64 never contains: a payload holding
	// one cannot be decoded as it stands, so it is not referenced.
	if len(payload) < dataURLHeadChars || bytes.IndexByte(payload, '\\') >= 0 {
		return nil
	}
	d.ok, d.mime, d.n, d.head = true, mime, len(payload), string(payload[:dataURLHeadChars])
	return nil
}

// admitImage reports whether a file names an image this route can serve: inline
// base64, of a type this API names, the type the file says, within the size cap and
// starting with that type's signature. The media read checks all of it again.
func admitImage(mime string, u dataURLHead) bool {
	if !u.ok || u.mime != mime {
		return false
	}
	allowed := false
	for _, t := range fleet.RecordMediaTypes {
		if t == mime {
			allowed = true
		}
	}
	if !allowed {
		return false
	}
	if size := u.n / 4 * 3; size <= 0 || size > fleet.MaxRecordMediaBytes {
		return false
	}
	head, err := base64.StdEncoding.DecodeString(u.head)
	return err == nil && http.DetectContentType(head) == mime
}

// mediaRef is what a `media` reference holds: a place in one session's record,
// bound to the conversation it was read from. It holds no content and no path.
type mediaRef struct {
	V string `json:"v"`
	S string `json:"s"` // session id
	C int64  `json:"c"` // the session's start, ms
	M string `json:"m"` // message id
	P int    `json:"p"` // part index
	A int    `json:"a"` // index in a tool's attachments, -1 for the part itself
}

const mediaRefVersion = "om1"

func (r mediaRef) encode() string {
	r.V = mediaRefVersion
	b, _ := json.Marshal(r)
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseMediaRef returns the place a reference names, and false for a value that is
// not a reference this reader issued.
func parseMediaRef(ref string) (mediaRef, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(ref)
	if err != nil {
		return mediaRef{}, false
	}
	var r mediaRef
	if json.Unmarshal(raw, &r) != nil || r.V != mediaRefVersion || r.S == "" || r.M == "" || r.P < 0 || r.A < -1 {
		return mediaRef{}, false
	}
	// Re-encoding must give back the value: a reference with extra fields or
	// another spelling is not one this reader issued.
	if r.encode() != ref {
		return mediaRef{}, false
	}
	return r, true
}

// maxCallIDBytes bounds a tool call's id. A longer one is not the runtime's.
const maxCallIDBytes = 256

// classifyRecordMessage reduces one raw message to a recordMessage. It is the
// allow-list. A message whose shape cannot be read for an id and a role is neither
// an entry nor an anchor.
func classifyRecordMessage(raw json.RawMessage, sc recordScope) recordMessage {
	var m struct {
		Info  wireRecordInfo    `json:"info"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		var lenient struct {
			Info struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			} `json:"info"`
		}
		if json.Unmarshal(raw, &lenient) != nil {
			return recordMessage{}
		}
		// Readable as an id and a role and nothing more: nothing of it is read, and it
		// is finished, so it is passed over and never blocks what follows it.
		return recordMessage{id: lenient.Info.ID, assistant: lenient.Info.Role == "assistant", errored: true}
	}
	rec := recordMessage{id: m.Info.ID, assistant: m.Info.Role == "assistant", completed: m.Info.Time.Completed}
	if rec.id == "" || (m.Info.Role != "assistant" && m.Info.Role != "user") {
		return rec
	}
	rec.errored = len(m.Info.Error) > 0 && string(m.Info.Error) != "null"
	if rec.assistant {
		rec.admitted = !rec.errored && m.Info.ModelID != ""
	} else {
		rec.admitted = true
	}
	rec.parts = make([]recordPart, len(m.Parts))
	created := m.Info.Time.Created
	for i, p := range m.Parts {
		var wp wireRecordPart
		if err := json.Unmarshal(p, &wp); err != nil {
			continue // a part that cannot be classified is left out, and only it
		}
		if rec.assistant {
			rec.parts[i] = classifyAssistantPart(wp, i, len(m.Parts), m.Info, sc, rec.id)
		} else {
			rec.parts[i] = classifyUserPart(wp, i, created, sc, rec.id)
		}
	}
	return rec
}

func msAt(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// partAt is the runtime's own timestamp on a part: when it ended, else when it began,
// else the message's.
func partAt(t wireRecordTime, fallback int64) time.Time {
	switch {
	case t.End > 0:
		return msAt(t.End)
	case t.Start > 0:
		return msAt(t.Start)
	}
	return msAt(fallback)
}

func classifyUserPart(wp wireRecordPart, idx int, created int64, sc recordScope, id string) recordPart {
	switch wp.Type {
	case "text":
		if wp.Synthetic || wp.Ignored || strings.TrimSpace(wp.Text) == "" {
			return recordPart{}
		}
		text, cut := cutTurnText(wp.Text)
		return recordPart{kind: partEntry, settled: true, entry: fleet.RecordMessage{
			At: msAt(created), Kind: fleet.RecordKindText, Role: fleet.RecordRoleUser, Text: text, Truncated: cut}}
	case "file":
		if !admitImage(wp.Mime, wp.URL) {
			return recordPart{}
		}
		return recordPart{kind: partEntry, settled: true, entry: fleet.RecordMessage{
			At: msAt(created), Kind: fleet.RecordKindImage, Role: fleet.RecordRoleUser,
			Media: mediaRef{S: sc.session, C: sc.created, M: id, P: idx, A: -1}.encode()}}
	}
	return recordPart{}
}

func classifyAssistantPart(wp wireRecordPart, idx, total int, info wireRecordInfo, sc recordScope, id string) recordPart {
	finished := info.Time.Completed > 0
	// A part is settled once the runtime has written its end, or has moved on to a
	// part after it, or finished the message.
	settled := finished || wp.Time.End > 0 || idx < total-1
	switch wp.Type {
	case "text":
		if wp.Synthetic || wp.Ignored || strings.TrimSpace(wp.Text) == "" {
			return recordPart{}
		}
		kind := fleet.RecordKindText
		if info.Summary {
			kind = fleet.RecordKindSummary
		}
		text, cut := cutTurnText(wp.Text)
		return recordPart{kind: partEntry, settled: settled, entry: fleet.RecordMessage{
			At: partAt(wp.Time, info.Time.Created), Kind: kind, Role: fleet.RecordRoleAssistant,
			Text: text, Model: info.ModelID, Truncated: cut}}
	case "reasoning":
		if info.Summary || strings.TrimSpace(wp.Text) == "" {
			return recordPart{}
		}
		text, cut := cutTurnText(wp.Text)
		return recordPart{kind: partEntry, settled: settled, entry: fleet.RecordMessage{
			At: partAt(wp.Time, info.Time.Created), Kind: fleet.RecordKindReasoning, Role: fleet.RecordRoleAssistant,
			Text: text, Model: info.ModelID, Truncated: cut}}
	case "tool":
		if info.Summary {
			return recordPart{}
		}
		return classifyToolPart(wp, info, sc, id, idx)
	}
	return recordPart{}
}

// classifyToolPart: a call, as the runtime recorded it. Its `at` is the runtime's
// time for when the call began, so it is the same in every state.
func classifyToolPart(wp wireRecordPart, info wireRecordInfo, sc recordScope, id string, idx int) recordPart {
	if wp.CallID == "" || wp.Tool == "" || len(wp.CallID) > maxCallIDBytes {
		return recordPart{}
	}
	switch wp.State.Status {
	case "pending", "running", "completed", "error":
	default:
		return recordPart{}
	}
	start := wp.State.Time.Start
	if start <= 0 {
		start = info.Time.Created
	}
	input, cut := recordToolInput(wp.State.Input)
	p := recordPart{
		kind:    partTool,
		settled: wp.State.Status != "pending", // a call is only a call once its input is in
		callID:  wp.CallID,
		status:  wp.State.Status,
		tool: fleet.RecordMessage{
			At: msAt(start), Kind: fleet.RecordKindTool, Role: fleet.RecordRoleAssistant, Model: info.ModelID,
			Truncated: cut, Tool: &fleet.RecordTool{ID: wp.CallID, Name: wp.Tool, Input: input}},
	}
	switch wp.State.Status {
	case "completed":
		p.result, p.resultCut = cutTurnText(wp.State.Output)
		p.hasResult = true
		end := wp.State.Time.End
		if end <= 0 {
			end = start
		}
		for a, rawFile := range wp.State.Attachments {
			var f wireRecordFile
			if json.Unmarshal(rawFile, &f) != nil || f.Type != "file" || !admitImage(f.Mime, f.URL) {
				continue
			}
			p.attachment = append(p.attachment, fleet.RecordMessage{
				At: msAt(end), Kind: fleet.RecordKindImage, Role: fleet.RecordRoleAssistant, Model: info.ModelID,
				Media: mediaRef{S: sc.session, C: sc.created, M: id, P: idx, A: a}.encode()})
		}
	case "error":
		p.result, p.resultCut = cutTurnText(wp.State.Error)
		p.hasResult = p.result != ""
	}
	return p
}

// recordToolInput returns a call's input as the runtime recorded it, compacted. An
// input over the cap cannot be cut as JSON, so it becomes a JSON string of the
// leading part of its text, and the message says it was cut.
func recordToolInput(raw json.RawMessage) (json.RawMessage, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return nil, false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, false
	}
	if buf.Len() <= fleet.MaxRecordFieldBytes {
		return json.RawMessage(buf.Bytes()), false
	}
	cut, _ := cutTurnText(buf.String())
	b, err := json.Marshal(cut)
	if err != nil {
		return nil, false
	}
	return b, true
}
