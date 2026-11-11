package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// Everything in this file is synthetic: fake drivers over generated rows, no
// real session content (muster #464).

// listRig is a service watched the way a mirror client watches it — one
// unfiltered local subscriber, so the event plane runs — over a fake driver
// that counts its enumerations.
type listRig struct {
	svc    *Service
	srv    *httptest.Server
	d      *fleetFakeDriver
	cancel func() // the watcher
}

func newListRig(t *testing.T, n int) *listRig {
	t.Helper()
	svc := New("testbox")
	d := newFleetFakeDriver(syntheticFleet(n))
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	rig := &listRig{svc: svc, srv: httptestServer(t, svc), d: d}
	rig.cancel = watchLocal(t, svc, driver.SubscribeFilter{})
	waitCovered(t, svc)
	return rig
}

// watchLocal attaches a local-scope subscriber and returns its release.
func watchLocal(t *testing.T, svc *Service, f driver.SubscribeFilter) func() {
	t.Helper()
	_, _, _, cancel := svc.Events(context.Background(), ScopeLocal, f, 0, "")
	var once sync.Once
	release := func() { once.Do(cancel) }
	t.Cleanup(release)
	return release
}

// waitCovered blocks until the snapshot's precondition holds: the lane is up,
// unfiltered, and every pump has attached.
func waitCovered(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, ok := svc.listRevisionNow(); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the event plane never reached a state that covers the local runtimes")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type listReply struct {
	code   int
	header http.Header
	body   []byte
}

func (r listReply) etag() string { return r.header.Get("ETag") }

func (r listReply) collection(t *testing.T) fleet.Collection[fleet.Session] {
	t.Helper()
	var col fleet.Collection[fleet.Session]
	if err := json.Unmarshal(r.body, &col); err != nil {
		t.Fatalf("decoding %q: %v", r.body, err)
	}
	return col
}

func listGet(t *testing.T, srv *httptest.Server, query string, header map[string]string) listReply {
	t.Helper()
	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions"+query, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return listReply{code: resp.StatusCode, header: resp.Header, body: body}
}

func inm(v string) map[string]string { return map[string]string{"If-None-Match": v} }

// ---------------------------------------------------------------- the oracle

func TestListSnapshot_TwoReadsWithNoChangeEnumerateOnce(t *testing.T) {
	rig := newListRig(t, 200)
	a := listGet(t, rig.srv, "?scope=local", nil)
	b := listGet(t, rig.srv, "?scope=local", nil)
	if a.code != 200 || b.code != 200 {
		t.Fatalf("codes %d, %d", a.code, b.code)
	}
	if got := rig.d.Enumerations(); got != 1 {
		t.Fatalf("two reads with nothing changed enumerated %d times, want 1", got)
	}
	if a.etag() == "" || a.etag() != b.etag() {
		t.Fatalf("validators %q and %q, want one non-empty value", a.etag(), b.etag())
	}
	if string(a.body) != string(b.body) {
		t.Fatal("the same snapshot was served as two different bodies")
	}
	if n := len(a.collection(t).Items()); n != 200 {
		t.Fatalf("served %d items, want 200", n)
	}
}

func TestListSnapshot_AChangeBetweenReadsReenumeratesAndChangesTheETag(t *testing.T) {
	rig := newListRig(t, 20)
	a := listGet(t, rig.srv, "?scope=local", nil)
	rig.d.Replace(syntheticFleet(21))
	rig.svc.events.publish(createdEvent("testbox", "syn-0020", "/work/synthetic/020"))
	b := listGet(t, rig.srv, "?scope=local", nil)

	if got := rig.d.Enumerations(); got != 2 {
		t.Fatalf("enumerations = %d, want 2 (a change between the reads)", got)
	}
	if a.etag() == b.etag() {
		t.Fatalf("the validator did not change across a change: %q", a.etag())
	}
	if n := len(b.collection(t).Items()); n != 21 {
		t.Fatalf("the second read served %d items, want the 21 after the change", n)
	}
}

func TestListSnapshot_IfNoneMatchCurrentIs304WithNoBody(t *testing.T) {
	rig := newListRig(t, 50)
	first := listGet(t, rig.srv, "?scope=local", nil)
	got := listGet(t, rig.srv, "?scope=local", inm(first.etag()))

	if got.code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", got.code)
	}
	if len(got.body) != 0 {
		t.Fatalf("a 304 carried %d bytes of body", len(got.body))
	}
	if got.etag() != first.etag() {
		t.Fatalf("the 304 carries %q, want the validator %q", got.etag(), first.etag())
	}
	if cc := got.header.Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if v := got.header.Get("Vary"); v != "Authorization" {
		t.Errorf("Vary = %q, want Authorization", v)
	}
	if rig.d.Enumerations() != 1 {
		t.Fatalf("the conditional read enumerated (%d total)", rig.d.Enumerations())
	}
}

func TestListSnapshot_IfNoneMatchStaleIs200WithTheNewValidator(t *testing.T) {
	rig := newListRig(t, 10)
	old := listGet(t, rig.srv, "?scope=local", nil)
	rig.svc.events.publish(stateEventOn("testbox", fleet.EventSessionState, "syn-0000"))

	for name, header := range map[string]string{
		"a stale validator":      old.etag(),
		"a list of stale ones":   `"not-it", ` + old.etag(),
		"a weak stale validator": "W/" + old.etag(),
	} {
		got := listGet(t, rig.srv, "?scope=local", inm(header))
		if got.code != 200 {
			t.Fatalf("%s: status %d, want 200", name, got.code)
		}
		if got.etag() == "" || got.etag() == old.etag() {
			t.Fatalf("%s: new validator %q, old %q", name, got.etag(), old.etag())
		}
		if len(got.body) == 0 {
			t.Fatalf("%s: empty body", name)
		}
	}

	// And the current one, in each spelling a client may send it.
	cur := listGet(t, rig.srv, "?scope=local", nil).etag()
	for name, header := range map[string]string{
		"a weak current one":       "W/" + cur,
		"a list holding it":        `"nope", ` + cur + `, "other"`,
		"the any-representation *": "*",
	} {
		if got := listGet(t, rig.srv, "?scope=local", inm(header)); got.code != http.StatusNotModified {
			t.Errorf("%s: status %d, want 304", name, got.code)
		}
	}
}

func TestIfNoneMatchHits(t *testing.T) {
	const tag = `"abc"`
	for header, want := range map[string]bool{
		``:                      false,
		`*`:                     true,
		`"abc"`:                 true,
		`W/"abc"`:               true,
		`"x","abc"`:             true,
		` "x" , W/"abc" `:       true,
		`"abcd"`:                false,
		`abc`:                   false,
		`"x", "y"`:              false,
		`W/"x", W/"y"`:          false,
		`,,"abc"`:               true,
		`"ABC"`:                 false,
		`W/`:                    false,
		`"x"` + "\t" + `,"abc"`: true,
	} {
		if got := ifNoneMatchHits(header, tag); got != want {
			t.Errorf("ifNoneMatchHits(%q) = %v, want %v", header, got, want)
		}
	}
}

// ------------------------------------------------- one negative per source

// readTwiceAround performs: read, change(), read — and reports whether the
// second read enumerated again and carried a different validator. A snapshot
// that survives change() is the bug each test below guards against.
func readTwiceAround(t *testing.T, rig *listRig, change func()) (reenumerated, validatorMoved bool) {
	t.Helper()
	a := listGet(t, rig.srv, "?scope=local", nil)
	if a.etag() == "" {
		t.Fatal("the first read was offered no validator — the rig is not covered")
	}
	before := rig.svc.lists.built.Load()
	change()
	b := listGet(t, rig.srv, "?scope=local", nil)
	return rig.svc.lists.built.Load() > before, b.etag() != a.etag()
}

func TestListSnapshot_DriverEventInvalidates(t *testing.T) {
	// Through the whole path: the driver's own stream, the pump, the hub.
	for name, ev := range map[string]fleet.Event{
		"created": createdEvent("testbox", "syn-0099", "/work/synthetic/099"),
		"state":   stateEventOn("testbox", fleet.EventSessionState, "syn-0000"),
		"closed":  stateEventOn("testbox", fleet.EventSessionClosed, "syn-0000"),
	} {
		t.Run(name, func(t *testing.T) {
			rig := newListRig(t, 5)
			re, moved := readTwiceAround(t, rig, func() {
				at := rig.svc.events.currentCursor()
				rig.d.events <- ev
				waitUntil(t, "the event to reach the hub", func() bool { return rig.svc.events.currentCursor() > at })
			})
			if !re || !moved {
				t.Fatalf("a %s event did not invalidate: reenumerated=%v validatorMoved=%v", name, re, moved)
			}
		})
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// watchedLabelDriver is labelDriver — which can create, rename, close and list
// like a substrate — made watchable, with its enumerations counted. Its stream
// carries nothing: every effect below must reach the snapshot WITHOUT an event,
// which is exactly the case the service-side generation exists for.
type watchedLabelDriver struct {
	*labelDriver
	calls  atomic.Int64
	events chan fleet.Event
}

func newWatchedLabelDriver() *watchedLabelDriver {
	return &watchedLabelDriver{labelDriver: newLabelDriver(), events: make(chan fleet.Event, 8)}
}

func (d *watchedLabelDriver) List(ctx context.Context, req fleet.Request, f driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	d.calls.Add(1)
	return d.labelDriver.List(ctx, req, f)
}

func (d *watchedLabelDriver) Subscribe(ctx context.Context, req fleet.Request, f driver.SubscribeFilter) (driver.EventStream, error) {
	return &feedStream{events: d.events}, nil
}

func TestListSnapshot_WritesThroughTheServiceInvalidateBeforeAnyEvent(t *testing.T) {
	newRig := func(t *testing.T) (*Service, *httptest.Server, *watchedLabelDriver, string) {
		t.Helper()
		svc := New("test-machine")
		d := newWatchedLabelDriver()
		if err := svc.RegisterLocalDriver("fake", d); err != nil {
			t.Fatal(err)
		}
		srv := httptestServer(t, svc)
		code, raw := create(t, srv, testToken, nil)
		if code != http.StatusCreated {
			t.Fatalf("create = %d %s", code, raw)
		}
		id := decodeSession(t, raw).ID
		watchLocal(t, svc, driver.SubscribeFilter{})
		waitCovered(t, svc)
		d.calls.Store(0) // what the setup itself listed is not what is measured
		return svc, srv, d, id
	}
	readTwice := func(t *testing.T, srv *httptest.Server, d *watchedLabelDriver, write func(srv *httptest.Server, id string), id string) {
		t.Helper()
		a := listGet(t, srv, "?scope=local", nil)
		if a.etag() == "" {
			t.Fatal("the first read was offered no validator")
		}
		again := listGet(t, srv, "?scope=local", nil)
		if again.etag() != a.etag() || d.calls.Load() != 1 {
			t.Fatalf("an unchanged read was not served from the snapshot (enumerations %d)", d.calls.Load())
		}
		write(srv, id)
		b := listGet(t, srv, "?scope=local", nil)
		if d.calls.Load() < 2 || b.etag() == a.etag() {
			t.Fatalf("the write did not invalidate: enumerations=%d, validator moved=%v", d.calls.Load(), b.etag() != a.etag())
		}
	}

	t.Run("label write", func(t *testing.T) {
		_, srv, d, id := newRig(t)
		readTwice(t, srv, d, func(srv *httptest.Server, id string) {
			if code, raw := postLabels(t, srv, testToken, id, "", map[string]any{"k": "v"}); code != http.StatusOK {
				t.Fatalf("labels = %d %s", code, raw)
			}
		}, id)
	})
	t.Run("close", func(t *testing.T) {
		_, srv, d, id := newRig(t)
		readTwice(t, srv, d, func(srv *httptest.Server, id string) {
			if code, raw := labelCall(t, http.MethodDelete, srv.URL+"/v1/machines/test-machine/sessions/"+id, testToken, nil, nil); code/100 != 2 {
				t.Fatalf("close = %d %s", code, raw)
			}
		}, id)
	})
	t.Run("create without labels", func(t *testing.T) {
		_, srv, d, id := newRig(t)
		readTwice(t, srv, d, func(srv *httptest.Server, _ string) {
			if code, raw := create(t, srv, testToken, nil); code != http.StatusCreated {
				t.Fatalf("create = %d %s", code, raw)
			}
		}, id)
	})
	t.Run("rename", func(t *testing.T) {
		_, srv, d, id := newRig(t)
		readTwice(t, srv, d, func(srv *httptest.Server, id string) {
			code, raw := labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/"+id+"/rename", testToken,
				map[string]string{"name": "renamed"}, nil)
			if code/100 != 2 {
				t.Fatalf("rename = %d %s", code, raw)
			}
		}, id)
	})
}

func TestListSnapshot_ARefusedMutationDoesNotChurnTheSnapshot(t *testing.T) {
	svc := New("test-machine")
	d := newWatchedLabelDriver()
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	// This instance refuses every mutation of its own sessions.
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: false}))
	t.Cleanup(srv.Close)
	watchLocal(t, svc, driver.SubscribeFilter{})
	waitCovered(t, svc)

	a := listGet(t, srv, "?scope=local", nil)
	if code, _ := labelCall(t, http.MethodDelete, srv.URL+"/v1/machines/test-machine/sessions/s1", testToken, nil, nil); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("the mutation was not refused: %d", code)
	}
	b := listGet(t, srv, "?scope=local", nil)
	if a.etag() == "" || a.etag() != b.etag() || d.calls.Load() != 1 {
		t.Fatalf("a refused request moved the snapshot (enumerations %d)", d.calls.Load())
	}
}

func TestListSnapshot_PendingRuntimeChangeInvalidates(t *testing.T) {
	rig := newListRig(t, 5)
	pending := OptionalRuntime{Runtime: "later", DeadlineMs: 1000}
	rig.svc.recordUnavailable(pending, fleet.CauseProbeTimeout, 0, true)
	re, moved := readTwiceAround(t, rig, func() {
		rig.svc.recordUnavailable(pending, fleet.CauseProbeTimeout, 1, true)
	})
	if !re || !moved {
		t.Fatalf("a change to a pending runtime's row did not invalidate: reenumerated=%v moved=%v", re, moved)
	}
	// And a pending runtime does not by itself make a listing unkeepable: its
	// source is the service's own statement, not a driver that failed to answer.
	if got := listGet(t, rig.srv, "?scope=local", nil); got.etag() == "" {
		t.Fatal("a listing carrying a pending-runtime source was offered no validator")
	}
}

func TestListSnapshot_LateRegisteredRuntimeBypassesUntilItIsPumped(t *testing.T) {
	rig := newListRig(t, 5)
	if listGet(t, rig.srv, "?scope=local", nil).etag() == "" {
		t.Fatal("no validator before the runtime registered")
	}
	// A runtime registered after the lane started is pumped from then on (#465);
	// this one cannot attach on its first attempt, so for one backoff nothing the
	// snapshot could be coherent against reports its changes.
	late := newFleetFakeDriver(nil)
	late.failFirst, late.failWith = 1, fmt.Errorf("the runtime was not answering yet")
	if err := rig.svc.RegisterLocalDriver("late", late); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got := listGet(t, rig.srv, "?scope=local", nil)
		if got.etag() != "" {
			t.Fatalf("read %d: offered a validator for a listing the event plane does not cover", i)
		}
	}
	if rig.d.Enumerations() < 3 || late.Enumerations() < 3 {
		t.Fatalf("reads were not enumerated: %d / %d", rig.d.Enumerations(), late.Enumerations())
	}

	// Once it is pumped the listing is cached again — it does not bypass for as
	// long as the lane runs, which is what it did while nothing ever pumped it.
	waitCovered(t, rig.svc)
	first := listGet(t, rig.srv, "?scope=local", nil)
	second := listGet(t, rig.srv, "?scope=local", nil)
	if first.etag() == "" || first.etag() != second.etag() {
		t.Fatalf("after the late runtime was pumped: validators %q / %q", first.etag(), second.etag())
	}
}

func TestListSnapshot_PeerRegistrationInvalidates(t *testing.T) {
	rig := newListRig(t, 5)
	re, moved := readTwiceAround(t, rig, func() {
		if err := rig.svc.RegisterPeerDriver("peerbox", &stub.Driver{DeadlineMs: 200}); err != nil {
			t.Fatal(err)
		}
	})
	if !re || !moved {
		t.Fatalf("registering a peer did not invalidate: reenumerated=%v moved=%v", re, moved)
	}
	rig.svc.UnregisterPeerDriver("peerbox")
	if listGet(t, rig.srv, "?scope=local", nil).etag() == "" {
		t.Fatal("removing the peer left the local read without a validator")
	}
}

func TestListSnapshot_AFailedSubscriptionBypassesUntilItIsBack(t *testing.T) {
	svc := New("testbox")
	d := newFleetFakeDriver(syntheticFleet(5))
	d.failFirst, d.failWith = 1, fmt.Errorf("the multiplexer was not there")
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptestServer(t, svc)
	watchLocal(t, svc, driver.SubscribeFilter{})
	waitForAttempts(t, &d.feedDriver, 1)

	// The stream is up in the hub's books and deaf in fact.
	got := listGet(t, srv, "?scope=local", nil)
	if got.etag() != "" {
		t.Fatal("offered a validator while the driver's subscription had failed")
	}
	// Retried after the first backoff; from then on the read is cached again.
	waitCovered(t, svc)
	first := listGet(t, srv, "?scope=local", nil)
	second := listGet(t, srv, "?scope=local", nil)
	if first.etag() == "" || first.etag() != second.etag() {
		t.Fatalf("after recovery: validators %q / %q", first.etag(), second.etag())
	}
}

func TestListSnapshot_ADriverThatCannotStreamIsNeverCovered(t *testing.T) {
	svc := New("testbox")
	d := &listCountingDriver{Driver: stub.Driver{DeadlineMs: 200}, sessions: []fleet.Session{existingSession("s1", "/work")}}
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptestServer(t, svc)
	watchLocal(t, svc, driver.SubscribeFilter{})
	time.Sleep(100 * time.Millisecond) // the pump has had its one ErrUnsupported answer
	for i := 0; i < 3; i++ {
		if got := listGet(t, srv, "?scope=local", nil); got.etag() != "" {
			t.Fatalf("read %d: a validator for a runtime that reports no events", i)
		}
	}
	if d.Calls() != 3 {
		t.Fatalf("enumerations = %d, want one per read", d.Calls())
	}
}

func TestListSnapshot_LingerExpiryBypassesAndARestartInvalidates(t *testing.T) {
	svc := New("testbox")
	svc.events.linger = 0 // tear the lane down the moment the last watcher leaves
	d := newFleetFakeDriver(syntheticFleet(5))
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptestServer(t, svc)
	release := watchLocal(t, svc, driver.SubscribeFilter{})
	waitCovered(t, svc)
	old := listGet(t, srv, "?scope=local", nil)
	if old.etag() == "" {
		t.Fatal("no validator while watched")
	}

	release() // nobody is watching: the lane stops and the cursor freezes
	waitUntil(t, "the lane to stop", func() bool { return !svc.events.streamLive() })
	if got := listGet(t, srv, "?scope=local", nil); got.etag() != "" {
		t.Fatal("offered a validator with the lane stopped")
	}

	// A new watcher restarts it. Nothing was stamped in between — the cursor is
	// the same — yet the world may have moved, so the old validator must not hold.
	watchLocal(t, svc, driver.SubscribeFilter{})
	waitCovered(t, svc)
	fresh := listGet(t, srv, "?scope=local", nil)
	if fresh.etag() == "" || fresh.etag() == old.etag() {
		t.Fatalf("validator after a lane restart: %q (was %q)", fresh.etag(), old.etag())
	}
	if got := listGet(t, srv, "?scope=local", inm(old.etag())); got.code != 200 {
		t.Fatalf("the pre-restart validator was answered %d", got.code)
	}
}

func TestListSnapshot_ANarrowedLocalLaneBypasses(t *testing.T) {
	svc := New("testbox")
	d := newFleetFakeDriver(syntheticFleet(5))
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptestServer(t, svc)
	// Every watcher names its sessions, so the lane watches only those.
	watchLocal(t, svc, driver.SubscribeFilter{Sessions: []string{"syn-0001"}})
	waitForAttempts(t, &d.feedDriver, 1)
	for i := 0; i < 2; i++ {
		if got := listGet(t, srv, "?scope=local", nil); got.etag() != "" {
			t.Fatal("a validator from a lane that watches one session")
		}
	}
	if d.Enumerations() != 2 {
		t.Fatalf("enumerations = %d, want 2", d.Enumerations())
	}
}

func TestListSnapshot_AgeIsBounded(t *testing.T) {
	rig := newListRig(t, 5)
	var mu sync.Mutex
	clock := time.Now()
	rig.svc.lists.setClock(func() time.Time { mu.Lock(); defer mu.Unlock(); return clock })
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	a := listGet(t, rig.srv, "?scope=local", nil)
	advance(listSnapshotMaxAge - time.Millisecond)
	if b := listGet(t, rig.srv, "?scope=local", nil); b.etag() != a.etag() || rig.d.Enumerations() != 1 {
		t.Fatalf("just inside the bound the read enumerated (%d)", rig.d.Enumerations())
	}
	advance(2 * time.Millisecond)
	c := listGet(t, rig.srv, "?scope=local", nil)
	if rig.d.Enumerations() != 2 || c.etag() == "" || c.etag() == a.etag() {
		t.Fatalf("past the bound: enumerations=%d, validator %q (was %q)", rig.d.Enumerations(), c.etag(), a.etag())
	}
}

func TestListSnapshot_AnUnwatchedServiceOffersNoValidator(t *testing.T) {
	svc := New("testbox")
	d := newFleetFakeDriver(syntheticFleet(5))
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptestServer(t, svc)
	for i := 0; i < 3; i++ {
		got := listGet(t, srv, "?scope=local", nil)
		if got.etag() != "" {
			t.Fatal("a validator from a service nobody watches")
		}
		// And the response is what it always was: with no feed position, because
		// the cursor of an unwatched service is not a resume point.
		if got.collection(t).Feed() != nil {
			t.Fatal("an unwatched service published a feed position")
		}
	}
	if d.Enumerations() != 3 {
		t.Fatalf("enumerations = %d, want one per read", d.Enumerations())
	}
	if c := svc.lists.counters(); c["bypassed"] != 3 || c["served"] != 0 {
		t.Fatalf("counters = %v", c)
	}
}

func TestListSnapshot_ADegradedAnswerIsNotKept(t *testing.T) {
	rig := newListRig(t, 5)
	rig.d.tmu.Lock()
	rig.d.source = &fleet.SourceStatus{Machine: "testbox", Status: fleet.SourceDegraded, Error: "synthetic"}
	rig.d.tmu.Unlock()
	for i := 0; i < 2; i++ {
		if got := listGet(t, rig.srv, "?scope=local", nil); got.etag() != "" || got.collection(t).Complete() {
			t.Fatalf("read %d: a degraded listing was offered a validator", i)
		}
	}
	if rig.d.Enumerations() != 2 {
		t.Fatalf("enumerations = %d, want 2", rig.d.Enumerations())
	}
	rig.d.tmu.Lock()
	rig.d.source = nil
	rig.d.tmu.Unlock()
	if got := listGet(t, rig.srv, "?scope=local", nil); got.etag() == "" {
		t.Fatal("the recovered listing was not kept")
	}
}

func TestListSnapshot_AShortDeadlineThatCostADriverIsNotKept(t *testing.T) {
	rig := newListRig(t, 5)
	rig.d.tmu.Lock()
	rig.d.delay = 300 * time.Millisecond
	rig.d.tmu.Unlock()
	short := map[string]string{"Fleet-Deadline-Ms": "20"}
	got := listGet(t, rig.srv, "?scope=local", short)
	if got.etag() != "" || got.collection(t).Complete() {
		t.Fatal("a listing that lost a driver to the caller's deadline was offered a validator")
	}
	// A later caller with time to spare must not inherit it.
	rig.d.tmu.Lock()
	rig.d.delay = 0
	rig.d.tmu.Unlock()
	full := listGet(t, rig.srv, "?scope=local", nil)
	if full.etag() == "" || !full.collection(t).Complete() || len(full.collection(t).Items()) != 5 {
		t.Fatalf("the next read did not enumerate in full: etag %q", full.etag())
	}
}

// ------------------------------------------------------- scope and keying

func TestListSnapshot_FleetScopeWithAPeerOffersNoValidator(t *testing.T) {
	rig := newListRig(t, 5)
	if err := rig.svc.RegisterPeerDriver("peerbox", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	before := rig.d.Enumerations()
	for i := 0; i < 2; i++ {
		got := listGet(t, rig.srv, "?scope=fleet", nil)
		if got.etag() != "" {
			t.Fatal("a fleet read with a peer configured was offered a validator")
		}
	}
	if rig.d.Enumerations()-before != 2 {
		t.Fatalf("fleet reads enumerated %d times, want 2", rig.d.Enumerations()-before)
	}
	// The local read of the same machine is still kept.
	a := listGet(t, rig.srv, "?scope=local", nil)
	b := listGet(t, rig.srv, "?scope=local", nil)
	if a.etag() == "" || a.etag() != b.etag() {
		t.Fatalf("local validators %q / %q", a.etag(), b.etag())
	}
}

func TestListSnapshot_FleetScopeWithNoPeersIsTheLocalQuestion(t *testing.T) {
	rig := newListRig(t, 5)
	a := listGet(t, rig.srv, "?scope=local", nil)
	b := listGet(t, rig.srv, "?scope=fleet", nil)
	c := listGet(t, rig.srv, "", nil) // fleet is the default (§3.2)
	if a.etag() == "" || a.etag() != b.etag() || a.etag() != c.etag() {
		t.Fatalf("validators %q / %q / %q, want one shared by the same question", a.etag(), b.etag(), c.etag())
	}
	if rig.d.Enumerations() != 1 {
		t.Fatalf("enumerations = %d, want 1", rig.d.Enumerations())
	}
}

func TestListSnapshot_FilterIsPartOfTheKey(t *testing.T) {
	rig := newListRig(t, 5)
	plain := listGet(t, rig.srv, "?scope=local", nil)
	byStatus := listGet(t, rig.srv, "?scope=local&status=idle", nil)
	if plain.etag() == "" || byStatus.etag() == "" || plain.etag() == byStatus.etag() {
		t.Fatalf("a filtered read shared a validator with the plain one: %q / %q", plain.etag(), byStatus.etag())
	}
	if rig.d.Enumerations() != 2 {
		t.Fatalf("enumerations = %d, want one per question", rig.d.Enumerations())
	}
	// A validator from one question never matches another.
	if got := listGet(t, rig.srv, "?scope=local&status=idle", inm(plain.etag())); got.code != 200 {
		t.Fatalf("the plain validator was honoured for a filtered read: %d", got.code)
	}

	// The key is the parsed filter: label order and parameters the list does not
	// read neither split nor merge anything.
	a := listGet(t, rig.srv, "?scope=local&label=a:1&label=b:2", nil)
	b := listGet(t, rig.srv, "?scope=local&label=b:2&label=a:1&machine=x&runtime=y", nil)
	if a.etag() == "" || a.etag() != b.etag() {
		t.Fatalf("the same labels in another order and extra parameters: %q / %q", a.etag(), b.etag())
	}
	if rig.d.Enumerations() != 3 {
		t.Fatalf("enumerations = %d, want 3", rig.d.Enumerations())
	}
}

func TestListKey_IsUnambiguous(t *testing.T) {
	// Length-prefixed fields: two different filters cannot spell the same key.
	a := newListKey(ScopeLocal, driver.ListFilter{CwdPrefix: "ab", Agent: "c"})
	b := newListKey(ScopeLocal, driver.ListFilter{CwdPrefix: "a", Agent: "bc"})
	c := newListKey(ScopeLocal, driver.ListFilter{Labels: map[string]string{"a": "b|1:c"}})
	d := newListKey(ScopeLocal, driver.ListFilter{Labels: map[string]string{"a": "b", "1": "c"}})
	if a == b || c == d || a == c {
		t.Fatalf("colliding keys: %q %q %q %q", a, b, c, d)
	}
	if newListKey(ScopeLocal, driver.ListFilter{}) == newListKey(ScopeFleet, driver.ListFilter{}) {
		t.Fatal("scope is not part of the key")
	}
}

func TestListSnapshot_EntriesAreBounded(t *testing.T) {
	rig := newListRig(t, 3)
	for i := 0; i < listSnapshotMaxEntries+5; i++ {
		listGet(t, rig.srv, fmt.Sprintf("?scope=local&cwdPrefix=/q%d", i), nil)
	}
	if n := rig.svc.lists.entryCount(); n > listSnapshotMaxEntries {
		t.Fatalf("%d entries kept, bound is %d", n, listSnapshotMaxEntries)
	}
}

func TestListSnapshot_StaleEntriesAreDroppedWhenTheRevisionMoves(t *testing.T) {
	rig := newListRig(t, 3)
	for i := 0; i < 4; i++ {
		listGet(t, rig.srv, fmt.Sprintf("?scope=local&cwdPrefix=/q%d", i), nil)
	}
	rig.svc.events.publish(stateEventOn("testbox", fleet.EventSessionState, "syn-0000"))
	listGet(t, rig.srv, "?scope=local", nil)
	if n := rig.svc.lists.entryCount(); n != 1 {
		t.Fatalf("%d entries after the revision moved, want only the fresh one", n)
	}
}

func TestListSnapshot_ASweepAlwaysEnumerates(t *testing.T) {
	rig := newListRig(t, 5)
	listGet(t, rig.srv, "?scope=local", nil)
	before := rig.d.Enumerations()
	// SweepLocal is what notices closed sessions (history.go); it must read the
	// drivers whatever a snapshot holds, or a close would wait for the age bound.
	rig.svc.SweepLocal(context.Background())
	if rig.d.Enumerations() != before+1 {
		t.Fatalf("a sweep did not enumerate (%d -> %d)", before, rig.d.Enumerations())
	}
}

func TestListSnapshot_ConcurrentColdReadsShareOneEnumeration(t *testing.T) {
	rig := newListRig(t, 50)
	rig.d.tmu.Lock()
	rig.d.delay = 150 * time.Millisecond
	rig.d.tmu.Unlock()

	const readers = 8
	replies := make([]listReply, readers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			replies[i] = listGet(t, rig.srv, "?scope=local", nil)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := rig.d.Enumerations(); got != 1 {
		t.Fatalf("%d concurrent cold reads enumerated %d times, want 1", readers, got)
	}
	for i, r := range replies {
		if r.code != 200 || r.etag() != replies[0].etag() || len(r.body) == 0 {
			t.Fatalf("reader %d: %d %q", i, r.code, r.etag())
		}
	}
}

func TestListSnapshot_HealthReportsTheCounters(t *testing.T) {
	rig := newListRig(t, 3)
	listGet(t, rig.srv, "?scope=local", nil)
	listGet(t, rig.srv, "?scope=local", nil)
	req := authedRequest(t, http.MethodGet, rig.srv.URL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		ListSnapshot map[string]int64 `json:"listSnapshot"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ListSnapshot["built"] != 1 || body.ListSnapshot["served"] != 1 {
		t.Fatalf("listSnapshot = %v, want one build and one served read", body.ListSnapshot)
	}
}

// ---------------------------------------------------------- the race oracle

// A read that begins after a change has COMPLETED never sees a listing older
// than that change. "Completed" is the point where the hub has stamped the
// event for it; the reader notes the newest completed version before it sends,
// and the version in what it gets back may not be smaller. Run under -race.
func TestListSnapshot_NoReadOlderThanACompletedChange(t *testing.T) {
	rig := newListRig(t, 1)
	row := func(version int) []fleet.Session {
		s := syntheticFleet(1)
		s[0].Cwd = fleet.AbsolutePath(fmt.Sprintf("/work/version/%d", version))
		return s
	}
	rig.d.Replace(row(0))
	versionOf := func(r listReply) int {
		items := r.collection(t).Items()
		if len(items) != 1 {
			t.Errorf("%d items", len(items))
			return -1
		}
		var v int
		if _, err := fmt.Sscanf(string(items[0].Cwd), "/work/version/%d", &v); err != nil {
			t.Errorf("cwd %q: %v", items[0].Cwd, err)
		}
		return v
	}

	var completed atomic.Int64
	var reads atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				want := int(completed.Load())
				got := versionOf(listGet(t, rig.srv, "?scope=local", nil))
				reads.Add(1)
				if got < want {
					t.Errorf("a read that began after version %d completed was served version %d", want, got)
					return
				}
			}
		}()
	}

	for v := 1; v <= 40; v++ {
		rig.d.Replace(row(v))
		rig.svc.events.publish(stateEventOn("testbox", fleet.EventSessionState, "syn-0000"))
		completed.Store(int64(v))
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if reads.Load() <= int64(rig.d.Enumerations()) {
		t.Fatalf("%d reads over %d enumerations: nothing was served from the snapshot, so the race was not exercised",
			reads.Load(), rig.d.Enumerations())
	}
}
