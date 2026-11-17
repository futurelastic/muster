package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// SourceState is one machine's contribution status to a fanned-out query
// (§9).
type SourceState string

const (
	SourceOK           SourceState = "ok"
	SourceUnreachable  SourceState = "unreachable"
	SourceUnauthorized SourceState = "unauthorized"
	SourceDegraded     SourceState = "degraded"
)

func (s SourceState) valid() bool {
	switch s {
	case SourceOK, SourceUnreachable, SourceUnauthorized, SourceDegraded:
		return true
	default:
		return false
	}
}

func (s SourceState) MarshalJSON() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid SourceState", string(s))
	}
	return json.Marshal(string(s))
}

func (s *SourceState) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := SourceState(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid SourceState", raw)
	}
	*s = v
	return nil
}

// SourceStatus is one machine's contribution to a Collection (§9). A failed
// source contributes a SourceStatus, never a silent absence from Items —
// see §5.7: "there are no sessions on that machine" and "I could not reach
// that machine to ask" are different facts, trivially easy to collapse into
// the same empty list.
type SourceStatus struct {
	Machine    MachineId   `json:"machine"`
	Status     SourceState `json:"status"`
	Count      *int        `json:"count,omitempty"`
	Error      string      `json:"error,omitempty"`
	ObservedAt Timestamp   `json:"observedAt"`

	// Quotas lists the credentials on this machine that are refusing work, one
	// block each, ordered by credential name (#10, per credential since muster
	// #490). Empty means no credential this service knows of is blocked — not
	// that a credential it has never seen has quota. It replaces the single
	// `quota` this field used to be: a machine does not have one account, so a
	// single block could only be the machine's, and that is what stopped every
	// session when one credential ran out.
	//
	// It is the same fact StatusQuotaBlocked already reports per session,
	// surfaced here per machine, and readable without enumerating a single
	// session on it: a scheduler reads it to learn which credentials to stop
	// dispatching to, and dispatches to the rest.
	//
	// Deliberately a field of its own, not folded into Status. A machine can
	// be reachable and answering (Status: SourceOK) while every session on
	// one of its credentials is refused — reachable-and-answering and
	// willing-to-work are different facts. This envelope exists precisely so
	// a caller never has to infer one from the other; collapsing them here
	// would reintroduce, at the source level, exactly the confusion this
	// type already forbids at the session level. A caller that cannot tell
	// "this machine is gone" from "this machine is fine but a credential is
	// blocked" retries the wrong one forever.
	Quotas []QuotaBlock `json:"quotas,omitempty"`
}

// ErrCollectionNeedsSources is returned by NewCollection when constructed
// with zero sources. An empty Items with no Sources is, verbatim, "the
// exact bug the design exists to prevent" (§9): it is indistinguishable
// from "genuinely nothing out there" and from "could not reach anyone to
// ask." Even an unfederated, single-machine answer carries at least the
// self source — api-http.md §3.2: "a scope=local response carries exactly
// one SourceStatus."
var ErrCollectionNeedsSources = errors.New("fleet: a Collection must have at least one source")

// Collection is the envelope every plural response wraps (§9). Its fields
// are unexported and reached only through NewCollection and the accessors
// below, so the shape the spec calls out by name — items with no sources —
// cannot be produced by ordinary use of this package.
//
// This is not a complete guarantee: Go's zero value, Collection[T]{}, still
// exists and still has exactly that shape, because nothing in the language
// lets a struct forbid its own zero value. NewCollection is the
// enforcement point for every other path; collection_test.go tests both
// that it rejects an empty Sources slice and that decoding a wire envelope
// with items but no sources fails the same way.
type Collection[T any] struct {
	items    []T
	sources  []SourceStatus
	complete bool
	feed     *FeedPosition
}

// FeedPosition marks where a snapshot sits in a service's event sequence
// (§7.3), so a client can list once and then watch deltas rather than poll.
//
// # Why it is on the envelope and not a second call
//
// A mirror is a snapshot plus every event after it. The cursor was readable
// before this existed — from the health endpoint — but only as a SEPARATE
// request, and neither ordering is safe: read it after the listing and
// anything that happened in between is lost with nothing to notice, read it
// before and you are relying on an accident nobody wrote down. One response
// carrying both closes the question.
//
// # The cursor is stamped BEFORE the enumeration, deliberately
//
// So a snapshot may already contain changes newer than the cursor it carries,
// and replaying from that cursor re-applies them. That overlap is the point.
// Applying an event twice to a mirror keyed by session id changes nothing;
// missing one leaves a mirror that is wrong forever and cannot tell. The
// design is arranged to produce the recoverable failure — the same asymmetry
// §7.3 uses to insist a gap be announced rather than silently skipped.
//
// # Absent means "not a resume point", which is not the same as zero
//
// A service only advances its sequence while it is actually observing a
// driver; with nothing subscribed, the cursor is frozen while the world moves.
// A number handed out then would look resumable and would silently skip
// everything that happened before the first subscription. So it is omitted
// instead, and §5.7 applies as it always does: absence is a real answer, and a
// client that finds none must subscribe first and list second.
type FeedPosition struct {
	Cursor int64  `json:"cursor"`
	Epoch  string `json:"epoch"`
}

// NewCollection builds a Collection. It computes Complete itself rather
// than accepting it as a parameter: the spec (§9) says Complete is "false
// if any source failed to answer" but never says who computes it, and a
// caller-supplied bool is exactly the kind of value that can silently drift
// from what Sources actually reports — the same class of bug §9 warns a
// bare boolean invites callers to ignore, one level up.
//
// Complete is true iff every source's Status is SourceOK. SourceDegraded
// counts as not-fully-answered on the same footing as SourceUnreachable and
// SourceUnauthorized: a degraded source's data is present but not to be
// trusted at face value (§13.2), so treating it as "answered cleanly" would
// reintroduce the exact confidence-flattening §5.6 forbids. The spec's
// prose does not settle this explicitly; it is recorded as a decision here
// and in session-abstraction.md §9.
func NewCollection[T any](items []T, sources []SourceStatus) (Collection[T], error) {
	if len(sources) == 0 {
		return Collection[T]{}, ErrCollectionNeedsSources
	}
	complete := true
	for _, s := range sources {
		if s.Status != SourceOK {
			complete = false
			break
		}
	}
	return Collection[T]{items: items, sources: sources, complete: complete}, nil
}

// Items returns the collected values. May be empty even when Complete is
// true — that combination means "asked everyone, genuinely found nothing,"
// which is a different fact from any source being absent (§5.7).
func (c Collection[T]) Items() []T { return c.items }

// Sources returns every machine's contribution status. Never empty for a
// Collection built through NewCollection or decoded from the wire.
func (c Collection[T]) Sources() []SourceStatus { return c.sources }

// Complete reports whether every source answered ok. A caller that ignores
// this and reads only Items has made a choice, not an oversight (§9) — the
// field is deliberately impossible to miss at the top level.
func (c Collection[T]) Complete() bool { return c.complete }

// collectionWire is the JSON shape of Collection[T] (§9): items, sources,
// complete, all exported on the wire even though the Go type keeps them
// unexported for construction safety.
type collectionWire[T any] struct {
	Items    []T            `json:"items"`
	Sources  []SourceStatus `json:"sources"`
	Complete bool           `json:"complete"`
	Feed     *FeedPosition  `json:"feed,omitempty"`
}

func (c Collection[T]) MarshalJSON() ([]byte, error) {
	items := c.items
	if items == nil {
		items = []T{}
	}
	return json.Marshal(collectionWire[T]{Items: items, Sources: c.sources, Complete: c.complete, Feed: c.feed})
}

// UnmarshalJSON decodes a wire envelope through NewCollection, so a decoded
// Collection carries exactly the same guarantee a locally-built one does —
// including that Complete is recomputed from Sources rather than trusted
// verbatim from whichever peer produced the JSON. A different
// implementation's bug (or a differing reading of an ambiguous spec
// sentence) that emits a mismatched `complete` should not propagate into
// this process's view of the fleet.
func (c *Collection[T]) UnmarshalJSON(b []byte) error {
	var w collectionWire[T]
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	built, err := NewCollection(w.Items, w.Sources)
	if err != nil {
		return err
	}
	built.feed = w.Feed
	*c = built
	return nil
}

// WithFeed returns a copy of this Collection carrying the sequence position
// the snapshot was taken at. See FeedPosition for why it is stamped before the
// enumeration and why omitting it is a meaningful answer.
func (c Collection[T]) WithFeed(cursor int64, epoch string) Collection[T] {
	c.feed = &FeedPosition{Cursor: cursor, Epoch: epoch}
	return c
}

// Feed reports where this snapshot sits in the sequence, or nil when the
// answer is not a usable resume point.
func (c Collection[T]) Feed() *FeedPosition { return c.feed }
