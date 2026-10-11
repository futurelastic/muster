package main

import (
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
	os.Exit(m.Run())
}
