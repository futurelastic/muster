package main

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// muster #480: on a Mac with a GUI login launchd restores only gui/<uid>, so a
// Background unit is not back after a reboot, and a copy that does land in
// gui/<uid> makes the next bootstrap into user/<uid> fail. These tests run the
// real install, restore, status and uninstall against a fake launchctl that
// refuses what launchd refuses (see fakeManager).

// seq is the manager commands since the last reset, doctor and reads left out.
func (f *svcFixture) seq() string { return strings.Join(f.mgr.sequence(), "\n") }

func TestInstallWritesALoginItemThatRunsRestoreAtLogin(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	got, err := os.ReadFile(f.p.LoginUnit)
	if err != nil {
		t.Fatalf("no login item: %v", err)
	}
	item := string(got)
	// It calls launchctl itself and not the muster binary: after a rollback that binary
	// is the previous release's and has no `restore`, and a login item that depended on
	// it would stop working with it. The bootstrap is guarded, so a login with the unit
	// already loaded is a no-op.
	for _, want := range []string{
		"<string>" + svcLoginLabel + "</string>",
		"<string>/bin/sh</string>\n\t\t<string>-c</string>",
		`/bin/launchctl print "user/$uid/` + svcLabel + `" &gt;/dev/null 2&gt;&amp;1 || `,
		`exec /bin/launchctl bootstrap "user/$uid" '` + f.p.Unit + `'`,
		"<key>RunAtLoad</key>\n\t<true/>",
	} {
		if !strings.Contains(item, want) {
			t.Errorf("login item lacks %q:\n%s", want, item)
		}
	}
	// What makes launchd load it into gui/<uid> at login is the ABSENCE of the
	// session-type limit; and it must run once, not be kept alive.
	for _, bad := range []string{"LimitLoadToSessionType", "KeepAlive", "AbandonProcessGroup"} {
		if strings.Contains(item, bad) {
			t.Errorf("login item must not carry %s:\n%s", bad, item)
		}
	}
	// The unit itself is still a Background job.
	if unit, _ := os.ReadFile(f.p.Unit); !strings.Contains(string(unit), "<key>LimitLoadToSessionType</key>\n\t<string>Background</string>") {
		t.Errorf("the unit lost its Background limit:\n%s", unit)
	}
	if !strings.Contains(f.out.String(), "login") || !strings.Contains(f.out.String(), f.p.LoginUnit) {
		t.Errorf("install did not report the login item:\n%s", f.out)
	}
	// It is written, never loaded: loading it would run `restore` beside this install.
	if strings.Contains(f.seq(), svcLoginLabel) || strings.Contains(f.seq(), f.p.LoginUnit) {
		t.Errorf("install loaded the login item:\n%s", f.seq())
	}

	// A second run changes nothing, the login item included.
	f.mgr.reset()
	if code := f.install(t); code != 0 {
		t.Fatalf("second install: %d\n%s%s", code, f.out, f.errb)
	}
	if !strings.Contains(f.out.String(), "login     unchanged") || !strings.Contains(f.out.String(), "Nothing to change") {
		t.Errorf("second run:\n%s", f.out)
	}
}

func TestLinuxWritesNoLoginItem(t *testing.T) {
	f := newSvcFixture(t, "linux")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	if f.p.LoginUnit != "" {
		t.Errorf("a Linux layout has no login item, got %q", f.p.LoginUnit)
	}
	for _, line := range strings.Split(f.out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "login ") {
			t.Errorf("a Linux install reported a login item: %q", line)
		}
	}
}

// The failed update in the issue: the unit sits in gui/<uid>, and install must
// move it before it bootstraps, instead of failing with "did not come up healthy".
func TestInstallMovesACopyLoadedInTheGuiDomain(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	// An update ran while the unit was loaded only in gui/<uid>.
	f.mgr.loaded, f.mgr.active, f.mgr.guiLoaded = false, false, true
	if err := os.WriteFile(f.exe, []byte("binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mgr.reset()
	if code := f.install(t); code != 0 {
		t.Fatalf("install with a gui copy: %d\n%s%s", code, f.out, f.errb)
	}
	seq := f.seq()
	out := "launchctl bootout gui/501/" + svcLabel
	in := "launchctl bootstrap user/501 " + f.p.Unit
	if !strings.Contains(seq, out) || !strings.Contains(seq, in) || strings.Index(seq, out) > strings.Index(seq, in) {
		t.Errorf("the gui copy must be booted out before the bootstrap:\n%s", seq)
	}
	if f.mgr.guiLoaded || !f.mgr.loaded || !f.mgr.active {
		t.Errorf("after the move: gui=%v user loaded=%v active=%v", f.mgr.guiLoaded, f.mgr.loaded, f.mgr.active)
	}
	for _, want := range []string{"domain", "moved out of gui/501 into user/501", "serverContext", "health    ok"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("install did not say %q:\n%s", want, f.out)
		}
	}
}

func TestInstallSaysWhenTheGuiCopyWillNotGo(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	f.mgr.guiLoaded, f.mgr.stubborn = true, true
	code := f.install(t)
	if code != 1 || !strings.Contains(f.errb.String(), "loaded in gui/501") || !strings.Contains(f.errb.String(), "launchctl bootout gui/501/"+svcLabel) {
		t.Errorf("exit %d, stderr: %s", code, f.errb)
	}
	if strings.Contains(f.seq(), "launchctl bootstrap user/") {
		t.Errorf("bootstrapped into user/ over a copy that is still in gui/:\n%s", f.seq())
	}
}

func TestInstallNoStartLeavesBothDomainsAlone(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	f.mgr.guiLoaded = true
	if code := f.install(t, "--no-start"); code != 0 {
		t.Fatalf("install --no-start: %d\n%s%s", code, f.out, f.errb)
	}
	if got := f.mgr.sequence(); len(got) != 1 || got[0] != "doctor" {
		t.Errorf("--no-start touched the manager: %v", got)
	}
	if _, err := os.Stat(f.p.LoginUnit); err != nil {
		t.Errorf("--no-start still writes the files, the login item included: %v", err)
	}
}

func TestRestore(t *testing.T) {
	type step struct {
		name     string
		goos     string
		setup    func(f *svcFixture)
		wantCode int
		seqHas   []string
		seqNot   []string
		outHas   []string
	}
	start := "launchctl bootstrap user/501 "
	for _, tc := range []step{
		{
			name: "after a reboot nothing is loaded: it is bootstrapped into user/<uid>", goos: "darwin",
			setup:  func(f *svcFixture) { f.mgr.loaded, f.mgr.active = false, false },
			seqHas: []string{start}, seqNot: []string{"bootout"},
			outHas: []string{"service   changed    started", "health    ok"},
		},
		{
			name: "loaded but stopped: kickstarted, not reloaded", goos: "darwin",
			setup:  func(f *svcFixture) { f.mgr.active = false },
			seqHas: []string{"launchctl kickstart -k user/501/" + svcLabel}, seqNot: []string{"bootstrap", "bootout"},
			outHas: []string{"started"},
		},
		{
			name: "already loaded and running: nothing to change", goos: "darwin",
			setup:  func(f *svcFixture) {},
			seqNot: []string{"bootstrap", "bootout", "kickstart"},
			outHas: []string{"service   unchanged  running", "Nothing to change"},
		},
		{
			name: "a copy in gui/<uid> is moved, said aloud", goos: "darwin",
			setup:  func(f *svcFixture) { f.mgr.loaded, f.mgr.active, f.mgr.guiLoaded = false, false, true },
			seqHas: []string{"launchctl bootout gui/501/" + svcLabel, start},
			outHas: []string{"moved out of gui/501 into user/501"},
		},
		{
			name: "linux: registered but stopped is started", goos: "linux",
			setup:  func(f *svcFixture) { f.mgr.active = false },
			seqHas: []string{"systemctl --user start muster.service"},
			outHas: []string{"started"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSvcFixture(t, tc.goos)
			if code := f.install(t); code != 0 {
				t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
			}
			unitBefore, _ := os.ReadFile(f.p.Unit)
			tc.setup(f)
			f.mgr.reset()
			if code := f.run(t, "restore"); code != tc.wantCode {
				t.Fatalf("restore exit %d, want %d\n%s%s", code, tc.wantCode, f.out, f.errb)
			}
			seq := f.seq()
			for _, w := range tc.seqHas {
				if !strings.Contains(seq, w) {
					t.Errorf("sequence lacks %q:\n%s", w, seq)
				}
			}
			for _, w := range tc.seqNot {
				if strings.Contains(seq, w) {
					t.Errorf("sequence has %q:\n%s", w, seq)
				}
			}
			for _, w := range tc.outHas {
				if !strings.Contains(f.out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, f.out)
				}
			}
			// restore writes no file: it is safe at any time and as often as you like.
			if unitAfter, _ := os.ReadFile(f.p.Unit); string(unitAfter) != string(unitBefore) {
				t.Error("restore rewrote the unit")
			}
			if !f.mgr.loaded || !f.mgr.active || f.mgr.guiLoaded {
				t.Errorf("after restore: user loaded=%v active=%v gui=%v", f.mgr.loaded, f.mgr.active, f.mgr.guiLoaded)
			}
			if strings.Contains(seq, "doctor") {
				t.Errorf("restore ran doctor, which is install's step:\n%s", seq)
			}
		})
	}
}

func TestRestoreRefusesWhatIsNotInstalledAndReportsWhatNeverComesUp(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	if code := f.run(t, "restore"); code != 1 || !strings.Contains(f.errb.String(), "not installed as a service") || len(f.mgr.sequence()) != 0 {
		t.Errorf("before install: exit %d, stderr %q, calls %v", code, f.errb, f.mgr.sequence())
	}
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	// The address the unit names has nothing listening.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	if code := f.run(t, "install", "--addr", dead, "--tmux", f.tmux, "--no-start"); code != 0 {
		t.Fatalf("install --no-start: %d\n%s%s", code, f.out, f.errb)
	}
	f.env.HealthWait = 100 * time.Millisecond
	f.mgr.loaded, f.mgr.active = false, false
	if code := f.run(t, "restore"); code != 1 || !strings.Contains(f.errb.String(), "did not become healthy") || !strings.Contains(f.errb.String(), "muster.log") {
		t.Errorf("an unhealthy restore: exit %d, stderr %q", code, f.errb)
	}
}

func TestStatusNamesAGuiCopyAndAMissingLoginItem(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	if code := f.run(t, "status"); code != 0 || !strings.Contains(f.out.String(), "domain    ok") || !strings.Contains(f.out.String(), "login     ok") {
		t.Fatalf("a healthy install: exit %d\n%s", code, f.out)
	}

	// The unit is in gui/<uid> and not in user/<uid>.
	f.mgr.loaded, f.mgr.active, f.mgr.guiLoaded = false, false, true
	code := f.run(t, "status")
	if code != 1 || !strings.Contains(f.out.String(), "domain    NOT OK   loaded in gui/501, not user/501") || !strings.Contains(f.out.String(), "muster service restore") {
		t.Errorf("a gui copy: exit %d\n%s", code, f.out)
	}

	// An install from before the login item: it works until the next reboot.
	f.mgr.loaded, f.mgr.active, f.mgr.guiLoaded = true, true, false
	if err := os.Remove(f.p.LoginUnit); err != nil {
		t.Fatal(err)
	}
	code = f.run(t, "status")
	if code != 1 || !strings.Contains(f.out.String(), "login     NOT OK") || !strings.Contains(f.out.String(), "muster service install") {
		t.Errorf("no login item: exit %d\n%s", code, f.out)
	}
}

func TestUninstallRemovesTheGuiCopyAndTheLoginItem(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	// launchd has run the login item at a login, so it is loaded in gui/<uid>, and a
	// copy of the unit is there too.
	f.mgr.loginInGUI, f.mgr.guiLoaded, f.mgr.loaded, f.mgr.active = true, true, false, false
	f.mgr.reset()
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall: %d\n%s%s", code, f.out, f.errb)
	}
	if f.mgr.guiLoaded || f.mgr.loginInGUI || f.mgr.loaded {
		t.Errorf("still loaded: gui copy=%v login item=%v user=%v", f.mgr.guiLoaded, f.mgr.loginInGUI, f.mgr.loaded)
	}
	for _, gone := range []string{f.p.Unit, f.p.LoginUnit} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, want := range []string{"domain    removed", "login     removed"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("uninstall did not say %q:\n%s", want, f.out)
		}
	}
	// Again: nothing is there, and that is not an error.
	if code := f.run(t, "uninstall"); code != 0 || !strings.Contains(f.out.String(), "login     absent") {
		t.Errorf("second uninstall: exit %d\n%s", code, f.out)
	}
}

func TestServiceDomainRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    serviceDomains
		want rowStatus
		has  string
	}{
		{"a copy in gui/<uid> is a warning that names the fix", serviceDomains{UID: 501, GUI: true}, statusWarn, "muster service restore"},
		{"in both, the gui copy is what is wrong", serviceDomains{UID: 501, GUI: true, User: true}, statusWarn, "gui/501"},
		{"only in user/<uid> is right", serviceDomains{UID: 501, User: true}, statusPass, "user/501"},
		{"loaded nowhere is a skip, so the row exists wherever launchd does", serviceDomains{UID: 501}, statusSkip, "not loaded in either"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := judgeServiceDomain(tc.d)
			if row.ID != "service.domain" || row.Status != tc.want || !strings.Contains(row.Summary+row.Detail, tc.has) {
				t.Errorf("row %+v, want %s containing %q", row, tc.want, tc.has)
			}
		})
	}
}

// A warning is not an exit code: install runs doctor before it moves the copy, and
// a failing row would stop it. And the row is there on every macOS host, so that
// `--skip=service.domain` means the same on all of them.
func TestServiceDomainRowSitsInTheRunAndNeverFailsIt(t *testing.T) {
	old := doctorServiceDomain
	defer func() { doctorServiceDomain = old }()
	doctorServiceDomain = func() (serviceDomains, bool) { return serviceDomains{UID: 501, GUI: true}, true }
	rows := runChecks(context.Background(), testDoctorEnv(nil))
	if row := rowByID(t, rows, "service.domain"); row.Status != statusWarn {
		t.Errorf("through runChecks: %+v", row)
	}

	doctorServiceDomain = func() (serviceDomains, bool) { return serviceDomains{UID: 501}, true }
	if row := rowByID(t, runChecks(context.Background(), testDoctorEnv(nil)), "service.domain"); row.Status != statusSkip {
		t.Errorf("loaded nowhere: %+v", row)
	}
	env := testDoctorEnv(nil)
	env.Skip = map[string]bool{"service.domain": true}
	if row := rowByID(t, runChecks(context.Background(), env), "service.domain"); row.Status != statusSkip || !strings.Contains(row.Summary, "--skip") {
		t.Errorf("--skip: %+v", row)
	}

	// A host that is not macOS has no such row (as service.task has none off Windows).
	doctorServiceDomain = func() (serviceDomains, bool) { return serviceDomains{}, false }
	for _, r := range runChecks(context.Background(), testDoctorEnv(nil)) {
		if r.ID == "service.domain" {
			t.Errorf("a host with no launchd got the row: %+v", r)
		}
	}
}

// Booting out a gui/<uid> job that does not abandon its process group takes the
// session server and every session with it. The service never does that on purpose,
// so it refuses, changes nothing and names the command for the person who accepts it.
func TestAGuiCopyThatWouldEndTheSessionsIsNotMoved(t *testing.T) {
	for _, verb := range []string{"install", "restore", "uninstall"} {
		t.Run(verb, func(t *testing.T) {
			f := newSvcFixture(t, "darwin")
			if code := f.install(t); code != 0 {
				t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
			}
			f.mgr.loaded, f.mgr.active, f.mgr.guiLoaded, f.mgr.guiLeaves = false, false, true, true
			f.mgr.reset()
			var code int
			if verb == "install" {
				code = f.install(t)
			} else {
				code = f.run(t, verb)
			}
			msg := f.errb.String()
			if code != 1 || !strings.Contains(msg, "end every session") || !strings.Contains(msg, "launchctl bootout gui/501/"+svcLabel) {
				t.Errorf("exit %d, stderr: %s", code, msg)
			}
			if strings.Contains(f.seq(), "bootout") || strings.Contains(f.seq(), "bootstrap") {
				t.Errorf("it changed launchd anyway:\n%s", f.seq())
			}
			if !f.mgr.guiLoaded {
				t.Error("the gui copy is gone")
			}
			if _, err := os.Stat(f.p.Unit); err != nil {
				t.Errorf("the unit file was touched: %v", err)
			}
		})
	}
}

// The login item bootstraps the same file at a login. If it gets there between an
// update's unload and load, the update's own bootstrap is refused, and the unit is
// loaded from what is on disk, so that is not a failure.
func TestABootstrapRefusedBecauseTheUnitIsAlreadyLoadedIsNotAFailure(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	f.mgr.loaded, f.mgr.active = false, false
	f.mgr.failOn, f.mgr.loadsOnFailure = "launchctl bootstrap user/", true
	if code := f.run(t, "restore"); code != 0 {
		t.Fatalf("restore: %d\n%s%s", code, f.out, f.errb)
	}
	// ... but a refusal with nothing loaded afterwards is.
	f.mgr.loaded, f.mgr.active, f.mgr.loadsOnFailure = false, false, false
	if code := f.run(t, "restore"); code != 1 || !strings.Contains(f.errb.String(), "launchctl bootstrap") {
		t.Errorf("a bootstrap that loaded nothing: exit %d, stderr %s", code, f.errb)
	}
}

func TestLoginItemIsWrittenOnlyOnceDoctorPasses(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	f.mgr.doctorFail = true
	if code := f.install(t); code != 1 {
		t.Fatalf("install with a failing doctor: exit %d\n%s%s", code, f.out, f.errb)
	}
	if _, err := os.Stat(f.p.LoginUnit); err == nil {
		t.Error("a unit doctor rejected has a login item that would start it at the next login")
	}
	f.mgr.doctorFail = false
	if code := f.install(t, "--no-start"); code != 0 {
		t.Fatalf("install --no-start: %d\n%s%s", code, f.out, f.errb)
	}
	if _, err := os.Stat(f.p.LoginUnit); err != nil {
		t.Errorf("--no-start writes every file once doctor passes, the login item included: %v", err)
	}
}

func TestRestoreOnLinuxReloadsTheManagerBeforeItStarts(t *testing.T) {
	f := newSvcFixture(t, "linux")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	f.mgr.active = false
	f.mgr.reset()
	if code := f.run(t, "restore"); code != 0 {
		t.Fatalf("restore: %d\n%s%s", code, f.out, f.errb)
	}
	seq := f.seq()
	reload, start := strings.Index(seq, "daemon-reload"), strings.Index(seq, "--user start")
	if reload < 0 || start < 0 || reload > start {
		t.Errorf("a unit edited by hand must be read again before it is started:\n%s", seq)
	}
}

func TestShQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/h/Library/LaunchAgents/x.plist": `'/h/Library/LaunchAgents/x.plist'`,
		"/h/it's here/x.plist":            `'/h/it'\''s here/x.plist'`,
		"/h/a b/$HOME/`x`":                "'/h/a b/$HOME/`x`'",
	} {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
