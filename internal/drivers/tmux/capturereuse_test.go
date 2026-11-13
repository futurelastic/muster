package tmux

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Tests for muster #449: a listing reads a pane's screen only when the pane's
// activity fingerprint moved (or the cached screen aged out), and serves the rest
// from the last capture without changing what it reports.

var reuseBase = time.Unix(1785760000, 0)

// reuseRig is a driver on a clock the test moves, over a fake multiplexer.
type reuseRig struct {
	t   *testing.T
	f   *fakeMux
	d   *Driver
	mu  sync.Mutex
	now time.Time
	// seen is how many of f's calls earlier listings already accounted for.
	seen int
	// beforeRead, when set, runs once, just before the multiplexer is asked for the
	// next capture: what happens to a pane between a listing and the read that
	// follows it.
	beforeRead func()
}

func newReuseRig(t *testing.T, f *fakeMux, opts ...Option) *reuseRig {
	t.Helper()
	r := &reuseRig{t: t, f: f, now: reuseBase}
	all := append([]Option{
		withExec(func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if slices.Contains(args, "capture-pane") {
				r.mu.Lock()
				hook := r.beforeRead
				r.beforeRead = nil
				r.mu.Unlock()
				if hook != nil {
					hook()
				}
			}
			return f.exec(ctx, name, args...)
		}),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }),
	}, opts...)
	r.d = New("testbox", all...)
	return r
}

func (r *reuseRig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// list takes one listing and returns the sessions by id, and the panes whose
// screen the multiplexer was asked to read during it.
func (r *reuseRig) list() (map[string]fleet.Session, []string) {
	r.t.Helper()
	col, err := r.d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		r.t.Fatalf("List: %v", err)
	}
	out := map[string]fleet.Session{}
	for _, s := range col.Items() {
		out[s.ID] = s
	}
	calls := r.f.callsSnapshot()
	read := panesRead(calls[r.seen:])
	r.seen = len(calls)
	return out, read
}

// panesRead is the pane ids named by the capture-pane commands in these calls.
func panesRead(calls [][]string) []string {
	var read []string
	for _, args := range calls {
		for i, a := range args {
			if a != "capture-pane" {
				continue
			}
			for j := i + 1; j < len(args)-1 && args[j] != ";"; j++ {
				if args[j] == "-t" {
					read = append(read, args[j+1])
					break
				}
			}
		}
	}
	return read
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// idleAndBusy is two sessions old enough to be past their start-up window: %1 has
// been silent for an hour, %2 printed a second ago.
func idleAndBusy() *fakeMux {
	at := reuseBase.Unix()
	return &fakeMux{
		sessions: []fakeSession{
			{name: "idle", paneID: "%1", cwd: "/work/idle", pid: 100, created: at - 160000, title: "2_1_220", activity: at - 3600},
			{name: "busy", paneID: "%2", cwd: "/work/busy", pid: 200, created: at - 160000, title: "2_1_220", activity: at - 1},
		},
		captures: map[string]string{
			"%1": idleFixtureFor("idle"),
			"%2": fixtureUnsent,
		},
	}
}

const (
	testIdleAfter = 2 * time.Minute
	testMaxAge    = 30 * time.Second
)

func TestCaptureReuse_AnIdlePaneIsReadOnceAndABusyOneEveryPass(t *testing.T) {
	r := newReuseRig(t, idleAndBusy(), WithCaptureReuse(testIdleAfter, testMaxAge))

	first, read := r.list()
	if !sameSet(read, []string{"%1", "%2"}) {
		t.Fatalf("the first listing read %v, want both panes: nothing is cached yet", read)
	}
	for i := 0; i < 4; i++ {
		r.advance(time.Second)
		got, read := r.list()
		if !sameSet(read, []string{"%2"}) {
			t.Fatalf("listing %d read %v, want only the pane that printed a second ago", i+2, read)
		}
		if a, b := first["idle"].State, got["idle"].State; a.Status != b.Status || a.ScreenDigest != b.ScreenDigest {
			t.Errorf("the idle session read differently from its cached screen: %q/%q vs %q/%q",
				a.Status, a.ScreenDigest, b.Status, b.ScreenDigest)
		}
	}
	if n := r.d.Counters()[counterCaptureReused]; n != 4 {
		t.Errorf("capture.reused = %d, want 4 (one idle pane, four listings that reused it)", n)
	}
}

// The ask's own test: a recorded idle-then-active pane is reported on the very next
// pass — no ladder step to wait out.
func TestCaptureReuse_APaneThatBecomesActiveIsReportedOnTheNextPass(t *testing.T) {
	r := newReuseRig(t, idleAndBusy(), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.list()
	r.advance(time.Second)
	before, read := r.list()
	if !sameSet(read, []string{"%2"}) {
		t.Fatalf("setup: the idle pane was read: %v", read)
	}

	r.advance(time.Second)
	r.f.mu.Lock()
	r.f.captures["%1"] = fixtureUnsent
	r.f.sessions[0].activity = r.now.Unix() // it printed just now
	r.f.mu.Unlock()
	after, read := r.list()
	if !sameSet(read, []string{"%1", "%2"}) {
		t.Fatalf("the pane that printed was read? %v, want both", read)
	}
	if a, b := before["idle"].State, after["idle"].State; a.ScreenDigest == b.ScreenDigest || a.Status == b.Status {
		t.Errorf("the new screen was not reported on the next pass: %q/%q -> %q/%q",
			a.Status, a.ScreenDigest, b.Status, b.ScreenDigest)
	}
}

func TestCaptureReuse_AnyFingerprintFieldMovingReadsThePaneAgain(t *testing.T) {
	cases := map[string]func(*fakeMux){
		"cursor column": func(f *fakeMux) { f.sessions[0].cursorX++ },
		"cursor row":    func(f *fakeMux) { f.sessions[0].cursorY++ },
		"scrollback":    func(f *fakeMux) { f.sessions[0].history++ },
		"pane height":   func(f *fakeMux) { f.heights = map[string]int{"%1": 30} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := newReuseRig(t, idleAndBusy(), WithCaptureReuse(testIdleAfter, testMaxAge))
			r.list()
			r.advance(time.Second)
			r.list()
			r.advance(time.Second)
			r.f.mu.Lock()
			change(r.f)
			r.f.mu.Unlock()
			if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
				t.Fatalf("read %v after the %s moved, want the idle pane read again", read, name)
			}
			r.advance(time.Second)
			if _, read := r.list(); !sameSet(read, []string{"%2"}) {
				t.Fatalf("read %v once it held still again, want it reused", read)
			}
		})
	}
}

func TestCaptureReuse_ACapturedScreenIsReadAgainAtTheCeiling(t *testing.T) {
	r := newReuseRig(t, idleAndBusy(), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.list()
	r.advance(testMaxAge/2 - time.Second)
	if _, read := r.list(); sameSet(read, []string{"%1", "%2"}) {
		t.Fatalf("the idle pane was read again before half the ceiling had passed: %v", read)
	}
	r.advance(testMaxAge)
	if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
		t.Fatalf("read %v past the ceiling, want the idle pane read again", read)
	}
}

func TestCaptureReuse_TheCeilingIsSpreadAcrossPanesAndNeverExceedsTheSetting(t *testing.T) {
	c := &captureReuse{idleAfter: testIdleAfter, maxAge: testMaxAge}
	distinct := map[time.Duration]bool{}
	for i := 0; i < 70; i++ {
		got := c.ceilingFor("%" + intToStr(i))
		if got > testMaxAge || got < testMaxAge/2 {
			t.Fatalf("ceiling %v is outside [%v, %v]", got, testMaxAge/2, testMaxAge)
		}
		if again := c.ceilingFor("%" + intToStr(i)); again != got {
			t.Fatalf("the same pane got two ceilings: %v then %v", got, again)
		}
		distinct[got] = true
	}
	if len(distinct) < 20 {
		t.Errorf("70 panes share only %d distinct ceilings; a fleet cached together would be read together", len(distinct))
	}
}

func TestCaptureReuse_InputSentToAPaneDropsItsCachedScreen(t *testing.T) {
	r := newReuseRig(t, idleAndBusy(), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.list()
	r.advance(time.Second)
	if _, read := r.list(); !sameSet(read, []string{"%2"}) {
		t.Fatalf("setup: the idle pane was read: %v", read)
	}
	// Through the driver's own runner, as every verb sends input.
	if _, err := r.d.run(context.Background(), r.d.bin, "send-keys", "-t", "%1", "Up"); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Second)
	if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
		t.Fatalf("read %v after input to %%1, want it read again", read)
	}
}

func TestCaptureReuse_NoteInputReadsTheTargetOfEveryInputCommand(t *testing.T) {
	entries := func() *captureReuse {
		return &captureReuse{entries: map[string]cachedCapture{"%1": {}, "%2": {}, "%3": {}}}
	}
	cases := []struct {
		name string
		args []string
		want []string // panes still cached
	}{
		{"send-keys", []string{"send-keys", "-t", "%1", "Up"}, []string{"%2", "%3"}},
		{"a chained load-buffer then paste-buffer",
			[]string{"load-buffer", "-b", "b", "/tmp/f", ";", "paste-buffer", "-p", "-b", "b", "-t", "%3", "-d"}, []string{"%1", "%2"}},
		{"a read is not input", []string{"capture-pane", "-p", "-e", "-t", "%1", "-S", "-24"}, []string{"%1", "%2", "%3"}},
		{"a target that names a session drops everything", []string{"send-keys", "-t", "=alpha", "Up"}, nil},
		{"a command with no target", []string{"list-panes", "-a"}, []string{"%1", "%2", "%3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := entries()
			r.noteInput(c.args)
			var left []string
			for id := range r.entries {
				left = append(left, id)
			}
			if !sameSet(left, c.want) {
				t.Errorf("still cached %v, want %v", left, c.want)
			}
		})
	}
}

func TestCaptureReuse_AStartingPaneIsAlwaysRead(t *testing.T) {
	f := idleAndBusy()
	f.sessions[0].created = reuseBase.Unix() - 10 // inside startingWindow
	r := newReuseRig(t, f, WithCaptureReuse(testIdleAfter, testMaxAge))
	for i := 0; i < 3; i++ {
		if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
			t.Fatalf("listing %d read %v, want the starting pane read every pass", i+1, read)
		}
		r.advance(time.Second)
	}
}

// The activity second has one-second resolution: a capture taken in the same second
// as the last output cannot prove nothing came after it, so it is not trusted until
// a listing taken in a later second has seen the pane still.
func TestCaptureReuse_ASecondOfOutputIsNotTrustedInThatSecond(t *testing.T) {
	f := idleAndBusy()
	f.sessions[0].activity = reuseBase.Unix()
	f.sessions[1].activity = reuseBase.Unix()
	r := newReuseRig(t, f, WithCaptureReuse(time.Millisecond, testMaxAge))
	for i := 0; i < 2; i++ {
		if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
			t.Fatalf("listing %d in the second of the last output read %v, want every pane", i+1, read)
		}
	}
	r.advance(2 * time.Second)
	if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
		t.Fatalf("the first listing after that second read %v: it has to read once more to hold a screen it can trust", read)
	}
	if _, read := r.list(); len(read) != 0 {
		t.Fatalf("with both panes unchanged since a past second the listing read %v, want nothing", read)
	}
}

func TestCaptureReuse_APaneTheMultiplexerGivesNoActivityForIsAlwaysRead(t *testing.T) {
	f := idleAndBusy()
	f.sessions[0].activity = 0
	r := newReuseRig(t, f, WithCaptureReuse(testIdleAfter, testMaxAge))
	for i := 0; i < 3; i++ {
		if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
			t.Fatalf("listing %d read %v, want the pane with no signal read every pass", i+1, read)
		}
		r.advance(time.Second)
	}
}

func TestCaptureReuse_OffByDefaultAndTheListingIsUnchanged(t *testing.T) {
	r := newReuseRig(t, idleAndBusy())
	for i := 0; i < 3; i++ {
		if _, read := r.list(); !sameSet(read, []string{"%1", "%2"}) {
			t.Fatalf("listing %d read %v without the option, want every pane every pass", i+1, read)
		}
		r.advance(time.Second)
	}
	for _, args := range r.f.callsSnapshot() {
		if len(args) > 0 && args[0] == "list-panes" && strings.Contains(strings.Join(args, " "), "window_activity") {
			t.Fatalf("a driver that does not reuse asked for the fingerprint: %v", args)
		}
	}
	if _, ok := r.d.Counters()[counterCaptureReused]; ok {
		t.Error("capture.reused is counted by a driver that never reuses")
	}
}

// A verb that acts on a screen reads it fresh whatever the cache holds.
func TestCaptureReuse_AVerbThatActsReadsTheScreenFresh(t *testing.T) {
	r := newReuseRig(t, idleAndBusy(), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.list()
	r.advance(time.Second)
	if _, read := r.list(); !sameSet(read, []string{"%2"}) {
		t.Fatalf("setup: the idle pane was read: %v", read)
	}
	if _, err := r.d.State(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "idle", Name: "idle"}); err != nil {
		t.Fatal(err)
	}
	read := panesRead(r.f.callsSnapshot()[r.seen:])
	if !sameSet(read, []string{"%1", "%2"}) && !sameSet(read, []string{"%1"}) {
		t.Fatalf("State read %v, want the idle pane's screen read fresh", read)
	}
}

// idleFleet is n sessions, every one silent for an hour and long past its start-up
// window: %1 .. %n.
func idleFleet(n int) *fakeMux {
	at := reuseBase.Unix()
	f := &fakeMux{captures: map[string]string{}}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("idle%d", i)
		id := fmt.Sprintf("%%%d", i)
		f.sessions = append(f.sessions, fakeSession{name: name, paneID: id, cwd: "/work/" + name,
			pid: 100 + i, created: at - 160000, title: "2_1_220", activity: at - 3600})
		f.captures[id] = idleFixtureFor(name)
	}
	return f
}

// settle takes the two listings after which an idle fleet is fully cached, then
// moves the clock past every pane's ceiling so the next listing re-reads them all.
func (r *reuseRig) settle() {
	r.t.Helper()
	r.list()
	r.advance(time.Second)
	if _, read := r.list(); len(read) != 0 {
		r.t.Fatalf("a settled idle fleet was read again: %v", read)
	}
	r.advance(testMaxAge + time.Second)
}

func (r *reuseRig) screenChanges(pane string) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.f.captures[pane] = fixtureUnsent
}

// Output that arrives between a listing and the read that follows it makes the
// screen differ under a fingerprint that has not moved yet. That is a race inside
// the check; the next listing shows the fingerprint moved. It must not cost the
// saving (muster #472: it did, for the life of the process).
func TestCaptureReuse_OutputBetweenTheListingAndTheReadIsARaceNotAMiss(t *testing.T) {
	logs := captureLog(t)
	r := newReuseRig(t, idleFleet(4), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.settle()

	r.mu.Lock()
	r.beforeRead = func() {
		r.screenChanges("%1")
		r.f.mu.Lock()
		r.f.sessions[0].activity = r.now.Unix()
		r.f.mu.Unlock()
	}
	r.mu.Unlock()
	r.list() // every pane's ceiling has passed, so all are read; %1 changed under the read

	r.advance(time.Second)
	got, read := r.list()
	if !slices.Contains(read, "%1") {
		t.Fatalf("the pane whose fingerprint moved was not read: %v", read)
	}
	if got["idle1"].State.Status == fleet.StatusIdle {
		t.Errorf("the changed screen still reports as the old one: %q", got["idle1"].State.Status)
	}
	if !r.d.reuse.enabled() {
		t.Fatal("reuse was switched off by output that raced the listing")
	}
	c := r.d.Counters()
	if c[counterCaptureReuseMissed] != 0 || c[counterCaptureReuseRaced] != 1 {
		t.Fatalf("reuse_missed=%d reuse_raced=%d, want 0 and 1", c[counterCaptureReuseMissed], c[counterCaptureReuseRaced])
	}
	if len(r.d.reuse.distrusted) != 0 {
		t.Fatalf("a raced pane was distrusted: %v", r.d.reuse.distrusted)
	}
	if strings.Contains(logs.String(), "cannot be trusted") {
		t.Errorf("the log says the signal cannot be trusted:\n%s", logs)
	}
}

// A change that no listing explains is a miss, and it costs the pane that showed
// it: that one is read on every pass, the others keep their saving.
func TestCaptureReuse_AChangeNoListingExplainsDistrustsThatPaneOnly(t *testing.T) {
	logs := captureLog(t)
	r := newReuseRig(t, idleFleet(5), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.settle()

	r.screenChanges("%1") // the fingerprint does not move
	r.list()              // past every ceiling: all read, %1 differs, suspected
	if n := r.d.Counters()[counterCaptureReuseMissed]; n != 0 {
		t.Fatalf("capture.reuse_missed = %d after one listing, want 0: the verdict waits for the next", n)
	}

	r.advance(time.Second)
	_, read := r.list() // the fingerprint is still the cached one: confirmed
	if !sameSet(read, []string{"%1"}) {
		t.Fatalf("the listing that confirmed the miss read %v, want only the pane that missed", read)
	}
	if n := r.d.Counters()[counterCaptureReuseMissed]; n != 1 {
		t.Fatalf("capture.reuse_missed = %d, want 1", n)
	}
	if !r.d.reuse.enabled() {
		t.Fatal("one pane shown to miss switched reuse off for the whole host")
	}
	for i := 0; i < 3; i++ {
		r.advance(time.Second)
		if _, read := r.list(); !sameSet(read, []string{"%1"}) {
			t.Fatalf("listing %d after the miss read %v, want %%1 on every pass and nothing else", i+1, read)
		}
	}
	if !strings.Contains(logs.String(), "pane %1 changed although") {
		t.Errorf("the log does not name the pane:\n%s", logs)
	}
}

// Several distinct panes disagreeing with their fingerprints is what a
// multiplexer that does not report output looks like: that switches reuse off.
func TestCaptureReuse_SeveralPanesShownToMissSwitchReuseOffForTheHost(t *testing.T) {
	logs := captureLog(t)
	r := newReuseRig(t, idleFleet(5), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.settle()

	for _, p := range []string{"%1", "%2", "%3"} {
		r.screenChanges(p)
	}
	r.list()
	r.advance(time.Second)
	r.list()
	if r.d.reuse.enabled() {
		t.Fatalf("reuse is still on after %d panes were shown to miss", reuseDistrustLimit)
	}
	if n := r.d.Counters()[counterCaptureReuseMissed]; n != 3 {
		t.Fatalf("capture.reuse_missed = %d, want 3", n)
	}
	r.advance(time.Second)
	if _, read := r.list(); len(read) != 5 {
		t.Fatalf("with reuse off a listing read %v, want every pane", read)
	}
	if !strings.Contains(logs.String(), "cannot be trusted here") {
		t.Errorf("the host-wide switch-off was not logged:\n%s", logs)
	}
}

// A suspect whose pane goes away before the next listing is dropped unjudged.
func TestCaptureReuse_ASuspectWhosePaneClosedIsDropped(t *testing.T) {
	r := newReuseRig(t, idleFleet(3), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.settle()
	r.screenChanges("%1")
	r.list()
	r.f.mu.Lock()
	r.f.sessions = r.f.sessions[1:]
	r.f.mu.Unlock()
	r.advance(time.Second)
	r.list()
	if n := r.d.Counters()[counterCaptureReuseMissed]; n != 0 {
		t.Fatalf("capture.reuse_missed = %d for a pane that no longer exists", n)
	}
	r.d.reuse.mu.Lock()
	left := len(r.d.reuse.suspects) + len(r.d.reuse.distrusted)
	r.d.reuse.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d suspect/distrust records outlived their pane", left)
	}
}

func TestCaptureReuse_AClosedSessionIsForgotten(t *testing.T) {
	r := newReuseRig(t, idleAndBusy(), WithCaptureReuse(testIdleAfter, testMaxAge))
	r.list()
	r.f.mu.Lock()
	r.f.sessions = r.f.sessions[1:]
	r.f.mu.Unlock()
	r.advance(time.Second)
	r.list()
	r.d.reuse.mu.Lock()
	_, kept := r.d.reuse.entries["%1"]
	r.d.reuse.mu.Unlock()
	if kept {
		t.Error("the cache still holds a screen for a pane the listing no longer has")
	}
}

// Reuse must be invisible in what a listing reports: the same script of screens and
// activity, read with and without it, gives the same state at every step.
func TestCaptureReuse_ReportsExactlyWhatReadingEveryPaneReports(t *testing.T) {
	plain := newReuseRig(t, idleAndBusy())
	reusing := newReuseRig(t, idleAndBusy(), WithCaptureReuse(time.Millisecond, testMaxAge))

	both := func(change func(*fakeMux, time.Time)) {
		for _, r := range []*reuseRig{plain, reusing} {
			r.f.mu.Lock()
			change(r.f, r.now)
			r.f.mu.Unlock()
		}
	}
	type step struct {
		name   string
		wait   time.Duration
		change func(*fakeMux, time.Time)
	}
	steps := []step{
		{"first read", 0, nil},
		{"nothing moves", time.Second, nil},
		{"nothing moves for a while", 20 * time.Second, nil},
		{"the idle pane gets input waiting", time.Second, func(f *fakeMux, now time.Time) {
			f.captures["%1"] = fixtureUnsent
			f.sessions[0].activity = now.Unix()
		}},
		{"it holds still", 3 * time.Second, nil},
		{"it holds still longer", time.Minute, nil},
		{"it clears", time.Second, func(f *fakeMux, now time.Time) {
			f.captures["%1"] = idleFixtureFor("cleared")
			f.sessions[0].activity = now.Unix()
			f.sessions[0].cursorY = 3
		}},
		{"and holds still", 5 * time.Second, nil},
		{"the busy pane changes", time.Second, func(f *fakeMux, now time.Time) {
			f.captures["%2"] = idleFixtureFor("busy done")
			f.sessions[1].activity = now.Unix()
		}},
		{"and holds still", 40 * time.Second, nil},
	}
	for _, s := range steps {
		plain.advance(s.wait)
		reusing.advance(s.wait)
		if s.change != nil {
			both(s.change)
		}
		a, _ := plain.list()
		b, _ := reusing.list()
		for id, want := range a {
			got, ok := b[id]
			if !ok {
				t.Fatalf("%s: %s is listed without reuse and missing with it", s.name, id)
			}
			if !reflect.DeepEqual(want.State, got.State) {
				t.Fatalf("%s: %s reads differently with reuse:\n without: %+v\n with:    %+v", s.name, id, want.State, got.State)
			}
		}
	}
	if n := reusing.d.Counters()[counterCaptureReused]; n == 0 {
		t.Error("the script never reused a screen; the comparison proved nothing")
	}
}

// The measured shape of the problem: a machine holding about seventy sessions of
// which only two are doing anything. After the first listing a pass asks the
// multiplexer to read those two, and the cost of the pass no longer grows with the
// idle share.
func TestCaptureReuse_AMostlyIdleFleetCostsOnlyItsActivePanes(t *testing.T) {
	f := idleAndBusy()
	at := reuseBase.Unix()
	for i := 0; i < 68; i++ {
		f.sessions = append(f.sessions, fakeSession{
			name: "idle" + intToStr(i), paneID: "%" + intToStr(100+i), cwd: "/work/x", pid: 1000 + i,
			created: at - 160000, title: "2_1_220", activity: at - 90000,
		})
		f.captures["%"+intToStr(100+i)] = idleFixtureFor("idle" + intToStr(i))
	}
	r := newReuseRig(t, f, WithCaptureReuse(testIdleAfter, testMaxAge))
	if _, read := r.list(); len(read) != 70 {
		t.Fatalf("the first listing read %d panes, want all 70", len(read))
	}
	for i := 0; i < 5; i++ {
		r.advance(time.Second)
		_, read := r.list()
		if !sameSet(read, []string{"%2"}) {
			t.Fatalf("listing %d read %d panes %v, want only the one that printed a second before the first pass", i+2, len(read), read)
		}
	}
	if n := r.d.Counters()[counterCaptureReused]; n != 5*69 {
		t.Errorf("capture.reused = %d, want %d", n, 5*69)
	}
}
