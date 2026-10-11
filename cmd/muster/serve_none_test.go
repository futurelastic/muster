package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// startServe runs `muster serve` as a child process under env and returns it
// with its stderr scanner. The child is killed when the test ends.
func startServe(t *testing.T, env ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "serve")
	cmd.Env = append(os.Environ(), append([]string{asMainEnv + "=1"}, env...)...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, bufio.NewScanner(stderr)
}

var listeningRe = regexp.MustCompile(`listening on (127\.0\.0\.1:\d+) `)

// TestServeWithNoLocalDriver is the oracle for FLEET_RUNTIME=none: the service
// starts with nothing registered, lists no runtimes, and answers. Runs on every
// platform because "none" is not Windows-specific; only its being the default is.
func TestServeWithNoLocalDriver(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "muster.log")
	_, sc := startServe(t,
		"FLEET_RUNTIME=none", "FLEET_TOKEN=secret", "FLEET_MACHINE=testbox",
		"FLEET_ADDR=127.0.0.1:0", "FLEET_LOG_FILE="+logPath,
		// An inherited opencode switch would register a runtime and defeat the test.
		"FLEET_RUNTIME_OPENCODE=", "FLEET_STATE_DIR=", "FLEET_CONFIG=", "FLEET_PEERS=")

	var addr string
	deadline := time.After(20 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
wait:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the service exited before it listened")
			}
			if m := listeningRe.FindStringSubmatch(l); m != nil {
				addr = m[1]
				break wait
			}
		case <-deadline:
			t.Fatal("the service did not start listening")
		}
	}

	req, _ := http.NewRequest("GET", "http://"+addr+"/v1/runtimes", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /v1/runtimes = %d", resp.StatusCode)
	}
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 0 {
		t.Errorf("a service with no local driver listed %d runtimes: %s", len(body.Items), body.Items)
	}

	// FLEET_LOG_FILE got the same log the console did.
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "FLEET_RUNTIME=none") || !strings.Contains(string(logged), "listening on") {
		t.Errorf("log file = %q", logged)
	}
}
