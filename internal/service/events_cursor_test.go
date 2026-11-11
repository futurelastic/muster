package service

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

// reservations is a stand-in for the durable write: it counts calls and keeps
// the largest ceiling made durable, which is all the properties below need.
type reservations struct {
	mu       sync.Mutex
	saves    int
	ceiling  int64
	ceilings []int64
	fail     bool
}

func (r *reservations) save(c int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves++
	if r.fail {
		return errors.New("disk full")
	}
	r.ceiling = c
	r.ceilings = append(r.ceilings, c)
	return nil
}

func (r *reservations) snapshot() (int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saves, r.ceiling
}

// #415: the cursor file was rewritten, with a full drive flush, once per
// published event. It is rewritten once per block now — counted, not assumed.
func TestCursorIsMadeDurableOncePerBlockNotOncePerEvent(t *testing.T) {
	h := newHub("testbox", "e")
	r := &reservations{}
	h.reserve, h.block = r.save, 8

	const events = 100
	for i := 0; i < events; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
	}
	saves, _ := r.snapshot()
	if want := (events + 7) / 8; saves != want {
		t.Fatalf("%d events made the cursor durable %d times, want %d (one per block of 8) — one per event is the 49 000 drive flushes a day this replaced", events, saves, want)
	}
}

// The property the whole scheme rests on: when a subscriber holds cursor c, a
// ceiling >= c is already on disk. Checked at every event, from the receiving
// side — the only place "handed out" is observable.
func TestDurableCeilingNeverTrailsAHandedOutCursor(t *testing.T) {
	h := newHub("testbox", "e")
	r := &reservations{}
	h.reserve, h.block = r.save, 5
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	for i := 0; i < 60; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
		ev := <-sub.ch
		if _, ceiling := r.snapshot(); ev.Cursor > ceiling {
			t.Fatalf("event %d was handed out with cursor %d while the durable ceiling was %d — a crash here would let the next run reuse %d", i, ev.Cursor, ceiling, ev.Cursor)
		}
	}
}

// Publishers racing at a block boundary make one write between them, and none
// of them stamps past what is durable. Run under -race with more publishers
// than a block holds, which is the case the retry loop in publish exists for.
func TestConcurrentPublishersAtABoundaryNeverOutrunTheCeiling(t *testing.T) {
	h := newHub("testbox", "e")
	r := &reservations{}
	h.reserve, h.block = r.save, 4
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	sub.ch = make(chan fleet.Event, 256) // the default 64 would mark this subscriber dropped

	const publishers = 50
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.publish(sessionEvent("a", fleet.StatusIdle))
		}()
	}
	wg.Wait()

	saves, ceiling := r.snapshot()
	if got := h.currentCursor(); got > ceiling {
		t.Fatalf("cursor reached %d with the durable ceiling at %d", got, ceiling)
	}
	if want := (publishers + 3) / 4; saves != want {
		t.Errorf("%d racing publishers made %d writes, want %d — a writer per publisher means the re-check under the lock is missing", publishers, saves, want)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 1; i < len(r.ceilings); i++ {
		if r.ceilings[i] <= r.ceilings[i-1] {
			t.Fatalf("ceilings went %v: a later write must reserve strictly more", r.ceilings)
		}
	}
}

// The flush is a disk operation. While one is in flight nothing that merely
// reads the hub may queue behind it — that queue was the other half of the
// cost, every publisher and every poll waiting on a flush for 65 bytes.
func TestTheDurableWriteIsNotTakenUnderTheHubLock(t *testing.T) {
	h := newHub("testbox", "e")
	entered, release := make(chan struct{}), make(chan struct{})
	h.reserve = func(int64) error {
		close(entered)
		<-release
		return nil
	}
	published := make(chan struct{})
	go func() {
		defer close(published)
		h.publish(sessionEvent("a", fleet.StatusIdle))
	}()
	<-entered

	read := make(chan struct{})
	go func() {
		h.currentCursor()
		h.subscriberCount()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the hub lock was held across the durable write: a reader waited on the flush")
	}
	close(release)
	<-published
}

// A disk that will not take the write must not stop the feed — the per-event
// write this replaced dropped its error, and a supervisor waiting on an event
// is worse off than one that gets it with a number that is not yet durable.
// The write is retried on the next event, not abandoned.
func TestFeedContinuesAndRetriesWhenTheCeilingCannotBeWritten(t *testing.T) {
	h := newHub("testbox", "e")
	r := &reservations{fail: true}
	h.reserve, h.block = r.save, 8
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	for i := 1; i <= 3; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
		if ev := <-sub.ch; ev.Cursor != int64(i) {
			t.Fatalf("event %d stamped %d while the disk was failing", i, ev.Cursor)
		}
	}
	if saves, _ := r.snapshot(); saves != 3 {
		t.Errorf("%d write attempts for 3 events while failing, want 3 (retry every event)", saves)
	}

	r.mu.Lock()
	r.fail = false
	r.mu.Unlock()
	h.publish(sessionEvent("a", fleet.StatusIdle))
	if _, ceiling := r.snapshot(); ceiling < h.currentCursor() {
		t.Errorf("once the disk recovered the ceiling is %d, below the cursor %d", ceiling, h.currentCursor())
	}
}

// §7.3 across a real restart, through the real store: the new run's first
// cursor is above every cursor the old run handed out, the epoch is the same,
// and a quiet service does not touch the file at all.
func TestRestartContinuesPastEveryCursorTheLastRunHandedOut(t *testing.T) {
	dir := t.TempDir()
	open := func() *Service {
		st, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := NewWithState("testbox", st)
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}

	first := open()
	sub, _, _ := first.events.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	var last int64
	for i := 0; i < 7; i++ {
		first.events.publish(sessionEvent("a", fleet.StatusIdle))
		last = (<-sub.ch).Cursor
	}

	second := open()
	if second.epoch != first.epoch {
		t.Fatalf("epoch changed across the restart: %q -> %q", first.epoch, second.epoch)
	}
	sub2, _, _ := second.events.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	second.events.publish(sessionEvent("a", fleet.StatusIdle))
	if got := (<-sub2.ch).Cursor; got <= last {
		t.Fatalf("restart handed out cursor %d after the previous run had handed out %d — numbers reused under one epoch (§7.3)", got, last)
	}
}

// A file an older build wrote holds the exact last cursor. Read as a ceiling it
// is still a number nothing exceeds, so the first event after upgrading
// continues from it and raises the file before anyone sees that event.
func TestAnExactCursorFromAnOlderBuildReadsAsACeiling(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.json"), []byte(`{"epoch":"old-epoch","cursor":4670000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewWithState("testbox", st)
	if err != nil {
		t.Fatal(err)
	}
	sub, _, _ := svc.events.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	svc.events.publish(sessionEvent("a", fleet.StatusIdle))
	ev := <-sub.ch
	if ev.Cursor != 4670001 || ev.Epoch != "old-epoch" {
		t.Fatalf("first event after upgrade = cursor %d epoch %q, want 4670001 under old-epoch", ev.Cursor, ev.Epoch)
	}
	var seq eventSequence
	if found, err := st.Load("events", &seq); err != nil || !found {
		t.Fatalf("events file unreadable: found=%v err=%v", found, err)
	}
	if seq.Cursor < ev.Cursor {
		t.Errorf("file holds %d, below the cursor %d just handed out", seq.Cursor, ev.Cursor)
	}
}

// openRestartable opens the service the way startup does, over one directory, so
// a test can "restart" by opening it again. Whatever the first run left in the
// directory is all the second one has — no ring, no subscribers.
func openRestartable(t *testing.T, dir string) *Service {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewWithState("testbox", st)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// publishN publishes n events through a subscriber and returns the last cursor
// that subscriber received — "handed out" is only observable on the receiving
// side.
func publishN(t *testing.T, svc *Service, n int) int64 {
	t.Helper()
	sub, _, _ := svc.events.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	var last int64
	for i := 0; i < n; i++ {
		svc.events.publish(sessionEvent("a", fleet.StatusIdle))
		last = (<-sub.ch).Cursor
	}
	return last
}

// #418 (a), at the hub: restored the way startup restores it — the cursor and the
// epoch kept, the ring empty. The window check used to read an empty ring as
// "holds everything" because the floor stayed 0, so a subscriber that was behind
// the cursor was resumed with an empty backlog instead of being told its events
// were gone. The same answer is now given before and after the first event
// arrives.
func TestAnEmptyWindowAfterARestoreAnnouncesAnyCursorBelowTheRestoredOne(t *testing.T) {
	restored := func() *hub {
		h := newHub("testbox", "e")
		h.cursor = 100
		return h
	}
	for _, c := range []struct {
		name   string
		cursor int64
		resync bool
	}{
		{"behind the restored cursor", 50, true},
		{"one behind", 99, true},
		{"zero, with the epoch", 0, true},
		{"exactly the restored cursor", 100, false},
		{"ahead of anything stamped", 101, true},
	} {
		h := restored()
		_, backlog, resync := h.add(ScopeFleet, driver.SubscribeFilter{}, c.cursor, "e")
		if resync != c.resync || len(backlog) != 0 {
			t.Errorf("%s: resync=%v backlog=%d, want resync=%v backlog=0", c.name, resync, len(backlog), c.resync)
		}

		// After the first event the ring is not empty and the older check applies;
		// it must agree with what the empty ring said.
		h.publish(sessionEvent("a", fleet.StatusIdle))
		_, _, after := h.add(ScopeFleet, driver.SubscribeFilter{}, c.cursor, "e")
		if c.cursor <= 100 && after != c.resync {
			t.Errorf("%s: resync=%v with an empty ring but %v once an event arrived — the answer must not depend on whether anything has happened yet", c.name, c.resync, after)
		}
	}

	// Not a restore: nothing stamped, nothing below the cursor, and a subscriber
	// with no history is not made to resync.
	fresh := newHub("testbox", "e")
	if _, _, resync := fresh.add(ScopeFleet, driver.SubscribeFilter{}, 0, "e"); resync {
		t.Error("a hub that has stamped nothing resynced a subscriber at cursor 0")
	}
	if _, _, resync := fresh.add(ScopeFleet, driver.SubscribeFilter{}, 0, ""); resync {
		t.Error("a subscriber with no cursor and no epoch must start from now, not resync")
	}
}

// #418 (a) and (b), across a real graceful restart through the real store: a
// subscriber that held the last cursor is resumed, one that was behind it is
// told, and the first event after the restart reaches the resumed one as its
// backlog.
func TestAGracefulStopLetsACaughtUpSubscriberResumeAndAnnouncesTheOthers(t *testing.T) {
	dir := t.TempDir()
	first := openRestartable(t, dir)
	last := publishN(t, first, 7)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openRestartable(t, dir)
	if second.epoch != first.epoch {
		t.Fatalf("epoch changed across the restart: %q -> %q", first.epoch, second.epoch)
	}
	if got := second.events.currentCursor(); got != last {
		t.Fatalf("restored cursor = %d, want %d: Close must leave the file holding exactly the last cursor handed out, not the block-ahead ceiling", got, last)
	}

	_, backlog, resync := second.events.add(ScopeFleet, driver.SubscribeFilter{}, last, first.epoch)
	if resync || len(backlog) != 0 {
		t.Fatalf("a subscriber caught up at %d before a graceful restart: resync=%v backlog=%d, want neither", last, resync, len(backlog))
	}
	if _, _, resync := second.events.add(ScopeFleet, driver.SubscribeFilter{}, last-1, first.epoch); !resync {
		t.Errorf("a subscriber one event behind (%d, last was %d) was resumed silently, though that event is gone", last-1, last)
	}
	if _, _, resync := second.events.add(ScopeFleet, driver.SubscribeFilter{}, 1, first.epoch); !resync {
		t.Error("a subscriber far behind was resumed silently across the restart")
	}

	second.events.publish(sessionEvent("a", fleet.StatusIdle))
	_, backlog, resync = second.events.add(ScopeFleet, driver.SubscribeFilter{}, last, first.epoch)
	if resync || len(backlog) != 1 || backlog[0].Cursor != last+1 {
		t.Fatalf("after one event, the caught-up subscriber got resync=%v backlog=%v, want exactly cursor %d", resync, backlog, last+1)
	}
}

// The same restart without the stop: the file holds the ceiling, which is above
// the last event, so a subscriber that was caught up cannot be told apart from
// one that was behind and is announced like every other. That is the honest side
// of the trade the issue names — and the property to protect, because the
// alternative resumes the ones that were behind without a word.
func TestACrashAnnouncesCursorExpiredToEverySubscriberBelowTheCeiling(t *testing.T) {
	dir := t.TempDir()
	first := openRestartable(t, dir)
	last := publishN(t, first, 7) // far below one block of 1024: the file holds the ceiling

	second := openRestartable(t, dir) // no Close
	if got := second.events.currentCursor(); got <= last {
		t.Fatalf("restored cursor %d is not above the last cursor handed out (%d): a crash must continue from the ceiling", got, last)
	}
	for _, c := range []int64{last, last - 1, 1} {
		if _, backlog, resync := second.events.add(ScopeFleet, driver.SubscribeFilter{}, c, first.epoch); !resync || len(backlog) != 0 {
			t.Errorf("after a crash a subscriber at %d (last handed out %d) got resync=%v backlog=%d, want resync", c, last, resync, len(backlog))
		}
	}
}

// Close must not lower the durable ceiling under a publisher: a cursor handed
// out above what the file holds is reused by the next run (§7.3). Held open
// mid-write, the seal must stop a publisher from stamping, and when it lets go
// the file must end up above every cursor handed out.
func TestAPublisherCannotStampWhileTheSealIsBeingWritten(t *testing.T) {
	h := newHub("testbox", "e")
	var (
		mu       sync.Mutex
		ceilings []int64
	)
	entered, release := make(chan struct{}), make(chan struct{})
	h.block = 1000
	h.reserve = func(c int64) error {
		mu.Lock()
		isSeal := len(ceilings) == 1
		mu.Unlock()
		if isSeal { // the second write is the seal's: hold it open
			close(entered)
			<-release
		}
		mu.Lock()
		ceilings = append(ceilings, c)
		mu.Unlock()
		return nil
	}
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	for i := 0; i < 5; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
		<-sub.ch
	}
	if h.currentCursor() != 5 {
		t.Fatalf("setup: cursor %d, want 5", h.currentCursor())
	}

	sealed := make(chan error, 1)
	go func() { sealed <- h.seal() }()
	<-entered

	published := make(chan struct{})
	go func() {
		defer close(published)
		h.publish(sessionEvent("a", fleet.StatusIdle))
	}()
	select {
	case <-published:
		t.Fatal("a publisher stamped while the seal was lowering the file: its cursor would be above what is durable")
	case <-time.After(150 * time.Millisecond):
	}
	if got := h.currentCursor(); got != 5 {
		t.Fatalf("cursor moved to %d while the seal's write was in flight", got)
	}

	close(release)
	if err := <-sealed; err != nil {
		t.Fatal(err)
	}
	<-published
	ev := <-sub.ch
	mu.Lock()
	defer mu.Unlock()
	if len(ceilings) != 3 || ceilings[1] != 5 {
		t.Fatalf("ceilings written = %v, want [first block, exact 5, a fresh one]", ceilings)
	}
	if last := ceilings[len(ceilings)-1]; last < ev.Cursor {
		t.Fatalf("the file ends at %d, below the cursor %d just handed out", last, ev.Cursor)
	}
}

// A disk that will not take the seal leaves the feed alone. The ceiling is left
// to be rewritten by the next publish rather than trusted, so the invariant that
// the file never trails a handed-out cursor still holds.
func TestAFailedSealIsReportedAndTheNextPublishRestoresACeiling(t *testing.T) {
	h := newHub("testbox", "e")
	r := &reservations{}
	h.reserve, h.block = r.save, 8
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	for i := 0; i < 3; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
		<-sub.ch
	}

	r.mu.Lock()
	r.fail = true
	r.mu.Unlock()
	if err := h.seal(); err == nil {
		t.Fatal("a seal the disk refused reported success")
	}
	r.mu.Lock()
	r.fail = false
	r.mu.Unlock()

	h.publish(sessionEvent("a", fleet.StatusIdle))
	ev := <-sub.ch
	if ev.Cursor != 4 {
		t.Fatalf("event after a failed seal = cursor %d, want 4", ev.Cursor)
	}
	if _, ceiling := r.snapshot(); ceiling < ev.Cursor {
		t.Fatalf("the durable ceiling %d is below the cursor %d just handed out", ceiling, ev.Cursor)
	}
}

// Nothing to adjust costs nothing: a service without durable state, and a run
// that is already exact (a quiet one restored from an exact file), neither
// flush for it.
func TestSealWritesNothingWhenThereIsNothingToMakeExact(t *testing.T) {
	if err := newHub("testbox", "e").seal(); err != nil {
		t.Errorf("seal without durable state = %v, want nil", err)
	}
	if err := New("testbox").Close(); err != nil {
		t.Errorf("Close on a service without a store = %v, want nil", err)
	}

	h := newHub("testbox", "e")
	r := &reservations{}
	h.reserve, h.block = r.save, 8
	h.cursor, h.reserved = 40, 40
	if err := h.seal(); err != nil {
		t.Fatal(err)
	}
	if saves, _ := r.snapshot(); saves != 0 {
		t.Errorf("seal of a file that was already exact wrote %d times, want 0", saves)
	}

	h.publish(sessionEvent("a", fleet.StatusIdle)) // 41, makes 48 durable
	if err := h.seal(); err != nil {
		t.Fatal(err)
	}
	if saves, ceiling := r.snapshot(); saves != 2 || ceiling != 41 {
		t.Errorf("after one event and a seal: %d writes, file at %d, want 2 writes and 41", saves, ceiling)
	}
}
