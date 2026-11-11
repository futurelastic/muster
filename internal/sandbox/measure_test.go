package sandbox

import (
	"testing"
	"time"
)

func TestMeasuredTTLTrustsAFindingLongAndAFailureBrieflyWithABackoffInTheBackground(t *testing.T) {
	if got := MeasuredTTL(true, false, 0); got != MeasuredFoundTTL {
		t.Errorf("a finding: %v, want %v", got, MeasuredFoundTTL)
	}
	if got := MeasuredTTL(true, true, 9); got != MeasuredFoundTTL {
		t.Errorf("a finding, however many failures came before: %v, want %v", got, MeasuredFoundTTL)
	}
	// In the background a failure that repeats waits twice as long each time.
	for _, c := range []struct {
		failures int
		want     time.Duration
	}{
		{0, 30 * time.Second}, {1, 30 * time.Second}, {2, time.Minute}, {3, 2 * time.Minute},
		{4, 4 * time.Minute}, {5, 5 * time.Minute}, {40, 5 * time.Minute},
	} {
		if got := MeasuredTTL(false, false, c.failures); got != c.want {
			t.Errorf("background, %d failures: %v, want %v", c.failures, got, c.want)
		}
	}
	// A caller about to act on the answer gets the short lifetime however many
	// failures came before it.
	for _, n := range []int{0, 1, 7, 40} {
		if got := MeasuredTTL(false, true, n); got != MeasuredFailTTL {
			t.Errorf("strict, %d failures: %v, want %v", n, got, MeasuredFailTTL)
		}
	}
}
