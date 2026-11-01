package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// muster #386: a sessionEnv entry's fromFile is "absolute" by the HOST's rule,
// not by a leading "/". The old test was Unix-only, so a Windows machine could
// not declare a fromFile at all — every drive or UNC path was refused at start
// with "must be an absolute path", and `muster doctor`'s config.load row failed
// the same way, because both go through validateConfigExtras.
//
// This lives in cmd/muster, not beside sessionenv.Validate, because this
// package is in the Windows gate (scripts/gate-windows.sh) and internal/sessionenv
// is not: a case written there would pass on every machine that runs it and
// never run on the one it is about. The name is on that script's MUST_PASS list.
func TestSessionEnvFromFileIsJudgedByTheHostsAbsoluteRule(t *testing.T) {
	const (
		refusal = "must be an absolute path"
		// A leaf no row text contains, so the leak check below means what it says.
		leaf = "fromfile-probe"
	)
	sep := string(filepath.Separator)

	type fromFileCase struct {
		name string
		file string
		want string // "" = the file starts
	}
	cases := []fromFileCase{
		// Built from the platform's own temp dir, so it is absolute in whichever
		// form this host writes: /tmp/…/<leaf> on Unix, C:\Users\…\<leaf> on Windows.
		{"a temp-dir path, absolute on this host", filepath.Join(t.TempDir(), leaf), ""},

		// Relative is refused on every host, with the same message.
		{"a bare file name", leaf, refusal},
		{"a dot-relative path", "." + sep + leaf, refusal},
		{"a parent-relative path", ".." + sep + leaf, refusal},
		{"a nested relative path", filepath.Join("secrets", leaf), refusal},
	}
	if hostOS() == "windows" {
		cases = append(cases,
			fromFileCase{"a drive path", `C:\ProgramData\muster\` + leaf, ""},
			fromFileCase{"a UNC path", `\\fileserver\share\muster\` + leaf, ""},
			// Rooted but not absolute: no drive, so it resolves against the
			// current drive. A leading "/" alone no longer counts here.
			fromFileCase{"a rooted path with no drive", `\ProgramData\muster\` + leaf, refusal},
			fromFileCase{"a drive-relative path", `C:` + leaf, refusal},
			fromFileCase{"a leading slash with no drive", "/etc/fleet/" + leaf, refusal},
		)
	} else {
		cases = append(cases,
			fromFileCase{"a rooted Unix path", "/etc/fleet/" + leaf, ""},
			// A Windows path is just a relative name here; the rule is the host's.
			fromFileCase{"a Windows drive path", `C:\ProgramData\muster\` + leaf, refusal},
		)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fleet.json")
			putCfg(t, path, startupDoc(map[string]any{"sessionEnv": []any{
				map[string]any{"name": "FLEET_TOKEN", "fromFile": c.file, "required": true},
			}}))
			cfg, err := loadConfig(path)
			if err != nil {
				t.Fatalf("the test's own config does not load: %v", err)
			}

			// "none" is the one runtime that is a choice on every platform
			// (ADR 303); this entry carries no appliesTo, so it needs no other.
			env := map[string]string{"FLEET_CONFIG": path, "FLEET_RUNTIME": "none"}
			startErr := validateConfigExtras(cfg, envOf(env))
			row := rowByID(t, runChecks(context.Background(), testDoctorEnv(env)), "config.load")

			if c.want == "" {
				if startErr != nil {
					t.Fatalf("a fromFile that is absolute on this host was refused at start: %v", startErr)
				}
				if row.Status != statusPass {
					t.Fatalf("config.load = %+v, want pass (start accepted the same file)", row)
				}
				return
			}
			if startErr == nil || !strings.Contains(startErr.Error(), c.want) {
				t.Fatalf("start = %v, want a refusal saying %q", startErr, c.want)
			}
			if row.Status != statusFail || !strings.Contains(row.Detail, c.want) {
				t.Fatalf("config.load = %+v, want a fail saying %q (the same verdict as start)", row, c.want)
			}
			if strings.Contains(row.Detail+row.Summary, c.file) {
				t.Fatalf("config.load leaks the fromFile path: %+v", row)
			}
		})
	}
}
