package service

import (
	"fmt"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// closeTombstones fills a store's history with n ClosedByClose tombstones, for
// sessions that are NOT running, the way a machine that has been up for weeks
// carries thousands of them.
func closeTombstones(h *historyStore, n int, at time.Time) {
	for i := 0; i < n; i++ {
		started := at.Add(-time.Duration(n-i) * time.Minute)
		h.closed = append(h.closed, fleet.ClosedSession{
			SessionRef: fleet.SessionRef{Machine: h.self, ID: fmt.Sprintf("gone-%d", i)},
			Runtime:    "tmux",
			StartedAt:  &started,
			ClosedAt:   at,
			ClosedBy:   fleet.ClosedByClose,
		})
	}
	h.closedChangedLocked()
}

func liveSessions(n int, at time.Time) []fleet.Session {
	out := make([]fleet.Session, n)
	for i := range out {
		started := at.Add(-time.Duration(i+1) * time.Hour)
		out[i] = fleet.Session{
			SessionRef: fleet.SessionRef{Machine: "m", ID: fmt.Sprintf("live-%d", i)},
			Runtime:    "tmux",
			StartedAt:  &started,
		}
	}
	return out
}

// #415: every live session was checked against every retained tombstone on
// every observe pass. Counted: a pass over 80 live sessions and 5 000
// tombstones, none of them related, examines no tombstone at all.
func TestObserveDoesNotWalkTheHistoryForSessionsWithNoTombstone(t *testing.T) {
	h := newHistoryStore("m")
	now := time.Now()
	closeTombstones(h, 5000, now)

	live := liveSessions(80, now)
	for pass := 0; pass < 3; pass++ {
		h.observe("tmux", live, false, now, nil)
	}
	if h.scanned != 0 {
		t.Fatalf("3 passes over 80 live sessions examined %d tombstones, want 0 — that is the O(live x closed) walk (80 x 5000 = 400 000 a pass)", h.scanned)
	}
	if len(h.closed) != 5000 {
		t.Fatalf("history changed: %d tombstones, want 5000", len(h.closed))
	}
}

// What the index must not cost: a close that did not take is still withdrawn,
// the right tombstone is the one removed, and the walk happens once for it, not
// once per live session.
func TestWithdrawStillRemovesTheMatchingTombstoneAndOnlyThat(t *testing.T) {
	h := newHistoryStore("m")
	now := time.Now()
	closeTombstones(h, 5000, now)

	live := liveSessions(80, now)
	// live-7 was "closed" but is still running: same runtime, id and start.
	premature := live[7]
	h.closed = append(h.closed, fleet.ClosedSession{
		SessionRef: premature.SessionRef, Runtime: "tmux",
		StartedAt: premature.StartedAt, ClosedAt: now, ClosedBy: fleet.ClosedByClose,
	})
	// Same id, a DIFFERENT start: an older occupant of the id, which is rightly
	// a tombstone and must survive.
	older := now.Add(-100 * time.Hour)
	h.closed = append(h.closed, fleet.ClosedSession{
		SessionRef: premature.SessionRef, Runtime: "tmux",
		StartedAt: &older, ClosedAt: now, ClosedBy: fleet.ClosedByClose,
	})
	h.closedChangedLocked()
	before := len(h.closed)

	h.observe("tmux", live, false, now, nil)

	if len(h.closed) != before-1 {
		t.Fatalf("%d tombstones after the pass, want %d: exactly the premature one goes", len(h.closed), before-1)
	}
	for _, c := range h.closed {
		if c.ID == premature.ID && c.StartedAt.Equal(*premature.StartedAt) {
			t.Fatal("the premature tombstone is still there")
		}
	}
	survived := false
	for _, c := range h.closed {
		if c.ID == premature.ID && c.StartedAt.Equal(older) {
			survived = true
		}
	}
	if !survived {
		t.Error("the tombstone of an earlier occupant of the same id was withdrawn")
	}
	if h.scanned > int64(before) {
		t.Errorf("one withdrawal examined %d tombstones, want at most one walk (%d)", h.scanned, before)
	}
}

// The index is dropped by every change to the history, so a tombstone written
// after it was built is still found. A missing entry would leave a premature
// close standing, which is the failure the walk existed to prevent.
func TestWithdrawSeesATombstoneWrittenAfterTheIndexWasBuilt(t *testing.T) {
	h := newHistoryStore("m")
	now := time.Now()
	live := liveSessions(3, now)

	h.observe("tmux", live, true, now, nil) // sights them and builds the index
	h.closedByRequest("tmux", live[1].ID)   // the close that "did not take"
	if len(h.closed) != 1 {
		t.Fatalf("want one tombstone, have %d", len(h.closed))
	}
	h.observe("tmux", live, false, now, nil)
	if len(h.closed) != 0 {
		t.Fatalf("the premature tombstone survived a listing that shows the session running: %+v", h.closed)
	}
}

// Retention drops tombstones; the index must not keep answering for them.
func TestWithdrawIndexForgetsTombstonesThatAgedOut(t *testing.T) {
	h := newHistoryStore("m")
	now := time.Now()
	h.now = func() time.Time { return now }
	h.retention = time.Hour
	live := liveSessions(2, now)
	h.closed = append(h.closed, fleet.ClosedSession{
		SessionRef: live[0].SessionRef, Runtime: "tmux",
		StartedAt: live[0].StartedAt, ClosedAt: now.Add(-2 * time.Hour), ClosedBy: fleet.ClosedByClose,
	})
	h.closedChangedLocked()
	h.closeKeysLocked() // build it while the tombstone is still there

	h.mu.Lock()
	h.pruneLocked(now)
	h.mu.Unlock()
	if len(h.closed) != 0 {
		t.Fatalf("prune kept an aged-out tombstone: %+v", h.closed)
	}
	if _, stale := h.closeKeysLocked()[tombKey{"tmux", live[0].ID, live[0].StartedAt.UnixNano()}]; stale {
		t.Error("the index still answers for a tombstone retention removed")
	}
}
