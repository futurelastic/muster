package fleet

import (
	"encoding/json"
	"time"
)

// RecordFormat is the version of the RecordMessage shape (muster #441).
//
// It changes only when a key is renamed or removed, which is a new ruling. Later
// work adds KINDS under the same format, never new meanings for an old key, so a
// client reads a RecordPage against the format it knows and skips any Kind it
// does not recognise.
const RecordFormat = 1

// RecordKind says what one RecordMessage is. The set is closed per format, but a
// reader must skip a kind it has not heard of: a runtime that can read more of
// its record adds kinds without changing RecordFormat.
type RecordKind string

const (
	// RecordKindText is prose someone wrote: the Text field holds it.
	RecordKindText RecordKind = "text"
	// RecordKindReasoning is the agent's reasoning, Text. No driver produces it
	// yet.
	RecordKindReasoning RecordKind = "reasoning"
	// RecordKindTool is a tool call paired with its result, Tool. No driver
	// produces it yet.
	RecordKindTool RecordKind = "tool"
	// RecordKindImage is an image, Media. No driver produces it yet.
	RecordKindImage RecordKind = "image"
	// RecordKindSummary is a summary the runtime wrote of earlier entries, Text.
	// No driver produces it yet.
	RecordKindSummary RecordKind = "summary"
	// RecordKindSystem is the runtime speaking rather than a person or the
	// agent, Text. No driver produces it yet.
	RecordKindSystem RecordKind = "system"
)

// RecordRole says which side of the conversation wrote a RecordMessage.
type RecordRole string

const (
	RecordRoleUser      RecordRole = "user"
	RecordRoleAssistant RecordRole = "assistant"
)

// RecordMessage is one entry of a session's record: what was said or done, by
// whom, and when.
//
// # What this is, and why it is not a Turn (muster #441)
//
// A Turn (turns.go) is the narrow shape ruled in docs/adr/258-assistant-turns-read.md:
// the agent's own prose, bare {at, text}, deliberately with no Kind and no Role.
// This is the wider shape ruled in docs/adr/441-a-sessions-full-record-is-read-under-its-own-grant.md,
// served on its own route under its own grant, and it has both because a record
// is a conversation of more than one voice and more than one kind of entry.
//
// The keys are fixed by RecordFormat. Tool, Parent and Media are declared now so
// that the format-1 key set is settled in one place; the driver that fills them
// is a later change, and until then they are absent.
//
// What a driver puts INTO this type is the allow-list of that driver's reader
// (docs/adr/441-…, "What each runtime emits today"). Nothing downstream of the
// driver — the service, the relay, this type — filters or widens it.
type RecordMessage struct {
	// At is the runtime's own timestamp on the entry. UTC.
	At time.Time `json:"at"`

	// Kind is what this entry is.
	Kind RecordKind `json:"kind"`

	// Role is who wrote it.
	Role RecordRole `json:"role"`

	// Text is the prose, for the text, reasoning, summary and system kinds,
	// bounded by MaxRecordFieldBytes.
	Text string `json:"text,omitempty"`

	// Tool is a call and its result paired by the service, for the tool kind.
	Tool *RecordTool `json:"tool,omitempty"`

	// Parent is the id of the tool call this entry was made inside, when it
	// belongs to a delegated sub-task. Absent for a top-level entry.
	Parent string `json:"parent,omitempty"`

	// Model is the model that wrote an assistant entry, when the record says.
	Model string `json:"model,omitempty"`

	// Media is a reference to an image, for the image kind. It is read
	// separately; image bytes are never inline here.
	Media string `json:"media,omitempty"`

	// Truncated is true when a field of this entry was cut at
	// MaxRecordFieldBytes. Absent otherwise.
	Truncated bool `json:"truncated,omitempty"`
}

// RecordTool is a tool call and its result, for a message of kind tool.
type RecordTool struct {
	// ID is the runtime's id for the call; a RecordMessage.Parent names one.
	ID string `json:"id"`

	// Name is the tool's name.
	Name string `json:"name"`

	// Input is what the call was given, as the runtime recorded it.
	Input json.RawMessage `json:"input,omitempty"`

	// Result is what the call returned, absent while it has not.
	Result string `json:"result,omitempty"`

	// Status is the runtime's own word for how the call ended.
	Status string `json:"status,omitempty"`
}

// RecordPage is one read of a session's record, oldest first.
type RecordPage struct {
	// Messages is in the order the record holds them. Empty (never null) when
	// there is nothing new, which is a normal answer to a poll.
	Messages []RecordMessage `json:"messages"`

	// Next is an opaque cursor: pass it back as `since` to read what the record
	// holds after this page. It is returned even when Messages is empty, so a
	// poller always has a place to resume from. Do not parse it. It is a
	// different cursor from TurnsPage.Next and neither is accepted by the
	// other's route.
	Next string `json:"next"`

	// Format is RecordFormat.
	Format int `json:"format"`
}

// RecordQuery is what a caller asks of a session's record.
type RecordQuery struct {
	// Since is a cursor from a previous RecordPage.Next. Empty means "the most
	// recent Limit entries".
	Since string

	// Limit bounds the page. Zero means DefaultRecordLimit; more than
	// MaxRecordLimit is refused rather than silently clamped, for the reason
	// TurnsQuery gives.
	Limit int
}

const (
	// DefaultRecordLimit is the page size when the caller names none.
	DefaultRecordLimit = 20

	// MaxRecordLimit is the largest page a caller may ask for.
	MaxRecordLimit = 100

	// MaxRecordFieldBytes bounds one text field of one RecordMessage. It is the
	// same 64 KiB as a Turn's, for the same reason.
	MaxRecordFieldBytes = MaxTurnTextBytes
)
