package tmux

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"strings"
)

// fakeCtl is a control-mode client that never spawns anything. Tests push
// notifications into it to simulate the multiplexer.
type fakeCtl struct {
	session string
	notes   chan ctlNote
	closed  bool // reaped: Close was called
	dead    bool // exited on its own: notes closed, Close NOT called
	mu      sync.Mutex
}

func (c *fakeCtl) Notes() <-chan ctlNote { return c.notes }
func (c *fakeCtl) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		if !c.dead {
			close(c.notes)
		}
	}
	return nil
}

// die simulates the client exiting because its host session went away: the
// notification channel ends, but nobody has reaped it. Keeping this distinct
// from Close is the whole point — a real client that dies still has to be
// waited on, and a fake that conflates dying with being reaped cannot show a
// driver forgetting to do so.
func (c *fakeCtl) die() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dead && !c.closed {
		c.dead = true
		close(c.notes)
	}
}

func (c *fakeCtl) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// ctlRegistry records every client the driver opened, so tests can assert
// on how many were opened and that all were reaped.
type ctlRegistry struct {
	mu      sync.Mutex
	opened  []*fakeCtl
	failFor map[string]bool
	// dieFor names sessions whose attachment "succeeds" and then exits at once,
	// without ever delivering a notification. Distinct from failFor on purpose:
	// a dial that fails opens no client and so can never die, while this one
	// produces a death per attempt — the shape a re-attach budget has to bound
	// and a failing dial does not. Scoped per session so the lifecycle client,
	// which lives on an arbitrary session, can stay healthy while a watched
	// session flaps.
	dieFor map[string]bool
}

func (r *ctlRegistry) dial(ctx context.Context, bin, session string) (ctlConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failFor[session] {
		return nil, errors.New("cannot attach")
	}
	c := &fakeCtl{session: session, notes: make(chan ctlNote, 16)}
	r.opened = append(r.opened, c)
	if r.dieFor[session] {
		c.die()
	}
	return c, nil
}

// dialsFor counts the CONTENT clients ever opened for a session — including the
// ones that have since died. Index 0 is the lifecycle client, skipped for the
// reason contentFor gives.
func (r *ctlRegistry) dialsFor(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for i, c := range r.opened {
		if i == 0 {
			continue
		}
		if c.session == name {
			n++
		}
	}
	return n
}

func (r *ctlRegistry) at(i int) *fakeCtl {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened[i]
}

func (r *ctlRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opened)
}

// contentFor returns the CONTENT client for a session.
//
// Index 0 is always the lifecycle client — Subscribe dials it first, and it
// may well be attached to the same session a content client is. It filters
// for lifecycle notifications only, so firing %output at it is correctly a
// no-op. Skipping it here is not a workaround; a helper that returned the
// first name match would silently test nothing.
func (r *ctlRegistry) contentFor(name string) *fakeCtl {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.opened {
		if i == 0 {
			continue
		}
		if c.session == name {
			return c
		}
	}
	return nil
}

func (r *ctlRegistry) allClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.opened {
		if !c.isClosed() {
			return false
		}
	}
	return true
}

func newSubDriver(f *fakeMux, r *ctlRegistry) *Driver {
	return New("testbox",
		withExec(f.exec),
		withCtlDialer(r.dial),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	)
}

// fire pushes a notification that the driver should treat as a change
// trigger.
func fire(c *fakeCtl, name string) {
	if c == nil {
		return
	}
	select {
	case c.notes <- ctlNote{Name: name}:
	default:
	}
}

func nextWithin(t *testing.T, s driver.EventStream, d time.Duration) (fleet.Event, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		return fleet.Event{}, false
	}
	return ev, true
}

// The cost property that justified the design: O(subscribers), not
// O(sessions). One lifecycle client plus one content client per MATCHING
// session — not one per session on the machine.
func TestSubscribeOpensOneClientPerMatchingSessionPlusLifecycle(t *testing.T) {
	f := twoSessions()
	// Add sessions that do NOT match the filter.
	for i := 0; i < 8; i++ {
		id := "%" + intToStr(200+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "other" + intToStr(i), paneID: id, cwd: "/elsewhere", pid: 900 + i, created: 1785600009,
		})
		f.captures[id] = idleFixtureFor("other")
	}
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{CwdPrefix: "/work/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 1 lifecycle + 1 matching content client. Ten sessions exist.
	if got := r.count(); got != 2 {
		t.Errorf("opened %d control clients for 10 sessions with 1 matching; "+
			"want 2 (lifecycle + matching). Cost must be O(subscribers), not O(sessions)", got)
	}
}

// A content notification triggers a re-read, and a real status change is
// emitted as an event.
func TestSubscribeEmitsStateChangeOnTrigger(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// alpha was idle at subscribe time; make it working.
	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event after a content notification")
	}
	if ev.Kind != fleet.EventSessionState {
		t.Fatalf("want session.state, got %q", ev.Kind)
	}
	p, ok := ev.Payload.(fleet.SessionStatePayload)
	if !ok {
		t.Fatalf("payload type %T, want SessionStatePayload", ev.Payload)
	}
	if p.Ref.ID != "alpha💬" || p.State.Status != fleet.StatusWorking {
		t.Errorf("got %q -> %q, want alpha💬 -> working", p.Ref.ID, p.State.Status)
	}
}

// §7.3's fields are the service's to assign; a driver must not invent them.
func TestSubscribeLeavesCursorAndEpochToTheService(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	s, _ := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	defer s.Close()

	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event")
	}
	if ev.Cursor != 0 || ev.Epoch != "" {
		t.Errorf("driver stamped cursor=%d epoch=%q; §7.3 assigns these per "+
			"service instance and a driver has neither", ev.Cursor, ev.Epoch)
	}
	if ev.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", ev.Machine)
	}
}

// A session appearing and disappearing must produce created/closed, driven
// by the GLOBAL lifecycle notifications rather than by any per-session
// client.
func TestSubscribeEmitsCreatedAndClosed(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	lifecycle := r.opened[0] // the first client opened is the lifecycle one

	f.addSession(fakeSession{
		name: "gamma", paneID: "%9", cwd: "/work/gamma", pid: 300, created: 1785600002,
	}, idleFixtureFor("gamma"))
	fire(lifecycle, "sessions-changed")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event after a lifecycle notification")
	}
	if ev.Kind != fleet.EventSessionCreated {
		t.Fatalf("want session.created, got %q", ev.Kind)
	}
	if sess, ok := ev.Payload.(fleet.Session); !ok || sess.ID != "gamma" {
		t.Errorf("payload = %+v, want session gamma", ev.Payload)
	}

	// Now remove it.
	f.dropLastSession()
	fire(lifecycle, "sessions-changed")

	ev, ok = nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event after removal")
	}
	if ev.Kind != fleet.EventSessionClosed {
		t.Fatalf("want session.closed, got %q", ev.Kind)
	}
}

// §5.7 on the event stream. A failed read must not be diffed as though it
// had succeeded, or every unreadable session emits a spurious "closed" and
// subscribers conclude the fleet died.
func TestSubscribeDoesNotReportClosedWhenTheReadFailed(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	lifecycle := r.opened[0]
	f.setFailList(true)
	fire(lifecycle, "sessions-changed")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("a failed read should still say something, not go silent")
	}
	if ev.Kind == fleet.EventSessionClosed {
		t.Fatal("a failed read was diffed as sessions disappearing (§5.7)")
	}
	if ev.Kind != fleet.EventSourceStatus {
		t.Fatalf("want source.status, got %q", ev.Kind)
	}
	src, ok := ev.Payload.(fleet.SourceStatus)
	if !ok || src.Status == fleet.SourceOK {
		t.Errorf("payload = %+v, want a non-ok SourceStatus", ev.Payload)
	}
}

// Every client a subscription opens must be reaped, or each subscribe call
// leaks an attached process — the exact failure the demand-driven design
// exists to avoid.
func TestCloseReapsEveryControlClient(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if r.count() != 3 { // lifecycle + 2 sessions
		t.Fatalf("expected 3 clients, got %d", r.count())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.allClosed() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !r.allClosed() {
		t.Error("Close left control clients attached")
	}
	// Idempotent: a double close must not panic on an already-closed channel.
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned %v", err)
	}
}

// The documented edge: control mode has no unattached form, so with zero
// sessions there is nothing to attach a lifecycle client to. That must be an
// honest refusal, not a silent stream that never delivers.
//
// And it must be the TRANSIENT refusal. Reported as ErrUnsupported it read as
// "this substrate cannot stream", which is false and is acted on permanently:
// the service's pump gave up for good, so a machine that was merely empty when
// somebody subscribed reported the sessions it went on to start to nobody.
func TestSubscribeRefusesWhenThereIsNothingToAttachTo(t *testing.T) {
	f := &fakeMux{captures: map[string]string{}}
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	_, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err == nil {
		t.Fatal("want an error when there is no session to attach to")
	}
	if !errors.Is(err, driver.ErrNotReady) {
		t.Errorf("want ErrNotReady wrapped, got %v", err)
	}
	if errors.Is(err, driver.ErrUnsupported) {
		t.Error("an empty machine must not claim the substrate cannot stream; " +
			"a caller told that stops asking forever")
	}
}

func TestParseNoteRejectsNonNotifications(t *testing.T) {
	cases := []struct {
		line string
		want bool
		name string
	}{
		{"%output %169 hello", true, "output"},
		{"%sessions-changed", true, "sessions-changed"},
		{"%unlinked-window-add @177", true, "unlinked-window-add"},
		{"not a notification", false, ""},
		{"", false, ""},
		{"%", false, ""},
	}
	for _, tc := range cases {
		got, ok := parseNote(tc.line)
		if ok != tc.want {
			t.Errorf("parseNote(%q) ok = %v, want %v", tc.line, ok, tc.want)
			continue
		}
		if ok && got.Name != tc.name {
			t.Errorf("parseNote(%q) name = %q, want %q", tc.line, got.Name, tc.name)
		}
	}
}

// The scoping asymmetry measured against the real substrate, pinned as a
// property of the classification helpers so a future edit cannot quietly
// reclassify a global notification as a per-attachment one.
func TestNotificationClassification(t *testing.T) {
	global := []string{"sessions-changed", "unlinked-window-add", "unlinked-window-close"}
	for _, n := range global {
		if !isLifecycleNote(n) {
			t.Errorf("%q is delivered fleet-wide and must drive lifecycle", n)
		}
	}
	perAttachment := []string{"output", "subscription-changed"}
	for _, n := range perAttachment {
		if !isContentNote(n) {
			t.Errorf("%q is a content notification", n)
		}
		if isLifecycleNote(n) {
			t.Errorf("%q is scoped to the attached session; it cannot be trusted "+
				"as a fleet-wide lifecycle signal", n)
		}
	}
}

// The lifecycle client is hosted by a session the driver neither owns nor
// created. When that session exits, the client dies with it — and the stream
// must not quietly stop delivering. It must say it is degraded, and
// re-attach.
func TestLifecycleClientSurvivesItsHostSessionDying(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	before := r.count()
	// Kill the host: closing the client closes its notification channel,
	// which is exactly what happens when the session it is attached to
	// exits.
	_ = r.opened[0].Close()

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("stream went silent when the lifecycle client died; a deaf stream " +
			"is indistinguishable from a quiet fleet")
	}
	if ev.Kind != fleet.EventSourceStatus {
		t.Fatalf("want source.status, got %q", ev.Kind)
	}
	src, ok := ev.Payload.(fleet.SourceStatus)
	if !ok || src.Status != fleet.SourceDegraded {
		t.Errorf("payload = %+v, want a degraded SourceStatus", ev.Payload)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.count() > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("no re-attach: still %d clients", r.count())
}

// D4's whole point: naming a session costs one connection, describing a
// directory costs one per match. A caller that knows what it wants should not
// pay for what it does not.
func TestNamingSessionsCostsOneClientEach(t *testing.T) {
	f := twoSessions()
	for i := 0; i < 38; i++ {
		id := "%" + intToStr(300+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "bulk" + intToStr(i), paneID: id, cwd: "/work/alpha", pid: 700 + i, created: 1785600020,
		})
		f.captures[id] = idleFixtureFor("bulk")
	}
	// Every one of those 40 sessions shares /work/alpha, so a prefix filter
	// would attach to all of them.
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	s, err := d.Subscribe(context.Background(), testCaller,
		driver.SubscribeFilter{Sessions: []string{"alpha💬", "bulk7"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if got := r.count(); got != 3 {
		t.Errorf("opened %d clients for 2 named sessions out of 40; want 3 "+
			"(lifecycle + one per named session). Filter granularity IS the cost", got)
	}
}

// Events for sessions outside the filter must not be delivered, even though
// lifecycle notifications arrive fleet-wide.
func TestNamedSubscriptionIgnoresOtherSessions(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	s, err := d.Subscribe(context.Background(), testCaller,
		driver.SubscribeFilter{Sessions: []string{"alpha💬"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lifecycle := r.opened[0]

	// A session appears that the caller did not ask about.
	f.addSession(fakeSession{
		name: "unrelated", paneID: "%77", cwd: "/elsewhere", pid: 400, created: 1785600030,
	}, idleFixtureFor("unrelated"))
	fire(lifecycle, "sessions-changed")

	if ev, ok := nextWithin(t, s, 1500*time.Millisecond); ok {
		t.Errorf("delivered %q for a session outside the filter: %+v", ev.Kind, ev.Payload)
	}

	// ...and one it did ask about still comes through.
	f.setCapture("%1", fixtureWorking)
	fire(lifecycle, "sessions-changed")
	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event for the named session")
	}
	if ev.Kind != fleet.EventSessionState {
		t.Errorf("kind = %q, want session.state", ev.Kind)
	}
}

// Both selectors narrow, and they compose with AND — the same rule ListFilter
// follows, so callers do not have to remember two conventions.
func TestFilterSelectorsCompose(t *testing.T) {
	cases := []struct {
		name   string
		filter driver.SubscribeFilter
		id     string
		cwd    string
		want   bool
	}{
		{"zero value matches everything", driver.SubscribeFilter{}, "a", "/x", true},
		{"id only", driver.SubscribeFilter{Sessions: []string{"a"}}, "a", "/x", true},
		{"id only, miss", driver.SubscribeFilter{Sessions: []string{"a"}}, "b", "/x", false},
		{"prefix only", driver.SubscribeFilter{CwdPrefix: "/x"}, "a", "/x/y", true},
		{"both, both satisfied", driver.SubscribeFilter{Sessions: []string{"a"}, CwdPrefix: "/x"}, "a", "/x/y", true},
		{"both, prefix fails", driver.SubscribeFilter{Sessions: []string{"a"}, CwdPrefix: "/z"}, "a", "/x/y", false},
		{"both, id fails", driver.SubscribeFilter{Sessions: []string{"a"}, CwdPrefix: "/x"}, "b", "/x/y", false},
	}
	for _, tc := range cases {
		if got := tc.filter.Matches(tc.id, tc.cwd); got != tc.want {
			t.Errorf("%s: Matches(%q,%q) = %v, want %v", tc.name, tc.id, tc.cwd, got, tc.want)
		}
	}
}

// A subscription's cost is paid by a multiplexer server that other tools
// share. One forgotten subscriber once held 62 clients on a 69-session host,
// exhausted the server's descriptors, and made every new attach fail while
// every session was alive — so this bound is a safety property, not a tuning
// knob.
func TestSubscribeCapsContentClients(t *testing.T) {
	f := twoSessions()
	for i := 0; i < 60; i++ {
		id := "%" + intToStr(500+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "big" + intToStr(i), paneID: id, cwd: "/w", pid: 5000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("big" + intToStr(i))
	}
	// A fake dialer, like every other subscription test. The real one would
	// attach genuine multiplexer clients from a unit test — to whatever server
	// the machine running it has — and those die at once, which is how this
	// test came to trip #162 under load instead of measuring the cap.
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := d.Subscribe(ctx, testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stream.Close()

	es, ok := stream.(*eventStream)
	if !ok {
		t.Fatalf("unexpected stream type %T", stream)
	}
	es.mu.Lock()
	n := len(es.conns)
	es.mu.Unlock()

	// conns also holds the single lifecycle client, which is per subscription
	// rather than per session and is not what the cap governs.
	if n > maxContentClients+1 {
		t.Errorf("opened %d clients for %d sessions; cap is %d(+1 lifecycle) — an unbounded "+
			"subscription exhausts a server other tools depend on", n, len(f.sessions), maxContentClients)
	}
	if n == 0 {
		t.Error("capped to nothing; a subscription with no content clients loses its change triggers")
	}
}

// The account blocking is one fact about the machine, and a supervisor should
// be told once. Before this, 48 sessions each discovered it by being dispatched
// work and stalling — every discovery costing a session that was already sent.
func TestSubscribeAnnouncesAndRetractsAnAccountBlock(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const rule = "────────────────────"
	f.setCapture("%1", "transcript\n  ⎿  You've hit your weekly limit · resets Aug 10 at 12am (Asia/Tokyo)\n"+
		rule+"\n❯ \n"+rule+"\n")
	fire(r.contentFor("alpha💬"), "output")

	p, ok := awaitQuota(t, s)
	if !ok {
		t.Fatal("the account started refusing work and the stream never said so")
	}
	if !p.Blocked || p.Quota == nil || !strings.Contains(p.Quota.ResetHint, "aug 10") {
		t.Errorf("payload = %+v, want blocked with a reset hint", p)
	}
	if p.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", p.Machine)
	}

	// Recovery is a positive statement, not an absence to infer.
	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	p, ok = awaitQuota(t, s)
	if !ok {
		t.Fatal("the account recovered and the stream never retracted the block")
	}
	if p.Blocked || p.Quota != nil {
		t.Errorf("payload = %+v, want an explicit unblocked", p)
	}
}

// A subscriber connecting mid-outage must not wait for a transition that has
// already happened.
func TestSubscribeAnnouncesABlockAlreadyInForce(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	const rule = "────────────────────"
	f.setCapture("%1", "transcript\n  ⎿  You've hit your weekly limit · resets Aug 10\n"+rule+"\n❯ \n"+rule+"\n")
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fire(r.contentFor("alpha💬"), "output")

	p, ok := awaitQuota(t, s)
	if !ok || !p.Blocked {
		t.Fatalf("a subscriber joining during an outage was told nothing (got %+v, %v)", p, ok)
	}
}

func awaitQuota(t *testing.T, s driver.EventStream) (fleet.MachineQuotaPayload, bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		ev, ok := nextWithin(t, s, time.Until(deadline))
		if !ok {
			return fleet.MachineQuotaPayload{}, false
		}
		if ev.Kind == fleet.EventMachineQuota {
			p, ok := ev.Payload.(fleet.MachineQuotaPayload)
			if !ok {
				t.Fatalf("payload type %T, want MachineQuotaPayload", ev.Payload)
			}
			return p, true
		}
	}
	return fleet.MachineQuotaPayload{}, false
}

// #12: the machine-level event fires once at an actual credential
// transition — not on the seed read that merely establishes a baseline,
// not a second time for a later trigger that changed nothing further, and
// not once per session (this fixture carries two).
func TestSubscribeAnnouncesACredentialTransitionOnlyOnce(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}

	credPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := time.Unix(1785600500, 0)
	if err := os.Chtimes(credPath, baseline, baseline); err != nil {
		t.Fatal(err)
	}

	d := New("testbox",
		withExec(f.exec),
		withCtlDialer(r.dial),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithCredentialPath(credPath),
	)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A trigger with nothing changed about the credential store: the seed
	// read already saw this generation, so this must not read as a
	// transition.
	f.setCapture("%2", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")
	if _, ok := awaitAccount(t, s, 500*time.Millisecond); ok {
		t.Fatal("machine.account fired with no credential change — the seed read must not count as a transition")
	}

	// The actual transition.
	rotated := time.Unix(1785700000, 0)
	if err := os.Chtimes(credPath, rotated, rotated); err != nil {
		t.Fatal(err)
	}
	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	p, ok := awaitAccount(t, s, 4*time.Second)
	if !ok {
		t.Fatal("the credential material changed and the stream never said so")
	}
	if p.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", p.Machine)
	}
	if !p.Generation.Equal(rotated) {
		t.Errorf("Generation = %v, want %v", p.Generation, rotated)
	}

	// A second trigger with nothing further changed must not re-announce —
	// once per transition, not once per read that happens to notice it.
	f.setCapture("%2", fixtureUnsent)
	fire(r.contentFor("beta"), "output")
	if _, ok := awaitAccount(t, s, 500*time.Millisecond); ok {
		t.Fatal("machine.account fired a second time with no further change — a transition is reported once")
	}
}

func awaitAccount(t *testing.T, s driver.EventStream, d time.Duration) (fleet.MachineAccountPayload, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ev, ok := nextWithin(t, s, time.Until(deadline))
		if !ok {
			return fleet.MachineAccountPayload{}, false
		}
		if ev.Kind == fleet.EventMachineAccount {
			p, ok := ev.Payload.(fleet.MachineAccountPayload)
			if !ok {
				t.Fatalf("payload type %T, want MachineAccountPayload", ev.Payload)
			}
			return p, true
		}
	}
	return fleet.MachineAccountPayload{}, false
}

// dyingDial opens control clients that are already dead: every attach
// "succeeds" and the client exits at once, the way a real client does when
// the session it attached to is gone. The lifecycle supervisor therefore
// spends its whole life losing its host, announcing it, and re-attaching.
func dyingDial(_ context.Context, _, session string) (ctlConn, error) {
	c := &fakeCtl{session: session, notes: make(chan ctlNote)}
	close(c.notes)
	c.closed = true
	return c, nil
}

// The stream has two writers to its output — the engine, and the lifecycle
// supervisor announcing a lost host — and only one of them closes it. Closing
// the stream while the supervisor was mid-announcement closed the channel
// under a concurrent send: the race detector reports it, and outside the
// detector the same interleaving is a send on a closed channel, which panics
// the whole service rather than ending one subscription. It was first seen in
// a test that unintentionally attached real multiplexer clients, which die at
// once for exactly this reason.
func TestClosingDuringLifecycleReattachDoesNotRaceTheEngine(t *testing.T) {
	for i := 0; i < 20; i++ {
		f := twoSessions()
		d := New("testbox",
			withExec(f.exec),
			withCtlDialer(dyingDial),
			withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		)
		s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
		if err != nil {
			t.Fatal(err)
		}
		// Let the supervisor go round its lose-announce-reattach loop a few
		// times with nobody reading, so a send is in flight when Close lands.
		time.Sleep(time.Duration(5+i) * time.Millisecond)
		_ = s.Close()

		// The stream must still end cleanly: every writer stopped, then EOF.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		for {
			if _, err := s.Next(ctx); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("iteration %d: stream did not end after Close: %v", i, err)
				}
				break
			}
		}
		cancel()
	}
}

// A lifecycle client that died with its host session must still be reaped.
//
// For the real transport Close is the only place the client subprocess is
// waited on, so a supervisor that re-attaches without closing the client it
// lost leaves one zombie per host-session death, per live subscription — on
// a long-running service over sessions that come and go, without bound.
// Every replaced client must report closed, and the live one must not.
func TestLifecycleClientThatLostItsHostIsReaped(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// The supervisor announces each loss on the stream and blocks until
	// someone reads it, so read everything until the stream ends.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := s.Next(context.Background()); err != nil {
				return
			}
		}
	}()

	// Index 0 is the first lifecycle client; every client dialled after the
	// content clients is a replacement lifecycle client.
	base := r.count()
	lifecycle := []*fakeCtl{r.at(0)}

	const deaths = 3
	for n := 0; n < deaths; n++ {
		lifecycle[n].die()

		deadline := time.Now().Add(3 * time.Second)
		for r.count() < base+n+1 {
			if time.Now().After(deadline) {
				t.Fatalf("death %d: no re-attach, still %d clients", n+1, r.count())
			}
			time.Sleep(5 * time.Millisecond)
		}
		lifecycle = append(lifecycle, r.at(base+n))
	}

	for i, c := range lifecycle[:deaths] {
		if !c.isClosed() {
			t.Errorf("lifecycle client %d lost its host and was replaced but never closed; "+
				"for a real client that is a subprocess nobody waits on", i)
		}
	}
	if lifecycle[deaths].isClosed() {
		t.Error("the live replacement lifecycle client was closed while the stream is open")
	}

	_ = s.Close()
	<-drained
	if !r.allClosed() {
		t.Error("clients left open after Close")
	}
}

// A content client whose session exited must be reaped while the subscription
// stays open.
//
// A content client exits with the session it is attached to, and for the real
// transport Close is the only place its process is waited on. The fake
// multiplexer here still lists the session afterwards, which is exactly the
// shape the engine's diff cannot see: a session re-created under the same id
// between two reads is never observed to close, so nothing but the pump knows
// its client died. Left in the stream's client map, each one is a zombie for
// the life of the subscription and holds a maxContentClients slot besides.
// die(), not Close(), is what the test drives: a fake that conflated the two
// could not show the reap missing.
func TestContentClientWhoseSessionExitedIsReaped(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := s.Next(context.Background()); err != nil {
				return
			}
		}
	}()

	dead, live := r.contentFor("beta"), r.contentFor("alpha💬")
	if dead == nil || live == nil {
		t.Fatalf("expected a content client per session, opened %d", r.count())
	}
	dead.die()

	es := s.(*eventStream)
	// Asserted by IDENTITY, not by absence. Since #172 the stream re-attaches a
	// fresh client for a session whose client died while the session is still
	// listed, so "no entry for beta" is true for one coalesce window and then
	// stops being true — an absence assertion here would be a test racing a
	// timer. What must never be true again is that THIS client, the dead one, is
	// still the stream's client for beta.
	heldDead := func() bool {
		es.mu.Lock()
		defer es.mu.Unlock()
		cur, ok := es.conns["beta"]
		return ok && cur == ctlConn(dead)
	}
	deadline := time.Now().Add(2 * time.Second)
	for (!dead.isClosed() || heldDead()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !dead.isClosed() {
		t.Error("content client exited with its session but was never closed; " +
			"for a real client that is a subprocess nobody waits on")
	}
	if heldDead() {
		t.Error("the dead content client is still held by the stream, occupying a client slot")
	}

	// The subscription itself is untouched.
	es.mu.Lock()
	closed := es.closed
	es.mu.Unlock()
	if closed {
		t.Error("a content client dying closed the whole subscription")
	}
	if live.isClosed() {
		t.Error("the other session's content client was closed")
	}
	if r.at(0).isClosed() {
		t.Error("the lifecycle client was closed")
	}

	_ = s.Close()
	<-drained
	if !r.allClosed() {
		t.Error("clients left open after Close")
	}
}

// currentContent reads the client the stream considers live for a session,
// which is the only thing that can answer "did it re-attach": the registry
// records every client ever opened, and contentFor returns the first match —
// after a re-attach that is the DEAD one.
func currentContent(es *eventStream, id string) *fakeCtl {
	es.mu.Lock()
	defer es.mu.Unlock()
	c, _ := es.conns[id].(*fakeCtl)
	return c
}

// A session killed and re-created under the same id must get a NEW content
// client while the subscription stays open (#172).
//
// Follow-up to #170, and only observable because of it. A content client dies
// with its session; a session that exits and comes back under the same id
// between two reads is never seen to close, so the engine's diff never takes
// the one branch that opens a content client — the id never left `known`.
// Before #170 the dead client sat in the stream's map and the gap looked like a
// live attachment; after it, the state is a matching session in `known` with no
// client at all, and this is the test that it does not stay that way.
//
// The fake multiplexer listing the session throughout IS the re-creation: from
// the driver's side an incarnation it never saw leave is indistinguishable from
// one that never left, which is exactly why the pump's mark is needed.
func TestRecreatedSessionGetsANewContentClient(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	es := s.(*eventStream)

	dead := r.contentFor("beta")
	if dead == nil {
		t.Fatalf("expected a content client for beta, opened %d", r.count())
	}
	// The session exited: its client's notifications end, and nobody reaped it.
	dead.die()

	var fresh *fakeCtl
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := currentContent(es, "beta"); c != nil && c != dead {
			fresh = c
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fresh == nil {
		t.Fatalf("the session is still listed and its content client died, but no new client was "+
			"dialled for it (%d clients opened in total); its state changes have silently "+
			"stopped being pushed for the life of the subscription", r.count())
	}
	if fresh.isClosed() {
		t.Fatal("the replacement content client was closed on arrival")
	}

	// Present in the map is not the claim — being WIRED is. A client whose
	// notifications do not reach the engine's trigger would satisfy every
	// assertion above and still leave the session's push latency broken.
	f.setCapture("%2", fixtureWorking)
	fire(fresh, "output")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("the replacement content client's notification drove no re-read; it is attached " +
			"to the stream's map but not to its trigger")
	}
	if ev.Kind != fleet.EventSessionState {
		t.Fatalf("want session.state, got %q", ev.Kind)
	}
	p, ok := ev.Payload.(fleet.SessionStatePayload)
	if !ok {
		t.Fatalf("payload type %T, want SessionStatePayload", ev.Payload)
	}
	if p.Ref.ID != "beta" || p.State.Status != fleet.StatusWorking {
		t.Errorf("got %q -> %q, want beta -> working", p.Ref.ID, p.State.Status)
	}

	// The rest of the subscription is untouched by any of it.
	if r.at(0).isClosed() {
		t.Error("the lifecycle client was closed")
	}
	if c := r.contentFor("alpha💬"); c == nil || c.isClosed() {
		t.Error("the other session's content client was disturbed")
	}
}

// Re-attaching must be bounded, not merely rate-limited (#172).
//
// The re-attach is driven by a death, so a session that keeps enumerating while
// every attachment to it dies at once produces a death per attempt: without a
// budget that is one dial plus one full enumeration every coalesce window, for
// as long as the subscription lives. This is the same re-dial storm that
// disqualified re-attaching every matching session on every pass, reached from
// the other direction, so the bound is measured here rather than assumed.
//
// A dial that FAILS is deliberately not this case and needs no budget: it opens
// no client, so nothing dies and nothing marks the id.
func TestReattachGivesUpOnAContentClientThatNeverWorks(t *testing.T) {
	f := twoSessions()
	// The lifecycle client attaches to the first enumerated session, so scoping
	// the flap to beta leaves the supervisor out of it.
	r := &ctlRegistry{dieFor: map[string]bool{"beta": true}}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// Registration order matters, and it is LIFO: the wait is registered first
	// so that Close runs before it. A test that waits for the drain first hangs
	// on every exit path, because at that point nothing has ended the stream.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := s.Next(context.Background()); err != nil {
				return
			}
		}
	}()
	defer func() { <-drained }()
	defer s.Close()

	// One dial at subscribe time, then at most maxFutileReattach replacements.
	want := 1 + maxFutileReattach
	deadline := time.Now().Add(10 * time.Second)
	for r.dialsFor("beta") < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.dialsFor("beta"); got != want {
		t.Fatalf("dialled %d content clients for a session whose attachment always dies, want %d "+
			"(one initial + %d re-attaches)", got, want, maxFutileReattach)
	}

	// And then it stops. Several coalesce windows with no growth is the bound;
	// an unbounded implementation adds one dial per window.
	time.Sleep(5 * coalesceWindow)
	if got := r.dialsFor("beta"); got != want {
		t.Errorf("still re-dialling after the budget was spent: %d clients, want %d — a session "+
			"that lists but cannot be held costs one enumeration per coalesce window forever",
			got, want)
	}

	// Giving up is scoped to that session. The subscription and everything else
	// on it stay live.
	es := s.(*eventStream)
	es.mu.Lock()
	closed := es.closed
	es.mu.Unlock()
	if closed {
		t.Error("giving up on one session's content client closed the whole subscription")
	}
	if c := r.contentFor("alpha💬"); c == nil || c.isClosed() {
		t.Error("the other session's content client was disturbed")
	}
	if r.at(0).isClosed() {
		t.Error("the lifecycle client was closed")
	}
}

// A session observed to close must not carry its futile count into a later
// incarnation of the same id (#172).
//
// Session ids are reused — a launcher re-creating the same named session is the
// ordinary case, not an exotic one. A budget that was never cleared would make a
// subscription progressively deafer to one name the longer it ran, and the
// symptom would be latency: the hardest kind of bug to attribute to a counter
// nobody reset.
func TestReattachBudgetResetsWhenTheSessionIsObservedGone(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	es := s.(*eventStream)
	// Registration order matters, and it is LIFO: the wait is registered first
	// so that Close runs before it. A test that waits for the drain first hangs
	// on every exit path, because at that point nothing has ended the stream.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := s.Next(context.Background()); err != nil {
				return
			}
		}
	}()
	defer func() { <-drained }()
	defer s.Close()

	// A session this stream can watch appear and disappear, so the whole
	// lifecycle runs through the engine's own diff rather than being arranged.
	f.addSession(fakeSession{
		name: "gamma", paneID: "%9", cwd: "/work/gamma", pid: 300, created: 1785600002,
	}, idleFixtureFor("gamma"))
	fire(r.at(0), "sessions-changed")

	var born *fakeCtl
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := currentContent(es, "gamma"); c != nil {
			born = c
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if born == nil {
		t.Fatal("a session appeared and matched the filter but got no content client")
	}

	// It dies having said nothing, which spends one unit of the budget.
	born.die()
	spent := func() int {
		es.mu.Lock()
		defer es.mu.Unlock()
		return es.reattach["gamma"].futile
	}
	deadline = time.Now().Add(3 * time.Second)
	for spent() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if spent() == 0 {
		t.Fatal("a content client died without delivering anything and nothing was counted " +
			"against the re-attach budget")
	}

	// Now the session really goes away, and the engine sees it go: everything
	// the stream remembers about that id is stale, the counter included.
	f.dropLastSession()
	fire(r.at(0), "sessions-changed")

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		es.mu.Lock()
		_, held := es.reattach["gamma"]
		es.mu.Unlock()
		if !held {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("gamma closed but its re-attach bookkeeping survived (futile=%d); a reused session "+
		"id would start its next life with a spent budget", spent())
}

// withSettleDelays shortens the follow-up schedule (#343) so a test does not
// wait out the production one.
func withSettleDelays(ds ...time.Duration) Option {
	return func(d *Driver) { d.settleDelays = ds }
}

// #343: a session created while the subscription is already at its content
// client cap has no client that can hear its own startup repaint, so nothing
// ever wakes a read for it. The feed announced it as unknown and then said
// nothing; here the repaint (unknown -> idle) happens with NO notification on
// any client, and the stream must still carry the settled state.
func TestSubscribeFollowsUpANewSessionThatHasNoContentClient(t *testing.T) {
	f := twoSessions()
	for i := 0; i < maxContentClients+4; i++ {
		id := "%" + intToStr(500+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "big" + intToStr(i), paneID: id, cwd: "/w", pid: 5000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("big" + intToStr(i))
	}
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	withSettleDelays(50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond)(d)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A new session whose screen has not painted yet. The lifecycle client says
	// it exists; no content client can be opened for it (cap is full).
	f.addSession(fakeSession{
		name: "newcomer", paneID: "%9", cwd: "/work/new", pid: 300, created: 1785600003,
	}, "")
	fire(r.opened[0], "sessions-changed")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok || ev.Kind != fleet.EventSessionCreated {
		t.Fatalf("want session.created first, got %+v ok=%v", ev, ok)
	}
	if sess := ev.Payload.(fleet.Session); sess.State.Status != fleet.StatusUnknown {
		t.Fatalf("fixture error: the newcomer must start unknown, got %q", sess.State.Status)
	}
	if r.contentFor("newcomer") != nil {
		t.Fatal("fixture error: the cap was supposed to leave the newcomer without a content client")
	}

	// The repaint. No notification fires anywhere.
	f.setCapture("%9", idleFixtureFor("newcomer"))

	ev, ok = nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no session.state for a new session whose state settled without any notification")
	}
	p, isState := ev.Payload.(fleet.SessionStatePayload)
	if ev.Kind != fleet.EventSessionState || !isState || p.Ref.ID != "newcomer" || p.State.Status == fleet.StatusUnknown {
		t.Fatalf("want session.state newcomer -> not unknown, got %q %+v", ev.Kind, ev.Payload)
	}
}

// The follow-up is armed by an unsettled session and by nothing else: a fleet
// in which every session is settled reads nothing without a notification.
func TestSettleFollowUpArmsNothingOnAQuietFleet(t *testing.T) {
	f := &fakeMux{
		sessions: []fakeSession{{name: "a", paneID: "%1", cwd: "/w", pid: 1, created: 1785600000, title: "2_1_220"}},
		captures: map[string]string{"%1": idleFixtureFor("a")},
	}
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	withSettleDelays(10 * time.Millisecond)(d)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	before := len(f.callsSnapshot())
	time.Sleep(300 * time.Millisecond)
	if after := len(f.callsSnapshot()); after != before {
		t.Errorf("a settled, quiet fleet made %d multiplexer calls with no notification", after-before)
	}
}

func TestSettled(t *testing.T) {
	id := "session_x"
	for _, tc := range []struct {
		name string
		st   fleet.SessionState
		want bool
	}{
		{"unknown", fleet.SessionState{Status: fleet.StatusUnknown}, false},
		{"idle, no channel", fleet.SessionState{Status: fleet.StatusIdle}, true},
		{"idle, channel connecting", fleet.SessionState{Status: fleet.StatusIdle,
			ControlChannel: &fleet.ControlChannel{State: fleet.ControlChannelConnecting}}, false},
		{"idle, active without a bridge id", fleet.SessionState{Status: fleet.StatusIdle,
			ControlChannel: &fleet.ControlChannel{State: fleet.ControlChannelActive}}, false},
		{"idle, active with a bridge id", fleet.SessionState{Status: fleet.StatusIdle,
			ControlChannel: &fleet.ControlChannel{State: fleet.ControlChannelActive, BridgeID: &id}}, true},
		{"idle, channel off", fleet.SessionState{Status: fleet.StatusIdle,
			ControlChannel: &fleet.ControlChannel{State: fleet.ControlChannelOff}}, true},
	} {
		if got := settled(tc.st); got != tc.want {
			t.Errorf("%s: settled = %v, want %v", tc.name, got, tc.want)
		}
	}
}
