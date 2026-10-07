package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// rcDriver is a driver that can toggle remote control, so the route can be
// exercised without a multiplexer. It records what it was asked for.
type rcDriver struct {
	stub.Driver
	support *fleet.RemoteControlSupport
	calls   []bool
	err     error
}

func (d *rcDriver) Capabilities() fleet.DriverCapabilities {
	c := d.Driver.Capabilities()
	c.RemoteControl = d.support
	return c
}

func (d *rcDriver) SetRemoteControl(ctx context.Context, req fleet.Request, ref fleet.SessionRef, enabled bool) (fleet.Ack, error) {
	d.calls = append(d.calls, enabled)
	if d.err != nil {
		return fleet.Ack{}, d.err
	}
	return fleet.Ack{Accepted: true}, nil
}

var _ driver.RemoteControlSetter = (*rcDriver)(nil)

func rcRequest(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/testbox/sessions/s1/remote-control", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func rcServer(t *testing.T, d driver.Driver, cfg Config) *httptest.Server {
	t.Helper()
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return srv
}

func TestRemoteControl_ReturnsAcceptedAndForwardsEnabled(t *testing.T) {
	d := &rcDriver{Driver: stub.Driver{DeadlineMs: 1000}, support: &fleet.RemoteControlSupport{Toggle: true, Off: true}}
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})

	for _, enabled := range []string{"true", "false"} {
		code, body := rcRequest(t, srv, testToken, `{"enabled":`+enabled+`}`)
		if code != http.StatusAccepted || !strings.Contains(body, `"accepted":true`) {
			t.Fatalf("enabled=%s → %d %s, want 202 accepted", enabled, code, body)
		}
	}
	if len(d.calls) != 2 || !d.calls[0] || d.calls[1] {
		t.Fatalf("driver saw %v, want [true false] (false must reach the driver, not be dropped)", d.calls)
	}
}

func TestRemoteControl_MissingEnabledIsInvalid(t *testing.T) {
	d := &rcDriver{Driver: stub.Driver{DeadlineMs: 1000}, support: &fleet.RemoteControlSupport{Toggle: true, Off: true}}
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})
	for _, body := range []string{`{}`, `{"enabled":null}`, `not json`} {
		if code, raw := rcRequest(t, srv, testToken, body); code != http.StatusBadRequest {
			t.Errorf("%s → %d %s, want 400", body, code, raw)
		}
	}
	if len(d.calls) != 0 {
		t.Fatalf("an invalid body reached the driver: %v", d.calls)
	}
}

// A driver without the interface, and a driver that can turn it on but declares
// no way to turn it off, are both unsupported — never emulated.
func TestRemoteControl_UnsupportedIsReportedNotEmulated(t *testing.T) {
	plain := rcServer(t, &stub.Driver{DeadlineMs: 1000}, Config{Token: testToken, AllowLocalMutations: true})
	if code, raw := rcRequest(t, plain, testToken, `{"enabled":true}`); code != http.StatusNotImplemented {
		t.Fatalf("driver without the interface → %d %s, want 501 unsupported", code, raw)
	}

	d := &rcDriver{Driver: stub.Driver{DeadlineMs: 1000}, support: &fleet.RemoteControlSupport{Toggle: true, Off: false}}
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})
	if code, raw := rcRequest(t, srv, testToken, `{"enabled":false}`); code != http.StatusNotImplemented {
		t.Fatalf("off against off:false → %d %s, want 501 unsupported", code, raw)
	}
	if len(d.calls) != 0 {
		t.Fatalf("a refused off reached the driver: %v", d.calls)
	}
	if code, raw := rcRequest(t, srv, testToken, `{"enabled":true}`); code != http.StatusAccepted {
		t.Fatalf("on against toggle:true → %d %s, want 202", code, raw)
	}
}

// The grant is its own: send, keys and every other verb do not imply it, and
// the refusal names it.
func TestRemoteControl_IsItsOwnGrantNotImpliedByAnythingElse(t *testing.T) {
	d := &rcDriver{Driver: stub.Driver{DeadlineMs: 1000}, support: &fleet.RemoteControlSupport{Toggle: true, Off: true}}
	var allButRC []Grant
	for _, g := range Grants() {
		if g != GrantRemoteControl {
			allButRC = append(allButRC, g)
		}
	}
	srv := rcServer(t, d, Config{Principals: []Principal{
		{Name: "everything-else", Token: "tok-other", Grants: allButRC},
		{Name: "publisher", Token: "tok-rc", Grants: []Grant{GrantRead, GrantRemoteControl}},
	}})

	code, raw := rcRequest(t, srv, "tok-other", `{"enabled":true}`)
	if code != http.StatusUnauthorized || !strings.Contains(raw, "remote-control grant") {
		t.Fatalf("without the grant → %d %s, want 401 naming the remote-control grant", code, raw)
	}
	if len(d.calls) != 0 {
		t.Fatalf("a refused call reached the driver: %v", d.calls)
	}
	if code, raw := rcRequest(t, srv, "tok-rc", `{"enabled":true}`); code != http.StatusAccepted {
		t.Fatalf("with the grant → %d %s, want 202", code, raw)
	}
}

// A driver's refusal keeps its kind on the wire (a retryable conflict stays a
// conflict), so a client can tell "try again" from "this is wrong".
func TestRemoteControl_AdoptsTheDriversTypedRefusal(t *testing.T) {
	d := &rcDriver{
		Driver:  stub.Driver{DeadlineMs: 1000},
		support: &fleet.RemoteControlSupport{Toggle: true, Off: true},
		err:     &fleet.Error{Kind: fleet.ErrorConflict, Message: "session is busy", Retryable: true},
	}
	srv := rcServer(t, d, Config{Token: testToken, AllowLocalMutations: true})
	code, raw := rcRequest(t, srv, testToken, `{"enabled":true}`)
	if code != http.StatusConflict || !strings.Contains(raw, "session is busy") {
		t.Fatalf("→ %d %s, want 409 with the driver's message", code, raw)
	}
}

func TestRemoteControl_LegacyModeReportsTheGrantOnlyUnderLocalMutations(t *testing.T) {
	for _, allow := range []bool{false, true} {
		srv := rcServer(t, &stub.Driver{DeadlineMs: 1000}, Config{Token: testToken, AllowLocalMutations: allow})
		_, report := whoami(t, srv, testToken, "")
		if got := hasGrant(report.Grants, GrantRemoteControl); got != allow {
			t.Errorf("AllowLocalMutations=%v: whoami lists remote-control = %v", allow, got)
		}
	}
}
