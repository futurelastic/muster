package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/sandbox"
)

// muster #456: a launcher that declines to start the runtime is not a lost port
// race. startServer retries a child that dies before it answers, because that is
// what losing the port to another process looks like; the launcher's refusal is
// the same on every attempt, so retrying it only delays it and hides its reason.
//
// The launcher here is a script that says what the real one says (a line on
// standard error that begins with the launcher's prefix, and its exit status), so
// the wiring is checked on every platform with a shell. The real launcher's own
// refusal reaching the same error is sandbox_linux_test.go's.

// refusingLauncher is a launcher stand-in: it writes stderr, exits with status,
// and never starts the runtime named after it. The returned counter is how many
// times startServer asked for a command, which is how many attempts it made.
func refusingLauncher(t *testing.T, stderr string, status int) (launchFunc, *int) {
	t.Helper()
	skipOnWindows(t, "needs a shell script for a launcher")
	script := filepath.Join(t.TempDir(), "launcher")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q >&2\nexit %d\n", stderr, status)
	if stderr == "" {
		body = fmt.Sprintf("#!/bin/sh\nexit %d\n", status)
	}
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	return func(port int) ([]string, error) {
		attempts++
		return []string{script}, nil
	}, &attempts
}

func startWith(t *testing.T, launch launchFunc) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The runtime is never reached when the launcher refuses; a path that cannot
	// run keeps that honest.
	_, err := startServer(ctx, filepath.Join(t.TempDir(), "never-run"), t.TempDir(), "muster", nil, launch)
	return err
}

func TestStartServerDoesNotRetryTheLaunchersRefusalAndReturnsItsReason(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		line   string
	}{
		{"refused before running anything", 125, "muster sandbox: sandbox.readPaths: /srv/gone does not exist"},
		{"the exec of the runtime failed", 127, "muster sandbox: starting /opt/rt/opencode: permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			launch, attempts := refusingLauncher(t, c.line, c.status)
			err := startWith(t, launch)
			if !errors.Is(err, errLauncherRefused) {
				t.Fatalf("err = %v, want the launcher's refusal", err)
			}
			if *attempts != 1 {
				t.Fatalf("startServer started the launcher %d times; a refusal is not a port race and is not retried", *attempts)
			}
			why := strings.TrimPrefix(c.line, "muster sandbox: ")
			if !strings.Contains(err.Error(), why) {
				t.Fatalf("the launcher's own reason did not reach the error:\n  want it to contain %q\n  got  %v", why, err)
			}
			if strings.Contains(err.Error(), "did not become ready") {
				t.Fatalf("the reason is hidden behind the generic start error: %v", err)
			}
		})
	}
}

// What marks a launcher's refusal is the pair, and neither half stands alone: a
// runtime may exit 125 or 127 for reasons of its own, and any process may print a
// line that looks like the launcher's. Mistaking either for a refusal would stop a
// start that a retry fixes.
func TestStartServerStillRetriesWhatIsOnlyHalfALauncherRefusal(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		line   string
		wrap   bool
	}{
		{"the launcher's status with no line of its own", 125, "", true},
		{"the launcher's line with another status", 1, "muster sandbox: looks like a refusal", true},
		{"no launcher at all", 125, "muster sandbox: written by the runtime itself", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			launch, attempts := refusingLauncher(t, c.line, c.status)
			if !c.wrap {
				// A plain start: the script is the runtime itself, with no launcher in front.
				script := filepath.Join(t.TempDir(), "runtime")
				body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q >&2\nexit %d\n", c.line, c.status)
				if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, err := startServer(ctx, script, t.TempDir(), "muster", nil, nil)
				if err == nil || errors.Is(err, errLauncherRefused) || !strings.Contains(err.Error(), "after 3 attempts") {
					t.Fatalf("a runtime's own exit was treated as a launcher refusal: %v", err)
				}
				return
			}
			err := startWith(t, launch)
			if err == nil || errors.Is(err, errLauncherRefused) {
				t.Fatalf("err = %v, want a start that failed after its attempts", err)
			}
			if *attempts != startAttempts || !strings.Contains(err.Error(), "after 3 attempts") {
				t.Fatalf("attempts = %d, err = %v; want the %d attempts a port race is given", *attempts, err, startAttempts)
			}
		})
	}
}

// The launcher's own entry point, the one the service's binary answers to, is
// what refuses here: this test binary is the launcher (fakeopencode_test.go's
// TestMain), so the status and the line startServer reads are the real ones and
// not a script's idea of them. A document that is not a policy is refused before
// anything is asked of the kernel, so this holds on any Linux whatever its kernel
// offers; elsewhere the entry point refuses as well, saying the mechanism is not
// there.
func TestStartServerReadsTheRealLauncherEntryPointsRefusal(t *testing.T) {
	skipOnWindows(t, "no sandbox launcher on this platform")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	launch := func(int) ([]string, error) {
		attempts++
		return []string{exe, sandbox.LauncherArg, "--policy", "this is not a policy document", "--"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = startServer(ctx, exe, t.TempDir(), "muster", nil, launch)
	if !errors.Is(err, errLauncherRefused) || attempts != 1 {
		t.Fatalf("attempts = %d, err = %v; want one start, refused by the launcher", attempts, err)
	}
	want := "is not available on this platform"
	if runtime.GOOS == "linux" {
		want = "the policy does not parse"
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the launcher's own words did not reach the error: want %q in %v", want, err)
	}
}
