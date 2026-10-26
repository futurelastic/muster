package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ConversationRef locates the record the RUNTIME keeps of a session's
// conversation — the identifier it filed that record under, which is not any
// identifier this service assigns.
//
// # Why this is worth a field, independent of ever reading the record
//
// Every local source in this service ultimately quotes the process's own
// announcement about itself. The screen is what the runtime chose to paint;
// the status is this driver's reading of that paint; a delivery receipt is the
// same surface read twice. When the thing being asked is the thing that is
// broken, all of those agree and all of them are wrong together — measured
// here at its sharpest: 51 of 52 sessions read healthy while the account
// underneath every one of them was refusing work.
//
// The runtime writes its conversation record for its OWN purposes, unasked,
// without knowing anyone is watching. That makes it an independent witness
// rather than another echo, and it is the first source in this service that is
// not the process describing itself. Knowing WHICH record belongs to a session
// is therefore worth having even if nothing ever opens it: two sessions
// claiming one record, or a live session with no record at all, are facts about
// identity that no amount of screen reading can produce.
//
// # Three states, because absence and failure are different answers (§5.7)
//
//	field absent (nil)      nobody looked — no record store on this substrate,
//	                        or lookup is not configured. Not a claim about the
//	                        session.
//	Known false + Evidence  we looked and could not tell, and the evidence says
//	                        why. This is a real answer.
//	Known true  + Source    we can name the record, and Source says how that
//	                        was learned.
//
// The middle state is not hypothetical. Measured over a live fleet, one session
// in thirteen had several equally possible records and had to be refused.
//
// # Why Source exists at all
//
// Because a caller that cannot tell a read value from a MATCHED one will
// corroborate against a guess and believe it is evidence — which is this type's
// own motivating failure, arriving one level up. §2.3 already separates a
// structured read from a screen guess (Confidence), and §4.3 separates a peer's
// own declaration from a floor nobody confirmed (CapabilitySource). This is the
// third instance of the same problem, so it takes the same shape rather than
// inventing a fourth.
type ConversationRef struct {
	// Known is false when a lookup happened and produced no answer. It is
	// never the way to say "no lookup happened" — that is the absent field.
	Known bool `json:"known"`

	// ID is the runtime's own identifier for the conversation, present only
	// when Known.
	ID string `json:"id,omitempty"`

	// Source is how the identifier was learned. Required when Known, absent
	// otherwise.
	Source ConversationSource `json:"source,omitempty"`

	// Evidence is prose for humans, present in BOTH the resolved and the
	// unresolved case — unlike the Known/Reason pairs elsewhere in this
	// package, where a reason only exists for a "no".
	//
	// A "yes" needs prose here too, because two derivations of equal
	// confidence do not exist: "the only record carrying this session's name"
	// and "the only one left after two others were ruled out" are different
	// strengths of the same answer, and a caller weighing whether to act on
	// the identifier is entitled to know which it got.
	//
	// Do not parse it (§2.3). Branch on Known and Source.
	Evidence string `json:"evidence"`
}

// ConversationSource is how an identifier was learned. A closed set, decoded
// strictly, for the same reason as Status and Confidence: an unrecognised
// value must be an error rather than a silent default.
//
// # Why both members were named before both were produced
//
// Both are written now: the terminal driver derives (and captures, for a create
// that dictates the id, muster #224), and the opencode driver captures from the
// create response (muster #378). It was named before anything wrote it because
// this service federates, peers have already been caught running different
// vintages of this code, and a strict decoder makes a NEW member a wire break
// for whichever peer is older — it would reject the whole session rather than
// one field. Naming the second value early cost a line; naming it late costs the
// incident that Build exists to prevent.
type ConversationSource string

const (
	// ConversationDerived means the service matched the record to the
	// session rather than being told which it was. The match is not a wild
	// guess — it reads back, out of a file the runtime wrote, the very name
	// this service passed on the command line at creation — but it is still
	// a match, and it can be wrong in ways a dictated value cannot.
	ConversationDerived ConversationSource = "derived"

	// ConversationCaptured means the driver observed the identifier as the
	// session was created, with nothing to match — first produced by a
	// create carrying SessionSpec.ConversationId (muster #224): the
	// driver told the runtime which id to start under, so there is nothing
	// to derive and no title to match against. The opencode driver produces
	// it for every session, from the id the runtime's create response names
	// (muster #378).
	ConversationCaptured ConversationSource = "captured"
)

func (s ConversationSource) valid() bool {
	return s == ConversationDerived || s == ConversationCaptured
}

func (s ConversationSource) MarshalJSON() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid ConversationSource", string(s))
	}
	return json.Marshal(string(s))
}

// UnmarshalJSON rejects the empty string along with anything else outside the
// set: an absent provenance is not the same fact as "derived", and coercing it
// would hand a caller a matched value wearing no label.
func (s *ConversationSource) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := ConversationSource(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid ConversationSource", raw)
	}
	*s = v
	return nil
}

// ResolvedConversation names the record a session's conversation is kept in,
// and how that was learned.
//
// Prefer this and UnresolvedConversation over a struct literal, for the reason
// ObservedState and InferredState exist: a hand-filled literal can typo the
// provenance into a value that lies about how the identifier was obtained, and
// these two constructors are the only places that decision has to be made
// correctly.
func ResolvedConversation(id string, source ConversationSource, evidence string) *ConversationRef {
	return &ConversationRef{Known: true, ID: id, Source: source, Evidence: evidence}
}

// UnresolvedConversation records that a lookup happened and produced nothing,
// with the reason it produced nothing. It is a real answer — never use it for
// "no lookup was attempted", which is the absent field.
func UnresolvedConversation(evidence string) *ConversationRef {
	return &ConversationRef{Evidence: evidence}
}

// ValidateConversationId reports whether id is shaped like a UUID a runtime
// can be launched under: 8-4-4-4-12 hexadecimal, the same shape
// internal/drivers/tmux already requires of every conversation id it reads
// back out of a runtime's own store (#180 L5). Checked here, in the fleet
// package every driver already imports, so "not a UUID" is refused at the API
// boundary (muster #224) before the value ever reaches a driver that
// would pass it straight to a runtime's command line.
func ValidateConversationId(id string) error {
	if !uuidShaped(id) {
		return fmt.Errorf("fleet: conversationId %q is not UUID-shaped (8-4-4-4-12 hexadecimal)", id)
	}
	return nil
}

// uuidShaped reports whether s is 8-4-4-4-12 hexadecimal — copied rather than
// imported from internal/drivers/tmux's own identically-named, unexported
// helper: this package is imported BY every driver (including tmux), so a
// dependency the other way is not available, and the shape is small and
// stable enough that duplicating it once costs less than restructuring which
// package owns it.
func uuidShaped(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
	}
	return true
}

// ErrConversationRefIncoherent is returned when a ref would state something it
// cannot support — an identifier with no provenance, a provenance with no
// identifier, or an answer with no evidence at all.
var ErrConversationRefIncoherent = errors.New("fleet: a ConversationRef must carry an id and a source when known, neither when not, and evidence either way")

func (r ConversationRef) coherent() bool {
	if r.Evidence == "" {
		return false
	}
	if r.Known {
		return r.ID != "" && r.Source.valid()
	}
	return r.ID == "" && r.Source == ""
}

type conversationRefWire ConversationRef

// MarshalJSON refuses an incoherent ref rather than emitting one.
//
// The failure being prevented is specific: a ref that says known and carries no
// id reads, to every client, as an identifier that happens to be empty — and a
// client that then compares two empty identifiers concludes two sessions share
// a conversation. Refusing to encode is loud; encoding it is silently wrong in
// exactly the direction this field was added to close.
func (r ConversationRef) MarshalJSON() ([]byte, error) {
	if !r.coherent() {
		return nil, ErrConversationRefIncoherent
	}
	return json.Marshal(conversationRefWire(r))
}

// UnmarshalJSON applies the same rule to what a peer sends. A federated read is
// not a more trustworthy source than our own encoder; it is a less trustworthy
// one, since it may be running code this build has never seen.
func (r *ConversationRef) UnmarshalJSON(b []byte) error {
	var wire conversationRefWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	got := ConversationRef(wire)
	if !got.coherent() {
		return ErrConversationRefIncoherent
	}
	*r = got
	return nil
}
