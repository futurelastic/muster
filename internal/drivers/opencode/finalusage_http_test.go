package opencode

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/service"
	"github.com/futurelastic/muster/internal/state"
)

// muster #485, end to end through the layers this repo owns: a real service over
// a state directory, the real driver, only the runtime faked. The issue's own
// "done when": a closed session's final figures come back from the closed list
// after the runtime's record of it is gone, and a restart of the service does not
// lose them.
func TestFinalUsage_ACloseOverHTTPLeavesTheFiguresInTheClosedListAcrossARestart(t *testing.T) {
	const token = "t485"
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	dir := t.TempDir()

	serve := func() (*httptest.Server, func(method, path string, body any) (int, []byte)) {
		st, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := service.NewWithState("test-machine", st)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.RegisterLocalDriver(DefaultRuntime, d); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(service.NewMux(svc, service.Config{Token: token, AllowLocalMutations: true}))
		t.Cleanup(srv.Close)
		return srv, func(method, path string, body any) (int, []byte) {
			t.Helper()
			var rdr io.Reader
			if body != nil {
				raw, _ := json.Marshal(body)
				rdr = bytes.NewReader(raw)
			}
			req, _ := http.NewRequest(method, srv.URL+path, rdr)
			req.Header.Set("Authorization", "Bearer "+token)
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", "k485")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, raw
		}
	}

	_, call := serve()
	code, raw := call(http.MethodPost, "/v1/machines/test-machine/sessions", map[string]any{"cwd": "/work/x"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	var sess fleet.Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		t.Fatal(err)
	}
	f.setLastMessage(sess.ID, userMsg("u1", 1000))
	f.setLastMessage(sess.ID, onModel(asstMsg("a1", 1001, 1010,
		step(tok(100, 10, i64(5), 1000, 50), f64(0.25)),
		step(tok(200, 20, i64(7), 2000, 0), f64(0.5)),
	), "provider-x", "model-y"))

	if code, raw := call(http.MethodDelete, "/v1/machines/test-machine/sessions/"+sess.ID, nil); code != http.StatusAccepted {
		t.Fatalf("close: %d %s", code, raw)
	}
	f.mu.Lock()
	_, stillThere := f.messages[sess.ID]
	f.mu.Unlock()
	if stillThere {
		t.Fatal("the runtime still holds the session's record; the figure must come from before it went")
	}

	check := func(call func(method, path string, body any) (int, []byte), when string) {
		t.Helper()
		code, raw := call(http.MethodGet, "/v1/sessions/closed?scope=local", nil)
		if code != http.StatusOK {
			t.Fatalf("%s: closed list %d %s", when, code, raw)
		}
		var col fleet.Collection[fleet.ClosedSession]
		if err := json.Unmarshal(raw, &col); err != nil {
			t.Fatalf("%s: %v in %s", when, err, raw)
		}
		if len(col.Items()) != 1 || col.Items()[0].ID != sess.ID {
			t.Fatalf("%s: want the one closed session, got %s", when, raw)
		}
		c := col.Items()[0]
		u := c.Usage
		if u == nil || u.Input != 300 || u.Output != 30 || u.CacheRead != 3000 || u.CacheWrite != 50 ||
			u.Reasoning == nil || *u.Reasoning != 12 || u.Cost == nil || *u.Cost != 0.75 ||
			u.Provider != "provider-x" || u.Model != "model-y" || u.Source != fleet.ConfidenceObserved ||
			u.AsOf.UnixMilli() != 1010 {
			t.Fatalf("%s: usage = %+v in %s", when, u, raw)
		}
		if c.ClosedBy != fleet.ClosedByClose || c.ClosedAt.IsZero() {
			t.Errorf("%s: the end the figures belong to is the record's own closedAt: %+v", when, c)
		}
		if strings.Contains(string(raw), "\"text\"") {
			t.Errorf("%s: content on the closed list: %s", when, raw)
		}
	}
	_, call = serve() // the first service is left to the cleanup; this is the one after the restart
	check(call, "after a restart")
}
