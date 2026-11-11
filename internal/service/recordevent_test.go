package service

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// session.record (muster #444): a session's record grew. The service treats it
// like every other frame about one session — it stamps a cursor, applies the
// subscriber's `session` and `cwdPrefix` selectors, and relays a peer's frame —
// and adds exactly one policy of its own: it publishes none while its operator
// has switched the record route off.

func recordEventOn(machine fleet.MachineId, id, cursor string) fleet.Event {
	return fleet.Event{
		Machine: machine, Kind: fleet.EventSessionRecord,
		Payload: fleet.SessionRecordPayload{
			Ref:    fleet.SessionRef{Machine: machine, ID: id},
			Cursor: cursor,
		},
	}
}

// Both selectors apply to the kind, typed (a local driver's frame) and as the
// generic map a peer's frame decodes to: an id of "" would admit it to every
// subscriber (#361).
func TestSessionRecordFramesAreNarrowedBySessionAndDirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		relayed bool
	}{
		{"a local driver's frame", false},
		{"a frame relayed from a peer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := fleet.MachineId("testbox")
			if tc.relayed {
				machine = "peerbox"
			}
			ev := recordEventOn(machine, "a", "c1")
			if tc.relayed {
				ev = overTheWire(t, ev)
			}
			if id, _ := eventTarget(ev); id != "a" {
				t.Fatalf("eventTarget = %q, want %q: the frame must name its session", id, "a")
			}

			h := newHub("testbox", "e")
			named, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{Sessions: []string{"a"}}, 0, "")
			other, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{Sessions: []string{"b"}}, 0, "")
			all, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
			h.publish(ev)

			if got := pending(named.ch); len(got) != 1 || got[0].Kind != fleet.EventSessionRecord {
				t.Errorf("a subscriber naming the session got %v, want its one session.record", kinds(got))
			}
			if got := pending(other.ch); len(got) != 0 {
				t.Errorf("a subscriber naming another session got %v, want nothing", kinds(got))
			}
			if got := pending(all.ch); len(got) != 1 {
				t.Errorf("an unfiltered subscriber got %v, want its one session.record", kinds(got))
			}
		})
	}
}

// The directory selector reaches the kind too. The frame names the session and
// nothing else, so the hub places it from what it learned of the session —
// the same path state and labels frames take.
func TestSessionRecordFramesFollowTheDirectorySelector(t *testing.T) {
	h := newHub("testbox", "e")
	h.publish(createdEvent("testbox", "in1", insideDir))
	h.publish(createdEvent("testbox", "out1", outsideDir))
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")

	h.publish(recordEventOn("testbox", "in1", "c1"))
	h.publish(recordEventOn("testbox", "out1", "c1"))

	want := []string{"session.record:in1"}
	if got := kinds(pending(sub.ch)); !sameStrings(got, want) {
		t.Errorf("a directory subscriber got %v, want only the inside session's frame %v", got, want)
	}
}

// The hub stamps the cursor and epoch; a driver leaves them zero and cannot
// influence them (§7.3), and a relayed frame keeps its peer's own coordinates.
func TestSessionRecordFramesAreStampedAndKeepTheirOrigin(t *testing.T) {
	h := newHub("testbox", "epoch-1")
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	relayed := recordEventOn("peerbox", "a", "c1")
	relayed.Origin = &fleet.EventOrigin{Cursor: 9, Epoch: "peer-epoch"}
	h.publish(recordEventOn("testbox", "a", "c1"))
	h.publish(relayed)

	first, second := <-sub.ch, <-sub.ch
	if first.Cursor != 1 || second.Cursor != 2 || first.Epoch != "epoch-1" {
		t.Errorf("cursors/epoch = %d,%d/%q; the service assigns them", first.Cursor, second.Cursor, first.Epoch)
	}
	if second.Origin == nil || second.Origin.Cursor != 9 || second.Origin.Epoch != "peer-epoch" {
		t.Errorf("origin = %+v; the peer's own coordinates must survive", second.Origin)
	}
}

// While the operator's record switch is on this machine's record route answers
// 501, so an event telling a client to pull it would mislead. This machine's own
// frames are dropped before they take a cursor (the sequence has no hole); a
// frame a peer sent is the peer's statement about its own route and passes.
func TestSessionRecordFramesAreNotPublishedWhileTheRecordSwitchIsOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse bool
		want   []string
	}{
		{"switch off", false, []string{"session.record:own", "session.record:peer", "session.state:own"}},
		{"switch on", true, []string{"session.record:peer", "session.state:own"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New("testbox")
			svc.refuseRecord.Store(tc.refuse)
			sub, _, _ := svc.events.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

			stream := &replayStream{events: []fleet.Event{
				recordEventOn("testbox", "own", "c1"),
				recordEventOn("peerbox", "peer", "c2"),
				stateEventOn("testbox", fleet.EventSessionState, "own"),
			}}
			_ = svc.drainStream(context.Background(), stream, func(fleet.Event) {})

			got := pending(sub.ch)
			if g := kinds(got); !sameStrings(g, tc.want) {
				t.Errorf("published %v, want %v", g, tc.want)
			}
			for i, ev := range got {
				if ev.Cursor != int64(i+1) {
					t.Errorf("frame %d has cursor %d: a dropped frame must not leave a hole", i, ev.Cursor)
				}
			}
		})
	}
}

// What the feed carries is a reference and a cursor and nothing else: asserted
// on the frame as a client reads it, not only on the Go type.
func TestSessionRecordFrameOnTheWireCarriesNoContent(t *testing.T) {
	svc := New("testbox")
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for svc.events.subscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Date(2026, 11, 11, 6, 0, 0, 0, time.UTC)
	svc.events.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionRecord,
		Payload: fleet.SessionRecordPayload{
			Ref:       fleet.SessionRef{Machine: "testbox", ID: "s1", Name: "n"},
			StartedAt: &started,
			Cursor:    "opaque-cursor",
		},
	})

	sc := bufio.NewScanner(resp.Body)
	var sawEventLine bool
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			sawEventLine = line == "event: session.record"
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame struct {
			Kind    string                     `json:"kind"`
			Payload map[string]json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Kind != "session.record" {
			continue
		}
		keys := make([]string, 0, len(frame.Payload))
		for k := range frame.Payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if want := "cursor,ref,startedAt"; strings.Join(keys, ",") != want {
			t.Fatalf("payload keys = %v, want exactly %s: the feed carries a reference and a cursor, never content", keys, want)
		}
		if !sawEventLine {
			t.Error("the frame's `event:` line must name the kind too")
		}
		return
	}
	t.Fatal("no session.record frame arrived")
}
