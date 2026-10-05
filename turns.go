package fleet

import (
	"errors"
	"time"
)

// Turn is one thing a session's agent said: the text the runtime recorded as
// written by the agent itself, and when.
//
// # What a Turn is, and what it is not (muster #258)
//
// This is the one place this API returns content a SESSION produced rather
// than content the RUNTIME reported about itself (session-abstraction.md §5.8,
// narrowed by docs/adr/258-assistant-turns-read.md). The boundary is the whole
// of the type, so it is stated here where a future field would be added:
//
//   - IN: the text blocks of a record entry the runtime wrote as an assistant
//     message of the session's own agent.
//   - OUT, always: tool calls and tool results, file contents the agent read,
//     the messages a human or another session sent in, system and hook output,
//     the agent's reasoning blocks, a sub-agent's chatter, and the runtime's own
//     synthetic notices (an API error is the runtime speaking, not the agent).
//
// A field that would carry any OUT category is not an extension of this type;
// it is a new ruling. There is deliberately no Kind, no Role and no Raw here:
// each would be a place for the next category to slip in unreviewed.
type Turn struct {
	// At is the runtime's own timestamp on the entry, not the time it was
	// read. UTC.
	At time.Time `json:"at"`

	// Text is what the agent wrote, bounded by MaxTurnTextBytes.
	Text string `json:"text"`

	// Truncated is true when Text was cut at MaxTurnTextBytes. Absent
	// otherwise, so the common turn carries no extra field.
	Truncated bool `json:"truncated,omitempty"`
}

// TurnsPage is one read of a session's assistant turns, oldest first.
type TurnsPage struct {
	// Turns is in the order the agent wrote them. Empty (never null) when
	// there is nothing new, which is a normal answer to a poll.
	Turns []Turn `json:"turns"`

	// Next is an opaque cursor: pass it back as `since` to read what the agent
	// wrote after this page. It is returned even when Turns is empty, so a
	// poller always has a place to resume from. Do not parse it.
	Next string `json:"next"`

	// Pending is the agent's own text drawn directly above a prompt that is open
	// right now (muster #266). Absent whenever no prompt is open, and whenever
	// the screen cannot be read as agent text with certainty.
	Pending *PendingTurn `json:"pending,omitempty"`
}

// PendingTurn is agent text the runtime has drawn but not yet recorded.
//
// # Why it exists
//
// An assistant message that holds a text block and an open question is written
// to the record only once the question is answered, so a client rendering the
// conversation from Turns shows the question without the explanation it refers
// to. The text is on the screen, directly above the dialog; this is that text,
// for as long as the dialog is up.
//
// # The boundary is Turn's, read from a different place
//
// Only the contiguous block of the agent's own prose immediately above the
// dialog's opening rule. A block that holds a tool call, tool output, an
// inbound message, a status line or anything else the classifier cannot tie to
// the agent's own writing is left out whole: this fails to absent, never to a
// guess. Like Turn, there is deliberately no Kind and no Raw.
//
// It is NOT a transcript entry. The text is as the screen draws it — one row
// per line, the runtime's own indentation removed — so a paragraph the pane
// wrapped is several lines here, and the text the record later holds can differ
// in its line breaks and markup. A client that renders both must replace this
// with the real turn when the prompt resolves, and never merge them.
type PendingTurn struct {
	// ObservedAt is when this service read the screen. Not a timestamp the
	// runtime wrote — there is none for text it has not recorded — which is why
	// it is not called At.
	ObservedAt time.Time `json:"observedAt"`

	// Text is the agent's text above the prompt, bounded by MaxTurnTextBytes.
	Text string `json:"text"`

	// Truncated is true when Text is not the whole block: its head ran above the
	// captured window, or it was cut at MaxTurnTextBytes. Absent otherwise.
	Truncated bool `json:"truncated,omitempty"`

	// Source is always "screen". It is a field so a client can tell this from a
	// Turn without knowing which list it came out of, and so a later source (the
	// record itself, once written) has somewhere to differ.
	Source string `json:"source"`

	// Nonce is the open prompt's own nonce (SessionPrompt.Nonce), so a client can
	// tie this text to the prompt card it explains, and drop it when the nonce
	// changes.
	Nonce string `json:"nonce"`
}

// PendingSourceScreen is PendingTurn.Source for text read off the screen.
const PendingSourceScreen = "screen"

// TurnsQuery is what a caller asks of a session's assistant turns.
type TurnsQuery struct {
	// Since is a cursor from a previous TurnsPage.Next. Empty means "the most
	// recent Limit turns".
	Since string

	// Limit bounds the page. Zero means DefaultTurnsLimit; more than
	// MaxTurnsLimit is refused rather than silently clamped, because a caller
	// that asked for 500 and got 100 would conclude the session had written
	// 100.
	Limit int
}

const (
	// DefaultTurnsLimit is the page size when the caller names none.
	DefaultTurnsLimit = 20

	// MaxTurnsLimit is the largest page a caller may ask for.
	MaxTurnsLimit = 100

	// MaxTurnTextBytes bounds one Turn's Text. A single assistant entry can be
	// as large as the agent chose to write, and a response is the wrong place
	// to find out how large.
	MaxTurnTextBytes = 64 << 10
)

// ErrNoTurnRecord is returned when the session exists but the runtime's record
// of its conversation could not be identified or opened: a session created a
// moment ago whose record is not written yet, one resumed in a way the record
// cannot be tied back to, or a record the service cannot read. It is the
// absence of a source, not an empty conversation — a conversation with no agent
// text is a TurnsPage with no Turns (§5.7) — and the service maps it to
// not_found with Retryable set.
var ErrNoTurnRecord = errors.New("fleet: no readable conversation record could be identified for this session")
