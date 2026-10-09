package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Token usage, read from the runtime's own record (muster #285).
//
// # What the record says, measured rather than assumed
//
// Every assistant entry carries `message.usage` with the request's own counts
// (`input_tokens`, `output_tokens`, `cache_read_input_tokens`,
// `cache_creation_input_tokens`). Three properties decide how it is read:
//
//   - One API message is written as SEVERAL entries when it holds several
//     content blocks (a text block, then each tool call), and each of those
//     entries repeats the message's usage. Measured on a live record: 219
//     assistant entries for 82 distinct message ids, up to 13 entries for one.
//     Summing entries would overstate a session several-fold, so the sum is
//     over message ids, and a repeated id contributes only what it adds.
//   - The usage is per request, and a turn is many requests. The session's
//     spend is the sum, never the last entry (the trial that found 63 thousand
//     logged against 1.19 million true).
//   - It carries no cost. Cost stays absent: this service does not price tokens.
//
// A turn is the span between two `system/turn_duration` markers — the same
// marker `turns` counts (docs/adr/111) — so LastTurn is the usage accumulated
// since the previous marker.
//
// # Incremental, and never blocking a poll on a big file
//
// The first read of a record is the whole file, and a long session's record is
// large. A memo per record keeps the running sum and the byte offset it is
// complete up to, so every later read is the appended tail only. A first read
// of a file over usageSyncMaxBytes happens in the background and the block is
// absent ("not yet known") until it lands: absent is the honest answer for a
// number that has not been computed, and the alternative is a state read that
// stalls for as long as the file is long.
const usageSyncMaxBytes = 8 << 20

// usageEntry is the subset of one record line the usage reader decodes. Content
// is deliberately not among it: this reads what the runtime counted, never what
// was said.
type usageEntry struct {
	Type              string `json:"type"`
	Subtype           string `json:"subtype"`
	Timestamp         string `json:"timestamp"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Message           struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// usageRecentIDs bounds how many message ids the memo remembers to recognise a
// repeated one. The entries of one message are adjacent in the record, so a
// short window is enough and the memo does not grow with the session.
const usageRecentIDs = 256

// usageMemo is the running sum over one record.
type usageMemo struct {
	offset  int64 // bytes of the record fully consumed (always on a line boundary)
	total   fleet.UsageFigures
	turn    fleet.UsageFigures // since the last turn marker
	last    *fleet.TurnUsage
	asOf    time.Time
	counted bool // at least one usage-bearing entry has been read

	recent map[string]fleet.UsageFigures
	order  []string
}

func (m *usageMemo) add(id string, f fleet.UsageFigures, at time.Time) {
	delta := f
	if id != "" {
		if prev, ok := m.recent[id]; ok {
			delta = fleet.UsageFigures{
				Input:      maxZero(f.Input - prev.Input),
				Output:     maxZero(f.Output - prev.Output),
				CacheRead:  maxZero(f.CacheRead - prev.CacheRead),
				CacheWrite: maxZero(f.CacheWrite - prev.CacheWrite),
			}
			m.recent[id] = fleet.UsageFigures{
				Input: max64(f.Input, prev.Input), Output: max64(f.Output, prev.Output),
				CacheRead: max64(f.CacheRead, prev.CacheRead), CacheWrite: max64(f.CacheWrite, prev.CacheWrite),
			}
		} else {
			if m.recent == nil {
				m.recent = map[string]fleet.UsageFigures{}
			}
			m.recent[id] = f
			m.order = append(m.order, id)
			if len(m.order) > usageRecentIDs {
				delete(m.recent, m.order[0])
				m.order = m.order[1:]
			}
		}
	}
	m.total = m.total.Add(delta)
	m.turn = m.turn.Add(delta)
	m.counted = true
	if at.After(m.asOf) {
		m.asOf = at
	}
}

func maxZero(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// feed consumes one decoded record line.
func (m *usageMemo) feed(line []byte) {
	var e usageEntry
	if err := json.Unmarshal(line, &e); err != nil {
		// A torn or half-written line; the runtime appends and a reader may
		// arrive mid-write (the same allowance every reader of this store makes).
		return
	}
	at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	switch {
	case e.Type == "system" && e.Subtype == "turn_duration":
		if !m.turn.IsZero() {
			m.last = fleet.NewTurnUsage(m.turn, at)
		}
		m.turn = fleet.UsageFigures{}
	case e.Type == "assistant" && e.Message.Usage != nil && !e.IsAPIErrorMessage && e.Message.Model != "<synthetic>":
		u := e.Message.Usage
		m.add(e.Message.ID, fleet.UsageFigures{
			Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		}, at)
	}
}

// advance reads the record from m.offset to its last complete line.
// ok is false when the file cannot be read or is now shorter than the memo
// (truncated or replaced: the caller starts over).
func (m *usageMemo) advance(path string) (ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < m.offset {
		return false
	}
	if info.Size() == m.offset {
		return true
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, m.offset, info.Size()-m.offset), 64<<10)
	for {
		line, n, complete, rerr := readBoundedLine(r, recordLineLimit)
		if !complete {
			// A last line with no newline is the runtime mid-append: left for next time.
			return rerr == nil || errors.Is(rerr, io.EOF)
		}
		m.offset += int64(n)
		if line != nil {
			m.feed(line)
		}
	}
}

func (m *usageMemo) snapshot() *fleet.Usage {
	if !m.counted {
		return nil
	}
	var last *fleet.TurnUsage
	if m.last != nil {
		c := *m.last
		last = &c
	}
	return fleet.NewUsage(m.total, fleet.ConfidenceInferred, m.asOf.UTC(), last)
}

// usageReader holds the memos for every record this driver has read usage from.
type usageReader struct {
	mu       sync.Mutex
	memos    map[string]*usageMemo
	scanning map[string]bool
}

// read answers the usage of the record at path: nil when it cannot yet say.
func (u *usageReader) read(path string) *fleet.Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.memos == nil {
		u.memos = map[string]*usageMemo{}
		u.scanning = map[string]bool{}
	}
	m := u.memos[path]
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if m == nil {
		m = &usageMemo{}
		if info.Size() > usageSyncMaxBytes {
			u.scanAsyncLocked(path)
			return nil
		}
	} else if info.Size()-m.offset > usageSyncMaxBytes {
		u.scanAsyncLocked(path)
		return m.snapshot()
	}
	if !m.advance(path) {
		// The record shrank or vanished under the memo: start over, never trust it.
		m = &usageMemo{}
		if !m.advance(path) {
			delete(u.memos, path)
			return nil
		}
	}
	u.memos[path] = m
	return m.snapshot()
}

// scanAsyncLocked starts one background scan of path, if none is running. The
// caller holds u.mu.
func (u *usageReader) scanAsyncLocked(path string) {
	if u.scanning[path] {
		return
	}
	u.scanning[path] = true
	prior := u.memos[path]
	go func() {
		var m *usageMemo
		if prior != nil {
			c := *prior
			c.recent = copyRecent(prior.recent)
			c.order = append([]string(nil), prior.order...)
			m = &c
		} else {
			m = &usageMemo{}
		}
		ok := m.advance(path)
		u.mu.Lock()
		defer u.mu.Unlock()
		delete(u.scanning, path)
		if ok {
			u.memos[path] = m
		}
	}()
}

func copyRecent(in map[string]fleet.UsageFigures) map[string]fleet.UsageFigures {
	out := make(map[string]fleet.UsageFigures, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// usageFor answers #285's usage for one session, given its already-resolved
// conversation record. nil when no record store is configured, the conversation
// is not resolved, or the figures are not yet known — never a zero block.
func (d *Driver) usageFor(cwd string, conv *fleet.ConversationRef) *fleet.Usage {
	if d.conversations == nil || conv == nil || !conv.Known {
		return nil
	}
	return d.usage.read(d.conversations.recordPath(cwd, conv.ID))
}

// upgradeUsageFromRecord is usageFor for State's shape (a bare SessionState with
// no pre-resolved Conversation), mirroring upgradeTurnsFromRecord.
func (d *Driver) upgradeUsageFromRecord(ctx context.Context, st fleet.SessionState, cwd, name string, created time.Time, paneID string, pid int) fleet.SessionState {
	if d.conversations == nil {
		return st
	}
	ref := d.conversations.lookup(conversationKey{pane: paneID, created: created}, cwd, name, created,
		processGeneration{pid: pid}, d.liveConversationSource(ctx, pid, cwd))
	st.Usage = d.usageFor(cwd, ref)
	return st
}
