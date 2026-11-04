package tmux

import (
	"context"
	"sync"
	"time"
)

// One process-table snapshot for the whole machine (muster #404).
//
// # The cost this bounds, measured
//
// Agent discovery (agentprocess.go) reads the whole process table to answer a
// listing. That was one `ps -A -ww` per List, and List is not called once per
// request: the event stream calls it once per notification burst for EVERY
// subscriber, and a request, a reconcile and a stream can overlap. On a busy
// machine (about a thousand processes, a dozen subscribers) a `ps` child was
// alive in most one-second samples of the service, and its cost grows with the
// number of processes on the machine — so the service got slower exactly when
// the machine was busiest.
//
// The table is a fact about the MACHINE, not about a listing or a subscriber.
// So it is taken once and shared: every List inside procTableTTL of the last
// read reuses it, whoever asks, and callers that arrive while a read is running
// wait for that read instead of starting their own. The cost is a fixed number
// of `ps` per unit of time, whatever the number of sessions or subscribers.
//
// # What a snapshot is allowed to say, and when it is not trusted
//
// The only thing read from the table is each session's agent, which is the
// `--agent` on a runtime's command line. An argv does not change after the
// process starts, so a snapshot a few seconds old gives the same answer for
// every runtime that existed when it was taken. Two things can differ, and
// both are bounded by procTableTTL:
//
//   - a runtime that started or exited since: its agent appears or disappears
//     one snapshot later;
//   - a shell left in the foreground, or a runtime brought to it, changes which
//     process the foreground rule picks.
//
// Neither makes a listing incomplete. "Complete" (§5.7) is about a SOURCE that
// could not be read, and a snapshot that was read is read. What it must not do
// is report "no agent" for a session it never saw: a pane created after the
// snapshot has a root pid the snapshot does not contain. That is evidence the
// snapshot is too old FOR THIS LISTING, so a snapshot at least procTableMinAge
// old is retaken when it lacks a live pane's root. The minimum age is what keeps
// that rule bounded: the worst case is one read a second, not one per call.
//
// # A failed read
//
// A read that failed is remembered for procTableErrTTL, long enough that a burst
// shares it rather than each caller starting a `ps` that is about to fail the
// same way, short enough that the next listing after a blip tries again. The
// listing is degraded exactly as before (processAgents reports ok=false).
//
// A read that failed because ITS CALLER gave up (a deadline, a cancelled
// request) says nothing about the machine and is never remembered: the callers
// waiting on it take their own turn.
const (
	// procTableTTL is how long a snapshot answers every listing. The agent is
	// identity, not liveness; the event stream's own bound for record-derived
	// fields (streamRecordMaxAge) is the same five seconds.
	procTableTTL = 5 * time.Second
	// procTableMinAge is how old a snapshot must be before a pane it does not
	// contain is allowed to retake it.
	procTableMinAge = time.Second
	// procTableErrTTL is how long a failed read is shared.
	procTableErrTTL = time.Second
)

// procIndex is a parsed process table, indexed once so a listing's walk costs
// the size of the sessions' own subtrees, not of the table.
type procIndex struct {
	byPID    map[int]processRow
	children map[int][]int
}

func indexProcessTable(table []processRow) *procIndex {
	idx := &procIndex{
		byPID:    make(map[int]processRow, len(table)),
		children: make(map[int][]int, len(table)),
	}
	for _, p := range table {
		idx.byPID[p.pid] = p
		idx.children[p.ppid] = append(idx.children[p.ppid], p.pid)
	}
	return idx
}

// lacksAny reports whether some root pid is not in the snapshot.
func (x *procIndex) lacksAny(roots map[string]int) bool {
	for _, pid := range roots {
		if _, ok := x.byPID[pid]; !ok {
			return true
		}
	}
	return false
}

// procTableCache holds the machine's last process-table read.
type procTableCache struct {
	mu    sync.Mutex
	idx   *procIndex
	err   error
	at    time.Time // when the read STARTED: the snapshot is no younger than that
	valid bool
	// flight is non-nil while a read is running; it is closed when that read ends.
	flight chan struct{}
}

// processTable returns the machine's process table for a listing whose live
// panes have these root pids. See the file comment for when a held snapshot is
// reused and when it is retaken.
func (d *Driver) processTable(ctx context.Context, roots map[string]int) (*procIndex, error) {
	c := &d.procTable
	for {
		c.mu.Lock()
		if c.valid {
			age := d.now().Sub(c.at)
			switch {
			case c.err != nil:
				if age >= 0 && age < procTableErrTTL {
					err := c.err
					c.mu.Unlock()
					return nil, err
				}
			case age >= 0 && age < procTableTTL && !(age >= procTableMinAge && c.idx.lacksAny(roots)):
				idx := c.idx
				c.mu.Unlock()
				return idx, nil
			}
		}
		if ch := c.flight; ch != nil {
			// A read is already running: wait for it rather than start another,
			// but only as long as this caller itself is willing to wait.
			c.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		c.flight = ch
		started := d.now()
		c.mu.Unlock()

		out, err := d.psRun(ctx, d.psBin, processTableArgs...)
		var idx *procIndex
		if err == nil {
			idx = indexProcessTable(parseProcessTable(string(out)))
		}

		c.mu.Lock()
		c.flight = nil
		if err == nil || ctx.Err() == nil {
			c.idx, c.err, c.at, c.valid = idx, err, started, true
		}
		c.mu.Unlock()
		close(ch)
		return idx, err
	}
}
