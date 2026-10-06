package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// turnsDriver is stub.Driver that can read turns and remembers what it was asked.
type turnsDriver struct {
	stub.Driver
	mu   sync.Mutex
	got  []fleet.TurnsQuery
	reqs []fleet.Request
	page fleet.TurnsPage
	err  error
}

func (d *turnsDriver) Turns(_ context.Context, req fleet.Request, _ fleet.SessionRef, q fleet.TurnsQuery) (fleet.TurnsPage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, q)
	d.reqs = append(d.reqs, req)
	return d.page, d.err
}

func turnsServer(t *testing.T, d *turnsDriver, cfg Config) *httptest.Server {
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

func getTurns(t *testing.T, srv *httptest.Server, token, query string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/owner/sessions/s1/turns"+query, nil)
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

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func TestTurns_ServesThePageAndForwardsTheQuery(t *testing.T) {
	at := time.Date(2026, 10, 4, 10, 0, 4, 0, time.UTC)
	d := &turnsDriver{Driver: stub.Driver{DeadlineMs: 500},
		page: fleet.TurnsPage{Turns: []fleet.Turn{{At: at, Text: "what the agent said"}}, Next: "cur-2"}}
	srv := turnsServer(t, d, Config{})
	logs := captureLog(t)

	started := "2026-10-04T09:00:00Z"
	resp, body := getTurns(t, srv, testToken, "?since=cur-1&limit=7&startedAt="+started)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var page fleet.TurnsPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.Turns[0].Text != "what the agent said" || !page.Turns[0].At.Equal(at) || page.Next != "cur-2" {
		t.Fatalf("page = %+v", page)
	}
	if len(d.got) != 1 || d.got[0].Since != "cur-1" || d.got[0].Limit != 7 {
		t.Errorf("driver saw %+v", d.got)
	}
	if want := d.reqs[0].Expect.StartedAt; want == nil || want.UTC().Format(time.RFC3339) != started {
		t.Errorf("startedAt did not reach the driver: %v", want)
	}

	line := logs.String()
	for _, want := range []string{"audit:", "verb=read-turns", "owner/s1", "turns=1", "pending=0", "outcome=performed"} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "what the agent said") {
		t.Errorf("the audit line carries the content it is auditing:\n%s", line)
	}
}

func TestTurns_NothingNewIsAnEmptyListNotNull(t *testing.T) {
	d := &turnsDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.TurnsPage{Next: "cur"}}
	_, body := getTurns(t, turnsServer(t, d, Config{}), testToken, "")
	if !strings.Contains(string(body), `"turns":[]`) {
		t.Errorf("body = %s", body)
	}
}

func TestTurns_RefusalsAndErrorKinds(t *testing.T) {
	cases := []struct {
		name  string
		query string
		err   error
		kind  fleet.ErrorKind
	}{
		{"limit not a number", "?limit=lots", nil, fleet.ErrorInvalid},
		{"limit zero", "?limit=0", nil, fleet.ErrorInvalid},
		{"limit over the cap is refused, not clamped", "?limit=101", nil, fleet.ErrorInvalid},
		{"startedAt unparseable is not silently dropped", "?startedAt=yesterday", nil, fleet.ErrorInvalid},
		{"any other driver failure", "", errors.New("x"), fleet.ErrorInvalid},
		{"stale", "", fleet.ErrAmbiguousTarget, fleet.ErrorConflict},
		{"no such session", "", fleet.ErrNoSuchSession, fleet.ErrorNotFound},
		{"no record yet", "", fleet.ErrNoTurnRecord, fleet.ErrorNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &turnsDriver{Driver: stub.Driver{DeadlineMs: 500}, err: c.err}
			resp, body := getTurns(t, turnsServer(t, d, Config{}), testToken, c.query)
			var env fleet.ErrorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
			if env.Error.Kind != c.kind {
				t.Errorf("kind = %s (%d), want %s: %s", env.Error.Kind, resp.StatusCode, c.kind, env.Error.Message)
			}
			if c.err == fleet.ErrNoTurnRecord && !env.Error.Retryable {
				t.Error("a record that is not there yet should be retryable")
			}
			if c.query != "" && len(d.got) != 0 {
				t.Error("a malformed query reached the driver")
			}
		})
	}
}

func TestTurns_RuntimeThatCannotReadTurnsIsUnsupported(t *testing.T) {
	svc := New("owner")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 500}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	t.Cleanup(srv.Close)
	resp, _ := getTurns(t, srv, testToken, "")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

// Content is gated on the grant that already governs speaking to a session; no
// new grant exists, and read alone is not enough.
func TestTurns_RequiresTheSendGrantUnderAPrincipalTable(t *testing.T) {
	d := &turnsDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.TurnsPage{Next: "c"}}
	srv := turnsServer(t, d, Config{Principals: []Principal{
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
		{Name: "operator", Token: "tok-op", Grants: []Grant{GrantSend}},
		{Name: "nobody", Token: "tok-none"},
	}})
	logs := captureLog(t)

	for _, tok := range []string{"tok-watch", "tok-none"} {
		resp, _ := getTurns(t, srv, tok, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tok, resp.StatusCode)
		}
	}
	if len(d.got) != 0 {
		t.Error("a refused caller reached the driver")
	}
	if !strings.Contains(logs.String(), "outcome=DENIED") {
		t.Errorf("a refusal left no audit line:\n%s", logs.String())
	}
	if resp, body := getTurns(t, srv, "tok-op", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("operator: status = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(logs.String(), `actor="operator"`) {
		t.Errorf("the audit line does not name the caller:\n%s", logs.String())
	}
	if resp, _ := getTurns(t, srv, "wrong", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown token: status = %d", resp.StatusCode)
	}
}

// muster#266: the pending entry rides on the same route, behind the same
// grant, and its audit line says THAT a pending entry left, never what it said.
func TestTurns_PendingTextIsServedAndAuditedWithoutItsContent(t *testing.T) {
	observed := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	d := &turnsDriver{Driver: stub.Driver{DeadlineMs: 500}, page: fleet.TurnsPage{Next: "cur", Pending: &fleet.PendingTurn{
		ObservedAt: observed, Text: "words above the question", Source: fleet.PendingSourceScreen, Nonce: "n-1"}}}
	logs := captureLog(t)
	resp, body := getTurns(t, turnsServer(t, d, Config{}), testToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var page fleet.TurnsPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if page.Pending == nil || page.Pending.Text != "words above the question" || page.Pending.Source != "screen" ||
		page.Pending.Nonce != "n-1" || !page.Pending.ObservedAt.Equal(observed) {
		t.Fatalf("pending = %+v", page.Pending)
	}
	if !strings.Contains(string(body), `"turns":[]`) {
		t.Errorf("a page with only a pending entry must still carry an empty turns list: %s", body)
	}
	if line := logs.String(); !strings.Contains(line, "pending=1") || strings.Contains(line, "words above the question") {
		t.Errorf("audit line:\n%s", line)
	}

	// No prompt open: no field at all, not a null.
	d.page = fleet.TurnsPage{Next: "cur"}
	_, body = getTurns(t, turnsServer(t, d, Config{}), testToken, "")
	if strings.Contains(string(body), "pending") {
		t.Errorf("body mentions pending with no prompt open: %s", body)
	}
}

// #268: a driver that recognises the addressed id as a live session's
// conversation id answers not_found with a reason a client can branch on and
// the session ids to use — the message never claims the session is gone.
func TestTurns_ConversationIdAddressNamesTheSessionIds(t *testing.T) {
	d := &turnsDriver{Driver: stub.Driver{DeadlineMs: 500}, err: fmt.Errorf("probe: %w",
		&fleet.ConversationIdError{ID: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70", SessionIds: []string{"alpha"}})}
	resp, body := getTurns(t, turnsServer(t, d, Config{}), testToken, "")
	var env fleet.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if env.Error.Kind != fleet.ErrorNotFound || resp.StatusCode != http.StatusNotFound {
		t.Errorf("kind = %s (%d), want not_found", env.Error.Kind, resp.StatusCode)
	}
	if env.Error.Reason != fleet.ReasonConversationId {
		t.Errorf("reason = %q, want %q", env.Error.Reason, fleet.ReasonConversationId)
	}
	if len(env.Error.SessionIds) != 1 || env.Error.SessionIds[0] != "alpha" {
		t.Errorf("sessionIds = %v, want [alpha]", env.Error.SessionIds)
	}
	if !strings.Contains(env.Error.Message, `"alpha"`) || !strings.Contains(env.Error.Message, "conversation id") {
		t.Errorf("message = %q, want it to name the session id", env.Error.Message)
	}
}
