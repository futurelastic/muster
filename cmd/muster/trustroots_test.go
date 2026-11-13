package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// muster #469: a trust root that is the home directory or "/" was dropped by the
// seeder without a word while the start-up line counted it as configured. These
// are the other half of the seeder's own tests: the start refuses it, the start-up
// line counts what is active, and the doctor lists what is.

// trustHome points HOME at a resolved temp directory and returns it. The
// resolution matters on macOS, where a temp path sits behind a link and the
// seeder judges both spellings.
func trustHome(t *testing.T) string {
	t.Helper()
	if hostOS() == "windows" {
		t.Skip("HOME and symlinks are not what a Windows start reads (ADR 303)")
	}
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	return home
}

func trustLink(t *testing.T, target string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "to-home")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	return link
}

func TestValidateConfigExtras_TrustRootThatCannotBeOne(t *testing.T) {
	home := trustHome(t)
	link := trustLink(t, home)
	valid := filepath.Join(home, "projects")
	tmux_ := map[string]string{"FLEET_RUNTIME": "tmux"}

	cases := []struct {
		name  string
		roots []string
		env   map[string]string
		want  string // "" = the file starts
	}{
		{"home", []string{home}, tmux_, "the home directory"},
		{"home with a trailing separator", []string{home + string(filepath.Separator)}, tmux_, "the home directory"},
		{"home spelled through a dot", []string{filepath.Join(home, "sub", "..")}, tmux_, "the home directory"},
		{"a symlink to home", []string{link}, tmux_, "the home directory"},
		{"the filesystem root", []string{string(filepath.Separator)}, tmux_, "the filesystem root"},
		// One refused root stops the start even beside a valid one: the operator wrote
		// something that cannot work, and a start that goes on is the silence #469 is about.
		{"home beside a valid root", []string{valid, home}, tmux_, "the home directory"},
		{"a valid root", []string{valid}, tmux_, ""},
		{"a root that does not exist yet", []string{filepath.Join(home, "not", "yet")}, tmux_, ""},
		// Trust roots are the tmux driver's alone: another primary runtime never reads them.
		{"home, FLEET_RUNTIME=none", []string{home}, map[string]string{"FLEET_RUNTIME": "none"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadStartupDoc(t, startupDoc(map[string]any{"trustRoots": c.roots}))
			err := validateConfigExtras(cfg, envOf(c.env))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("a file that starts was refused: %v", err)
			case c.want != "" && err == nil:
				t.Fatalf("a file whose start stops (%s) passed", c.want)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Fatalf("refusal %q does not say %q", err, c.want)
			}
			if err != nil && !strings.Contains(err.Error(), "name the folder that holds the working folders") {
				t.Errorf("the refusal %q does not say what a trust root is", err)
			}
		})
	}
}

// From the environment alone, with no config file: the same refusal, because
// FLEET_TRUST_ROOTS wins over the file and nothing else would catch it.
func TestValidateConfigExtras_TrustRootHomeFromTheEnvironment(t *testing.T) {
	home := trustHome(t)
	err := validateConfigExtras(nil, envOf(map[string]string{"FLEET_RUNTIME": "tmux", "FLEET_TRUST_ROOTS": home}))
	if err == nil || !strings.Contains(err.Error(), "the home directory") {
		t.Fatalf("home from FLEET_TRUST_ROOTS with no config file = %v, want a refusal", err)
	}
}

func TestTrustSeedStartLineCountsActiveRootsNotConfiguredOnes(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		active     []string
		want       string
		notWant    string
	}{
		{"every root active", 2, []string{"/a", "/b"}, "active for 2 root(s)", "NO active"},
		{"none active", 1, nil, "NO active root (1 configured", "configured for 1"},
		{"a repeat is counted once", 2, []string{"/a"}, "active for 1 of 2 configured", "NO active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := trustSeedStartLine(c.configured, c.active)
			if !strings.Contains(got, c.want) {
				t.Errorf("%q does not say %q", got, c.want)
			}
			if strings.Contains(got, c.notWant) {
				t.Errorf("%q says %q", got, c.notWant)
			}
		})
	}
}

func TestCheckTrustRoots(t *testing.T) {
	home := trustHome(t)
	link := trustLink(t, home)
	a := filepath.Join(home, "projects")
	b := filepath.Join(home, "work")
	tmux_ := map[string]string{"FLEET_RUNTIME": "tmux"}

	cases := []struct {
		name       string
		roots      string
		env        map[string]string
		cfgFailed  bool
		wantRow    bool
		wantStatus rowStatus
		summary    string // must appear in the summary
		detail     string // must appear in the detail
		notDetail  string // must NOT appear in the detail
	}{
		{"no roots configured: no row", "", tmux_, false, false, "", "", "", ""},
		{"another runtime does not read them: no row", home, map[string]string{"FLEET_RUNTIME": "none"}, false, false, "", "", "", ""},
		{"two valid roots", a + "," + b, tmux_, false, true, statusPass, "2 trust root(s) active of 2", "~/projects, ~/work", home},
		{"a root written twice counts once", a + "," + a, tmux_, false, true, statusPass, "1 trust root(s) active of 2", "counts once", ""},
		{"home alone fails and names it", home, tmux_, false, true, statusFail, "1 of 1 configured trust root(s) cannot be used", "~ is the home directory", home},
		{"a symlink to home fails", link, tmux_, false, true, statusFail, "cannot be used", "is the home directory", ""},
		{"the filesystem root fails", "/", tmux_, false, true, statusFail, "cannot be used", "/ is the filesystem root", ""},
		{"home beside a valid root still fails", a + "," + home, tmux_, false, true, statusFail, "1 of 2 configured", "the home directory", ""},
		{"a relative root fails", "work/repos", tmux_, false, true, statusFail, "cannot be used", "not an absolute path", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range c.env {
				env[k] = v
			}
			if c.roots != "" {
				env["FLEET_TRUST_ROOTS"] = c.roots
			}
			row, ok := checkTrustRoots(nil, c.cfgFailed, envOf(env))
			if ok != c.wantRow {
				t.Fatalf("row present = %v, want %v (%+v)", ok, c.wantRow, row)
			}
			if !ok {
				return
			}
			if row.ID != "trust.roots" {
				t.Errorf("id = %q", row.ID)
			}
			if row.Status != c.wantStatus {
				t.Errorf("status = %s, want %s (%s / %s)", row.Status, c.wantStatus, row.Summary, row.Detail)
			}
			if !strings.Contains(row.Summary, c.summary) {
				t.Errorf("summary %q does not say %q", row.Summary, c.summary)
			}
			if !strings.Contains(row.Detail, c.detail) {
				t.Errorf("detail %q does not say %q", row.Detail, c.detail)
			}
			if c.notDetail != "" && strings.Contains(row.Detail, c.notDetail) {
				t.Errorf("detail %q carries the absolute path %q", row.Detail, c.notDetail)
			}
		})
	}
}

// The roots come from the config file when the environment names none, and a file
// that did not load leaves the row undecidable rather than silent.
func TestCheckTrustRootsReadsTheFileAndWaitsForAConfigThatLoads(t *testing.T) {
	home := trustHome(t)
	a := filepath.Join(home, "projects")
	cfg := loadStartupDoc(t, startupDoc(map[string]any{"trustRoots": []string{a}}))
	env := envOf(map[string]string{"FLEET_RUNTIME": "tmux"})

	row, ok := checkTrustRoots(cfg, false, env)
	if !ok || row.Status != statusPass || !strings.Contains(row.Detail, "~/projects") {
		t.Fatalf("roots from the file = %+v (present %v)", row, ok)
	}
	row, ok = checkTrustRoots(nil, true, env)
	if !ok || row.Status != statusSkip || !strings.Contains(row.Summary, "config.load") {
		t.Fatalf("a config that did not load = %+v (present %v), want a skip naming config.load", row, ok)
	}
}

// The row is in the doctor's report, ahead of anything that needs a network, and a
// root that is home turns the exit code red — with no FLEET_CONFIG at all, where
// validateConfigExtras is not run and the row is the only thing that says it.
func TestDoctorReportsATrustRootThatIsHome(t *testing.T) {
	home := trustHome(t)
	rows := runChecks(context.Background(), func() doctorEnv {
		e := testDoctorEnv(map[string]string{"FLEET_TRUST_ROOTS": home})
		e.Offline = true
		return e
	}())
	row := rowByID(t, rows, "trust.roots")
	if row.Status != statusFail || !strings.Contains(row.Detail, "the home directory") {
		t.Fatalf("trust.roots = %+v, want a fail naming the home directory", row)
	}

	rows = runChecks(context.Background(), func() doctorEnv {
		e := testDoctorEnv(map[string]string{"FLEET_TRUST_ROOTS": filepath.Join(home, "projects")})
		e.Offline = true
		return e
	}())
	if row := rowByID(t, rows, "trust.roots"); row.Status != statusPass {
		t.Fatalf("trust.roots for a valid root = %+v, want a pass", row)
	}
}

// serveTrustLines starts the real `serve` path with the given trust roots and home,
// and returns its stderr lines up to the point the service is serving or has
// stopped, together with whether the process had exited by then. The wiring in
// main.go is what this covers: the helpers above judge a root, and nothing else
// asserts that the start calls them.
func serveTrustLines(t *testing.T, home, roots string) (lines []string, exited bool) {
	t.Helper()
	cmd, sc := startServe(t,
		"HOME="+home, "FLEET_TRUST_ROOTS="+roots, "FLEET_RUNTIME=tmux", "FLEET_TOKEN=secret",
		"FLEET_MACHINE=testbox", "FLEET_ADDR=127.0.0.1:0", "FLEET_STATE_DIR="+filepath.Join(home, "state"),
		"FLEET_RUNTIME_OPENCODE=", "FLEET_CONFIG=", "FLEET_PEERS=", "FLEET_LOG_FILE=", "FLEET_TRUST_STATE_PATH=")
	done := make(chan struct{})
	out := make(chan string)
	go func() {
		defer close(done)
		for sc.Scan() {
			out <- sc.Text()
		}
	}()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case l := <-out:
			lines = append(lines, l)
			// Everything the trust wiring says comes before the listener does.
			if listeningRe.MatchString(l) || strings.Contains(l, "trust-seed active") || strings.Contains(l, "NO active root") {
				return lines, false
			}
		case <-done:
			_ = cmd.Wait()
			return lines, true
		case <-deadline:
			t.Fatalf("the service neither stopped nor said anything about its trust roots; saw %q", lines)
		}
	}
}

// The wiring, end to end: a trust root that is home stops the real start with the
// refusal, and a valid root's start says it is active for one root — the count of
// roots in force, not of the list. Needs only the environment, not a running tmux:
// everything asserted is logged before the runtime's driver is built.
func TestServeRefusesAHomeTrustRootAndCountsActiveOnes(t *testing.T) {
	home := trustHome(t)
	if err := os.MkdirAll(filepath.Join(home, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}

	lines, exited := serveTrustLines(t, home, home)
	if !exited {
		t.Fatalf("a service whose only trust root is home kept going: %q", lines)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "the home directory") {
		t.Errorf("the refusal does not say why: %q", got)
	}

	lines, _ = serveTrustLines(t, home, filepath.Join(home, "projects"))
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "trust-seed active for 1 root(s)") {
		t.Errorf("a valid root's start does not report it active: %q", got)
	}

	// A root written twice is one root in force: the line counts what is active, so
	// it is the case that goes red if main.go goes back to counting the list.
	twice := filepath.Join(home, "projects")
	lines, _ = serveTrustLines(t, home, twice+","+twice)
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "trust-seed active for 1 of 2 configured root(s)") {
		t.Errorf("the start counted the list, not the roots in force: %q", got)
	}
}
