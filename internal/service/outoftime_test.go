package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// A call that ran out of time is answered as running out of time (muster #476):
// unreachable, retryable, with a message that names the budget that ended it. It
// is never invalid, which a client reads as a refusal and stops retrying.

func envelopeError(t *testing.T, rec *httptest.ResponseRecorder) fleet.Error {
	t.Helper()
	var env fleet.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decoding the error envelope: %v", err)
	}
	return env.Error
}

func TestDriverOutOfTimeErrorsNameTheBudget(t *testing.T) {
	own := &driver.StepTimeout{Step: "the wait for the runtime to become ready", Budget: 10 * time.Second, Err: context.DeadlineExceeded}
	cut := &driver.StepTimeout{Step: "the wait for the runtime's model catalog to settle", Err: context.DeadlineExceeded}
	gone := &driver.StepTimeout{Step: "the wait for the runtime to become ready", Err: context.Canceled}
	cases := []struct {
		name     string
		err      error
		contains []string
		excludes []string
	}{
		{"a plain deadline keeps the message the client guide shows", context.DeadlineExceeded,
			[]string{"no answer within 30s"}, []string{"budget", "cancelled"}},
		{"a step's own budget is named, not the caller's deadline", fmt.Errorf("opencode: server did not become ready: %w", own),
			[]string{"the wait for the runtime to become ready", "its own budget of 10s", "30s had not run out"}, []string{"no answer within"}},
		{"a deadline that ended a named step says which step", fmt.Errorf("create: %w", cut),
			[]string{"no answer within 30s", "the wait for the runtime's model catalog to settle"}, []string{"its own budget"}},
		{"a cancelled caller is a cancellation", context.Canceled,
			[]string{"cancelled"}, []string{"no answer within"}},
		{"a wrapped cancellation too", fmt.Errorf("create: %w", gone),
			[]string{"cancelled"}, []string{"no answer within", "its own budget"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeDriverError(rec, "testbox", 30*time.Second, c.err)
			if rec.Code != http.StatusGatewayTimeout {
				t.Fatalf("status = %d, want 504", rec.Code)
			}
			got := envelopeError(t, rec)
			if got.Kind != fleet.ErrorUnreachable || !got.Retryable {
				t.Errorf("kind %q retryable %v, want unreachable and retryable", got.Kind, got.Retryable)
			}
			for _, want := range c.contains {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q does not say %q", got.Message, want)
				}
			}
			for _, not := range c.excludes {
				if strings.Contains(got.Message, not) {
					t.Errorf("message %q says %q", got.Message, not)
				}
			}
		})
	}
}

// The message is fixed wording and durations: the wrapped error's own text, which
// can hold the runtime's loopback address or a working directory, is not copied.
func TestOutOfTimeMessageDoesNotCopyTheWrappedText(t *testing.T) {
	err := fmt.Errorf("opencode: server at 127.0.0.1:41234 in /some/dir did not become ready: %w",
		&driver.StepTimeout{Step: "the wait for the runtime to become ready", Budget: 10 * time.Second, Err: context.DeadlineExceeded})
	msg := outOfTimeMessage(err, 30*time.Second)
	for _, leak := range []string{"127.0.0.1", "/some/dir", "opencode:"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message %q carries %q from the wrapped error", msg, leak)
		}
	}
}

// lateCreateDriver is a stub whose Create fails with err, as a create does that
// ran out of time.
type lateCreateDriver struct {
	stub.Driver
	err error
}

func (d *lateCreateDriver) Create(context.Context, fleet.Request, string, fleet.SessionSpec) (fleet.Session, error) {
	return fleet.Session{}, d.err
}

// Through the real mux: the response a client sees, and the audit line an
// operator reads afterwards, both say a create that ran out of time failed and
// can be retried.
func TestCreateThatRunsOutOfTimeIsAnsweredAndAuditedAsFailed(t *testing.T) {
	for name, err := range map[string]error{
		"a step's budget": fmt.Errorf("opencode: server did not become ready: %w",
			&driver.StepTimeout{Step: "the wait for the runtime to become ready", Budget: time.Second, Err: context.DeadlineExceeded}),
		"a cancelled caller": fmt.Errorf("create: %w", context.Canceled),
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureAuditLog(t)
			srv := createServer(t, &lateCreateDriver{Driver: stub.Driver{DeadlineMs: 1000}, err: err})

			req, rerr := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/testbox/sessions", strings.NewReader(`{"cwd":"/tmp","name":"s1"}`))
			if rerr != nil {
				t.Fatal(rerr)
			}
			req.Header.Set("Authorization", "Bearer tok")
			req.Header.Set("Idempotency-Key", "k-late")
			resp, rerr := http.DefaultClient.Do(req)
			if rerr != nil {
				t.Fatal(rerr)
			}
			defer resp.Body.Close()
			var env fleet.ErrorEnvelope
			if derr := json.NewDecoder(resp.Body).Decode(&env); derr != nil {
				t.Fatal(derr)
			}
			if resp.StatusCode != http.StatusGatewayTimeout || !env.Error.Retryable || env.Error.Kind != fleet.ErrorUnreachable {
				t.Errorf("status %d, kind %q, retryable %v; want 504, unreachable, retryable", resp.StatusCode, env.Error.Kind, env.Error.Retryable)
			}
			line := auditLine(t, logs.String(), "create")
			if !strings.Contains(line, "status=504") || strings.Contains(line, "outcome=refused") {
				t.Errorf("audit line %q: want a failure with status=504, not a refusal", line)
			}
			if !strings.Contains(line, "phase.create=") {
				t.Errorf("audit line %q lacks phase.create", line)
			}
		})
	}
}
