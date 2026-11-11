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
	// RecordKindReasoning is the agent's reasoning, Text. The terminal runtime
	// emits it (muster #442).
	RecordKindReasoning RecordKind = "reasoning"
	// RecordKindTool is a tool call paired with its result, Tool. The terminal
	// runtime emits it (muster #442). A tool message whose Tool.ID a reader
	// already holds REPLACES the held one: the call is sent as soon as it is
	// recorded, with Status "running", and again, complete, when its result is.
	RecordKindTool RecordKind = "tool"
	// RecordKindImage is an image, Media. The terminal runtime emits it (muster
	// #442); the bytes are read through the media route, never from this entry.
	RecordKindImage RecordKind = "image"
	// RecordKindSummary is a summary the runtime wrote of earlier entries, Text.
	// The terminal runtime emits it for a compaction (muster #442).
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
// The keys are fixed by RecordFormat. Tool, Parent and Media were declared with
// the route so that the format-1 key set was settled in one place; the terminal
// runtime fills them since muster #442.
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

	// Tool is a call and its result paired by the reader, for the tool kind.
	Tool *RecordTool `json:"tool,omitempty"`

	// Parent is the id of the tool call this entry was made inside, when it
	// belongs to a delegated sub-task. Absent for a top-level entry.
	Parent string `json:"parent,omitempty"`

	// Model is the model that wrote an assistant entry, when the record says.
	Model string `json:"model,omitempty"`

	// Media is a reference to an image, for the image kind. It is read
	// separately (GET …/record/media/{ref}); image bytes are never inline here.
	// Opaque: never parse it, and never carry it to another session.
	Media string `json:"media,omitempty"`

	// Truncated is true when a field of this entry was cut at
	// MaxRecordFieldBytes. Absent otherwise. For a tool message the cut field may
	// be the input (which then reads as a JSON string holding the leading part of
	// the input's JSON text) or the result.
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

	// Status is RecordToolRunning while the record holds no result for the call,
	// and RecordToolCompleted or RecordToolError once it does, taken from the
	// result. A client that does not know a value shows it as it is.
	Status string `json:"status,omitempty"`
}

// The Status a tool message carries (muster #442).
const (
	// RecordToolRunning: the record holds the call and no result for it yet.
	RecordToolRunning = "running"
	// RecordToolCompleted: the result is in, and the runtime did not mark it an error.
	RecordToolCompleted = "completed"
	// RecordToolError: the result is in, and the runtime marked it an error
	// (including a call that was interrupted).
	RecordToolError = "error"
)

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

// RecordMedia is one image read through GET …/{id}/record/media/{ref} (muster
// #442): the bytes and their type. It is never part of a RecordPage.
type RecordMedia struct {
	// ContentType is one of the image types in RecordMediaTypes, and the bytes
	// have been checked to be that type.
	ContentType string

	// Data is the image, at most MaxRecordMediaBytes.
	Data []byte
}

// MaxRecordMediaBytes bounds one image a record reference can name, decoded. An
// image over it is not referenced at all, so a reference a page hands out is one
// the media route can serve.
const MaxRecordMediaBytes = 16 << 20

// RecordMediaTypes are the image types a record reference can name. The media
// route serves bytes under the caller's grant, so the set is the browser image
// types and nothing a viewer could be made to run.
var RecordMediaTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}
