package main

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/machinelist"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/service"
)

type reloadRig struct {
	t       *testing.T
	path    string
	dir     string
	rl      *reloader
	srv     *httptest.Server
	svc     *service.Service
	logbuf  strings.Builder
	cleanup func()
}

func rlPrincipal(name, token string, grants ...string) map[string]any {
	return map[string]any{"name": name, "token": token, "grants": grants}
}

func putCfg(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	if err := writeJSONAtomic(path, doc); err != nil {
		t.Fatal(err)
	}
}

// newReloadRig starts a service the way main does, over a config file, with the
// reloader main wires up (but driven by the test, not by a ticker).
func newReloadRig(t *testing.T, doc map[string]any) *reloadRig {
	t.Helper()
	return newReloadRigIn(t, t.TempDir(), doc)
}

// newReloadRigIn is newReloadRig in a directory the test has already put files
// in, such as a machine list the config points at.
func newReloadRigIn(t *testing.T, dir string, doc map[string]any) *reloadRig {
	t.Helper()
	path := filepath.Join(dir, "fleet.json")
	putCfg(t, path, doc)
	cfg, raw, err := loadConfigRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	principals, err := cfg.principals()
	if err != nil {
		t.Fatal(err)
	}
	// Startup, as main does it: hand-written peers, the machine list over them.
	handPeers, err := resolvePeers("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	listOut := loadMachineList(cfg, dir, "test-machine")
	if err := listOut.fatal(); err != nil {
		t.Fatal(err)
	}
	if err := listOut.commit(); err != nil {
		t.Fatal(err)
	}
	principals = listOut.principalsFor(principals, handPeers)
	table := service.NewPrincipalTable(principals)
	rl := &reloader{
		path: path, getenv: func(string) string { return "" }, self: "test-machine",
		svc: svc, table: table, listOut: listOut,
		builder:   &peerBuilder{self: "test-machine", stateDir: dir, selfBuild: fleet.SelfBuild()},
		statusDir: dir, applied: cfg, lastHash: contentHash(raw),
		peers: map[fleet.MachineId]appliedPeer{},
	}
	peers := listOut.peers(handPeers)
	if err := rl.startPeers(peers, cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(service.NewMux(svc, service.Config{Table: table}))
	t.Cleanup(srv.Close)
	return &reloadRig{t: t, path: path, dir: dir, rl: rl, srv: srv, svc: svc}
}

func (r *reloadRig) get(path, token string) (int, string) {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (r *reloadRig) stream(token string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.srv.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		r.t.Fatalf("stream for %s: %v %v", token, resp, err)
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func rlEnds(resp *http.Response, d time.Duration) bool {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func rlDoc(principals ...map[string]any) map[string]any {
	var ps []any
	for _, p := range principals {
		ps = append(ps, p)
	}
	return map[string]any{"principals": ps}
}

// #347: a principal and a peer added to the file take effect on the running
// service, and an existing stream stays open.
func TestReload_AddPrincipalAndPeerWithoutRestart(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
	stream := r.stream("tok-old")

	if code, _ := r.get("/v1/health", "tok-new"); code != 401 {
		t.Fatalf("before the edit, the new token = %d, want 401", code)
	}
	doc := rlDoc(rlPrincipal("old", "tok-old", "read"), rlPrincipal("new", "tok-new", "read"))
	doc["peers"] = []any{map[string]any{"machine": "other", "url": "http://127.0.0.1:1", "token": "t"}}
	putCfg(t, r.path, doc)
	r.rl.check("test")

	if code, _ := r.get("/v1/health", "tok-new"); code != 200 {
		t.Errorf("the new principal = %d, want 200 without a restart", code)
	}
	if _, body := r.get("/v1/machines", "tok-old"); !strings.Contains(body, `"other"`) {
		t.Errorf("the new peer is not listed: %s", body)
	}
	if rlEnds(stream, 300*time.Millisecond) {
		t.Error("an existing stream was closed by the reload")
	}

	// Taking the peer out again removes it.
	delete(doc, "peers")
	putCfg(t, r.path, doc)
	r.rl.check("test")
	if _, body := r.get("/v1/machines", "tok-old"); strings.Contains(body, `"other"`) {
		t.Errorf("a removed peer is still listed: %s", body)
	}
}

// #347: revoking a principal in the file makes its next request a 401 and closes
// its streams, while another principal's stream continues.
func TestReload_RevokeIs401AndClosesOnlyItsStreams(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("gone", "tok-gone", "read"), rlPrincipal("stays", "tok-stays", "read")))
	gone, stays := r.stream("tok-gone"), r.stream("tok-stays")

	putCfg(t, r.path, rlDoc(rlPrincipal("stays", "tok-stays", "read")))
	r.rl.check("test")

	if code, _ := r.get("/v1/health", "tok-gone"); code != 401 {
		t.Errorf("revoked principal = %d, want 401", code)
	}
	if !rlEnds(gone, 2*time.Second) {
		t.Error("the revoked principal's stream is still open")
	}
	if rlEnds(stays, 300*time.Millisecond) {
		t.Error("another principal's stream was closed")
	}
}

// A config that fails validation is refused as a whole: the last good table
// stays, and doctor can say why.
func TestReload_InvalidConfigKeepsLastGood(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))

	cases := map[string]func(){
		"not json": func() { os.WriteFile(r.path, []byte("{nope"), 0o600) },
		"unknown grant": func() {
			putCfg(t, r.path, rlDoc(rlPrincipal("old", "tok-old", "read"), rlPrincipal("bad", "tok-bad", "levitate")))
		},
		"no principals": func() { putCfg(t, r.path, rlDoc()) },
		"peer is self": func() {
			d := rlDoc(rlPrincipal("old", "tok-old", "read"), rlPrincipal("new", "tok-new", "read"))
			d["peers"] = []any{map[string]any{"machine": "test-machine", "url": "http://127.0.0.1:1", "token": "t"}}
			putCfg(t, r.path, d)
		},
	}
	for name, write := range cases {
		write()
		r.rl.check("test")
		if code, _ := r.get("/v1/health", "tok-old"); code != 200 {
			t.Errorf("%s: the last good principal = %d, want 200", name, code)
		}
		if code, _ := r.get("/v1/health", "tok-new"); code != 401 {
			t.Errorf("%s: half of the refused file was applied (new = %d)", name, code)
		}
		row := checkReloadStatus(r.dir, r.path)
		if row.Status != statusWarn || !strings.Contains(row.Summary, "refused") || row.Detail == "" {
			t.Errorf("%s: doctor row = %+v, want a warn naming the refusal", name, row)
		}
	}

	// Fixing the file applies it and clears the warning.
	putCfg(t, r.path, rlDoc(rlPrincipal("old", "tok-old", "read"), rlPrincipal("new", "tok-new", "read")))
	r.rl.check("test")
	if code, _ := r.get("/v1/health", "tok-new"); code != 200 {
		t.Errorf("after the fix, new = %d, want 200", code)
	}
	if row := checkReloadStatus(r.dir, r.path); row.Status != statusPass {
		t.Errorf("doctor row after the fix = %+v, want pass", row)
	}
}

// A setting only a restart applies is named in the refusal, and the principals
// edited alongside it are not half-applied.
func TestReload_RestartOnlySettingIsNamedAndRefused(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))

	doc := rlDoc(rlPrincipal("old", "tok-old", "read"), rlPrincipal("new", "tok-new", "read"))
	doc["maxInputBytes"] = 12345
	doc["defaultRuntime"] = "stub"
	putCfg(t, r.path, doc)
	r.rl.check("test")

	if code, _ := r.get("/v1/health", "tok-new"); code != 401 {
		t.Errorf("principal from a refused file = %d, want 401", code)
	}
	row := checkReloadStatus(r.dir, r.path)
	for _, want := range []string{"maxInputBytes", "defaultRuntime", "restart"} {
		if !strings.Contains(row.Detail, want) {
			t.Errorf("refusal %q does not name %q", row.Detail, want)
		}
	}
}

// sessionEnvDoc is a valid config plus a "new" principal and the given sessionEnv,
// so a refused file can be told apart from an applied one by the new token.
func sessionEnvDoc(env ...map[string]any) map[string]any {
	doc := rlDoc(rlPrincipal("old", "tok-old", "read"), rlPrincipal("new", "tok-new", "read"))
	var es []any
	for _, e := range env {
		es = append(es, e)
	}
	doc["sessionEnv"] = es
	return doc
}

// #366: a reload refused because a restart-only section changed says whether the
// file would start. "Only a restart applies" read as "a restart will apply it",
// and a restart onto a file that does not start is a crash-loop.
func TestReload_RestartOnlyRefusalSaysARestartWouldFail(t *testing.T) {
	cases := []struct {
		name string
		env  []map[string]any
		want string // the start-up error the refusal must carry
	}{
		{"duplicate name",
			[]map[string]any{
				{"name": "GH_TOKEN", "fromSession": "id"},
				{"name": "GH_TOKEN", "fromSession": "id"},
			},
			`"GH_TOKEN" is declared more than once`},
		{"relative fromFile",
			[]map[string]any{{"name": "GH_TOKEN", "fromFile": "secrets/gh-token"}},
			"must be an absolute path"},
		{"unknown runtime id",
			[]map[string]any{{"name": "GH_TOKEN", "fromSession": "id",
				"appliesTo": map[string]any{"runtimes": []string{"opencod"}}}},
			`"opencod"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
			putCfg(t, r.path, sessionEnvDoc(c.env...))
			r.rl.check("test")

			row := checkReloadStatus(r.dir, r.path)
			if row.Status != statusWarn {
				t.Fatalf("doctor row = %+v, want a warn naming the refusal", row)
			}
			for _, want := range []string{"sessionEnv", "only a restart applies", "a restart would fail", c.want} {
				if !strings.Contains(row.Detail, want) {
					t.Errorf("the refusal %q does not say %q", row.Detail, want)
				}
			}
			if strings.Contains(row.Detail, "passes start-up validation") {
				t.Errorf("a file that would not start was called valid: %q", row.Detail)
			}
			// A refused reload still applies nothing, including the file's principals.
			if code, _ := r.get("/v1/health", "tok-new"); code != 401 {
				t.Errorf("a principal from the refused file = %d, want 401", code)
			}
			if code, _ := r.get("/v1/health", "tok-old"); code != 200 {
				t.Errorf("the last good principal = %d, want 200", code)
			}
		})
	}
}

// #366: the same refusal for a restart-only change that WOULD start says so, so
// the operator can restart knowing the file is the one the service will accept.
func TestReload_RestartOnlyRefusalSaysTheFilePassesStartup(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
	putCfg(t, r.path, sessionEnvDoc(map[string]any{"name": "GH_TOKEN", "fromSession": "id"}))
	r.rl.check("test")

	row := checkReloadStatus(r.dir, r.path)
	if row.Status != statusWarn {
		t.Fatalf("doctor row = %+v, want a warn naming the refusal", row)
	}
	for _, want := range []string{"sessionEnv", "only a restart applies", "the file passes start-up validation", "nothing from this edit was applied"} {
		if !strings.Contains(row.Detail, want) {
			t.Errorf("the refusal %q does not say %q", row.Detail, want)
		}
	}
	if strings.Contains(row.Detail, "would fail") {
		t.Errorf("a file that starts was reported as failing: %q", row.Detail)
	}
	if code, _ := r.get("/v1/health", "tok-new"); code != 401 {
		t.Errorf("a principal from the refused file = %d, want 401", code)
	}
}

// #366: the verdict is the one `muster doctor` gives the same file, because both
// are the same checks. A fix to either surface that the other lacked would show here.
func TestReload_RestartOnlyVerdictMatchesDoctorConfigLoad(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
	putCfg(t, r.path, sessionEnvDoc(
		map[string]any{"name": "GH_TOKEN", "fromSession": "id"},
		map[string]any{"name": "GH_TOKEN", "fromSession": "id"},
	))
	r.rl.check("test")

	cfg, err := loadConfig(r.path)
	if err != nil {
		t.Fatal(err)
	}
	doctorErr := validateConfigExtras(cfg, func(string) string { return "" })
	if doctorErr == nil {
		t.Fatal("doctor's check accepted a file with a duplicate sessionEnv name")
	}
	if row := checkReloadStatus(r.dir, r.path); !strings.Contains(row.Detail, doctorErr.Error()) {
		t.Errorf("the refusal %q does not carry doctor's error %q", row.Detail, doctorErr)
	}
}

// #370: a restart that loads a new file rewrites the record, so the refusal the
// restart already resolved is not reported by doctor afterwards.
func TestReloadStart_RestartClearsAnEarlierRefusal(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
	good := sessionEnvDoc(map[string]any{"name": "GH_TOKEN", "fromSession": "id"})
	putCfg(t, r.path, good)
	r.rl.check("test")
	if row := checkReloadStatus(r.dir, r.path); row.Status != statusWarn || !strings.Contains(row.Summary, "refused") {
		t.Fatalf("setup: doctor row = %+v, want the restart-only refusal", row)
	}

	// The restart: a new process, over the same state directory, loading the file.
	r2 := newReloadRigIn(t, r.dir, good)
	r2.rl.recordStart()

	st := r2.rl.readStatus()
	raw, _ := os.ReadFile(r2.path)
	if st.AppliedHash != contentHash(raw) {
		t.Errorf("appliedHash = %q, want the started file's %q", st.AppliedHash, contentHash(raw))
	}
	if st.AppliedReason != "start" || st.AppliedAt == "" {
		t.Errorf("appliedReason/appliedAt = %q / %q, want start and a time", st.AppliedReason, st.AppliedAt)
	}
	if st.RefusedHash != "" || st.RefusedAt != "" || st.Error != "" {
		t.Errorf("the refusal survived the restart: %+v", st)
	}
	if row := checkReloadStatus(r2.dir, r2.path); row.Status != statusPass {
		t.Errorf("doctor row right after the restart = %+v, want pass", row)
	}
}

// #370: the record is replaced, not merged. A previous process's applied hash and
// time, and whatever it said about the machine list, are not believed after a start.
func TestReloadStart_ReplacesWhatThePreviousProcessRecorded(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
	stale := reloadStatus{
		AppliedHash: "0000000000000000", AppliedAt: "2026-01-01T00:00:00Z",
		RefusedHash: "1111111111111111", RefusedAt: "2026-01-01T01:00:00Z", Error: "an old refusal",
		ListRefusedHash: "2222222222222222", ListRefusedAt: "2026-01-01T01:00:00Z", ListError: "an old list refusal",
		ListAppliedHash: "3333333333333333", ListAppliedSeq: 9, ListAppliedAt: "2026-01-01T00:00:00Z",
	}
	if err := writeJSONAtomic(filepath.Join(r.dir, reloadStatusFile), stale); err != nil {
		t.Fatal(err)
	}

	r.rl.recordStart()

	raw, _ := os.ReadFile(r.path)
	want := reloadStatus{AppliedHash: contentHash(raw), AppliedReason: "start"}
	got := r.rl.readStatus()
	if got.AppliedAt == "" || got.AppliedAt == stale.AppliedAt {
		t.Errorf("appliedAt = %q, want the start time", got.AppliedAt)
	}
	got.AppliedAt = ""
	if got != want {
		t.Errorf("status after a start = %+v, want %+v", got, want)
	}
}

// #370: a start on a verified machine list records that list as the one in force,
// and a refusal of a previous list is not carried over. Not a hash of the config
// alone: doctor's list warning keys on the list file's own hash.
func TestReloadStart_RecordsTheMachineListItLoaded(t *testing.T) {
	r, l := newListRig(t, "only", 4, map[string]string{"m-a": "http://127.0.0.1:1"})
	// A previous process refused a list; the file on disk has since been replaced by a good one.
	if err := writeJSONAtomic(filepath.Join(r.dir, reloadStatusFile), reloadStatus{
		AppliedHash: "0000000000000000", ListRefusedHash: "2222222222222222", ListError: "an old list refusal",
	}); err != nil {
		t.Fatal(err)
	}

	r.rl.recordStart()

	rawList, _ := os.ReadFile(l.path)
	st := r.rl.readStatus()
	if st.ListAppliedHash != contentHash(rawList) || st.ListAppliedSeq != 4 || st.ListAppliedAt == "" {
		t.Errorf("list applied = %q seq %d at %q, want the started list's %q seq 4", st.ListAppliedHash, st.ListAppliedSeq, st.ListAppliedAt, contentHash(rawList))
	}
	if st.ListRefusedHash != "" || st.ListRefusedAt != "" || st.ListError != "" {
		t.Errorf("the list refusal survived the restart: %+v", st)
	}
	if row := checkReloadStatus(r.dir, r.path); row.Status != statusPass {
		t.Errorf("doctor row right after the restart = %+v, want pass", row)
	}
}

// #370: after a start, a reload still works from that record: the next edit is
// applied and the record moves on from "start".
func TestReloadStart_ALaterReloadMovesTheRecordOn(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
	r.rl.recordStart()

	putCfg(t, r.path, rlDoc(rlPrincipal("old", "tok-old", "read"), rlPrincipal("new", "tok-new", "read")))
	r.rl.check("SIGHUP")

	raw, _ := os.ReadFile(r.path)
	st := r.rl.readStatus()
	if st.AppliedHash != contentHash(raw) || st.AppliedReason != "SIGHUP" {
		t.Errorf("status after a reload = %+v, want the new hash applied by SIGHUP", st)
	}
	if code, _ := r.get("/v1/health", "tok-new"); code != 200 {
		t.Errorf("the reloaded principal = %d, want 200", code)
	}
}

// Nothing happens when the file did not change, and nothing is logged for it.
func TestReload_UnchangedFileIsNotReapplied(t *testing.T) {
	r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
	before, _ := os.ReadFile(filepath.Join(r.dir, reloadStatusFile))
	r.rl.check("test")
	after, _ := os.ReadFile(filepath.Join(r.dir, reloadStatusFile))
	if string(before) != string(after) {
		t.Error("an unchanged file rewrote the status")
	}
}

func TestReloadStatus_DoctorSkipsWithoutStateDir(t *testing.T) {
	if row := checkReloadStatus("", "x"); row.Status != statusSkip {
		t.Errorf("row = %+v, want skip", row)
	}
}

// --- the owner-signed machine list on a running service (muster #354) ---------

type liveList struct {
	owner ed25519.PrivateKey
	path  string
}

// put signs and writes a list naming the given machines (id -> address) at seq.
func (l liveList) put(t *testing.T, seq uint64, machines map[string]string) []byte {
	t.Helper()
	doc := machinelist.Document{
		Fleet: "fleet-1", Seq: seq, Signers: []string{listPub(l.owner)},
		Machines: []machinelist.Machine{}, Issuers: []machinelist.Issuer{},
	}
	var ids []string
	for id := range machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		doc.Machines = append(doc.Machines, machinelist.Machine{
			ID: id, PublicKey: listPub(listKey(byte(0x40 + i))),
			Addresses: []string{machines[id]}, Grants: []string{"read"},
		})
	}
	raw, err := machinelist.Sign(doc, l.owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := putFileAtomic(l.path, raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func putFileAtomic(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newListRig(t *testing.T, mode string, firstSeq uint64, first map[string]string) (*reloadRig, liveList) {
	t.Helper()
	dir := t.TempDir()
	l := liveList{owner: listKey(0x01), path: filepath.Join(dir, "machines.json")}
	l.put(t, firstSeq, first)
	// Peers from a list authenticate by key, so this machine needs its own.
	if _, priv, err := peerkey.Generate(); err != nil {
		t.Fatal(err)
	} else if err := peerkey.SavePrivate(filepath.Join(dir, machineKeyFile), priv); err != nil {
		t.Fatal(err)
	}
	doc := rlDoc(rlPrincipal("op", "tok-op", "read"))
	doc["machineList"] = map[string]any{"mode": mode, "path": l.path, "fleet": "fleet-1", "signer": listPub(l.owner)}
	return newReloadRigIn(t, dir, doc), l
}

func liveMachinesBody(r *reloadRig) string {
	_, body := r.get("/v1/machines", "tok-op")
	return body
}

func liveHasPrincipal(r *reloadRig, name string) bool {
	for _, p := range r.rl.table.Snapshot() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// A newer signed list adds one machine and retires another on a running
// service, and a stream open the whole time is not touched.
func TestReloadList_NewerListAddsAndRemovesAPeer(t *testing.T) {
	r, l := newListRig(t, "only", 1, map[string]string{"m-a": "http://127.0.0.1:1", "m-b": "http://127.0.0.1:2"})
	stream := r.stream("tok-op")
	if b := liveMachinesBody(r); !strings.Contains(b, `"m-a"`) || !strings.Contains(b, `"m-b"`) {
		t.Fatalf("the start did not apply the list: %s", b)
	}

	l.put(t, 2, map[string]string{"m-a": "http://127.0.0.1:1", "m-c": "http://127.0.0.1:3"})
	r.rl.check("test")

	b := liveMachinesBody(r)
	if !strings.Contains(b, `"m-c"`) {
		t.Errorf("the machine the new list adds is missing: %s", b)
	}
	if strings.Contains(b, `"m-b"`) {
		t.Errorf("the machine the new list retires is still listed: %s", b)
	}
	if !liveHasPrincipal(r, "m-c") || liveHasPrincipal(r, "m-b") {
		t.Errorf("the principal table does not follow the list: %+v", r.rl.table.Snapshot())
	}
	if rlEnds(stream, 300*time.Millisecond) {
		t.Error("an unrelated stream was closed by the list reload")
	}
	if row := checkReloadStatus(r.dir, r.path); row.Status != statusPass {
		t.Errorf("doctor row after a good list = %+v, want pass", row)
	}
	if seq, _ := machinelist.ReadSeq(r.dir); seq != 2 {
		t.Errorf("the highest accepted sequence = %d, want 2 (the rollback guard moved)", seq)
	}
}

// A list that does not verify, and an older genuine one, are each refused as a
// whole: the last good list stays and doctor says why.
func TestReloadList_TamperedAndRollbackAreRefusedLastGoodKept(t *testing.T) {
	r, l := newListRig(t, "only", 5, map[string]string{"m-a": "http://127.0.0.1:1"})
	raw := l.put(t, 6, map[string]string{"m-a": "http://127.0.0.1:1", "m-b": "http://127.0.0.1:2"})
	r.rl.check("test")
	if !strings.Contains(liveMachinesBody(r), `"m-b"`) {
		t.Fatal("the good list was not applied")
	}

	good := map[string]string{"m-a": "http://127.0.0.1:1", "m-b": "http://127.0.0.1:2"}
	cases := map[string]func(){
		"tampered": func() {
			// The payload is signed; swap in a different, validly encoded one.
			var env machinelist.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			other := liveList{owner: l.owner, path: filepath.Join(r.dir, "other.json")}
			var env2 machinelist.Envelope
			if err := json.Unmarshal(other.put(t, 6, map[string]string{"m-z": "http://127.0.0.1:7"}), &env2); err != nil {
				t.Fatal(err)
			}
			env.Payload = env2.Payload
			bad, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			if err := putFileAtomic(l.path, bad); err != nil {
				t.Fatal(err)
			}
		},
		"rollback":                      func() { l.put(t, 3, map[string]string{"m-z": "http://127.0.0.1:7"}) },
		"same sequence, different list": func() { l.put(t, 6, map[string]string{"m-z": "http://127.0.0.1:7"}) },
		"signed by a stranger": func() {
			l2 := liveList{owner: listKey(0x77), path: l.path}
			l2.put(t, 9, map[string]string{"m-z": "http://127.0.0.1:7"})
		},
		"not json": func() { _ = putFileAtomic(l.path, []byte("{nope")) },
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		cases[name]()
		r.rl.check("test")
		b := liveMachinesBody(r)
		if !strings.Contains(b, `"m-a"`) || !strings.Contains(b, `"m-b"`) || strings.Contains(b, `"m-z"`) {
			t.Errorf("%s: the last good list did not stay: %s", name, b)
		}
		if seq, _ := machinelist.ReadSeq(r.dir); seq != 6 {
			t.Errorf("%s: the accepted sequence moved to %d", name, seq)
		}
		row := checkReloadStatus(r.dir, r.path)
		if row.Status != statusWarn || !strings.Contains(row.Summary, "machine list") || row.Detail == "" {
			t.Errorf("%s: doctor row = %+v, want a warn naming the list", name, row)
		}
		if code, _ := r.get("/v1/health", "tok-op"); code != 200 {
			t.Errorf("%s: the operator = %d, want 200", name, code)
		}
	}

	// The owner signs a good, higher list: it applies and the warning clears.
	l.put(t, 7, good)
	r.rl.check("test")
	if row := checkReloadStatus(r.dir, r.path); row.Status != statusPass {
		t.Errorf("doctor row after a good list = %+v, want pass", row)
	}
}

// A list that was already looked at is not applied again, and a list change
// leaves the config's own record alone.
func TestReloadList_UnchangedListIsNotReapplied(t *testing.T) {
	r, l := newListRig(t, "only", 1, map[string]string{"m-a": "http://127.0.0.1:1"})
	r.rl.check("test")
	statusPath := filepath.Join(r.dir, reloadStatusFile)
	if _, err := os.Stat(statusPath); !os.IsNotExist(err) {
		t.Fatal("an unchanged list and config wrote a status")
	}
	l.put(t, 2, map[string]string{"m-a": "http://127.0.0.1:1", "m-b": "http://127.0.0.1:2"})
	r.rl.check("test")
	st := r.rl.readStatus()
	if st.ListAppliedSeq != 2 || st.AppliedHash != contentHash(liveMustRead(t, r.path)) {
		t.Errorf("status after a list change = %+v", st)
	}
	before, _ := os.ReadFile(statusPath)
	peerBefore := r.rl.peers["m-b"].drv
	r.rl.check("test")
	after, _ := os.ReadFile(statusPath)
	if string(before) != string(after) {
		t.Error("an unchanged list rewrote the status")
	}
	if r.rl.peers["m-b"].drv != peerBefore {
		t.Error("an unchanged list rebuilt a peer")
	}
}

// A refused list does not hold back an unrelated edit to the config, and the
// config edit is applied over the list in force.
func TestReloadList_RefusedListDoesNotBlockAConfigEdit(t *testing.T) {
	r, l := newListRig(t, "only", 4, map[string]string{"m-a": "http://127.0.0.1:1"})
	l.put(t, 2, map[string]string{"m-z": "http://127.0.0.1:7"}) // a rollback
	doc := rlDoc(rlPrincipal("op", "tok-op", "read"), rlPrincipal("new", "tok-new", "read"))
	doc["machineList"] = map[string]any{"mode": "only", "path": l.path, "fleet": "fleet-1", "signer": listPub(l.owner)}
	putCfg(t, r.path, doc)
	r.rl.check("test")

	if code, _ := r.get("/v1/health", "tok-new"); code != 200 {
		t.Errorf("the config edit = %d, want 200", code)
	}
	if b := liveMachinesBody(r); !strings.Contains(b, `"m-a"`) || strings.Contains(b, `"m-z"`) {
		t.Errorf("the list in force changed: %s", b)
	}
	row := checkReloadStatus(r.dir, r.path)
	if row.Status != statusWarn || !strings.Contains(row.Detail, "older") {
		t.Errorf("doctor row = %+v, want a warn naming the older list", row)
	}
}

// Where the list is, who signs it and what mode it is in are still read once.
func TestReloadList_ThePathAndPinsStayRestartOnly(t *testing.T) {
	r, l := newListRig(t, "only", 1, map[string]string{"m-a": "http://127.0.0.1:1"})
	doc := rlDoc(rlPrincipal("op", "tok-op", "read"), rlPrincipal("new", "tok-new", "read"))
	doc["machineList"] = map[string]any{"mode": "only", "path": l.path + ".other", "fleet": "fleet-1", "signer": listPub(l.owner)}
	putCfg(t, r.path, doc)
	r.rl.check("test")
	if code, _ := r.get("/v1/health", "tok-new"); code != 401 {
		t.Errorf("an edit that moves the list = %d, want 401 (refused whole)", code)
	}
	if row := checkReloadStatus(r.dir, r.path); !strings.Contains(row.Detail, "machineList") {
		t.Errorf("refusal %q does not name machineList", row.Detail)
	}
}

func liveMustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
