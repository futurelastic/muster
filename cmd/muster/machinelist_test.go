package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/machinelist"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/service"
)

func listKey(b byte) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = b
	}
	return ed25519.NewKeyFromSeed(seed)
}

func listPub(k ed25519.PrivateKey) string {
	return peerkey.FormatPublicKey(k.Public().(ed25519.PublicKey))
}

// listFixture writes a signed list and a config pointing at it, in a temp dir.
type listFixture struct {
	dir, listPath, stateDir string
	owner                   ed25519.PrivateKey
}

func newListFixture(t *testing.T, mode string, seq uint64) (*listFixture, *fileConfig) {
	t.Helper()
	f := &listFixture{dir: t.TempDir(), owner: listKey(0x01)}
	f.listPath = filepath.Join(f.dir, "machines.json")
	f.stateDir = filepath.Join(f.dir, "state")
	f.write(t, seq, f.owner)
	cfg := peersFrom(t, `{"peers":[
		{"machine":"machine-b","url":"http://old-b:1","token":"tb"},
		{"machine":"hand-only","url":"http://hand:1","token":"th"}],
	  "principals":[
		{"name":"operator","token":"op","grants":["read","create"]},
		{"name":"machine-b","token":"old-token","grants":["read"]},
		{"name":"hand-only","token":"h","grants":["read"]}]}`)
	cfg.MachineList = &machineListConfig{Mode: mode, Path: f.listPath, Fleet: "fleet-1", Signer: listPub(f.owner)}
	return f, cfg
}

func (f *listFixture) write(t *testing.T, seq uint64, key ed25519.PrivateKey) {
	t.Helper()
	doc := machinelist.Document{
		Fleet: "fleet-1", Seq: seq, Signers: []string{listPub(f.owner)},
		Machines: []machinelist.Machine{
			{ID: "machine-a", PublicKey: listPub(listKey(0x11)), Addresses: []string{"http://a:1"}, Grants: []string{"read", "send"}},
			{ID: "machine-b", PublicKey: listPub(listKey(0x22)), Addresses: []string{"http://b:1", "http://b2:1"}, Grants: []string{"read", "create"}},
		},
		Issuers: []machinelist.Issuer{{ID: "sso", PublicKey: listPub(listKey(0x33))}},
	}
	if key == nil {
		key = f.owner
	}
	if k := listPub(key); k != doc.Signers[0] {
		doc.NextSigner = k
	}
	raw, err := machinelist.Sign(doc, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.listPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func basePeers(t *testing.T, cfg *fileConfig) []peerEntry {
	t.Helper()
	b, err := resolvePeers("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestShadowChangesNothing(t *testing.T) {
	f, cfg := newListFixture(t, "shadow", 4)
	base := basePeers(t, cfg)
	basePrincipals, err := cfg.principals()
	if err != nil {
		t.Fatal(err)
	}
	o := loadMachineList(cfg, f.stateDir, "self")
	if o.err != nil || o.verified == nil {
		t.Fatalf("shadow did not verify: %v", o.err)
	}
	if got := o.peers(base); !reflect.DeepEqual(got, base) {
		t.Errorf("shadow changed the peers: %+v", got)
	}
	if got := o.principalsFor(basePrincipals, base); !reflect.DeepEqual(got, basePrincipals) {
		t.Errorf("shadow changed the principals: %+v", got)
	}
	if err := o.commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, machinelist.SeqFile)); !os.IsNotExist(err) {
		t.Error("shadow recorded a sequence number: it must write nothing")
	}
	diff := strings.Join(o.shadowDiff(base), "\n")
	for _, want := range []string{"+ machine-a", "~ machine-b", "- hand-only"} {
		if !strings.Contains(diff, want) {
			t.Errorf("shadow report lacks %q:\n%s", want, diff)
		}
	}
}

func TestShadowWithInvalidListIsNeverFatal(t *testing.T) {
	f, cfg := newListFixture(t, "shadow", 4)
	_ = os.WriteFile(f.listPath, []byte("not a list"), 0o644)
	o := loadMachineList(cfg, f.stateDir, "self")
	if o.err == nil {
		t.Fatal("garbage verified")
	}
	if o.fatal() != nil {
		t.Error("shadow must never stop the service")
	}
	base := basePeers(t, cfg)
	if got := o.peers(base); !reflect.DeepEqual(got, base) {
		t.Error("shadow changed the peers")
	}
}

func TestOnlyWithInvalidListFailsClosed(t *testing.T) {
	cases := map[string]func(*listFixture, *testing.T){
		"garbage":    func(f *listFixture, t *testing.T) { _ = os.WriteFile(f.listPath, []byte("{}"), 0o644) },
		"missing":    func(f *listFixture, t *testing.T) { _ = os.Remove(f.listPath) },
		"bad signer": func(f *listFixture, t *testing.T) { f.write(t, 4, listKey(0x09)) },
		"edited": func(f *listFixture, t *testing.T) {
			raw, _ := os.ReadFile(f.listPath)
			_ = os.WriteFile(f.listPath, bytes.Replace(raw, []byte(`"signature": "`), []byte(`"signature": "AA`), 1), 0o644)
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f, cfg := newListFixture(t, "only", 4)
			breakIt(f, t)
			o := loadMachineList(cfg, f.stateDir, "self")
			if o.fatal() == nil {
				t.Fatal("only-mode with an invalid list must stop the service")
			}
			if got := o.peers(basePeers(t, cfg)); len(got) != 0 {
				t.Errorf("only-mode fell back to hand-written peers: %+v", got)
			}
		})
	}
}

func TestOnlyAndPreferNeedAStateDir(t *testing.T) {
	for _, mode := range []string{"prefer", "only"} {
		_, cfg := newListFixture(t, mode, 4)
		o := loadMachineList(cfg, "", "self")
		if o.err == nil || !strings.Contains(o.err.Error(), "FLEET_STATE_DIR") {
			t.Errorf("%s with no state dir: %v", mode, o.err)
		}
	}
}

func TestRollbackAcrossRestarts(t *testing.T) {
	f, cfg := newListFixture(t, "only", 5)
	o := loadMachineList(cfg, f.stateDir, "self")
	if o.err != nil {
		t.Fatal(o.err)
	}
	if err := o.commit(); err != nil {
		t.Fatal(err)
	}
	f.write(t, 3, nil) // an older genuine list, replayed
	if o := loadMachineList(cfg, f.stateDir, "self"); o.fatal() == nil || !strings.Contains(o.err.Error(), "older") {
		t.Fatalf("an older genuine list was accepted: %v", o.err)
	}
	f.write(t, 6, nil)
	if o := loadMachineList(cfg, f.stateDir, "self"); o.err != nil {
		t.Fatalf("a newer list: %v", o.err)
	}
}

func TestPreferMerge(t *testing.T) {
	f, cfg := newListFixture(t, "prefer", 4)
	base := basePeers(t, cfg)
	basePrincipals, _ := cfg.principals()
	o := loadMachineList(cfg, f.stateDir, "machine-a") // this machine is machine-a
	if o.err != nil {
		t.Fatal(o.err)
	}
	peers := o.peers(base)
	got := map[string]peerEntry{}
	for _, p := range peers {
		got[string(p.machine)] = p
	}
	if _, ok := got["machine-a"]; ok {
		t.Error("a machine is not its own peer")
	}
	if b := got["machine-b"]; !reflect.DeepEqual(b.urls, []string{"http://b:1", "http://b2:1"}) || b.auth != peerAuthKey {
		t.Errorf("the list did not win for machine-b: %+v", b)
	}
	if h, ok := got["hand-only"]; !ok || h.urls[0] != "http://hand:1" {
		t.Errorf("prefer dropped a hand-written peer the list does not name: %+v", peers)
	}
	pr := map[string]service.Principal{}
	for _, p := range o.principalsFor(basePrincipals, base) {
		pr[p.Name] = p
	}
	if pr["operator"].Token != "op" || pr["hand-only"].Token != "h" {
		t.Error("prefer touched principals the list does not name")
	}
	if p := pr["machine-b"]; p.Token != "" || len(p.PublicKey) == 0 || !p.Allows(service.GrantCreate) {
		t.Errorf("machine-b's principal is not the list's: %+v", p)
	}
	if err := o.commit(); err != nil {
		t.Fatal(err)
	}
	if n, _ := machinelist.ReadSeq(f.stateDir); n != 4 {
		t.Errorf("sequence recorded = %d, want 4", n)
	}
}

func TestPreferWithInvalidListFallsBackToHandWritten(t *testing.T) {
	f, cfg := newListFixture(t, "prefer", 4)
	_ = os.WriteFile(f.listPath, []byte("x"), 0o644)
	base := basePeers(t, cfg)
	o := loadMachineList(cfg, f.stateDir, "self")
	if o.err == nil || o.fatal() != nil {
		t.Fatalf("err=%v fatal=%v", o.err, o.fatal())
	}
	if got := o.peers(base); !reflect.DeepEqual(got, base) {
		t.Error("prefer with an invalid list must leave the hand-written peers alone")
	}
}

func TestOnlyIsTheSoleSourceOfPeers(t *testing.T) {
	f, cfg := newListFixture(t, "only", 4)
	base := basePeers(t, cfg)
	basePrincipals, _ := cfg.principals()
	o := loadMachineList(cfg, f.stateDir, "self")
	peers := o.peers(base)
	if len(peers) != 2 || peers[0].machine != "machine-a" || peers[1].machine != "machine-b" {
		t.Fatalf("peers = %+v", peers)
	}
	names := map[string]bool{}
	for _, p := range o.principalsFor(basePrincipals, base) {
		names[p.Name] = true
	}
	if !names["operator"] {
		t.Error("only-mode dropped a client principal")
	}
	if names["hand-only"] {
		t.Error("only-mode kept the principal of a hand-written peer it no longer has")
	}
}

func TestListGrantUnknownRefusesTheList(t *testing.T) {
	f, cfg := newListFixture(t, "only", 4)
	doc := machinelist.Document{Fleet: "fleet-1", Seq: 4, Signers: []string{listPub(f.owner)},
		Machines: []machinelist.Machine{{ID: "m", PublicKey: listPub(listKey(0x22)), Addresses: []string{"http://m:1"}, Grants: []string{"teleport"}}}}
	raw, _ := machinelist.Sign(doc, f.owner)
	_ = os.WriteFile(f.listPath, raw, 0o644)
	if o := loadMachineList(cfg, f.stateDir, "self"); o.err == nil || !strings.Contains(o.err.Error(), "teleport") {
		t.Fatalf("an unknown grant was accepted: %v", o.err)
	}
}

func TestMachineListConfigValidation(t *testing.T) {
	good := listPub(listKey(1))
	cases := map[string]machineListConfig{
		"bad mode":   {Mode: "always"},
		"no path":    {Mode: "only", Fleet: "f", Signer: good},
		"no fleet":   {Mode: "only", Path: "/p", Signer: good},
		"no signer":  {Mode: "only", Path: "/p", Fleet: "f"},
		"bad signer": {Mode: "only", Path: "/p", Fleet: "f", Signer: "ed25519:x"},
		"bad next":   {Mode: "only", Path: "/p", Fleet: "f", Signer: good, NextSigner: "nope"},
	}
	for name, c := range cases {
		if err := c.validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (&machineListConfig{Mode: "off"}).validate(); err != nil {
		t.Errorf("off: %v", err)
	}
	var none *machineListConfig
	if err := none.validate(); err != nil {
		t.Errorf("absent: %v", err)
	}
}

func TestListFromEffectivePeersForDoctor(t *testing.T) {
	f, cfg := newListFixture(t, "only", 4)
	env := map[string]string{"FLEET_STATE_DIR": f.stateDir, "FLEET_MACHINE": "machine-a"}
	got, err := doctorPeers(func(k string) string { return env[k] }, cfg)
	if err != nil || len(got) != 1 || got[0].machine != "machine-b" || got[0].auth != peerAuthKey {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestMachineListCLIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "owner.key")
	run := func(args ...string) (string, string, int) {
		var out, errb bytes.Buffer
		handled, code := runMachineList(append([]string{"machine-list"}, args...), func(string) string { return "" }, &out, &errb)
		if !handled {
			t.Fatal("not handled")
		}
		return out.String(), errb.String(), code
	}
	pub, _, code := run("keygen", "--out="+keyPath)
	if code != 0 || !strings.HasPrefix(pub, "ed25519:") {
		t.Fatalf("keygen: %q %d", pub, code)
	}
	if _, _, code := run("keygen", "--out="+keyPath); code == 0 {
		t.Error("keygen replaced an existing key")
	}
	pub = strings.TrimSpace(pub)
	draft, list := filepath.Join(dir, "draft.json"), filepath.Join(dir, "list.json")
	if _, e, code := run("create", "--fleet=fleet-1", "--key="+keyPath, "--out="+draft,
		"--machine=machine-a,"+listPub(listKey(0x11))+",http://a:1|http://a2:1,read|send",
		"--issuer=sso,"+listPub(listKey(0x33))); code != 0 {
		t.Fatalf("create: %s", e)
	}
	if _, e, code := run("sign", "--in="+draft, "--key="+keyPath, "--seq=2", "--out="+list); code != 0 {
		t.Fatalf("sign: %s", e)
	}
	out, e, code := run("verify", list, "--fleet=fleet-1", "--signer="+pub)
	if code != 0 || !strings.Contains(out, "seq 2") {
		t.Fatalf("verify: %q %q", out, e)
	}
	if _, _, code := run("verify", list, "--fleet=other", "--signer="+pub); code == 0 {
		t.Error("verify accepted another fleet's pin")
	}
	if _, _, code := run("verify", list, "--fleet=fleet-1", "--signer="+pub, "--min-seq=3"); code == 0 {
		t.Error("verify accepted a rolled-back list")
	}
	shown, _, code := run("show", list, "--fleet=fleet-1", "--signer="+pub)
	if code != 0 || !strings.Contains(shown, "signature: verified") || !strings.Contains(shown, "http://a2:1") {
		t.Fatalf("show: %q", shown)
	}
	unpinned, _, _ := run("show", list)
	if !strings.Contains(unpinned, "NOT VERIFIED") {
		t.Errorf("show without a pin must say it did not verify:\n%s", unpinned)
	}
	var env machinelist.Envelope
	raw, _ := os.ReadFile(list)
	_ = json.Unmarshal(raw, &env)
	if env.Format != machinelist.Format {
		t.Errorf("envelope = %+v", env)
	}
	// A different pinned key refuses it.
	if _, _, code := run("verify", list, "--fleet=fleet-1", "--signer="+listPub(listKey(9))); code == 0 {
		t.Error("an unpinned signer verified")
	}
}

func TestDoctorRow(t *testing.T) {
	f, cfg := newListFixture(t, "shadow", 4)
	base := basePeers(t, cfg)
	row := listDoctorRow(loadMachineList(cfg, f.stateDir, "self"), base)
	if row.ID != "machine-list" || row.Status != statusPass || !strings.Contains(row.Detail, "shadow changes nothing") {
		t.Errorf("shadow row = %+v", row)
	}
	cfg.MachineList.Mode = "only"
	_ = os.Remove(f.listPath)
	if row := listDoctorRow(loadMachineList(cfg, f.stateDir, "self"), base); row.Status != statusFail {
		t.Errorf("only-mode with a missing list must fail the check: %+v", row)
	}
	if row := listDoctorRow(loadMachineList(nil, "", "self"), nil); row.Status != statusSkip {
		t.Errorf("no list: %+v", row)
	}
}
