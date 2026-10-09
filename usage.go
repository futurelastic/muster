package fleet

// UsageFigures is the arithmetic form of what a runtime reports it spent: the
// four token counts every runtime this service drives has, and the two figures
// only some report. It is what drivers add up; the wire forms (Usage and
// TurnUsage) carry the same six numbers flat.
//
// Everything here is the RUNTIME's own account of itself (§5.8) — never a
// number this service computed from a price table. Cost in particular is
// whatever the runtime itself said it cost, in the unit it said it in (a
// runtime that reports a price reports US dollars; this type does not
// convert), and is absent for a runtime that reports none.
type UsageFigures struct {
	// Input is tokens the model read that were not served from a cache.
	Input int64
	// Output is tokens the model wrote, as the runtime counts them. Where a
	// runtime counts its reasoning tokens inside this figure (Reasoning is then
	// absent) they are in here; where it counts them apart, they are not.
	Output int64
	// CacheRead is tokens served from the runtime's prompt cache.
	CacheRead int64
	// CacheWrite is tokens written into the runtime's prompt cache.
	CacheWrite int64
	// Reasoning is reasoning tokens the runtime reports SEPARATELY from Output,
	// ADDITIONAL to it. Nil when the runtime does not split them out — which is
	// not "zero reasoning": it may be counted inside Output.
	Reasoning *int64
	// Cost is what the runtime itself reports the requests cost. Nil when it
	// reports none, never zero for "unknown".
	Cost *float64
}

// Add returns the sum of u and o. Reasoning and Cost are present in the result
// when either side has one: a figure one side lacked contributes nothing rather
// than erasing the other's.
func (u UsageFigures) Add(o UsageFigures) UsageFigures {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	if u.Reasoning != nil || o.Reasoning != nil {
		var a, b int64
		if u.Reasoning != nil {
			a = *u.Reasoning
		}
		if o.Reasoning != nil {
			b = *o.Reasoning
		}
		s := a + b
		u.Reasoning = &s
	} else {
		u.Reasoning = nil
	}
	if u.Cost != nil || o.Cost != nil {
		var a, b float64
		if u.Cost != nil {
			a = *u.Cost
		}
		if o.Cost != nil {
			b = *o.Cost
		}
		s := a + b
		u.Cost = &s
	} else {
		u.Cost = nil
	}
	return u
}

// Sub returns the figures in u that are not in o, for a driver that holds a
// running total and the total as of an earlier moment. Counts are clamped at
// zero: a runtime that rewrote a record downward is not a negative spend.
func (u UsageFigures) Sub(o UsageFigures) UsageFigures {
	clamp := func(a, b int64) int64 {
		if a < b {
			return 0
		}
		return a - b
	}
	u.Input = clamp(u.Input, o.Input)
	u.Output = clamp(u.Output, o.Output)
	u.CacheRead = clamp(u.CacheRead, o.CacheRead)
	u.CacheWrite = clamp(u.CacheWrite, o.CacheWrite)
	if u.Reasoning != nil {
		var b int64
		if o.Reasoning != nil {
			b = *o.Reasoning
		}
		v := clamp(*u.Reasoning, b)
		u.Reasoning = &v
	}
	if u.Cost != nil {
		var b float64
		if o.Cost != nil {
			b = *o.Cost
		}
		v := *u.Cost - b
		if v < 0 {
			v = 0
		}
		u.Cost = &v
	}
	return u
}

// IsZero reports whether every count is zero and no optional figure is
// present. A turn with no usage at all is not worth a block.
func (u UsageFigures) IsZero() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 &&
		u.Reasoning == nil && u.Cost == nil
}

// Usage is what a session has spent, as its runtime reports it (muster #285).
//
// # Cumulative, and the SUM of every step
//
// The figures are the total over every request the runtime made for this
// session, not the last request's. A runtime reports usage per request (a turn
// is many requests: one per tool round), and reading only the last understates
// a run by an order of magnitude — measured 2–20×, in one run 63 thousand
// logged against 1.19 million true. A driver sums every step.
//
// # Absent is "not yet known", never zero
//
// A driver declaring DriverCapabilities.ReportsUsage that has not read a figure
// yet — no record found, nothing recorded — leaves the block off. Zero is a
// finding: the runtime reported, and nothing was spent. Reasoning and Cost
// follow the same rule field by field: absent means the runtime does not report
// that figure, not that it was nil-valued.
//
// # Only what the runtime reports
//
// Nothing here is derived from a price table, and a driver that would have to
// guess a cost leaves Cost off. Tokens and cost are the runtime's account of
// itself (§5.8), not content the session produced.
type Usage struct {
	Input      int64    `json:"input"`
	Output     int64    `json:"output"`
	CacheRead  int64    `json:"cacheRead"`
	CacheWrite int64    `json:"cacheWrite"`
	Reasoning  *int64   `json:"reasoning,omitempty"`
	Cost       *float64 `json:"cost,omitempty"`

	// Source says how the driver learned the figures: observed from a
	// structured API, inferred from a record the runtime writes to disk. The
	// same closed set as SessionState.Confidence, for the same reason.
	Source Confidence `json:"source"`

	// AsOf is the moment of the runtime's own newest entry counted in these
	// figures — not the time this service read them. A reader seeing figures
	// that stopped moving can tell a session that stopped spending from a reader
	// that stopped reading.
	AsOf Timestamp `json:"asOf"`

	// LastTurn is the same figures for the most recent COMPLETED turn, so a
	// consumer attributing spend to turns reads it off the state it already
	// receives — a turn ending moves the cumulative figures and this together,
	// in one session.state event. Absent until a turn has completed, and whenever
	// the driver cannot tell where a turn begins.
	LastTurn *TurnUsage `json:"lastTurn,omitempty"`
}

// TurnUsage is the usage of one completed turn: every request between the
// previous turn's end and this one's (muster #285).
type TurnUsage struct {
	// At is when the runtime recorded the turn as finished.
	At         Timestamp `json:"at"`
	Input      int64     `json:"input"`
	Output     int64     `json:"output"`
	CacheRead  int64     `json:"cacheRead"`
	CacheWrite int64     `json:"cacheWrite"`
	Reasoning  *int64    `json:"reasoning,omitempty"`
	Cost       *float64  `json:"cost,omitempty"`
}

// Figures returns the arithmetic form of u's cumulative figures.
func (u Usage) Figures() UsageFigures {
	return UsageFigures{u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Reasoning, u.Cost}
}

// Figures returns the arithmetic form of t's figures.
func (t TurnUsage) Figures() UsageFigures {
	return UsageFigures{t.Input, t.Output, t.CacheRead, t.CacheWrite, t.Reasoning, t.Cost}
}

// NewUsage builds the cumulative block from summed figures.
func NewUsage(f UsageFigures, source Confidence, asOf Timestamp, last *TurnUsage) *Usage {
	return &Usage{
		Input: f.Input, Output: f.Output, CacheRead: f.CacheRead, CacheWrite: f.CacheWrite,
		Reasoning: f.Reasoning, Cost: f.Cost, Source: source, AsOf: asOf, LastTurn: last,
	}
}

// NewTurnUsage builds one turn's figures.
func NewTurnUsage(f UsageFigures, at Timestamp) *TurnUsage {
	return &TurnUsage{
		At: at, Input: f.Input, Output: f.Output, CacheRead: f.CacheRead, CacheWrite: f.CacheWrite,
		Reasoning: f.Reasoning, Cost: f.Cost,
	}
}

// sameUsage compares two usage blocks for MateriallyDiffers. Figures only move
// when a request completes, so this fires at most once per request — not per
// repaint — and a consumer summing off the feed needs every one of them.
func sameUsage(a, b *Usage) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !sameFigures(a.Figures(), b.Figures()) {
		return false
	}
	if a.Source != b.Source {
		return false
	}
	if (a.LastTurn == nil) != (b.LastTurn == nil) {
		return false
	}
	if a.LastTurn != nil && (!a.LastTurn.At.Equal(b.LastTurn.At) ||
		!sameFigures(a.LastTurn.Figures(), b.LastTurn.Figures())) {
		return false
	}
	return true
}

func sameFigures(a, b UsageFigures) bool {
	if a.Input != b.Input || a.Output != b.Output || a.CacheRead != b.CacheRead || a.CacheWrite != b.CacheWrite {
		return false
	}
	if (a.Reasoning == nil) != (b.Reasoning == nil) || (a.Reasoning != nil && *a.Reasoning != *b.Reasoning) {
		return false
	}
	return (a.Cost == nil) == (b.Cost == nil) && (a.Cost == nil || *a.Cost == *b.Cost)
}
