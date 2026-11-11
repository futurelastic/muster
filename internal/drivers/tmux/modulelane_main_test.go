package tmux

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/sandbox"
)

// TestMain lets this test binary stand in for a real external delivery module
// (#185): when it is started with a behaviour in its environment it serves the
// module protocol on stdio and exits; otherwise it runs the tests. That is what
// lets modulelane_e2e_test.go drive a REAL child process through the real
// launcher — pipes, a minimal environment, process groups — with nothing built.
func TestMain(m *testing.M) {
	// muster #439: on Linux this test binary is the sandbox launcher, as the
	// service's own binary is in production, and the two small tools the live
	// sandbox test's stand-in agent connects with.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case sandbox.LauncherArg:
			os.Exit(sandbox.LauncherMain(os.Args[2:]))
		case "__unixdial", "__tcpdial":
			network := "unix"
			if os.Args[1] == "__tcpdial" {
				network = "tcp"
			}
			c, err := net.DialTimeout(network, os.Args[2], 2*time.Second)
			if err != nil {
				fmt.Println("denied:", err)
				os.Exit(1)
			}
			_ = c.Close()
			fmt.Println("connected")
			os.Exit(0)
		}
	}
	modtest.MaybeServe()
	os.Exit(m.Run())
}
