package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
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
	dir := t.TempDir()
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
	table := service.NewPrincipalTable(principals)
	rl := &reloader{
		path: path, getenv: func(string) string { return "" }, self: "test-machine",
		svc: svc, table: table,
		builder:   &peerBuilder{self: "test-machine", stateDir: dir, selfBuild: fleet.SelfBuild()},
		statusDir: dir, applied: cfg, lastHash: contentHash(raw),
		peers: map[fleet.MachineId]appliedPeer{},
	}
	peers, err := resolvePeers("", cfg)
	if err != nil {
		t.Fatal(err)
	}
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
