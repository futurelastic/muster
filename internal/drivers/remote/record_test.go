package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/service"
)

// A peer's record reader, as the machine that owns the session would have it.
type peerRecordDriver struct {
	stub.Driver
	q    fleet.RecordQuery
	req  fleet.Request
	page fleet.RecordPage
	err  error
}

func (d *peerRecordDriver) Record(_ context.Context, req fleet.Request, _ fleet.SessionRef, q fleet.RecordQuery) (fleet.RecordPage, error) {
	d.q, d.req = q, req
	return d.page, d.err
}

// peerServiceWith is peerService with the service configuration in the caller's
// hands, for the tests that need the peer's own switch.
func peerServiceWith(t *testing.T, self fleet.MachineId, d *peerRecordDriver, cfg service.Config) string {
	t.Helper()
	svc := service.New(self)
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatalf("registering driver on the peer: %v", err)
	}
	srv := httptest.NewServer(service.NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return srv.URL
}

func readRecordVia(t *testing.T, srv *httptest.Server, token, query string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/peerbox/sessions/s1/record"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp.StatusCode, b.Bytes()
}

func ownersPage() fleet.RecordPage {
	at := time.Date(2026, 10, 4, 10, 0, 4, 0, time.UTC)
	return fleet.RecordPage{
		Messages: []fleet.RecordMessage{
			{At: at, Kind: fleet.RecordKindText, Role: fleet.RecordRoleUser, Text: "asked on the peer", Truncated: true},
			{At: at.Add(time.Second), Kind: fleet.RecordKindText, Role: fleet.RecordRoleAssistant, Text: "said on the peer", Model: "model-x"},
		},
		Next:   "peer-cursor",
		Format: fleet.RecordFormat,
	}
}

// A caller on one machine reads a session's record on a peer — through a real
// HTTP hop, with the query and the caller's corroboration arriving at the
// machine that owns the session, and the page coming back exactly as the owner
// wrote it.
func TestFederatedRecordReturnsWhatTheOwnerReturns(t *testing.T) {
	const token = "federation-token"
	owner := ownersPage()
	pd := &peerRecordDriver{Driver: stub.Driver{DeadlineMs: 2000}, page: owner}
	base := peerServiceWith(t, "peerbox", pd, service.Config{Token: token})
	srv := entryService(t, token, New("peerbox", base, WithDeadline(2*time.Second)))

	status, body := readRecordVia(t, srv, token, "?since=c1&limit=5&startedAt=2026-10-04T09:00:00Z")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var page fleet.RecordPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page, owner) {
		t.Fatalf("entry machine returned %+v, the owner returned %+v", page, owner)
	}
	if pd.q.Since != "c1" || pd.q.Limit != 5 {
		t.Errorf("peer driver saw %+v", pd.q)
	}
	if want := pd.req.Expect.StartedAt; want == nil || want.UTC().Format(time.RFC3339) != "2026-10-04T09:00:00Z" {
		t.Errorf("startedAt did not reach the machine that owns the session: %v", want)
	}
}

func TestFederatedRecordKindsSurviveTheHop(t *testing.T) {
	const token = "federation-token"
	for name, tc := range map[string]struct {
		err       error
		kind      fleet.ErrorKind
		retryable bool
	}{
		"stale corroboration": {fleet.ErrAmbiguousTarget, fleet.ErrorConflict, false},
		"no such session":     {fleet.ErrNoSuchSession, fleet.ErrorNotFound, false},
		"no record":           {fleet.ErrNoTurnRecord, fleet.ErrorNotFound, true},
	} {
		t.Run(name, func(t *testing.T) {
			pd := &peerRecordDriver{Driver: stub.Driver{DeadlineMs: 2000}, err: tc.err}
			srv := entryService(t, token, New("peerbox", peerServiceWith(t, "peerbox", pd, service.Config{Token: token})))
			_, body := readRecordVia(t, srv, token, "")
			var env fleet.ErrorEnvelope
			if err := json.Unmarshal(body, &env); err != nil || env.Error.Kind != tc.kind || env.Error.Retryable != tc.retryable {
				t.Fatalf("kind = %q retryable = %v (%v), want %q %v: %s", env.Error.Kind, env.Error.Retryable, err, tc.kind, tc.retryable, body)
			}
		})
	}
}

// A peer on a build that has no record route, or a runtime that cannot read one,
// is `unsupported` — never an empty record.
func TestFederatedRecordAbsentOnThePeerIsUnsupported(t *testing.T) {
	const token = "federation-token"

	older := httptest.NewServer(http.NotFoundHandler()) // a bare 404, as a build predating the route answers
	t.Cleanup(older.Close)
	srv := entryService(t, token, New("peerbox", older.URL))
	_, body := readRecordVia(t, srv, token, "")
	if !strings.Contains(string(body), `"unsupported"`) || strings.Contains(string(body), `"messages"`) {
		t.Errorf("older peer: %s", body)
	}

	srv = entryService(t, token, New("peerbox", peerService(t, "peerbox", "stub", &stub.Driver{DeadlineMs: 2000}, token, false)))
	_, body = readRecordVia(t, srv, token, "")
	if !strings.Contains(string(body), `"unsupported"`) {
		t.Errorf("runtime without a record reader: %s", body)
	}
}

// The owning machine's switch is its own: a call relayed to it from a machine
// that serves the route still arrives refused, with the reason a client can show.
func TestFederatedRecordPeerSwitchReasonSurvives(t *testing.T) {
	const token = "federation-token"
	pd := &peerRecordDriver{Driver: stub.Driver{DeadlineMs: 2000}, page: ownersPage()}
	base := peerServiceWith(t, "peerbox", pd, service.Config{Token: token, RefuseRecord: true})
	srv := entryService(t, token, New("peerbox", base))

	status, body := readRecordVia(t, srv, token, "")
	var env fleet.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if status != http.StatusNotImplemented || env.Error.Kind != fleet.ErrorUnsupported || env.Error.Reason != fleet.ReasonRecordOff {
		t.Fatalf("status = %d, error = %+v; want 501 unsupported with reason %q", status, env.Error, fleet.ReasonRecordOff)
	}
	if pd.q != (fleet.RecordQuery{}) {
		t.Errorf("the owner's driver was reached despite its switch: %+v", pd.q)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A relayed read is audited on both machines — one content-read line each — and
// neither carries what was said.
func TestFederatedRecordAuditedOnBothMachines(t *testing.T) {
	const token = "federation-token"
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	pd := &peerRecordDriver{Driver: stub.Driver{DeadlineMs: 2000}, page: ownersPage()}
	base := peerServiceWith(t, "peerbox", pd, service.Config{Token: token})
	srv := entryService(t, token, New("peerbox", base))
	if status, body := readRecordVia(t, srv, token, ""); status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	var lines []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "audit: content-read") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d content-read lines, want one per machine:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if !strings.Contains(l, "entries=2") || !strings.Contains(l, "outcome=performed") {
			t.Errorf("line = %s", l)
		}
	}
	for _, secret := range []string{"asked on the peer", "said on the peer", "peer-cursor", "model-x"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the audit trail carries %q", secret)
		}
	}
}
