package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// #342: the row asks whether the agent program is found where a session starts
// it. The answer is handed in, never the machine's own.
func TestCheckAgentProgram(t *testing.T) {
	say := func(found, known bool) func(context.Context, string) (bool, bool) {
		return func(context.Context, string) (bool, bool) { return found, known }
	}
	for _, tc := range []struct {
		name    string
		runtime string
		ask     func(context.Context, string) (bool, bool)
		want    rowStatus
		summary string
	}{
		{"found passes", "tmux", say(true, true), statusPass, "is found"},
		{"missing fails with the cause", "tmux", say(false, true), statusFail, "not-installed"},
		{"a probe that could not decide is unknown, not a failure", "tmux", say(false, false), statusUnknown, "could not tell"},
		{"another runtime starts no agent program", "stub", say(false, true), statusSkip, "does not start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := doctorAgentProgram
			doctorAgentProgram = tc.ask
			defer func() { doctorAgentProgram = old }()
			row := checkAgentProgram(context.Background(), tc.runtime, time.Second)
			if row.ID != "runtime.agent-program" || row.Status != tc.want || !strings.Contains(row.Summary, tc.summary) {
				t.Errorf("row = %+v, want %s containing %q", row, tc.want, tc.summary)
			}
			if tc.want == statusFail && !strings.Contains(row.Detail, "runtime-unavailable") {
				t.Errorf("a failing row must say what the service does meanwhile; detail = %q", row.Detail)
			}
			if strings.Contains(row.Summary+row.Detail, "/") && tc.want != statusFail {
				t.Errorf("row carries a path: %+v", row)
			}
		})
	}
}

func TestDoctorRunsTheAgentProgramRow(t *testing.T) {
	old := doctorAgentProgram
	doctorAgentProgram = func(context.Context, string) (bool, bool) { return false, true }
	defer func() { doctorAgentProgram = old }()

	rows := runChecks(context.Background(), testDoctorEnv(nil))
	if r := rowByID(t, rows, "runtime.agent-program"); r.Status != statusFail {
		t.Fatalf("missing program: status = %s, want fail", r.Status)
	}
	env := testDoctorEnv(nil)
	env.Skip = map[string]bool{"runtime.agent-program": true}
	rows = runChecks(context.Background(), env)
	if r := rowByID(t, rows, "runtime.agent-program"); r.Status != statusSkip {
		t.Fatalf("--skip: status = %s, want skip", r.Status)
	}
}
