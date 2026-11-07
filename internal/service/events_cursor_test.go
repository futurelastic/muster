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
