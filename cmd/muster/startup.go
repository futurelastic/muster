package main

import (
	"context"
	"log"
	"time"

	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/tmux"
	"github.com/futurelastic/muster/internal/service"
)

// finishStartup is the part of startup that runs after the listeners are open
// (muster #508), in the order it always ran: the reconcile (§12), the delivery
// lanes its result decides, the modules, and the sweep that records sessions that
// ended while the service was down.
//
// reconciled says the service held the record store and refused changes before
// the listeners opened (main did both); finishStartup lifts the refusal as soon
// as the reconcile's result is applied and the modules have been started, and
// before the sweep, which only reads. ctx is cancelled at shutdown: a reconcile
// then ends early, and nothing is started.
//
// Nothing here destroys anything: §12 rule 4 is as absolute as it was, and the
// only change is where in the process's life this runs.
func finishStartup(ctx context.Context, svc *service.Service, localDriver driver.Driver, reconciled bool, startedAt time.Time) {
	if rec, ok := localDriver.(interface {
		Reconcile(context.Context) (tmux.Reconciliation, error)
	}); ok {
		begun := time.Now()
		got, err := rec.Reconcile(ctx)
		if err != nil {
			log.Printf("muster: reconciliation failed after %dms: %v", driver.Millis(time.Since(begun)), err)
		} else {
			log.Printf("muster: reconciled — %s", got)
			// Orphans and disappearances are named individually. §12 rule 4
			// forbids acting on them, which makes reporting them the entire
			// value: a session this service cannot explain is one for a
			// human to look at, and a human cannot look at a count.
			for _, s := range got.Orphaned {
				log.Printf("muster:   orphaned %q cwd=%s (%s)", s.ID, s.Cwd, s.State.Evidence)
			}
			for _, s := range got.Vanished {
				log.Printf("muster:   vanished %q (%s)", s.ID, s.State.Evidence)
			}
			// #185: an adopted session's delivery lane stays, to be re-attached
			// when its module is ready; a vanished one's is closed and dropped.
			if lr, ok := localDriver.(interface{ ReconcileLanes(tmux.Reconciliation) }); ok {
				lr.ReconcileLanes(got)
			}
		}
	}
	if ctx.Err() != nil {
		return
	}

	// #185: start the delivery modules last among the startup work, without
	// blocking: each child comes up on its own schedule and a create that lands
	// first takes the built-in lane.
	if sm, ok := localDriver.(interface{ StartDeliveryModules() }); ok {
		sm.StartDeliveryModules()
	}
	if reconciled {
		svc.SetReconciling(false)
		log.Printf("muster: startup: changes to sessions are accepted (%dms after start)", driver.Millis(time.Since(startedAt)))
	}

	// Sessions that ended while this service was down are found missing by
	// one complete local read, and get their closed-session record (#179).
	// Runs for every runtime, reconciling driver or not.
	sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	svc.SweepLocal(sweepCtx)
	cancel()
}
