package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plantedWorld(t *testing.T) (scratch string, dirs map[string]string) {
	t.Helper()
	scratch = t.TempDir()
	dirs = compatDirs(scratch)
	for _, role := range compatConfigRoles {
		if err := os.MkdirAll(filepath.Join(dirs[role], ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := plantCompatConfiguration(scratch, dirs); err != nil {
		t.Fatal(err)
	}
	return scratch, dirs
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// Each of the four sources touches a file of its own, inside the directory of
// the session it belongs to — the evidence is a directory listing, so two
// sources sharing a name, or a source touching its neighbour's directory, would
// make the check say nothing.
func TestPlantCompatConfigurationGivesEachSourceItsOwnSentinel(t *testing.T) {
	scratch, dirs := plantedWorld(t)
	for _, role := range compatConfigRoles {
		dir := dirs[role]
		hookCmd := func(file string) string {
			m := readJSON(t, filepath.Join(dir, ".claude", file))
			hooks := m["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
			return hooks["command"].(string)
		}
		project, local := hookCmd("settings.json"), hookCmd("settings.local.json")
		if !strings.Contains(project, filepath.Join(dir, cfgProjectHook)) || !strings.Contains(local, filepath.Join(dir, cfgLocalHook)) {
			t.Errorf("%s: hooks do not touch their own sentinels: %q / %q", role, project, local)
		}
		ambient := readJSON(t, filepath.Join(dir, ".mcp.json"))["mcpServers"].(map[string]any)["ambient"].(map[string]any)["args"].([]any)[1].(string)
		named := readJSON(t, compatConfigMcpFile(scratch, role))["mcpServers"].(map[string]any)["named"].(map[string]any)["args"].([]any)[1].(string)
		if !strings.Contains(ambient, filepath.Join(dir, cfgAmbientMcp)) || !strings.Contains(named, filepath.Join(dir, cfgNamedMcp)) {
			t.Errorf("%s: servers do not touch their own sentinels: %q / %q", role, ambient, named)
		}
		// The project file keeps remote control off — the local file that normally
		// says so is the layer a confined session does not load — and approves the
		// project's own server so the control session is not parked on a question.
		ps := readJSON(t, filepath.Join(dir, ".claude", "settings.json"))
		if ps["remoteControlAtStartup"] != false || ps["enableAllProjectMcpServers"] != true {
			t.Errorf("%s: project settings = %v", role, ps)
		}
	}
	// The named server file is in the scratch directory, never inside a working
	// directory the session could read as its own configuration.
	for _, role := range compatConfigRoles {
		if strings.HasPrefix(compatConfigMcpFile(scratch, role), dirs[role]) {
			t.Errorf("%s: the --mcp-config file sits inside the working directory", role)
		}
	}
}

func seenAll(names ...string) map[string]bool {
	m := map[string]bool{cfgProjectHook: false, cfgLocalHook: false, cfgAmbientMcp: false, cfgNamedMcp: false}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestConfigurationVerdicts(t *testing.T) {
	control := compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook, cfgLocalHook, cfgAmbientMcp, cfgNamedMcp)}
	cases := []struct {
		name         string
		g, c         compatConfigSeen
		pass, errd   bool
		detailHas    string
		detailHasNot string
	}{
		{"the requirement keeps what it names and leaves the rest out",
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook, cfgNamedMcp)}, control, true, false, "ran the project hook", ""},
		{"a leaked local hook fails",
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook, cfgNamedMcp, cfgLocalHook)}, control, false, false, "local settings' hook", ""},
		{"a leaked project tool server fails",
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook, cfgNamedMcp, cfgAmbientMcp)}, control, false, false, "project's own tool server", ""},
		{"dropping the named tool server fails",
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook)}, control, false, false, "did not run the tool server named with --mcp-config", ""},
		{"dropping the project's own hook fails",
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgNamedMcp)}, control, false, false, "did not run the project settings' hook", ""},
		{"a session that never shows a composer fails",
			compatConfigSeen{boot: compatBoot{ready: false}, seen: seenAll()}, control, false, false, "never showed a composer", ""},
		{"a control that did not run every source cannot judge",
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook, cfgNamedMcp)},
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook, cfgNamedMcp, cfgLocalHook)}, false, true, "premise is missing", ""},
		{"a control that never booted cannot judge",
			compatConfigSeen{boot: compatBoot{ready: true}, seen: seenAll(cfgProjectHook, cfgNamedMcp)},
			compatConfigSeen{boot: compatBoot{ready: false}, seen: seenAll()}, false, true, "never showed a composer", ""},
	}
	for _, tc := range cases {
		h := &compatHarness{}
		h.ev.g, h.ev.gc = tc.g, tc.c
		v := h.evalConfiguration()
		if v.Pass != tc.pass || v.Err != tc.errd {
			t.Errorf("%s: pass=%v err=%v (%s), want pass=%v err=%v", tc.name, v.Pass, v.Err, v.Detail, tc.pass, tc.errd)
		}
		if !strings.Contains(v.Detail, tc.detailHas) {
			t.Errorf("%s: detail %q lacks %q", tc.name, v.Detail, tc.detailHas)
		}
	}
}
