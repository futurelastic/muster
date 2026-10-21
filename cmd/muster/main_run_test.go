package main

import (
	"context"
	"os"
	"testing"
)

// asMainEnv makes this test binary behave as the muster binary, so a test can
// start the real `serve` path — runtime choice, log file, listeners — as a child
// process and look at what it does, with no separate build step.
const asMainEnv = "MUSTER_TEST_AS_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(asMainEnv) == "1" {
		main()
		os.Exit(0)
	}
	// The generic doctor tests run on whatever machine this is; whether it has a
	// scheduled task of the service's name is not what they are about.
	doctorServiceTask = func(func(string) string) (doctorRow, bool) { return doctorRow{}, false }
	// Likewise which tmux, if any, the machine has: the runtime.tmux-version row
	// is tested on its own with versions it is handed.
	doctorTmuxVersion = func(context.Context, string) (string, error) { return "tmux 3.7a", nil }
	// And whether it has the agent CLI: runtime.agent-program is tested on its own
	// with answers it is handed.
	doctorAgentProgram = func(context.Context, string) (bool, bool) { return true, true }
	os.Exit(m.Run())
}
