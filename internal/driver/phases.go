package driver

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Phase timings for a request (muster #450).
//
// A slow create used to be measurable only from the caller's side: the audit
// line said what happened and nothing about where the time went, so a 33 s
// create could be split (27 s in the terminal) only by a caller that happened
// to time its own phases. A request that wants its phases recorded carries a
// *Phases on its context; whatever runs under that context — the HTTP handler,
// a driver — notes how long a named step took, and the audit line prints them.
//
// It lives in this package, not in the service, so a driver can note a phase
// without importing the layer above it. Recording is best-effort by design: a
// context with no recorder (every unit test that calls a driver directly, every
// background caller) makes NotePhase a no-op, so adding a note to a driver can
// never change what the driver does.

// Phase is one named step and how long it took.
type Phase struct {
	Name string
	Took time.Duration
}

// Phases collects the phases of one request, in the order they finished.
// Safe for concurrent use: a handler and a driver call may both note.
type Phases struct {
	mu    sync.Mutex
	items []Phase
}

type phasesKey struct{}

// WithPhases returns ctx carrying p, so NotePhase under it records into p.
func WithPhases(ctx context.Context, p *Phases) context.Context {
	return context.WithValue(ctx, phasesKey{}, p)
}

// NotePhase records that the step called name started at since and has just
// finished. It does nothing when ctx carries no recorder.
func NotePhase(ctx context.Context, name string, since time.Time) {
	p, ok := ctx.Value(phasesKey{}).(*Phases)
	if !ok || p == nil {
		return
	}
	p.mu.Lock()
	p.items = append(p.items, Phase{Name: name, Took: time.Since(since)})
	p.mu.Unlock()
}

// All returns a copy of what was noted, in order.
func (p *Phases) All() []Phase {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Phase(nil), p.items...)
}

// Millis is a duration in whole milliseconds, rounded to nearest. The audit line
// reports every duration the same way so a reader never converts units.
func Millis(d time.Duration) int64 { return (d + time.Millisecond/2).Milliseconds() }

// Fields renders the phases as log fields, one `phase.<name>=<ms>` token each,
// space separated, in the order they finished, or "" when none were noted. A
// name noted twice appears twice: the line says what happened, not what was
// expected to.
func (p *Phases) Fields() string {
	all := p.All()
	if len(all) == 0 {
		return ""
	}
	var b strings.Builder
	for i, ph := range all {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("phase.")
		b.WriteString(ph.Name)
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(Millis(ph.Took), 10))
	}
	return b.String()
}
