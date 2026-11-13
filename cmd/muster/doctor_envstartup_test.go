package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// muster #471: with FLEET_CONFIG unset the doctor stopped at the single-token
// warning and never ran validateConfigExtras, so every start-up check that reads
// only the environment was a refusal to start that it passed on a machine
// configured that way. The start runs the function with a nil config; so does the
// doctor now, and a refusal is a failing config.load row.

func envOnlyRows(env map[string]string) []doctorRow {
	e := testDoctorEnv(env)
	e.Offline = true
	return runChecks(context.Background(), e)
}

func TestDoctorEnvOnlyStartupChecks(t *testing.T) {
	home := trustHome(t)
	link := trustLink(t, home)

	cases := []struct {
		name   string
		env    map[string]string
		detail string // "" = config.load is the single-token warning, not a failure
	}{
		{"a relative trust root", map[string]string{"FLEET_TRUST_ROOTS": "work/repos"}, `trust root "work/repos" must be an absolute path`},
		{"a relative root beside a valid one", map[string]string{"FLEET_TRUST_ROOTS": filepath.Join(home, "projects") + ",work/repos"}, `"work/repos" must be an absolute path`},
		{"home as a trust root", map[string]string{"FLEET_TRUST_ROOTS": home}, "the home directory"},
		{"a link to home as a trust root", map[string]string{"FLEET_TRUST_ROOTS": link}, "the home directory"},
		{"the filesystem root as a trust root", map[string]string{"FLEET_TRUST_ROOTS": "/"}, "the filesystem root"},
		{"an invalid opencode provider entry", map[string]string{"FLEET_RUNTIME_OPENCODE": "1", "FLEET_OPENCODE_PROVIDERS": "opencode/"}, "FLEET_OPENCODE_PROVIDERS"},

		// What the start does not refuse, the doctor does not either.
		{"a valid trust root", map[string]string{"FLEET_TRUST_ROOTS": filepath.Join(home, "projects")}, ""},
		{"a clean environment", map[string]string{}, ""},
		{"an invalid provider entry the runtime does not read", map[string]string{"FLEET_OPENCODE_PROVIDERS": "opencode/"}, ""},
		{"a relative trust root under a runtime that does not read it", map[string]string{"FLEET_RUNTIME": "none", "FLEET_TRUST_ROOTS": "work/repos"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"FLEET_TOKEN": "t", "FLEET_MACHINE": "m1"}
			for k, v := range c.env {
				env[k] = v
			}
			rows := envOnlyRows(env)
			row := rowByID(t, rows, "config.load")
			if c.detail == "" {
				if row.Status != statusWarn || !strings.Contains(row.Summary, "single-token mode") {
					t.Fatalf("config.load = %+v, want the single-token warning", row)
				}
				return
			}
			if row.Status != statusFail {
				t.Fatalf("config.load = %+v, want a fail: the start refuses this environment", row)
			}
			if !strings.Contains(row.Detail, c.detail) {
				t.Errorf("detail %q does not say %q", row.Detail, c.detail)
			}
			// The refusal does not replace the mode: the warning's one fact is still true.
			if !strings.Contains(row.Summary, "single-token mode") {
				t.Errorf("summary %q hides that FLEET_CONFIG is unset", row.Summary)
			}
			if strings.Contains(row.Summary+row.Detail, home) {
				t.Errorf("row carries the absolute home path: %+v", row)
			}
			// Rows that read only the environment stay decidable: a refused environment is
			// not a config that did not load.
			if tok := rowByID(t, rows, "token.source"); tok.Status != statusPass {
				t.Errorf("token.source = %+v, want it decided from FLEET_TOKEN", tok)
			}
		})
	}
}

// The exit code is what a script reads: a refused environment-only machine is red,
// and the same machine with the setting fixed is not.
func TestDoctorEnvOnlyRefusalTurnsTheExitCodeRed(t *testing.T) {
	trustHome(t)
	env := map[string]string{
		"FLEET_MACHINE": "m1", "FLEET_TOKEN": "t", "FLEET_ADDR": "127.0.0.1:9000", "FLEET_ALLOW_MUTATIONS": "1",
	}
	if code, out, _ := runDoctorTest(env, "--offline"); code != 0 {
		t.Fatalf("a clean environment-only setup: exit %d, want 0\n%s", code, out)
	}
	env["FLEET_TRUST_ROOTS"] = "work/repos"
	code, out, _ := runDoctorTest(env, "--offline")
	if code != 1 || !strings.Contains(out, "must be an absolute path") {
		t.Fatalf("a relative trust root: exit %d, want 1 naming it\n%s", code, out)
	}
	env["FLEET_TRUST_ROOTS"] = ""
	env["FLEET_RUNTIME_OPENCODE"] = "1"
	env["FLEET_OPENCODE_PROVIDERS"] = "opencode/"
	if code, out, _ := runDoctorTest(env, "--offline"); code != 1 || !strings.Contains(out, "FLEET_OPENCODE_PROVIDERS") {
		t.Fatalf("an invalid provider entry: exit %d, want 1 naming it\n%s", code, out)
	}
}

// The failure is reported once. A refused environment's roots are not repeated as a
// second judgement in trust.roots, which waits on config.load; and a file that loads,
// with the environment's roots valid, still gets the listing.
func TestDoctorEnvOnlyRefusalIsNotRepeatedByTrustRoots(t *testing.T) {
	home := trustHome(t)
	rows := envOnlyRows(map[string]string{"FLEET_TOKEN": "t", "FLEET_TRUST_ROOTS": "work/repos"})
	for _, r := range rows {
		if r.Status == statusFail && r.ID != "config.load" {
			t.Errorf("row %s also fails: %+v", r.ID, r)
		}
	}
	if tr := rowByID(t, rows, "trust.roots"); tr.Status != statusSkip {
		t.Errorf("trust.roots = %+v, want a skip while config.load fails", tr)
	}

	cfg := writeTestConfig(t, t.TempDir(), []testPrincipal{{Name: "op", Token: "t", Grants: grants("read")}}, nil)
	rows = envOnlyRows(map[string]string{"FLEET_CONFIG": cfg, "FLEET_TRUST_ROOTS": filepath.Join(home, "projects")})
	if row := rowByID(t, rows, "config.load"); row.Status != statusPass {
		t.Fatalf("config.load = %+v, want a pass", row)
	}
	if tr := rowByID(t, rows, "trust.roots"); tr.Status != statusPass || !strings.Contains(tr.Detail, "~/projects") {
		t.Fatalf("trust.roots = %+v, want the listing", tr)
	}

	// With a file, a refused environment root is the same refusal in the same row.
	rows = envOnlyRows(map[string]string{"FLEET_CONFIG": cfg, "FLEET_TRUST_ROOTS": home})
	if row := rowByID(t, rows, "config.load"); row.Status != statusFail || !strings.Contains(row.Detail, "the home directory") {
		t.Fatalf("config.load with a file and a home root = %+v, want a fail naming it", row)
	}
	if tr := rowByID(t, rows, "trust.roots"); tr.Status != statusSkip {
		t.Errorf("trust.roots = %+v, want a skip while config.load fails", tr)
	}
}
