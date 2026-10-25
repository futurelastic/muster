package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/remote"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// #361: `cwdPrefix` on the event stream. A prefix can only be applied to a
// frame that says where its session runs, and only session.created does. These
// tests hold the hub to the behaviour a client reads from the filter's name:
// every frame about a session under the directory, none about any other.

const (
	insideDir  = "/work/in/project"
	outsideDir = "/work/out/project"
	prefixDir  = "/work/in"
)

func createdEvent(machine fleet.MachineId, id, cwd string) fleet.Event {
	return fleet.Event{
		Machine: machine, Kind: fleet.EventSessionCreated,
		Payload: fleet.Session{
			SessionRef: fleet.SessionRef{Machine: machine, ID: id},
			Runtime:    "fake",
			Cwd:        fleet.AbsolutePath(cwd),
			State:      fleet.InferredState(fleet.StatusIdle, "test", nil),
		},
	}
}

func stateEventOn(machine fleet.MachineId, kind fleet.EventKind, id string) fleet.Event {
	return fleet.Event{
		Machine: machine, Kind: kind,
		Payload: fleet.SessionStatePayload{
			Ref:   fleet.SessionRef{Machine: machine, ID: id},
			State: fleet.InferredState(fleet.StatusWorking, "test", nil),
		},
	}
}

func labelsEventOn(machine fleet.MachineId, id string) fleet.Event {
	return fleet.Event{
		Machine: machine, Kind: fleet.EventSessionLabels,
		Payload: fleet.SessionLabelsPayload{
			Ref:    fleet.SessionRef{Machine: machine, ID: id},
			Labels: map[string]string{"k": "v"},
		},
	}
}

// overTheWire is what a frame looks like once a peer's service has encoded it
// and this one has decoded it into fleet.Event: Payload is a generic map, not
// the type the peer's driver published. It goes through the same JSON the
// remote driver's stream reads.
func overTheWire(t *testing.T, ev fleet.Event) fleet.Event {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out fleet.Event
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, isMap := out.Payload.(map[string]any); !isMap {
		t.Fatalf("a relayed payload decodes to %T; the test assumes the generic map", out.Payload)
	}
	return out
}

func pending(ch <-chan fleet.Event) []fleet.Event {
	var out []fleet.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

// kinds renders the frames a subscriber got as "kind:id", in order.
func kinds(evs []fleet.Event) []string {
	var out []string
	for _, ev := range evs {
		id, _ := eventTarget(ev)
		out = append(out, string(ev.Kind)+":"+id)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lifecycle publishes one session's whole life: created, working, labelled,
// closed. relay turns every frame into what a peer's would look like here.
func lifecycle(t *testing.T, h *hub, machine fleet.MachineId, id, cwd string, relay bool) {
	t.Helper()
	for _, ev := range []fleet.Event{
		createdEvent(machine, id, cwd),
		stateEventOn(machine, fleet.EventSessionState, id),
		labelsEventOn(machine, id),
		stateEventOn(machine, fleet.EventSessionClosed, id),
	} {
		if relay {
			ev = overTheWire(t, ev)
		}
		h.publish(ev)
	}
}

// The measured failure: only session.created reached a cwdPrefix subscriber.
// The session's state, labels and close frames name it and nothing else, so
// the prefix has to be applied to the directory the hub already learned from
// session.created. Run for a local session and for the same frames as a peer
// relays them, which arrive as generic maps.
func TestCwdPrefixDeliversAnInsideSessionsWholeLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		machine fleet.MachineId
		relay   bool
	}{
		{"local session", "testbox", false},
		{"relayed from a peer", "peerbox", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub("testbox", "e")
			prefixed, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")
			everything, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

			lifecycle(t, h, tc.machine, "in1", insideDir, tc.relay)
			lifecycle(t, h, tc.machine, "out1", outsideDir, tc.relay)

			want := []string{
				"session.created:in1", "session.state:in1", "session.labels:in1", "session.closed:in1",
			}
			if got := kinds(pending(prefixed.ch)); !sameStrings(got, want) {
				t.Errorf("cwdPrefix subscriber got %v, want exactly the inside session's frames %v", got, want)
			}
			if got := len(pending(everything.ch)); got != 8 {
				t.Errorf("an unfiltered subscriber got %d frames, want all 8 — the filter must not touch it", got)
			}
		})
	}
}

// A subscriber that resumes with a cursor is replayed from the ring. The
// session may have closed, and left the index, since; the replay has to match
// what live delivery matched.
func TestCwdPrefixBacklogReplayMatchesLiveDelivery(t *testing.T) {
	h := newHub("testbox", "e")
	lifecycle(t, h, "testbox", "in1", insideDir, false)
	lifecycle(t, h, "peerbox", "out1", outsideDir, true)

	_, backlog, needResync := h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "e")
	if needResync {
		t.Fatal("cursor 0 against a full window must resume, not resync")
	}
	want := []string{"session.created:in1", "session.state:in1", "session.labels:in1", "session.closed:in1"}
	if got := kinds(backlog); !sameStrings(got, want) {
		t.Errorf("replayed %v, want %v", got, want)
	}
}

// Ids are recyclable (§5.4): the directory a closed id had must not be applied
// to the next session that takes the name.
func TestCwdPrefixUsesTheRecycledIdsNewDirectory(t *testing.T) {
	h := newHub("testbox", "e")
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")

	lifecycle(t, h, "testbox", "s", insideDir, false)
	pending(sub.ch)

	h.publish(createdEvent("testbox", "s", outsideDir))
	h.publish(stateEventOn("testbox", fleet.EventSessionState, "s"))
	if got := kinds(pending(sub.ch)); len(got) != 0 {
		t.Errorf("a recycled id now outside the prefix delivered %v", got)
	}
}

// A rename changes the id and not the directory. The frames that follow carry
// the new id, so the index has to follow.
func TestCwdPrefixFollowsARename(t *testing.T) {
	for _, relay := range []bool{false, true} {
		h := newHub("testbox", "e")
		sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")
		h.publish(createdEvent("testbox", "old-in", insideDir))
		h.publish(createdEvent("testbox", "old-out", outsideDir))
		pending(sub.ch)

		for _, pair := range [][2]string{{"old-in", "new-in"}, {"old-out", "new-out"}} {
			ev := fleet.Event{Machine: "testbox", Kind: fleet.EventSessionRenamed,
				Payload: fleet.SessionRenamed{Machine: "testbox", From: pair[0], To: pair[1],
					Corroboration: fleet.RenameAccepted}}
			if relay {
				ev = overTheWire(t, ev)
			}
			h.publish(ev)
		}
		pending(sub.ch) // the rename frames themselves are not what is under test
		h.publish(stateEventOn("testbox", fleet.EventSessionState, "new-in"))
		h.publish(stateEventOn("testbox", fleet.EventSessionState, "new-out"))

		want := []string{"session.state:new-in"}
		if got := kinds(pending(sub.ch)); !sameStrings(got, want) {
			t.Errorf("relay=%v: after a rename got %v, want %v", relay, got, want)
		}
	}
}

// A frame for a session the hub has never seen cannot be placed. Dropping it
// would be a silent hole; it is admitted on the id test alone, and the id test
// still holds.
func TestUnknownDirectoryIsJudgedOnTheIdAlone(t *testing.T) {
	h := newHub("testbox", "e")
	prefixed, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")
	named, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{Sessions: []string{"x"}, CwdPrefix: prefixDir}, 0, "")

	h.publish(stateEventOn("testbox", fleet.EventSessionState, "x"))
	h.publish(stateEventOn("testbox", fleet.EventSessionState, "y"))

	if got := kinds(pending(prefixed.ch)); !sameStrings(got, []string{"session.state:x", "session.state:y"}) {
		t.Errorf("prefix-only subscriber got %v; an unplaceable frame must reach it", got)
	}
	if got := kinds(pending(named.ch)); !sameStrings(got, []string{"session.state:x"}) {
		t.Errorf("a subscriber that also names a session got %v; the id test must still apply", got)
	}
}

// Frames that are not about one session are not the filter's business.
func TestCwdPrefixLeavesNonSessionFramesAlone(t *testing.T) {
	h := newHub("testbox", "e")
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")

	h.publish(fleet.Event{Machine: "peerbox", Kind: fleet.EventMachineQuota,
		Payload: fleet.MachineQuotaPayload{Machine: "peerbox", Blocked: true}})
	h.publish(overTheWire(t, fleet.Event{Machine: "peerbox", Kind: fleet.EventMachineQuota,
		Payload: fleet.MachineQuotaPayload{Machine: "peerbox"}}))
	h.publish(fleet.Event{Machine: "peerbox", Kind: fleet.EventSourceStatus,
		Payload: fleet.SourceStatus{Machine: "peerbox", Status: fleet.SourceOK}})

	if got := len(pending(sub.ch)); got != 3 {
		t.Errorf("got %d of 3 machine-level frames", got)
	}
}

// Every listing the service performs teaches the index, whatever it was asked,
// so a client that lists and then subscribes has its sessions placed.
func TestAListingTeachesTheDirectoryIndex(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("fake", &sessionDriver{}); err != nil {
		t.Fatal(err)
	}
	sub, _, _ := svc.events.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work/two"}, 0, "")

	// sessionDriver reports s1 under /work/one, whatever the filter says.
	if _, err := svc.ListSessions(context.Background(), fleet.SystemRequest(), ScopeLocal,
		driver.ListFilter{CwdPrefix: "/work/one"}, 0); err != nil {
		t.Fatal(err)
	}
	svc.events.publish(stateEventOn("testbox", fleet.EventSessionState, "s1"))

	if got := kinds(pending(sub.ch)); len(got) != 0 {
		t.Errorf("s1 is under /work/one, yet a /work/two subscriber got %v", got)
	}
}

// listCountingDriver answers List from a fixed table and counts the calls.
type listCountingDriver struct {
	stub.Driver
	mu       sync.Mutex
	calls    int
	sessions []fleet.Session
	// ignoreID makes List answer with everything it holds whatever id it is
	// asked for — what the remote driver does, since it does not forward the
	// id filter to the peer.
	ignoreID bool
}

func (d *listCountingDriver) List(_ context.Context, _ fleet.Request, f driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	var out []fleet.Session
	for _, s := range d.sessions {
		if f.ID == "" || d.ignoreID || s.ID == f.ID {
			out = append(out, s)
		}
	}
	return fleet.NewCollection(out,
		[]fleet.SourceStatus{{Machine: "testbox", Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

func (d *listCountingDriver) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func existingSession(id, cwd string) fleet.Session {
	return fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "testbox", ID: id},
		Runtime:    "fake", Cwd: fleet.AbsolutePath(cwd),
		State: fleet.InferredState(fleet.StatusIdle, "test", nil),
	}
}

// replayStream is an EventStream that yields a fixed sequence and then ends.
type replayStream struct{ events []fleet.Event }

func (s *replayStream) Next(ctx context.Context) (fleet.Event, error) {
	if len(s.events) == 0 {
		return fleet.Event{}, errors.New("end of test stream")
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func (s *replayStream) Close() error { return nil }

// A session that existed before this service started watching never has a
// session.created in the stream — its first frame is a state change. The
// service asks the source where it runs, once, before publishing the frame.
func TestPreExistingSessionsAreLocatedOnTheirFirstFrame(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ignoreID  bool
		wantCalls int
	}{
		// A local driver narrows by id: one cheap listing per unknown session.
		{"driver that narrows by id", false, 2},
		// The remote driver does not forward the id: one listing answers for all.
		{"driver that lists everything", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New("testbox")
			d := &listCountingDriver{ignoreID: tc.ignoreID, sessions: []fleet.Session{
				existingSession("in1", insideDir),
				existingSession("out1", outsideDir),
			}}
			sub, _, _ := svc.events.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")

			stream := &replayStream{events: []fleet.Event{
				stateEventOn("testbox", fleet.EventSessionState, "in1"),
				stateEventOn("testbox", fleet.EventSessionState, "out1"),
				stateEventOn("testbox", fleet.EventSessionState, "in1"),
				stateEventOn("testbox", fleet.EventSessionState, "out1"),
				stateEventOn("testbox", fleet.EventSessionClosed, "in1"),
			}}
			_ = svc.drainStream(context.Background(), stream, func(ev fleet.Event) {
				svc.resolveCwd(context.Background(), "testbox", d, fleet.SystemRequest(), ev)
			})

			want := []string{"session.state:in1", "session.state:in1", "session.closed:in1"}
			if got := kinds(pending(sub.ch)); !sameStrings(got, want) {
				t.Errorf("got %v, want only the inside session's frames %v", got, want)
			}
			if n := d.Calls(); n != tc.wantCalls {
				t.Errorf("source listed %d times, want %d — once per session it had to place, never per frame", n, tc.wantCalls)
			}
		})
	}
}

// With nobody filtering by directory, no frame is worth a listing: the lookup
// exists for the filter, and an unfiltered stream pays nothing for it.
func TestNoDirectoryLookupWithoutADirectorySubscriber(t *testing.T) {
	svc := New("testbox")
	d := &listCountingDriver{sessions: []fleet.Session{existingSession("in1", insideDir)}}
	svc.events.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	svc.events.add(ScopeFleet, driver.SubscribeFilter{Sessions: []string{"in1"}}, 0, "")

	svc.resolveCwd(context.Background(), "testbox", d, fleet.SystemRequest(),
		stateEventOn("testbox", fleet.EventSessionState, "in1"))
	if n := d.Calls(); n != 0 {
		t.Errorf("listed %d times with no cwdPrefix subscriber", n)
	}
}

// A session the source cannot place is not asked about again for every frame it
// sends — a busy session would turn each state change into a listing.
func TestAnUnplaceableSessionIsNotRelistedPerFrame(t *testing.T) {
	svc := New("testbox")
	d := &listCountingDriver{} // knows no sessions at all
	svc.events.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: prefixDir}, 0, "")

	for i := 0; i < 5; i++ {
		svc.resolveCwd(context.Background(), "testbox", d, fleet.SystemRequest(),
			stateEventOn("testbox", fleet.EventSessionState, "ghost"))
	}
	if n := d.Calls(); n != 1 {
		t.Errorf("listed %d times for 5 frames of one unplaceable session, want 1", n)
	}
}

// peerFeedDriver is a peer machine's one runtime: it lists a fixed set of
// sessions and streams whatever the test pushes.
type peerFeedDriver struct {
	listCountingDriver
	feed chan fleet.Event
}

func (d *peerFeedDriver) Subscribe(context.Context, fleet.Request, driver.SubscribeFilter) (driver.EventStream, error) {
	return &feedStream{events: d.feed}, nil
}

// attachedFilters is the filter of every subscriber currently attached to h — on
// a peer, one per stream a machine asked it for.
func attachedFilters(h *hub) []driver.SubscribeFilter {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]driver.SubscribeFilter, 0, len(h.subs))
	for _, s := range h.subs {
		out = append(out, s.filter)
	}
	return out
}

// sseFrames reads the data frames of an event stream off the wire.
func sseFrames(t *testing.T, resp *http.Response) <-chan fleet.Event {
	t.Helper()
	out := make(chan fleet.Event, 128)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev fleet.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				out <- ev
			}
		}
	}()
	return out
}

// sessionFrames collects "kind:id" for every session-bound frame until `want`
// of them arrived or the deadline passed. Machine-level frames — source status
// and the like — are not what a directory filter is about.
func sessionFrames(frames <-chan fleet.Event, want int, within time.Duration) []string {
	var got []string
	deadline := time.After(within)
	for len(got) < want {
		select {
		case ev, ok := <-frames:
			if !ok {
				return got
			}
			if id, _ := eventTarget(ev); id != "" {
				got = append(got, string(ev.Kind)+":"+id)
			}
		case <-deadline:
			return got
		}
	}
	return got
}

// The measurement from #361, end to end: two services, the real remote driver
// between them, real HTTP and SSE. The peer runs one session inside the
// prefix and one outside, plus a pair that were already running before this
// service began watching, so their first frame is a state change. A
// cwdPrefix stream on this side must carry the inside sessions' whole lives
// and nothing from the outside ones; an unfiltered stream carries both.
func TestCwdPrefixOverARealPeerHop(t *testing.T) {
	peerSvc := New("peerbox")
	pd := &peerFeedDriver{
		listCountingDriver: listCountingDriver{sessions: []fleet.Session{
			{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "pre-in"}, Runtime: "fake",
				Cwd: fleet.AbsolutePath(insideDir), State: fleet.InferredState(fleet.StatusIdle, "test", nil)},
			{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "pre-out"}, Runtime: "fake",
				Cwd: fleet.AbsolutePath(outsideDir), State: fleet.InferredState(fleet.StatusIdle, "test", nil)},
		}},
		feed: make(chan fleet.Event, 64),
	}
	pd.DeadlineMs = 2000
	if err := peerSvc.RegisterLocalDriver("fake", pd); err != nil {
		t.Fatal(err)
	}
	peerSrv := httptest.NewServer(NewMux(peerSvc, Config{Principals: []Principal{
		{Name: "entrybox", Token: "peer-token", Grants: []Grant{GrantRead}},
	}}))
	t.Cleanup(peerSrv.Close)

	svc := New("entrybox")
	// Tear the driver-side stream down with the last client, so the peer's
	// server is not held open for the default minute at cleanup.
	svc.events.linger = 0
	if err := svc.RegisterPeerDriver("peerbox",
		remote.New("peerbox", peerSrv.URL, remote.WithIdentity("peer-token"), remote.WithSelf("entrybox"))); err != nil {
		t.Fatal(err)
	}
	srv := httptestServer(t, svc)

	open := func(query string) <-chan fleet.Event {
		req := authedRequest(t, http.MethodGet, srv.URL+"/v1/events"+query, nil)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return sseFrames(t, resp)
	}
	prefixed := open("?cwdPrefix=" + prefixDir)
	everything := open("")

	// Both streams attached here, and this side's pump attached to the peer —
	// the pump the two of them need TOGETHER. The unfiltered stream widens what
	// the peer is asked for, which replaces the pump the first one started: the
	// peer is first asked for the prefix, then for everything, and a frame it
	// publishes while the old subscription is going and the wide one is not yet
	// there reaches nobody (it is not replayed to a subscription that opens
	// later). So "some subscriber is attached on the peer" is not enough; the
	// only one left must be the wide one.
	settled := func() bool {
		f := attachedFilters(peerSvc.events)
		return svc.events.subscriberCount() == 2 &&
			len(f) == 1 && f[0].CwdPrefix == "" && len(f[0].Sessions) == 0
	}
	deadline := time.Now().Add(5 * time.Second)
	for !settled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !settled() {
		t.Fatalf("the streams did not attach in time: %d here, peer holds %+v",
			svc.events.subscriberCount(), attachedFilters(peerSvc.events))
	}

	for _, ev := range []fleet.Event{
		createdEvent("peerbox", "in1", insideDir),
		createdEvent("peerbox", "out1", outsideDir),
		stateEventOn("peerbox", fleet.EventSessionState, "in1"),
		stateEventOn("peerbox", fleet.EventSessionState, "out1"),
		stateEventOn("peerbox", fleet.EventSessionState, "pre-in"),
		stateEventOn("peerbox", fleet.EventSessionState, "pre-out"),
		stateEventOn("peerbox", fleet.EventSessionClosed, "in1"),
		stateEventOn("peerbox", fleet.EventSessionClosed, "out1"),
	} {
		pd.feed <- ev
	}

	wantInside := []string{
		"session.created:in1", "session.state:in1", "session.state:pre-in", "session.closed:in1",
	}
	if got := sessionFrames(prefixed, len(wantInside), 5*time.Second); !sameStrings(got, wantInside) {
		t.Errorf("cwdPrefix stream = %v, want %v", got, wantInside)
	}
	if got := sessionFrames(prefixed, 1, 300*time.Millisecond); len(got) != 0 {
		t.Errorf("cwdPrefix stream carried a frame from outside the prefix: %v", got)
	}
	if got := sessionFrames(everything, 8, 5*time.Second); len(got) != 8 {
		t.Errorf("unfiltered stream = %v, want all 8 frames", got)
	}
}

// The measurement from #372, end to end: with only a cwdPrefix subscriber here,
// the peer is asked for that directory, so a session outside it never crosses
// the wire — it is not relayed here and discarded, it is not relayed. The
// frames of a session whose directory only the peer can place are filtered by
// the peer's own hub, which is why pushing the prefix there is safe.
func TestPrefixIsPushedDownSoTheOutsideFramesNeverCrossThePeerHop(t *testing.T) {
	peerSvc := New("peerbox")
	pd := &peerFeedDriver{
		listCountingDriver: listCountingDriver{sessions: []fleet.Session{
			{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "pre-in"}, Runtime: "fake",
				Cwd: fleet.AbsolutePath(insideDir), State: fleet.InferredState(fleet.StatusIdle, "test", nil)},
			{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "pre-out"}, Runtime: "fake",
				Cwd: fleet.AbsolutePath(outsideDir), State: fleet.InferredState(fleet.StatusIdle, "test", nil)},
		}},
		feed: make(chan fleet.Event, 64),
	}
	pd.DeadlineMs = 2000
	if err := peerSvc.RegisterLocalDriver("fake", pd); err != nil {
		t.Fatal(err)
	}
	peerSrv := httptest.NewServer(NewMux(peerSvc, Config{Principals: []Principal{
		{Name: "entrybox", Token: "peer-token", Grants: []Grant{GrantRead}},
	}}))
	t.Cleanup(peerSrv.Close)

	svc := New("entrybox")
	svc.events.linger = 0
	if err := svc.RegisterPeerDriver("peerbox",
		remote.New("peerbox", peerSrv.URL, remote.WithIdentity("peer-token"), remote.WithSelf("entrybox"))); err != nil {
		t.Fatal(err)
	}
	srv := httptestServer(t, svc)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/events?cwdPrefix="+prefixDir, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	prefixed := sseFrames(t, resp)

	deadline := time.Now().Add(5 * time.Second)
	for (svc.events.subscriberCount() < 1 || peerSvc.events.subscriberCount() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if svc.events.subscriberCount() < 1 || peerSvc.events.subscriberCount() == 0 {
		t.Fatal("the stream did not attach in time")
	}

	for _, ev := range []fleet.Event{
		createdEvent("peerbox", "in1", insideDir),
		createdEvent("peerbox", "out1", outsideDir),
		stateEventOn("peerbox", fleet.EventSessionState, "in1"),
		stateEventOn("peerbox", fleet.EventSessionState, "out1"),
		stateEventOn("peerbox", fleet.EventSessionState, "pre-in"),
		stateEventOn("peerbox", fleet.EventSessionState, "pre-out"),
		stateEventOn("peerbox", fleet.EventSessionClosed, "in1"),
		stateEventOn("peerbox", fleet.EventSessionClosed, "out1"),
	} {
		pd.feed <- ev
	}

	wantInside := []string{
		"session.created:in1", "session.state:in1", "session.state:pre-in", "session.closed:in1",
	}
	if got := sessionFrames(prefixed, len(wantInside), 5*time.Second); !sameStrings(got, wantInside) {
		t.Errorf("cwdPrefix stream = %v, want %v", got, wantInside)
	}

	// What this service's own hub was handed, not what its subscriber got.
	svc.events.mu.Lock()
	var relayed []string
	for _, ev := range svc.events.ring {
		if id, _ := eventTarget(ev); id != "" {
			relayed = append(relayed, string(ev.Kind)+":"+id)
		}
	}
	svc.events.mu.Unlock()
	if !sameStrings(relayed, wantInside) {
		t.Errorf("the peer relayed %v to this service's hub, want only %v: the prefix must have been pushed to the peer", relayed, wantInside)
	}
}
