package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// ssePeer serves a canned event stream and records what was asked of it.
func ssePeer(t *testing.T, frames []string, gotQuery *atomic.Value, hold bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotQuery != nil {
			gotQuery.Store(r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for _, fr := range frames {
			fmt.Fprint(w, fr)
			if f != nil {
				f.Flush()
			}
		}
		if hold {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func frame(kind, data string) string {
	return "event: " + kind + "\ndata: " + data + "\n\n"
}

// §13.1 in the event plane. Without scope=local, two mutually-configured peers
// would each hold a stream to the other and relay indefinitely — and unlike a
// bad request, a long-lived loop does not announce itself.
func TestPeerSubscriptionAsksForLocalScopeOnly(t *testing.T) {
	var q atomic.Value
	srv := ssePeer(t, nil, &q, true)
	d := New("peerbox", srv.URL)

	s, err := d.Subscribe(context.Background(), caller,
		driver.SubscribeFilter{Sessions: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := q.Load().(string); ok && v != "" {
			if !strings.Contains(v, "scope=local") {
				t.Errorf("query = %q, must ask the peer for its local view (§13.1)", v)
			}
			if !strings.Contains(v, "session=a") || !strings.Contains(v, "session=b") {
				t.Errorf("query = %q, must forward the named sessions", v)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("peer was never asked")
}

// The hub pushes a directory prefix down to a peer stream (#372); the driver is
// the last hop that has to put it on the wire, or the peer streams its whole
// fleet regardless.
func TestPeerSubscriptionForwardsTheDirectoryPrefix(t *testing.T) {
	var q atomic.Value
	srv := ssePeer(t, nil, &q, true)
	d := New("peerbox", srv.URL)

	s, err := d.Subscribe(context.Background(), caller,
		driver.SubscribeFilter{CwdPrefix: "/work/app"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := q.Load().(string); ok && v != "" {
			if !strings.Contains(v, "cwdPrefix=%2Fwork%2Fapp") {
				t.Errorf("query = %q, must forward the directory prefix", v)
			}
			if strings.Contains(v, "session=") {
				t.Errorf("query = %q, names sessions nobody asked for", v)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the peer was never asked")
}

// The approved semantics: a relayed event keeps the peer's coordinates as
// provenance and leaves ours unset for the local hub to stamp.
func TestRelayedEventCarriesOriginAndDefersLocalStamping(t *testing.T) {
	data := `{"cursor":41,"epoch":"peer-epoch","machine":"peerbox","kind":"session.state",` +
		`"payload":{"ref":{"machine":"peerbox","id":"s1"},` +
		`"state":{"status":"working","confidence":"inferred","evidence":"x"}}}`
	srv := ssePeer(t, []string{frame("session.state", data)}, nil, true)
	d := New("peerbox", srv.URL)

	s, err := d.Subscribe(context.Background(), caller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Origin == nil {
		t.Fatal("relayed event lost the peer's coordinates; a caller could no longer resume against that peer directly")
	}
	if ev.Origin.Cursor != 41 || ev.Origin.Epoch != "peer-epoch" {
		t.Errorf("origin = %+v, want the peer's own cursor and epoch", *ev.Origin)
	}
	if ev.Cursor != 0 || ev.Epoch != "" {
		t.Errorf("cursor/epoch = %d/%q; a proxy must not present a peer's sequence as its own — "+
			"the local hub stamps these", ev.Cursor, ev.Epoch)
	}
	if ev.Machine != "peerbox" {
		t.Errorf("machine = %q; an event must keep naming where it happened", ev.Machine)
	}
}

// session.record (muster #444) is relayed like every other frame: the peer's
// payload — a reference and a cursor, nothing else — arrives as the peer sent it,
// with the peer's coordinates as provenance. The remote driver adds no logic for
// the kind, and this holds that it does not need any.
func TestRelayedSessionRecordKeepsItsPayloadAndOrigin(t *testing.T) {
	data := `{"cursor":7,"epoch":"peer-epoch","machine":"peerbox","kind":"session.record",` +
		`"payload":{"ref":{"machine":"peerbox","id":"s1"},"startedAt":"2026-11-11T06:00:00Z","cursor":"peer-cursor"}}`
	srv := ssePeer(t, []string{frame("session.record", data)}, nil, true)
	d := New("peerbox", srv.URL)

	s, err := d.Subscribe(context.Background(), caller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != fleet.EventSessionRecord {
		t.Fatalf("kind = %q, want session.record", ev.Kind)
	}
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("a relayed payload decodes to %T, want the generic map", ev.Payload)
	}
	if p["cursor"] != "peer-cursor" {
		t.Errorf("cursor = %v; the peer's cursor is a position in the peer's record and must pass through untouched", p["cursor"])
	}
	ref, _ := p["ref"].(map[string]any)
	if ref["id"] != "s1" || ref["machine"] != "peerbox" {
		t.Errorf("ref = %v, want the peer's session", ref)
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "cursor,ref,startedAt" {
		t.Errorf("payload keys = %v; the relay must add nothing to a reference and a cursor", keys)
	}
	if ev.Origin == nil || ev.Origin.Cursor != 7 || ev.Origin.Epoch != "peer-epoch" {
		t.Errorf("origin = %+v, want the peer's own coordinates", ev.Origin)
	}
	if ev.Cursor != 0 || ev.Epoch != "" {
		t.Errorf("cursor/epoch = %d/%q; the local hub stamps these", ev.Cursor, ev.Epoch)
	}
}

// A dropped stream is news. Reconnecting quietly would leave a caller unable
// to tell "the peer has nothing to say" from "we stopped listening".
func TestPeerStreamEndingIsAnnounced(t *testing.T) {
	// Serves one frame then closes, repeatedly.
	srv := ssePeer(t, []string{frame("session.state",
		`{"cursor":1,"epoch":"e","machine":"peerbox","kind":"session.state",`+
			`"payload":{"ref":{"machine":"peerbox","id":"s1"},`+
			`"state":{"status":"idle","confidence":"inferred","evidence":"x"}}}`)}, nil, false)
	d := New("peerbox", srv.URL)

	s, err := d.Subscribe(context.Background(), caller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var sawStatus bool
	for i := 0; i < 4 && !sawStatus; i++ {
		ev, err := s.Next(ctx)
		if err != nil {
			break
		}
		if ev.Kind == fleet.EventSourceStatus {
			sawStatus = true
			src, ok := ev.Payload.(fleet.SourceStatus)
			if !ok || src.Status == fleet.SourceOK {
				t.Errorf("payload = %+v, want a non-ok SourceStatus", ev.Payload)
			}
		}
	}
	if !sawStatus {
		t.Error("stream ended without telling anyone")
	}
}

// Subscribing is an operation like any other: it presents the caller's
// authority, and this driver has none of its own to fall back on.
func TestPeerSubscriptionRequiresCallerAuthority(t *testing.T) {
	d := New("peerbox", "http://127.0.0.1:1")
	if _, err := d.Subscribe(context.Background(), noAuthority, driver.SubscribeFilter{}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("want ErrNoCallerAuthority, got %v", err)
	}
}
