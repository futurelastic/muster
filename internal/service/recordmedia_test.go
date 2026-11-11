package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// mediaDriver is stub.Driver that can read an image of a record and remembers
// what it was asked.
type mediaDriver struct {
	stub.Driver
	mu    sync.Mutex
	refs  []string
	reqs  []fleet.Request
	media fleet.RecordMedia
	err   error
}

func (d *mediaDriver) RecordMedia(_ context.Context, req fleet.Request, _ fleet.SessionRef, ref string) (fleet.RecordMedia, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refs = append(d.refs, ref)
	d.reqs = append(d.reqs, req)
	return d.media, d.err
}

func (d *mediaDriver) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.refs)
}

func mediaServer(t *testing.T, d any, cfg Config) *httptest.Server {
	t.Helper()
	svc := New("owner")
	switch v := d.(type) {
	case *mediaDriver:
		if err := svc.RegisterLocalDriver("stub", v); err != nil {
			t.Fatal(err)
		}
	case *recordDriver:
		if err := svc.RegisterLocalDriver("stub", v); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unexpected driver %T", d)
	}
	if cfg.Token == "" && len(cfg.Principals) == 0 {
		cfg.Token = testToken
	}
	srv := httptest.NewServer(NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return srv
}

const mediaRefSecret = "REF-SECRET-MARKER"

func getMedia(t *testing.T, srv *httptest.Server, token, ref, query string) (*http.Response, []byte) {
	t.Helper()
	return getPath(t, srv, token, "/v1/machines/owner/sessions/s1/record/media/"+ref+query)
}

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func TestRecordMedia_ServesTheBytesWithTheirHeadersAndForwardsTheQuery(t *testing.T) {
	d := &mediaDriver{Driver: stub.Driver{DeadlineMs: 500}, media: fleet.RecordMedia{ContentType: "image/png", Data: pngBytes}}
	srv := mediaServer(t, d, Config{})

	started := "2026-10-04T09:00:00Z"
	resp, body := getMedia(t, srv, testToken, mediaRefSecret, "?startedAt="+started)
	if resp.StatusCode != http.StatusOK || string(body) != string(pngBytes) {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(body))
	}
	for k, want := range map[string]string{
		"Content-Type": "image/png", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
		"Content-Length": fmt.Sprint(len(pngBytes)),
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if len(d.refs) != 1 || d.refs[0] != mediaRefSecret {
		t.Errorf("driver saw %v", d.refs)
	}
	if want := d.reqs[0].Expect.StartedAt; want == nil || want.UTC().Format("2006-01-02T15:04:05Z") != started {
		t.Errorf("startedAt did not reach the driver: %v", want)
	}
	if resp, _ := getMedia(t, srv, testToken, "x", "?startedAt=yesterday"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a startedAt that does not parse: status %d, want 400", resp.StatusCode)
	}
}

// The media route needs the record grant and nothing else, and nothing else stands
// in for it.
func TestRecordMedia_RequiresTheRecordGrantAndOnlyThat(t *testing.T) {
	var everythingButRecord []Grant
	for _, g := range Grants() {
		if g != GrantRecord {
			everythingButRecord = append(everythingButRecord, g)
		}
	}
	d := &mediaDriver{Driver: stub.Driver{DeadlineMs: 500}, media: fleet.RecordMedia{ContentType: "image/png", Data: pngBytes}}
	srv := mediaServer(t, d, Config{Principals: []Principal{
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
		{Name: "operator", Token: "tok-send", Grants: []Grant{GrantSend}},
		{Name: "everything", Token: "tok-all", Grants: everythingButRecord},
		{Name: "reader", Token: "tok-record", Grants: []Grant{GrantRecord}},
		{Name: "nobody", Token: "tok-none"},
	}})
	logs := captureLog(t)

	for _, tok := range []string{"tok-watch", "tok-send", "tok-all", "tok-none", "wrong"} {
		if resp, _ := getMedia(t, srv, tok, "r", ""); resp.StatusCode != http.StatusUnauthorized {
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
	// A holder of record alone, with neither read nor send, is served.
	if resp, body := getMedia(t, srv, "tok-record", "r", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("record holder: status = %d: %s", resp.StatusCode, body)
	}
}

func TestRecordMedia_SwitchRefusesEveryCallAfterTheGrant(t *testing.T) {
	d := &mediaDriver{Driver: stub.Driver{DeadlineMs: 500}, media: fleet.RecordMedia{ContentType: "image/png", Data: pngBytes}}
	srv := mediaServer(t, d, Config{RefuseRecord: true, Principals: []Principal{
		{Name: "reader", Token: "tok-record", Grants: []Grant{GrantRecord}},
		{Name: "operator", Token: "tok-send", Grants: []Grant{GrantSend}},
	}})
	logs := captureLog(t)

	resp, body := getMedia(t, srv, "tok-send", "r", "")
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), fleet.ReasonRecordOff) {
		t.Errorf("a non-holder: status %d: %s; want a plain 401 that does not mention the switch", resp.StatusCode, body)
	}
	resp, body = getMedia(t, srv, "tok-record", "r", "")
	var env fleet.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusNotImplemented || env.Error.Kind != fleet.ErrorUnsupported || env.Error.Reason != fleet.ReasonRecordOff {
		t.Errorf("a holder: status %d, error %+v; want 501 unsupported %q", resp.StatusCode, env.Error, fleet.ReasonRecordOff)
	}
	if resp, body := getPath(t, srv, "tok-record", "/v1/machines/elsewhere/sessions/s1/record/media/r"); resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), fleet.ReasonRecordOff) {
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

// The trail says an image was read, who read it and how many left, and never the
// reference: a reference is a place in somebody's conversation.
func TestRecordMedia_AuditNeverCarriesTheReference(t *testing.T) {
	d := &mediaDriver{Driver: stub.Driver{DeadlineMs: 500}, media: fleet.RecordMedia{ContentType: "image/png", Data: pngBytes}}
	srv := mediaServer(t, d, Config{Principals: []Principal{
		{Name: "reader", Token: "tok-record", Grants: []Grant{GrantRecord}},
	}})
	logs := captureLog(t)

	if resp, body := getMedia(t, srv, "tok-record", mediaRefSecret, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
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
		`actor="reader"`, "route=GET /v1/machines/{machine}/sessions/{id}/record/media/{ref}", "target=owner/s1",
		"entries=1", "outcome=performed", "status=200", "took=",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("content-read line lacks %q: %s", want, lines[0])
		}
	}
	if strings.Contains(logs.String(), mediaRefSecret) {
		t.Errorf("the audit trail carries the reference:\n%s", logs.String())
	}
}

func TestRecordMedia_NotFoundUnsupportedAndNonImageAnswers(t *testing.T) {
	// no such image: 404, and not retryable
	d := &mediaDriver{Driver: stub.Driver{DeadlineMs: 500}, err: fmt.Errorf("wrapped: %w", fleet.ErrNoSuchMedia)}
	srv := mediaServer(t, d, Config{})
	resp, body := getMedia(t, srv, testToken, "gone", "")
	var env fleet.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusNotFound || env.Error.Kind != fleet.ErrorNotFound || env.Error.Retryable {
		t.Errorf("status %d, error %+v; want 404 not_found, not retryable", resp.StatusCode, env.Error)
	}

	// a record that cannot be identified yet is the retryable 404 the record route has
	d2 := &mediaDriver{Driver: stub.Driver{DeadlineMs: 500}, err: fleet.ErrNoTurnRecord}
	resp, body = getMedia(t, mediaServer(t, d2, Config{}), testToken, "r", "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `"retryable":true`) {
		t.Errorf("no record yet: status %d: %s", resp.StatusCode, body)
	}

	// a runtime that carries no images
	plain := &recordDriver{Driver: stub.Driver{DeadlineMs: 500}}
	resp, body = getMedia(t, mediaServer(t, plain, Config{}), testToken, "r", "")
	if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "unsupported") {
		t.Errorf("a runtime without images: status %d: %s", resp.StatusCode, body)
	}

	// an answer that is not an image this API serves is never passed on
	for name, m := range map[string]fleet.RecordMedia{
		"html":      {ContentType: "text/html", Data: []byte("<script>1</script>")},
		"svg":       {ContentType: "image/svg+xml", Data: []byte("<svg/>")},
		"empty":     {ContentType: "image/png"},
		"no type":   {Data: pngBytes},
		"too large": {ContentType: "image/png", Data: make([]byte, fleet.MaxRecordMediaBytes+1)},
	} {
		d := &mediaDriver{Driver: stub.Driver{DeadlineMs: 500}, media: m}
		resp, body := getMedia(t, mediaServer(t, d, Config{}), testToken, "r", "")
		if resp.StatusCode != http.StatusBadRequest || len(body) > 400 {
			t.Errorf("%s: status %d, %d bytes; want a refusal", name, resp.StatusCode, len(body))
		}
	}
}

func TestIsRecordMediaPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/machines/m/sessions/s1/record/media/REF":     true,
		"/v1/machines/m/sessions/s1/record/media/":        false,
		"/v1/machines/m/sessions/s1/record/media":         false,
		"/v1/machines/m/sessions/s1/record":               false,
		"/v1/machines/m/sessions/s1/record/other/REF":     false,
		"/v1/machines/m/sessions/s1/record/media/REF/x":   false,
		"/v1/machines/m/sessions/record/media/record":     false,
		"/v1/machines/m/sessions/media/record/media/REF":  true,
		"/v1/machines/m/sessions/record/record/media/REF": true,
	} {
		if got := isRecordMediaPath(path); got != want {
			t.Errorf("isRecordMediaPath(%q) = %v, want %v", path, got, want)
		}
	}
}
