package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/service"
)

// A peer's media reader, as the machine that owns the session would have it.
type peerMediaDriver struct {
	stub.Driver
	ref   string
	req   fleet.Request
	media fleet.RecordMedia
	err   error
}

func (d *peerMediaDriver) RecordMedia(_ context.Context, req fleet.Request, _ fleet.SessionRef, ref string) (fleet.RecordMedia, error) {
	d.ref, d.req = ref, req
	return d.media, d.err
}

func peerMediaService(t *testing.T, d *peerMediaDriver, cfg service.Config) string {
	t.Helper()
	svc := service.New("peerbox")
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatalf("registering driver on the peer: %v", err)
	}
	srv := httptest.NewServer(service.NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return srv.URL
}

func readMediaVia(t *testing.T, srv *httptest.Server, token, ref, query string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/peerbox/sessions/s1/record/media/"+ref+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp, b.Bytes()
}

var peerPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 48)...)

// The bytes a peer's session holds are what the entry machine's caller gets, with
// the reference and the caller's corroboration arriving at the owner; and a
// relayed read is audited on both machines without the reference.
func TestFederatedRecordMediaRelaysTheBytes(t *testing.T) {
	const token = "federation-token"
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	pd := &peerMediaDriver{Driver: stub.Driver{DeadlineMs: 2000}, media: fleet.RecordMedia{ContentType: "image/png", Data: peerPNG}}
	srv := entryService(t, token, New("peerbox", peerMediaService(t, pd, service.Config{Token: token}), WithDeadline(2*time.Second)))

	resp, body := readMediaVia(t, srv, token, "PEER-REF-MARKER", "?startedAt=2026-10-04T09:00:00Z")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, peerPNG) {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(body))
	}
	if resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", resp.Header)
	}
	if pd.ref != "PEER-REF-MARKER" {
		t.Errorf("the owner saw reference %q", pd.ref)
	}
	if want := pd.req.Expect.StartedAt; want == nil || want.UTC().Format(time.RFC3339) != "2026-10-04T09:00:00Z" {
		t.Errorf("startedAt did not reach the machine that owns the session: %v", want)
	}

	n := 0
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "audit: content-read") {
			n++
			if !strings.Contains(l, "entries=1") || !strings.Contains(l, "outcome=performed") {
				t.Errorf("line = %s", l)
			}
		}
	}
	if n != 2 {
		t.Errorf("%d content-read lines, want one per machine:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "PEER-REF-MARKER") {
		t.Error("the audit trail carries the reference")
	}
}

func TestFederatedRecordMediaKindsSurviveTheHop(t *testing.T) {
	const token = "federation-token"
	for name, tc := range map[string]struct {
		err       error
		kind      fleet.ErrorKind
		retryable bool
	}{
		"expired or unknown reference": {fleet.ErrNoSuchMedia, fleet.ErrorNotFound, false},
		"stale corroboration":          {fleet.ErrAmbiguousTarget, fleet.ErrorConflict, false},
		"no such session":              {fleet.ErrNoSuchSession, fleet.ErrorNotFound, false},
		"no record yet":                {fleet.ErrNoTurnRecord, fleet.ErrorNotFound, true},
	} {
		t.Run(name, func(t *testing.T) {
			pd := &peerMediaDriver{Driver: stub.Driver{DeadlineMs: 2000}, err: tc.err}
			srv := entryService(t, token, New("peerbox", peerMediaService(t, pd, service.Config{Token: token})))
			resp, body := readMediaVia(t, srv, token, "r", "")
			var env fleet.ErrorEnvelope
			if err := json.Unmarshal(body, &env); err != nil || env.Error.Kind != tc.kind || env.Error.Retryable != tc.retryable {
				t.Fatalf("status %d, kind = %q retryable = %v (%v), want %q %v: %s", resp.StatusCode, env.Error.Kind, env.Error.Retryable, err, tc.kind, tc.retryable, body)
			}
		})
	}
}

func TestFederatedRecordMediaAbsentOnThePeerIsUnsupported(t *testing.T) {
	const token = "federation-token"
	older := httptest.NewServer(http.NotFoundHandler()) // a build that predates the route
	t.Cleanup(older.Close)
	srv := entryService(t, token, New("peerbox", older.URL))
	_, body := readMediaVia(t, srv, token, "r", "")
	if !strings.Contains(string(body), `"unsupported"`) {
		t.Errorf("older peer: %s", body)
	}
	srv = entryService(t, token, New("peerbox", peerService(t, "peerbox", "stub", &stub.Driver{DeadlineMs: 2000}, token, false)))
	_, body = readMediaVia(t, srv, token, "r", "")
	if !strings.Contains(string(body), `"unsupported"`) {
		t.Errorf("runtime without images: %s", body)
	}
}

// What a peer sends is checked on the way through: a type this API does not name,
// no bytes, or more than the cap is never handed on.
func TestFederatedRecordMediaRefusesWhatIsNotAnImage(t *testing.T) {
	const token = "federation-token"
	for name, tc := range map[string]struct {
		contentType string
		body        []byte
	}{
		"html":      {"text/html", []byte("<script>alert(1)</script>")},
		"svg":       {"image/svg+xml", []byte("<svg/>")},
		"no type":   {"", peerPNG},
		"empty":     {"image/png", nil},
		"too large": {"image/png", make([]byte, fleet.MaxRecordMediaBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				} else {
					w.Header()["Content-Type"] = nil // no type at all, and no sniffing by the server
				}
				_, _ = w.Write(tc.body)
			}))
			t.Cleanup(peer.Close)
			srv := entryService(t, token, New("peerbox", peer.URL))
			resp, body := readMediaVia(t, srv, token, "r", "")
			if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "<script>") || len(body) > 600 {
				t.Errorf("status %d, %d bytes: a peer's non-image answer was passed on", resp.StatusCode, len(body))
			}
		})
	}
}
