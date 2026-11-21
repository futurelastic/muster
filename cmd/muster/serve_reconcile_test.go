package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// fakeMultiplexer writes a stand-in for the terminal multiplexer whose FIRST
// pane listing blocks until release exists. That first listing is the startup
// reconcile's, so for as long as release is absent the reconcile is pending —
// deterministically, with no sleeps and no dependence on how fast the machine is.
// Every other call, listings included, answers at once with nothing running.
func fakeMultiplexer(t *testing.T) (bin, started, release string) {
	t.Helper()
	dir := t.TempDir()
	bin, started, release = filepath.Join(dir, "mux"), filepath.Join(dir, "started"), filepath.Join(dir, "release")
	script := `#!/bin/sh
case "$1" in
-V) echo "tmux 3.7a" ;;
list-panes)
	if [ ! -e "` + started + `" ]; then
		: > "` + started + `"
		while [ ! -e "` + release + `" ]; do sleep 0.05; done
	fi
	;;
*) ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, started, release
}

func waitForFile(t *testing.T, path, what string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// muster #508, through the real start: the port opens while the reconcile is
// still running. Reads are answered, a call that would change a session is
// refused by name and retryably, and once the reconcile has applied its result
// the refusal lifts and the log says how long the reconcile took.
func TestServeAnswersWhileReconcilePending(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake multiplexer is a shell script")
	}
	bin, started, release := fakeMultiplexer(t)
	cmd, sc := startServe(t,
		"FLEET_RUNTIME=tmux", "FLEET_TMUX_BIN="+bin, "FLEET_TMUX_SANDBOX=off",
		"FLEET_TOKEN=secret", "FLEET_MACHINE=testbox", "FLEET_ADDR=127.0.0.1:0",
		"FLEET_ALLOW_MUTATIONS=1", "FLEET_LOG_FILE=", "FLEET_STATE_DIR="+filepath.Join(t.TempDir(), "state"),
		"FLEET_RECORD_ROOT=", "FLEET_PROCESS_SESSIONS_ROOT=", "FLEET_CREDENTIAL_PATH=",
		// Whatever this machine's own environment sets must not reach the child.
		"FLEET_RUNTIME_OPENCODE=", "FLEET_CONFIG=", "FLEET_PEERS=", "FLEET_INBOX_INDEX=",
		"FLEET_TRUST_ROOTS=", "FLEET_DEFAULT_RUNTIME=")
	_ = cmd
	lines := scanLines(sc)

	var log []string
	collect := func(until string) {
		t.Helper()
		timeout := time.After(30 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the service exited before %q; it said %q", until, log)
				}
				log = append(log, l)
				if strings.Contains(l, until) {
					return
				}
			case <-timeout:
				t.Fatalf("no %q within 30s; it said %q", until, log)
			}
		}
	}

	collect("listening on")
	m := listeningRe.FindStringSubmatch(log[len(log)-1])
	if m == nil {
		t.Fatalf("could not read the address from %q", log[len(log)-1])
	}
	base := "http://" + m[1]

	// The reconcile's listing is now running and blocked: the service is listening
	// while it is pending, and has not got as far as saying it reconciled.
	waitForFile(t, started, "the reconcile to start its listing")
	if _, err := os.Stat(release); err == nil {
		t.Fatal("test setup: the reconcile was released before the checks")
	}
	for _, l := range log {
		if strings.Contains(l, "reconciled —") {
			t.Fatalf("the reconcile finished before it was released: %q", log)
		}
	}

	call := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s %s while the reconcile was pending: %v", method, path, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	if resp := call("GET", "/v1/health", ""); resp.StatusCode != 200 {
		t.Errorf("GET /v1/health while reconciling = %d, want 200", resp.StatusCode)
	}
	if resp := call("GET", "/v1/sessions", ""); resp.StatusCode != 200 {
		t.Errorf("GET /v1/sessions while reconciling = %d, want 200", resp.StatusCode)
	}
	resp := call("POST", "/v1/machines/testbox/sessions", `{"cwd":"/tmp"}`)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("a create while reconciling = %d, want 504", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("the refusal does not say when to retry")
	}
	if e := decodeErrorBody(t, resp); e.Kind != fleet.ErrorUnreachable || e.Reason != fleet.ReasonReconciling || !e.Retryable {
		t.Errorf("error = %+v, want unreachable / reconciling / retryable", e)
	}

	// Let the reconcile finish.
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	collect("changes to sessions are accepted")

	var reconciledAt, acceptedAt int = -1, -1
	for i, l := range log {
		switch {
		case strings.Contains(l, "reconciled —"):
			reconciledAt = i
			for _, want := range []string{" took=", "phase.list=", "phase.starttimes=", "phase.save="} {
				if !strings.Contains(l, want) {
					t.Errorf("the reconcile line lacks %q: %s", want, l)
				}
			}
			if strings.Contains(l, "unjudged=") {
				t.Errorf("start times were not read: %s", l)
			}
		case strings.Contains(l, "changes to sessions are accepted"):
			acceptedAt = i
		}
	}
	listeningAt := -1
	for i, l := range log {
		if strings.Contains(l, "listening on") {
			listeningAt = i
			break
		}
	}
	if !(listeningAt >= 0 && listeningAt < reconciledAt && reconciledAt < acceptedAt) {
		t.Errorf("the log should say listening, then reconciled, then accepted; got positions %d, %d, %d in %q",
			listeningAt, reconciledAt, acceptedAt, log)
	}

	t.Logf("startup log:\n%s", strings.Join(log, "\n"))

	// And the refusal is gone: this call now reaches the session lookup, and finds
	// no such session, which is an answer about the session and not about startup.
	resp = call("POST", "/v1/machines/testbox/sessions/ghost/input", `{"text":"x"}`)
	if e := decodeErrorBody(t, resp); e.Reason == fleet.ReasonReconciling {
		t.Errorf("a change is still refused after the reconcile finished: %+v", e)
	}
}

func decodeErrorBody(t *testing.T, resp *http.Response) fleet.Error {
	t.Helper()
	var env fleet.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decoding the error body (status %d): %v", resp.StatusCode, err)
	}
	return env.Error
}
