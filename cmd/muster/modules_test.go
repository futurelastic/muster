package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// #185: the switch. Everything here is about what the composition root resolves
// from three environment variables — never about what a module then does.

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func installFakeModule(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil { // WriteFile honours the umask
		t.Fatal(err)
	}
	return path
}

// Nothing set: no module, no client, nothing to log — a daemon exactly as it was
// before modules existed.
func TestModuleConfig_Unset(t *testing.T) {
	for _, v := range []string{"", "   ", ",,", " , "} {
		got := loadDeliveryModules(envOf(map[string]string{"FLEET_DELIVERY_MODULES": v}), "/nowhere/bin/muster", "")
		if len(got.Enabled) != 0 || len(got.Config.Clients) != 0 || len(got.Problems) != 0 || len(got.Invalid) != 0 {
			t.Errorf("FLEET_DELIVERY_MODULES=%q resolved to %+v, want nothing", v, got)
		}
		if lines := got.describe(); len(lines) != 0 {
			t.Errorf("FLEET_DELIVERY_MODULES=%q logged %v", v, lines)
		}
	}
}

func TestModuleConfig_Enabled(t *testing.T) {
	dir := t.TempDir()
	path := installFakeModule(t, dir, "relay-a")
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES": "relay-a, relay-b,relay-a",
		"FLEET_MODULES_DIR":      dir,
		"FLEET_STATE_DIR":        "/state",
		"PATH":                   "/usr/bin",
	}), "/nowhere/bin/muster", "/state")

	if strings.Join(got.Enabled, ",") != "relay-a,relay-b" {
		t.Fatalf("Enabled = %v, want the deduplicated list in the configured order", got.Enabled)
	}
	if len(got.Config.Clients) != 1 || got.Config.Clients[0].Name != "relay-a" || got.Config.Clients[0].Path != path {
		t.Fatalf("Clients = %+v, want only the module whose executable exists", got.Config.Clients)
	}
	if strings.Join(got.Config.Enabled, ",") != "relay-a,relay-b" {
		t.Errorf("the driver must be told the whole enabled list, installed or not: %v", got.Config.Enabled)
	}
	if len(got.Problems) != 1 || got.Problems[0].Name != "relay-b" {
		t.Fatalf("Problems = %+v, want relay-b named", got.Problems)
	}
	env := strings.Join(got.Config.Clients[0].Env, "\n")
	for _, want := range []string{"PATH=/usr/bin", "FLEET_STATE_DIR=/state"} {
		if !strings.Contains(env, want) {
			t.Errorf("child env %q lacks %s", env, want)
		}
	}
}

func TestModuleConfig_InvalidNameSkipped(t *testing.T) {
	dir := t.TempDir()
	installFakeModule(t, dir, "relay-a")
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES": "Relay, auto, terminal, ../x, relay-a",
		"FLEET_MODULES_DIR":      dir,
	}), "", "")
	if strings.Join(got.Enabled, ",") != "relay-a" {
		t.Fatalf("Enabled = %v, want only the legal name", got.Enabled)
	}
	if len(got.Invalid) != 4 {
		t.Fatalf("Invalid = %v, want the four illegal entries", got.Invalid)
	}
	if lines := strings.Join(got.describe(), "\n"); !strings.Contains(lines, "not a legal module name") {
		t.Errorf("an illegal name must be reported once: %q", lines)
	}
}

// An enabled name with no executable is reported once, by name and reason —
// never with the directory it looked in — and is not an error: a machine that
// could not fetch the module is a supported state.
func TestModuleConfig_MissingExecutableLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES": "relay-a",
		"FLEET_MODULES_DIR":      dir,
	}), "", "")
	lines := got.describe()
	if len(lines) != 1 || !strings.Contains(lines[0], `"relay-a"`) {
		t.Fatalf("lines = %q, want exactly one naming the module", lines)
	}
	if strings.Contains(lines[0], dir) {
		t.Errorf("a log line named the modules directory: %q", lines[0])
	}
	if len(got.Config.Clients) != 0 {
		t.Errorf("Clients = %+v, want none", got.Config.Clients)
	}
}

func TestModuleConfig_DefaultDirFromExecutable(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "muster")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	modules := filepath.Join(root, "libexec", "muster", "modules")
	path := installFakeModule(t, modules, "relay-a")

	got := loadDeliveryModules(envOf(map[string]string{"FLEET_DELIVERY_MODULES": "relay-a"}), exe, "")
	if len(got.Config.Clients) != 1 {
		t.Fatalf("the default directory (beside the binary) was not used: %+v", got)
	}
	wantDir, _ := filepath.EvalSymlinks(modules)
	gotDir, _ := filepath.EvalSymlinks(filepath.Dir(got.Config.Clients[0].Path))
	if gotDir != wantDir {
		t.Errorf("looked in %q, want %q", gotDir, wantDir)
	}
	_ = path
}

// #414: the same default under an npm install. The daemon there runs out of a
// platform package, not out of <prefix>/bin, and the module an operator put at
// <prefix>/libexec/muster/modules/<m> — where the standalone binary finds it —
// must be loaded with FLEET_MODULES_DIR unset, the unit's usual state.
func TestModuleConfig_DefaultDirUnderAnNpmInstall(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// npm's global layout is <prefix>/lib/node_modules, except on Windows.
	nodeModules := filepath.Join(prefix, "lib", "node_modules")
	if runtime.GOOS == "windows" {
		nodeModules = filepath.Join(prefix, "node_modules")
	}
	exe := filepath.Join(nodeModules, "@futurelastic", "muster-linux-x64", "bin", "muster")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	modules := filepath.Join(prefix, "libexec", "muster", "modules")
	installFakeModule(t, modules, "relay-a")

	vars := map[string]string{"FLEET_DELIVERY_MODULES": "relay-a"}
	got := loadDeliveryModules(envOf(vars), exe, "")
	if len(got.Problems) != 0 || len(got.Config.Clients) != 1 {
		t.Fatalf("the module at <prefix>/libexec/muster/modules was not loaded: problems=%+v clients=%+v",
			got.Problems, got.Config.Clients)
	}
	if gotDir := filepath.Dir(got.Config.Clients[0].Path); gotDir != modules {
		t.Errorf("looked in %q, want %q", gotDir, modules)
	}
}

// #414: what GET /v1/health is handed. Enabled-and-found is loaded; enabled and
// not found is skipped with the reason the log line carries; an illegal name is
// skipped too; and no switch at all is the zero status, which the service
// reports as no `modules` key.
func TestModuleConfig_HealthStatus(t *testing.T) {
	if st := loadDeliveryModules(envOf(nil), "", "").healthStatus(); len(st.Enabled)+len(st.Loaded)+len(st.Skipped) != 0 {
		t.Errorf("no switch: %+v, want the zero status", st)
	}

	dir := t.TempDir()
	installFakeModule(t, dir, "relay-a")
	st := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES": "relay-a, relay-b, Bad Name",
		"FLEET_MODULES_DIR":      dir,
	}), "", "").healthStatus()
	if strings.Join(st.Enabled, ",") != "relay-a,relay-b" || strings.Join(st.Loaded, ",") != "relay-a" {
		t.Errorf("enabled=%v loaded=%v, want [relay-a relay-b] and [relay-a]", st.Enabled, st.Loaded)
	}
	if len(st.Skipped) != 2 ||
		st.Skipped[0].Name != "Bad Name" || st.Skipped[0].Reason != "not a legal module name" ||
		st.Skipped[1].Name != "relay-b" || st.Skipped[1].Reason != "no executable installed" {
		t.Errorf("skipped = %+v, want the illegal name then relay-b: no executable installed", st.Skipped)
	}
	for _, s := range st.Skipped {
		if strings.Contains(s.Reason, dir) {
			t.Errorf("a reason names the modules directory: %q", s.Reason)
		}
	}

	// The case from the issue: enabled, FLEET_MODULES_DIR unset, nothing at the
	// default — the whole list is skipped, and health can say so.
	gone := loadDeliveryModules(envOf(map[string]string{"FLEET_DELIVERY_MODULES": "relay-a"}), filepath.Join(dir, "bin", "muster"), "").healthStatus()
	if len(gone.Loaded) != 0 || len(gone.Skipped) != 1 || gone.Skipped[0].Name != "relay-a" {
		t.Errorf("enabled with nothing at the default directory: %+v, want relay-a skipped", gone)
	}
}

// FLEET_DELIVERY_MODULE_ENV names the only extras a module child receives, and
// a FLEET_ name is never one of them: this service's own credentials are not
// the module's.
func TestModuleConfig_ForwardedEnvExcludesFleetNames(t *testing.T) {
	dir := t.TempDir()
	installFakeModule(t, dir, "relay-a")
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES":    "relay-a",
		"FLEET_MODULES_DIR":         dir,
		"FLEET_DELIVERY_MODULE_ENV": "RELAY_TOKEN_FILE, FLEET_TOKEN",
		"RELAY_TOKEN_FILE":          "/somewhere",
		"FLEET_TOKEN":               "secret-value",
		"UNRELATED":                 "x",
	}), "", "")
	env := strings.Join(got.Config.Clients[0].Env, "\n")
	if !strings.Contains(env, "RELAY_TOKEN_FILE=/somewhere") {
		t.Errorf("the forwarded name was not forwarded: %q", env)
	}
	if strings.Contains(env, "FLEET_TOKEN") || strings.Contains(env, "secret-value") || strings.Contains(env, "UNRELATED") {
		t.Errorf("the child received something it was not entitled to: %q", env)
	}
	if lines := strings.Join(got.describe(), "\n"); !strings.Contains(lines, "FLEET_TOKEN") || strings.Contains(lines, "secret-value") {
		t.Errorf("the refusal must be reported by name and never by value: %q", lines)
	}
}

// Per ADR 160 the row only ever warns: it names the difference and lets the
// install proceed.
func TestDoctor_DeliveryModulesRow(t *testing.T) {
	rowFor := func(vars map[string]string) doctorRow {
		return checkDeliveryModules(envOf(vars))
	}

	if r := rowFor(nil); r.Status != statusPass || !strings.Contains(r.Summary, "built-in") {
		t.Errorf("unset: %+v, want a pass naming the built-in path", r)
	}

	dir := t.TempDir()
	if r := rowFor(map[string]string{"FLEET_DELIVERY_MODULES": "relay-a", "FLEET_MODULES_DIR": dir}); r.Status != statusWarn ||
		!strings.Contains(r.Detail, `"relay-a"`) || strings.Contains(r.Detail, dir) {
		t.Errorf("enabled but absent: %+v, want a warn naming the module and not the directory", r)
	}

	installFakeModule(t, dir, "relay-a")
	if r := rowFor(map[string]string{"FLEET_DELIVERY_MODULES": "relay-a", "FLEET_MODULES_DIR": dir}); r.Status != statusPass {
		t.Errorf("enabled and present: %+v, want a pass", r)
	}

	if r := rowFor(map[string]string{"FLEET_DELIVERY_MODULES": "Relay"}); r.Status != statusWarn {
		t.Errorf("an illegal name: %+v, want a warn", r)
	}

	// The row never fails, whatever it finds.
	for _, vars := range []map[string]string{
		{"FLEET_DELIVERY_MODULES": "a,b,c", "FLEET_MODULES_DIR": "/does/not/exist"},
		{"FLEET_DELIVERY_MODULES": "auto,terminal"},
	} {
		if r := rowFor(vars); r.Status == statusFail {
			t.Errorf("%v: the row failed; ADR 160 says a missing module is a warn", vars)
		}
	}

	// It is a real row of the doctor: --skip accepts its id.
	env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m", "FLEET_TOKEN": "t"})
	env.Skip = map[string]bool{"delivery.modules": true}
	rows := runChecks(t.Context(), env)
	if r := rowByID(t, rows, "delivery.modules"); r.Status != statusSkip {
		t.Errorf("--skip=delivery.modules did not skip it: %+v", r)
	}
}
