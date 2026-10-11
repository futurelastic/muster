package main

import (
	"runtime"
	"testing"
)

// skipOnWindows skips a test whose subject is a Unix tool or a POSIX mode.
//
// The Windows gate (scripts/gate-windows.sh, #303) prints every skip, and each
// one must say why in a message ending "(windows, #303)" so the summary can be
// read without opening the test. A skip is green to `go test`; this is what
// keeps a deliberate one distinguishable from a test that quietly stopped
// running.
func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skipf("%s (windows, #303)", why)
	}
}
