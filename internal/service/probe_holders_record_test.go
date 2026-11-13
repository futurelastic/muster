package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #473: a runtime whose server is broken cannot say what it holds, but
// the driver that started the session can. A bare id (no ?runtime=) must then
// resolve to that driver, not to "existence could not be confirmed".

// holdingDriver is a driver whose State read of the ids in failErr fails and that
// keeps its own record of what it holds.
type holdingDriver struct {
	idDriver
	record map[string]bool
	closed []string
}

func (d *holdingDriver) Holds(id string) bool { return d.record[id] }

func (d *holdingDriver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	d.closed = append(d.closed, ref.ID)
	delete(d.record, ref.ID)
	return fleet.Ack{Accepted: true, Note: "the runtime's own confirmation was not available (test)"}, nil
}

func TestProbeHolders_ADriverThatHoldsTheIdButCannotReadItIsTheHolder(t *testing.T) {
	svc := New("test-machine")
	broken := &holdingDriver{
		idDriver: idDriver{Driver: stub.Driver{DeadlineMs: 200},
			failErr: map[string]error{"ses_a": fmt.Errorf("driver: GET /session/status: 500")}},
		record: map[string]bool{"ses_a": true},
	}
	local := map[fleet.RuntimeId]driver.Driver{"broken": broken}

	holders, inconclusive := svc.probeHolders(context.Background(), fleet.Request{}, local, "ses_a", 0)
	if len(holders) != 1 || len(inconclusive) != 0 {
		t.Errorf("holders = %v, inconclusive = %v; a driver whose own record holds the id is the holder", holders, inconclusive)
	}

	// The record is the evidence: a driver that does not hold the id, or cannot say,
	// is still "could not be asked" (the guardrail #60 shipped).
	holders, inconclusive = svc.probeHolders(context.Background(), fleet.Request{}, local, "ses_unknown_to_it", 0)
	if len(holders) != 0 {
		t.Errorf("holders = %v for an id the record does not hold", holders)
	}
	_ = inconclusive
	broken.idDriver.failErr["ses_b"] = fmt.Errorf("driver: connection refused")
	holders, inconclusive = svc.probeHolders(context.Background(), fleet.Request{}, local, "ses_b", 0)
	if len(holders) != 0 || len(inconclusive) != 1 {
		t.Errorf("holders = %v, inconclusive = %v; an id that is not in the record stays inconclusive", holders, inconclusive)
	}
}

func TestClose_WithNoRuntimeHintReachesTheDriverThatHoldsABrokenSession(t *testing.T) {
	healthy := &idDriver{Driver: stub.Driver{DeadlineMs: 200}} // affirms absence of every id
	broken := &holdingDriver{
		idDriver: idDriver{Driver: stub.Driver{DeadlineMs: 200},
			failErr: map[string]error{"ses_a": fmt.Errorf("driver: GET /session/status: 500")}},
		record: map[string]bool{"ses_a": true},
	}
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("healthy", healthy); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("broken", broken); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)

	del := func(id string) (*http.Response, string) {
		t.Helper()
		resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodDelete, srv.URL+"/v1/machines/test-machine/sessions/"+id, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, body := del("ses_a")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE of the session whose runtime is broken = %d %s, want 202", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Fleet-Runtime"); got != "broken" {
		t.Errorf("Fleet-Runtime = %q, want broken", got)
	}
	if !strings.Contains(body, `"accepted":true`) || !strings.Contains(body, "confirmation was not available") {
		t.Errorf("the answer must carry the note: %s", body)
	}
	if len(broken.closed) != 1 || broken.closed[0] != "ses_a" {
		t.Errorf("the holder was asked to close %v, want [ses_a]", broken.closed)
	}

	// An id nobody holds is still not found, not routed to the broken driver.
	if resp, body := del("ses_nobody"); resp.StatusCode == http.StatusAccepted {
		t.Errorf("DELETE of an id nobody holds = %d %s", resp.StatusCode, body)
	}
	if len(broken.closed) != 1 {
		t.Errorf("a close of an unknown id reached the broken driver: %v", broken.closed)
	}
}
