package main

import (
	"context"
	"os"
	"testing"

	"github.com/futurelastic/muster/internal/sandbox"
)

// asMainEnv makes this test binary behave as the muster binary, so a test can
// start the real `serve` path — runtime choice, log file, listeners — as a child
// process and look at what it does, with no separate build step.
const asMainEnv = "MUSTER_TEST_AS_MAIN"

func TestMain(m *testing.M) {
	// muster #456: the service, run as this binary, starts the opencode runtime
	// through itself as the sandbox launcher to measure it, in an environment it
	// BUILDS, which carries no asMainEnv: so the launcher is recognised by argv, as
	// main does, or that child would be a second copy of this whole test run.
	if len(os.Args) > 1 && os.Args[1] == sandbox.LauncherArg {
		os.Exit(sandbox.LauncherMain(os.Args[2:]))
	}
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
	// And what the host can offer as a sandbox: the real check starts a confined child
	// on Linux and applies a profile on macOS. sandbox.host is tested on its own with
	// reports it is handed.
	doctorSandboxHost = func() *sandbox.HostReport {
		return &sandbox.HostReport{Platform: "test", Reason: "no sandbox mechanism is implemented for a test host"}
	}
	os.Exit(m.Run())
}
