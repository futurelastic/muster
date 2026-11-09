package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
	if runtime.GOOS == "windows" {
		// The fixture lays out a unix home and a fake executable by mode bits;
		// the command itself refuses Windows, which a test below covers.
		t.Skip("per-user service units are written on macOS and Linux")
	}
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
	s := svcSettings{Machine: "m", Addr: "127.0.0.1:1", Tmux: `/t m/tmux`, Carried: [][2]string{
		{"FLEET_OPENCODE_BIN", `/o p/"q"/50%/open&code`}, {"FLEET_OPENCODE_PROVIDERS", "deepseek,!x/<y>"}, {"LANG", "en_US.UTF-8"},
	}}
	for goos, render := range map[string]func(svcPaths, svcSettings) string{"linux": renderSystemdUnit, "darwin": renderLaunchdPlist} {
		got, bin := parseUnit(goos, render(p, s))
		if !reflect.DeepEqual(got, s) || bin != p.Bin {
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

// ---- muster #447: install keeps what an operator put in the unit -------------

// operatorVar is one variable an operator adds to the unit by hand.
type operatorVar struct{ name, value string }

// addToUnit adds variables to the unit install wrote, the way a person would:
// as plist keys on macOS, and on Linux as Environment= lines in the three styles
// systemd takes (quoted, bare, several on one line).
func addToUnit(t *testing.T, f *svcFixture, vars ...operatorVar) {
	t.Helper()
	raw, err := os.ReadFile(f.p.Unit)
	if err != nil {
		t.Fatal(err)
	}
	var add, anchor string
	switch f.env.GOOS {
	case "darwin":
		anchor = "\t</dict>\n\t<key>LimitLoadToSessionType</key>"
		for _, v := range vars {
			add += "\t\t<key>" + v.name + "</key>\n\t\t<string>" + xmlEscape(v.value) + "</string>\n"
		}
	default:
		anchor = "KillMode=process"
		for i, v := range vars {
			switch i % 3 {
			case 0:
				add += `Environment="` + v.name + "=" + v.value + `"` + "\n"
			case 1:
				add += "Environment=" + v.name + "=" + v.value + "\n"
			default:
				add += "Environment=" + v.name + "=" + v.value + ` "ZZ_` + v.name + `=ignored"` + "\n"
			}
		}
	}
	if !strings.Contains(string(raw), anchor) {
		t.Fatalf("the unit has no %q to add before:\n%s", anchor, raw)
	}
	out := strings.Replace(string(raw), anchor, add+anchor, 1)
	if err := os.WriteFile(f.p.Unit, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeOpencode is a binary that answers the version probe the way the runtime's
// driver asks, so a start with the runtime switched on finds it installed.
func fakeOpencode(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// An update runs `service install` with no flags. Everything the operator added
// to the unit that the command does not own comes back in the unit it writes: the
// switches that turn a runtime on, the locale. Flags still win over the earlier
// value of what they set, the first-install default is untouched, and a variable
// install writes itself is never doubled.
func TestServiceInstallKeepsOperatorVariables(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			if code := f.install(t, "--machine", "first"); code != 0 {
				t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
			}
			oc := fakeOpencode(t)
			kept := []operatorVar{
				{"FLEET_RUNTIME_OPENCODE", "1"}, {"FLEET_OPENCODE_BIN", oc}, {"FLEET_OPENCODE_PROVIDERS", "deepseek,opencode/deepseek-v4-pro"},
				{"FLEET_OPENCODE_IDLE_STOP", "30m"}, {"FLEET_INBOX_INDEX", "/var/inbox"}, {"LANG", "en_US.UTF-8"}, {"LC_ALL", "en_US.UTF-8"},
			}
			addToUnit(t, f, kept...)
			f.mgr.reset()

			// No flags: the update path.
			if code := f.run(t, "install"); code != 0 {
				t.Fatalf("update: %d\n%s%s", code, f.out, f.errb)
			}
			unit, _ := os.ReadFile(f.p.Unit)
			for _, v := range kept {
				want := `Environment="` + v.name + "=" + v.value + `"`
				if goos == "darwin" {
					want = "<key>" + v.name + "</key>\n\t\t<string>" + v.value + "</string>"
				}
				if !strings.Contains(string(unit), want) {
					t.Errorf("the update dropped %s:\n%s", v.name, unit)
				}
			}
			if strings.Contains(string(unit), "ignored") {
				t.Errorf("a variable outside FLEET_* and LANG/LC_* was carried:\n%s", unit)
			}
			// The earlier choices stayed too, once each.
			for _, name := range []string{"FLEET_MACHINE", "FLEET_ADDR", "FLEET_STATE_DIR", "FLEET_CONFIG", "FLEET_TMUX_BIN"} {
				if n := strings.Count(string(unit), name); n != 1 {
					t.Errorf("%s appears %d times in the unit:\n%s", name, n, unit)
				}
			}
			if !strings.Contains(string(unit), "first") {
				t.Errorf("the machine id chosen earlier was lost:\n%s", unit)
			}
			// doctor ran under the unit's whole environment, the carried part included.
			for _, v := range kept {
				if !contains(f.mgr.doctorEnv, v.name+"="+v.value) {
					t.Errorf("doctor did not run with %s=%s: %v", v.name, v.value, f.mgr.doctorEnv)
				}
			}
			if !strings.Contains(f.out.String(), "carried   7 variable(s)") {
				t.Errorf("install did not say what it kept:\n%s", f.out)
			}
			if strings.Contains(f.out.String(), "/var/inbox") || strings.Contains(f.out.String(), "deepseek") {
				t.Errorf("install printed a carried value:\n%s", f.out)
			}

			// A second run is a no-op, then a flag wins over the earlier value only
			// for what it sets.
			f.mgr.reset()
			if code := f.run(t, "install"); code != 0 || !strings.Contains(f.out.String(), "Nothing to change") {
				t.Fatalf("a rerun: %d\n%s%s", code, f.out, f.errb)
			}
			if code := f.run(t, "install", "--machine", "second"); code != 0 {
				t.Fatalf("flagged: %d\n%s%s", code, f.out, f.errb)
			}
			unit, _ = os.ReadFile(f.p.Unit)
			if !strings.Contains(string(unit), "second") || strings.Contains(string(unit), "first") {
				t.Errorf("--machine did not win:\n%s", unit)
			}
			if !strings.Contains(string(unit), "FLEET_OPENCODE_IDLE_STOP") || !strings.Contains(string(unit), "LC_ALL") {
				t.Errorf("a flag took the carried set with it:\n%s", unit)
			}

			// status lists the names and leaves the values in the unit.
			if code := f.run(t, "status"); code != 0 {
				t.Fatalf("status: %d\n%s%s", code, f.out, f.errb)
			}
			if !strings.Contains(f.out.String(), "FLEET_OPENCODE_BIN") || !strings.Contains(f.out.String(), "LC_ALL") {
				t.Errorf("status does not list the carried set:\n%s", f.out)
			}
			if strings.Contains(f.out.String(), "/var/inbox") {
				t.Errorf("status printed a carried value:\n%s", f.out)
			}
		})
	}
}

// The state directory and the table are the layout's: a different value in the
// unit is not kept, and the install says it reset it instead of dropping it
// without a word.
func TestServiceInstallSaysWhenItResetsALayoutVariable(t *testing.T) {
	f := newSvcFixture(t, "linux")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	raw, _ := os.ReadFile(f.p.Unit)
	edited := strings.Replace(string(raw), `FLEET_STATE_DIR=`+f.p.StateDir, `FLEET_STATE_DIR=/elsewhere`, 1)
	if edited == string(raw) {
		t.Fatalf("no state directory line to edit:\n%s", raw)
	}
	if err := os.WriteFile(f.p.Unit, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("update: %d\n%s%s", code, f.out, f.errb)
	}
	if !strings.Contains(f.out.String(), "note      the unit sets FLEET_STATE_DIR=/elsewhere") {
		t.Errorf("no note about the reset:\n%s", f.out)
	}
	if unit, _ := os.ReadFile(f.p.Unit); !strings.Contains(string(unit), f.p.StateDir) || strings.Contains(string(unit), "/elsewhere") {
		t.Errorf("the layout's state directory was not written back:\n%s", unit)
	}
}

// systemd's own spellings of Environment= are read, an empty one clears what is
// above it, and only FLEET_* and the locale are taken.
func TestParseUnitReadsSystemdEnvironmentLikeSystemd(t *testing.T) {
	text := `[Service]
ExecStart="/bin/muster" serve
Environment="FLEET_MACHINE=gone"
Environment=
Environment="FLEET_MACHINE=m" FLEET_RUNTIME_OPENCODE=1 'LANG=en_US.UTF-8'
# Environment=FLEET_COMMENTED=1
Environment="FLEET_SPACED=a b" "NOT_OURS=1" FLEET_PCT=50%%
`
	got, bin := parseUnit("linux", text)
	want := [][2]string{{"FLEET_PCT", "50%"}, {"FLEET_RUNTIME_OPENCODE", "1"}, {"FLEET_SPACED", "a b"}, {"LANG", "en_US.UTF-8"}}
	if got.Machine != "m" || bin != "/bin/muster" || !reflect.DeepEqual(got.Carried, want) {
		t.Errorf("parsed %+v %q, want machine m and carried %v", got, bin, want)
	}
}

// The oracle of #447, whole: install, add the two variables that switch the
// opencode runtime on, install again with no flags, and then start the service
// the way the unit starts it. The runtime must still be listed, because the
// service reads only what the unit hands it.
func TestServiceInstallKeptRuntimeStillServes(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			if code := f.install(t); code != 0 {
				t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
			}
			oc := fakeOpencode(t)
			addToUnit(t, f, operatorVar{"FLEET_RUNTIME_OPENCODE", "1"}, operatorVar{"FLEET_OPENCODE_BIN", oc})
			if code := f.run(t, "install"); code != 0 {
				t.Fatalf("update: %d\n%s%s", code, f.out, f.errb)
			}

			// The unit as the manager would read it. The service is started as a child
			// of this test, so three things are set for it that a unit would not:
			// a free port, no terminal runtime (it needs tmux), and the test's marker.
			raw, _ := os.ReadFile(f.p.Unit)
			s, _ := parseUnit(goos, string(raw))
			var env []string
			for _, kv := range svcVars(f.p, s) {
				env = append(env, kv[0]+"="+kv[1])
			}
			env = append(env, "FLEET_ADDR=127.0.0.1:0", "FLEET_RUNTIME=none")
			// The unit is the service's whole environment, so nothing the machine
			// running this test exports may reach it: a machine that has the opencode
			// switch in its own environment would make this pass without the unit.
			set := map[string]bool{}
			for _, kv := range env {
				set[strings.SplitN(kv, "=", 2)[0]] = true
			}
			for _, kv := range os.Environ() {
				if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "FLEET_") && !set[name] {
					env = append(env, name+"=")
				}
			}
			_, sc := startServe(t, env...)

			var addr string
			lines := make(chan string)
			go func() {
				for sc.Scan() {
					lines <- sc.Text()
				}
				close(lines)
			}()
			deadline := time.After(20 * time.Second)
		wait:
			for {
				select {
				case l, ok := <-lines:
					if !ok {
						t.Fatal("the service exited before it listened")
					}
					if m := listeningRe.FindStringSubmatch(l); m != nil {
						addr = m[1]
						break wait
					}
				case <-deadline:
					t.Fatal("the service did not start listening")
				}
			}

			req, _ := http.NewRequest("GET", "http://"+addr+"/v1/runtimes", nil)
			req.Header.Set("Authorization", "Bearer "+readToken(f.p))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body struct {
				Items []struct {
					Runtime string `json:"runtime"`
					Status  string `json:"status"`
				} `json:"items"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != 200 {
				t.Fatalf("GET /v1/runtimes = %d, %v", resp.StatusCode, err)
			}
			found := false
			for _, it := range body.Items {
				if it.Runtime == "opencode" {
					found = true
					if it.Status != "available" {
						t.Errorf("opencode is listed %q after the update, want available", it.Status)
					}
				}
			}
			if !found {
				t.Errorf("/v1/runtimes does not list opencode after an update that kept its variables: %+v", body.Items)
			}
		})
	}
}
