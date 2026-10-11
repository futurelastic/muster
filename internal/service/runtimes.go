package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// An optional runtime is one the operator enabled but whose driver may not be
// buildable at the moment the service starts (muster #297). The case that
// motivated this: a runtime's version probe is killed by its own deadline while
// the machine is busy restarting, and the runtime used to vanish until the next
// restart — to a caller indistinguishable from one nobody enabled.
//
// The service therefore keeps what it knows about an enabled runtime that has
// no driver: why, and whether it is still trying. That state is what
// ListRuntimes lists, what a create naming the runtime is refused with, and
// what route resolution consults so a runtime that is merely late is not
// silently routed around.

const (
	defaultOptionalRetryMin   = 2 * time.Second
	defaultOptionalRetryMax   = 60 * time.Second
	defaultOptionalDeadlineMs = 30000
)

// unavailableRuntime is one enabled runtime with no working driver.
type unavailableRuntime struct {
	caps   fleet.DriverCapabilities
	cause  fleet.UnavailableCause
	reason string
	// retrying is true while a background retry is running; only then does a
	// create answer "try again" instead of "this needs somebody's hands".
	retrying bool
}

// OptionalRuntime describes a runtime StartOptionalRuntime builds, and keeps
// trying to build while the reason it failed is one time can fix.
type OptionalRuntime struct {
	Runtime fleet.RuntimeId
	// DeadlineMs is the deadline the runtime's driver will declare. It goes on
	// the row listed while there is no driver; zero uses a default.
	DeadlineMs int64
	// Build constructs the driver. attempt is 0 for the synchronous first try
	// and counts up. An error that is (or wraps) *driver.UnavailableError says
	// why; any other error is a refusal no retry can fix. Build must honour
	// ctx: it is cancelled by the stop function StartOptionalRuntime returns.
	Build func(ctx context.Context, attempt int) (driver.Driver, error)
	// RetryMin and RetryMax bound the doubling wait between retries. Zero
	// means 2s and 60s.
	RetryMin, RetryMax time.Duration
	// Logf receives the service's log lines about this runtime; nil uses the
	// standard logger.
	Logf func(format string, args ...any)
}

// StartOptionalRuntime builds and registers an optional runtime.
//
// The first attempt is synchronous, so on a healthy machine the runtime is
// registered before this returns — before SetDefaultRuntime, before any
// listener — exactly as it was when registration was a plain call. When that
// attempt fails:
//
//   - for a cause no waiting fixes (not installed, not executable, a refused
//     configuration) the runtime is recorded as unavailable once and left;
//   - for a probe that did not answer in time it is recorded as unavailable and
//     retried in the background with a doubling wait, with no attempt limit,
//     until it answers, a non-retryable cause appears, or ctx ends.
//
// The returned stop function cancels the retry and waits for it to finish. It
// is safe to call more than once, and after it returns the service registers
// nothing more for this runtime — so the caller's shutdown can stop it first
// and then shut down whatever driver was built.
func (s *Service) StartOptionalRuntime(ctx context.Context, o OptionalRuntime) (stop func()) {
	if o.RetryMin <= 0 {
		o.RetryMin = defaultOptionalRetryMin
	}
	if o.RetryMax <= 0 {
		o.RetryMax = defaultOptionalRetryMax
	}
	if o.RetryMax < o.RetryMin {
		o.RetryMax = o.RetryMin
	}
	if o.DeadlineMs <= 0 {
		o.DeadlineMs = defaultOptionalDeadlineMs
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}

	if s.attemptOptional(ctx, o, 0) {
		return func() {}
	}

	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		delay := o.RetryMin
		for attempt := 1; ; attempt++ {
			t := time.NewTimer(delay)
			select {
			case <-rctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			if s.attemptOptional(rctx, o, attempt) {
				return
			}
			if delay *= 2; delay > o.RetryMax {
				delay = o.RetryMax
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// attemptOptional runs one attempt and records its outcome. It reports whether
// nothing more should be tried: registered, a cause waiting cannot fix, or the
// context ended.
func (s *Service) attemptOptional(ctx context.Context, o OptionalRuntime, attempt int) (done bool) {
	d, err := o.Build(ctx, attempt)
	if err == nil {
		if rerr := s.RegisterLocalDriver(o.Runtime, d); rerr != nil {
			if sd, ok := d.(interface{ Shutdown() error }); ok {
				_ = sd.Shutdown()
			}
			o.Logf("muster: registering %s runtime: %v", o.Runtime, rerr)
			s.recordUnavailable(o, fleet.CauseStartFailed, attempt, false)
			return true
		}
		if attempt == 0 {
			o.Logf("muster: %s runtime registered", o.Runtime)
		} else {
			o.Logf("muster: %s runtime registered after %d retries", o.Runtime, attempt)
		}
		return true
	}
	if ctx.Err() != nil {
		// Stopping: the failure is the cancellation, not a fact about the
		// runtime, so it must not overwrite what the row last said.
		return true
	}
	cause := fleet.CauseStartFailed
	var ue *driver.UnavailableError
	if errors.As(err, &ue) {
		cause = ue.Cause
	}
	retry := cause.Retryable()
	if attempt == 0 {
		if retry {
			o.Logf("muster: %s runtime unavailable (%s), retrying in the background: %v", o.Runtime, cause, err)
		} else {
			o.Logf("muster: %s runtime not started, continuing without it (%s): %v", o.Runtime, cause, err)
		}
	}
	s.recordUnavailable(o, cause, attempt, retry)
	return !retry
}

// recordUnavailable sets the row an enabled runtime without a driver lists as.
// The reason is a sentence about the runtime, never the underlying error: that
// error carries the resolved path of a binary, which belongs in the service's
// own log and not on a wire any caller can read.
func (s *Service) recordUnavailable(o OptionalRuntime, cause fleet.UnavailableCause, attempt int, retrying bool) {
	var reason string
	switch cause {
	case fleet.CauseNotInstalled:
		reason = fmt.Sprintf("the %s runtime is enabled, but its binary was not found on this machine", o.Runtime)
	case fleet.CauseNotExecutable:
		reason = fmt.Sprintf("the %s runtime is enabled, but its binary cannot be run", o.Runtime)
	case fleet.CauseProbeTimeout:
		reason = fmt.Sprintf("the %s runtime is enabled, but its version check did not answer in time; retrying in the background (attempt %d failed)", o.Runtime, attempt+1)
	case fleet.CauseProbeFailed:
		reason = fmt.Sprintf("the %s runtime is enabled, but its version check failed", o.Runtime)
	default:
		reason = fmt.Sprintf("the %s runtime is enabled, but it could not be started; see the service log", o.Runtime)
	}
	u := unavailableRuntime{
		caps:     fleet.DriverCapabilities{DeadlineMs: o.DeadlineMs}.Assumed(),
		cause:    cause,
		reason:   reason,
		retrying: retrying,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, registered := s.local[o.Runtime]; registered {
		return // a driver arrived by another route; the row would be a lie
	}
	if s.unavailable == nil {
		s.unavailable = make(map[fleet.RuntimeId]unavailableRuntime)
	}
	s.unavailable[o.Runtime] = u
}

// unavailableError is what a create naming an enabled-but-unusable runtime
// answers. A runtime still being retried is "unreachable, retryable" — nothing
// is known yet, ask again; one that needs somebody's hands is "unsupported".
// Neither is the not_found an id nobody enabled gets: the runtime was
// configured, and the answer must say so.
func unavailableError(machine fleet.MachineId, rt fleet.RuntimeId, u unavailableRuntime) *fleet.Error {
	e := &fleet.Error{
		Kind:    fleet.ErrorUnsupported,
		Message: fmt.Sprintf("runtime %q is unavailable: %s", rt, u.reason),
		Machine: machine,
		Reason:  fleet.ReasonRuntimeUnavailable,
	}
	if u.retrying {
		e.Kind = fleet.ErrorUnreachable
		e.Retryable = true
	}
	return e
}

// unavailableRows returns the rows of enabled runtimes with no driver, in
// runtime-id order, for ListRuntimes. Callers hold s.mu.
func (s *Service) unavailableRowsLocked() []fleet.RuntimeInfo {
	if len(s.unavailable) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.unavailable))
	for rt := range s.unavailable {
		ids = append(ids, string(rt))
	}
	sort.Strings(ids)
	out := make([]fleet.RuntimeInfo, 0, len(ids))
	for _, id := range ids {
		u := s.unavailable[fleet.RuntimeId(id)]
		out = append(out, fleet.RuntimeInfo{
			Machine:      s.self,
			Runtime:      fleet.RuntimeId(id),
			Capabilities: u.caps,
			Status:       fleet.RuntimeUnavailable,
			Cause:        u.cause,
			Reason:       u.reason,
		})
	}
	return out
}

// retryingRuntimesLocked returns the enabled runtimes that have no driver yet
// but are still being tried. Callers hold s.mu.
func (s *Service) retryingRuntimesLocked() map[fleet.RuntimeId]unavailableRuntime {
	var out map[fleet.RuntimeId]unavailableRuntime
	for rt, u := range s.unavailable {
		if !u.retrying {
			continue
		}
		if out == nil {
			out = make(map[fleet.RuntimeId]unavailableRuntime)
		}
		out[rt] = u
	}
	return out
}
