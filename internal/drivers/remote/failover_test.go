package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Tests for muster #335: a peer known by several addresses, only some of which
// exist at a time.

// deadAddr is an address nothing listens on: the dial is refused.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// countingPeer answers a session list and counts what reaches it.
func countingPeer(t *testing.T) (*httptest.Server, *atomic.Int64, *capture) {
	t.Helper()
	var hits atomic.Int64
	var rec capture
	srv := peerServing(t, 200, json.RawMessage(
		`{"items":[],"sources":[{"machine":"laptop","status":"ok","observedAt":"2026-09-30T00:00:00Z"}],"complete":true}`), &rec)
	// Wrap so every request is counted, including the background capability probe.
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(counted.Close)
	return counted, &hits, &rec
}

func listErr(d *Driver) error {
	col, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		return err
	}
	for _, s := range col.Sources() {
		if s.Status != fleet.SourceOK {
			return &fleet.Error{Kind: fleet.ErrorUnreachable, Message: s.Error}
		}
	}
	return nil
}

// The incident: the first address is not there, the second is. The call
// succeeds, the second is remembered, and the next call does not dial the dead
// one first.
func TestFailsOverToTheAddressThatAnswersAndRemembersIt(t *testing.T) {
	good, hits, _ := countingPeer(t)
	dead := deadAddr(t)

	dials := map[string]*atomic.Int64{}
	for _, a := range []string{strings.TrimPrefix(dead, "http://"), strings.TrimPrefix(good.URL, "http://")} {
		dials[a] = new(atomic.Int64)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if c, ok := dials[addr]; ok {
			c.Add(1)
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	d := New("laptop", dead, WithFallbackURLs(good.URL), WithHTTPClient(&http.Client{Transport: tr}))
	t.Cleanup(d.Shutdown)

	if err := listErr(d); err != nil {
		t.Fatalf("the second address answers, so the call must succeed: %v", err)
	}
	if got, ok := d.Address(); !ok || got != good.URL {
		t.Fatalf("Address() = %q,%v; want the address that answered, %q", got, ok, good.URL)
	}
	deadKey := strings.TrimPrefix(dead, "http://")
	before := dials[deadKey].Load()
	for i := 0; i < 3; i++ {
		if err := listErr(d); err != nil {
			t.Fatal(err)
		}
	}
	if after := dials[deadKey].Load(); after != before {
		t.Errorf("the address that answered last must be tried first: the dead one was dialled %d more time(s)", after-before)
	}
	if hits.Load() == 0 {
		t.Error("the live peer was never reached")
	}
}

// When the remembered address goes away and the first returns, the driver
// walks back to it without being told.
func TestFailsBackWhenTheRememberedAddressDisappears(t *testing.T) {
	a, _, _ := countingPeer(t)
	b, _, _ := countingPeer(t)
	d := New("laptop", a.URL, WithFallbackURLs(b.URL))
	t.Cleanup(d.Shutdown)

	a.Close()
	if err := listErr(d); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Address(); got != b.URL {
		t.Fatalf("on b, got %q", got)
	}
	// The laptop changes connection: b is gone, a returns on the same address.
	a2, err := net.Listen("tcp", strings.TrimPrefix(a.URL, "http://"))
	if err != nil {
		t.Skipf("could not rebind the first address: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"sources":[{"machine":"laptop","status":"ok","observedAt":"2026-09-30T00:00:00Z"}],"complete":true}`))
	})}
	go func() { _ = srv.Serve(a2) }()
	t.Cleanup(func() { _ = srv.Close() })
	b.Close()

	if err := listErr(d); err != nil {
		t.Fatalf("the first address is back: %v", err)
	}
	if got, _ := d.Address(); got != a.URL {
		t.Errorf("must be back on the first address, got %q", got)
	}
}

// An address that swallows the SYN must not use up the caller's whole deadline:
// the connect budget moves on.
func TestAnAddressThatNeverAcceptsIsPassedOverWithinTheConnectBudget(t *testing.T) {
	good, _, _ := countingPeer(t)
	black := "http://203.0.113.9:7000" // TEST-NET-3; the dial below never really leaves
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "203.0.113.9:7000" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	d := New("laptop", black, WithFallbackURLs(good.URL),
		WithHTTPClient(&http.Client{Transport: tr}), WithConnectBudget(100*time.Millisecond), WithDeadline(10*time.Second))
	t.Cleanup(d.Shutdown)

	started := time.Now()
	if err := listErr(d); err != nil {
		t.Fatalf("fall back after the connect budget: %v", err)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("took %s: the black-holed address consumed the caller's deadline", took)
	}
}

// The safety rule: once a connection exists the request may have been
// delivered, so it is NOT replayed on another address.
func TestAnErrorAfterConnectingIsNeverRetriedElsewhere(t *testing.T) {
	var firstHits, secondHits atomic.Int64
	// Accepts the connection, reads the request, then drops it without a reply.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(second.Close)

	d := New("laptop", first.URL, WithFallbackURLs(second.URL))
	t.Cleanup(d.Shutdown)
	_, err := d.Create(context.Background(), caller, "key-1", fleet.SessionSpec{Cwd: "/w", Name: "n"})
	if err == nil {
		t.Fatal("a connection dropped mid-request must be an error")
	}
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnreachable {
		t.Errorf("want unreachable, got %v", err)
	}
	if secondHits.Load() != 0 {
		t.Errorf("the second address received %d request(s): a delivered create must never be replayed", secondHits.Load())
	}
	if firstHits.Load() != 1 {
		t.Errorf("first address hits = %d, want exactly 1", firstHits.Load())
	}
}

// A POST that never connected is replayed whole on the next address: body,
// idempotency key and credential all arrive.
func TestAFailedOverCreateCarriesItsBodyAndKey(t *testing.T) {
	var rec capture
	good := peerServing(t, 201, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "laptop", ID: "new-1"},
		Runtime:    "claude-code-tmux",
		State:      fleet.InferredState(fleet.StatusStarting, "just created", nil),
	}, &rec)
	d := New("laptop", deadAddr(t), WithFallbackURLs(good.URL))
	t.Cleanup(d.Shutdown)

	ref, err := d.Create(context.Background(), caller, "key-77", fleet.SessionSpec{Cwd: "/w", Name: "named"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "new-1" || rec.idem != "key-77" || !strings.Contains(rec.body, `"named"`) || rec.auth == "" {
		t.Errorf("ref=%+v capture=%+v: the replayed create lost something", ref, rec)
	}
}

// Nothing answers: the call is unreachable as ever, and the peer is not
// reported as having an address in use that never answered a thing.
func TestAllAddressesDownIsUnreachable(t *testing.T) {
	d := New("laptop", deadAddr(t), WithFallbackURLs(deadAddr(t)), WithDeadline(time.Second))
	t.Cleanup(d.Shutdown)
	err := listErr(d)
	if err == nil {
		t.Fatal("no address answers: want an unreachable source")
	}
	if !strings.Contains(err.Error(), "no answer from laptop") && !strings.Contains(err.Error(), "laptop") {
		t.Errorf("error should name the peer: %v", err)
	}
}

// A peer with one address is exactly what it was: no fallback machinery, and
// nothing to report in machine status.
func TestASingleAddressPeerReportsNoAddress(t *testing.T) {
	good, _, _ := countingPeer(t)
	d := New("solo", good.URL)
	t.Cleanup(d.Shutdown)
	if err := listErr(d); err != nil {
		t.Fatal(err)
	}
	if got, ok := d.Address(); ok || got != "" {
		t.Errorf("Address() = %q,%v; a single-address peer has nothing to report", got, ok)
	}
	var _ driver.AddressReporter = d
}

func TestAddressDropsCredentialsInTheURL(t *testing.T) {
	d := New("p", "http://user:secret@10.1.1.5:7000/", WithFallbackURLs("http://10.2.2.5:7000"))
	t.Cleanup(d.Shutdown)
	got, ok := d.Address()
	if !ok || strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Errorf("Address() = %q,%v must not carry userinfo", got, ok)
	}
}
