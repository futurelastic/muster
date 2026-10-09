package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const grantsFixture = `{
  "maxInputBytes": 1000000,
  "principals": [
    {"name": "supervisor", "token": "tok-a", "grants": ["read", "send"]},
    {"name": "other", "token": "tok-b", "grants": ["read"]}
  ]
}
`

func writeGrantsFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func grantsOf(t *testing.T, path, name string) []string {
	t.Helper()
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range c.Principals {
		if p.Name == name {
			return p.Grants
		}
	}
	t.Fatalf("no principal %q", name)
	return nil
}

func edit(t *testing.T, path string, revoke bool, name string, grants []string, allow bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := applyGrantEdit(grantEdit{
		cfgPath: path, name: name, grants: grants, revoke: revoke, allowHumanRelay: allow,
		now: time.Date(2026, 10, 9, 5, 30, 0, 0, time.UTC),
	}, &out)
	return out.String(), err
}

func TestGrantAddsToAnExistingPrincipalAndKeepsTheRest(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	out, err := edit(t, p, false, "supervisor", []string{"remote-control", "send"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(grantsOf(t, p, "supervisor"), ","); got != "read,send,remote-control" {
		t.Errorf("grants = %s", got)
	}
	if got := strings.Join(grantsOf(t, p, "other"), ","); got != "read" {
		t.Errorf("another principal changed: %s", got)
	}
	// Untouched settings survive the rewrite, numbers included.
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"maxInputBytes": 1000000`) {
		t.Errorf("a setting was altered by the rewrite:\n%s", raw)
	}
	// The result is printed in whoami's keys.
	var last struct {
		Principal string   `json:"principal"`
		Grants    []string `json:"grants"`
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-2]), &last); err != nil {
		t.Fatalf("grant set line is not JSON: %v\n%s", err, out)
	}
	if last.Principal != "supervisor" || strings.Join(last.Grants, ",") != "read,send,remote-control" {
		t.Errorf("printed set = %+v", last)
	}
	if !strings.Contains(out, "restart") {
		t.Errorf("output does not say the running service is unchanged:\n%s", out)
	}
}

func TestGrantWritesADatedBackupOfTheOriginal(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	if _, err := edit(t, p, false, "other", []string{"send"}, false); err != nil {
		t.Fatal(err)
	}
	b := p + ".20261009-053000.bak"
	got, err := os.ReadFile(b)
	if err != nil {
		t.Fatalf("no dated backup: %v", err)
	}
	if string(got) != grantsFixture {
		t.Errorf("backup is not the original bytes:\n%s", got)
	}
	if st, _ := os.Stat(b); st.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600 (it holds every token)", st.Mode().Perm())
	}
}

func TestGrantRefusesAnUnknownNameAndWritesNothing(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	_, err := edit(t, p, false, "supervisor", []string{"send", "nope"}, false)
	if err == nil || !strings.Contains(err.Error(), `unknown grant "nope"`) {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(p); string(raw) != grantsFixture {
		t.Error("config changed on a refused edit")
	}
	if m, _ := filepath.Glob(p + ".*.bak"); len(m) != 0 {
		t.Errorf("a backup was written for a refused edit: %v", m)
	}
}

func TestGrantRefusesHumanRelayUnlessAllowed(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	_, err := edit(t, p, false, "supervisor", []string{"human-relay"}, false)
	if err == nil || !strings.Contains(err.Error(), "--allow-human-relay") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(p); string(raw) != grantsFixture {
		t.Error("config changed on a refused edit")
	}
	out, err := edit(t, p, false, "supervisor", []string{"human-relay"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "not shared with an agent") {
		t.Errorf("override printed no shared-principal warning:\n%s", out)
	}
	if !contains(grantsOf(t, p, "supervisor"), "human-relay") {
		t.Error("override did not grant")
	}
}

func TestRevokeRemovesAndRevokingHumanRelayNeedsNoOverride(t *testing.T) {
	p := writeGrantsFixture(t, `{"principals":[{"name":"a","token":"t","grants":["read","human-relay"]}]}`)
	if _, err := edit(t, p, true, "a", []string{"human-relay"}, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(grantsOf(t, p, "a"), ","); got != "read" {
		t.Errorf("grants = %s", got)
	}
}

func TestRevokeCanRemoveAGrantThisBuildDoesNotKnowButNothingElseUnknown(t *testing.T) {
	p := writeGrantsFixture(t, `{"principals":[{"name":"a","token":"t","grants":["read","retired"]}]}`)
	if _, err := edit(t, p, true, "a", []string{"typo"}, false); err == nil {
		t.Error("revoking a name nobody holds and nobody knows should be refused")
	}
	if _, err := edit(t, p, true, "a", []string{"retired"}, false); err != nil {
		t.Fatalf("repair by revoking a held, retired grant failed: %v", err)
	}
	if got := strings.Join(grantsOf(t, p, "a"), ","); got != "read" {
		t.Errorf("grants = %s", got)
	}
}

func TestGrantIsIdempotentAndWritesNoBackupWhenNothingChanges(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	out, err := edit(t, p, false, "supervisor", []string{"read"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no change") {
		t.Errorf("output = %s", out)
	}
	if raw, _ := os.ReadFile(p); string(raw) != grantsFixture {
		t.Error("config rewritten on a no-op")
	}
	if m, _ := filepath.Glob(p + ".*.bak"); len(m) != 0 {
		t.Errorf("backup written on a no-op: %v", m)
	}
}

func TestEditRefusesAnUnknownPrincipalAndInvalidJSON(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	if _, err := edit(t, p, false, "ghost", []string{"read"}, false); err == nil || !strings.Contains(err.Error(), `no principal "ghost"`) {
		t.Errorf("err = %v", err)
	}
	bad := writeGrantsFixture(t, `{not json`)
	if _, err := edit(t, bad, false, "a", []string{"read"}, false); err == nil {
		t.Error("invalid JSON must be refused")
	}
	if raw, _ := os.ReadFile(bad); string(raw) != `{not json` {
		t.Error("an invalid file was rewritten")
	}
}

func TestRevokingTheLastGrantIsAllowedAndSaid(t *testing.T) {
	p := writeGrantsFixture(t, `{"principals":[{"name":"a","token":"t","grants":["read"]}]}`)
	out, err := edit(t, p, true, "a", []string{"read"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "holds no grants") || !strings.Contains(out, `"grants":[]`) {
		t.Errorf("output = %s", out)
	}
}

func TestRunGrantEditParsesCommaAndSpaceSeparatedGrants(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	var out bytes.Buffer
	if err := runGrantEdit(false, []string{"other", "send,keys", "label", "--config=" + p}, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(grantsOf(t, p, "other"), ","); got != "read,send,keys,label" {
		t.Errorf("grants = %s", got)
	}
	if err := runGrantEdit(false, []string{"other", "--config=" + p}, &out); err == nil {
		t.Error("no grants named must be a usage error")
	}
	if err := runGrantEdit(false, []string{"other", "send", "--bogus", "--config=" + p}, &out); err == nil {
		t.Error("unknown flag must be refused")
	}
}

func TestTwoEditsInOneSecondKeepBothOriginals(t *testing.T) {
	p := writeGrantsFixture(t, grantsFixture)
	if _, err := edit(t, p, false, "other", []string{"send"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := edit(t, p, false, "other", []string{"keys"}, false); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(p + ".*.bak*")
	if len(m) != 2 {
		t.Fatalf("backups = %v, want two", m)
	}
	first, _ := os.ReadFile(p + ".20261009-053000.bak")
	if string(first) != grantsFixture {
		t.Errorf("the first original was overwritten:\n%s", first)
	}
}
