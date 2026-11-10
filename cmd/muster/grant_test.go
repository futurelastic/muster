package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/grant"
)

// muster #451: the operator's side of the ChatGPT sign-in — `muster grant` and the
// doctor row. The token endpoint is a server the test owns.

func grantProvider(t *testing.T) *grant.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_auth_id":"d1","user_code":"WXYZ-0001","interval":"1"}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"authorization_code":"c1","code_verifier":"v1"}`))
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ACCESS-1","refresh_token":"REFRESH-1","expires_in":7200}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &grant.Client{Issuer: srv.URL, ClientID: "test", HTTP: srv.Client(),
		Sleep: func(context.Context, time.Duration) error { return nil }}
}

func grantRun(t *testing.T, state string, client *grant.Client, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"grant"}, args...)
	full = append(full, "--state-dir="+state)
	handled, code := runGrantWith(full, func(string) string { return "" }, &out, &errb, client, time.Now)
	if !handled {
		t.Fatal("the grant command was not handled")
	}
	return code, out.String(), errb.String()
}

func TestGrantLoginStoresTheSignInOwnerOnlyAndNeverPrintsATokenOrTheRefreshToken(t *testing.T) {
	state := t.TempDir()
	code, out, errs := grantRun(t, state, grantProvider(t), "login")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if !strings.Contains(out, "WXYZ-0001") || !strings.Contains(out, "/codex/device") {
		t.Fatalf("login did not tell the person where to go and what to type:\n%s", out)
	}
	for _, secret := range []string{"ACCESS-1", "REFRESH-1"} {
		if strings.Contains(out+errs, secret) {
			t.Fatalf("login printed a credential (%s):\n%s%s", secret, out, errs)
		}
	}
	g, err := grant.NewStore(state).Load()
	if err != nil || g.Refresh != "REFRESH-1" || g.Access != "ACCESS-1" || g.Generation != 1 {
		t.Fatalf("stored grant = %+v (%v)", g, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(grant.NewStore(state).Path())
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("store mode %#o is not owner-only", info.Mode().Perm())
		}
	}
	if code, out, _ := grantRun(t, state, nil, "status"); code != 0 || !strings.Contains(out, "signed in") || strings.Contains(out, "REFRESH-1") {
		t.Fatalf("status = %d %q", code, out)
	}
}

const sourceAuth = `{
  "openai": {"type":"oauth","access":"SRC-ACCESS","refresh":"SRC-REFRESH","expires":%d,"accountId":"acct"},
  "other":  {"type":"api","key":"K"}
}`

func writeSource(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGrantImportTakesOnlyTheOpenaiSignInAndTellsTheOperatorToSignTheSourceOut(t *testing.T) {
	state := t.TempDir()
	src := writeSource(t, strings.Replace(sourceAuth, "%d", "4102444800000", 1))
	code, out, errs := grantRun(t, state, nil, "import", src)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	g, err := grant.NewStore(state).Load()
	if err != nil || g.Refresh != "SRC-REFRESH" || g.AccountID != "acct" || g.ExpiresAtMs != 4102444800000 {
		t.Fatalf("stored grant = %+v (%v)", g, err)
	}
	if !strings.Contains(errs, "sign") || !strings.Contains(errs, "out") {
		t.Fatalf("import did not warn that the source copy will stop working:\n%s", errs)
	}
	if strings.Contains(out+errs, "SRC-REFRESH") || strings.Contains(out+errs, "SRC-ACCESS") {
		t.Fatalf("import printed a credential:\n%s%s", out, errs)
	}
	// The other provider's key was not taken.
	raw, _ := os.ReadFile(grant.NewStore(state).Path())
	if strings.Contains(string(raw), `"K"`) {
		t.Fatalf("an unrelated credential was copied into the store: %s", raw)
	}
}

func TestGrantImportRefusesWhatItCannotRenew(t *testing.T) {
	cases := map[string]string{
		"no openai entry":      `{"other":{"type":"api","key":"K"}}`,
		"an api key":           `{"openai":{"type":"api","key":"placeholder"}}`,
		"no refresh token":     `{"openai":{"type":"oauth","access":"a","refresh":"","expires":1}}`,
		"a session's own copy": `{"openai":{"type":"oauth","access":"a","refresh":"` + grant.Placeholder + `","expires":1}}`,
		"not json":             `nope`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			code, _, errs := grantRun(t, state, nil, "import", writeSource(t, body))
			if code == 0 {
				t.Fatalf("accepted %s", name)
			}
			if errs == "" {
				t.Fatal("refused without saying why")
			}
			if _, err := grant.NewStore(state).Load(); err == nil {
				t.Fatalf("a refused import left a grant behind (%s)", name)
			}
		})
	}
}

func TestGrantStatusSaysWhatIsWrongAndExitsNonZeroUnlessOK(t *testing.T) {
	state := t.TempDir()
	if code, out, _ := grantRun(t, state, nil, "status"); code != 1 || !strings.Contains(out, "signed out") {
		t.Fatalf("empty: %d %q", code, out)
	}
	store := grant.NewStore(state)
	now := time.Now()
	if _, err := store.Replace(grant.Grant{Refresh: "R", Access: "A", ExpiresAtMs: now.Add(time.Hour).UnixMilli(), SignedInAtMs: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := grantRun(t, state, nil, "status"); code != 0 || !strings.Contains(out, "signed in") {
		t.Fatalf("ok: %d %q", code, out)
	}
	if _, _, err := store.Update(1, func(g *grant.Grant) { g.Revoked = &grant.Revoked{Status: 401, Code: "token_expired"} }); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := grantRun(t, state, nil, "status"); code != 1 || !strings.Contains(out, "revoked") || !strings.Contains(out, "sign in again") {
		t.Fatalf("revoked: %d %q", code, out)
	}
	if code, _, _ := grantRun(t, state, nil, "logout"); code != 0 {
		t.Fatalf("logout exit %d", code)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("logout left the grant behind")
	}
}

func TestGrantRefusesUnknownArgumentsAndDoesNotTouchOtherCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if handled, _ := runGrantWith([]string{"key", "show"}, func(string) string { return "" }, &out, &errb, nil, time.Now); handled {
		t.Fatal("grant consumed another command")
	}
	if code, _, errs := grantRun(t, t.TempDir(), nil, "login", "--bogus"); code != 2 || !strings.Contains(errs, "unknown argument") {
		t.Fatalf("code %d %q", code, errs)
	}
	if code, _, _ := grantRun(t, t.TempDir(), nil, "import"); code != 2 {
		t.Fatalf("import with no path exited %d, want 2", code)
	}
}

func TestDoctorGrantRow(t *testing.T) {
	now := time.Now()
	signedIn := func(t *testing.T, mut func(*grant.Grant)) string {
		state := t.TempDir()
		store := grant.NewStore(state)
		g := grant.Grant{Refresh: "R", Access: "A", ExpiresAtMs: now.Add(48 * time.Hour).UnixMilli(), SignedInAtMs: now.UnixMilli()}
		if mut != nil {
			mut(&g)
		}
		if _, err := store.Replace(g); err != nil {
			t.Fatal(err)
		}
		return state
	}
	t.Run("opencode not enabled", func(t *testing.T) {
		if row := checkOpencodeGrant(false, t.TempDir(), "", now); row.Status != statusSkip {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("no state directory", func(t *testing.T) {
		if row := checkOpencodeGrant(true, "", "", now); row.Status != statusSkip || !strings.Contains(row.Summary, "FLEET_STATE_DIR") {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("never signed in is not a finding", func(t *testing.T) {
		if row := checkOpencodeGrant(true, t.TempDir(), "", now); row.Status != statusSkip || !strings.Contains(row.Summary, "muster grant login") {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("signed in and usable", func(t *testing.T) {
		row := checkOpencodeGrant(true, signedIn(t, nil), "openai", now)
		if row.Status != statusPass || row.ID != "runtime.opencode.grant" {
			t.Fatalf("%+v", row)
		}
		if strings.Contains(row.Summary+row.Detail, "grants") && strings.Contains(row.Summary+row.Detail, string(os.PathSeparator)) {
			t.Fatalf("the row names a path: %+v", row)
		}
	})
	t.Run("revoked says what to run", func(t *testing.T) {
		state := signedIn(t, nil)
		if _, _, err := grant.NewStore(state).Update(1, func(g *grant.Grant) { g.Revoked = &grant.Revoked{Status: 401} }); err != nil {
			t.Fatal(err)
		}
		row := checkOpencodeGrant(true, state, "openai", now)
		if row.Status != statusWarn || !strings.Contains(row.Summary, "muster grant login") {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("a sign-in the policy does not allow is said", func(t *testing.T) {
		row := checkOpencodeGrant(true, signedIn(t, nil), "deepseek", now)
		if row.Status != statusWarn || !strings.Contains(row.Summary, "does not allow openai") {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("a lapsed access token warns", func(t *testing.T) {
		row := checkOpencodeGrant(true, signedIn(t, func(g *grant.Grant) { g.ExpiresAtMs = now.Add(-time.Minute).UnixMilli() }), "", now)
		if row.Status != statusWarn || !strings.Contains(row.Summary, "lapsed") {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("a world-readable store fails", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("modes do not apply")
		}
		state := signedIn(t, nil)
		if err := os.Chmod(grant.NewStore(state).Path(), 0o644); err != nil {
			t.Fatal(err)
		}
		row := checkOpencodeGrant(true, state, "", now)
		if row.Status != statusFail || !strings.Contains(row.Summary, "readable by more than its owner") {
			t.Fatalf("%+v", row)
		}
	})
	t.Run("a garbled store fails", func(t *testing.T) {
		state := t.TempDir()
		store := grant.NewStore(state)
		_ = os.MkdirAll(filepath.Dir(store.Path()), 0o700)
		_ = os.WriteFile(store.Path(), []byte("{nope"), 0o600)
		if row := checkOpencodeGrant(true, state, "", now); row.Status != statusFail {
			t.Fatalf("%+v", row)
		}
	})
}
