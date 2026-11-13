package fleet

import "encoding/json"

// Ack is what interrupt(), close() and discard() return (§3's operations
// table).
//
// The spec names this type but never gives it a shape — unlike
// DeliveryReceipt, which is fully specified. This is the shape this
// transcription settled on, matching the HTTP wire's own description of
// these two calls (api-http.md §3.3): both return 202 Accepted and "express
// intent" only; confirmation of what actually happened arrives later as a
// state change on the event stream (§4). Ack therefore carries only whether
// the request was accepted for processing — never a status of its own,
// since a driver that reported one here would be promising synchronous
// completion, which §5.6 ("degrade, never emulate") forbids a driver from
// promising when it cannot deliver it.
//
// rename() does NOT return this — see RenameAck below for why that
// operation earns its own shape instead of being squeezed into this one.
//
// Note is the one addition, and it is absent on every ordinary answer. A close
// of a session whose runtime could not be asked (its own confirmation failed
// or was not available) still goes ahead where this service holds the
// session's record — it stops what it started and drops the record — and says
// so here, in prose a person reads; a caller never parses it (muster #473).
type Ack struct {
	Accepted bool   `json:"accepted"`
	Note     string `json:"note,omitempty"`
}

// RenameAck is rename()'s own response (muster #222), not the bare Ack
// above. The reason is that rename's id half is NOT intent-only the way
// Ack's doctrine requires: by the time a driver's Rename returns, the
// multiplexer-level id change has already happened or it has not — unlike
// interrupt/close, whose real completion is confirmed later, only, on the
// event stream. Title is the separate, honestly-possibly-unresolved half:
// whether the runtime's own idea of its title (on a substrate that keeps
// one apart from the id) was brought along too. Reporting it here would
// break Ack's doctrine; reporting it as a value that admits "pending" does
// not, because nothing is being promised as already delivered.
type RenameAck struct {
	Accepted bool `json:"accepted"`

	// Title is nil when nothing is stated about the runtime's own title at
	// all — a peer built before this field existed, or a decode of an
	// unrecognised shape from one built AFTER it but ahead of this build
	// (see UnmarshalJSON). Never a claim that the title half does not
	// apply — that is TitleNotApplicable, a real, present value — nor a
	// claim that it failed. A consumer reads nil as "not stated" (§5.7),
	// the same rule DeliveryReceipt.Delivery already follows.
	Title *TitleSync `json:"title,omitempty"`
}

// renameAckWire is RenameAck's decode shape: Title stays raw so a shape
// this build cannot make sense of degrades to "absent" rather than failing
// the whole decode.
type renameAckWire struct {
	Accepted bool            `json:"accepted"`
	Title    json.RawMessage `json:"title,omitempty"`
}

// UnmarshalJSON is lenient about Title precisely the way
// DeliveryReceipt.UnmarshalJSON is lenient about an unrecognised Route
// (delivery.go): the rename this ack reports on has ALREADY HAPPENED on
// whichever side produced it. A caller that failed to decode the whole ack
// over an unrecognised title field would have to guess whether to retry —
// which risks the exact name collision rename's own corroboration exists to
// catch — merely because a future build added a status this one does not
// know yet. The Accepted half, which is what a caller acts on, must never
// be put at risk by the Title half failing to parse.
func (a *RenameAck) UnmarshalJSON(b []byte) error {
	var w renameAckWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*a = RenameAck{Accepted: w.Accepted}
	if len(w.Title) == 0 {
		return nil
	}
	var t TitleSync
	if err := json.Unmarshal(w.Title, &t); err == nil {
		a.Title = &t
	}
	// A Title that fails to decode (unknown status, or any other
	// incoherent shape TitleSync.UnmarshalJSON refuses) is silently left
	// absent rather than failing this whole decode — see this method's own
	// doc comment for why.
	return nil
}
