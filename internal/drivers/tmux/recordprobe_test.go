package tmux

import (
	"os"
	"sync/atomic"
	"testing"
)

// The probe is paid for by a blocked fleet on every read, so it must cost a
// stat per record, not a read of each one, while nothing has been appended.
func TestRecordProbeReadsARecordOnlyWhenItChanged(t *testing.T) {
	s := newServedFixture(t)
	path := s.d.conversations.recordPath("/work/beta", "conv-beta")

	var reads atomic.Int32
	var p recordProbe
	p.read = func(path string) (apiErrorFact, recordVerdict) {
		reads.Add(1)
		return latestServedEntry(path)
	}
	for i := 0; i < 5; i++ {
		if _, v := p.latest(path); v != recordCleanTurn {
			t.Fatalf("verdict = %v, want recordCleanTurn", v)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("an unchanged record was read %d times, want 1", n)
	}

	appendRecordLine(t, s.root, "/work/beta", "conv-beta", rateLimitEntry("conv-beta", s.refusedAt, "x"))
	if _, v := p.latest(path); v != recordAPIError {
		t.Fatalf("verdict after an appended refusal = %v, want recordAPIError", v)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("a changed record was read %d times in all, want 2", n)
	}

	// A record that vanished is unavailable, never a remembered answer.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, v := p.latest(path); v != recordUnavailable {
		t.Fatalf("verdict for a missing record = %v, want recordUnavailable", v)
	}
}
