package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// #333: the row reads the version of the tmux the service will run. The version
// is handed in, never the machine's own.
func TestCheckTmuxVersion(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/tmux", nil }
	notFound := func(string) (string, error) { return "", errors.New("not found") }
	say := func(out string, err error) func(context.Context, string) (string, error) {
		return func(context.Context, string) (string, error) { return out, err }
	}
	for _, tc := range []struct {
		name    string
		runtime string
		bin     string
		look    func(string) (string, error)
		ver     func(context.Context, string) (string, error)
		want    rowStatus
		summary string
	}{
		{"distribution tmux 3.4 warns", "tmux", "", found, say("tmux 3.4", nil), statusWarn, "cannot report #{bracket_paste_flag}"},
		{"3.6a warns", "tmux", "", found, say("tmux 3.6a", nil), statusWarn, "tmux 3.6a"},
		{"3.7 passes", "tmux", "", found, say("tmux 3.7", nil), statusPass, "provides #{bracket_paste_flag}"},
		{"an explicit binary is the one asked", "tmux", "/opt/tmux/bin/tmux", notFound, say("tmux 3.7c", nil), statusPass, "tmux 3.7c"},
		{"a development build is unknown, not a pass", "tmux", "", found, say("tmux next-3.8", nil), statusUnknown, "not recognised"},
		{"a binary that will not answer is unknown", "tmux", "", found, say("", errors.New("exit 1")), statusUnknown, "could not run"},
		{"no tmux at all is left to the runtime row", "tmux", "", notFound, say("tmux 3.4", nil), statusSkip, "runtime row"},
		{"another runtime does not run tmux", "stub", "", found, say("tmux 3.4", nil), statusSkip, "does not run tmux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := doctorTmuxVersion
			doctorTmuxVersion = tc.ver
			defer func() { doctorTmuxVersion = old }()
			env := testDoctorEnv(nil)
			env.LookPath = tc.look
			row := checkTmuxVersion(context.Background(), tc.runtime, tc.bin, env)
			if row.ID != "runtime.tmux-version" {
				t.Errorf("id = %q", row.ID)
			}
			if row.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", row.Status, tc.want, row.Summary)
			}
			if !strings.Contains(row.Summary, tc.summary) {
				t.Errorf("summary = %q, want it to contain %q", row.Summary, tc.summary)
			}
			if tc.want == statusWarn && !strings.Contains(row.Detail, "FLEET_TMUX_BIN") {
				t.Errorf("a warning row must say how to fix it; detail = %q", row.Detail)
			}
		})
	}
}

// The row is part of a doctor run and is skippable by id. An older tmux is a
// warning, not a failure: since #334 its sends are delivered, just without the
// flag check (ADR 160).
func TestDoctorRunsTheTmuxVersionRow(t *testing.T) {
	old := doctorTmuxVersion
	doctorTmuxVersion = func(context.Context, string) (string, error) { return "tmux 3.4", nil }
	defer func() { doctorTmuxVersion = old }()

	rows := runChecks(context.Background(), testDoctorEnv(nil))
	if r := rowByID(t, rows, "runtime.tmux-version"); r.Status != statusWarn {
		t.Fatalf("tmux 3.4: status = %s, want warn", r.Status)
	}

	env := testDoctorEnv(nil)
	env.Skip = map[string]bool{"runtime.tmux-version": true}
	rows = runChecks(context.Background(), env)
	if r := rowByID(t, rows, "runtime.tmux-version"); r.Status != statusSkip {
		t.Fatalf("--skip: status = %s, want skip", r.Status)
	}
}
