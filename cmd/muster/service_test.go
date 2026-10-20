package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeManager stands in for launchctl and systemctl: it keeps the two bits the
// installer reads (registered, running) and moves them the way the real
// commands do, and it records every command so a test can assert on the
// sequence. The doctor run (the installed binary invoked with "doctor") is
// answered here too, with the environment it was given.
type fakeManager struct {
	mu         sync.Mutex
	goos       string
	loaded     bool
	active     bool
	calls      []string
	doctorEnv  []string
	doctorArgs []string
	doctorFail bool
	failOn     string // a substring of a command line that fails
}

func (f *fakeManager) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	if len(args) > 0 && args[0] == "doctor" {
		f.calls = append(f.calls, "doctor")
		f.doctorEnv, f.doctorArgs = env, args
		if f.doctorFail {
			return []byte("FAIL     config.load   broken\n"), errors.New("exit status 1")
		}
		return []byte("PASS     config.load   ok\n"), nil
	}
	if len(args) > 0 && args[0] == "--version" {
		return []byte("muster test-build (abc)\n"), nil
	}
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		return []byte("boom"), errors.New("exit status 1")
	}
	// Reads are not part of the sequence a test asserts on.
	read := strings.Contains(line, "launchctl print") || strings.Contains(line, "is-enabled") || strings.Contains(line, "is-active")
	if !read {
		f.calls = append(f.calls, line)
	}
	switch {
	case strings.Contains(line, "launchctl print"):
		if !f.loaded {
			return nil, errors.New("not found")
		}
		if f.active {
			return []byte("state = running\n"), nil
		}
		return []byte("state = waiting\n"), nil
	case strings.Contains(line, "launchctl bootstrap"), strings.Contains(line, "enable --now"):
		f.loaded, f.active = true, true
	case strings.Contains(line, "launchctl bootout"), strings.Contains(line, "disable --now"):
		f.loaded, f.active = false, false
	case strings.Contains(line, "launchctl kickstart"), strings.Contains(line, "--user start"), strings.Contains(line, "--user restart"):
		f.active = true
	case strings.Contains(line, "is-enabled"):
		if !f.loaded {
			return nil, errors.New("disabled")
		}
	case strings.Contains(line, "is-active"):
		if !f.active {
			return nil, errors.New("inactive")
		}
	}
	return nil, nil
}

func (f *fakeManager) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeManager) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

type svcFixture struct {
	env  svcEnv
	mgr  *fakeManager
	p    svcPaths
	out  *bytes.Buffer
	errb *bytes.Buffer
	srv  *httptest.Server
	exe  string
	tmux string
}

// newSvcFixture builds a home, a running binary to install, a fake tmux and a
// health endpoint that answers 200 only with the supervisor's token.
func newSvcFixture(t *testing.T, goos string) *svcFixture {
	t.Helper()
	home := t.TempDir()
	dir := t.TempDir()
	exe := filepath.Join(dir, "muster-src")
	if err := os.WriteFile(exe, []byte("binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &svcFixture{mgr: &fakeManager{goos: goos}, out: &bytes.Buffer{}, errb: &bytes.Buffer{}, exe: exe, tmux: tmux}
	var token func() string
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" || r.Header.Get("Authorization") != "Bearer "+token() {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"build":{"version":"v9.9.9"}}`))
	}))
	t.Cleanup(f.srv.Close)
	p, err := svcLayout(goos, home)
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	token = func() string { return readToken(p) }
	f.env = svcEnv{
		GOOS: goos, Home: home, UID: 501, Exe: exe, Hostname: "Test-Host.example",
		LookPath: func(string) (string, error) { return "", errors.New("not on PATH") },
		Run:      f.mgr.run,
		HTTP:     f.srv.Client(),
		Out:      f.out, Err: f.errb,
		HealthWait: 2 * time.Second, HealthPoll: 10 * time.Millisecond,
	}
	return f
}

func (f *svcFixture) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *svcFixture) run(t *testing.T, args ...string) int {
	t.Helper()
	f.out.Reset()
	f.errb.Reset()
	return runServiceWith(context.Background(), f.env, args)
}

func (f *svcFixture) install(t *testing.T, extra ...string) int {
	t.Helper()
	return f.run(t, append([]string{"install", "--addr", f.addr(), "--tmux", f.tmux}, extra...)...)
}

func TestServiceInstallLinuxFirstRunThenNoChange(t *testing.T) {
	f := newSvcFixture(t, "linux")
	if code := f.install(t); code != 0 {
		t.Fatalf("first install exit %d\nout: %s\nerr: %s", code, f.out, f.errb)
	}
	// The binary is at the fixed path, executable, byte for byte.
	if b, err := os.ReadFile(f.p.Bin); err != nil || string(b) != "binary v1" {
		t.Fatalf("installed binary: %q %v", b, err)
	}
	if st, _ := os.Stat(f.p.Bin); st.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v", st.Mode().Perm())
	}
	if st, err := os.Stat(f.p.StateDir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("state dir: %v %v", st, err)
	}
	// The table has the supervisor, and the token file is private.
	c, err := loadConfig(f.p.Config)
	if err != nil || len(c.Principals) != 1 || c.Principals[0].Name != svcPrincipal {
		t.Fatalf("table: %+v %v", c, err)
	}
	if st, _ := os.Stat(f.p.Token); st.Mode().Perm() != 0o600 {
		t.Errorf("token mode %v", st.Mode().Perm())
	}
	unit, _ := os.ReadFile(f.p.Unit)
	for _, want := range []string{
		`ExecStart="` + f.p.Bin + `" serve`,
		`Environment="FLEET_ADDR=` + f.addr() + `"`,
		`Environment="FLEET_MACHINE=test-host"`,
		`Environment="FLEET_TMUX_BIN=` + f.tmux + `"`,
		"KillMode=process", "Restart=on-failure", "WantedBy=default.target",
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(string(unit), "FLEET_TOKEN") {
		t.Errorf("no secret belongs in the unit:\n%s", unit)
	}
	// doctor ran under the unit's environment and nothing else.
	if got := strings.Join(f.mgr.doctorEnv, " "); got != strings.Join(svcEnviron(f.p, svcSettings{Machine: "test-host", Addr: f.addr(), Tmux: f.tmux}), " ") {
		t.Errorf("doctor environment %q", got)
	}
	if !contains(f.mgr.doctorArgs, "--principal=supervisor") {
		t.Errorf("doctor args %v", f.mgr.doctorArgs)
	}
	if got := withoutReload(f.mgr.sequence()); strings.Join(got, "|") != "doctor|systemctl --user enable --now muster.service" {
		t.Errorf("sequence %v", got)
	}
	if !strings.Contains(f.out.String(), "health    ok") || !strings.Contains(f.out.String(), "v9.9.9") {
		t.Errorf("no health line:\n%s", f.out)
	}

	// A second run changes nothing and says so.
	f.mgr.reset()
	tokBefore, _ := os.ReadFile(f.p.Token)
	if code := f.install(t); code != 0 {
		t.Fatalf("second install exit %d\n%s%s", code, f.out, f.errb)
	}
	if !strings.Contains(f.out.String(), "Nothing to change") {
		t.Errorf("second run did not say it changed nothing:\n%s", f.out)
	}
	if got := f.mgr.sequence(); len(got) != 1 || got[0] != "doctor" {
		t.Errorf("second run touched the manager: %v", got)
	}
	if tokAfter, _ := os.ReadFile(f.p.Token); !bytes.Equal(tokBefore, tokAfter) {
		t.Error("the second run replaced the token")
	}
}

// withoutReload drops the daemon-reload the real run issues after writing a
// unit: it is needed then and not otherwise, and the order around it is what
// the test is about.
func withoutReload(seq []string) []string {
	var out []string
	for _, c := range seq {
		if !strings.Contains(c, "daemon-reload") {
			out = append(out, c)
		}
	}
	return out
}

func TestServiceInstallAfterUpgradeUpdatesBinaryAndRestarts(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			if code := f.install(t); code != 0 {
				t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
			}
			if err := os.WriteFile(f.exe, []byte("binary v2"), 0o755); err != nil {
				t.Fatal(err)
			}
			f.mgr.reset()
			if code := f.install(t); code != 0 {
				t.Fatalf("upgrade install: %d\n%s%s", code, f.out, f.errb)
			}
			if b, _ := os.ReadFile(f.p.Bin); string(b) != "binary v2" {
				t.Errorf("binary not updated: %q", b)
			}
			if !strings.Contains(f.out.String(), "restarted") {
				t.Errorf("no restart reported:\n%s", f.out)
			}
			seq := strings.Join(f.mgr.sequence(), "\n")
			restart := "systemctl --user restart muster.service"
			if goos == "darwin" {
				restart = "launchctl bootstrap user/501 " + f.p.Unit
			}
			if !strings.Contains(seq, restart) {
				t.Errorf("sequence lacks %q:\n%s", restart, seq)
			}
			if f.mgr.loaded != true || f.mgr.active != true {
				t.Error("the service is not running after the upgrade")
			}
		})
	}
}

func TestServiceInstallKeepsAnExistingTable(t *testing.T) {
	f := newSvcFixture(t, "linux")
	if err := os.MkdirAll(f.p.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := `{"principals":[{"name":"mine","token":"abc","grants":["read"]}]}`
	if err := os.WriteFile(f.p.Config, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.install(t, "--no-start"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, f.out, f.errb)
	}
	if b, _ := os.ReadFile(f.p.Config); string(b) != mine {
		t.Errorf("the existing table was rewritten:\n%s", b)
	}
	if _, err := os.Stat(f.p.Token); err == nil {
		t.Error("a token file was minted beside someone else's table")
	}
	if contains(f.mgr.doctorArgs, "--principal=supervisor") {
		t.Errorf("doctor asked about a principal the table does not have: %v", f.mgr.doctorArgs)
	}
	if !strings.Contains(f.out.String(), "not loaded") {
		t.Errorf("--no-start did not say so:\n%s", f.out)
	}
	if got := f.mgr.sequence(); len(got) != 1 {
		t.Errorf("--no-start called the manager: %v", got)
	}
}

func TestServiceInstallDoesNotStartWhenDoctorFails(t *testing.T) {
	f := newSvcFixture(t, "linux")
	f.mgr.doctorFail = true
	code := f.install(t)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(f.out.String(), "FAIL     config.load") {
		t.Errorf("doctor's output was not printed:\n%s", f.out)
	}
	if !strings.Contains(f.errb.String(), "not started") {
		t.Errorf("error does not say nothing was started: %s", f.errb)
	}
	if got := f.mgr.sequence(); len(got) != 1 {
		t.Errorf("manager called after a doctor failure: %v", got)
	}
}

func TestServiceInstallReportsAServiceThatNeverBecomesHealthy(t *testing.T) {
	f := newSvcFixture(t, "linux")
	f.env.HealthWait = 100 * time.Millisecond
	// A port nothing listens on.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	code := f.run(t, "install", "--addr", dead, "--tmux", f.tmux)
	if code != 1 || !strings.Contains(f.errb.String(), "did not become healthy") || !strings.Contains(f.errb.String(), "journalctl") {
		t.Errorf("exit %d, stderr: %s", code, f.errb)
	}
}

func TestServiceInstallRefusals(t *testing.T) {
	f := newSvcFixture(t, "linux")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"wildcard bind", []string{"install", "--addr", "0.0.0.0:9470", "--tmux", f.tmux}, "not a loopback"},
		{"lan address", []string{"install", "--addr", "192.168.1.5:9470", "--tmux", f.tmux}, "not a loopback"},
		{"ephemeral port", []string{"install", "--addr", "127.0.0.1:0", "--tmux", f.tmux}, "fixed port"},
		{"no port", []string{"install", "--addr", "127.0.0.1", "--tmux", f.tmux}, "HOST:PORT"},
		{"relative tmux", []string{"install", "--tmux", "tmux"}, "absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := f.run(t, tc.args...); code != 1 || !strings.Contains(f.errb.String(), tc.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, f.errb, tc.want)
			}
		})
	}
	// Nothing was written by any refusal.
	if _, err := os.Stat(f.p.Bin); err == nil {
		t.Error("a refused install still copied the binary")
	}

	w := newSvcFixture(t, "linux")
	w.env.GOOS = "windows"
	if code := w.run(t, "install"); code != 1 || !strings.Contains(w.errb.String(), "Windows") {
		t.Errorf("windows: exit %d, stderr %q", code, w.errb)
	}
}

func TestServiceStatus(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			if code := f.run(t, "status"); code != 1 || !strings.Contains(f.out.String(), "not installed") {
				t.Errorf("before install: exit %d\n%s", code, f.out)
			}
			if code := f.install(t); code != 0 {
				t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
			}
			if code := f.run(t, "status"); code != 0 {
				t.Fatalf("status exit %d\n%s%s", code, f.out, f.errb)
			}
			for _, want := range []string{f.p.Bin, "muster test-build", "v9.9.9", "loaded", "running"} {
				if !strings.Contains(f.out.String(), want) {
					t.Errorf("status lacks %q:\n%s", want, f.out)
				}
			}
			f.mgr.active = false
			if code := f.run(t, "status"); code != 1 || !strings.Contains(f.out.String(), "NOT OK") {
				t.Errorf("a stopped service: exit %d\n%s", code, f.out)
			}
		})
	}
}

func TestServiceUninstallKeepsDataUnlessPurged(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			if code := f.install(t); code != 0 {
				t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
			}
			if code := f.run(t, "uninstall"); code != 0 {
				t.Fatalf("uninstall exit %d\n%s%s", code, f.out, f.errb)
			}
			if _, err := os.Stat(f.p.Unit); err == nil {
				t.Error("the unit is still there")
			}
			if f.mgr.loaded {
				t.Error("the unit is still loaded")
			}
			for _, keep := range []string{f.p.Config, f.p.Token, f.p.StateDir, f.p.Bin} {
				if _, err := os.Stat(keep); err != nil {
					t.Errorf("uninstall removed %s", keep)
				}
			}
			// Again: nothing to do, and no error.
			if code := f.run(t, "uninstall"); code != 0 || !strings.Contains(f.out.String(), "absent") {
				t.Errorf("second uninstall: exit %d\n%s", code, f.out)
			}
			if code := f.run(t, "uninstall", "--purge"); code != 0 {
				t.Fatalf("purge exit %d\n%s%s", code, f.out, f.errb)
			}
			for _, gone := range []string{f.p.ConfigDir, f.p.StateDir, f.p.Bin} {
				if _, err := os.Stat(gone); err == nil {
					t.Errorf("--purge left %s", gone)
				}
			}
		})
	}
}

// A rerun without flags keeps what the unit already says, so a machine id or
// address an operator chose is not undone by the next upgrade.
func TestServiceInstallKeepsEarlierChoices(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	if code := f.run(t, "install", "--addr", f.addr(), "--tmux", f.tmux, "--machine", "chosen", "--no-start"); code != 0 {
		t.Fatalf("%d\n%s%s", code, f.out, f.errb)
	}
	if code := f.run(t, "install", "--no-start"); code != 0 {
		t.Fatalf("%d\n%s%s", code, f.out, f.errb)
	}
	if !strings.Contains(f.out.String(), "unit      unchanged") || !strings.Contains(f.out.String(), `machine "chosen"`) {
		t.Errorf("a rerun changed the unit:\n%s", f.out)
	}
	unit, _ := os.ReadFile(f.p.Unit)
	if !strings.Contains(string(unit), "<string>chosen</string>") {
		t.Errorf("unit lost the machine id:\n%s", unit)
	}
}

func TestUnitsRoundTripAwkwardPaths(t *testing.T) {
	p := svcPaths{Bin: `/home/a b/"q"/50%/muster`, ConfigDir: "/c", Config: `/c/x&y.json`, StateDir: "/s", Log: `/s/<l>.log`}
	s := svcSettings{Machine: "m", Addr: "127.0.0.1:1", Tmux: `/t m/tmux`}
	for goos, render := range map[string]func(svcPaths, svcSettings) string{"linux": renderSystemdUnit, "darwin": renderLaunchdPlist} {
		got, bin := parseUnit(goos, render(p, s))
		if got != s || bin != p.Bin {
			t.Errorf("%s: parsed %+v %q, want %+v %q", goos, got, bin, s, p.Bin)
		}
	}
	if !strings.Contains(renderLaunchdPlist(p, s), "x&amp;y.json") || !strings.Contains(renderLaunchdPlist(p, s), "&lt;l&gt;") {
		t.Error("the plist did not escape XML")
	}
}

func TestShortHost(t *testing.T) {
	for in, want := range map[string]string{
		"Mac-Studio.local": "mac-studio", "my_box": "my-box", "": "local", "...": "local", "Ünï": "n",
	} {
		if got := shortHost(in); got != want {
			t.Errorf("shortHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunServiceClaimsOnlyItsOwnWord(t *testing.T) {
	if handled, _ := runService([]string{"doctor"}, &bytes.Buffer{}, &bytes.Buffer{}); handled {
		t.Error("runService claimed doctor")
	}
	var out, errb bytes.Buffer
	f := newSvcFixture(t, "linux")
	f.env.Out, f.env.Err = &out, &errb
	if code := runServiceWith(context.Background(), f.env, []string{"bogus"}); code != 2 {
		t.Errorf("unknown command exit %d", code)
	}
	if code := runServiceWith(context.Background(), f.env, nil); code != 2 {
		t.Errorf("no command exit %d", code)
	}
	if code := runServiceWith(context.Background(), f.env, []string{"-h"}); code != 0 || !strings.Contains(out.String(), "muster service install") {
		t.Errorf("help: exit %d %q", code, out.String())
	}
	if code := runServiceWith(context.Background(), f.env, []string{"status", "extra"}); code != 2 {
		t.Errorf("stray argument exit %d", code)
	}
	if !strings.Contains(usageTop(), "service") {
		t.Error("usageTop does not name the service command")
	}
}
