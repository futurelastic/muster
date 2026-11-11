package sandbox

import (
	"os"
	"strconv"
	"testing"

	fleet "github.com/futurelastic/muster"
)

func testMain(m *testing.M) int {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case LauncherArg:
			// The seams a test binary has and the service does not: nothing in the
			// service reads these variables, and only this function sets the values
			// they lower. They let the rules that need no newer kernel than an
			// ordinary runner's be proved there, and the scope layer be proved
			// without the filter that would otherwise get there first.
			if v := os.Getenv("MUSTER_SANDBOX_TEST_MIN_ABI"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					minABI = n
				}
			}
			if os.Getenv("MUSTER_SANDBOX_TEST_LANDLOCK_ONLY") == "1" {
				applySeccomp = func(fleet.SandboxNetwork) error { return nil }
			}
			return LauncherMain(os.Args[2:])
		case "__probe":
			return probeMain(os.Args[2:])
		}
	}
	return m.Run()
}
