package sandbox

import (
	"os"
	"testing"
)

// TestMain lets a test binary stand in for the muster binary: on Linux it is
// the launcher when started with LauncherArg, and the probe a confined child
// runs when started with __probe (main_linux_test.go). Anywhere else it only runs
// the tests.
func TestMain(m *testing.M) { os.Exit(testMain(m)) }
