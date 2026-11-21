package service

import (
	"net/http"
	"strconv"
	"time"

	fleet "github.com/futurelastic/muster"
)

// The window between the listeners opening and the startup reconcile finishing
// (muster #508).
//
// A machine with about a hundred sessions takes over a minute to reconcile, and
// the service used to open its port only after that: every client got a refused
// connection for the whole time. It now listens first. Reads are answered the
// whole time. A call that would CHANGE one of this machine's own sessions is
// refused with a retryable `unreachable` carrying reason `reconciling` until the
// reconcile has applied its result, because a session created, renamed or closed
// in that window would write the very records the reconcile is still comparing
// the machine against (session-abstraction.md §12 and F30). The gate is the one
// check in mutating(), so no verb can be added that forgets it.

// retryAfterSeconds is what a refused call is told to wait. The window is long
// (tens of seconds on a large machine), so this is a polling interval, not a
// forecast of when it ends.
const retryAfterSeconds = 5

// SetReconciling opens or closes the window. main sets it true before the
// listeners open and false once the reconcile's result is applied.
func (s *Service) SetReconciling(on bool) {
	if on {
		s.reconcilingSince.Store(time.Now().UnixNano())
		return
	}
	s.reconcilingSince.Store(0)
}

// Reconciling reports whether the window is open, and since when.
func (s *Service) Reconciling() (bool, time.Time) {
	n := s.reconcilingSince.Load()
	if n == 0 {
		return false, time.Time{}
	}
	return true, time.Unix(0, n)
}

// refuseWhileReconciling answers a mutating call aimed at this machine's own
// sessions while the window is open, and reports whether it did. A call whose
// target is another machine is relayed, changes nothing here, and is not held.
func refuseWhileReconciling(w http.ResponseWriter, svc *Service, target fleet.MachineId) bool {
	if target != "" && target != svc.Self() {
		return false
	}
	on, since := svc.Reconciling()
	if !on {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	writeError(w, &fleet.Error{
		Kind: fleet.ErrorUnreachable,
		Message: "this machine is still reconciling the sessions it found at start (for " +
			time.Since(since).Round(time.Second).String() + " so far); reads are answered, " +
			"changes are accepted once it has finished — retry shortly",
		Machine:   svc.Self(),
		Reason:    fleet.ReasonReconciling,
		Retryable: true,
	})
	return true
}
