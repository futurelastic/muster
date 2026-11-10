package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #450: the profiling endpoint. Off by default, behind its own grant, and
// the grant is checked before the switch.

func debugServer(t *testing.T, profiling bool, ps ...Principal) *httptest.Server {
	t.Helper()
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: ps, Profiling: profiling}))
	t.Cleanup(srv.Close)
	return srv
}

func debugGet(t *testing.T, srv *httptest.Server, token, path string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func errMessage(t *testing.T, raw []byte) fleet.Error {
	t.Helper()
	var env fleet.ErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("not an error envelope: %q (%v)", raw, err)
	}
	return env.Error
}

var (
	holder    = Principal{Name: "operator", Token: "tok-debug", Grants: []Grant{GrantDebug}}
	notHolder = func() Principal {
		var all []Grant
		for _, g := range Grants() {
			if g != GrantDebug {
				all = append(all, g)
			}
		}
		return Principal{Name: "everything-else", Token: "tok-other", Grants: all}
	}()
)

func TestDebug_IsItsOwnGrantNotImpliedByAnythingElse(t *testing.T) {
	// On, so the only thing standing between this caller and a profile is the grant.
	srv := debugServer(t, true, holder, notHolder)
	code, body, _ := debugGet(t, srv, "tok-other", "/v1/debug/pprof/heap")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", code, body)
	}
	if e := errMessage(t, body); e.Kind != fleet.ErrorUnauthorized || !strings.Contains(e.Message, "debug grant") {
		t.Errorf("error = %+v, want unauthorized naming the debug grant", e)
	}
	if code, _, _ := debugGet(t, srv, "tok-other", "/v1/debug/pprof/"); code != http.StatusUnauthorized {
		t.Errorf("the index is behind the grant too: status = %d", code)
	}
}

func TestDebug_GrantIsCheckedBeforeTheSwitch(t *testing.T) {
	// Off. A caller without the grant must learn nothing about the switch.
	srv := debugServer(t, false, holder, notHolder)
	code, body, _ := debugGet(t, srv, "tok-other", "/v1/debug/pprof/heap")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if msg := errMessage(t, body).Message; strings.Contains(msg, "profilingEndpoint") {
		t.Errorf("a caller without the grant was told about the config switch: %q", msg)
	}
}

func TestDebug_OffByDefaultEvenForAHolder(t *testing.T) {
	srv := debugServer(t, false, holder)
	code, body, _ := debugGet(t, srv, "tok-debug", "/v1/debug/pprof/heap")
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", code, body)
	}
	e := errMessage(t, body)
	if e.Kind != fleet.ErrorUnsupported || !strings.Contains(e.Message, "profilingEndpoint") {
		t.Errorf("error = %+v, want unsupported naming profilingEndpoint", e)
	}
	// And the zero Config is that default.
	if (Config{}).Profiling {
		t.Error("Config.Profiling must default to false")
	}
}

func TestDebug_NoPrincipalTableMeansNobodyCanHoldIt(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	// Legacy single-token mode with every coarse flag on and the switch on.
	srv := httptest.NewServer(NewMux(svc, Config{
		Token: testToken, AllowLocalMutations: true, AllowPeerRelay: true, Profiling: true,
	}))
	t.Cleanup(srv.Close)
	code, body, _ := debugGet(t, srv, testToken, "/v1/debug/pprof/heap")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", code, body)
	}
	if msg := errMessage(t, body).Message; !strings.Contains(msg, "no principal table") {
		t.Errorf("message %q does not say why", msg)
	}
}

func TestDebug_AHolderWithTheSwitchOnGetsTheProfiles(t *testing.T) {
	srv := debugServer(t, true, holder)

	code, body, _ := debugGet(t, srv, "tok-debug", "/v1/debug/pprof/")
	if code != http.StatusOK {
		t.Fatalf("index status = %d: %s", code, body)
	}
	var idx struct{ Profiles []struct{ Name string } }
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range idx.Profiles {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "profile,trace,heap,allocs,goroutine,threadcreate" {
		t.Errorf("index lists %s", got)
	}

	// A snapshot profile is a gzip'd pprof protobuf.
	code, body, hdr := debugGet(t, srv, "tok-debug", "/v1/debug/pprof/heap")
	if code != http.StatusOK || len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("heap → %d, first bytes % x; want 200 and a gzip stream", code, body[:min(4, len(body))])
	}
	if hdr.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("content-type = %q", hdr.Get("Content-Type"))
	}

	// The text form names the goroutines running this very test.
	code, body, _ = debugGet(t, srv, "tok-debug", "/v1/debug/pprof/goroutine?debug=2")
	if code != http.StatusOK || !bytes.Contains(body, []byte("goroutine ")) {
		t.Errorf("goroutine?debug=2 → %d %q", code, body[:min(80, len(body))])
	}
}

func TestDebug_ACPUProfileIsTaken(t *testing.T) {
	if testing.Short() {
		t.Skip("takes a real second of sampling")
	}
	srv := debugServer(t, true, holder)
	code, body, _ := debugGet(t, srv, "tok-debug", "/v1/debug/pprof/profile?seconds=1")
	if code != http.StatusOK || len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("profile?seconds=1 → %d, % x", code, body[:min(4, len(body))])
	}
}

func TestDebug_RefusesWhatItDoesNotOfferOrCannotBound(t *testing.T) {
	srv := debugServer(t, true, holder)
	for _, tc := range []struct {
		path string
		code int
		kind fleet.ErrorKind
	}{
		{"/v1/debug/pprof/cmdline", 404, fleet.ErrorNotFound},
		{"/v1/debug/pprof/symbol", 404, fleet.ErrorNotFound},
		{"/v1/debug/pprof/nonsense", 404, fleet.ErrorNotFound},
		{"/v1/debug/pprof/profile?seconds=0", 400, fleet.ErrorInvalid},
		{"/v1/debug/pprof/profile?seconds=abc", 400, fleet.ErrorInvalid},
		{"/v1/debug/pprof/profile?seconds=121", 400, fleet.ErrorInvalid},
		{"/v1/debug/pprof/trace?seconds=-1", 400, fleet.ErrorInvalid},
		{"/v1/debug/pprof/goroutine?debug=9", 400, fleet.ErrorInvalid},
	} {
		code, body, _ := debugGet(t, srv, "tok-debug", tc.path)
		if code != tc.code {
			t.Errorf("%s → %d, want %d (%s)", tc.path, code, tc.code, body)
			continue
		}
		if e := errMessage(t, body); e.Kind != tc.kind {
			t.Errorf("%s → kind %q, want %q", tc.path, e.Kind, tc.kind)
		}
	}
}

func TestDebug_AProfilerAlreadyInUseIsAConflictNotAPlainTextError(t *testing.T) {
	srv := debugServer(t, true, holder)

	// Occupy the process's profiler and tracer ourselves, so there is no race to
	// win: whatever holds them, a request is told so in the envelope.
	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		t.Skipf("the profiler is already in use by whoever runs this test: %v", err)
	}
	cpuHeld := true
	defer func() {
		if cpuHeld {
			pprof.StopCPUProfile()
		}
	}()
	if err := trace.Start(io.Discard); err != nil {
		t.Skipf("the tracer is already in use by whoever runs this test: %v", err)
	}
	defer trace.Stop()

	for _, name := range []string{"profile", "trace"} {
		code, body, _ := debugGet(t, srv, "tok-debug", "/v1/debug/pprof/"+name+"?seconds=1")
		if code != http.StatusConflict {
			t.Fatalf("%s while one is running → %d %s, want 409", name, code, body)
		}
		if e := errMessage(t, body); e.Kind != fleet.ErrorConflict || !e.Retryable {
			t.Errorf("%s: error = %+v, want a retryable conflict", name, e)
		}
	}

	// Released, the same request is served: the refusal was about the moment.
	pprof.StopCPUProfile()
	cpuHeld = false
	if code, body, _ := debugGet(t, srv, "tok-debug", "/v1/debug/pprof/profile?seconds=1"); code != http.StatusOK {
		t.Errorf("after release → %d %s, want 200", code, body)
	}
}

func TestDebug_EveryRequestIsAuditedWithItsDuration(t *testing.T) {
	logs := captureAuditLog(t)
	srv := debugServer(t, true, holder, notHolder)

	debugGet(t, srv, "tok-debug", "/v1/debug/pprof/heap")
	debugGet(t, srv, "tok-other", "/v1/debug/pprof/heap")

	var performed, denied string
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "verb=debug ") {
			continue
		}
		if strings.Contains(line, "outcome=DENIED") {
			denied = line
		} else {
			performed = line
		}
	}
	if !strings.Contains(performed, `actor="operator"`) || !strings.Contains(performed, "outcome=performed status=200 took=") {
		t.Errorf("a served profile must be audited with its duration: %q", performed)
	}
	if !strings.Contains(denied, `actor="everything-else"`) {
		t.Errorf("a refused request must be audited: %q", denied)
	}
}

// The standard net/http/pprof registers on http.DefaultServeMux when imported. This
// service writes its own handlers precisely so that nothing it links can publish
// an unauthenticated profiling surface on the default mux.
func TestDebugRoutesAreNotOnTheDefaultMux(t *testing.T) {
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile", "/debug/pprof/heap"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, pattern := http.DefaultServeMux.Handler(req); pattern != "" {
			t.Errorf("%s is routed on http.DefaultServeMux by pattern %q", path, pattern)
		}
	}
}
