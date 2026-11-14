package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A StepTimeout is still the context error it wraps (muster #476): every
// errors.Is(err, context.DeadlineExceeded) in the service and the drivers keeps
// answering through it, and errors.As finds it through a wrapping.
func TestStepTimeoutUnwrapsToItsContextError(t *testing.T) {
	own := &StepTimeout{Step: "the wait for the thing", Budget: 10 * time.Second, Err: context.DeadlineExceeded}
	wrapped := fmt.Errorf("opencode: server did not become ready: %w", own)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("a step's own budget running out no longer reads as a deadline")
	}
	var got *StepTimeout
	if !errors.As(wrapped, &got) || got != own {
		t.Errorf("errors.As found %v, want the step timeout itself", got)
	}

	gone := &StepTimeout{Step: "the wait for the thing", Err: context.Canceled}
	if !errors.Is(gone, context.Canceled) || errors.Is(gone, context.DeadlineExceeded) {
		t.Error("a cancelled caller must read as cancelled and not as a deadline")
	}
}

// What the error says on its own, for a layer that folds it into a sentence.
func TestStepTimeoutSaysWhichBudgetRanOut(t *testing.T) {
	own := (&StepTimeout{Step: "the wait for the thing", Budget: 10 * time.Second, Err: context.DeadlineExceeded}).Error()
	for _, want := range []string{"the wait for the thing", "its own budget of 10s"} {
		if !strings.Contains(own, want) {
			t.Errorf("%q does not say %q", own, want)
		}
	}
	cut := (&StepTimeout{Step: "the wait for the thing", Err: context.DeadlineExceeded}).Error()
	if strings.Contains(cut, "its own budget") {
		t.Errorf("%q names a budget of its own for a step the request's deadline ended", cut)
	}
	if !strings.Contains(cut, "the wait for the thing") || !strings.Contains(cut, "cut short") {
		t.Errorf("%q does not say which step was cut short", cut)
	}
}
