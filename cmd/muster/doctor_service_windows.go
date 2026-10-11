package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaultTaskName is the name docs/install.md registers the task under.
const defaultTaskName = "muster"

// querySchtasks is the one place Task Scheduler is asked. A variable so a test
// can supply what it printed.
var querySchtasks = func(name string) (out []byte, err error) {
	cmd := exec.Command("schtasks.exe", "/Query", "/TN", name, "/XML")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return nil, errors.New("schtasks did not answer within 10s")
	}
	if err != nil {
		// A missing task is the one failure with its own meaning. Its text is
		// localised, so the exit status carries it: schtasks exits 1 for any
		// error, which is why the stderr text is only attached, never matched.
		return nil, fmt.Errorf("schtasks exited with an error: %s", strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// checkServiceTask answers the service.task row: does the scheduled task that
// runs this service exist, and is it set up so it keeps running?
//
// A task that cannot be read is "unknown", not a pass: the point of the row is
// to say what is true of the task, and a row that cannot see it says so.
func checkServiceTask(getenv func(string) string) (doctorRow, bool) {
	name := getenv("FLEET_TASK_NAME")
	if name == "" {
		name = defaultTaskName
	}
	row := doctorRow{ID: "service.task"}
	raw, err := querySchtasks(name)
	if err != nil {
		// Exit status 1 is also "access denied" and "schtasks missing" in some
		// shapes, so this is reported as a failure to find, with what it said.
		row.Status = statusFail
		row.Summary = fmt.Sprintf("no scheduled task named %q could be read — the service is not set up to start at log-on", name)
		row.Detail = err.Error() + "; register it as docs/install.md describes, or name it with FLEET_TASK_NAME"
		return row, true
	}
	info, err := parseTaskXML(raw)
	if err != nil {
		row.Status = statusUnknown
		row.Summary = fmt.Sprintf("the scheduled task %q was found but its definition could not be read", name)
		row.Detail = err.Error()
		return row, true
	}
	exe, _ := os.Executable()
	return judgeTask(info, exe), true
}
