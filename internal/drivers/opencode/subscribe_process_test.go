package opencode

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #284 against REAL per-session processes (the stand-in in
// fakeopencode_test.go): what the log on stderr and the end of a process look
// like from the event stream.

// postTo sends a body to a test-only endpoint of the session's own process.
func postTo(t *testing.T, d *Driver, id, path, body string) {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok {
		t.Fatalf("session %s is not known", id)
	}
	req, _ := http.NewRequest(http.MethodPost, known.srv.baseURL+path, bytes.NewBufferString(body))
	req.SetBasicAuth(known.srv.username, known.srv.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestSubscribe_Process_TheRuntimesOwnErrorNameIsCarriedPastUnknownError(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	s := subscribeIso(t, d)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	if ev := next(t, s); ev.Kind != fleet.EventSessionCreated {
		t.Fatalf("first event = %s, want session.created", ev.Kind)
	}

	// The API says only UnknownError; the server's log says what happened.
	postTo(t, d, sess.ID, "/__test/log", "ERROR 2026-10-10T00:00:00 service=session.prompt error=ProviderModelNotFoundError providerID=x modelID=y")
	postTo(t, d, sess.ID, "/__test/emit", `{"type":"session.error","properties":{"sessionID":"`+sess.ID+`","error":{"name":"UnknownError","data":{"message":"Unexpected server error"}}}}`)
	ev := next(t, s)
	p := statePayload(t, ev)
	if p.State.LastTurn == nil || !strings.HasPrefix(p.State.LastTurn.Reason, "ProviderModelNotFoundError") ||
		!strings.Contains(p.State.LastTurn.Reason, "Unexpected server error") {
		t.Fatalf("lastTurn = %+v, want the log's name with the API's message", p.State.LastTurn)
	}
	for _, v := range []string{"ERROR 2026", "providerID", "modelID"} {
		if strings.Contains(p.State.LastTurn.Reason, v) {
			t.Errorf("the log's text leaked into the event: %q", p.State.LastTurn.Reason)
		}
	}
}

func TestSubscribe_Process_ALogThatNamesNothingSpecificLeavesTheRuntimesName(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	s := subscribeIso(t, d)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	next(t, s)

	postTo(t, d, sess.ID, "/__test/emit", `{"type":"session.error","properties":{"sessionID":"`+sess.ID+`","error":{"name":"UnknownError","data":{"message":"Unexpected server error"}}}}`)
	p := statePayload(t, next(t, s))
	if p.State.LastTurn == nil || !strings.HasPrefix(p.State.LastTurn.Reason, "UnknownError") {
		t.Errorf("lastTurn = %+v, want the runtime's own UnknownError when its log has nothing better", p.State.LastTurn)
	}
}

func TestSubscribe_Process_ACloseIsOneClosedEventAndNotAGap(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	s := subscribeIso(t, d)
	a := mustCreate(t, d, "ka", fleet.SessionSpec{})
	next(t, s)

	if _, err := d.Close(context.Background(), testReq, a.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ev := next(t, s); ev.Kind != fleet.EventSessionClosed {
		t.Fatalf("event = %s, want session.closed", ev.Kind)
	}
	// The connection ended with the process. That is not a gap, and the stream
	// is still good: a new session is reported on it.
	b := mustCreate(t, d, "kb", fleet.SessionSpec{})
	ev := next(t, s)
	if got, ok := ev.Payload.(fleet.Session); ev.Kind != fleet.EventSessionCreated || !ok || got.ID != b.ID {
		t.Fatalf("event = %s %+v, want session.created for the second session", ev.Kind, ev.Payload)
	}
}

func TestSubscribe_Process_AProcessThatDiesIsReportedClosedOnce(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	s := subscribeIso(t, d)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	next(t, s)

	known, _ := d.wasSeen(sess.ID)
	if err := syscall.Kill(known.srv.proc.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ev := next(t, s)
	p := statePayload(t, ev)
	if ev.Kind != fleet.EventSessionClosed || p.Ref.ID != sess.ID || p.State.Status != fleet.StatusDead {
		t.Fatalf("event = %s %+v, want session.closed dead", ev.Kind, p)
	}
	silent(t, s, "a process death is one event")
}

func TestSubscribe_Process_ASecondSessionIsAttachedWhenItIsCreated(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	s := subscribeIso(t, d)
	a := mustCreate(t, d, "ka", fleet.SessionSpec{})
	b := mustCreate(t, d, "kb", fleet.SessionSpec{})
	next(t, s)
	next(t, s)

	postTo(t, d, a.ID, "/__test/emit", `{"type":"session.status","properties":{"sessionID":"`+a.ID+`","status":{"type":"busy"}}}`)
	postTo(t, d, b.ID, "/__test/emit", `{"type":"session.status","properties":{"sessionID":"`+b.ID+`","status":{"type":"busy"}}}`)
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		got[statePayload(t, next(t, s)).Ref.ID] = true
	}
	if !got[a.ID] || !got[b.ID] {
		t.Errorf("states seen for %v, want one from each session's own process", got)
	}
}

func TestStartServer_ARuntimeThatDiesAtStartNamesItsErrorFromItsLog(t *testing.T) {
	// A process that logs its error and exits before it ever answers.
	sh := stubShell(t)
	_, err := startServer(context.Background(), sh, "", "muster", nil, "")
	if err == nil {
		t.Fatal("startServer succeeded against a process that exits at once")
	}
	if !strings.Contains(err.Error(), "ProviderConfigError") {
		t.Errorf("err = %v, want the runtime's own error name from its log", err)
	}
}

func subscribeIso(t *testing.T, d *Driver) driver.EventStream {
	t.Helper()
	return subscribe(t, d, driver.SubscribeFilter{})
}

// stubShell writes an executable that logs an error name to stderr and exits,
// standing in for a runtime that cannot start. Like the real runtime it logs
// only when started with --print-logs (muster #288), so the test fails if the
// driver stops passing the flag.
func stubShell(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --print-logs ] && echo 'ERROR service=config error=ProviderConfigError bad provider' >&2; done\nexit 3\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
