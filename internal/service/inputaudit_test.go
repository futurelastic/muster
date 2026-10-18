package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/state"
)

// muster #330. The store, then the HTTP surface.

func newAuditSvc(t *testing.T, st *state.Store) *Service {
	t.Helper()
	var svc *Service
	if st != nil {
		var err error
		svc, err = NewWithState("owner", st)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		svc = New("owner")
	}
	return svc
}

func getAudit(t *testing.T, srv *httptest.Server, machine, token, query string) (*http.Response, InputAuditPage) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/"+machine+"/input-audit?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page InputAuditPage
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
	}
	return resp, page
}

// failingDriver fails every send at the driver level.
type failingDriver struct{ stub.Driver }

func (failingDriver) Send(context.Context, fleet.Request, fleet.SessionRef, string, driver.SendOptions) (fleet.DeliveryReceipt, error) {
	return fleet.DeliveryReceipt{}, errors.New("driver exploded")
}

func auditServer(t *testing.T, svc *Service, d driver.Driver, principals []Principal) *httptest.Server {
	t.Helper()
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("entrybox", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: principals}))
	t.Cleanup(srv.Close)
	return srv
}

var auditPrincipals = []Principal{
	{Name: "entrybox", Token: "peer-token", Grants: []Grant{GrantSend}},
	{Name: "agent", Token: "agent-token", Grants: []Grant{GrantSend}},
	{Name: "person", Token: "person-token", Grants: []Grant{GrantSend, GrantHumanRelay}},
	{Name: "auditor", Token: "auditor-token", Grants: []Grant{GrantRead}},
	{Name: "nobody", Token: "nobody-token"},
}

// The issue's use: a person's message and an agent's message are told apart,
// by the grant-derived fact, and each is findable by the hash of its text.
func TestInputAudit_TypedIsToldFromRelayed(t *testing.T) {
	svc := newAuditSvc(t, nil)
	srv := auditServer(t, svc, &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}, auditPrincipals)

	postInput(t, srv, "person-token", nil, map[string]any{"text": "approve 123"})
	postInput(t, srv, "agent-token", nil, map[string]any{"text": "approve 456", "from": map[string]any{"agent": "worker"}})

	hash := textSHA256("approve 123")
	if len(hash) != 64 {
		t.Fatalf("hash = %q", hash)
	}
	resp, page := getAudit(t, srv, "owner", "auditor-token", "sha256="+hash)
	if resp.StatusCode != http.StatusOK || len(page.Records) != 1 {
		t.Fatalf("status %d, records %+v", resp.StatusCode, page.Records)
	}
	got := page.Records[0]
	if got.Principal != "person" || !got.HumanRelay || got.Direction != "local" ||
		got.Session != "s1" || got.Machine != "owner" || got.Outcome != "queued" || got.Bytes != len("approve 123") {
		t.Errorf("record = %+v", got)
	}
	if page.Durable {
		t.Error("a service with no state directory must report durable=false")
	}

	_, page = getAudit(t, srv, "owner", "auditor-token", "sha256="+textSHA256("approve 456"))
	if len(page.Records) != 1 || page.Records[0].Principal != "agent" || page.Records[0].HumanRelay {
		t.Errorf("the agent's send must read as not human: %+v", page.Records)
	}
	// The text itself is never stored.
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "approve") {
		t.Errorf("the record carries the text: %s", raw)
	}
}

// A relayed-in call names the asserted principal and the relaying peer; the
// human-relay fact is the peer's assertion, honoured only from a configured peer.
func TestInputAudit_RelayedInNamesBothEnds(t *testing.T) {
	svc := newAuditSvc(t, nil)
	srv := auditServer(t, svc, &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}, auditPrincipals)

	postInput(t, srv, "peer-token", map[string]string{onBehalfOfHeader: "person", humanRelayHeader: "1"}, map[string]any{"text": "go"})
	postInput(t, srv, "agent-token", map[string]string{onBehalfOfHeader: "person", humanRelayHeader: "1"}, map[string]any{"text": "forged"})

	_, page := getAudit(t, srv, "owner", "auditor-token", "session=s1")
	if len(page.Records) != 2 {
		t.Fatalf("records = %+v", page.Records)
	}
	byHash := map[string]InputRecord{}
	for _, r := range page.Records {
		byHash[r.SHA256] = r
	}
	if r := byHash[textSHA256("go")]; r.Principal != "person via entrybox" || !r.HumanRelay || r.Direction != "relayed-in" {
		t.Errorf("trusted relay = %+v", r)
	}
	// A non-peer setting the headers is ignored: it is a direct call and its
	// own claim of human-ness is not believed.
	if r := byHash[textSHA256("forged")]; r.HumanRelay || r.Direction != "local" || r.Principal != "agent" {
		t.Errorf("a forged relay assertion was believed: %+v", r)
	}
}

// A call that addressed a peer is recorded on this machine too, as relayed-out.
func TestInputAudit_RelayedOutIsRecordedWhereItEntered(t *testing.T) {
	svc := newAuditSvc(t, nil)
	srv := auditServer(t, svc, &stub.Driver{DeadlineMs: 500}, []Principal{
		{Name: "agent", Token: "agent-token", Grants: []Grant{GrantSend, GrantRelay}},
		{Name: "auditor", Token: "auditor-token", Grants: []Grant{GrantRead}},
	})
	postInputTo(t, srv, "entrybox", "agent-token", nil, map[string]any{"text": "over there"})
	_, page := getAudit(t, srv, "owner", "auditor-token", "sha256="+textSHA256("over there"))
	if len(page.Records) != 1 || page.Records[0].Direction != "relayed-out" || page.Records[0].Machine != "entrybox" ||
		page.Records[0].Principal != "agent" {
		t.Errorf("records = %+v", page.Records)
	}
}

func TestInputAudit_DriverFailureIsRecordedAsError(t *testing.T) {
	svc := newAuditSvc(t, nil)
	srv := auditServer(t, svc, &failingDriver{Driver: stub.Driver{DeadlineMs: 500}}, auditPrincipals)
	postInput(t, srv, "agent-token", nil, map[string]any{"text": "maybe delivered"})
	_, page := getAudit(t, srv, "owner", "auditor-token", "session=s1")
	if len(page.Records) != 1 || page.Records[0].Outcome != inputOutcomeError {
		t.Errorf("a failed send must still leave a record: %+v", page.Records)
	}
}

// A request refused for its own shape never reached a driver: nothing delivered,
// nothing to prove.
func TestInputAudit_ShapeRefusalsLeaveNoRecord(t *testing.T) {
	svc := newAuditSvc(t, nil)
	srv := auditServer(t, svc, &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}, auditPrincipals)
	postInput(t, srv, "agent-token", nil, map[string]any{"text": ""})
	postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "route": "nonsense"})
	_, page := getAudit(t, srv, "owner", "auditor-token", "since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	if len(page.Records) != 0 {
		t.Errorf("records = %+v", page.Records)
	}
}

func TestInputAudit_ReadNeedsTheReadGrantAndAQuestion(t *testing.T) {
	svc := newAuditSvc(t, nil)
	srv := auditServer(t, svc, &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}, auditPrincipals)

	if resp, _ := getAudit(t, srv, "owner", "nobody-token", "session=s1"); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a principal with no read grant got %d", resp.StatusCode)
	}
	if resp, _ := getAudit(t, srv, "owner", "auditor-token", ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unbounded read got %d, want 400", resp.StatusCode)
	}
	if resp, _ := getAudit(t, srv, "owner", "auditor-token", "sha256=zz"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed hash got %d, want 400", resp.StatusCode)
	}
	if resp, _ := getAudit(t, srv, "entrybox", "auditor-token", "session=s1"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("another machine's audit got %d, want 400 (machine-local)", resp.StatusCode)
	}
}

func TestInputAudit_QueryWindowAndLimit(t *testing.T) {
	a := newInputAuditStore()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a.coveredFrom = base.Add(-time.Hour)
	for i := 0; i < 5; i++ {
		a.now = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		id := a.begin(InputRecord{Session: "s", SHA256: textSHA256("t"), Principal: "p"})
		a.finish(id, "submitted")
	}
	a.now = func() time.Time { return base.Add(10 * time.Minute) }
	page := a.query(inputAuditQuery{Session: "s", Since: base.Add(time.Minute), Until: base.Add(4 * time.Minute)})
	if len(page.Records) != 3 { // minutes 1,2,3 — until is exclusive
		t.Errorf("window gave %d records", len(page.Records))
	}
	page = a.query(inputAuditQuery{Session: "s", Limit: 2})
	if len(page.Records) != 2 || !page.Truncated || !page.Records[1].At.After(page.Records[0].At) {
		t.Errorf("limit: %+v truncated=%v (the newest two, oldest first)", page.Records, page.Truncated)
	}
	if !page.Records[1].At.Equal(base.Add(4 * time.Minute)) {
		t.Errorf("a cut must keep the newest, got %v", page.Records[1].At)
	}
}

func TestInputAudit_DurableAcrossARestartAndCrashReadsUnknown(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	done := a.begin(InputRecord{Session: "s", SHA256: textSHA256("one"), Principal: "p", Machine: "owner", Direction: "local"})
	a.finish(done, "submitted")
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("two"), Principal: "p", Machine: "owner", Direction: "local"}) // crash: never finished
	a.f.Close()

	b := newInputAuditStore()
	if err := b.load(st); err != nil {
		t.Fatal(err)
	}
	page := b.query(inputAuditQuery{Session: "s"})
	if !page.Durable || len(page.Records) != 2 {
		t.Fatalf("after a restart: %+v", page)
	}
	if page.Records[0].Outcome != "submitted" || page.Records[1].Outcome != inputOutcomeUnknown {
		t.Errorf("outcomes = %q, %q — an unfinished call must read unknown, never a delivery verdict",
			page.Records[0].Outcome, page.Records[1].Outcome)
	}
	if page.Records[0].ID == "" || page.Records[0].ID == page.Records[1].ID {
		t.Errorf("ids = %q, %q", page.Records[0].ID, page.Records[1].ID)
	}
	if fi, err := os.Stat(filepath.Join(dir, inputAuditFile)); err != nil || fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("the log must be owner-only: %v %v", fi, err)
	}
}

func TestInputAudit_CorruptLogIsRefusedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, inputAuditFile), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Open(dir)
	if err := newInputAuditStore().load(st); err == nil {
		t.Fatal("a corrupt log was accepted")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, inputAuditFile))
	if string(raw) != "{not json\n" {
		t.Errorf("the corrupt log was overwritten: %q", raw)
	}
}

// Retention drops expired records at once and moves coveredFrom up to the cut,
// so a reader is told the window it can no longer vouch for.
func TestInputAudit_RetentionMovesCoveredFrom(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	a.setRetention(time.Hour)
	t0 := time.Now()
	a.now = func() time.Time { return t0 }
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("old")})
	a.now = func() time.Time { return t0.Add(2 * time.Hour) }
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("new")})
	page := a.query(inputAuditQuery{Session: "s"})
	if len(page.Records) != 1 || page.Records[0].SHA256 != textSHA256("new") {
		t.Fatalf("records = %+v", page.Records)
	}
	if want := t0.Add(time.Hour); page.CoveredFrom.Before(want.Add(-time.Second)) {
		t.Errorf("coveredFrom = %v, want at or after the cut %v", page.CoveredFrom, want)
	}
	// the file was compacted and a restart agrees
	a.f.Close()
	b := newInputAuditStore()
	if err := b.load(st); err != nil {
		t.Fatal(err)
	}
	if got := b.query(inputAuditQuery{Session: "s"}); len(got.Records) != 1 {
		t.Errorf("after restart %+v", got.Records)
	}
}

// A write that fails never fails the input, and the gap is reported.
func TestInputAudit_AFailedWriteMovesCoveredFrom(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	before := a.coveredFrom
	a.f.Close() // every later write now fails
	time.Sleep(2 * time.Millisecond)
	id := a.begin(InputRecord{Session: "s", SHA256: textSHA256("x")})
	a.finish(id, "submitted")
	page := a.query(inputAuditQuery{Session: "s"})
	if len(page.Records) != 1 {
		t.Errorf("the in-memory record must still answer: %+v", page.Records)
	}
	if !page.CoveredFrom.After(before) {
		t.Errorf("coveredFrom did not move past the lost write: %v vs %v", page.CoveredFrom, before)
	}
}

func TestInputAudit_CapDropsTheOldestAndSaysSo(t *testing.T) {
	a := newInputAuditStore()
	first := a.coveredFrom
	for i := 0; i < maxInputAuditRecords+3; i++ {
		a.begin(InputRecord{Session: "s"})
	}
	page := a.query(inputAuditQuery{Session: "s", Limit: maxInputAuditLimit})
	a.mu.Lock()
	n := len(a.recs)
	a.mu.Unlock()
	if n != maxInputAuditRecords {
		t.Errorf("kept %d, want the cap %d", n, maxInputAuditRecords)
	}
	if !page.CoveredFrom.After(first) && !page.CoveredFrom.Equal(first) {
		t.Errorf("coveredFrom moved backwards")
	}
}

// muster #331. A write that did not finish leaves a partial line; the service
// must start over it and say what it lost, and must not leave one mid-file.

func writeAuditLog(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, inputAuditFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A crash mid-append leaves a partial final line. It is trimmed, the whole
// records before it are kept (a pending one reads unknown), and coveredFrom
// moves past the lost write.
func TestInputAudit_TornTailIsTrimmedAndCoveredFromMoves(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("one")}) // pending, whole
	a.f.Close()
	path := filepath.Join(dir, inputAuditFile)
	whole, _ := os.ReadFile(path)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"rec":{"id":"x-2","at":"2026-01-01T00:0`) // cut mid-line, no newline
	f.Close()

	b := newInputAuditStore()
	loadedAt := time.Now().Add(time.Hour)
	b.now = func() time.Time { return loadedAt }
	if err := b.load(st); err != nil {
		t.Fatalf("a torn tail stopped the service from starting: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(whole) {
		t.Errorf("the partial line was not trimmed away:\n%q\nwant\n%q", got, whole)
	}
	page := b.query(inputAuditQuery{Session: "s"})
	if len(page.Records) != 1 || page.Records[0].Outcome != inputOutcomeUnknown {
		t.Fatalf("records = %+v, want the one whole record, read as unknown", page.Records)
	}
	if !page.CoveredFrom.Equal(loadedAt.UTC()) {
		t.Errorf("coveredFrom = %v, want %v — a reader must be told of the gap", page.CoveredFrom, loadedAt.UTC())
	}
	// and appending after it produces a log that loads clean
	id := b.begin(InputRecord{Session: "s", SHA256: textSHA256("two")})
	b.finish(id, "submitted")
	b.f.Close()
	c := newInputAuditStore()
	if err := c.load(st); err != nil {
		t.Fatalf("log after recovery does not load: %v", err)
	}
	if got := c.query(inputAuditQuery{Session: "s"}); len(got.Records) != 2 {
		t.Errorf("after recovery %+v", got.Records)
	}
}

// A final record that is whole but lost only its newline is kept, and the next
// append starts a new line rather than running into it.
func TestInputAudit_WholeRecordWithoutNewlineIsKept(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("one")})
	a.f.Close()
	path := filepath.Join(dir, inputAuditFile)
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(string(raw), "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newInputAuditStore()
	if err := b.load(st); err != nil {
		t.Fatal(err)
	}
	b.begin(InputRecord{Session: "s", SHA256: textSHA256("two")})
	b.f.Close()
	c := newInputAuditStore()
	if err := c.load(st); err != nil {
		t.Fatalf("the appended record ran into the unterminated one: %v", err)
	}
	if got := c.query(inputAuditQuery{Session: "s"}); len(got.Records) != 2 {
		t.Errorf("records = %+v, want both", got.Records)
	}
}

// shortWriter writes half of a line and fails, as a full disk does.
type shortWriter struct {
	*os.File
	failNext bool
	noTrunc  bool
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.failNext {
		w.failNext = false
		n, _ := w.File.Write(p[:len(p)/2])
		return n, errors.New("no space left on device")
	}
	return w.File.Write(p)
}

func (w *shortWriter) Truncate(n int64) error {
	if w.noTrunc {
		return errors.New("truncate refused")
	}
	return w.File.Truncate(n)
}

// A short write is taken back out of the file, so a later good append does not
// bury a fragment mid-file, and the log still loads.
func TestInputAudit_ShortWriteIsNotLeftInTheFile(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	before := a.coveredFrom
	sw := &shortWriter{File: a.f.(*os.File)}
	a.f = sw
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("one")})
	sw.failNext = true
	time.Sleep(2 * time.Millisecond)
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("lost")})
	if !a.coveredFrom.After(before) {
		t.Errorf("coveredFrom did not move past the lost write")
	}
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("three")})
	sw.Close()

	b := newInputAuditStore()
	if err := b.load(st); err != nil {
		t.Fatalf("a short write followed by a good append broke the log: %v", err)
	}
	got := b.query(inputAuditQuery{Session: "s"})
	if len(got.Records) != 2 || got.Records[0].SHA256 != textSHA256("one") || got.Records[1].SHA256 != textSHA256("three") {
		t.Errorf("records = %+v, want the first and third", got.Records)
	}
}

// If the fragment cannot be taken out, the log stops taking appends, so the
// fragment stays the final line and the next start trims it.
func TestInputAudit_ShortWriteThatCannotBeTrimmedStopsAppending(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	sw := &shortWriter{File: a.f.(*os.File), failNext: true, noTrunc: true}
	a.f = sw
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("lost")})
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("after")})
	if a.f != nil {
		t.Errorf("the log kept taking appends after a fragment it could not remove")
	}
	b := newInputAuditStore()
	if err := b.load(st); err != nil {
		t.Fatalf("the fragment was not recoverable as a torn tail: %v", err)
	}
}

// A newline-terminated bad line is still damage nobody here caused, mid-file or
// last.
func TestInputAudit_TerminatedCorruptLinesStillRefuse(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	a := newInputAuditStore()
	if err := a.load(st); err != nil {
		t.Fatal(err)
	}
	a.begin(InputRecord{Session: "s", SHA256: textSHA256("one")})
	a.f.Close()
	path := filepath.Join(dir, inputAuditFile)
	raw, _ := os.ReadFile(path)
	writeAuditLog(t, dir, string(raw)+"{torn\n"+string(raw))
	if err := newInputAuditStore().load(st); err == nil {
		t.Fatal("a corrupt mid-file line was accepted")
	}
	writeAuditLog(t, dir, string(raw)+"{torn\n")
	if err := newInputAuditStore().load(st); err == nil {
		t.Fatal("a newline-terminated corrupt final line was accepted")
	}
	if got, _ := os.ReadFile(path); string(got) != string(raw)+"{torn\n" {
		t.Errorf("the log was modified: %q", got)
	}
}
