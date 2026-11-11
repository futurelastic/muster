package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// recordDriver is stub.Driver that can read a record and remembers what it was
// asked.
type recordDriver struct {
	stub.Driver
	mu   sync.Mutex
	got  []fleet.RecordQuery
	reqs []fleet.Request
	page fleet.RecordPage
	err  error
}

func (d *recordDriver) Record(_ context.Context, req fleet.Request, _ fleet.SessionRef, q fleet.RecordQuery) (fleet.RecordPage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, q)
	d.reqs = append(d.reqs, req)
	return d.page, d.err
}

func (d *recordDriver) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.got)
}

func recordServer(t *testing.T, d *recordDriver, cfg Config) *httptest.Server {
	t.Helper()
	svc := New("owner")
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	if cfg.Token == "" && len(cfg.Principals) == 0 {
		cfg.Token = testToken
	}
	srv := httptest.NewServer(NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return srv
}

func getPath(t *testing.T, srv *httptest.Server, token, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func getRecord(t *testing.T, srv *httptest.Server, token, query string) (*http.Response, []byte) {
	t.Helper()
	return getPath(t, srv, token, "/v1/machines/owner/sessions/s1/record"+query)
}

const recordSecret = "SECRET-TEXT-MARKER"

func recordPageOf(n int) fleet.RecordPage {
	at := time.Date(2026, 10, 4, 10, 0, 4, 0, time.UTC)
	p := fleet.RecordPage{Next: "cur-secret-2", Format: fleet.RecordFormat}
	for i := 0; i < n; i++ {
		role := fleet.RecordRoleUser
		if i%2 == 1 {
			role = fleet.RecordRoleAssistant
		}
		p.Messages = append(p.Messages, fleet.RecordMessage{
			At: at.Add(time.Duration(i) * time.Second), Kind: fleet.RecordKindText, Role: role,
			Text: recordSecret + " entry", Model: "model-secret",
		})
	}
	return p
}

func TestRecord_ServesThePageAndForwardsTheQuery(t *testing.T) {
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: recordPageOf(2)}
	srv := recordServer(t, d, Config{})

	started := "2026-10-04T09:00:00Z"
	resp, body := getRecord(t, srv, testToken, "?since=cur-1&limit=7&startedAt="+started)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var page fleet.RecordPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Next != "cur-secret-2" || page.Format != fleet.RecordFormat {
		t.Fatalf("page = %+v", page)
	}
	if len(d.got) != 1 || d.got[0].Since != "cur-1" || d.got[0].Limit != 7 {
		t.Errorf("driver saw %+v", d.got)
	}
	if want := d.reqs[0].Expect.StartedAt; want == nil || want.UTC().Format(time.RFC3339) != started {
		t.Errorf("startedAt did not reach the driver: %v", want)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// The format is the answering driver's to state; the service fills it in only
// where a driver left it unset, so a relayed peer's value is never overwritten.
func TestRecord_FormatIsFilledOnlyWhenUnset(t *testing.T) {
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.RecordPage{Next: "c"}}
	_, body := getRecord(t, recordServer(t, d, Config{}), testToken, "")
	if !strings.Contains(string(body), `"format":1`) {
		t.Errorf("a driver that left the format unset: %s", body)
	}
	d = &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.RecordPage{Next: "c", Format: 7}}
	_, body = getRecord(t, recordServer(t, d, Config{}), testToken, "")
	if !strings.Contains(string(body), `"format":7`) {
		t.Errorf("the answering driver's format was overwritten: %s", body)
	}
}

func TestRecord_NothingNewIsAnEmptyListNotNull(t *testing.T) {
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.RecordPage{Next: "cur"}}
	_, body := getRecord(t, recordServer(t, d, Config{}), testToken, "")
	if !strings.Contains(string(body), `"messages":[]`) || !strings.Contains(string(body), `"next":"cur"`) {
		t.Errorf("body = %s", body)
	}
}

func TestRecord_LimitOverTheCapIsRefusedNotClamped(t *testing.T) {
	for _, q := range []string{"?limit=101", "?limit=0", "?limit=-1", "?limit=x", "?limit=1000000"} {
		d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.RecordPage{Next: "c"}}
		resp, body := getRecord(t, recordServer(t, d, Config{}), testToken, q)
		var env fleet.ErrorEnvelope
		if err := json.Unmarshal(body, &env); err != nil || env.Error.Kind != fleet.ErrorInvalid || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, %s; want 400 invalid", q, resp.StatusCode, body)
		}
		if !strings.Contains(env.Error.Message, "refused rather than clamped") {
			t.Errorf("%s: message %q does not say it is refused rather than clamped", q, env.Error.Message)
		}
		if d.calls() != 0 {
			t.Errorf("%s: the driver was reached", q)
		}
	}
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.RecordPage{Next: "c"}}
	if resp, body := getRecord(t, recordServer(t, d, Config{}), testToken, "?limit=100"); resp.StatusCode != http.StatusOK {
		t.Fatalf("limit=100: %d %s", resp.StatusCode, body)
	}
	if d.got[0].Limit != 100 {
		t.Errorf("the driver saw limit %d, want 100", d.got[0].Limit)
	}
}

func TestRecord_RefusalsAndErrorKinds(t *testing.T) {
	cases := []struct {
		name  string
		query string
		err   error
		kind  fleet.ErrorKind
	}{
		{"startedAt unparseable is not silently dropped", "?startedAt=yesterday", nil, fleet.ErrorInvalid},
		{"a value that is not a record cursor", "", errors.New("since is not a cursor"), fleet.ErrorInvalid},
		{"stale corroboration or foreign cursor", "", fleet.ErrAmbiguousTarget, fleet.ErrorConflict},
		{"no such session", "", fleet.ErrNoSuchSession, fleet.ErrorNotFound},
		{"no record yet", "", fleet.ErrNoTurnRecord, fleet.ErrorNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, err: c.err}
			resp, body := getRecord(t, recordServer(t, d, Config{}), testToken, c.query)
			var env fleet.ErrorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
			if env.Error.Kind != c.kind {
				t.Errorf("kind = %s (%d), want %s: %s", env.Error.Kind, resp.StatusCode, c.kind, env.Error.Message)
			}
			if wantRetry := c.err == fleet.ErrNoTurnRecord; env.Error.Retryable != wantRetry {
				t.Errorf("retryable = %v, want %v", env.Error.Retryable, wantRetry)
			}
			if c.query != "" && d.calls() != 0 {
				t.Error("a malformed query reached the driver")
			}
		})
	}
}

func TestRecord_RuntimeThatCannotReadARecordIsUnsupported(t *testing.T) {
	svc := New("owner")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 500}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	t.Cleanup(srv.Close)
	resp, body := getRecord(t, srv, testToken, "")
	if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "unsupported") {
		t.Errorf("status = %d: %s; want 501 unsupported", resp.StatusCode, body)
	}
}

// `record` is its own grant: nothing else stands in for it, and holding send —
// which guards turns — does not reveal a record.
func TestRecord_RequiresTheRecordGrant(t *testing.T) {
	var everythingButRecord []Grant
	for _, g := range Grants() {
		if g != GrantRecord {
			everythingButRecord = append(everythingButRecord, g)
		}
	}
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: recordPageOf(1)}
	srv := recordServer(t, d, Config{Principals: []Principal{
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
		{Name: "operator", Token: "tok-send", Grants: []Grant{GrantSend}},
		{Name: "everything", Token: "tok-all", Grants: everythingButRecord},
		{Name: "reader", Token: "tok-record", Grants: []Grant{GrantRecord}},
		{Name: "nobody", Token: "tok-none"},
	}})
	logs := captureLog(t)

	for _, tok := range []string{"tok-watch", "tok-send", "tok-all", "tok-none", "wrong"} {
		resp, _ := getRecord(t, srv, tok, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tok, resp.StatusCode)
		}
	}
	if d.calls() != 0 {
		t.Error("a refused caller reached the driver")
	}
	for _, want := range []string{`actor="operator"`, "verb=record", "outcome=DENIED"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("a refusal's audit line lacks %q:\n%s", want, logs.String())
		}
	}
	if resp, body := getRecord(t, srv, "tok-record", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("record holder: status = %d: %s", resp.StatusCode, body)
	}
	// And turns is unchanged: a send holder reads it, a record holder does not.
	if resp, _ := getTurns(t, srv, "tok-send", ""); resp.StatusCode == http.StatusUnauthorized {
		t.Error("holding send no longer reads turns")
	}
	if resp, _ := getTurns(t, srv, "tok-record", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("holding record reads turns: status = %d, want 401", resp.StatusCode)
	}
}

// With no principal table a valid token is the credential, exactly as for turns.
func TestRecord_NoPrincipalTableAValidTokenSuffices(t *testing.T) {
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: recordPageOf(1)}
	srv := recordServer(t, d, Config{})
	if resp, body := getRecord(t, srv, testToken, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("valid token: status = %d: %s", resp.StatusCode, body)
	}
	if resp, _ := getRecord(t, srv, "wrong", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", resp.StatusCode)
	}
}

// The operator's switch refuses every call, whatever is asked and whoever asks,
// and says why in a reason a client can show.
func TestRecord_SwitchRefusesEveryCall(t *testing.T) {
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: recordPageOf(1)}
	srv := recordServer(t, d, Config{RefuseRecord: true})
	logs := captureLog(t)

	for _, q := range []string{"", "?since=c&limit=5", "?limit=101"} {
		resp, body := getRecord(t, srv, testToken, q)
		var env fleet.ErrorEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("%s: %d %s", q, resp.StatusCode, body)
		}
		if resp.StatusCode != http.StatusNotImplemented || env.Error.Kind != fleet.ErrorUnsupported || env.Error.Reason != fleet.ReasonRecordOff {
			t.Errorf("%s: status %d, error %+v; want 501 unsupported %q", q, resp.StatusCode, env.Error, fleet.ReasonRecordOff)
		}
		if !strings.Contains(env.Error.Message, "refuseRecordReads") {
			t.Errorf("%s: the message does not name the switch: %q", q, env.Error.Message)
		}
	}
	// A peer-target path is refused here too, before any relay.
	if resp, body := getPath(t, srv, testToken, "/v1/machines/elsewhere/sessions/s1/record"); resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), fleet.ReasonRecordOff) {
		t.Errorf("peer target: status %d: %s", resp.StatusCode, body)
	}
	if d.calls() != 0 {
		t.Error("the driver was reached despite the switch")
	}
	for _, want := range []string{"audit: content-read", "entries=0", "reason=record-off", "outcome=refused"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the refusal's audit line lacks %q:\n%s", want, logs.String())
		}
	}
}

// Grant first, switch second: a caller without the grant learns nothing about
// how this machine is configured, and a holder is told the switch is the reason.
func TestRecord_SwitchComesAfterTheGrant(t *testing.T) {
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: recordPageOf(1)}
	srv := recordServer(t, d, Config{RefuseRecord: true, Principals: []Principal{
		{Name: "reader", Token: "tok-record", Grants: []Grant{GrantRecord}},
		{Name: "operator", Token: "tok-send", Grants: []Grant{GrantSend}},
	}})
	resp, body := getRecord(t, srv, "tok-send", "")
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), fleet.ReasonRecordOff) {
		t.Errorf("a non-holder: status %d: %s; want a plain 401 that does not mention the switch", resp.StatusCode, body)
	}
	resp, body = getRecord(t, srv, "tok-record", "")
	if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), fleet.ReasonRecordOff) {
		t.Errorf("a holder: status %d: %s; want 501 record-off", resp.StatusCode, body)
	}
}

// A content read and a state read are told apart in the trail, and the trail
// never carries what was read.
func TestRecord_AuditTellsAContentReadFromAStateRead(t *testing.T) {
	d := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}, page: recordPageOf(3)}
	srv := recordServer(t, d, Config{Principals: []Principal{
		{Name: "reader", Token: "tok-record", Grants: []Grant{GrantRecord, GrantRead, GrantSend}},
	}})
	logs := captureLog(t)

	if resp, _ := getPath(t, srv, "tok-record", "/v1/health"); resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", resp.StatusCode)
	}
	getTurns(t, srv, "tok-record", "")
	if strings.Contains(logs.String(), "audit: content-read") {
		t.Fatalf("a state read or a turns read wrote a content-read line:\n%s", logs.String())
	}

	if resp, body := getRecord(t, srv, "tok-record", "?since=cursor-secret-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("record: %d %s", resp.StatusCode, body)
	}
	var lines []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "audit: content-read") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d content-read lines, want exactly one:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{
		`actor="reader"`, "route=GET /v1/machines/{machine}/sessions/{id}/record", "target=owner/s1",
		"entries=3", "outcome=performed", "status=200", "took=",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("content-read line lacks %q: %s", want, lines[0])
		}
	}
	for _, leak := range []string{recordSecret, "cur-secret", "cursor-secret", "model-secret"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("the audit trail carries %q:\n%s", leak, logs.String())
		}
	}
}

// A session may be named "record"; its state read is not the record route.
func TestIsRecordPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/machines/m/sessions/s1/record":     true,
		"/v1/machines/m/sessions/record":        false,
		"/v1/machines/m/sessions/s1/turns":      false,
		"/v1/machines/m/sessions/s1/records":    false,
		"/v1/sessions/record":                   false,
		"/v1/machines/m/sessions/record/record": true,
	} {
		if got := isRecordPath(path); got != want {
			t.Errorf("isRecordPath(%q) = %v, want %v", path, got, want)
		}
	}
}
