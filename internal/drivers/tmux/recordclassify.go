package tmux

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
)

// The allow-list of the record route for this runtime (muster #442).
//
// # The boundary, and where it is enforced
//
// docs/adr/441-a-sessions-full-record-is-read-under-its-own-grant.md puts the
// boundary on what a record read may carry at the reader, and
// docs/adr/442-… lists what this reader now admits. This file is where the line is
// drawn, and nowhere downstream: the service, the relay and the wire type carry
// whatever classifyRecordLine returns without looking at it.
//
// One line of the runtime's file in, an ordered list of entries out:
//
//   - the agent's own prose, as `text`, by the SAME rules turns uses (assistantEntry,
//     turns.go): a line's text blocks are joined and sit where the first one was;
//   - the agent's reasoning, as `reasoning`, when it is written out (a redacted or
//     empty one is left out);
//   - a tool call (`tool_use`), as a call, and a tool result (`tool_result`), as a
//     result the scanner pairs with the call by exact id — any tool, never filtered
//     by name;
//   - what a person typed, under the allow-list humanTextEntry always had, now also
//     when the message carries images;
//   - a compaction summary, as `summary`;
//   - an image, as an `image` carrying a reference, never its bytes.
//
// Everything else — meta and hook output, attachments, queued placeholders, the
// runtime's own markup, a message from another session, an entry of a shape this
// code has not seen — is out. A field that does not have the shape this reader
// expects fails the whole line; a field it has never seen is not consulted, which
// is why each category it excludes is named. A line from the sub-task directory is
// classified by the same rules and carries its own scope (recScope).

// recordEntryLineLimit bounds one line of the record for this route. It is larger
// than the 1 MiB the other readers use because an image is stored inline, as
// base64, on the line that carries it, and a line the reader skips is a line whose
// tool result and images are lost with it. The page scan and the media read share
// it so a page never hands out a reference the media read cannot serve.
const recordEntryLineLimit = 16 << 20

// recEntryKind is what classifyRecordLine found at one place on a line.
type recEntryKind int

const (
	// recMessage is a finished message: text, reasoning, summary or image.
	recMessage recEntryKind = iota
	// recCall is a tool call, as the call alone: Tool.Status is running.
	recCall
	// recResult is a tool result, not yet attached to any call.
	recResult
)

// recResultData is what a tool result carried.
type recResultData struct {
	id        string
	text      string
	truncated bool
	isError   bool
}

// recEntry is one classified place on a line. A line yields them in the order of
// its blocks, and that order never depends on what other lines hold (the
// cursor's position inside a line counts entries, so it must not).
type recEntry struct {
	kind recEntryKind
	msg  fleet.RecordMessage // recMessage, recCall
	res  recResultData       // recResult

	// mediaPath names the image block a recMessage of kind image was made from
	// ("c3" or "c3.r1"): where the media read finds it again.
	mediaPath string
}

// recScope says which file a line is read from, and so which lines belong to it.
type recScope struct {
	// conv is the conversation the file belongs to.
	conv string
	// parent is the id of the tool call that started the sub-task whose file this
	// is; empty for the conversation file. Every message from the file carries it.
	parent string
	// agent is the sub-task's id for a line from the sub-task directory, empty
	// for the conversation file itself.
	agent string
	// ref makes the media reference for an image block of a line; nil when the
	// caller only wants to classify.
	ref func(path string) string
}

// recEnvelope is the part of a line every kind shares. Fields whose content this
// reader never reads are decoded into types that do not keep it.
type recEnvelope struct {
	Type                      string          `json:"type"`
	Timestamp                 string          `json:"timestamp"`
	IsSidechain               bool            `json:"isSidechain"`
	IsMeta                    bool            `json:"isMeta"`
	IsCompactSummary          bool            `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
	IsAPIErrorMessage         bool            `json:"isApiErrorMessage"`
	AgentID                   string          `json:"agentId"`
	SessionID                 string          `json:"sessionId"`
	ToolUseResult             recPresent      `json:"toolUseResult"`
	Origin                    json.RawMessage `json:"origin"`
	PromptSource              string          `json:"promptSource"`
	Message                   struct {
		Role    string     `json:"role"`
		Model   string     `json:"model"`
		Content recContent `json:"content"`
	} `json:"message"`
}

// recBlock is one content block. Everything a block might hold that is large and
// not wanted (an input is kept, a result's content is walked, an image's data is
// only measured) is typed so decoding it does not copy it.
type recBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   json.RawMessage `json:"is_error"`
	Content   recContent      `json:"content"`
	Source    recImage        `json:"source"`
}

// recPresent records that a field was there and not null, and nothing else: a
// tool's structured result can be megabytes and is never read.
type recPresent bool

func (p *recPresent) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	*p = recPresent(len(t) > 0 && !bytes.Equal(t, []byte("null")))
	return nil
}

// recContent is a message's content: a string, or a list of blocks. A value of any
// other shape, or a list whose blocks do not decode, leaves valid false and the
// line is dropped. It never fails the decode of the line it sits in, so a shape
// this reader cannot read costs one line's entries and not an error.
type recContent struct {
	valid  bool
	isStr  bool
	str    string
	blocks []recBlock
}

func (c *recContent) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return nil
	}
	switch t[0] {
	case '"':
		if json.Unmarshal(t, &c.str) == nil {
			c.isStr, c.valid = true, true
		}
	case '[':
		if json.Unmarshal(t, &c.blocks) == nil {
			c.valid = true
		}
	}
	return nil
}

// b64Head is the length of a base64 string and its first characters, which is all
// the page scan needs of an image: enough to check the type, not a copy of it.
type b64Head struct {
	ok   bool
	n    int
	head string
}

// b64HeadChars is how much of the data is kept: 24 characters decode to 18
// bytes, which is more than any signature http.DetectContentType reads for the
// types in fleet.RecordMediaTypes.
const b64HeadChars = 24

func (d *b64Head) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return nil
	}
	body := b[1 : len(b)-1]
	if len(body) < b64HeadChars || bytes.IndexByte(body[:b64HeadChars], '\\') >= 0 {
		return nil
	}
	d.ok, d.n, d.head = true, len(body), string(body[:b64HeadChars])
	return nil
}

// recImage is an image block's `source`. It never fails a decode; a source of a
// shape that is not understood is simply not an admissible image.
type recImage struct {
	present bool
	typ     string
	media   string
	data    b64Head
}

func (im *recImage) UnmarshalJSON(b []byte) error {
	var v struct {
		Type      json.RawMessage `json:"type"`
		MediaType json.RawMessage `json:"media_type"`
		Data      b64Head         `json:"data"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	im.present = true
	im.typ, im.media, im.data = rawString(v.Type), rawString(v.MediaType), v.Data
	return nil
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// admitImage reports whether an image block names an image this route can serve:
// inline base64, one of the media types, within the size cap, and starting with
// that type's signature. The media read checks the whole of it again.
func admitImage(im recImage) (string, bool) {
	if !im.present || im.typ != "base64" || !im.data.ok {
		return "", false
	}
	allowed := false
	for _, t := range fleet.RecordMediaTypes {
		if t == im.media {
			allowed = true
		}
	}
	if !allowed {
		return "", false
	}
	if size := im.data.n / 4 * 3; size <= 0 || size > fleet.MaxRecordMediaBytes {
		return "", false
	}
	head, err := base64.StdEncoding.DecodeString(im.data.head)
	if err != nil || http.DetectContentType(head) != im.media {
		return "", false
	}
	return im.media, true
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

// classifyRecordLine classifies one line of one file. entries is empty for a line
// that is not part of the conversation, or whose shape cannot be classified with
// certainty; at is the line's own timestamp.
func classifyRecordLine(line []byte, sc recScope) (entries []recEntry, at time.Time) {
	var e recEnvelope
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, time.Time{}
	}
	// Which lines a file may hold. The conversation file holds the conversation: a
	// sidechain line in it (an older runtime wrote sub-agents there) cannot be tied
	// to the call that started it, so it stays out. A sub-task file holds only its
	// own sub-task's lines.
	if sc.agent == "" {
		if e.IsSidechain {
			return nil, time.Time{}
		}
	} else if !e.IsSidechain || e.AgentID != sc.agent || (e.SessionID != "" && e.SessionID != sc.conv) {
		return nil, time.Time{}
	}
	if e.IsMeta {
		return nil, time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return nil, time.Time{}
	}
	ts = ts.UTC()
	switch e.Type {
	case "assistant":
		entries = classifyAssistant(&e, ts)
	case "user":
		entries = classifyUser(&e, ts, sc)
	}
	return entries, ts
}

// classifyAssistant: the agent speaking. The envelope rules are assistantEntry's.
func classifyAssistant(e *recEnvelope, ts time.Time) []recEntry {
	if e.Message.Role != "assistant" || e.IsAPIErrorMessage ||
		e.Message.Model == "" || e.Message.Model == "<synthetic>" || !e.Message.Content.valid || e.Message.Content.isStr {
		return nil
	}
	model := e.Message.Model
	var (
		out     []recEntry
		textIdx = -1
		parts   []string
	)
	for _, b := range e.Message.Content.blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			if textIdx < 0 {
				textIdx = len(out)
				out = append(out, recEntry{kind: recMessage, msg: fleet.RecordMessage{
					At: ts, Kind: fleet.RecordKindText, Role: fleet.RecordRoleAssistant, Model: model}})
			}
			parts = append(parts, b.Text)
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			text, truncated := capText(b.Thinking, fleet.MaxRecordFieldBytes)
			out = append(out, recEntry{kind: recMessage, msg: fleet.RecordMessage{
				At: ts, Kind: fleet.RecordKindReasoning, Role: fleet.RecordRoleAssistant,
				Text: text, Model: model, Truncated: truncated}})
		case "tool_use":
			if b.ID == "" || b.Name == "" || len(b.ID) > 256 {
				continue
			}
			input, truncated := recordToolInput(b.Input)
			out = append(out, recEntry{kind: recCall, msg: fleet.RecordMessage{
				At: ts, Kind: fleet.RecordKindTool, Role: fleet.RecordRoleAssistant, Model: model, Truncated: truncated,
				Tool: &fleet.RecordTool{ID: b.ID, Name: b.Name, Input: input, Status: fleet.RecordToolRunning}}})
		}
	}
	if textIdx >= 0 {
		text, truncated := capText(strings.Join(parts, "\n\n"), fleet.MaxRecordFieldBytes)
		out[textIdx].msg.Text, out[textIdx].msg.Truncated = text, truncated
	}
	return out
}

// recordToolInput returns a call's input as the runtime recorded it, compacted.
// An input over the cap cannot be cut as JSON, so it becomes a JSON string of the
// leading part of its text, and the message says it was cut.
func recordToolInput(raw json.RawMessage) (json.RawMessage, bool) {
	if jsonAbsent(raw) {
		return nil, false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, false
	}
	if buf.Len() <= fleet.MaxRecordFieldBytes {
		return json.RawMessage(buf.Bytes()), false
	}
	cut, _ := capText(buf.String(), fleet.MaxRecordFieldBytes)
	b, err := json.Marshal(cut)
	if err != nil {
		return nil, false
	}
	return b, true
}

// jsonAbsent reports whether a raw JSON field was missing or null.
func jsonAbsent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// classifyUser: a user-role line is a person's message, a tool's result, or a
// compaction summary, and nothing else.
func classifyUser(e *recEnvelope, ts time.Time, sc recScope) []recEntry {
	if e.Message.Role != "user" || !e.Message.Content.valid {
		return nil
	}
	c := e.Message.Content
	if e.IsCompactSummary {
		return classifySummary(e, ts)
	}
	if !c.isStr {
		results := 0
		for _, b := range c.blocks {
			if b.Type == "tool_result" {
				results++
			}
		}
		if results > 0 {
			// A tool's result is a user-role line. A line that mixes results with
			// anything else is not one this reader has seen.
			if results != len(c.blocks) {
				return nil
			}
			return classifyResults(c.blocks, ts, sc)
		}
	}
	return classifyHuman(e, ts, sc)
}

// classifySummary: the runtime's compaction summary. It is the model's text, so
// its role is assistant, though the runtime stores it as a user line.
func classifySummary(e *recEnvelope, ts time.Time) []recEntry {
	if bool(e.ToolUseResult) {
		return nil
	}
	c := e.Message.Content
	text := c.str
	if !c.isStr {
		parts := make([]string, 0, len(c.blocks))
		for _, b := range c.blocks {
			if b.Type != "text" {
				return nil
			}
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n\n")
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	text, truncated := capText(text, fleet.MaxRecordFieldBytes)
	return []recEntry{{kind: recMessage, msg: fleet.RecordMessage{
		At: ts, Kind: fleet.RecordKindSummary, Role: fleet.RecordRoleAssistant, Text: text, Truncated: truncated}}}
}

// classifyResults: a line of tool results, in block order: the result, then any
// image the tool returned, whose role is the assistant's (the call was the
// assistant's).
func classifyResults(blocks []recBlock, ts time.Time, sc recScope) []recEntry {
	var out []recEntry
	for bi, b := range blocks {
		if b.ToolUseID == "" || len(b.ToolUseID) > 256 {
			return nil
		}
		res := recResultData{id: b.ToolUseID, isError: strings.TrimSpace(string(b.IsError)) == "true"}
		var images []recEntry
		if b.Content.valid {
			if b.Content.isStr {
				res.text = b.Content.str
			} else {
				var parts []string
				for ri, rb := range b.Content.blocks {
					switch rb.Type {
					case "text":
						if strings.TrimSpace(rb.Text) != "" {
							parts = append(parts, rb.Text)
						}
					case "image":
						path := fmt.Sprintf("c%d.r%d", bi, ri)
						if ent, ok := imageEntry(rb, path, fleet.RecordRoleAssistant, ts, sc); ok {
							images = append(images, ent)
						}
					}
				}
				res.text = strings.Join(parts, "\n\n")
			}
		}
		res.text, res.truncated = capText(res.text, fleet.MaxRecordFieldBytes)
		out = append(out, recEntry{kind: recResult, res: res})
		out = append(out, images...)
	}
	return out
}

// imageEntry makes the entry for an image block, or reports that it is not one
// this route can serve (the rest of its line is unaffected).
func imageEntry(b recBlock, path string, role fleet.RecordRole, ts time.Time, sc recScope) (recEntry, bool) {
	if _, ok := admitImage(b.Source); !ok {
		return recEntry{}, false
	}
	m := fleet.RecordMessage{At: ts, Kind: fleet.RecordKindImage, Role: role}
	if sc.ref != nil {
		m.Media = sc.ref(path)
	}
	return recEntry{kind: recMessage, msg: m, mediaPath: path}, true
}

// classifyHuman is the allow-list for what a person typed. It admits only an entry
// the runtime wrote as a top-level user message, authored by a human at the
// keyboard (or queued there), whose content is text and images and nothing else.
func classifyHuman(e *recEnvelope, ts time.Time, sc recScope) []recEntry {
	// Anything shown only in the runtime's own viewer is not a person speaking to
	// this session, and a tool's structured result is recorded on a user line.
	if e.IsVisibleInTranscriptOnly || bool(e.ToolUseResult) {
		return nil
	}
	// Another session or the runtime sending in is not a human typing; format 1
	// has no field to attribute it, so it stays out.
	if !jsonAbsent(e.Origin) {
		var origin struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(e.Origin, &origin); err != nil || (origin.Kind != "" && origin.Kind != "human") {
			return nil
		}
	}
	if e.PromptSource != "" && e.PromptSource != "typed" && e.PromptSource != "queued" {
		return nil
	}
	c := e.Message.Content
	if c.isStr {
		return humanText(c.str, ts)
	}
	if len(c.blocks) == 0 {
		return nil
	}
	var (
		parts   []string
		textIdx = -1
		out     []recEntry
	)
	for bi, b := range c.blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			if textIdx < 0 {
				textIdx = len(out)
				out = append(out, recEntry{kind: recMessage})
			}
			parts = append(parts, b.Text)
		case "image":
			if ent, ok := imageEntry(b, fmt.Sprintf("c%d", bi), fleet.RecordRoleUser, ts, sc); ok {
				out = append(out, ent)
			}
		default:
			// A document, an unknown block or anything else drops the whole entry:
			// a message that mixes text with another kind is neither one nor the
			// other until that kind is read.
			return nil
		}
	}
	text := strings.Join(parts, "\n\n")
	if textIdx >= 0 {
		t := humanText(text, ts)
		if t == nil {
			// The text is the runtime's own markup: the message is not a person's.
			return nil
		}
		out[textIdx] = t[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// humanText is the text entry of a person's message, or nil when the text is empty
// or the runtime's own markup.
func humanText(text string, ts time.Time) []recEntry {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	for _, prefix := range runtimeMarkupPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return nil
		}
	}
	text, truncated := capText(text, fleet.MaxRecordFieldBytes)
	return []recEntry{{kind: recMessage, msg: fleet.RecordMessage{
		At: ts, Kind: fleet.RecordKindText, Role: fleet.RecordRoleUser, Text: text, Truncated: truncated}}}
}
