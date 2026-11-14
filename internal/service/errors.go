package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/futurelastic/muster/internal/driver"
)

func isUnsupported(err error) bool {
	return errors.Is(err, driver.ErrUnsupported)
}

func isDeadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// isCancelled reports whether the work was abandoned because the request's
// context was cancelled: the caller hung up, or the service is stopping. It is
// not a refusal of anything the caller asked for (muster #476).
func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// outOfTimeMessage is the sentence an unreachable answer carries when a call ran
// out of time, naming the budget that ran out and never the wrapped error's own
// text (which can carry the runtime's loopback address or a working directory).
// deadline is the request's effective deadline.
//
// Three different things end a call early and each reads differently, because
// each is fixed somewhere different (muster #476): the request's deadline (the
// caller asks for more time), a step's own budget that is shorter than the
// deadline (the operator looks at that step), and a caller that went away
// (nothing to fix).
func outOfTimeMessage(err error, deadline time.Duration) string {
	if isCancelled(err) {
		return "the request was cancelled before an answer (the caller hung up, or the service is stopping)"
	}
	var step *driver.StepTimeout
	if errors.As(err, &step) {
		if step.Budget > 0 {
			msg := fmt.Sprintf("%s ran past its own budget of %s", step.Step, step.Budget)
			if deadline > step.Budget {
				msg += fmt.Sprintf(" (the request's deadline of %s had not run out)", deadline)
			}
			return msg
		}
		return fmt.Sprintf("no answer within %s (the deadline ran out during %s)", deadline, step.Step)
	}
	return "no answer within " + deadline.String()
}
