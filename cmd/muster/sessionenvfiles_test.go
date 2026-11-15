package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// muster #487: the doctor reads, before any create, what a create would leave
// out silently — an optional sessionEnv file that is there and unusable. A file
// that does not exist stays quiet.
func TestCheckSessionEnvFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	entry := func(name, file string, required bool) map[string]any {
		return map[string]any{"name": name, "fromFile": file, "required": required}
	}
	gone := filepath.Join(dir, "never-written")
	pretty := write("pretty", "{\n  \"agent\": {}\n}\n")
	good := write("good", "one-line")

	cases := []struct {
		name       string
		entries    []map[string]any
		wantRow    bool
		wantStatus rowStatus
		summary    string
		detail     string
	}{
		{name: "an optional file with an interior newline warns",
			entries: []map[string]any{entry("OPENCODE_CONFIG_CONTENT", pretty, false)},
			wantRow: true, wantStatus: statusWarn,
			summary: "creates start without OPENCODE_CONFIG_CONTENT", detail: "OPENCODE_CONFIG_CONTENT: value contains a newline"},
		{name: "an empty optional file warns",
			entries: []map[string]any{entry("EMPTY_VAR", write("empty", ""), false)},
			wantRow: true, wantStatus: statusWarn, summary: "EMPTY_VAR", detail: "EMPTY_VAR: file is empty"},
		{name: "an absent optional file stays quiet",
			entries: []map[string]any{entry("GONE_VAR", gone, false)},
			wantRow: true, wantStatus: statusPass, summary: "1 absent, which an optional entry allows"},
		{name: "a usable optional file passes",
			entries: []map[string]any{entry("GOOD_VAR", good, false)},
			wantRow: true, wantStatus: statusPass, summary: "none is present but unusable"},
		{name: "only the bad one is named",
			entries: []map[string]any{entry("GOOD_VAR", good, false), entry("BAD_VAR", pretty, false), entry("GONE_VAR", gone, false)},
			wantRow: true, wantStatus: statusWarn, summary: "1 optional sessionEnv file(s) are present but unusable — creates start without BAD_VAR"},
		{name: "a required entry is not this row's to judge",
			entries: []map[string]any{entry("REQ_VAR", pretty, true)}, wantRow: false},
		{name: "an entry that reads the session's identity has no file",
			entries: []map[string]any{{"name": "ID_VAR", "fromSession": "id"}}, wantRow: false},
		{name: "no entries, no row", entries: nil, wantRow: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			extra := map[string]any{}
			if len(c.entries) > 0 {
				extra["sessionEnv"] = c.entries
			}
			row, ok := checkSessionEnvFiles(loadStartupDoc(t, startupDoc(extra)))
			if ok != c.wantRow {
				t.Fatalf("row present = %v, want %v (%+v)", ok, c.wantRow, row)
			}
			if !ok {
				return
			}
			if row.ID != "sessionenv.files" || row.Status != c.wantStatus {
				t.Errorf("row = %s %s (%s / %s), want sessionenv.files %s", row.ID, row.Status, row.Summary, row.Detail, c.wantStatus)
			}
			if !strings.Contains(row.Summary, c.summary) {
				t.Errorf("summary %q does not say %q", row.Summary, c.summary)
			}
			if !strings.Contains(row.Detail, c.detail) {
				t.Errorf("detail %q does not say %q", row.Detail, c.detail)
			}
			for _, leak := range []string{dir, "\"agent\""} {
				if strings.Contains(row.Summary, leak) || strings.Contains(row.Detail, leak) {
					t.Errorf("the row carries a path or a value (%q): %s / %s", leak, row.Summary, row.Detail)
				}
			}
		})
	}
	if row, ok := checkSessionEnvFiles(nil); ok {
		t.Errorf("a config that did not load must leave the row out, got %+v", row)
	}
}

// The doctor's report, end to end: the warning is in it, and a warning does not
// turn the exit code red — the service starts and creates go on, which is why the
// row exists at all.
func TestDoctorReportsAnUnusableOptionalSessionEnvFile(t *testing.T) {
	dir := t.TempDir()
	pretty := filepath.Join(dir, "pretty")
	if err := os.WriteFile(pretty, []byte("a\nb"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "fleet.json")
	putCfg(t, cfgPath, startupDoc(map[string]any{
		"sessionEnv": []map[string]any{{"name": "OPENCODE_CONFIG_CONTENT", "fromFile": pretty}},
	}))
	e := testDoctorEnv(map[string]string{"FLEET_CONFIG": cfgPath})
	e.Offline = true
	rows := runChecks(context.Background(), e)
	row := rowByID(t, rows, "sessionenv.files")
	if row.Status != statusWarn || !strings.Contains(row.Summary, "OPENCODE_CONFIG_CONTENT") {
		t.Fatalf("row = %+v, want a warning naming OPENCODE_CONFIG_CONTENT", row)
	}
	if cfg := rowByID(t, rows, "config.load"); cfg.Status != statusPass {
		t.Errorf("config.load = %+v; an unusable optional file must not stop the start", cfg)
	}
}
