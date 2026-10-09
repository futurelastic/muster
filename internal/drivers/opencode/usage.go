package opencode

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Token usage, read from the runtime's own message record (muster #285).
//
// # What is summed, and why not the message's own figures
//
// An assistant message that took several steps (a model request per tool round)
// carries one step-finish part per step with that step's tokens and cost, and
// the message's own `tokens` hold the LAST step's. Cost, measured, is per step
// as well. Summing messages' own tokens, or taking the newest, understates a
// run by 2–20×. So this driver sums the step-finish parts, and falls back to a
// message's own figures only for a message that has no step records at all —
// never both for one message, which would count it twice.
//
// Reasoning tokens are reported apart from output and are carried as their own
// figure; cost is whatever the runtime reported, in the unit it used. Nothing
// is priced here.
//
// # A turn
//
// A turn is the run of assistant messages after one user message. LastTurn is
// the newest such run that has finished: while the session is working, the
// run in progress is in the cumulative figures but not yet "the last turn".
//
// # Not re-reading the whole history every time
//
// The first read fetches the history; each later read fetches a short tail and
// splices it onto what is remembered at the first message both have, widening
// the window until they overlap. Only the tail can still be changing (an
// unfinished message), so everything before the overlap is taken as settled.
const (
	usageTailWindow = 32
	usageTailGrowth = 4
)

// usageMsg is what is remembered of one message: no content, only the figures
// and the facts that place it in a turn.
type usageMsg struct {
	id        string
	user      bool
	assistant bool
	finished  bool // the runtime stamped the message completed
	figures   fleet.UsageFigures
	has       bool // the message reported any usage at all
	at        time.Time
}

// usageMemo is the remembered history of one session.
type usageMemo struct {
	mu   sync.Mutex
	msgs []usageMsg
	have bool // at least one full read has landed
}

func (d *Driver) usageMemoFor(id string) *usageMemo {
	d.usageMu.Lock()
	defer d.usageMu.Unlock()
	if d.usageMemos == nil {
		d.usageMemos = map[string]*usageMemo{}
	}
	m := d.usageMemos[id]
	if m == nil {
		m = &usageMemo{}
		d.usageMemos[id] = m
	}
	return m
}

// usageOf reads (or refreshes) the usage of one session. nil whenever the
// figures are not known: the read failed, or the history carries no usage — a
// best-effort enrichment on a state that was read successfully, the shape
// lastTurnFailure takes, so a failed message read never turns a good state
// into an error. idle says whether the session has no turn in progress, which
// decides whether the newest turn is "last".
func (d *Driver) usageOf(ctx context.Context, srv *server, id string, idle bool) *fleet.Usage {
	m := d.usageMemoFor(id)
	m.mu.Lock()
	defer m.mu.Unlock()

	limit := usageTailWindow
	if !m.have {
		limit = 0 // the whole history, once
	}
	for {
		path := "/session/" + url.PathEscape(id) + "/message"
		if limit > 0 {
			path = fmt.Sprintf("%s?limit=%d", path, limit)
		}
		var wire []wireMessage
		if err := d.do(ctx, srv, "GET", path, nil, &wire); err != nil {
			return m.snapshot(idle) // stale beats absent; AsOf says how stale
		}
		fresh := make([]usageMsg, 0, len(wire))
		for _, w := range wire {
			fresh = append(fresh, usageOfMessage(w))
		}
		whole := limit == 0 || len(wire) < limit
		if whole {
			m.msgs = fresh
			m.have = true
			break
		}
		if at := m.indexOf(fresh); at >= 0 {
			m.msgs = append(m.msgs[:at:at], fresh...)
			break
		}
		limit *= usageTailGrowth
	}
	return m.snapshot(idle)
}

// indexOf finds where the fetched tail overlaps the memory: the position in
// m.msgs of the fetched window's first message. -1 when it is not there, which
// means the window did not reach back far enough.
func (m *usageMemo) indexOf(fresh []usageMsg) int {
	if len(fresh) == 0 || fresh[0].id == "" {
		return -1
	}
	for i := len(m.msgs) - 1; i >= 0; i-- {
		if m.msgs[i].id == fresh[0].id {
			return i
		}
	}
	return -1
}

// usageOfMessage reduces one wire message to what is remembered of it.
func usageOfMessage(w wireMessage) usageMsg {
	u := usageMsg{
		id:        w.Info.ID,
		user:      w.Info.Role == "user",
		assistant: w.Info.Role == "assistant",
		finished:  w.Info.Time.Completed > 0,
	}
	switch {
	case w.Info.Time.Completed > 0:
		u.at = time.UnixMilli(w.Info.Time.Completed)
	case w.Info.Time.Created > 0:
		u.at = time.UnixMilli(w.Info.Time.Created)
	}
	if !u.assistant {
		return u
	}
	var steps int
	for _, p := range w.Parts {
		if p.Type != "step-finish" && p.Type != "step_finish" {
			continue
		}
		steps++
		if f, ok := figuresOf(p.Tokens, p.Cost); ok {
			u.figures = u.figures.Add(f)
			u.has = true
		}
	}
	if steps == 0 {
		if f, ok := figuresOf(w.Info.Tokens, w.Info.Cost); ok {
			u.figures, u.has = f, true
		}
	}
	return u
}

// figuresOf converts a wire breakdown. ok is false when the runtime reported
// neither tokens nor cost — nothing to count, as against a reported zero.
func figuresOf(t *wireTokens, cost *float64) (fleet.UsageFigures, bool) {
	var f fleet.UsageFigures
	if t == nil && cost == nil {
		return f, false
	}
	if t != nil {
		f.Input, f.Output, f.CacheRead, f.CacheWrite = t.Input, t.Output, t.Cache.Read, t.Cache.Write
		if t.Reasoning != nil {
			r := *t.Reasoning
			f.Reasoning = &r
		}
	}
	if cost != nil {
		c := *cost
		f.Cost = &c
	}
	return f, true
}

// snapshot builds the block from what is remembered. nil when no message has
// reported usage: "not yet known", not a zero block.
func (m *usageMemo) snapshot(idle bool) *fleet.Usage {
	var total fleet.UsageFigures
	var asOf time.Time
	counted := false
	for _, x := range m.msgs {
		if !x.has {
			continue
		}
		counted = true
		total = total.Add(x.figures)
		if x.at.After(asOf) {
			asOf = x.at
		}
	}
	if !counted {
		return nil
	}

	// Turns, newest first: the run of assistant messages after each user message.
	var last *fleet.TurnUsage
	end := len(m.msgs)
	for i := len(m.msgs) - 1; i >= -1 && last == nil; i-- {
		if i >= 0 && !m.msgs[i].user {
			continue
		}
		run := m.msgs[i+1 : end]
		newest := end == len(m.msgs)
		end = i
		if len(run) == 0 {
			continue
		}
		// The newest run is still in progress unless the session is idle; an
		// older one is over by construction. Any unfinished message in the run
		// means the turn has not ended.
		open := newest && !idle
		var sum fleet.UsageFigures
		var at time.Time
		reported := false
		for _, x := range run {
			if !x.assistant {
				continue
			}
			if !x.finished {
				open = true
			}
			if x.has {
				reported = true
				sum = sum.Add(x.figures)
			}
			if x.at.After(at) {
				at = x.at
			}
		}
		if open || !reported {
			continue
		}
		last = fleet.NewTurnUsage(sum, at.UTC())
	}
	return fleet.NewUsage(total, fleet.ConfidenceObserved, asOf.UTC(), last)
}

// usageSnapshot is what is remembered of a session's usage, with no request
// made: List's answer, which must stay one status read per server. nil until a
// State read or a turn's end has filled the memory in.
func (d *Driver) usageSnapshot(id string, idle bool) *fleet.Usage {
	d.usageMu.Lock()
	m := d.usageMemos[id]
	d.usageMu.Unlock()
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.have {
		return nil
	}
	return m.snapshot(idle)
}

// forgetUsage drops a session's remembered history once the session is gone.
func (d *Driver) forgetUsage(id string) {
	d.usageMu.Lock()
	delete(d.usageMemos, id)
	d.usageMu.Unlock()
}
