package service

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
	"github.com/futurelastic/muster/internal/state"
)

// The input audit — muster #330. See docs/adr/330-the-input-audit.md for the
// decisions; this file is the store and the read.
//
// # What it answers
//
// A client wants to prove later that a prompt in a session was typed by a
// person, and not sent by another agent through POST …/input. Nothing could
// answer that: the engine's transcript marks every user turn the same way, and
// the label convention in the text is controlled by the sender. The service,
// though, knows who authenticated and whether it treated the call as a person's
// own message (humanRelay — a grant, never a header). It writes that down, with
// a hash of the text it was handed, and lets a reader ask.
//
// # Why a log and not history.go's document
//
// history.go rewrites one JSON document per change. /input is the hot path and
// each record is immutable once final, so this store appends one JSON line per
// record to input-audit.jsonl and keeps an index in memory. Retention prunes
// by rewriting the file atomically.
//
// # Two phases, because a proof of absence must have no hole at the delivery
//
// "No agent sent this text at that time" is only as good as the record of every
// delivery. A record written after the driver returns is lost if the process
// dies between the delivery and the write — exactly the delivery a verifier
// most needs to see. So a `pending` line is appended and synced BEFORE the
// driver is called, and a final line with the same id after. A pending line
// with no final one (a crash) is read back as outcome "unknown".
//
// A write that fails never fails the input: the failure is logged and the
// store's coveredFrom moves to the failure, so a reader sees a gap instead of a
// silent claim of completeness.

// auditLogFile is the open log. An interface so a test can make a write end
// short; in service it is always an *os.File.
type auditLogFile interface {
	io.Writer
	Sync() error
	Truncate(size int64) error
	Stat() (os.FileInfo, error)
	Close() error
}

// inputAuditFile is this store's log in the state directory.
const inputAuditFile = "input-audit.jsonl"

// DefaultInputAuditRetention is how long an input record is kept when the
// operator configures nothing.
const DefaultInputAuditRetention = 14 * 24 * time.Hour

// maxInputAuditRecords bounds memory and file size whatever the rate: past it
// the oldest record is dropped and coveredFrom moves up to it, so a reader is
// told a window was lost rather than shown a short list as a complete one.
const maxInputAuditRecords = 100000

// inputAuditCompactEvery bounds how often retention rewrites the file.
const inputAuditCompactEvery = time.Hour

// Outcomes beyond a driver receipt's own.
const (
	// inputOutcomePending is a delivery in flight (or, after a restart, one the
	// process died in the middle of — reported as inputOutcomeUnknown then).
	inputOutcomePending = "pending"
	// inputOutcomeUnknown is the receipt outcome "unknown", and also what a
	// pending record that never got its final line reads back as.
	inputOutcomeUnknown = "unknown"
	// inputOutcomeError is a driver-level failure after the call was attempted:
	// the text may or may not have reached the session, so it is recorded.
	inputOutcomeError = "error"
)

// InputRecord is one audited /input call.
//
// Nothing here is taken from the caller's say-so except the text hash and the
// session id the caller addressed. Principal, HumanRelay and Direction are what
// this service determined.
type InputRecord struct {
	// ID is unique within this machine's log; the two phases of one call share it.
	ID string `json:"id"`
	// At is when the service accepted the call.
	At time.Time `json:"at"`
	// Principal is the authenticated caller, composed as the mutation audit line
	// composes it: a relayed-in call reads "<asserted principal> via <peer>".
	// Empty only on a machine with no principal table, where the shared token
	// names nobody.
	Principal string `json:"principal,omitempty"`
	// Machine is the machine the call addressed; Session the id it used.
	Machine fleet.MachineId `json:"machine"`
	Session string          `json:"session"`
	// SHA256 is the lowercase hex sha256 of the message text exactly as it was
	// sent (before any sender label is put in front of it).
	SHA256 string `json:"sha256"`
	// Bytes is the length of that text.
	Bytes int `json:"bytes"`
	// HumanRelay is the fact the service computed from grants (GrantHumanRelay
	// here, or a trusted peer's assertion of it): true means the call was treated
	// as a person's own message and delivered unlabelled; false means it was
	// labelled as an agent's. It is the field that tells typed from relayed.
	HumanRelay bool `json:"humanRelay"`
	// Route is the delivery route the caller asked for ("" when none).
	Route string `json:"route,omitempty"`
	// Direction is "local" (this machine owns the session), "relayed-out" (this
	// machine forwarded the call to a peer) or "relayed-in" (a trusted peer
	// forwarded it here).
	Direction string `json:"direction"`
	// Outcome is the receipt's outcome (submitted, queued, refused, unknown, …),
	// "error" when the driver failed, or "pending".
	Outcome string `json:"outcome"`
	// Person is the human the caller said it acted for, "<id>@<issuer>", set
	// only when the claim was honoured (muster #349): the principal held the
	// acting-person grant here, or a configured peer holding it relayed the
	// claim. Attribution only. Read it with Principal: a person is as
	// trustworthy as the principal that vouched for them, and the pair reads
	// "<person> · <principal> via <peer>".
	Person string `json:"person,omitempty"`
	// PersonDropped is set when a claim was made and NOT honoured: why, and the
	// claim as made. Person is then empty. A dropped claim is recorded rather
	// than ignored so a client that thinks it attributed a call can find out it
	// did not.
	PersonDropped string `json:"personDropped,omitempty"`
}

// withActing copies a request's acting-person decision into the record.
func (rec InputRecord) withActing(a actingResult) InputRecord {
	if a.Person != nil {
		rec.Person = a.Person.String()
	}
	if a.Dropped != "" {
		rec.PersonDropped = a.Dropped
		if a.Claimed != "" {
			rec.PersonDropped = a.Claimed + ": " + a.Dropped
		}
	}
	return rec
}

// inputAuditLine is what the log holds: a record, or the file's begin marker.
type inputAuditLine struct {
	Begin *time.Time   `json:"begin,omitempty"`
	Rec   *InputRecord `json:"rec,omitempty"`
}

// InputAuditPage is the read's answer.
type InputAuditPage struct {
	Records []InputRecord `json:"records"`
	// Truncated is true when more records matched than Limit allowed; the newest
	// are returned.
	Truncated bool `json:"truncated,omitempty"`
	// CoveredFrom is the earliest instant for which this machine's record is
	// complete. A query window that starts before it cannot prove absence: no
	// match there means "not recorded", not "did not happen". It is the latest
	// of when recording began, the retention cut, and any lost write or cap.
	CoveredFrom time.Time `json:"coveredFrom"`
	// Durable is false when there is no state directory: records are held in
	// memory and are gone with the process.
	Durable bool `json:"durable"`
}

type inputAuditStore struct {
	mu          sync.Mutex
	dir         string // empty: in memory only
	f           auditLogFile
	recs        []*InputRecord // oldest first
	byID        map[string]*InputRecord
	retention   time.Duration
	coveredFrom time.Time
	lastCompact time.Time
	seq         int64
	epoch       string
	now         func() time.Time
}

func newInputAuditStore() *inputAuditStore {
	now := time.Now()
	return &inputAuditStore{
		byID:        map[string]*InputRecord{},
		retention:   DefaultInputAuditRetention,
		coveredFrom: now,
		lastCompact: now,
		epoch:       strconv.FormatInt(now.UnixNano(), 36),
		now:         time.Now,
	}
}

// load adopts the log under a state store and opens it for appending. An
// unparseable line is an error the operator sees, state.Store's own rule: the
// tempting fallback is to start empty, which would silently drop the evidence.
func (a *inputAuditStore) load(st *state.Store) error {
	if st == nil || st.Dir() == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dir = st.Dir()
	path := filepath.Join(a.dir, inputAuditFile)

	torn, unterminated, begin, found, err := a.readLogLocked(path)
	if err != nil {
		return err
	}
	if found {
		if !begin.IsZero() {
			a.coveredFrom = begin
		}
		if torn >= 0 {
			// The process died (or the disk filled) in the middle of a write. The
			// partial line is not evidence of anything — it was never a whole
			// record — but what it was the start of is lost, so the log cannot
			// vouch for the time from here. The moment this machine noticed is
			// the honest bound: the lost record's own time is unreadable, and
			// nothing has been recorded since the service went down.
			if err := os.Truncate(path, torn); err != nil {
				return fmt.Errorf("input audit: %s ends in a partial line and could not be trimmed: %w", inputAuditFile, err)
			}
			if now := a.now(); now.After(a.coveredFrom) {
				a.coveredFrom = now
			}
			log.Printf("input audit: %s ended in a partial line (a write that did not finish); trimmed it, reads now report coveredFrom %s", inputAuditFile, a.coveredFrom.UTC().Format(time.RFC3339))
		}
	} else {
		// A first run: recording begins now, and the file says so.
		if err := a.rewriteLocked(); err != nil {
			return err
		}
	}
	// A pending record that survived a restart is a delivery the process died
	// in the middle of.
	for _, r := range a.recs {
		if r.Outcome == inputOutcomePending {
			r.Outcome = inputOutcomeUnknown
		}
	}
	// Not pruned here: retention is applied by cmd/muster after load, and a
	// prune with the default in force would discard what a longer one keeps.
	// The first begin or query prunes with the configured value.
	if a.f != nil {
		return nil // the first-run rewrite already opened it
	}
	if err := a.openLocked(); err != nil {
		return err
	}
	if unterminated {
		// A whole record that lost only its newline: end the line, or the next
		// append would run into it and make the two unreadable.
		if _, err := a.f.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("input audit: %w", err)
		}
	}
	return nil
}

// readLogLocked adopts every record in the log at path. found is false when
// there is no log. torn is the byte offset where a partial final line begins
// (-1 when the log ends cleanly); unterminated is true when the final line is a
// whole record with no newline after it.
//
// Only the final line can be torn: a write appends one newline-terminated line,
// so a crash or a short write leaves a tail with no newline. Any other line
// that does not parse — in particular a newline-terminated one — is damage this
// code did not cause, and stays an error the operator sees.
func (a *inputAuditStore) readLogLocked(path string) (torn int64, unterminated bool, begin time.Time, found bool, err error) {
	torn = -1
	raw, err := os.Open(path)
	if os.IsNotExist(err) {
		return torn, false, begin, false, nil
	}
	if err != nil {
		return torn, false, begin, false, fmt.Errorf("input audit: %w", err)
	}
	defer raw.Close()
	rd := bufio.NewReaderSize(raw, 64*1024)
	var off int64
	for n := 1; ; n++ {
		line, rerr := rd.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			return torn, false, begin, true, fmt.Errorf("input audit: reading %s: %w", inputAuditFile, rerr)
		}
		if len(line) == 0 {
			break
		}
		start := off
		off += int64(len(line))
		terminated := line[len(line)-1] == '\n'
		if len(bytes.TrimSpace(line)) > 0 {
			var ln inputAuditLine
			if jerr := json.Unmarshal(line, &ln); jerr != nil {
				if terminated {
					return torn, false, begin, true, fmt.Errorf("input audit: %s line %d is corrupt (not overwriting): %w", inputAuditFile, n, jerr)
				}
				return start, false, begin, true, nil // the torn tail; nothing follows it
			}
			if ln.Begin != nil {
				begin = *ln.Begin
			}
			if ln.Rec != nil {
				a.adoptLocked(*ln.Rec)
			}
			unterminated = !terminated
		} else if !terminated {
			return start, false, begin, true, nil // trailing blanks with no newline: a torn tail too
		}
		if rerr == io.EOF {
			break
		}
	}
	return torn, unterminated, begin, true, nil
}

// adoptLocked folds one log line in: a later line with the same id replaces the
// earlier one (the final phase over the pending).
func (a *inputAuditStore) adoptLocked(r InputRecord) {
	if prev, ok := a.byID[r.ID]; ok {
		*prev = r
		return
	}
	rec := r
	a.recs = append(a.recs, &rec)
	a.byID[rec.ID] = &rec
}

func (a *inputAuditStore) openLocked() error {
	f, err := os.OpenFile(filepath.Join(a.dir, inputAuditFile), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("input audit: %w", err)
	}
	a.f = f
	return nil
}

func (a *inputAuditStore) setRetention(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d <= 0 {
		d = DefaultInputAuditRetention
	}
	a.retention = d
}

// begin records an accepted call before it is delivered and returns its id.
// The write is synced: a record that exists only in the page cache is not a
// record of a delivery that has already happened.
func (a *inputAuditStore) begin(rec InputRecord) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.seq++
	rec.ID = a.epoch + "-" + strconv.FormatInt(a.seq, 10)
	rec.At = now.UTC()
	rec.Outcome = inputOutcomePending
	a.adoptLocked(rec)
	a.appendLocked(rec, true, now)
	a.pruneLocked(now)
	return rec.ID
}

// finish gives a call its outcome.
func (a *inputAuditStore) finish(id, outcome string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.byID[id]
	if !ok {
		return // pruned or capped while in flight; nothing to complete
	}
	r.Outcome = outcome
	a.appendLocked(*r, false, a.now())
}

// appendLocked writes one record line. A failure is logged and moves
// coveredFrom past this instant: the log can no longer vouch for it.
func (a *inputAuditStore) appendLocked(r InputRecord, sync bool, now time.Time) {
	if a.dir == "" {
		return
	}
	raw, err := json.Marshal(inputAuditLine{Rec: &r})
	if err == nil {
		raw = append(raw, '\n')
		if a.f == nil {
			err = fmt.Errorf("log is not open")
		} else {
			err = a.writeLineLocked(raw, sync)
		}
	}
	if err != nil {
		log.Printf("input audit: could not record %s: %v (reads now report coveredFrom after this call)", r.ID, err)
		if now.After(a.coveredFrom) {
			a.coveredFrom = now
		}
	}
}

// writeLineLocked appends one whole line, and leaves no part of it behind when
// that fails. A short write (a full disk) that stayed in the file would sit
// mid-file once a later append succeeded, and load refuses a corrupt line that
// is not the last one — the service would not start. So the file is put back
// to the length it had; if even that cannot be done the log is closed, so no
// later append can bury the fragment, and the next start trims it as a torn
// tail.
func (a *inputAuditStore) writeLineLocked(raw []byte, sync bool) error {
	fi, serr := a.f.Stat()
	_, err := a.f.Write(raw)
	if err != nil {
		if serr == nil {
			serr = a.f.Truncate(fi.Size())
		}
		if serr != nil {
			a.f.Close()
			a.f = nil
		}
		return err
	}
	if sync {
		return a.f.Sync()
	}
	return nil
}

// pruneLocked drops records past retention or past the cap and, at most once an
// interval, rewrites the file without them. In-memory records are dropped at
// once, so a read never returns what retention has expired.
func (a *inputAuditStore) pruneLocked(now time.Time) {
	cut := now.Add(-a.retention)
	drop := 0
	for drop < len(a.recs) && (a.recs[drop].At.Before(cut) || len(a.recs)-drop > maxInputAuditRecords) {
		r := a.recs[drop]
		delete(a.byID, r.ID)
		if r.At.After(a.coveredFrom) {
			a.coveredFrom = r.At
		}
		drop++
	}
	if cut.After(a.coveredFrom) {
		a.coveredFrom = cut
	}
	if drop > 0 {
		a.recs = append([]*InputRecord(nil), a.recs[drop:]...)
	}
	if a.dir == "" || (drop == 0 && now.Sub(a.lastCompact) < inputAuditCompactEvery) {
		return
	}
	if err := a.rewriteLocked(); err != nil {
		log.Printf("input audit: compaction failed: %v", err)
	} else {
		a.lastCompact = now
	}
}

// rewriteLocked replaces the log with a begin marker and the live records,
// atomically, then reopens it for appending.
func (a *inputAuditStore) rewriteLocked() error {
	if a.dir == "" {
		return nil
	}
	tmp, err := privfs.CreateTemp(a.dir, "."+inputAuditFile+".tmp*")
	if err != nil {
		return fmt.Errorf("input audit: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)
	begin := a.coveredFrom.UTC()
	_ = enc.Encode(inputAuditLine{Begin: &begin})
	for _, r := range a.recs {
		_ = enc.Encode(inputAuditLine{Rec: r})
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("input audit: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("input audit: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("input audit: %w", err)
	}
	if a.f != nil {
		a.f.Close()
		a.f = nil
	}
	if err := os.Rename(tmpName, filepath.Join(a.dir, inputAuditFile)); err != nil {
		_ = a.openLocked()
		return fmt.Errorf("input audit: %w", err)
	}
	return a.openLocked()
}

// inputAuditQuery selects records. Zero fields match everything.
type inputAuditQuery struct {
	Session   string
	SHA256    string
	Principal string
	Since     time.Time
	Until     time.Time
	Limit     int
}

const (
	defaultInputAuditLimit = 200
	maxInputAuditLimit     = 1000
)

func (a *inputAuditStore) query(q inputAuditQuery) InputAuditPage {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(a.now())
	limit := q.Limit
	if limit <= 0 {
		limit = defaultInputAuditLimit
	}
	if limit > maxInputAuditLimit {
		limit = maxInputAuditLimit
	}
	page := InputAuditPage{Records: []InputRecord{}, CoveredFrom: a.coveredFrom.UTC(), Durable: a.dir != ""}
	for i := len(a.recs) - 1; i >= 0; i-- { // newest first
		r := a.recs[i]
		if q.Session != "" && r.Session != q.Session {
			continue
		}
		if q.SHA256 != "" && r.SHA256 != q.SHA256 {
			continue
		}
		if q.Principal != "" && r.Principal != q.Principal {
			continue
		}
		if !q.Since.IsZero() && r.At.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && !r.At.Before(q.Until) {
			continue
		}
		if len(page.Records) == limit {
			page.Truncated = true
			break
		}
		page.Records = append(page.Records, *r)
	}
	// Oldest first reads as a timeline; the cut above kept the newest.
	for i, j := 0, len(page.Records)-1; i < j; i, j = i+1, j-1 {
		page.Records[i], page.Records[j] = page.Records[j], page.Records[i]
	}
	return page
}

// SetInputAuditRetention changes how long input records are kept. Non-positive
// restores the default.
func (s *Service) SetInputAuditRetention(d time.Duration) { s.inputAudit.setRetention(d) }

// textSHA256 is the hash recorded and queried: lowercase hex sha256.
func textSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// auditPrincipal is who authenticated, for the record. Unlike the mutation
// audit line it does not repeat an on-behalf-of header from a caller this
// service does not trust to relay (relayTrusted): the header is then ignored by
// every other decision, so the record names the credential that actually
// authenticated rather than a claim made against it.
func (svc *Service) auditPrincipal(r *http.Request) string {
	p := principalOrZero(r)
	if r.Header.Get(onBehalfOfHeader) != "" && !svc.relayTrusted(r) {
		return p.Name
	}
	// Without the person: that has its own field (InputRecord.Person), so a
	// reader filtering on principal still matches a call that named one.
	return callerFor(p, r).Principal
}

// directionOf names where a call entered relative to the machine that owns the
// session.
func (svc *Service) directionOf(r *http.Request, target fleet.MachineId) string {
	switch {
	case target != "" && target != svc.Self():
		return "relayed-out"
	case r.Header.Get(onBehalfOfHeader) != "" && svc.relayTrusted(r):
		return "relayed-in"
	}
	return "local"
}

// handleInputAudit answers GET /v1/machines/{machine}/input-audit (muster #330).
//
// Machine-local on purpose: the records on a machine are what that machine
// accepted. A call relayed through this machine is recorded here (relayed-out,
// with the principal this machine authenticated) and again on the owner
// (relayed-in); a verifier asks the machine the call entered.
func handleInputAudit(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		if machine != svc.Self() {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
				Message: "the input audit is machine-local: ask " + string(svc.Self()) +
					" for what it accepted, and the machine a call entered for a relayed call's origin"})
			return
		}
		q := r.URL.Query()
		query := inputAuditQuery{Session: q.Get("session"), Principal: q.Get("principal")}
		if raw := q.Get("sha256"); raw != "" {
			raw = strings.ToLower(raw)
			if len(raw) != 64 || strings.Trim(raw, "0123456789abcdef") != "" {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
					Message: "sha256 must be 64 lowercase hex characters: the sha256 of the message text"})
				return
			}
			query.SHA256 = raw
		}
		for _, f := range []struct {
			name string
			dst  *time.Time
		}{{"since", &query.Since}, {"until", &query.Until}} {
			if raw := q.Get(f.name); raw != "" {
				t, err := time.Parse(time.RFC3339Nano, raw)
				if err != nil {
					writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
						Message: f.name + " must be an RFC 3339 timestamp"})
					return
				}
				*f.dst = t
			}
		}
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
					Message: "limit must be a positive integer (at most " + strconv.Itoa(maxInputAuditLimit) + ")"})
				return
			}
			query.Limit = n
		}
		if query.Session == "" && query.SHA256 == "" && query.Since.IsZero() {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
				Message: "name what to look for: session, sha256 or since (a window)"})
			return
		}
		writeJSON(w, http.StatusOK, svc.inputAudit.query(query))
	}
}

// principalOrZero is the resolved principal, or the zero Principal on a machine
// with no principal table (where the shared token names nobody).
func principalOrZero(r *http.Request) Principal {
	p, _ := principalOf(r)
	return p
}
