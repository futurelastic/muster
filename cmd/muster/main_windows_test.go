package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWindowsRefusesTmuxRuntime: on Windows the terminal runtime is not a choice
// (ADR 303), so FLEET_RUNTIME=tmux must stop the service at startup — non-zero
// exit, naming the decision — rather than start and fail later on a missing
// binary. It runs the real serve path as a child process.
func TestWindowsRefusesTmuxRuntime(t *testing.T) {
	cmd := exec.Command(os.Args[0], "serve")
	cmd.Env = append(os.Environ(), asMainEnv+"=1", "FLEET_RUNTIME=tmux", "FLEET_TOKEN=secret", "FLEET_ADDR=127.0.0.1:0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() == 0 {
			t.Fatalf("the service did not refuse: err = %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the service did not exit\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ADR 303") {
		t.Errorf("the refusal does not name ADR 303:\n%s", out.String())
	}
}

// TestWindowsDefaultRuntimeIsNone: with FLEET_RUNTIME unset the service must
// choose "none" on its own. It is the same observation TestServeWithNoLocalDriver
// makes with the variable set; here the default is what is under test.
func TestWindowsDefaultRuntimeIsNone(t *testing.T) {
	if got := defaultRuntime(); got != "none" {
		t.Fatalf("defaultRuntime() = %q on windows, want none", got)
	}
}

// TestWindowsCompatRefuses: muster compat has no terminal runtime to check.
func TestWindowsCompatRefuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := runCompat([]string{"compat", "--claude", `C:\x\claude.exe`}, func(string) string { return "" }, &stdout, &stderr)
	if !handled || code != 2 || !strings.Contains(stderr.String(), "ADR 303") {
		t.Errorf("handled=%v code=%d stderr=%q", handled, code, stderr.String())
	}
}

// TestWindowsServiceTaskRowReadsTheTask drives checkServiceTask with what the
// scheduler would print, so the row's wiring (task name, missing task, parse
// failure) is checked without registering a real task.
func TestWindowsServiceTaskRowReadsTheTask(t *testing.T) {
	orig := querySchtasks
	t.Cleanup(func() { querySchtasks = orig })
	exe, _ := os.Executable()

	var asked string
	querySchtasks = func(name string) ([]byte, error) {
		asked = name
		return utf16le(strings.Replace(goodTaskXML, `C:\Users\someone\AppData\Local\Programs\muster\muster.exe`, exe, 1), true), nil
	}
	row, ok := checkServiceTask(func(k string) string {
		if k == "FLEET_TASK_NAME" {
			return "muster-test"
		}
		return ""
	})
	if !ok || row.Status != statusPass || asked != "muster-test" {
		t.Errorf("row = %+v ok=%v asked=%q", row, ok, asked)
	}

	// The row is part of what doctor reports on Windows: through runChecks, not
	// only through the function that builds it.
	origRow := doctorServiceTask
	doctorServiceTask = checkServiceTask
	t.Cleanup(func() { doctorServiceTask = origRow })
	rows := runChecks(context.Background(), testDoctorEnv(map[string]string{"FLEET_RUNTIME": "none", "FLEET_TOKEN": "t"}))
	if r := rowByID(t, rows, "service.task"); r.Status != statusPass {
		t.Errorf("doctor's service.task row = %+v, want pass", r)
	}

	querySchtasks = func(string) ([]byte, error) { return nil, errors.New("not found") }
	if row, _ := checkServiceTask(func(string) string { return "" }); row.Status != statusFail {
		t.Errorf("a missing task: %+v, want fail", row)
	}

	querySchtasks = func(string) ([]byte, error) { return []byte("not xml"), nil }
	if row, _ := checkServiceTask(func(string) string { return "" }); row.Status != statusUnknown {
		t.Errorf("an unreadable definition: %+v, want unknown", row)
	}
}
