package service

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #450: a slow create must be explainable from the service's own log
// line. These tests read the real audit line a real request through NewMux
// writes, not a helper's return value.

// phasingDriver is a stub that, like the tmux driver, notes a phase inside its own
// Create and takes a measurable moment doing it.
type phasingDriver struct {
	stub.Driver
	pause time.Duration
}

func (d *phasingDriver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	start := time.Now()
	time.Sleep(d.pause)
	driver.NotePhase(ctx, "terminal", start)
	return fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "testbox", ID: spec.Name, Name: spec.Name},
		Cwd:        spec.Cwd,
	}, nil
}

func (d *phasingDriver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	return fleet.SessionState{Status: fleet.StatusIdle}, nil
}

// captureAuditLog redirects the standard logger for one test. The audit lines go to it.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(b)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureAuditLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := log.Writer()
	log.SetOutput(c)
	t.Cleanup(func() { log.SetOutput(prev) })
	return c
}

func auditLine(t *testing.T, logs, verb string) string {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "audit:") && strings.Contains(line, "verb="+verb+" ") {
			return line
		}
	}
	t.Fatalf("no audit line with verb=%s in:\n%s", verb, logs)
	return ""
}

func createServer(t *testing.T, d driver.Driver) *httptest.Server {
	t.Helper()
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: []Principal{
		{Name: "worker", Token: "tok", Grants: []Grant{GrantRead, GrantCreate, GrantSend}},
	}}))
	t.Cleanup(srv.Close)
	return srv
}

func postCreate(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/testbox/sessions",
		strings.NewReader(`{"cwd":"/tmp","name":"s1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Idempotency-Key", "k-took")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

var tookField = regexp.MustCompile(` took=(\d+)(?: |$)`)

func TestCreateAuditLineCarriesTookAndItsPhases(t *testing.T) {
	logs := captureAuditLog(t)
	srv := createServer(t, &phasingDriver{Driver: stub.Driver{DeadlineMs: 1000}, pause: 40 * time.Millisecond})

	if code := postCreate(t, srv); code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	line := auditLine(t, logs.String(), "create")

	m := tookField.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("audit line has no took=<ms>: %s", line)
	}
	// The pause is in the driver, so the total cannot be shorter than it, and
	// the line must not have been measured around something else.
	if got := atoi(t, m[1]); got < 40 {
		t.Errorf("took=%d, want >= 40 (the driver paused 40 ms)", got)
	}
	// resolve, then the driver's own phase, then the whole create, then the state
	// read: the order they finished in.
	order := []string{"phase.resolve=", "phase.terminal=", "phase.create=", "phase.state="}
	at := -1
	for _, want := range order {
		i := strings.Index(line, want)
		if i < 0 {
			t.Fatalf("audit line lacks %s: %s", want, line)
		}
		if i < at {
			t.Errorf("%s is out of order in: %s", want, line)
		}
		at = i
	}
	if got := atoi(t, field(t, line, "phase.terminal")); got < 40 {
		t.Errorf("phase.terminal=%d, want >= 40", got)
	}
	// terminal is NESTED inside create (the spec says so): it can never exceed it.
	if term, create := atoi(t, field(t, line, "phase.terminal")), atoi(t, field(t, line, "phase.create")); term > create {
		t.Errorf("phase.terminal=%d exceeds phase.create=%d, but it is nested inside it", term, create)
	}
	// Existing readers of the line still find their fields, in their old order.
	if !strings.Contains(line, `outcome=performed status=201 took=`) {
		t.Errorf("took must follow status= so prefix readers are unaffected: %s", line)
	}
}

func TestEveryMutatingVerbCarriesTookButOnlyMeasuredPhases(t *testing.T) {
	logs := captureAuditLog(t)
	srv := createServer(t, &stub.Driver{DeadlineMs: 1000})

	call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok")
	line := auditLine(t, logs.String(), "send")
	if tookField.FindStringSubmatch(line) == nil {
		t.Fatalf("a send audit line has no took=: %s", line)
	}
	if strings.Contains(line, "phase.") {
		t.Errorf("send measured no phase, so the line must print none (not a zero): %s", line)
	}
}

func TestDeniedLinesAreUnchanged(t *testing.T) {
	logs := captureAuditLog(t)
	srv := createServer(t, &stub.Driver{DeadlineMs: 1000})

	// worker holds no close grant.
	call(t, srv, http.MethodDelete, "/v1/machines/testbox/sessions/s1", "tok")
	line := auditLine(t, logs.String(), "close")
	if !strings.HasSuffix(line, "outcome=DENIED") {
		t.Errorf("a denial ran nothing, so it carries no duration: %s", line)
	}
}

func field(t *testing.T, line, name string) string {
	t.Helper()
	m := regexp.MustCompile(` ` + regexp.QuoteMeta(name) + `=(\d+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no %s= in: %s", name, line)
	}
	return m[1]
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
