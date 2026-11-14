package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// A failure that is not a refusal is never `invalid` (muster #477). A client
// that reads a 400 as final stops retrying a create that only met a busy host,
// so the runtime failing on a request, and the runtime dying on every start
// attempt, answer unreachable and retryable; a runtime that refuses what it was
// asked (a 4xx) is still invalid, with its own words.

// runtimeErrorBody is what a runtime's own error text can look like: a directory
// and a loopback address, neither of which belongs in an answer to a caller.
const runtimeErrorBody = `{"name":"UnknownError","data":{"message":"ENOENT /Users/someone/project at 127.0.0.1:4096"}}`

// answering is a runtime that answers every request with status and body, and a
// *server pointed at it.
func answering(t *testing.T, status int, body string) (*Driver, *server) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	d := newTestDriver(t, newFakeServer(t))
	return d, &server{baseURL: ts.URL, username: "u", password: "p"}
}

const directoryPath = "/session?directory=%2FUsers%2Fsomeone%2Fproject"

func TestDo_RuntimeFailureIsUnreachableAndRetryable(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504, 599} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			d, srv := answering(t, status, runtimeErrorBody)
			err := d.do(context.Background(), srv, "POST", directoryPath, map[string]string{}, nil)

			var fe *fleet.Error
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v, want a *fleet.Error", err)
			}
			if fe.Kind != fleet.ErrorUnreachable || !fe.Retryable || fe.Machine != "test-machine" {
				t.Errorf("kind %q retryable %v machine %q, want unreachable, retryable, this machine", fe.Kind, fe.Retryable, fe.Machine)
			}
			if want := fmt.Sprintf("POST /session: the runtime answered %d", status); !strings.Contains(fe.Message, want) {
				t.Errorf("message %q does not say %q", fe.Message, want)
			}
			// Neither the runtime's text nor the query (which names the working
			// directory of a create) is copied into what a caller reads.
			for _, leak := range []string{"ENOENT", "Users", "someone", "127.0.0.1", "directory", "UnknownError"} {
				if strings.Contains(fe.Message, leak) {
					t.Errorf("message %q carries %q", fe.Message, leak)
				}
			}
		})
	}
}

// A 5xx is unreachable on the wire and still not a transport failure inside the
// driver: the optional reads that treat "nothing answered" as fatal must keep
// tolerating a runtime that answered an error, and a source that answered is
// degraded, not unreachable.
func TestDo_RuntimeFailureIsNotATransportFailure(t *testing.T) {
	d, srv := answering(t, 503, runtimeErrorBody)
	err := d.do(context.Background(), srv, "GET", "/config/providers", nil, nil)

	if isUnreachable(err) {
		t.Error("a runtime that answered 503 was read as one that did not answer: the catalog read would now fail a create")
	}
	if isNotFound(err) {
		t.Error("a 503 was read as a missing session")
	}
	if got := sourceStateFor(err); got != fleet.SourceDegraded {
		t.Errorf("sourceStateFor = %q, want degraded: the runtime answered, and is broken", got)
	}
	if !answeredWithAnError(err) {
		t.Error("a 503 is a runtime answering with an error (muster #474 reads it as one)")
	}
}

func TestDo_NoAnswerIsStillAnUnreachableTransportFailure(t *testing.T) {
	d, srv := answering(t, 200, "{}")
	// A server that is gone: the connection is refused.
	ts := httptest.NewServer(http.NotFoundHandler())
	srv.baseURL = ts.URL
	ts.Close()

	err := d.do(context.Background(), srv, "GET", "/session", nil, nil)
	if !isUnreachable(err) {
		t.Fatalf("err = %v, want the transport failure isUnreachable names", err)
	}
	var rf *runtimeFailure
	if errors.As(err, &rf) {
		t.Error("a connection that never answered was marked as a runtime that did")
	}
	if got := sourceStateFor(err); got != fleet.SourceUnreachable {
		t.Errorf("sourceStateFor = %q, want unreachable", got)
	}
}

// What a runtime refuses stays a refusal, with its own words: they are the
// useful answer there, and a retry would meet the same.
func TestDo_RuntimeRefusalStaysInvalid(t *testing.T) {
	for _, status := range []int{400, 403, 409, 422, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			d, srv := answering(t, status, "model not allowed")
			err := d.do(context.Background(), srv, "POST", "/session", map[string]string{}, nil)
			var fe *fleet.Error
			if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid || fe.Retryable {
				t.Fatalf("err = %v, want invalid and not retryable", err)
			}
			if !strings.Contains(fe.Message, fmt.Sprint(status)) || !strings.Contains(fe.Message, "model not allowed") {
				t.Errorf("message %q lost the runtime's refusal", fe.Message)
			}
			var rf *runtimeFailure
			if errors.As(err, &rf) {
				t.Error("a 4xx was marked as a runtime failure")
			}
		})
	}
}

// The create itself: a runtime that fails POST /session is retryable, and the
// create does not leave its server behind.
func TestCreate_RuntimeFailureOnTheSessionCreateIsRetryable(t *testing.T) {
	f := newFakeServer(t)
	f.createStatus = 503
	d := newTestDriver(t, f)

	_, err := createWithModel(t, d, "")
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a *fleet.Error", err)
	}
	if fe.Kind != fleet.ErrorUnreachable || !fe.Retryable {
		t.Errorf("kind %q retryable %v, want unreachable and retryable: a client reading a 400 stops asking", fe.Kind, fe.Retryable)
	}
	if strings.Contains(fe.Message, "ENOENT") || strings.Contains(fe.Message, "/Users/") {
		t.Errorf("message %q carries the runtime's own text", fe.Message)
	}

	// The same runtime answering a 4xx is a refusal, as before.
	f.mu.Lock()
	f.createStatus = 422
	f.mu.Unlock()
	if _, err := createAgain(t, d, "second", ""); kindOfErr(err) != fleet.ErrorInvalid {
		t.Errorf("a 422 on create = %v, want invalid", err)
	}
}

// ---- a start that failed after its attempts -----------------------------------

// A runtime that dies before it answers, three times, is the machine failing to
// start it now. The message is startServer's own: the attempts and the error NAME
// the runtime logged, never its text and never the working directory.
func TestStartFailure_ARuntimeThatKeepsDyingIsUnreachableAndRetryable(t *testing.T) {
	d := newTestDriver(t, newFakeServer(t))
	workdir := t.TempDir()
	_, err := startServer(context.Background(), stubShell(t), workdir, "muster", nil, nil)
	if err == nil {
		t.Fatal("startServer succeeded against a process that exits at once")
	}

	got := d.startFailure(err)
	var fe *fleet.Error
	if !errors.As(got, &fe) {
		t.Fatalf("startFailure = %v, want a *fleet.Error", got)
	}
	if fe.Kind != fleet.ErrorUnreachable || !fe.Retryable || fe.Machine != "test-machine" {
		t.Errorf("kind %q retryable %v machine %q, want unreachable, retryable, this machine", fe.Kind, fe.Retryable, fe.Machine)
	}
	for _, want := range []string{"after 3 attempts", "ProviderConfigError"} {
		if !strings.Contains(fe.Message, want) {
			t.Errorf("message %q does not say %q", fe.Message, want)
		}
	}
	for _, leak := range []string{workdir, "bad provider", "127.0.0.1"} {
		if strings.Contains(fe.Message, leak) {
			t.Errorf("message %q carries %q", fe.Message, leak)
		}
	}
}

// Everything else a start can end with keeps the answer it had: a launcher that
// refused (the same start refuses the same way), a readiness budget that ran out
// (already a deadline), a spawn that never happened.
func TestStartFailure_OtherFailuresAreReturnedAsTheyWere(t *testing.T) {
	d := newTestDriver(t, newFakeServer(t))
	for name, err := range map[string]error{
		"a launcher refusal":  fmt.Errorf("opencode: %w: no namespaces", errLauncherRefused),
		"a readiness budget":  fmt.Errorf("opencode: server did not become ready: %w", &driver.StepTimeout{Step: stepReady, Budget: time.Second, Err: context.DeadlineExceeded}),
		"a spawn that failed": fmt.Errorf("opencode: starting server: %w", os.ErrPermission),
	} {
		t.Run(name, func(t *testing.T) {
			if got := d.startFailure(err); got != err {
				t.Errorf("startFailure changed the answer: %v", got)
			}
		})
	}
}

// End to end: a create whose runtime dies on every start attempt reaches the
// caller as unreachable and retryable, and leaves no session behind.
func TestCreate_ARuntimeThatKeepsDyingIsRetryable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in runtime is a shell script")
	}
	// Answers the startup version probe, then dies on every `serve`.
	bin := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\n[ \"$1\" = --version ] && { echo 1.0.0; exit 0; }\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d, _ := newIsolatedDriver(t, WithBinary(bin))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := d.Create(ctx, testReq, "dies", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		t.Fatalf("Create = %v, want a *fleet.Error", err)
	}
	if fe.Kind != fleet.ErrorUnreachable || !fe.Retryable {
		t.Errorf("kind %q retryable %v, want unreachable and retryable (it was invalid, which a client reads as final)", fe.Kind, fe.Retryable)
	}
	if !strings.Contains(fe.Message, "after 3 attempts") {
		t.Errorf("message %q does not say the start was given up on", fe.Message)
	}
}
