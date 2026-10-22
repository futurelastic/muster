package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/privfs"
	"github.com/futurelastic/muster/internal/service"
	"github.com/futurelastic/muster/internal/state"
)

// muster #350: the operator's side — config, enrolment, the key command, doctor.

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := privfs.WriteFile(p, []byte(body)); err != nil {
		t.Fatal(err)
	}
	return p
}

func newPub(t *testing.T) string {
	t.Helper()
	pub, _, err := peerkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return peerkey.FormatPublicKey(pub)
}

func TestConfig_APrincipalHasATokenOrAKeyNotBothNotNeither(t *testing.T) {
	key := newPub(t)
	for name, tc := range map[string]struct {
		body string
		want string // substring of the error; empty means it loads
	}{
		"token only": {`{"principals":[{"name":"a","token":"t","grants":["read"]}]}`, ""},
		"key only":   {`{"principals":[{"name":"a","publicKey":"` + key + `","grants":["read"]}]}`, ""},
		"both":       {`{"principals":[{"name":"a","token":"t","publicKey":"` + key + `","grants":["read"]}]}`, "both a token and a publicKey"},
		"neither":    {`{"principals":[{"name":"a","grants":["read"]}]}`, "either a token or a publicKey"},
		"bad key":    {`{"principals":[{"name":"a","publicKey":"ed25519:nope","grants":["read"]}]}`, "publicKey"},
		"peer key + token": {`{"principals":[{"name":"a","token":"t","grants":["read"]}],"peers":[{"machine":"b","url":"http://x:1","auth":"key","token":"t"}]}`,
			"must not carry a token"},
		"peer auth junk": {`{"principals":[{"name":"a","token":"t","grants":["read"]}],"peers":[{"machine":"b","url":"http://x:1","auth":"sorcery"}]}`, "not \"token\" or \"key\""},
		"mixed peers":    {`{"principals":[{"name":"a","token":"t","grants":["read"]}],"peers":[{"machine":"b","url":"http://x:1","auth":"key"},{"machine":"c","url":"http://y:1","token":"s"}]}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(writeCfg(t, tc.body))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	cfg, err := loadConfig(writeCfg(t, `{"principals":[{"name":"a","publicKey":"`+key+`","grants":["read"]}],"peers":[{"machine":"b","url":"http://x:1","auth":"key"},{"machine":"c","url":"http://y:1","token":"s"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ps, err := cfg.principals()
	if err != nil || len(ps[0].PublicKey) != ed25519.PublicKeySize || ps[0].Token != "" {
		t.Fatalf("principals = %+v, %v", ps, err)
	}
	if cfg.peerAuthFor("b") != "key" || cfg.peerAuthFor("c") != "token" || cfg.peerAuthFor("unlisted") != "token" {
		t.Errorf("peerAuthFor: b=%s c=%s", cfg.peerAuthFor("b"), cfg.peerAuthFor("c"))
	}
	if _, ok := cfg.selfCredential("x"); ok {
		t.Error("a key principal has no token to offer as the system credential")
	}
}

func TestEnrol_APeerThatSignsIsAddedByItsPublicKeyAndGetsNoToken(t *testing.T) {
	dir := t.TempDir()
	cfg := enrolFixtureAt(t, dir)
	key := newPub(t)

	if err := addKeyPrincipal(cfg, "peerbox", "read,send", key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "peerbox.token")); err == nil {
		t.Error("a token file was written for a principal that signs")
	}
	c, err := loadConfig(cfg)
	if err != nil {
		t.Fatalf("the rewritten config does not load: %v", err)
	}
	var found bool
	for _, p := range c.Principals {
		if p.Name == "peerbox" {
			found = true
			if p.Token != "" || p.PublicKey != key || strings.Join(p.Grants, ",") != "read,send" {
				t.Errorf("entry = %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("principal not written")
	}
	if err := addKeyPrincipal(cfg, "peerbox", "read", key); err == nil {
		t.Error("a duplicate name was accepted")
	}
	if err := addKeyPrincipal(cfg, "other", "read", "ed25519:nope"); err == nil {
		t.Error("a bad key was accepted")
	}
	if err := addKeyPrincipal(cfg, "other", "teleport", key); err == nil {
		t.Error("an unknown grant was accepted")
	}
	handled, err := runPrincipal([]string{"principal", "add", "via-cli", "--grants=read", "--public-key=" + newPub(t), "--config=" + cfg})
	if !handled || err != nil {
		t.Fatalf("principal add --public-key: %v %v", handled, err)
	}
	if handled, err := runPrincipal([]string{"principal", "add", "both", "--grants=read", "--public-key=" + key, "--token-file=/tmp/x", "--config=" + cfg}); !handled || err == nil {
		t.Errorf("--public-key with --token-file must be refused: %v %v", handled, err)
	}
}

func TestKey_InitShowAndNeverReplace(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	getenv := func(string) string { return "" }

	if handled, code := runKey([]string{"key", "show", "--state-dir=" + dir}, getenv, &out, &errb); !handled || code == 0 {
		t.Fatalf("show with no key: %v %d", handled, code)
	}
	out.Reset()
	if _, code := runKey([]string{"key", "init", "--state-dir=" + dir}, getenv, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	first := strings.TrimSpace(out.String())
	if _, err := peerkey.ParsePublicKey(first); err != nil {
		t.Fatalf("init printed %q", first)
	}
	path := filepath.Join(dir, machineKeyFile)
	if r := privfs.Check(path); r.Verdict != privfs.OwnerOnly {
		t.Errorf("key file is %s (%s)", r.Verdict, r.Detail)
	}
	out.Reset()
	if _, code := runKey([]string{"key", "init", "--state-dir=" + dir}, getenv, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != first {
		t.Fatalf("a second init replaced the key: %d %q vs %q", code, out.String(), first)
	}
	out.Reset()
	if _, code := runKey([]string{"key", "show", "--state-dir=" + dir}, getenv, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != first {
		t.Fatalf("show: %d %q", code, out.String())
	}
	// The private half is never printed.
	raw, _ := os.ReadFile(path)
	seed := strings.TrimPrefix(strings.TrimSpace(string(raw)), "ed25519-private:")
	if strings.Contains(out.String()+errb.String(), seed) {
		t.Error("the private key reached the output")
	}
	if handled, _ := runKey([]string{"principal"}, getenv, &out, &errb); handled {
		t.Error("runKey consumed another command")
	}
}

func TestKey_StateDirResolution(t *testing.T) {
	env := func(k string) string {
		if k == "FLEET_STATE_DIR" {
			return "/from/env"
		}
		return ""
	}
	if d, _ := keyStateDir("/flag", env); d != "/flag" {
		t.Errorf("flag = %q", d)
	}
	if d, _ := keyStateDir("", env); d != "/from/env" {
		t.Errorf("env = %q", d)
	}
}

// rotation: a real owning service, a real signed request, the real key file.
func TestKeyRotate_PeersAreToldThenTheKeySwitches(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, machineKeyFile)
	pub, _, err := ensureMachineKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Not t.TempDir(): the service holds its input-audit log open for its whole
	// life and has no Close, and Windows cannot remove an open file, which would
	// fail the test's cleanup rather than the test. The leftover is a few bytes
	// in the OS temp directory.
	stateDir, err := os.MkdirTemp("", "muster-rotate-state-*")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := state.Open(stateDir)
	svc, err := service.NewWithState("owner", st)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(service.NewMux(svc, service.Config{Principals: []service.Principal{
		{Name: "me", PublicKey: pub, Grants: []service.Grant{service.GrantRead}},
	}}))
	t.Cleanup(srv.Close)

	cfg := writeCfg(t, `{"principals":[{"name":"x","token":"t","grants":["read"]}],"peers":[{"machine":"owner","url":"`+srv.URL+`","auth":"key"}]}`)
	var out bytes.Buffer
	getenv := func(string) string { return "" }
	if err := rotateMachineKey(t.Context(), keyPath, cfg, "me", getenv, srv.Client(), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	after, err := peerkey.LoadPrivate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	newPub := after.Public().(ed25519.PublicKey)
	if newPub.Equal(pub) {
		t.Fatal("the key did not change")
	}
	if _, err := os.Stat(keyPath + nextKeySuffix); err == nil {
		t.Error("the pending key file was left behind")
	}
	// The owner now accepts the new key (and the old, for the grace period).
	signer, _ := peerkey.NewSigner(keyPath, "me")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/health", nil)
	if err := signer.Sign(req, nil, "owner"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the new key was refused: %d", resp.StatusCode)
	}
}

func TestKeyRotate_AnUnreachablePeerLeavesTheOldKeyInPlaceAndTheNewOnePending(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, machineKeyFile)
	pub, _, _ := ensureMachineKey(keyPath)

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	cfg := writeCfg(t, `{"principals":[{"name":"x","token":"t","grants":["read"]}],"peers":[{"machine":"gone","url":"`+deadURL+`","auth":"key"}]}`)
	var out bytes.Buffer
	getenv := func(string) string { return "" }
	err := rotateMachineKey(t.Context(), keyPath, cfg, "me", getenv, &http.Client{}, &out)
	if err == nil || !strings.Contains(err.Error(), "still signs with the old one") {
		t.Fatalf("err = %v", err)
	}
	cur, _ := peerkey.LoadPrivate(keyPath)
	if !cur.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("the key switched although a peer was not told")
	}
	pending, err := peerkey.LoadPrivate(keyPath + nextKeySuffix)
	if err != nil {
		t.Fatalf("no pending key to retry with: %v", err)
	}
	// A second attempt offers the SAME new key, not another.
	_ = rotateMachineKey(t.Context(), keyPath, cfg, "me", getenv, &http.Client{}, &out)
	again, _ := peerkey.LoadPrivate(keyPath + nextKeySuffix)
	if !again.Public().(ed25519.PublicKey).Equal(pending.Public().(ed25519.PublicKey)) {
		t.Error("a retry minted a different key")
	}
}

func TestKeyRotate_RefusesWithoutAKeyPeer(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, machineKeyFile)
	_, _, _ = ensureMachineKey(keyPath)
	cfg := writeCfg(t, `{"principals":[{"name":"x","token":"t","grants":["read"]}],"peers":[{"machine":"p","url":"http://x:1","token":"s"}]}`)
	err := rotateMachineKey(t.Context(), keyPath, cfg, "me", func(string) string { return "" }, &http.Client{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("err = %v", err)
	}
}

func TestDoctor_KeyCredentialRow(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "FLEET_STATE_DIR" {
			return dir
		}
		return ""
	}
	cred, row := keyCredential(getenv, "me", "peerbox")
	if row.Status != statusFail || !strings.Contains(row.Detail, "muster key init") || cred.signer != nil {
		t.Fatalf("no key: %+v", row)
	}
	pub, _, _ := ensureMachineKey(filepath.Join(dir, machineKeyFile))
	cred, row = keyCredential(getenv, "me", "peerbox")
	if row.Status != statusPass || !strings.Contains(row.Summary, peerkey.Fingerprint(pub)) || cred.signer == nil {
		t.Fatalf("with key: %+v", row)
	}
	if row.ID != "peer.peerbox.credential" {
		t.Errorf("id = %q", row.ID)
	}
	_, row = keyCredential(func(string) string { return "" }, "me", "peerbox")
	if row.Status != statusFail {
		t.Errorf("no state dir: %+v", row)
	}
	// The row never carries the private key.
	b, _ := json.Marshal(row)
	raw, _ := os.ReadFile(filepath.Join(dir, machineKeyFile))
	if strings.Contains(string(b), strings.TrimPrefix(strings.TrimSpace(string(raw)), "ed25519-private:")) {
		t.Error("private key in a doctor row")
	}
}
