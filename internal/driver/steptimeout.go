package driver

import (
	"fmt"
	"time"
)

// StepTimeout is a driver step that ran out of time (muster #476).
//
// A deadline used to reach the service as a bare context error, and the service
// could only say one thing about it: "no answer within <the caller's deadline>".
// That is wrong whenever the step was bounded by a budget of its own that is
// shorter than the caller's: a 10 s readiness wait that ends a create at 10 s
// was reported as "no answer within 30s", sending the operator to look for a
// 30 s problem. A step that has a budget of its own, or that wants its name on
// the message, returns this instead; it wraps the context error, so every
// errors.Is(err, context.DeadlineExceeded) already in the code keeps answering.
type StepTimeout struct {
	// Step names what was being done, as a noun phrase that reads in the middle
	// of a sentence ("the wait for the runtime to become ready").
	Step string
	// Budget is the step's own budget, when that is what ran out. Zero means the
	// step's own budget was not the limit: the request's deadline ran out (or the
	// caller went away) while the step was running.
	Budget time.Duration
	// Err is the context error that ended the step.
	Err error
}

// Error reads on its own, because a layer that folds the error into a longer
// sentence (the park reason, a log line) has nothing else to say it with.
func (e *StepTimeout) Error() string {
	if e.Budget > 0 {
		return fmt.Sprintf("%s ran past its own budget of %s", e.Step, e.Budget)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s was cut short: %v", e.Step, e.Err)
	}
	return e.Step + " was cut short"
}

// Unwrap returns the context error, so errors.Is(err, context.DeadlineExceeded)
// and errors.Is(err, context.Canceled) see through a StepTimeout.
func (e *StepTimeout) Unwrap() error { return e.Err }
