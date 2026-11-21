package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

func listAll() driver.ListFilter { return driver.ListFilter{} }

func statMod(t *testing.T, dir string) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, sessionsFileName+".json"))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// §12 requires three classifications. The previous implementation could
// produce exactly one, because with no records everything is orphaned by
// definition — it looked implemented and could not do its job.
func TestReconcileDistinguishesAdoptedOrphanedAndVanished(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()

	// First run: nothing is remembered, so everything found is orphaned.
	first := stateDriver(t, f, dir)
	got, err := first.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orphaned) != 2 || len(got.Adopted) != 0 || len(got.Vanished) != 0 {
		t.Fatalf("first run: %s — everything should be orphaned on a cold start", got)
	}
	for _, s := range got.Orphaned {
		if s.State.Evidence == "" {
			t.Error("§12 requires orphans surfaced with identifying evidence")
		}
	}

	// Second run, same sessions: now they are remembered.
	second := stateDriver(t, f, dir)
	got, err = second.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Adopted) != 2 || len(got.Orphaned) != 0 {
		t.Fatalf("second run: %s — remembered sessions should be adopted", got)
	}

	// One disappears while nothing is watching.
	f.dropLastSession()
	third := stateDriver(t, f, dir)
	got, err = third.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Vanished) != 1 {
		t.Fatalf("third run: %s — a remembered session that is gone must be vanished", got)
	}
	v := got.Vanished[0]
	if v.State.Status != fleet.StatusDead {
		t.Errorf("vanished session reported %q, want dead", v.State.Status)
	}
	if v.State.Evidence == "" {
		t.Error("§12 wants evidence that it disappeared while unobserved")
	}
}

// A recycled id is two facts, not one. Reporting only "adopted" would
// attribute one session's history to another (§5.4).
func TestRecycledIdIsBothVanishedAndOrphaned(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()

	if _, err := stateDriver(t, f, dir).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Same name, different start time: a new session wearing an old name.
	f.mu.Lock()
	f.sessions[0].created = 1785699999
	f.mu.Unlock()

	got, err := stateDriver(t, f, dir).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var vanished, orphaned bool
	for _, s := range got.Vanished {
		if s.ID == "alpha💬" {
			vanished = true
		}
	}
	for _, s := range got.Orphaned {
		if s.ID == "alpha💬" {
			orphaned = true
		}
	}
	if !vanished || !orphaned {
		t.Errorf("recycled id produced vanished=%v orphaned=%v; both are true and "+
			"reporting one hides the other", vanished, orphaned)
	}
}

// §12 rule 4 is absolute. Reconciliation must never destroy, and the safest
// guarantee is that the code has no way to.
func TestReconcileDestroysNothing(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)
	if _, err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("reconciliation destroyed a session — §12 rule 4 is absolute")
		}
	}
}

// Records are written when the set changes, not on every read: a read happens
// on every event trigger, and persisting each would be a write amplifier.
func TestRecordsAreWrittenOnChangeNotOnEveryRead(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)
	ctx := context.Background()

	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}
	before := statMod(t, dir)
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, err := d.List(ctx, testCaller, listAll()); err != nil {
			t.Fatal(err)
		}
	}
	if after := statMod(t, dir); !after.Equal(before) {
		t.Error("an unchanged session set rewrote the records file on every read")
	}

	f.addSession(fakeSession{name: "delta", paneID: "%40", cwd: "/w", pid: 9, created: 1785600099},
		idleFixtureFor("delta"))
	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}
	if after := statMod(t, dir); after.Equal(before) {
		t.Error("a changed session set did not update the records file")
	}
}

// `since` is the discriminator this service added so nobody has to probe a
// pane to tell "somebody is mid-thought" from "nobody is coming back" (F34).
// It resets on restart unless the value survives — and restarts are how this
// service is deployed, so a 14-hour stall silently reads as minutes old.
func TestSinceSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	f := twoSessions()
	f.captures["%2"] = fixtureUnsent // beta holds unsent operator text

	// A service instance sees it, some time ago.
	early := time.Unix(1785600100, 0)
	d1 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return early }))
	if _, err := d1.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	// A NEW instance — same store, no in-memory history — reads it much later.
	late := early.Add(14 * time.Hour)
	d2 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return late }))
	col, err := d2.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, s := range col.Items() {
		if s.ID != "beta" {
			continue
		}
		found = true
		if s.State.Since == nil {
			t.Fatal("no since at all")
		}
		age := late.Sub(*s.State.Since)
		if age < 13*time.Hour {
			t.Errorf("age = %s, want ~14h — the restart reset it, which is the bug", age)
		}
		// §5.2: an age this instance did not measure must not pass as one it did.
		if !strings.Contains(s.State.Evidence, "restart") {
			t.Errorf("evidence does not say the age is second-hand: %q", s.State.Evidence)
		}
	}
	if !found {
		t.Fatal("beta missing from the listing")
	}
}

// A rename must not reset what a restart reads as `since` (muster
// #96/#97). The old implementation kept the durable session record keyed
// ONLY by name, so the moment a rename changed the key, the record's own
// history — FirstSeen, Status, StatusSince — had nothing to attach to and a
// fresh one was minted instead: the exact §8 defect
// TestSinceSurvivesARestart guards against, reached through a rename
// instead of a restart.
func TestRenamePreservesPersistedSince(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	f := twoSessions()
	f.captures["%2"] = fixtureUnsent // beta holds unsent operator text

	early := time.Unix(1785600100, 0)
	d1 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return early }))
	if _, err := d1.List(context.Background(), testCaller, listAll()); err != nil {
		t.Fatal(err)
	}

	req := testCaller
	started := time.Unix(1785600001, 0) // beta's own created time, from twoSessions
	req.Expect.StartedAt = &started
	if _, err := d1.Rename(context.Background(), req,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, "beta-x"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// A NEW instance — same store, no in-memory history — reconciles much
	// later, exactly TestSinceSurvivesARestart's own shape.
	late := early.Add(14 * time.Hour)
	d2 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return late }))
	got, err := d2.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range got.Orphaned {
		if s.ID == "beta-x" {
			t.Fatal("beta-x was orphaned rather than adopted — the rename broke reconciliation's own match against its persisted record")
		}
	}
	var found bool
	for _, s := range got.Adopted {
		if s.ID != "beta-x" {
			continue
		}
		found = true
		if s.State.Since == nil {
			t.Fatal("no since at all")
		}
		age := late.Sub(*s.State.Since)
		if age < 13*time.Hour {
			t.Errorf("age = %s, want ~14h — the rename reset it, which is the bug", age)
		}
	}
	if !found {
		t.Fatal("beta-x missing from Adopted")
	}
}

// The provenance must survive being cached, not just the first read. It was
// computed correctly and then dropped on the floor: the observation did not
// carry it, so every read after the first presented a second-hand age as one
// this instance had measured — §5.2's exact prohibition, reintroduced by the
// fix for it.
func TestCarriedAgeStaysMarkedOnLaterReads(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent

	early := time.Unix(1785600100, 0)
	d1 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return early }))
	if _, err := d1.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	late := early.Add(3 * time.Hour)
	d2 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return late }))

	for read := 1; read <= 3; read++ {
		col, err := d2.List(context.Background(), testCaller, driver.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range col.Items() {
			if s.ID != "beta" {
				continue
			}
			if !strings.Contains(s.State.Evidence, "restart") {
				t.Fatalf("read %d lost the provenance: %q", read, s.State.Evidence)
			}
		}
	}
}

// flakyListPanes wraps a fake multiplexer so that its Nth list-panes call and
// every one after it fails, while everything else answers normally. Reconcile
// reads the multiplexer in two separate steps — the listing, then the start
// times — so failing from the second read on is exactly "the listing worked
// and the start-time read did not" (muster #321).
func flakyListPanes(f *fakeMux, failFrom int32) (execFunc, *atomic.Int32) {
	var n atomic.Int32
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list-panes" {
			if n.Add(1) >= failFrom {
				return nil, errors.New("multiplexer read timed out")
			}
		}
		return f.exec(ctx, name, args...)
	}, &n
}

// muster #321: a failed start-time read must not be read as "every session
// changed identity". Before the fix, the failure was dropped, every start
// compared as the zero time, and every remembered session was reported as a
// recycled id — vanished and re-listed as an orphan, in one pass.
func TestReconcileFailedStartTimeReadJudgesNothing(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()

	// A first, healthy run remembers both sessions.
	if _, err := stateDriver(t, f, dir).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := (&Driver{store: st}).loadRecords()
	if len(before) != 2 {
		t.Fatalf("setup: %d records remembered, want 2", len(before))
	}

	exec, reads := flakyListPanes(f, 2)
	d := New("testbox", withExec(exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithState(st))
	got, err := d.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() < 3 {
		t.Fatalf("list-panes ran %d times; the failed read should have been retried once", reads.Load())
	}
	if len(got.Vanished) != 0 || len(got.Orphaned) != 0 {
		t.Fatalf("%s — a read that failed must not turn remembered sessions into vanished/orphaned", got)
	}
	if len(got.Adopted) != 2 || got.Unjudged != 2 {
		t.Fatalf("%s — both sessions stay remembered and are reported as not judged", got)
	}
	if got.ReadErr == "" || !strings.Contains(got.String(), "not judged") {
		t.Errorf("the failure was swallowed: ReadErr=%q String=%q", got.ReadErr, got.String())
	}

	// The records themselves are untouched, so the next pass judges against
	// what was really remembered rather than against a failure.
	after := (&Driver{store: st}).loadRecords()
	for id, rec := range before {
		if !after[id].Created.Equal(rec.Created) || after[id].FirstSeen != rec.FirstSeen {
			t.Errorf("record %q changed across a failed read: %+v -> %+v", id, rec, after[id])
		}
	}
}

// The listing itself failing is the same hazard one read earlier: List reports
// an unreachable source and returns no error, an empty list, and everything
// remembered would be "absent now".
func TestReconcileUnreachableListingJudgesNothing(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	if _, err := stateDriver(t, f, dir).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := (&Driver{store: st}).loadRecords()

	exec, _ := flakyListPanes(f, 1)
	d := New("testbox", withExec(exec), withNonce(func() string { return testNonce }), WithState(st))
	got, err := d.Reconcile(context.Background())
	if err == nil {
		t.Fatalf("%s — an unreadable listing must be an error, not an empty fleet", got)
	}
	if len(got.Vanished) != 0 {
		t.Errorf("an unreadable listing vanished %d sessions", len(got.Vanished))
	}
	after := (&Driver{store: st}).loadRecords()
	if len(after) != len(before) {
		t.Errorf("remembered set went from %d to %d records on a failed read", len(before), len(after))
	}
}

// muster #508: a reconcile that holds the port closed for a minute used to say
// only that it had finished. The line now says how long it took and where the
// time went, the listing's own steps among them.
func TestReconcileReportsTookAndPhases(t *testing.T) {
	d := stateDriver(t, twoSessions(), t.TempDir())
	got, err := d.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	line := got.String()
	for _, want := range []string{
		" took=", "phase.records=", "phase.list.enumerate=", "phase.list.classify=",
		"phase.list=", "phase.starttimes=", "phase.classify=", "phase.save=",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("reconcile line lacks %q: %s", want, line)
		}
	}
	// The steps are named in the order they finished, the listing's own before
	// the listing as a whole, so the line reads as a timeline.
	order := []string{"phase.records=", "phase.list.enumerate=", "phase.list=", "phase.starttimes=", "phase.classify=", "phase.save="}
	last := -1
	for _, name := range order {
		at := strings.Index(line, name)
		if at < last {
			t.Fatalf("%q is out of order in: %s", name, line)
		}
		last = at
	}
	if got.Took <= 0 {
		t.Errorf("Took = %v, want a measured duration", got.Took)
	}
}

// A reconcile that carries no phases of its own to a caller leaves ordinary
// reads exactly as they were: a List with no recorder on its context notes
// nothing and returns what it always did.
func TestListWithoutARecorderIsUnchanged(t *testing.T) {
	d := stateDriver(t, twoSessions(), t.TempDir())
	got, err := d.List(context.Background(), fleet.SystemRequest(), listAll())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Items()); n != 2 {
		t.Fatalf("List returned %d sessions, want 2", n)
	}
}

// threeSessions is the live set a restart finds when beta ended while the
// service was down and gamma started: alpha stays, beta is gone, gamma is new.
func alphaAndGamma() *fakeMux {
	return &fakeMux{
		sessions: []fakeSession{
			{name: "alpha💬", paneID: "%1", cwd: "/work/alpha", pid: 100, created: 1785600000, title: "2_1_220"},
			{name: "gamma", paneID: "%3", cwd: "/work/gamma", pid: 300, created: 1785600002, title: "2_1_220"},
		},
		captures: map[string]string{
			"%1": idleFixtureFor("alpha"),
			"%3": fixtureUnsent,
		},
	}
}

// remembered runs one healthy reconcile on a directory, so the records hold
// alpha and beta, and returns the directory.
func remembered(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := stateDriver(t, twoSessions(), dir).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return dir
}

func sessionsBytes(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, sessionsFileName+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// muster #508, and F30 once more: with the listeners open before the reconcile
// has finished, a client's read can land first. An ordinary read records the live
// set, so without a baseline taken beforehand the reconcile would compare the
// world against a record its own client had just written — every session adopted,
// beta never reported vanished, gamma never reported orphaned.
func TestReconcileBaselineTakenBeforeAReadSurvivesAReadWhilePending(t *testing.T) {
	dir := remembered(t)

	d := stateDriver(t, alphaAndGamma(), dir)
	d.BeginReconcile()
	// The client's read, served while the reconcile is still pending.
	if _, err := d.List(context.Background(), fleet.SystemRequest(), listAll()); err != nil {
		t.Fatal(err)
	}
	got, err := d.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Vanished) != 1 || got.Vanished[0].ID != "beta" {
		t.Errorf("vanished = %v, want beta alone: %s", got.Vanished, got)
	}
	if len(got.Orphaned) != 1 || got.Orphaned[0].ID != "gamma" {
		t.Errorf("orphaned = %v, want gamma alone: %s", got.Orphaned, got)
	}
	if len(got.Adopted) != 1 || got.Adopted[0].ID != "alpha💬" {
		t.Errorf("adopted = %v, want alpha alone: %s", got.Adopted, got)
	}

	// The control, to show the test can fail: the same sequence with no
	// baseline taken is the F30 failure, and the read has already written gamma.
	dir = remembered(t)
	c := stateDriver(t, alphaAndGamma(), dir)
	if _, err := c.List(context.Background(), fleet.SystemRequest(), listAll()); err != nil {
		t.Fatal(err)
	}
	lost, err := c.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lost.Orphaned) != 0 {
		t.Fatalf("control: a read before an unprotected reconcile no longer hides gamma (%s); this test no longer proves anything", lost)
	}
}

// While the reconcile is pending a listing is answered and writes nothing: the
// reconcile is the record store's one writer for the window, so its final save
// cannot lose, or be lost to, a write made by a read.
func TestListWhilePendingWritesNoRecords(t *testing.T) {
	dir := remembered(t)
	before := sessionsBytes(t, dir)
	modBefore := statMod(t, dir)

	d := stateDriver(t, alphaAndGamma(), dir)
	d.BeginReconcile()
	got, err := d.List(context.Background(), fleet.SystemRequest(), listAll())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Items()); n != 2 {
		t.Fatalf("the read was not answered: %d sessions, want 2", n)
	}
	if after := sessionsBytes(t, dir); string(after) != string(before) {
		t.Errorf("a listing wrote the record store while the reconcile was pending:\n%s\n->\n%s", before, after)
	}
	if !statMod(t, dir).Equal(modBefore) {
		t.Errorf("the record file was rewritten while the reconcile was pending")
	}
}

// The hold ends with the reconcile: reads record again, and the result of the
// reconcile is what the store then holds.
func TestReadsWriteRecordsAgainAfterReconcile(t *testing.T) {
	dir := remembered(t)
	d := stateDriver(t, alphaAndGamma(), dir)
	d.BeginReconcile()
	if _, err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.reconciling.Load() {
		t.Fatal("the hold outlived the reconcile")
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if recs := (&Driver{store: st}).loadRecords(); len(recs) != 2 || recs["gamma"].Cwd != "/work/gamma" || recs["beta"].Cwd != "" {
		t.Fatalf("the reconcile's result was not saved: %+v", recs)
	}

	// A session that appears after the reconcile is recorded by an ordinary read.
	f := alphaAndGamma()
	f.sessions = append(f.sessions, fakeSession{name: "delta", paneID: "%4", cwd: "/work/delta", pid: 400, created: 1785600003, title: "2_1_220"})
	f.captures["%4"] = fixtureUnsent
	d2 := stateDriver(t, f, dir)
	if _, err := d2.List(context.Background(), fleet.SystemRequest(), listAll()); err != nil {
		t.Fatal(err)
	}
	if recs := (&Driver{store: st}).loadRecords(); recs["delta"].Cwd != "/work/delta" {
		t.Errorf("a read after the reconcile did not record the new session: %+v", recs)
	}
}

// A reconcile that fails saved nothing and must not leave the store held: reads
// would never record again for the life of the process.
func TestReconcileErrorClearsPending(t *testing.T) {
	dir := remembered(t)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := sessionsBytes(t, dir)
	exec, _ := flakyListPanes(twoSessions(), 1)
	d := New("testbox", withExec(exec), withNonce(func() string { return testNonce }), WithState(st))
	d.BeginReconcile()
	if _, err := d.Reconcile(context.Background()); err == nil {
		t.Fatal("an unreadable listing must be an error")
	}
	if d.reconciling.Load() {
		t.Error("a failed reconcile left the record store held")
	}
	if after := sessionsBytes(t, dir); string(after) != string(before) {
		t.Error("a failed reconcile changed the remembered set")
	}
}

// The reconcile's own caller sets the bound. A listing that outlives a bound the
// caller put on the whole reconcile leaves the start-time read nothing to run in:
// it fails at once and every session reads as "not judged" — what a start on a
// machine with many sessions measured (muster #508). With no bound of its own on
// the context, each read takes the driver's, fresh.
func TestReconcileStartTimeReadNeedsABoundOfItsOwn(t *testing.T) {
	slow := func(f *fakeMux) execFunc {
		var n atomic.Int32
		return func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(args) > 0 && args[0] == "list-panes" && n.Add(1) == 1 {
				time.Sleep(150 * time.Millisecond) // a listing that ignores the caller's bound
			}
			return f.exec(ctx, name, args...)
		}
	}
	run := func(ctx context.Context) Reconciliation {
		dir := remembered(t)
		st, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		d := New("testbox", withExec(slow(twoSessions())), withNonce(func() string { return testNonce }), WithState(st))
		got, err := d.Reconcile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	bounded, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if got := run(bounded); got.Unjudged != 2 || !strings.Contains(got.ReadErr, "deadline exceeded") {
		t.Errorf("under a caller bound the listing outlives, the start-time read should have none left: %s", got)
	}
	if got := run(context.Background()); got.Unjudged != 0 || got.ReadErr != "" {
		t.Errorf("with no bound on the context the start-time read must judge identity: %s", got)
	}
}
