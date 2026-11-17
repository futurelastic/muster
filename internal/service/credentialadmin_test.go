package service

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #498: the credentials routes. These tests hold the gate, the grant and the
// route table with a driver that only records what it was asked; the driver's own
// rules and the whole flow against a real session are tested beside the driver.

const adminSecret = "sk-NEVER-APPEARS-IN-ANY-RESPONSE-OR-LOG-9f3a"

type managerDriver struct {
	stub.Driver
	mu    sync.Mutex
	calls []string
	err   error // returned by every call when set
}

func (m *managerDriver) note(s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, s)
	return m.err
}

func (m *managerDriver) called() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func (m *managerDriver) LinkKey(provider string, value []byte) (fleet.RuntimeCredential, error) {
	if err := m.note("link " + provider + " " + string(value)); err != nil {
		return fleet.RuntimeCredential{}, err
	}
	return fleet.RuntimeCredential{Provider: provider, Kind: fleet.CredentialAPIKey, Source: fleet.SourceLinked, State: fleet.CredentialUsable}, nil
}
func (m *managerDriver) UnlinkKey(provider string) (fleet.CredentialRemoval, error) {
	if err := m.note("unlink " + provider); err != nil {
		return fleet.CredentialRemoval{}, err
	}
	return fleet.CredentialRemoval{Provider: provider, Kind: fleet.CredentialAPIKey, Removed: true}, nil
}
func (m *managerDriver) StartLogin(provider string) (fleet.CredentialLogin, error) {
	if err := m.note("login-start " + provider); err != nil {
		return fleet.CredentialLogin{}, err
	}
	return fleet.CredentialLogin{ID: "a1", State: fleet.LoginPending, URL: "https://example.invalid/device", Code: "ABCD-1234"}, nil
}
func (m *managerDriver) LoginStatus(provider, id string) (fleet.CredentialLogin, error) {
	if err := m.note("login-status " + provider + " " + id); err != nil {
		return fleet.CredentialLogin{}, err
	}
	return fleet.CredentialLogin{ID: id, State: fleet.LoginApproved}, nil
}
func (m *managerDriver) SignOut(provider string) (fleet.CredentialRemoval, error) {
	if err := m.note("sign-out " + provider); err != nil {
		return fleet.CredentialRemoval{}, err
	}
	return fleet.CredentialRemoval{Provider: provider, Kind: fleet.CredentialSubscription, Removed: true}, nil
}

var _ driver.CredentialManager = (*managerDriver)(nil)

var (
	credHolder         = Principal{Name: "operator", Token: "tok-cred", Grants: []Grant{GrantCredentials}}
	credReadOnly       = Principal{Name: "watcher", Token: "tok-read", Grants: []Grant{GrantRead}}
	credEverythingElse = func() Principal {
		var all []Grant
		for _, g := range Grants() {
			if g != GrantCredentials {
				all = append(all, g)
			}
		}
		return Principal{Name: "everything-else", Token: "tok-other", Grants: all}
	}()
)

func credServer(t *testing.T, ps ...Principal) (*httptest.Server, *managerDriver) {
	t.Helper()
	svc := New("testbox")
	md := &managerDriver{Driver: stub.Driver{DeadlineMs: 1000}}
	if err := svc.RegisterLocalDriver("rt", md); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("plain", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: ps}))
	t.Cleanup(srv.Close)
	return srv, md
}

func credDo(t *testing.T, srv *httptest.Server, token, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// routes is every credentials route, with a body where one is read.
var credRoutes = []struct{ method, path, body string }{
	{"PUT", "/v1/runtimes/rt/credentials/openai/api-key", `{"value":"` + adminSecret + `"}`},
	{"DELETE", "/v1/runtimes/rt/credentials/openai/api-key", ""},
	{"POST", "/v1/runtimes/rt/credentials/openai/subscription/login", ""},
	{"GET", "/v1/runtimes/rt/credentials/openai/subscription/login/a1", ""},
	{"DELETE", "/v1/runtimes/rt/credentials/openai/subscription", ""},
}

// captureCredLog collects the service's log until the test ends and returns a reader
// of it that is safe against the handler goroutines still writing.
func captureCredLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	old := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(old) })
	return func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestCredentials_EveryRouteIsBehindItsOwnGrant(t *testing.T) {
	// Every other grant held, none of them is enough.
	srv, md := credServer(t, credHolder, credReadOnly, credEverythingElse)
	for _, who := range []string{"tok-read", "tok-other"} {
		for _, rt := range credRoutes {
			code, body := credDo(t, srv, who, rt.method, rt.path, rt.body)
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s as %s: status %d, want 401: %s", rt.method, rt.path, who, code, body)
				continue
			}
			if e := errMessage(t, body); e.Kind != fleet.ErrorUnauthorized || !strings.Contains(e.Message, "credentials grant") {
				t.Errorf("%s %s as %s: error = %+v", rt.method, rt.path, who, e)
			}
		}
	}
	if got := md.called(); len(got) != 0 {
		t.Fatalf("a caller without the grant reached the driver: %v", got)
	}
}

func TestCredentials_ANoPrincipalTableMachineRefusesEveryRoute(t *testing.T) {
	// The legacy single-token model: the all-verbs flag lets a token do every other
	// mutation, and must not do this one.
	svc := New("testbox")
	md := &managerDriver{Driver: stub.Driver{DeadlineMs: 1000}}
	if err := svc.RegisterLocalDriver("rt", md); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: "legacy", AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	for _, rt := range credRoutes {
		if code, body := credDo(t, srv, "legacy", rt.method, rt.path, rt.body); code != http.StatusUnauthorized {
			t.Errorf("%s %s: status %d, want 401: %s", rt.method, rt.path, code, body)
		}
	}
	if len(md.called()) != 0 {
		t.Fatalf("the driver was reached: %v", md.called())
	}
}

func TestCredentials_AHolderReachesTheDriverOnEveryRoute(t *testing.T) {
	srv, md := credServer(t, credHolder)
	want := []string{
		"link openai " + adminSecret, "unlink openai", "login-start openai", "login-status openai a1", "sign-out openai",
	}
	for i, rt := range credRoutes {
		code, body := credDo(t, srv, "tok-cred", rt.method, rt.path, rt.body)
		if code != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", rt.method, rt.path, code, body)
		}
		if got := md.called(); len(got) != i+1 || got[i] != want[i] {
			t.Fatalf("after %s %s the driver saw %v, want …%q", rt.method, rt.path, got, want[i])
		}
	}
}

func TestCredentials_GrantForVerbCallsAnUnlinkACredentialsActNotASessionClose(t *testing.T) {
	for _, rt := range credRoutes {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		if g := grantForVerb(req); g != GrantCredentials {
			t.Errorf("%s %s needs %q, want credentials", rt.method, rt.path, g)
		}
	}
	// The sessions routes are not caught by the credentials prefix.
	if g := grantForVerb(httptest.NewRequest("DELETE", "/v1/machines/m/sessions/s1", nil)); g != GrantClose {
		t.Errorf("a session close needs %q", g)
	}
	if g := grantForVerb(httptest.NewRequest("GET", "/v1/runtimes", nil)); g != GrantRead {
		t.Errorf("the runtimes listing needs %q", g)
	}
	if !ValidGrant(GrantCredentials) {
		t.Error("credentials is not a valid grant, so a config naming it would refuse to start")
	}
}

func TestCredentials_NeitherAnAnswerNorTheLogCarriesTheKey(t *testing.T) {
	logs := captureCredLog(t)
	srv, _ := credServer(t, credHolder)
	var all [][]byte
	for _, rt := range credRoutes {
		_, body := credDo(t, srv, "tok-cred", rt.method, rt.path, rt.body)
		all = append(all, body)
	}
	// Refusals that handle the body: unreadable JSON, an unknown field, a body past the cap.
	for _, bad := range []string{
		`{"value":"` + adminSecret + `"`,                                           // truncated
		`{"value":"` + adminSecret + `","extra":1}`,                                // unknown field
		`{"value":"` + adminSecret + `"} {"value":"` + adminSecret + `"}`,          // two documents
		`{"value":"` + strings.Repeat("k", maxCredentialBody) + adminSecret + `"}`, // too large
		`{"value": 12}`,
	} {
		code, body := credDo(t, srv, "tok-cred", "PUT", "/v1/runtimes/rt/credentials/openai/api-key", bad)
		if code != http.StatusBadRequest {
			t.Errorf("a malformed body answered %d: %s", code, body)
		}
		all = append(all, body)
	}
	for _, body := range all {
		if strings.Contains(string(body), adminSecret) {
			t.Fatalf("a response carries the key: %s", body)
		}
	}
	if logged := logs(); strings.Contains(logged, adminSecret) {
		t.Fatalf("the log carries the key:\n%s", logged)
	} else if !strings.Contains(logged, "verb=credentials") {
		t.Fatalf("no audit line was written:\n%s", logged)
	}
}

func TestCredentials_TheAuditLineNamesVerbProviderAndKind(t *testing.T) {
	logs := captureCredLog(t)
	srv, _ := credServer(t, credHolder, credReadOnly)
	credDo(t, srv, "tok-cred", "PUT", "/v1/runtimes/rt/credentials/openai/api-key", `{"value":"k"}`)
	credDo(t, srv, "tok-cred", "POST", "/v1/runtimes/rt/credentials/openai/subscription/login", "")
	credDo(t, srv, "tok-read", "DELETE", "/v1/runtimes/rt/credentials/openai/api-key", "")
	out := logs()
	for _, want := range []string{
		`actor="operator" verb=credentials`, "provider=openai kind=api-key action=link outcome=performed status=200",
		"kind=subscription action=login-start outcome=performed",
		`actor="watcher" verb=credentials`, "action=unlink outcome=DENIED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the audit trail lacks %q:\n%s", want, out)
		}
	}
}

func TestCredentials_TheRuntimeIsResolvedHereAndTheDriverMustTakeCredentials(t *testing.T) {
	srv, _ := credServer(t, credHolder)
	code, body := credDo(t, srv, "tok-cred", "DELETE", "/v1/runtimes/nowhere/credentials/openai/api-key", "")
	if code != http.StatusNotFound || errMessage(t, body).Kind != fleet.ErrorNotFound {
		t.Errorf("an unknown runtime: %d %s", code, body)
	}
	code, body = credDo(t, srv, "tok-cred", "DELETE", "/v1/runtimes/plain/credentials/openai/api-key", "")
	if code != http.StatusNotImplemented || errMessage(t, body).Kind != fleet.ErrorUnsupported {
		t.Errorf("a runtime that cannot take credentials: %d %s", code, body)
	}
	// A hostile path segment is cut to plain id characters in the message.
	code, body = credDo(t, srv, "tok-cred", "DELETE", "/v1/runtimes/%3Cscript%3E/credentials/openai/api-key", "")
	if code != http.StatusNotFound || strings.Contains(string(body), "<script>") {
		t.Errorf("a hostile runtime id: %d %s", code, body)
	}
}

func TestCredentials_ADriversRefusalKeepsItsKindAndAStrangeErrorIsNotRelayed(t *testing.T) {
	srv, md := credServer(t, credHolder)
	md.err = &fleet.Error{Kind: fleet.ErrorConflict, Message: "declared by the operator"}
	code, body := credDo(t, srv, "tok-cred", "PUT", "/v1/runtimes/rt/credentials/openai/api-key", `{"value":"k"}`)
	if code != http.StatusConflict || errMessage(t, body).Message != "declared by the operator" {
		t.Errorf("conflict: %d %s", code, body)
	}
	md.err = io.ErrUnexpectedEOF // a plain error: its text is the driver's own
	code, body = credDo(t, srv, "tok-cred", "PUT", "/v1/runtimes/rt/credentials/openai/api-key", `{"value":"k"}`)
	if code != http.StatusGatewayTimeout || strings.Contains(string(body), "EOF") {
		t.Errorf("a plain error must not be relayed: %d %s", code, body)
	}
}

func TestCredentials_TheViewIsStillRead(t *testing.T) {
	// Seeing which credentials exist stays `read`; only changing them is the new grant.
	srv, _ := credServer(t, credHolder, credReadOnly)
	if code, body := credDo(t, srv, "tok-read", "GET", "/v1/runtimes", ""); code != http.StatusOK {
		t.Fatalf("a reader lost the runtimes listing: %d %s", code, body)
	}
	var env struct {
		Items []fleet.RuntimeInfo `json:"items"`
	}
	_, body := credDo(t, srv, "tok-read", "GET", "/v1/runtimes", "")
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}
