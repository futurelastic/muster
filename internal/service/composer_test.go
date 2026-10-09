package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// composerDriver is stub.Driver that can read a composer (muster #276).
type composerDriver struct {
	stub.Driver
	read fleet.ComposerRead
	err  error
	reqs []fleet.Request
}

func (d *composerDriver) Composer(_ context.Context, req fleet.Request, _ fleet.SessionRef) (fleet.ComposerRead, error) {
	d.reqs = append(d.reqs, req)
	return d.read, d.err
}

func getComposer(t *testing.T, srv *httptest.Server, token, query string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/owner/sessions/s1/composer"+query, nil)
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

func composerServer(t *testing.T, d driver.Driver, cfg Config) *httptest.Server {
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

func TestComposer_ServesTheTextAndTheDigestAndAuditsWithoutTheText(t *testing.T) {
	d := &composerDriver{Driver: stub.Driver{DeadlineMs: 500}, read: fleet.ComposerRead{Text: "half a sentence", ComposerDigest: "dg-1"}}
	logs := captureLog(t)
	resp, body := getComposer(t, composerServer(t, d, Config{}), testToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var got fleet.ComposerRead
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got != d.read {
		t.Fatalf("got %+v", got)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	line := logs.String()
	if !strings.Contains(line, "verb=read-composer") || !strings.Contains(line, "chars=15") || strings.Contains(line, "half a sentence") {
		t.Errorf("audit line:\n%s", line)
	}
}

func TestComposer_NeedsTheReadGrantAndNoOther(t *testing.T) {
	d := &composerDriver{Driver: stub.Driver{DeadlineMs: 500}, read: fleet.ComposerRead{Text: "x", ComposerDigest: "d"}}
	srv := composerServer(t, d, Config{Principals: []Principal{
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
		{Name: "operator", Token: "tok-op", Grants: []Grant{GrantSend}},
	}})
	logs := captureLog(t)
	if resp, body := getComposer(t, srv, "tok-watch", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("read holder: status = %d: %s", resp.StatusCode, body)
	}
	if resp, _ := getComposer(t, srv, "tok-op", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a principal without read: status = %d, want 401", resp.StatusCode)
	}
	if len(d.reqs) != 1 {
		t.Errorf("driver reached %d times, want 1 (the refused caller must not reach it)", len(d.reqs))
	}
	if !strings.Contains(logs.String(), "outcome=DENIED") {
		t.Errorf("a refusal left no audit line:\n%s", logs.String())
	}
}

func TestComposer_UnsupportedAndRefusalsKeepTheirKinds(t *testing.T) {
	// A driver without the capability.
	if resp, _ := getComposer(t, composerServer(t, &stub.Driver{DeadlineMs: 500}, Config{}), testToken, ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("unsupported runtime: status = %d, want 501", resp.StatusCode)
	}

	// A malformed startedAt is refused, not dropped.
	d := &composerDriver{Driver: stub.Driver{DeadlineMs: 500}}
	resp, _ := getComposer(t, composerServer(t, d, Config{}), testToken, "?startedAt=yesterday")
	if resp.StatusCode != http.StatusBadRequest || len(d.reqs) != 0 {
		t.Errorf("bad startedAt: status = %d, driver calls = %d", resp.StatusCode, len(d.reqs))
	}
}
