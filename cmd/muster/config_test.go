package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #47: trustRoots is a plain, optional field on the same file principals and
// peers already live in — this asserts it round-trips and that a config
// with no trustRoots at all (the common case, and every config that existed
// before #47) still loads exactly as it did.
func TestLoadConfigReadsTrustRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"principals": [{"name": "op", "token": "tok", "grants": ["read"]}],
		"trustRoots": ["/Users/someone/workspace", "/Users/someone/tools"]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/Users/someone/workspace", "/Users/someone/tools"}
	if len(cfg.TrustRoots) != len(want) {
		t.Fatalf("TrustRoots = %v, want %v", cfg.TrustRoots, want)
	}
	for i, w := range want {
		if cfg.TrustRoots[i] != w {
			t.Errorf("TrustRoots[%d] = %q, want %q", i, cfg.TrustRoots[i], w)
		}
	}
}

func TestLoadConfigWithoutTrustRootsLeavesItEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [{"name": "op", "token": "tok"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustRoots) != 0 {
		t.Errorf("TrustRoots = %v, want empty", cfg.TrustRoots)
	}
}

// muster issue #60: defaultRuntime is a plain, optional field on the
// same file trustRoots lives on, and round-trips the same way.
func TestLoadConfigReadsDefaultRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"principals": [{"name": "op", "token": "tok", "grants": ["read"]}],
		"defaultRuntime": "tmux"
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultRuntime != "tmux" {
		t.Errorf("DefaultRuntime = %q, want %q", cfg.DefaultRuntime, "tmux")
	}
}

func TestLoadConfigWithoutDefaultRuntimeLeavesItEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [{"name": "op", "token": "tok"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultRuntime != "" {
		t.Errorf("DefaultRuntime = %q, want empty — absent means the older behaviour", cfg.DefaultRuntime)
	}
}

// muster #130: maxInputBytes is a plain, optional field on the same
// file defaultRuntime already lives on, and round-trips the same way.
func TestLoadConfigReadsMaxInputBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"principals": [{"name": "op", "token": "tok", "grants": ["read"]}],
		"maxInputBytes": 4096
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxInputBytes != 4096 {
		t.Errorf("MaxInputBytes = %d, want 4096", cfg.MaxInputBytes)
	}
}

func TestLoadConfigWithoutMaxInputBytesLeavesItZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [{"name": "op", "token": "tok"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxInputBytes != 0 {
		t.Errorf("MaxInputBytes = %d, want 0 — absent means the shipped default applies unchanged (#130)", cfg.MaxInputBytes)
	}
}

// muster #272: gateRemoteControlInput is a plain optional boolean; absent
// means the behaviour from before #272 (the gate is off for one release).
func TestLoadConfigReadsGateRemoteControlInput(t *testing.T) {
	for body, want := range map[string]bool{
		`{"principals": [{"name": "op", "token": "tok"}], "gateRemoteControlInput": true}`:  true,
		`{"principals": [{"name": "op", "token": "tok"}], "gateRemoteControlInput": false}`: false,
		`{"principals": [{"name": "op", "token": "tok"}]}`:                                  false,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GateRemoteControlInput != want {
			t.Errorf("%s: GateRemoteControlInput = %v, want %v", body, cfg.GateRemoteControlInput, want)
		}
	}
}

// muster issue #94: sessionEnv is a plain, optional field on the same
// file trustRoots and defaultRuntime already live on, and round-trips the
// same way — including appliesTo, which is itself optional within an entry.
func TestLoadConfigReadsSessionEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"principals": [{"name": "op", "token": "tok", "grants": ["read"]}],
		"sessionEnv": [
			{"name": "FLEET_IDENTITY", "fromFile": "/etc/fleet/identity", "required": true,
			 "appliesTo": {"agents": ["sid"], "markers": ["filing"]}},
			{"name": "FLEET_NICE_TO_HAVE", "fromFile": "/etc/fleet/optional"}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SessionEnv) != 2 {
		t.Fatalf("SessionEnv = %v, want 2 entries", cfg.SessionEnv)
	}

	entries := cfg.sessionEnv()
	if len(entries) != 2 {
		t.Fatalf("sessionEnv() = %v, want 2 entries", entries)
	}

	first := entries[0]
	if first.Name != "FLEET_IDENTITY" || first.FromFile != "/etc/fleet/identity" || !first.Required {
		t.Errorf("first entry = %+v, want the required identity entry", first)
	}
	if len(first.AppliesTo.Agents) != 1 || first.AppliesTo.Agents[0] != "sid" {
		t.Errorf("first entry AppliesTo.Agents = %v, want [\"sid\"]", first.AppliesTo.Agents)
	}
	if len(first.AppliesTo.Markers) != 1 || first.AppliesTo.Markers[0] != "filing" {
		t.Errorf("first entry AppliesTo.Markers = %v, want [\"filing\"]", first.AppliesTo.Markers)
	}

	second := entries[1]
	if second.Name != "FLEET_NICE_TO_HAVE" || second.Required {
		t.Errorf("second entry = %+v, want a non-required entry with the default appliesTo (matches everything)", second)
	}
	if len(second.AppliesTo.Agents) != 0 || len(second.AppliesTo.Markers) != 0 {
		t.Errorf("second entry AppliesTo = %+v, want the zero value — no appliesTo block was given", second.AppliesTo)
	}
}

func TestLoadConfigWithoutSessionEnvLeavesItEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [{"name": "op", "token": "tok"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SessionEnv) != 0 {
		t.Errorf("SessionEnv = %v, want empty", cfg.SessionEnv)
	}
	if entries := cfg.sessionEnv(); entries != nil {
		t.Errorf("sessionEnv() = %v, want nil so WithSessionEnv's off-by-default check "+
			"(len == 0) still sees an unconfigured driver", entries)
	}
}

// The loader's DisallowUnknownFields (muster issue #94's finding 1: the
// code ships before the config entry, never the reverse) must keep applying
// inside a sessionEnv entry, not just at the top level — a typo here is
// exactly the kind of mistake that should fail the daemon at startup rather
// than silently produce an entry with a zero-value field.
func TestLoadConfigRejectsATypoInsideASessionEnvEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"principals": [{"name": "op", "token": "tok"}],
		"sessionEnv": [{"nam": "FLEET_IDENTITY", "fromFile": "/etc/fleet/identity"}]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("a misspelled field inside a sessionEnv entry was accepted silently")
	}
}

func TestTrustSeedIntervalDefaultsAndValidates(t *testing.T) {
	t.Setenv("FLEET_TRUST_SEED_INTERVAL", "")
	if got := trustSeedInterval(); got != 2*time.Minute {
		t.Errorf("default = %s, want 2m", got)
	}

	t.Setenv("FLEET_TRUST_SEED_INTERVAL", "30s")
	if got := trustSeedInterval(); got != 30*time.Second {
		t.Errorf("30s = %s, want 30s", got)
	}

	// An invalid or non-positive value falls back to the default rather
	// than producing a zero or negative ticker interval, which
	// time.NewTicker panics on.
	for _, bad := range []string{"not-a-duration", "-5m", "0s"} {
		t.Setenv("FLEET_TRUST_SEED_INTERVAL", bad)
		if got := trustSeedInterval(); got != 2*time.Minute {
			t.Errorf("bad value %q = %s, want the 2m default", bad, got)
		}
	}
}

// muster #95: main.go's FLEET_TOKEN gate is only safe to relax for a
// missing token when loadConfig's own guards are trustworthy — an
// unreadable file, a malformed one, and a file naming no principals must
// all still refuse to load, exactly as before. These three had no direct
// test; the pipeline this issue touches is only as sound as they are, so
// pin all three here rather than trust that main.go's log.Fatal on any
// loadConfig error was ever exercised.
func TestLoadConfigRejectsAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := loadConfig(path); err == nil {
		t.Fatal("a config path that does not exist was accepted silently")
	}
}

func TestLoadConfigRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [{"name": "op", "token": "tok"}` // truncated, no closing brackets
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("malformed JSON was accepted silently")
	}
}

// This is the guard that decides whether an "empty-principal-table" startup
// is even reachable: loadConfig refuses it outright, so main.go's
// requireToken(cfgFile) can safely read cfgFile != nil as "a validated,
// non-empty table" rather than having to re-check emptiness itself.
func TestLoadConfigRejectsAnEmptyPrincipalTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": []}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("a config naming no principals was accepted silently")
	}
}

// muster #98: selfCredential is what lets a table-only deployment give
// its own outbound peer identity a credential — Service.peerRequest already
// names that identity "system:"+self, so the table only needs to carry a
// principal by that exact name.
func TestSelfCredentialFindsTheMatchingPrincipal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [
		{"name": "op", "token": "op-tok"},
		{"name": "system:machine-a", "token": "self-tok"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := cfg.selfCredential("machine-a")
	if !ok || tok != "self-tok" {
		t.Errorf("selfCredential(\"machine-a\") = (%q, %v), want (\"self-tok\", true)", tok, ok)
	}
}

// No principal named for this machine's own system identity is "nothing
// configured", not an error — main.go's peerCredential must be free to fall
// through to an empty credential (fail closed) rather than treat this as a
// malformed table. Also covers the "no principals at all for this identity,
// but the table names OTHER principals" case, which is the one an operator
// who has never heard of #98 will actually have.
func TestSelfCredentialWithNoMatchingPrincipalIsNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [{"name": "op", "token": "op-tok"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok, ok := cfg.selfCredential("machine-a"); ok || tok != "" {
		t.Errorf("selfCredential(\"machine-a\") = (%q, %v), want (\"\", false) — no principal names this machine's system identity", tok, ok)
	}
	// A different machine's identity must not match either — this is a
	// per-machine name, not a wildcard.
	if tok, ok := cfg.selfCredential("machine-b"); ok || tok != "" {
		t.Errorf("selfCredential(\"machine-b\") = (%q, %v), want (\"\", false)", tok, ok)
	}
}

func TestLoadConfigRejectsAPrincipalMissingATokenOrName(t *testing.T) {
	cases := []string{
		`{"principals": [{"name": "op"}]}`,
		`{"principals": [{"token": "tok"}]}`,
	}
	for _, body := range cases {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Errorf("body %s: a principal missing a name or token was accepted silently", body)
		}
	}
}
