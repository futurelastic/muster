package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/drivers/opencode"
	"github.com/futurelastic/muster/internal/drivers/tmux"
)

// muster #374: the start-up checks that need only the file and the environment are
// one function, called by main.go, by `muster doctor` (config.load) and by a
// reload's restart-only refusal. Before it, closedRetentionDays: -1 passed
// config.load and "the file passes start-up validation", and the restart stopped.

// startupDoc is a loadable config plus the given extra top-level settings.
func startupDoc(extra map[string]any) map[string]any {
	doc := rlDoc(rlPrincipal("op", "tok-op", "read"))
	for k, v := range extra {
		doc[k] = v
	}
	return doc
}

func loadStartupDoc(t *testing.T, doc map[string]any) *fileConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.json")
	putCfg(t, path, doc)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("the test's own config does not load: %v", err)
	}
	return cfg
}

func TestValidateConfigExtras_StartupSettings(t *testing.T) {
	if hostOS() == "windows" {
		t.Skip("the runtime set differs on Windows (ADR 303); the cases below name tmux")
	}
	tmux_ := map[string]string{"FLEET_RUNTIME": "tmux"}
	cases := []struct {
		name  string
		extra map[string]any
		env   map[string]string
		want  string // "" = the file starts
	}{
		{"nothing set", nil, tmux_, ""},

		{"closedRetentionDays below zero", map[string]any{"closedRetentionDays": -1}, tmux_,
			"closedRetentionDays must be positive, got -1"},
		{"closedRetentionDays positive", map[string]any{"closedRetentionDays": 30}, tmux_, ""},
		{"inputAuditRetentionDays below zero", map[string]any{"inputAuditRetentionDays": -7}, tmux_,
			"inputAuditRetentionDays must be positive, got -7"},
		{"inputAuditRetentionDays positive", map[string]any{"inputAuditRetentionDays": 90}, tmux_, ""},

		{"maxInputBytes below zero", map[string]any{"maxInputBytes": -5}, tmux_,
			"maxInputBytes must be positive, got -5"},
		{"maxInputBytes at the ceiling", map[string]any{"maxInputBytes": 1 << 20}, tmux_,
			"at or above the"},
		{"maxInputBytes in range", map[string]any{"maxInputBytes": 12345}, tmux_, ""},

		{"defaultRuntime typo", map[string]any{"defaultRuntime": "tmx"}, tmux_,
			`defaultRuntime "tmx" is not a runtime this machine runs (it runs: ` + string(tmux.DefaultRuntime) + `)`},
		// FLEET_RUNTIME=tmux names the choice; the id a default names is the
		// driver's own (claude-code-tmux), as it is to SetDefaultRuntime.
		{"defaultRuntime is the primary runtime's id", map[string]any{"defaultRuntime": string(tmux.DefaultRuntime)}, tmux_, ""},
		{"defaultRuntime opencode, not enabled", map[string]any{"defaultRuntime": string(opencode.DefaultRuntime)}, tmux_,
			`defaultRuntime "opencode" is not a runtime this machine runs`},
		{"defaultRuntime opencode, enabled",
			map[string]any{"defaultRuntime": string(opencode.DefaultRuntime)},
			map[string]string{"FLEET_RUNTIME": "tmux", "FLEET_RUNTIME_OPENCODE": "1"}, ""},
		{"defaultRuntime opencode alone, FLEET_RUNTIME=none",
			map[string]any{"defaultRuntime": string(opencode.DefaultRuntime)},
			map[string]string{"FLEET_RUNTIME": "none", "FLEET_RUNTIME_OPENCODE": "1"}, ""},
		{"defaultRuntime stub under FLEET_RUNTIME=stub", map[string]any{"defaultRuntime": "stub"},
			map[string]string{"FLEET_RUNTIME": "stub"}, ""},
		{"defaultRuntime tmux under FLEET_RUNTIME=none", map[string]any{"defaultRuntime": string(tmux.DefaultRuntime)},
			map[string]string{"FLEET_RUNTIME": "none"}, "it runs: none"},
		// A FLEET_RUNTIME the build cannot run stops the start before any default
		// runtime is looked at; the runtime row says it, this does not say it twice.
		{"defaultRuntime under an unknown FLEET_RUNTIME", map[string]any{"defaultRuntime": "tmx"},
			map[string]string{"FLEET_RUNTIME": "nonsense"}, ""},

		// Trust roots are the tmux driver's: another primary runtime never reads them.
		{"relative trust root, tmux", map[string]any{"trustRoots": []string{"work/repos"}}, tmux_,
			`trust root "work/repos" must be an absolute path`},
		{"absolute trust root, tmux", map[string]any{"trustRoots": []string{"/work/repos"}}, tmux_, ""},
		{"relative trust root, FLEET_RUNTIME=none", map[string]any{"trustRoots": []string{"work/repos"}},
			map[string]string{"FLEET_RUNTIME": "none"}, ""},
		{"relative trust root from the environment",
			nil,
			map[string]string{"FLEET_RUNTIME": "tmux", "FLEET_TRUST_ROOTS": "work/repos"},
			`trust root "work/repos" must be an absolute path`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadStartupDoc(t, startupDoc(c.extra))
			err := validateConfigExtras(cfg, envOf(c.env))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("a file that starts was refused: %v", err)
			case c.want != "" && err == nil:
				t.Fatalf("a file whose start stops (%s) passed", c.want)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Fatalf("refusal %q does not say %q", err, c.want)
			}
		})
	}
}

// With no config file at all, the environment's own settings are still checked.
func TestValidateConfigExtras_NoConfigFile(t *testing.T) {
	if hostOS() == "windows" {
		t.Skip("tmux is not a choice on Windows (ADR 303)")
	}
	if err := validateConfigExtras(nil, envOf(map[string]string{"FLEET_RUNTIME": "tmux"})); err != nil {
		t.Fatalf("no config, nothing to refuse: %v", err)
	}
	err := validateConfigExtras(nil, envOf(map[string]string{"FLEET_RUNTIME": "tmux", "FLEET_TRUST_ROOTS": "rel"}))
	if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("a relative trust root from FLEET_TRUST_ROOTS with no config file = %v, want a refusal", err)
	}
}

// Only a list that is the sole source of peers stops the start. In prefer and
// shadow mode the service reports it and goes on.
func TestValidateConfigExtras_MachineList(t *testing.T) {
	for _, c := range []struct {
		mode string
		stop bool
	}{{"only", true}, {"prefer", false}, {"shadow", false}} {
		t.Run(c.mode, func(t *testing.T) {
			f, cfg := newListFixture(t, c.mode, 4)
			env := envOf(map[string]string{"FLEET_RUNTIME": "stub", "FLEET_STATE_DIR": f.stateDir})
			if err := validateConfigExtras(cfg, env); err != nil {
				t.Fatalf("a list that verifies was refused: %v", err)
			}
			if err := os.WriteFile(f.listPath, []byte("not a list"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := validateConfigExtras(cfg, env)
			switch {
			case c.stop && err == nil:
				t.Fatal("mode only with a list that does not verify passed: the start stops on it")
			case c.stop && !strings.Contains(err.Error(), "does not verify"):
				t.Fatalf("refusal %q does not say the list does not verify", err)
			case !c.stop && err != nil:
				t.Fatalf("mode %s carries on without the list, but the check refused: %v", c.mode, err)
			}
		})
	}
}

// The three surfaces agree: for each setting a restart would stop on, doctor's
// config.load row fails with the function's own error, and a restart-only reload
// refusal says "a restart would fail" with the same words.
func TestStartupChecks_DoctorAndReloadAgreeWithTheStart(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		names string // the restart-only setting the refusal names
		want  string
	}{
		{"defaultRuntime", map[string]any{"defaultRuntime": "tmx"}, "defaultRuntime",
			`defaultRuntime "tmx" is not a runtime this machine runs`},
		{"maxInputBytes", map[string]any{"maxInputBytes": -5}, "maxInputBytes",
			"maxInputBytes must be positive, got -5"},
		{"closedRetentionDays", map[string]any{"closedRetentionDays": -1}, "closedRetentionDays",
			"closedRetentionDays must be positive, got -1"},
		{"inputAuditRetentionDays", map[string]any{"inputAuditRetentionDays": -1}, "inputAuditRetentionDays",
			"inputAuditRetentionDays must be positive, got -1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := startupDoc(c.extra)
			doc["principals"] = append(doc["principals"].([]any), rlPrincipal("new", "tok-new", "read"))

			// doctor: config.load.
			dir := t.TempDir()
			path := filepath.Join(dir, "fleet.json")
			putCfg(t, path, doc)
			rows := runChecks(context.Background(), testDoctorEnv(map[string]string{
				"FLEET_CONFIG": path, "FLEET_RUNTIME": "stub", "FLEET_STATE_DIR": dir}))
			row := rowByID(t, rows, "config.load")
			if row.Status != statusFail {
				t.Fatalf("config.load = %+v, want fail: the restart stops on this file", row)
			}
			if !strings.Contains(row.Detail, c.want) {
				t.Errorf("config.load detail %q does not say %q", row.Detail, c.want)
			}

			// a reload of a restart-only change onto the same file.
			r := newReloadRig(t, rlDoc(rlPrincipal("old", "tok-old", "read")))
			putCfg(t, r.path, doc)
			r.rl.check("test")
			refusal := checkReloadStatus(r.dir, r.path)
			if refusal.Status != statusWarn {
				t.Fatalf("reload status row = %+v, want a warn naming the refusal", refusal)
			}
			for _, want := range []string{c.names, "only a restart applies", "a restart would fail", c.want} {
				if !strings.Contains(refusal.Detail, want) {
					t.Errorf("the refusal %q does not say %q", refusal.Detail, want)
				}
			}
			if strings.Contains(refusal.Detail, "passes start-up validation") {
				t.Errorf("a file the restart stops on was called valid: %q", refusal.Detail)
			}
		})
	}
}

// The machine list case of the same agreement: a list that must verify and does
// not fails config.load and a restart-only refusal, not only the machine-list row.
func TestStartupChecks_DoctorAndReloadAgreeOnAMachineListThatMustVerify(t *testing.T) {
	r, l := newListRig(t, "only", 1, map[string]string{"m-a": "http://127.0.0.1:1"})
	// In production the reloader's environment is the process's, so it names the
	// state directory the list's sequence is remembered in; the rig does not.
	r.rl.getenv = func(k string) string {
		if k == "FLEET_STATE_DIR" {
			return r.dir
		}
		return ""
	}

	// A restart-only edit (the list's path moves to a file that is not there).
	doc := rlDoc(rlPrincipal("op", "tok-op", "read"))
	doc["machineList"] = map[string]any{"mode": "only", "path": l.path + ".gone", "fleet": "fleet-1", "signer": listPub(l.owner)}
	putCfg(t, r.path, doc)
	r.rl.check("test")

	refusal := checkReloadStatus(r.dir, r.path)
	for _, want := range []string{"machineList", "a restart would fail", "does not verify", "no such file"} {
		if !strings.Contains(refusal.Detail, want) {
			t.Errorf("the refusal %q does not say %q", refusal.Detail, want)
		}
	}

	rows := runChecks(context.Background(), testDoctorEnv(map[string]string{
		"FLEET_CONFIG": r.path, "FLEET_RUNTIME": "stub", "FLEET_STATE_DIR": r.dir}))
	row := rowByID(t, rows, "config.load")
	if row.Status != statusFail || !strings.Contains(row.Detail, "does not verify") || !strings.Contains(row.Detail, "no such file") {
		t.Errorf("config.load = %+v, want a fail naming the list", row)
	}
}

// A file the start accepts is still called valid: the new checks add refusals only
// where the start refuses.
func TestStartupChecks_AFileThatStartsStillPassesBothSurfaces(t *testing.T) {
	doc := startupDoc(map[string]any{
		"defaultRuntime": "stub", "maxInputBytes": 12345,
		"closedRetentionDays": 30, "inputAuditRetentionDays": 90,
	})
	doc["principals"] = append(doc["principals"].([]any), rlPrincipal("new", "tok-new", "read"))

	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.json")
	putCfg(t, path, doc)
	rows := runChecks(context.Background(), testDoctorEnv(map[string]string{
		"FLEET_CONFIG": path, "FLEET_RUNTIME": "stub", "FLEET_STATE_DIR": dir}))
	if row := rowByID(t, rows, "config.load"); row.Status != statusPass {
		t.Errorf("config.load = %+v, want pass", row)
	}
}
