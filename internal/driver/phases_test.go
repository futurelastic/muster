package driver

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNotePhaseWithoutARecorderIsANoOp(t *testing.T) {
	// Must not panic and must not need a recorder: every direct driver call in
	// every other package's tests runs without one.
	NotePhase(context.Background(), "terminal", time.Now())
	var nilPhases *Phases
	if got := nilPhases.Fields(); got != "" {
		t.Fatalf("nil recorder rendered %q, want empty", got)
	}
}

func TestPhasesRecordInOrderAndRenderAsFields(t *testing.T) {
	p := &Phases{}
	ctx := WithPhases(context.Background(), p)
	NotePhase(ctx, "resolve", time.Now().Add(-3*time.Millisecond))
	NotePhase(ctx, "terminal", time.Now().Add(-1500*time.Millisecond))

	all := p.All()
	if len(all) != 2 || all[0].Name != "resolve" || all[1].Name != "terminal" {
		t.Fatalf("phases = %+v, want resolve then terminal", all)
	}
	fields := p.Fields()
	if !strings.HasPrefix(fields, "phase.resolve=") || !strings.Contains(fields, " phase.terminal=15") {
		t.Fatalf("fields = %q, want phase.resolve=… phase.terminal=15xx in order", fields)
	}
	if strings.ContainsAny(fields, "\n\"") {
		t.Fatalf("fields = %q must be plain space-separated tokens", fields)
	}
}

func TestPhasesAreSafeForConcurrentNotes(t *testing.T) {
	p := &Phases{}
	ctx := WithPhases(context.Background(), p)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); NotePhase(ctx, "x", time.Now()) }()
	}
	wg.Wait()
	if n := len(p.All()); n != 50 {
		t.Fatalf("recorded %d phases, want 50", n)
	}
}

func TestMillisRoundsToNearest(t *testing.T) {
	cases := map[time.Duration]int64{
		0:                       0,
		499 * time.Microsecond:  0,
		500 * time.Microsecond:  1,
		1499 * time.Microsecond: 1,
		9214 * time.Millisecond: 9214,
	}
	for d, want := range cases {
		if got := Millis(d); got != want {
			t.Errorf("Millis(%v) = %d, want %d", d, got, want)
		}
	}
}
