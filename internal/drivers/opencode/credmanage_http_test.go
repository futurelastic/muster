package opencode

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/service"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #498, end to end through the layers this repo owns: a real service with a
// principal table, the real driver and its stand-in runtime as a real child process.
// The issue's proof: with `credentials` granted, link an API key, see it usable in
// the view, start a session that uses it, unlink, see it absent; without the grant
// every route is refused; and neither a response nor the log ever holds the value.
func TestCredentialsOverHTTP_LinkSeeUseUnlinkAndNeverLeakTheValue(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	const machine = "test-machine"

	var logBuf bytes.Buffer
	var logMu sync.Mutex
	oldW := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logBuf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(oldW) })

	d, _ := newIsolatedDriver(t, WithLinkedEnv(sessionenv.NewLinked(t.TempDir())), WithLogins(&fakeLogins{}))
	svc := service.New(machine)
	if err := svc.RegisterLocalDriver(DefaultRuntime, d); err != nil {
		t.Fatal(err)
	}
	ops := service.Principal{Name: "ops", Token: "tok-ops", Grants: []service.Grant{
		service.GrantRead, service.GrantCreate, service.GrantClose, service.GrantCredentials}}
	plain := service.Principal{Name: "plain", Token: "tok-plain", Grants: []service.Grant{
		service.GrantRead, service.GrantCreate, service.GrantClose, service.GrantSend}}
	srv := httptest.NewServer(service.NewMux(svc, service.Config{Principals: []service.Principal{ops, plain}}))
	t.Cleanup(srv.Close)

	var responses [][]byte
	n := 0
	call := func(token, method, path string, body any) (int, []byte) {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rdr = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if method == http.MethodPost && strings.HasSuffix(path, "/sessions") {
			n++
			req.Header.Set("Idempotency-Key", "k498-"+strconv.Itoa(n))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		responses = append(responses, raw)
		return resp.StatusCode, raw
	}
	credentialsOf := func() []fleet.RuntimeCredential {
		t.Helper()
		code, raw := call("tok-ops", "GET", "/v1/runtimes", nil)
		if code != http.StatusOK {
			t.Fatalf("runtimes: %d %s", code, raw)
		}
		var env struct {
			Items []fleet.RuntimeInfo `json:"items"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		for _, r := range env.Items {
			if r.Runtime == DefaultRuntime && r.Machine == machine {
				return r.Credentials
			}
		}
		return nil
	}
	newSession := func() fleet.Session {
		t.Helper()
		code, raw := call("tok-ops", "POST", "/v1/machines/"+machine+"/sessions", map[string]any{"cwd": t.TempDir()})
		if code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, raw)
		}
		var s fleet.Session
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	keyPath := "/v1/runtimes/" + string(DefaultRuntime) + "/credentials/openai/api-key"

	if got := credentialsOf(); len(got) != 0 {
		t.Fatalf("before: credentials = %+v", got)
	}
	// Link, see it usable.
	if code, raw := call("tok-ops", "PUT", keyPath, map[string]string{"value": secretKey}); code != http.StatusOK {
		t.Fatalf("link: %d %s", code, raw)
	}
	got := credentialsOf()
	if want := row("openai", fleet.CredentialAPIKey, fleet.SourceLinked, fleet.CredentialUsable); len(got) != 1 || got[0] != want {
		t.Fatalf("after link: credentials = %+v, want %+v", got, want)
	}
	// A session started now uses it.
	with := newSession()
	if v := inside(t, d, with.ID).vars()[openaiKeyEnv]; v != secretKey {
		t.Fatalf("a session after the link sees %q", v)
	}
	// Without the grant every route is refused, and the key is not touched.
	for _, rt := range []struct{ method, path string }{
		{"PUT", keyPath}, {"DELETE", keyPath},
		{"POST", "/v1/runtimes/opencode/credentials/openai/subscription/login"},
		{"GET", "/v1/runtimes/opencode/credentials/openai/subscription/login/x"},
		{"DELETE", "/v1/runtimes/opencode/credentials/openai/subscription"},
	} {
		if code, raw := call("tok-plain", rt.method, rt.path, map[string]string{"value": secretKey}); code != http.StatusUnauthorized {
			t.Errorf("%s %s without the grant: %d %s", rt.method, rt.path, code, raw)
		}
	}
	if got := credentialsOf(); len(got) != 1 {
		t.Fatalf("a refused call changed the view: %+v", got)
	}
	// Unlink: absent in the view, absent in the next session.
	code, raw := call("tok-ops", "DELETE", keyPath, nil)
	var rem fleet.CredentialRemoval
	if code != http.StatusOK || json.Unmarshal(raw, &rem) != nil || !rem.Removed {
		t.Fatalf("unlink: %d %s", code, raw)
	}
	if got := credentialsOf(); len(got) != 0 {
		t.Fatalf("after unlink: credentials = %+v", got)
	}
	without := newSession()
	if _, has := inside(t, d, without.ID).vars()[openaiKeyEnv]; has {
		t.Fatal("a session after the unlink still has the key")
	}

	// The value is nowhere a client or an operator can read: not in any response, not in the log.
	for _, raw := range responses {
		if strings.Contains(string(raw), secretKey) {
			t.Fatalf("a response carries the key: %s", raw)
		}
	}
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if strings.Contains(logged, secretKey) {
		t.Fatalf("the log carries the key:\n%s", logged)
	}
	for _, want := range []string{"verb=credentials", `provider=openai kind=api-key action=link outcome=performed`, "action=unlink outcome=performed", "outcome=DENIED"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the audit trail lacks %q", want)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
