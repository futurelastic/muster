package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/service"
)

// muster #461 across a real hop: caller -> entry service -> remote driver ->
// the peer's service -> one of the peer's TWO runtimes, both holding session id
// "s1". The id is only unique within a runtime (§2.2), so which one a call
// reaches is the caller's to say, and the peer's to resolve (§13.1).

// holder is a runtime on the peer that holds session "s1" and records what
// reached it.
type holder struct {
	stub.Driver
	rt fleet.RuntimeId

	mu    sync.Mutex
	texts []string
	turns int
}

func (h *holder) session() fleet.Session {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "owner", ID: "s1"},
		Runtime:    h.rt, Cwd: "/work/" + fleet.AbsolutePath(h.rt), StartedAt: &ts,
		State: fleet.ObservedState(fleet.StatusIdle, "holder fixture", nil),
	}
}

func (h *holder) List(context.Context, fleet.Request, driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	return fleet.NewCollection([]fleet.Session{h.session()},
		[]fleet.SourceStatus{{Machine: "owner", Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

func (h *holder) State(_ context.Context, _ fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	if ref.ID != "s1" {
		return fleet.SessionState{}, fleet.ErrNoSuchSession
	}
	return h.session().State, nil
}

func (h *holder) Send(_ context.Context, _ fleet.Request, _ fleet.SessionRef, text string, _ driver.SendOptions) (fleet.DeliveryReceipt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.texts = append(h.texts, text)
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
}

func (h *holder) Turns(context.Context, fleet.Request, fleet.SessionRef, fleet.TurnsQuery) (fleet.TurnsPage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turns++
	return fleet.TurnsPage{Turns: []fleet.Turn{{At: time.Now(), Text: "said under " + string(h.rt)}}}, nil
}

func (h *holder) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.texts...)
}

func (h *holder) turnReads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turns
}

const hopToken = "hop-token"

// runtimeHop builds the entry service and the peer with its two runtimes.
func runtimeHop(t *testing.T) (entry *httptest.Server, alpha, beta *holder) {
	t.Helper()
	alpha = &holder{Driver: stub.Driver{DeadlineMs: 500}, rt: "alpha"}
	beta = &holder{Driver: stub.Driver{DeadlineMs: 500}, rt: "beta"}

	svcB := service.New("owner")
	for _, h := range []*holder{alpha, beta} {
		if err := svcB.RegisterLocalDriver(h.rt, h); err != nil {
			t.Fatal(err)
		}
	}
	owner := httptest.NewServer(service.NewMux(svcB, service.Config{Token: hopToken, AllowLocalMutations: true}))
	t.Cleanup(owner.Close)

	svcA := service.New("entry")
	if err := svcA.RegisterPeerDriver("owner", New("owner", owner.URL, WithDeadline(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	entry = httptest.NewServer(service.NewMux(svcA, service.Config{Token: hopToken, AllowLocalMutations: true, AllowPeerRelay: true}))
	t.Cleanup(entry.Close)
	return entry, alpha, beta
}

func hopCall(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	req.Header.Set("Authorization", "Bearer "+hopToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// Reading the session: the row it answers with is the one under the runtime the
// caller named. The listing the entry service answers from spans both runtimes,
// so naming one has to pick the row, not just reach the peer.
//
// The peer lists its runtimes in no fixed order, so one round could return the
// right row by luck; several make an implementation that ignores the name fail
// every time.
func TestHopGetSessionIsTheRowUnderTheNamedRuntime(t *testing.T) {
	entry, _, _ := runtimeHop(t)

	for round := 0; round < 8; round++ {
		for _, rt := range []string{"alpha", "beta"} {
			status, body := hopCall(t, entry, http.MethodGet, "/v1/machines/owner/sessions/s1?runtime="+rt, "")
			if status != http.StatusOK {
				t.Fatalf("?runtime=%s: status %d: %s", rt, status, body)
			}
			var got fleet.Session
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Runtime != fleet.RuntimeId(rt) || got.Cwd != fleet.AbsolutePath("/work/"+rt) {
				t.Fatalf("round %d: ?runtime=%s answered with the row of runtime %q (cwd %q)", round, rt, got.Runtime, got.Cwd)
			}
		}
	}
}

// A runtime that does not hold the id is not silently replaced by one that
// does: the peer's own answer reaches the caller.
func TestHopGetSessionUnderARuntimeThePeerLacksIsRefusedByThePeer(t *testing.T) {
	entry, _, _ := runtimeHop(t)

	status, body := hopCall(t, entry, http.MethodGet, "/v1/machines/owner/sessions/s1?runtime=gamma", "")
	if status != http.StatusNotFound {
		t.Fatalf("status %d: %s, want the peer's not_found for a runtime it does not have", status, body)
	}
}

// Input: without a runtime the peer answers "pass ?runtime=" — unchanged, and
// nothing is delivered to either. With one, exactly that runtime gets the text.
func TestHopInputReachesTheNamedRuntimeOnly(t *testing.T) {
	entry, alpha, beta := runtimeHop(t)
	const text = `{"text":"hello"}`

	status, body := hopCall(t, entry, http.MethodPost, "/v1/machines/owner/sessions/s1/input", text)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "?runtime=") {
		t.Fatalf("no runtime: status %d: %s, want the peer's own invalid asking for ?runtime=", status, body)
	}
	if len(alpha.sent())+len(beta.sent()) != 0 {
		t.Fatalf("an unresolved call delivered input: alpha %v beta %v", alpha.sent(), beta.sent())
	}

	status, body = hopCall(t, entry, http.MethodPost, "/v1/machines/owner/sessions/s1/input?runtime=beta", text)
	if status != http.StatusOK {
		t.Fatalf("?runtime=beta: status %d: %s", status, body)
	}
	if got := beta.sent(); len(got) != 1 || got[0] != "hello" {
		t.Errorf("beta received %v, want [hello]", got)
	}
	if got := alpha.sent(); len(got) != 0 {
		t.Errorf("alpha received %v, want nothing: the caller named beta", got)
	}
}

// Turns is one of the routes that already took ?runtime= on the peer.
func TestHopTurnsAreReadFromTheNamedRuntime(t *testing.T) {
	entry, alpha, beta := runtimeHop(t)

	status, body := hopCall(t, entry, http.MethodGet, "/v1/machines/owner/sessions/s1/turns", "")
	if status != http.StatusBadRequest {
		t.Fatalf("no runtime: status %d: %s, want the peer's invalid", status, body)
	}

	status, body = hopCall(t, entry, http.MethodGet, "/v1/machines/owner/sessions/s1/turns?runtime=alpha", "")
	if status != http.StatusOK {
		t.Fatalf("?runtime=alpha: status %d: %s", status, body)
	}
	if !strings.Contains(string(body), "said under alpha") {
		t.Errorf("body %s, want alpha's turn", body)
	}
	if alpha.turnReads() != 1 || beta.turnReads() != 0 {
		t.Errorf("turn reads: alpha %d beta %d, want 1 and 0", alpha.turnReads(), beta.turnReads())
	}
}
