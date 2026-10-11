package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/privfs"
)

// Enrolment writes a bearer token to disk, so the platform-specific half of it
// is the part worth running everywhere: the token file must be owner-only on
// the platform's own terms (a mode on Unix, a protected access list on
// Windows) and the config rewrite must not loosen the file it replaces
// (#303). These tests ran on no platform before this file existed.

const enrolFixture = `{
  "maxInputBytes": 1000000,
  "principals": [
    {"name": "supervisor", "token": "tok-a", "grants": ["read", "send"]}
  ]
}
`

func enrolFixtureAt(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := privfs.WriteFile(p, []byte(enrolFixture)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnrolAddWritesAnOwnerOnlyTokenFileAndTheMatchingPrincipal(t *testing.T) {
	dir := t.TempDir()
	cfg := enrolFixtureAt(t, dir)

	if err := addPrincipal(cfg, "client", "read,send", ""); err != nil {
		t.Fatalf("addPrincipal: %v", err)
	}

	tokenFile := filepath.Join(dir, "client.token")
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("no token file at the default path: %v", err)
	}
	token := strings.TrimSuffix(string(raw), "\n")
	if len(token) < 40 || strings.ContainsAny(token, " \r\n") {
		t.Errorf("token %q does not look like 32 random bytes in base64url", token)
	}
	if r := privfs.Check(tokenFile); r.Verdict != privfs.OwnerOnly {
		t.Errorf("token file is %s (%s), want owner-only", r.Verdict, r.Detail)
	}

	// The config names the principal with exactly that token and those grants,
	// and kept what was already there.
	cfgRaw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MaxInputBytes int `json:"maxInputBytes"`
		Principals    []struct {
			Name   string   `json:"name"`
			Token  string   `json:"token"`
			Grants []string `json:"grants"`
		} `json:"principals"`
	}
	if err := json.Unmarshal(cfgRaw, &doc); err != nil {
		t.Fatalf("config is not JSON after the rewrite: %v\n%s", err, cfgRaw)
	}
	if doc.MaxInputBytes != 1000000 || len(doc.Principals) != 2 {
		t.Fatalf("config after add = %+v", doc)
	}
	if p := doc.Principals[0]; p.Name != "supervisor" || p.Token != "tok-a" {
		t.Errorf("an existing principal changed: %+v", p)
	}
	added := doc.Principals[1]
	if added.Name != "client" || added.Token != token || strings.Join(added.Grants, ",") != "read,send" {
		t.Errorf("added principal = %+v, want client / the token on disk / read,send", added)
	}
	if r := privfs.Check(cfg); r.Verdict != privfs.OwnerOnly {
		t.Errorf("the rewritten config is %s (%s), want owner-only (it holds every token)", r.Verdict, r.Detail)
	}
	// The result must load with the service's own loader.
	if _, err := loadConfig(cfg); err != nil {
		t.Errorf("the service cannot load what enrolment wrote: %v", err)
	}
}

func TestEnrolAddHonoursAnExplicitTokenFile(t *testing.T) {
	dir := t.TempDir()
	cfg := enrolFixtureAt(t, dir)
	elsewhere := filepath.Join(dir, "out", "c.tok")
	if err := os.Mkdir(filepath.Dir(elsewhere), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := addPrincipal(cfg, "client", "read", elsewhere); err != nil {
		t.Fatalf("addPrincipal: %v", err)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("token not written where asked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "client.token")); err == nil {
		t.Error("a token file was also written at the default path")
	}
	if r := privfs.Check(elsewhere); r.Verdict != privfs.OwnerOnly {
		t.Errorf("token file is %s (%s), want owner-only", r.Verdict, r.Detail)
	}
}

func TestEnrolRefusalsWriteNothing(t *testing.T) {
	cases := []struct {
		name    string
		prin    string
		grants  string
		wantErr string
	}{
		{"unknown grant", "client", "read,nope", `unknown grant "nope"`},
		{"no grants", "client", "", "--grants is required"},
		{"blank name", "  ", "read", "needs a name"},
		{"duplicate", "supervisor", "read", "already exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := enrolFixtureAt(t, dir)
			err := addPrincipal(cfg, tc.prin, tc.grants, "")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			if raw, _ := os.ReadFile(cfg); string(raw) != enrolFixture {
				t.Errorf("config changed on a refused add:\n%s", raw)
			}
			if m, _ := filepath.Glob(filepath.Join(dir, "*.token")); len(m) != 0 {
				t.Errorf("a token file was written for a refused add: %v", m)
			}
		})
	}
}

func TestEnrolRefusesAConfigThatIsNotJSONAndLeavesItAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	const torn = `{"principals": [`
	if err := os.WriteFile(cfg, []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	err := addPrincipal(cfg, "client", "read", "")
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(cfg); string(raw) != torn {
		t.Errorf("a config that would not parse was rewritten: %q", raw)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.token")); len(m) != 0 {
		t.Errorf("a token file was written for a config that would not parse: %v", m)
	}
}

func TestEnrolMintsDistinctTokens(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		tok, err := mintToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("mintToken returned %q twice", tok)
		}
		seen[tok] = true
	}
}

func TestRunPrincipalAddAndListThroughTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	cfg := enrolFixtureAt(t, dir)
	handled, err := runPrincipal([]string{"principal", "add", "cli-client", "--grants=read", "--config=" + cfg})
	if !handled || err != nil {
		t.Fatalf("principal add = %v, %v", handled, err)
	}
	tokenFile := filepath.Join(dir, "cli-client.token")
	if r := privfs.Check(tokenFile); r.Verdict != privfs.OwnerOnly {
		t.Errorf("token file is %s (%s), want owner-only", r.Verdict, r.Detail)
	}
	handled, err = runPrincipal([]string{"principal", "list", "--config=" + cfg})
	if !handled || err != nil {
		t.Fatalf("principal list = %v, %v", handled, err)
	}
	// An unrelated invocation is not consumed.
	if handled, err := runPrincipal([]string{"doctor"}); handled || err != nil {
		t.Errorf("runPrincipal(doctor) = %v, %v; want not handled", handled, err)
	}
}
