//go:build !windows

package modtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sidecarBehaviour has no sidecar to read here: Install puts the behaviour in
// the wrapper script itself.
func sidecarBehaviour() string { return "" }

// Install writes an executable /bin/sh wrapper at dir/name that re-executes the
// running test binary as a fake module with behaviour b, and returns its path.
// The behaviour rides in the wrapper itself, not the parent's environment,
// because the launcher gives the child a minimal environment. (So the child's
// environment also holds EnvBehaviour and GORACE, and whatever the shell adds.)
func Install(t testing.TB, dir, name string, b Behaviour) string {
	t.Helper()
	blob, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	// GORACE: a race-instrumented binary sleeps a second at exit by default to
	// flush reports; a module that has been told to stop should not.
	script := "#!/bin/sh\n" + EnvBehaviour + "=" + shQuote(string(blob)) + " GORACE=atexit_sleep_ms=0 exec " + shQuote(exe) + " -test.run='^$' \"$@\"\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// shQuote single-quotes s for /bin/sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
