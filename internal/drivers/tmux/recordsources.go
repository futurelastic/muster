package tmux

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Reading the record: sources, pairing, paging and the cursor (muster #442).
//
// docs/adr/442-… is the argument; this file is the mechanism. In short:
//
//   - A record is read from SOURCES: the conversation file and, in the directory
//     the runtime keeps beside it, one file per delegated sub-task. Every entry
//     from a sub-task file carries `parent`, the id of the call that started it,
//     which the runtime writes next to the file.
//   - A tool call and its result are two lines. A call is sent where it was
//     recorded; its result is folded in when the same page reads it too, and
//     otherwise the call goes out `running` and a LATER page sends it again,
//     complete, with the same tool id (an update a client applies by id). Nothing
//     about a pending call is carried in the cursor.
//   - The cursor holds a position in each source. A position is a byte offset and
//     the number of that line's entries already sent, so a page may end inside a
//     line.

const (
	// recordCursorVersion2 is the cursor that carries sub-task positions. The
	// first version (recordCursorVersion, "r1") is still accepted: it names only a
	// position in the conversation file.
	recordCursorVersion2 = "r2"

	// recordSourcesMax bounds how many sub-task files one read follows, and so how
	// long a cursor can get.
	recordSourcesMax = 64

	// recordWatermarkSlack is how far behind the moment a cursor is issued its
	// watermark sits. It absorbs the difference between the runtime's file times
	// and this process's clock.
	recordWatermarkSlack = 2 * time.Minute

	// recordMetaMax bounds the sidecar file that names a sub-task's parent call.
	recordMetaMax = 64 << 10
)

var errInvalidRecordCursor = errors.New("since is not a cursor this API issued")

// ---------------------------------------------------------------------------
// Positions and the cursor

// recPos is a place in one source: the byte offset of a line, and how many of the
// entries that line yields were already sent.
type recPos struct {
	off int64
	k   int
}

func (p recPos) less(o recPos) bool { return p.off < o.off || (p.off == o.off && p.k < o.k) }

func (p recPos) String() string {
	if p.k == 0 {
		return strconv.FormatInt(p.off, 10)
	}
	return strconv.FormatInt(p.off, 10) + "." + strconv.Itoa(p.k)
}

func parseRecPos(s string) (recPos, bool) {
	offS, kS, hasK := strings.Cut(s, ".")
	off, err := strconv.ParseInt(offS, 10, 64)
	if err != nil || off < 0 {
		return recPos{}, false
	}
	k := 0
	if hasK {
		k, err = strconv.Atoi(kS)
		if err != nil || k <= 0 {
			return recPos{}, false
		}
	}
	return recPos{off: off, k: k}, true
}

// recCursor is what a cursor says.
type recCursor struct {
	// legacy is a cursor of the first version: a position in the conversation file
	// and nothing about sub-tasks.
	legacy bool
	main   recPos
	// water is the watermark: the moment (less slack) the cursor was issued.
	water time.Time
	// subs are the positions the cursor names, by sub-task id.
	subs map[string]recPos
}

func validRecordID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// parseRecordCursor reads a cursor issued for this route against conv. A cursor
// that is not one this route issued — a turns cursor included — is a plain error;
// one issued against another conversation is a conflict, as turns' is.
func parseRecordCursor(cursor, conv string) (recCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return recCursor{}, errInvalidRecordCursor
	}
	parts := strings.SplitN(string(raw), "|", 5)
	var (
		cur     recCursor
		cursorC string
	)
	switch {
	case parts[0] == recordCursorVersion && len(parts) == 3:
		pos, ok := parseRecPos(parts[2])
		if !ok {
			return recCursor{}, errInvalidRecordCursor
		}
		cur = recCursor{legacy: true, main: pos}
		cursorC = parts[1]
	case parts[0] == recordCursorVersion2 && len(parts) == 5:
		pos, ok := parseRecPos(parts[2])
		ms, err := strconv.ParseInt(parts[3], 10, 64)
		if !ok || err != nil || ms < 0 {
			return recCursor{}, errInvalidRecordCursor
		}
		cur = recCursor{main: pos, water: time.UnixMilli(ms).UTC(), subs: map[string]recPos{}}
		if parts[4] != "" {
			for _, kv := range strings.Split(parts[4], ",") {
				id, ps, ok := strings.Cut(kv, ":")
				p, pok := parseRecPos(ps)
				if !ok || !pok || !validRecordID(id) {
					return recCursor{}, errInvalidRecordCursor
				}
				cur.subs[id] = p
			}
		}
		cursorC = parts[1]
	default:
		return recCursor{}, errInvalidRecordCursor
	}
	if cursorC != conv {
		return recCursor{}, fmt.Errorf(
			"%w: the cursor was issued for another conversation; this session's record has been replaced since (a /clear or a relaunch). Read without `since` to start again",
			fleet.ErrAmbiguousTarget)
	}
	return cur, nil
}

// recordCursor builds a cursor. subs are the sub-task positions to carry; at most
// recordSourcesMax are listed, the most recently written first.
func recordCursor(conv string, main recPos, now time.Time, subs []recListed) string {
	if len(subs) > recordSourcesMax {
		sort.SliceStable(subs, func(i, j int) bool { return subs[i].mtime.After(subs[j].mtime) })
		subs = subs[:recordSourcesMax]
	}
	sort.SliceStable(subs, func(i, j int) bool { return subs[i].id < subs[j].id })
	var sb strings.Builder
	for i, s := range subs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(s.id + ":" + s.pos.String())
	}
	water := now.Add(-recordWatermarkSlack).UnixMilli()
	if water < 0 {
		water = 0
	}
	payload := recordCursorVersion2 + "|" + conv + "|" + main.String() + "|" + strconv.FormatInt(water, 10) + "|" + sb.String()
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// recListed is a sub-task the cursor being built names.
type recListed struct {
	id    string
	pos   recPos
	mtime time.Time
}

// ---------------------------------------------------------------------------
// Sources

// recSource is one file a record read draws from.
type recSource struct {
	// id is empty for the conversation file, the sub-task's id otherwise.
	id       string
	path     string
	parent   string
	mtime    time.Time
	finished bool
}

func (s recSource) scope(conv string) recScope {
	sc := recScope{conv: conv, agent: s.id, parent: s.parent}
	return sc
}

// subTaskDir is where the runtime keeps the conversation's sub-task files.
func subTaskDir(convPath string) string {
	return strings.TrimSuffix(convPath, ".jsonl") + string(filepath.Separator) + "subagents"
}

// subTaskFile is a directory entry that might be a sub-task file: its name is the
// runtime's and it is a regular file. Nothing about it has been verified yet.
type subTaskFile struct {
	id    string
	mtime time.Time
}

// listSubTaskFiles lists the candidates of a conversation, by name and file time
// only: the sidecars are read for the ones a read actually needs, which on a
// session that has delegated hundreds of sub-tasks is a few.
func listSubTaskFiles(convPath string) []subTaskFile {
	dir := subTaskDir(convPath)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []subTaskFile
	for _, ent := range ents {
		name := ent.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		if !validRecordID(id) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, subTaskFile{id: id, mtime: fi.ModTime()})
	}
	return out
}

// loadSubTask is one sub-task file by id, if it can be read with certainty. A file
// qualifies only with certainty: its name is the runtime's (`agent-<id>.jsonl` with
// an id of plain characters), it is a regular file and not a link, and the sidecar
// the runtime writes beside it names the call that started it. A file that fails
// any of that is not read, and a file that is not read leaks nothing.
func loadSubTask(dir, id string) (recSource, bool) {
	if !validRecordID(id) {
		return recSource{}, false
	}
	path := filepath.Join(dir, "agent-"+id+".jsonl")
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return recSource{}, false
	}
	parent, finished, ok := readSubTaskMeta(filepath.Join(dir, "agent-"+id+".meta.json"))
	if !ok {
		return recSource{}, false
	}
	return recSource{id: id, path: path, parent: parent, mtime: fi.ModTime(), finished: finished}, true
}

// readSubTaskMeta reads the sidecar that names a sub-task's parent call.
func readSubTaskMeta(path string) (parent string, finished, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > recordMetaMax {
		return "", false, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, false
	}
	var m struct {
		ToolUseID string `json:"toolUseId"`
		Finished  bool   `json:"finished"`
	}
	if err := json.Unmarshal(b, &m); err != nil || !validRecordID(m.ToolUseID) || len(m.ToolUseID) > 256 {
		return "", false, false
	}
	return m.ToolUseID, m.Finished, true
}

// firstTimestamp is the time on the first line of a file, which never changes. It
// tells a sub-task that began after a watermark from one that began before it.
func firstTimestamp(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	line, _, complete, _ := readBoundedLine(bufio.NewReaderSize(f, 64<<10), recordEntryLineLimit)
	if !complete || line == nil {
		return time.Time{}, false
	}
	var e struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(line, &e) != nil {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

// ---------------------------------------------------------------------------
// Scanning one source

// recItem is one place of one source's scan: a message, a call, a result.
type recItem struct {
	msg    fleet.RecordMessage
	sortAt time.Time

	// where it is: the line, and which entry of it
	lineStart int64
	lineEnd   int64
	idx       int

	// call: a tool call, folded into by a later result of the same scan
	call bool

	// A result. hidden: the scan holds its call, so it completes that call and
	// sends nothing of its own (into is the call's index). orphan: the scan does
	// not hold the call, so the result is resolved against the file before it.
	hidden   bool
	into     int
	orphan   bool
	res      recResultData
	resolved bool
}

// recScan is one source's items from one scan, in file order.
type recScan struct {
	items []recItem
	// consumed is the offset just past the last complete line the scan finished.
	consumed int64
}

func (s recScan) visible() int {
	n := 0
	for i := range s.items {
		if !s.items[i].hidden {
			n++
		}
	}
	return n
}

// mediaRef is the reference an image entry carries: where in which source's file
// the image block sits, bound to the conversation it was read from. It holds no
// content and no path.
func mediaRef(conv, agent string, lineOff int64, path string) string {
	src := "main"
	if agent != "" {
		src = agent
	}
	return base64.RawURLEncoding.EncodeToString([]byte("m1|" + conv + "|" + src + "|" + strconv.FormatInt(lineOff, 10) + "|" + path))
}

// scanRecordSource reads whole lines of one file from `from` (skipping the first
// `skip` entries of the first line) to `to`, and classifies them. It stops after
// the line that brings the visible items to maxVisible (when that is above zero).
// A final line with no newline yet is the runtime mid-append and is left for the
// next read, never half-parsed.
func scanRecordSource(f *os.File, sc recScope, from int64, skip int, to int64, maxVisible int) (recScan, error) {
	out := recScan{consumed: from}
	if to <= from {
		return out, nil
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, from, to-from), 64<<10)
	pos := from
	visible := 0
	open := map[string]int{}
	first := true
	for {
		line, n, complete, rerr := readBoundedLine(r, recordEntryLineLimit)
		if !complete {
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return out, rerr
			}
			return out, nil
		}
		lineStart := pos
		pos += int64(n)
		out.consumed = pos
		if line != nil {
			lsc := sc
			lsc.ref = func(path string) string { return mediaRef(sc.conv, sc.agent, lineStart, path) }
			entries, ts := classifyRecordLine(line, lsc)
			for i, ent := range entries {
				if first && i < skip {
					continue
				}
				it := recItem{sortAt: ts, lineStart: lineStart, lineEnd: pos, idx: i}
				switch ent.kind {
				case recMessage:
					it.msg = ent.msg
					visible++
				case recCall:
					it.msg, it.call = ent.msg, true
					if _, dup := open[ent.msg.Tool.ID]; !dup {
						open[ent.msg.Tool.ID] = len(out.items)
					}
					visible++
				case recResult:
					it.res = ent.res
					if ci, ok := open[ent.res.id]; ok {
						it.hidden, it.into = true, ci
						delete(open, ent.res.id)
					} else {
						it.orphan = true
						visible++
					}
				}
				if it.msg.Kind != "" {
					it.msg.Parent = sc.parent
				}
				out.items = append(out.items, it)
			}
		}
		first = false
		if maxVisible > 0 && visible >= maxVisible {
			return out, nil
		}
	}
}

// tailScan reads the most recent part of one file: a window back from the end,
// widened until it holds enough or reaches the start or the ceiling.
func tailScan(f *os.File, size int64, sc recScope, limit int) (recScan, error) {
	window := turnsTailStart
	for {
		start := size - window
		if start < 0 {
			start = 0
		}
		start, err := alignToLine(f, start)
		if err != nil {
			return recScan{}, err
		}
		out, err := scanRecordSource(f, sc, start, 0, size, 0)
		if err != nil {
			return recScan{}, err
		}
		if out.visible() >= limit || start == 0 || window >= turnsTailMax {
			return out, nil
		}
		window *= 4
		if window > turnsTailMax {
			window = turnsTailMax
		}
	}
}

// resolveOrphans finds the call of each orphan result in the file before it and
// completes the pair. A result is attached only to a call with exactly its id,
// found in this file, recorded before it, with no other result for that id
// between them. An orphan for which that does not hold is left unresolved and the
// page omits it: a result is never attached to a call it was not for.
func resolveOrphans(f *os.File, sc recScope, orphans []*recItem) error {
	if len(orphans) == 0 {
		return nil
	}
	want := map[string][]*recItem{}
	minStart, maxStart := orphans[0].lineStart, orphans[0].lineStart
	ids := make([][]byte, 0, len(orphans))
	for _, o := range orphans {
		if _, seen := want[o.res.id]; !seen {
			ids = append(ids, []byte(o.res.id))
		}
		want[o.res.id] = append(want[o.res.id], o)
		if o.lineStart < minStart {
			minStart = o.lineStart
		}
		if o.lineStart > maxStart {
			maxStart = o.lineStart
		}
	}

	type foundCall struct {
		msg fleet.RecordMessage
		pos recPos
	}
	var (
		calls   map[string]foundCall
		results map[string][]recPos
	)
	window := turnsTailStart
	for {
		ws := minStart - window
		if ws < 0 {
			ws = 0
		}
		ws, err := alignToLine(f, ws)
		if err != nil {
			return err
		}
		calls, results = map[string]foundCall{}, map[string][]recPos{}
		if maxStart > ws {
			r := bufio.NewReaderSize(io.NewSectionReader(f, ws, maxStart-ws), 64<<10)
			pos := ws
			for {
				line, n, complete, rerr := readBoundedLine(r, recordEntryLineLimit)
				if !complete {
					if rerr != nil && !errors.Is(rerr, io.EOF) {
						return rerr
					}
					break
				}
				lineStart := pos
				pos += int64(n)
				if line == nil || !containsAnyID(line, ids) {
					continue
				}
				entries, _ := classifyRecordLine(line, recScope{conv: sc.conv, agent: sc.agent})
				for i, ent := range entries {
					at := recPos{off: lineStart, k: i}
					switch ent.kind {
					case recCall:
						if _, wanted := want[ent.msg.Tool.ID]; wanted {
							if _, have := calls[ent.msg.Tool.ID]; !have {
								m := ent.msg
								m.Parent = sc.parent
								calls[ent.msg.Tool.ID] = foundCall{msg: m, pos: at}
							}
						}
					case recResult:
						if c, have := calls[ent.res.id]; have && c.pos.less(at) {
							results[ent.res.id] = append(results[ent.res.id], at)
						}
					}
				}
			}
		}
		if len(calls) >= len(want) || ws == 0 || window >= turnsTailMax {
			break
		}
		window *= 4
		if window > turnsTailMax {
			window = turnsTailMax
		}
	}

	for id, group := range want {
		c, ok := calls[id]
		if !ok {
			continue
		}
		for _, o := range group {
			here := recPos{off: o.lineStart, k: o.idx}
			if !c.pos.less(here) {
				continue
			}
			dup := false
			for _, rp := range results[id] {
				if rp.less(here) {
					dup = true
				}
			}
			if dup {
				continue
			}
			o.msg = completeCall(c.msg, o.res)
			o.resolved = true
		}
	}
	return nil
}

func containsAnyID(line []byte, ids [][]byte) bool {
	for _, id := range ids {
		if bytes.Contains(line, id) {
			return true
		}
	}
	return false
}

// completeCall folds a result into a call's message. The message keeps the call's
// own time: an update is the same entry, finished, and sits where the call sat.
func completeCall(call fleet.RecordMessage, res recResultData) fleet.RecordMessage {
	t := *call.Tool
	t.Result = res.text
	t.Status = fleet.RecordToolCompleted
	if res.isError {
		t.Status = fleet.RecordToolError
	}
	call.Tool = &t
	call.Truncated = call.Truncated || res.truncated
	return call
}

// ---------------------------------------------------------------------------
// Reading a page

// recRead is one source in one read.
type recRead struct {
	src  recSource
	f    *os.File
	size int64
	sc   recScope
	scan recScan
}

// itemRef names one item of one source's scan.
type itemRef struct{ s, i int }

// readRecord answers one query against one conversation file. Split from Record
// so the reading — the part that holds the boundary — is testable without a
// multiplexer.
func readRecord(path, since string, limit int) (fleet.RecordPage, error) {
	return readRecordAt(path, since, limit, time.Now())
}

func readRecordAt(path, since string, limit int, now time.Time) (fleet.RecordPage, error) {
	conv := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	f, err := os.Open(path)
	if err != nil {
		return fleet.RecordPage{}, fmt.Errorf("%w: %v", fleet.ErrNoTurnRecord, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fleet.RecordPage{}, fmt.Errorf("%w: %v", fleet.ErrNoTurnRecord, err)
	}

	var cur recCursor
	if since != "" {
		if cur, err = parseRecordCursor(since, conv); err != nil {
			return fleet.RecordPage{}, err
		}
		if cur.main.off > info.Size() {
			return fleet.RecordPage{}, fmt.Errorf(
				"%w: the cursor points past the end of this session's record (it was truncated or replaced since); read without `since` to start again",
				fleet.ErrAmbiguousTarget)
		}
	}

	reads := []*recRead{{src: recSource{path: path}, f: f, size: info.Size()}}
	reads[0].sc = reads[0].src.scope(conv)

	// Which sub-task files this read follows. Their sidecars are read only for the
	// ones that survive this choice.
	cands := listSubTaskFiles(path)
	if since == "" {
		// The most recent sub-tasks only: there is no backward paging, so what
		// matters is the tail and what follows it.
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].mtime.After(cands[j].mtime) })
		if len(cands) > recordSourcesMax {
			cands = cands[:recordSourcesMax]
		}
	}
	var subs []recSource
	for _, c := range cands {
		if since != "" && !cur.legacy {
			if _, named := cur.subs[c.id]; !named && c.mtime.Before(cur.water) {
				continue // finished before the cursor was issued, and consumed
			}
		}
		if s, ok := loadSubTask(subTaskDir(path), c.id); ok {
			subs = append(subs, s)
		}
	}
	startPos := map[string]recPos{}
	water := cur.water
	for _, s := range subs {
		pos, read := subTaskStart(s, cur, since != "", water)
		if !read {
			continue
		}
		sf, err := os.Open(s.path)
		if err != nil {
			continue
		}
		defer sf.Close()
		sfi, err := sf.Stat()
		if err != nil || !sfi.Mode().IsRegular() {
			continue
		}
		if pos.off == -1 { // follow from the end
			pos = recPos{off: sfi.Size()}
		}
		if pos.off > sfi.Size() { // replaced since: its new content is all new
			pos = recPos{}
		}
		startPos[s.id] = pos
		rd := &recRead{src: s, f: sf, size: sfi.Size()}
		rd.sc = s.scope(conv)
		reads = append(reads, rd)
	}

	// Scan.
	for ri, rd := range reads {
		var scan recScan
		var err error
		switch {
		case since == "":
			scan, err = tailScan(rd.f, rd.size, rd.sc, limit)
		case ri == 0:
			scan, err = forwardScan(rd, cur.main, limit)
		default:
			scan, err = forwardScan(rd, startPos[rd.src.id], limit)
		}
		if err != nil {
			return fleet.RecordPage{}, err
		}
		rd.scan = scan
	}

	// Select what the page holds.
	scans := make([]recScan, len(reads))
	for i, rd := range reads {
		scans[i] = rd.scan
	}
	var order []itemRef
	if since == "" {
		order = selectTail(scans, limit)
	} else {
		order = selectForward(scans, limit)
	}
	taken := make([]map[int]bool, len(scans))
	for i := range taken {
		taken[i] = map[int]bool{}
	}
	for _, ref := range order {
		taken[ref.s][ref.i] = true
	}

	// Fold each taken result into its taken call, and resolve the results whose
	// call is not in the scan.
	orphans := make([][]*recItem, len(scans))
	for _, ref := range order {
		it := &scans[ref.s].items[ref.i]
		switch {
		case it.hidden && taken[ref.s][it.into]:
			c := &scans[ref.s].items[it.into]
			c.msg = completeCall(c.msg, it.res)
		case it.orphan:
			orphans[ref.s] = append(orphans[ref.s], it)
		}
	}
	for si, group := range orphans {
		if err := resolveOrphans(reads[si].f, reads[si].sc, group); err != nil {
			return fleet.RecordPage{}, err
		}
	}

	msgs := make([]fleet.RecordMessage, 0, len(order))
	for _, ref := range order {
		it := scans[ref.s].items[ref.i]
		if it.hidden || (it.orphan && !it.resolved) {
			continue
		}
		msgs = append(msgs, it.msg)
	}

	// The cursor: where each source is now.
	var main recPos
	var listed []recListed
	for si, rd := range reads {
		var pos recPos
		if since == "" || len(scans[si].items) == 0 || nextUntaken(scans[si], taken[si]) < 0 {
			pos = recPos{off: scans[si].consumed}
		} else {
			it := scans[si].items[nextUntaken(scans[si], taken[si])]
			pos = recPos{off: it.lineStart, k: it.idx}
		}
		if si == 0 {
			main = pos
			continue
		}
		if rd.src.mtime.After(now.Add(-recordWatermarkSlack)) || !rd.src.finished || pos.off < rd.size || pos.k > 0 {
			listed = append(listed, recListed{id: rd.src.id, pos: pos, mtime: rd.src.mtime})
		}
	}
	return fleet.RecordPage{Messages: msgs, Next: recordCursor(conv, main, now, listed), Format: fleet.RecordFormat}, nil
}

// forwardScan reads one source from a cursor position: up to scanMax bytes, and
// until it holds a page's worth.
func forwardScan(rd *recRead, pos recPos, limit int) (recScan, error) {
	to := rd.size
	if rd.size-pos.off > turnsScanMax {
		to = pos.off + turnsScanMax
	}
	return scanRecordSource(rd.f, rd.sc, pos.off, pos.k, to, limit)
}

// subTaskStart says whether a sub-task file is read in this read, and from where.
// read false means it is not read and not carried. A position with off -1 means
// "from the end of the file as it is now".
//
// A cursor names the sub-tasks it needs to: those still being written, those
// unread, and those written within its watermark. A file it does not name is
// either one that finished before the watermark (done) or one the cursor has
// never seen: its first line, which never changes, says which.
func subTaskStart(s recSource, cur recCursor, forward bool, water time.Time) (recPos, bool) {
	if !forward {
		return recPos{}, true // a tail read reads the tail of each; the scan places it
	}
	if p, ok := cur.subs[s.id]; ok {
		return p, true
	}
	if cur.legacy {
		return recPos{off: -1}, true // an older cursor says nothing of sub-tasks: follow from now
	}
	if s.mtime.Before(water) {
		return recPos{}, false // finished before the cursor was issued, and consumed
	}
	if first, ok := firstTimestamp(s.path); ok && first.After(water) {
		return recPos{}, true // began after the cursor was issued: all of it is new
	}
	return recPos{off: -1}, true // older than the cursor and not named by it: follow from now
}

// selectForward takes the next `limit` visible items across the scans in the
// order of their times (ties: the conversation file first, then by source). A
// result that completes a call already taken is free: it takes no room on the
// page.
func selectForward(scans []recScan, limit int) []itemRef {
	next := make([]int, len(scans))
	var order []itemRef
	visible := 0
	for {
		for s := range scans {
			for next[s] < len(scans[s].items) && scans[s].items[next[s]].hidden {
				order = append(order, itemRef{s, next[s]})
				next[s]++
			}
		}
		if visible >= limit {
			break
		}
		best := -1
		for s := range scans {
			if next[s] >= len(scans[s].items) {
				continue
			}
			if best < 0 || scans[s].items[next[s]].sortAt.Before(scans[best].items[next[best]].sortAt) {
				best = s
			}
		}
		if best < 0 {
			break
		}
		order = append(order, itemRef{best, next[best]})
		next[best]++
		visible++
	}
	return order
}

// selectTail merges every scan in time order and takes the last `limit` visible
// items with whatever hidden results follow them.
func selectTail(scans []recScan, limit int) []itemRef {
	next := make([]int, len(scans))
	var all []itemRef
	for {
		best := -1
		for s := range scans {
			if next[s] >= len(scans[s].items) {
				continue
			}
			if best < 0 || scans[s].items[next[s]].sortAt.Before(scans[best].items[next[best]].sortAt) {
				best = s
			}
		}
		if best < 0 {
			break
		}
		all = append(all, itemRef{best, next[best]})
		next[best]++
	}
	cut, v := len(all), 0
	for cut > 0 && v < limit {
		cut--
		ref := all[cut]
		if !scans[ref.s].items[ref.i].hidden {
			v++
		}
	}
	return all[cut:]
}

// nextUntaken is the index of the first item of a scan the page did not take, or
// -1 when it took them all.
func nextUntaken(s recScan, taken map[int]bool) int {
	for i := range s.items {
		if !taken[i] {
			return i
		}
	}
	return -1
}
