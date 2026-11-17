package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #416: a client refused 560 times a day left `outcome=refused status=400`
// in the log and nothing that said why — the reason was in the response body the
// client never logged. These tests read the audit line a real request through
// NewMux writes.

// A create with no cwd is the case the issue was filed from. The line must carry
// the kind and the message the caller was shown, after the fields it always had.
func TestARefusedCreateSaysWhyInItsAuditLine(t *testing.T) {
	logs := captureAuditLog(t)
	srv := createServer(t, &refusingCreateDriver{})

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/testbox/sessions",
		strings.NewReader(`{"name":"s1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Idempotency-Key", "k-no-cwd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with no cwd: status %d, want 400", resp.StatusCode)
	}

	line := auditLine(t, logs.String(), "create")
	for _, want := range []string{
		"outcome=refused status=400",
		" kind=invalid",
		` reason="create: cwd is required"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line lacks %q: %s", want, line)
		}
	}
	// The new tokens come last, so a reader of the line up to took= is unaffected.
	if i, j := strings.Index(line, " took="), strings.Index(line, " kind="); i < 0 || j < i {
		t.Errorf("kind= must follow took=: %s", line)
	}
}

// A mutation that ran leaves no kind and no reason: nothing was refused.
func TestAPerformedMutationCarriesNoKindOrReason(t *testing.T) {
	logs := captureAuditLog(t)
	srv := createServer(t, &phasingDriver{Driver: stub.Driver{DeadlineMs: 1000}})

	if code := postCreate(t, srv); code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	line := auditLine(t, logs.String(), "create")
	if strings.Contains(line, " kind=") || strings.Contains(line, " reason=") {
		t.Errorf("a performed create must not carry kind or reason: %s", line)
	}
}

// A request refused for want of a grant ran nothing, so it carries no duration,
// but it says which grant — the DENIED line is the other half of the issue.
func TestADeniedLineSaysWhichGrantIsMissing(t *testing.T) {
	logs := captureAuditLog(t)
	srv := createServer(t, &stub.Driver{DeadlineMs: 1000})

	// worker holds no close grant.
	call(t, srv, http.MethodDelete, "/v1/machines/testbox/sessions/s1", "tok")
	line := auditLine(t, logs.String(), "close")
	if !strings.Contains(line, "outcome=DENIED kind=unauthorized reason=") {
		t.Errorf("a denial must say kind and reason right after the outcome: %s", line)
	}
	if !strings.Contains(line, `reason="principal worker does not hold the close grant`) {
		t.Errorf("a denial must name the principal and the missing grant: %s", line)
	}
	if strings.Contains(line, "took=") {
		t.Errorf("a denial ran nothing, so it carries no duration: %s", line)
	}
}

// The read lines are the same defect: a refused turns read said only status=400.
func TestARefusedTurnsReadSaysWhyInItsAuditLine(t *testing.T) {
	logs := captureAuditLog(t)
	srv := createServer(t, &stub.Driver{DeadlineMs: 1000})

	if code := call(t, srv, http.MethodGet, "/v1/machines/testbox/sessions/s1/turns?limit=0", "tok"); code != http.StatusBadRequest {
		t.Fatalf("turns with limit=0: status %d, want 400", code)
	}
	line := auditLine(t, logs.String(), "read-turns")
	for _, want := range []string{"outcome=refused status=400", " kind=invalid", ` reason="limit must be an integer from 1 to `} {
		if !strings.Contains(line, want) {
			t.Errorf("read-turns line lacks %q: %s", want, line)
		}
	}
}

// refusingCreateDriver refuses a create with no cwd the way a runtime does.
type refusingCreateDriver struct{ stub.Driver }

func (d *refusingCreateDriver) Create(_ context.Context, _ fleet.Request, _ string, spec fleet.SessionSpec) (fleet.Session, error) {
	if spec.Cwd == "" {
		return fleet.Session{}, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: cwd is required", Machine: "testbox"}
	}
	return fleet.Session{}, nil
}

func TestClipReasonKeepsAShortMessageAndBoundsALongOne(t *testing.T) {
	if got := clipReason("create: cwd is required"); got != "create: cwd is required" {
		t.Errorf("a short message is unchanged, got %q", got)
	}
	exact := strings.Repeat("a", auditReasonMax)
	if got := clipReason(exact); got != exact {
		t.Errorf("a message of exactly %d bytes is unchanged", auditReasonMax)
	}
	long := strings.Repeat("a", 5000)
	got := clipReason(long)
	if len(got) != auditReasonMax || !strings.HasSuffix(got, "...") {
		t.Errorf("a long message is cut to %d bytes ending in ..., got %d bytes: %q", auditReasonMax, len(got), got)
	}
}

// Cutting through a multi-byte character would put an invalid sequence in the log.
func TestClipReasonCutsOnARuneBoundary(t *testing.T) {
	// "é" is two bytes; an odd offset would land inside one.
	for _, prefix := range []string{"", "x"} {
		s := prefix + strings.Repeat("é", 300)
		got := clipReason(s)
		if !utf8.ValidString(got) {
			t.Errorf("prefix %q: the cut left invalid UTF-8: %q", prefix, got)
		}
		if len(got) > auditReasonMax || !strings.HasSuffix(got, "...") {
			t.Errorf("prefix %q: %d bytes, want <= %d and a ... marker", prefix, len(got), auditReasonMax)
		}
	}
}

// A message is the caller's words as much as ours: it must not be able to end the
// line or open a field of its own.
func TestErrorNoteFieldsQuoteWhatTheyCarry(t *testing.T) {
	got := noteOf(&fleet.Error{Kind: fleet.ErrorInvalid, Message: "bad\nnext=1 \"quoted\""}).fields()
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("a newline in the message reached the line: %q", got)
	}
	if want := ` kind=invalid reason="bad\nnext=1 \"quoted\""`; got != want {
		t.Errorf("fields = %q, want %q", got, want)
	}

	// A relayed error carries whatever the peer sent as its kind.
	got = noteOf(&fleet.Error{Kind: "weird kind\nx=1", Message: "m"}).fields()
	if strings.ContainsAny(got, "\n\r") || !strings.Contains(got, ` kind="weird kind\nx=1"`) {
		t.Errorf("a kind that is not a token must be quoted: %q", got)
	}

	if got := noteOf(&fleet.Error{Kind: fleet.ErrorUnreachable}).fields(); got != " kind=unreachable" {
		t.Errorf("an error with no message prints no reason, got %q", got)
	}
	if got := noteOf(nil).fields(); got != "" {
		t.Errorf("no error, no fields; got %q", got)
	}
}
