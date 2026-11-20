package main

import (
	"bufio"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// serveOnStateDir starts `muster serve` with no runtime on dir and returns the
// child with every stderr line it writes, as they come, and a channel closed when
// the stream ends (the process exited).
func serveOnStateDir(t *testing.T, dir string) (pid int, lines <-chan string) {
	t.Helper()
	cmd, sc := startServe(t,
		"FLEET_RUNTIME=none", "FLEET_TOKEN=secret", "FLEET_MACHINE=testbox",
		"FLEET_ADDR=127.0.0.1:0", "FLEET_LOG_FILE=", "FLEET_STATE_DIR="+dir,
		"FLEET_RUNTIME_OPENCODE=", "FLEET_CONFIG=", "FLEET_PEERS=")
	return cmd.Process.Pid, scanLines(sc)
}

func scanLines(sc *bufio.Scanner) <-chan string {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

// muster #507, through the real start: a second service on a state directory a
// running one owns is refused before it reads or changes anything in it, says whose
// the directory is, and leaves the first service serving. Its driver — whose
// start-up sweep stops the servers it finds under the directory — is never built.
func TestServeRefusesAStateDirectoryAnotherServiceOwns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	firstPid, first := serveOnStateDir(t, dir)
	var addr string
	timeout := time.After(20 * time.Second)
wait:
	for {
		select {
		case l, ok := <-first:
			if !ok {
				t.Fatal("the first service exited before it listened")
			}
			if m := listeningRe.FindStringSubmatch(l); m != nil {
				addr = m[1]
				break wait
			}
		case <-timeout:
			t.Fatal("the first service did not start listening")
		}
	}

	_, second := serveOnStateDir(t, dir)
	var said []string
	exited := false
	timeout = time.After(20 * time.Second)
read:
	for {
		select {
		case l, ok := <-second:
			if !ok {
				exited = true
				break read
			}
			said = append(said, l)
		case <-timeout:
			t.Fatalf("the second service neither stopped nor was refused; it said %q", said)
		}
	}
	if !exited {
		t.Fatal("the second service kept running")
	}
	all := strings.Join(said, "\n")
	if !strings.Contains(all, "is in use by a running muster service") || !strings.Contains(all, "pid "+strconv.Itoa(firstPid)) {
		t.Errorf("the refusal does not say whose the directory is (first service is pid %d): %q", firstPid, all)
	}
	if strings.Contains(all, "listening on") {
		t.Errorf("the second service got as far as listening: %q", all)
	}

	// The first one never noticed.
	req, _ := http.NewRequest("GET", "http://"+addr+"/v1/runtimes", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the first service stopped answering after the second start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET /v1/runtimes on the first service = %d", resp.StatusCode)
	}
}
