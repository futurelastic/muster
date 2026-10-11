package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/privfs"
)

// --- helpers ------------------------------------------------------------------

type testPrincipal struct {
	Name   string   `json:"name"`
	Token  string   `json:"token"`
	Grants []string `json:"grants"`
}

type testPeer struct {
	Machine string `json:"machine"`
	URL     string `json:"url"`
	Token   string `json:"token,omitempty"`
}

const supervisorList = "read,create,send,interrupt,close,rename,discard,keys"

func grants(list string) []string { return strings.Split(list, ",") }

func writeTestConfig(t *testing.T, dir string, principals []testPrincipal, peers []testPeer) string {
	t.Helper()
	doc := map[string]any{"principals": principals}
	if len(peers) > 0 {
		doc["peers"] = peers
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testGetenv reads vars. An unset FLEET_RUNTIME reads as "tmux", the default
// these tests were written against: the service's own default is "none" on
// Windows (ADR 303), and a test about the terminal runtime's rows must mean the
// same thing on every platform. A test about the default itself sets nothing and
// asks defaultRuntime directly (runtime_test.go).
func testGetenv(vars map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := vars[k]; ok {
			return v
		}
		if k == "FLEET_RUNTIME" {
			return "tmux"
		}
		return ""
	}
}

func testDoctorEnv(vars map[string]string) doctorEnv {
	return doctorEnv{
		Getenv:   testGetenv(vars),
		Skip:     map[string]bool{},
		Timeout:  2 * time.Second,
		HTTP:     &http.Client{Timeout: 2 * time.Second},
		LookPath: func(string) (string, error) { return "tmux", nil },
	}
}

func rowByID(t *testing.T, rows []doctorRow, id string) doctorRow {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", id, rows)
	return doctorRow{}
}

func runDoctorTest(vars map[string]string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	// No working directory: the clone row (#201) is about whichever clone the
	// tests happen to run in, and is tested on its own in doctor_hooks_test.go.
	_, code = runDoctorAt("", append([]string{"doctor"}, args...), testGetenv(vars), &out, &errb)
	return code, out.String(), errb.String()
}

// executable writes a stand-in for the tmux binary. On Windows it is a file
// like any other: the doctor's tmux row judges a POSIX execute bit that Windows
// does not report, so the tests that assert THAT row's pass skip there
// themselves (execBitRowsSkipOnWindows) — the ones that only need some tmux
// path to be configured keep running.
func executable(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-tmux")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// execBitRowsSkipOnWindows skips a case that asserts a row which judges a POSIX
// execute bit. The row itself is a separate, known gap (#312); the skip says so
// in the gate's summary instead of hiding it.
func execBitRowsSkipOnWindows(t *testing.T) {
	t.Helper()
	skipOnWindows(t, "the row judges a POSIX execute bit, which Windows does not report")
}

// --- rows ---------------------------------------------------------------------

// The #68 shape: a verified install whose supervising principal was never
// given keys. Named by --principal, it must fail and name the grant.
func TestDoctorConfigMissingKeysGrantFails(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTestConfig(t, dir, []testPrincipal{{Name: "sup", Token: "tok-sup-secret", Grants: grants("read,send")}}, nil)
	vars := map[string]string{"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_RUNTIME": "stub", "FLEET_ADDR": "127.0.0.1:9000"}

	code, out, _ := runDoctorTest(vars, "--principal=sup")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	env := testDoctorEnv(vars)
	env.Principal = "sup"
	row := rowByID(t, runChecks(context.Background(), env), "principals.supervisor")
	if row.Status != statusFail || !strings.Contains(row.Summary, "keys") {
		t.Fatalf("got %+v, want a fail naming keys", row)
	}

	env.Principal = "nobody"
	if row := rowByID(t, runChecks(context.Background(), env), "principals.supervisor"); row.Status != statusFail {
		t.Fatalf("unknown principal: got %+v, want fail", row)
	}
}

func TestDoctorSupervisorWithoutPrincipal(t *testing.T) {
	cases := []struct {
		name       string
		principals []testPrincipal
		want       rowStatus
	}{
		{"one full supervisor", []testPrincipal{
			{Name: "sup", Token: "a", Grants: grants(supervisorList)},
			{Name: "viewer", Token: "b", Grants: grants("read")},
		}, statusPass},
		{"a principal one grant short is drift", []testPrincipal{
			{Name: "sup", Token: "a", Grants: grants(supervisorList)},
			{Name: "sup-new-machine", Token: "b", Grants: grants("read,create,send,interrupt,close,discard,keys")},
		}, statusWarn},
		{"nobody can fully drive sessions", []testPrincipal{
			{Name: "viewer", Token: "b", Grants: grants("read,send")},
		}, statusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeTestConfig(t, t.TempDir(), tc.principals, nil)
			rows := runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_CONFIG": cfg, "FLEET_RUNTIME": "stub"}))
			row := rowByID(t, rows, "principals.supervisor")
			if row.Status != tc.want {
				t.Fatalf("got %+v, want %s", row, tc.want)
			}
			if tc.want == statusWarn && !strings.Contains(row.Summary, "rename") {
				t.Fatalf("warn must name the missing grant: %+v", row)
			}
		})
	}
}

func TestDoctorRelayRequiredOnlyWithPeers(t *testing.T) {
	peer := []testPeer{{Machine: "m2", URL: "http://192.0.2.1:9000", Token: "p"}}
	cases := []struct {
		name  string
		sup   string
		peers []testPeer
		want  rowStatus
	}{
		{"no peers", supervisorList, nil, statusSkip},
		{"peers, no relay", supervisorList, peer, statusFail},
		{"peers, relay", supervisorList + ",relay", peer, statusPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeTestConfig(t, t.TempDir(), []testPrincipal{
				{Name: "sup", Token: "a", Grants: grants(tc.sup)},
				{Name: "system:m1", Token: "s", Grants: grants("read")},
			}, tc.peers)
			env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_RUNTIME": "stub"})
			env.Offline = true
			if row := rowByID(t, runChecks(context.Background(), env), "principals.relay"); row.Status != tc.want {
				t.Fatalf("got %+v, want %s", row, tc.want)
			}
		})
	}

	// Single-token mode expresses relay as FLEET_ALLOW_RELAY.
	env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m1", "FLEET_TOKEN": "t", "FLEET_RUNTIME": "stub", "FLEET_PEERS": "m2=http://192.0.2.1:9000"})
	env.Offline = true
	if row := rowByID(t, runChecks(context.Background(), env), "principals.relay"); row.Status != statusFail {
		t.Fatalf("single-token without FLEET_ALLOW_RELAY: got %+v, want fail", row)
	}
}

func TestDoctorInboxIndex(t *testing.T) {
	dir := t.TempDir()
	bin := executable(t, dir)
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, "index")
	if err := os.Mkdir(index, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		vars map[string]string
		skip string
		want rowStatus
	}{
		{"unset under tmux is named", map[string]string{"FLEET_TMUX_BIN": bin}, "", statusWarn},
		{"stub has no inbox", map[string]string{"FLEET_RUNTIME": "stub"}, "", statusSkip},
		{"--skip", map[string]string{"FLEET_TMUX_BIN": bin}, "inbox.index", statusSkip},
		{"set to a file", map[string]string{"FLEET_TMUX_BIN": bin, "FLEET_INBOX_INDEX": file}, "", statusFail},
		{"set to a directory", map[string]string{"FLEET_TMUX_BIN": bin, "FLEET_INBOX_INDEX": index}, "", statusPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testDoctorEnv(tc.vars)
			if tc.skip != "" {
				env.Skip[tc.skip] = true
			}
			rows := runChecks(context.Background(), env)
			if row := rowByID(t, rows, "inbox.index"); row.Status != tc.want {
				t.Fatalf("got %+v, want %s", row, tc.want)
			}
			if tc.want != statusPass {
				if row := rowByID(t, rows, "inbox.mode-class"); row.Status != statusSkip {
					t.Fatalf("mode-class must skip when inbox.index does not pass: %+v", row)
				}
			}
		})
	}
}

// The #148 shape: an index that resolves but carries no permission-mode class
// uses the inbox for nothing.
func TestDoctorModeClass(t *testing.T) {
	entry := func(class string) string {
		b, _ := json.Marshal(map[string]string{
			"socket": "s", "token_path": "/nonexistent/token-never-read", "started_at": "2026-01-01T00:00:00Z", "mode_class": class,
		})
		return string(b)
	}
	cases := []struct {
		name    string
		entries map[string]string
		want    rowStatus
	}{
		{"all attestable", map[string]string{"1.json": entry("bypass"), "2.json": entry("prompting")}, statusPass},
		{"missing class", map[string]string{"1.json": entry("bypass"), "2.json": entry("")}, statusWarn},
		{"unrecognised class", map[string]string{"1.json": entry("yolo")}, statusFail},
		{"empty index", map[string]string{"README": "not an entry"}, statusUnknown},
		{"torn entry", map[string]string{"1.json": `{"socket":`}, statusWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.entries {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if row := checkModeClass(dir); row.Status != tc.want {
				t.Fatalf("got %+v, want %s", row, tc.want)
			}
		})
	}
}

// The state.dir row answers "is this directory writable and private to its
// owner" with the platform's own vocabulary: a POSIX mode and access(2) on
// Unix, the directory's access list (privfs.Check) elsewhere. The rows that do
// not depend on that vocabulary run everywhere; the rest are written twice, so
// neither platform's answer is asserted through the other's mechanism.
func TestDoctorStateDir(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	type stateCase struct {
		name     string
		dir      string
		want     rowStatus
		needUser bool
	}
	cases := []stateCase{
		{"unset", "", statusWarn, false},
		{"a file", file, statusFail, false},
		{"not yet created", filepath.Join(root, "later", "state"), statusPass, false},
	}
	if runtime.GOOS != "windows" {
		mk := func(name string, mode os.FileMode) string {
			p := filepath.Join(root, name)
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
			return p
		}
		cases = append(cases,
			stateCase{"not writable", mk("ro", 0o500), statusFail, true},
			stateCase{"too open", mk("open", 0o755), statusWarn, false},
			stateCase{"0700", mk("good", 0o700), statusPass, false},
		)
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "ro"), 0o700) })
	} else {
		// Created the way the service creates it: the owner-only protected list
		// applied at creation, which the row must read back as a pass.
		good := filepath.Join(root, "good")
		if err := privfs.MkdirAll(good); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, stateCase{"owner-only list", good, statusPass, false})
		// Created the ordinary way, it inherits the parent's list. Whether that
		// list names anyone else depends on the account and the runner's
		// temporary directory, so the expected row is whatever privfs reads —
		// and a list that happens to be owner-only already proves nothing.
		open := filepath.Join(root, "open")
		if err := os.Mkdir(open, 0o700); err != nil {
			t.Fatal(err)
		}
		switch rep := privfs.Check(open); rep.Verdict {
		case privfs.Loose:
			cases = append(cases, stateCase{"inherited list names someone else", open, statusWarn, false})
		case privfs.Unknown:
			cases = append(cases, stateCase{"inherited list unreadable", open, statusUnknown, false})
		default:
			t.Logf("the runner's temporary directory is already owner-only; the inherited-list row is not exercised (windows, #303)")
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needUser && os.Geteuid() == 0 {
				t.Skip("root writes through 0500")
			}
			if row := checkStateDir(tc.dir); row.Status != tc.want {
				t.Fatalf("got %+v, want %s", row, tc.want)
			}
		})
	}
}

func TestDoctorTokenSource(t *testing.T) {
	rows := runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_RUNTIME": "stub"}))
	if row := rowByID(t, rows, "token.source"); row.Status != statusFail {
		t.Fatalf("no token, no table: got %+v, want fail", row)
	}
	rows = runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_RUNTIME": "stub", "FLEET_TOKEN": "t"}))
	if row := rowByID(t, rows, "token.source"); row.Status != statusPass {
		t.Fatalf("token: got %+v, want pass", row)
	}
}

func TestDoctorConfigLoadFailureHidesPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"principals": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_CONFIG": path, "FLEET_RUNTIME": "stub"}))
	row := rowByID(t, rows, "config.load")
	if row.Status != statusFail {
		t.Fatalf("got %+v, want fail", row)
	}
	if strings.Contains(row.Detail+row.Summary, dir) {
		t.Fatalf("row leaks the config path: %+v", row)
	}
	if row := rowByID(t, rows, "token.source"); row.Status != statusSkip {
		t.Fatalf("token.source must not decide on a config that failed: %+v", row)
	}

	// An unknown grant is the startup failure enrolment was built to prevent.
	cfg := writeTestConfig(t, t.TempDir(), []testPrincipal{{Name: "x", Token: "t", Grants: grants("read,superuser")}}, nil)
	if row := rowByID(t, runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_CONFIG": cfg})), "config.load"); row.Status != statusFail {
		t.Fatalf("unknown grant: got %+v, want fail", row)
	}
}

// #98: a table-only deployment has nothing to present for its own peer reads
// unless the table names system:<self>.
func TestDoctorSelfCredential98(t *testing.T) {
	peers := []testPeer{{Machine: "m2", URL: "http://192.0.2.1:9000", Token: "p"}}
	without := writeTestConfig(t, t.TempDir(), []testPrincipal{{Name: "sup", Token: "a", Grants: grants("read")}}, peers)
	with := writeTestConfig(t, t.TempDir(), []testPrincipal{
		{Name: "sup", Token: "a", Grants: grants("read")},
		{Name: "system:m1", Token: "s", Grants: grants("read")},
	}, peers)
	for cfg, want := range map[string]rowStatus{without: statusFail, with: statusPass} {
		env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_RUNTIME": "stub"})
		env.Offline = true
		if row := rowByID(t, runChecks(context.Background(), env), "peer.self-credential"); row.Status != want {
			t.Fatalf("got %+v, want %s", row, want)
		}
	}
}

func TestDoctorBindAddr(t *testing.T) {
	cases := map[string]rowStatus{
		"":               statusWarn,
		":0":             statusWarn,
		"0.0.0.0:9000":   statusWarn,
		"garbage":        statusFail,
		"10.0.0.5:9000":  statusPass,
		"127.0.0.1:9000": statusPass,
	}
	for addr, want := range cases {
		row := checkBind(addr)
		if row.Status != want {
			t.Errorf("%q: got %+v, want %s", addr, row, want)
		}
		if strings.Contains(row.Summary+row.Detail, "10.0.0.5") {
			t.Errorf("%q: row echoes the address: %+v", addr, row)
		}
	}
	if row := checkBind("127.0.0.1:9000"); !strings.Contains(row.Summary, "loopback-only") {
		t.Errorf("loopback bind must say so: %+v", row)
	}
}

func TestDoctorRuntime(t *testing.T) {
	bin := executable(t, t.TempDir())
	notFound := func(string) (string, error) { return "", os.ErrNotExist }
	found := func(string) (string, error) { return "tmux", nil }
	cases := []struct {
		name, runtime, bin string
		look               func(string) (string, error)
		want               rowStatus
		needsExecBit       bool
	}{
		{"stub", "stub", "", notFound, statusPass, false},
		{"explicit binary", "tmux", bin, notFound, statusPass, true},
		{"explicit binary missing", "tmux", bin + ".missing", found, statusFail, false},
		{"only on shell PATH", "tmux", "", found, statusWarn, false},
		{"bare name on PATH", "tmux", "tmux", found, statusWarn, false},
		{"bare name nowhere", "tmux", "tmux", notFound, statusFail, false},
		{"nowhere", "tmux", "", notFound, statusFail, false},
		{"unknown runtime", "screen", "", found, statusFail, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsExecBit {
				execBitRowsSkipOnWindows(t)
			}
			if row := checkRuntimeOn("linux", tc.runtime, tc.bin, tc.look); row.Status != tc.want {
				t.Errorf("got %+v, want %s", row, tc.want)
			}
		})
	}
}

func TestDoctorPeerReachable(t *testing.T) {
	var gotAuth string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"build":{"known":false}}`))
	}))
	defer ok.Close()
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer refused.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	check := func(url string, offline bool) doctorRow {
		cfg := writeTestConfig(t, t.TempDir(), []testPrincipal{
			{Name: "sup", Token: "a", Grants: grants(supervisorList + ",relay")},
			{Name: "system:m1", Token: "s", Grants: grants("read")},
		}, []testPeer{{Machine: "m2", URL: url, Token: "peer-secret"}})
		env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_RUNTIME": "stub"})
		env.Offline = offline
		return rowByID(t, runChecks(context.Background(), env), "peer.m2.reachable")
	}

	if row := check(ok.URL, false); row.Status == statusFail || !strings.Contains(row.Summary, "credential accepted") {
		t.Fatalf("200: got %+v", row)
	}
	if gotAuth != "Bearer peer-secret" {
		t.Fatalf("probe presented %q, want the per-peer credential", gotAuth)
	}
	if row := check(refused.URL, false); row.Status != statusFail || !strings.Contains(row.Summary, "refused") {
		t.Fatalf("401: got %+v", row)
	}
	if row := check(closedURL, false); row.Status != statusFail || !strings.Contains(row.Summary, "unreachable") {
		t.Fatalf("closed: got %+v", row)
	}
	if row := check(ok.URL, true); row.Status != statusUnknown {
		t.Fatalf("offline: got %+v", row)
	}
}

// The triage ruling for #160: whether a peer grants this machine keys is not
// readable locally until #154. The row ships now and says so — never a guess.
func TestDoctorPeerGrantsUnknownWhenOffline(t *testing.T) {
	cfg := writeTestConfig(t, t.TempDir(), []testPrincipal{{Name: "system:m1", Token: "s", Grants: grants("read")}},
		[]testPeer{{Machine: "m2", URL: "http://192.0.2.1:9000", Token: "p"}, {Machine: "m3", URL: "http://192.0.2.2:9000", Token: "q"}})
	env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_RUNTIME": "stub"})
	env.Offline = true
	rows := runChecks(context.Background(), env)
	for _, id := range []string{"peer.m2.grants", "peer.m3.grants"} {
		row := rowByID(t, rows, id)
		if row.Status != statusUnknown || len(row.Refs) == 0 || row.Refs[0] != 154 {
			t.Fatalf("%s: got %+v, want unknown citing #154", id, row)
		}
	}
}

func TestDoctorNeverPrintsSecretsOrPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	index := filepath.Join(dir, "index")
	if err := os.Mkdir(index, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(index, "1.json"), []byte(`{"socket":"s","token_path":"/x","started_at":"2026-01-01T00:00:00Z","mode_class":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeTestConfig(t, dir, []testPrincipal{
		{Name: "sup", Token: "SECRET-principal-token", Grants: grants("read")},
	}, []testPeer{{Machine: "m2", URL: srv.URL, Token: "SECRET-peer-token"}})
	vars := map[string]string{
		"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_TOKEN": "SECRET-env-token",
		"FLEET_STATE_DIR": state, "FLEET_INBOX_INDEX": index, "FLEET_TMUX_BIN": executable(t, dir),
		"FLEET_ADDR": "10.0.0.5:9000",
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, args := range [][]string{nil, {"--json"}} {
		_, out, errOut := runDoctorTest(vars, args...)
		for _, leak := range []string{"SECRET-", dir, host, "10.0.0.5"} {
			if strings.Contains(out+errOut, leak) {
				t.Fatalf("output %v contains %q:\n%s", args, leak, out)
			}
		}
	}
}

func TestDoctorIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "index")
	if err := os.Mkdir(index, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(index, "1.json"), []byte(`{"socket":"s","token_path":"/x","started_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeTestConfig(t, dir, []testPrincipal{{Name: "sup", Token: "t", Grants: grants("read")}}, nil)
	state := filepath.Join(dir, "state-not-created")

	snapshot := func() string {
		var b strings.Builder
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// os.Stat, not d.Info(): on Windows a DirEntry carries the metadata
			// the PARENT's listing cached, which lags a child created since —
			// the directory's own mtime reads old until something refreshes
			// it, and a snapshot taken twice then disagrees with itself.
			info, err := os.Stat(p)
			if err != nil {
				return err
			}
			b.WriteString(p + " " + info.Mode().String() + " " + info.ModTime().String() + "\n")
			return nil
		})
		return b.String()
	}
	before := snapshot()
	runDoctorTest(map[string]string{
		"FLEET_CONFIG": cfg, "FLEET_STATE_DIR": state, "FLEET_INBOX_INDEX": index,
		"FLEET_TMUX_BIN": "/nonexistent",
	}, "--offline")
	if after := snapshot(); after != before {
		t.Fatalf("doctor changed the filesystem:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("doctor created the state directory (err=%v)", err)
	}
}

func TestDoctorExitCodes(t *testing.T) {
	warnOnly := map[string]string{
		"FLEET_MACHINE": "m1", "FLEET_TOKEN": "t", "FLEET_RUNTIME": "stub",
		"FLEET_ADDR": "127.0.0.1:9000", "FLEET_ALLOW_MUTATIONS": "1",
	}
	if code, out, _ := runDoctorTest(warnOnly); code != 0 {
		t.Fatalf("warn-only: exit %d, want 0\n%s", code, out)
	}
	if code, _, _ := runDoctorTest(map[string]string{"FLEET_RUNTIME": "stub"}); code != 1 {
		t.Fatalf("no token: exit %d, want 1", code)
	}
	if code, _, _ := runDoctorTest(warnOnly, "--bogus"); code != 2 {
		t.Fatalf("unknown flag: exit %d, want 2", code)
	}
	if code, _, errOut := runDoctorTest(warnOnly, "--skip=inbox.idx"); code != 2 || !strings.Contains(errOut, "inbox.idx") {
		t.Fatalf("a --skip naming no row must be a usage error: exit %d, %q", code, errOut)
	}
	if code, out, _ := runDoctorTest(warnOnly, "--help"); code != 0 || !strings.Contains(out, "usage") {
		t.Fatalf("--help: exit %d", code)
	}
	if handled, _ := runDoctor([]string{"principal", "list"}, os.Getenv, &bytes.Buffer{}, &bytes.Buffer{}); handled {
		t.Fatal("doctor must not consume other subcommands")
	}
}

func TestDoctorJSONShape(t *testing.T) {
	_, out, _ := runDoctorTest(map[string]string{"FLEET_RUNTIME": "stub", "FLEET_PEERS": "m2=http://192.0.2.1:9000", "FLEET_TOKEN": "t", "FLEET_MACHINE": "m1"}, "--json", "--offline")
	var doc struct {
		Build   string            `json:"build"`
		Machine string            `json:"machine"`
		Rows    []doctorRow       `json:"rows"`
		Counts  map[rowStatus]int `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if doc.Machine != "m1" || len(doc.Rows) == 0 {
		t.Fatalf("unexpected document: %+v", doc)
	}
	seen := map[string]bool{}
	total := 0
	for _, r := range doc.Rows {
		if seen[r.ID] {
			t.Fatalf("duplicate row id %q", r.ID)
		}
		seen[r.ID] = true
	}
	for _, n := range doc.Counts {
		total += n
	}
	if total != len(doc.Rows) {
		t.Fatalf("counts sum to %d, rows are %d", total, len(doc.Rows))
	}
}

// muster #297 (o): the runtime.opencode row names what the install can be
// asked without running it, and never runs the binary.
func TestDoctorOpencodeRuntimeRow(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "was-run")
	// A binary that records being executed: the doctor must never do that —
	// whether it answers in time is the running service's to report.
	runnable := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(runnable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	notRunnable := filepath.Join(dir, "opencode-noexec")
	if err := os.WriteFile(notRunnable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmuxBin := executable(t, dir)

	cases := []struct {
		name         string
		vars         map[string]string
		path         string
		want         rowStatus
		needsExecBit bool
	}{
		{"not enabled", map[string]string{"FLEET_TMUX_BIN": tmuxBin}, "", statusSkip, false},
		{"enabled, absolute path to an executable", map[string]string{
			"FLEET_TMUX_BIN": tmuxBin, "FLEET_RUNTIME_OPENCODE": "1", "FLEET_OPENCODE_BIN": runnable}, "", statusPass, true},
		{"enabled, absolute path that is missing", map[string]string{
			"FLEET_TMUX_BIN": tmuxBin, "FLEET_RUNTIME_OPENCODE": "1", "FLEET_OPENCODE_BIN": filepath.Join(dir, "nope")}, "", statusFail, false},
		{"enabled, a file that cannot be run", map[string]string{
			"FLEET_TMUX_BIN": tmuxBin, "FLEET_RUNTIME_OPENCODE": "1", "FLEET_OPENCODE_BIN": notRunnable}, "", statusFail, true},
		{"enabled, bare name found on this shell's PATH", map[string]string{
			"FLEET_TMUX_BIN": tmuxBin, "FLEET_RUNTIME_OPENCODE": "1"}, dir, statusWarn, true},
		{"enabled, bare name found nowhere", map[string]string{
			"FLEET_TMUX_BIN": tmuxBin, "FLEET_RUNTIME_OPENCODE": "1"}, t.TempDir(), statusFail, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsExecBit {
				execBitRowsSkipOnWindows(t)
			}
			if tc.path != "" {
				t.Setenv("PATH", tc.path)
			}
			rows := runChecks(context.Background(), testDoctorEnv(tc.vars))
			row := rowByID(t, rows, "runtime.opencode")
			if row.Status != tc.want {
				t.Fatalf("got %+v, want %s", row, tc.want)
			}
			// Paste-safe: a row names the variable, never the path.
			if strings.Contains(row.Summary+row.Detail, dir) {
				t.Errorf("row carries a filesystem path: %+v", row)
			}
		})
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the doctor executed the opencode binary")
	}
}
