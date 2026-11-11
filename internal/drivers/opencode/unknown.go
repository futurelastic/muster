package opencode

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sessions a restart leaves unknown (muster #445).
//
// A restart adopts every session it finds and drops none (§7.5). One it cannot
// relaunch faithfully is listed `unknown` and keeps its record and its scoped
// directory until its caller closes it: the directory is a whole runtime store and
// package cache, hundreds of megabytes, and a caller that never comes back leaves it
// there for good. The other rows about this runtime do not see it — `servers` counts
// processes with no record, `dirs` counts directories with no record, and these have
// both — so nothing said that disk was being held by sessions nobody used again.
//
// # What this decides, and what it deliberately does not
//
// Which sessions are listed `unknown` is decided by the running service, in memory,
// at its start (resumeOne). The doctor is another process, and for a session that
// has no server the listing carries no size either. A record on disk says two things
// about it, and both are counted here:
//
//   - a create that carried caller-supplied environment, whose values this driver
//     never kept (recreateReason). That is the class a restart can never relaunch,
//     and the one measured on a machine that had the problem (#445);
//   - a relaunch that FAILED at the last start, for a reason that may pass: the
//     runtime would not start, a sandbox that cannot be enforced now, a context file
//     that is gone (#446). The service writes that fact onto the record when it lists
//     the session unknown (noteRelaunch), because from a record alone such a session
//     looks exactly like one of a service that is merely stopped.
//
// Not counted, on purpose:
//   - a session with no such mark, whatever the service is doing. A stopped service
//     leaves every session without a server, and a restart relaunches the clean ones;
//     counting them would warn about every session whenever the doctor is run before
//     the service starts, which is when it is meant to be run. The mark is the one
//     thing that separates a session that has failed a start from one that has not
//     been asked yet;
//   - a record that cannot be read. It is listed `unknown` too, but names no
//     directory to measure; it is counted apart (Unreadable), like the dirs row does.
//
// What the mark says is what the LAST start did, not what the next one will: a
// session it names is retried at every start and its mark comes off when one works
// (relaunch), so a person who has fixed the cause sees the row clear after the next
// start and not before. That is said in the row, because it is the one way the row
// can be out of date.
//
// A session of either class whose directory a running server holds is not counted: the
// service is up and a server serves it, so it is listed as running. That is the same
// identity the dirs row uses for "a server holds it", and it is what keeps a healthy
// machine quiet.
//
// A counted session of the first class is one of two things in the running service:
// `unknown` (a restart found it and could not relaunch it) or `dead` (its server exited
// since it was created, and nothing has closed it). The doctor cannot tell them apart
// from a record, and the remedy is the same — its caller closes it — so the row says
// both. What it can never be is a session that is merely idle: a session created with
// caller-supplied environment is never parked (park.go), so a record that says so and
// has no server is not a live session waiting for its next input. A session of the
// second class is `unknown` in the running service, and listed so with its reason.
//
// Everything here only reads. It removes no record and no directory, and it starts
// and stops nothing: reaping stays with the caller that created the session.

// UnknownSession is one session that runs no server and that a restart did not or cannot
// relaunch: listed `unknown` by the service, or `dead` when its server exited since it
// was created.
type UnknownSession struct {
	// ID is the runtime's own session id, what a caller passes to close it. The
	// session's name is deliberately not carried: a title is free text.
	ID string
	// StartedAt is when the session was created, from its record. Zero when the
	// record has none.
	StartedAt time.Time
	// Bytes is the size of the regular files under the session's directory. It is
	// only meaningful when Measured.
	Bytes int64
	// Measured is false when no size could be given: the record names no directory
	// this driver made, or the time allowed for measuring ran out before this one.
	// A directory that does not exist is measured, and holds nothing.
	Measured bool
	// Partial is true when the walk stopped at its bound, so Bytes is a lower bound.
	Partial bool
	// RelaunchFailed is true for a session that could have been relaunched but whose
	// relaunch failed at the last start (muster #446); false for one created with
	// caller-supplied environment, which no start can relaunch. A record that is
	// both is the second: it needs re-creation whatever else is wrong with it.
	RelaunchFailed bool
}

// UnknownReport is what the doctor says about the sessions a restart left unknown.
type UnknownReport struct {
	// Sessions are the counted ones, oldest first.
	Sessions []UnknownSession
	// Unreadable is the number of record files that could not be read. The service
	// lists each as `unknown`, but there is nothing here to measure.
	Unreadable int
	// Uninspected is the number of processes that look like servers but could not be
	// inspected: with it non-zero a counted session may in fact be running.
	Uninspected int
}

// RelaunchFailedCount is how many of the sessions are ones whose relaunch failed at the
// last start (muster #446), the rest being the ones no start can relaunch.
func (r UnknownReport) RelaunchFailedCount() int {
	n := 0
	for _, s := range r.Sessions {
		if s.RelaunchFailed {
			n++
		}
	}
	return n
}

// Bytes is the sum of the measured sizes, and whether any figure in it is a lower
// bound or missing.
func (r UnknownReport) Bytes() (total int64, exact bool) {
	exact = true
	for _, s := range r.Sessions {
		if !s.Measured || s.Partial {
			exact = false
		}
		if s.Measured {
			total += s.Bytes
		}
	}
	return total, exact
}

// unknownWalkBudget is the most time the doctor spends measuring directories, all of
// them together. A session directory holds a package cache — many thousands of
// files — and a doctor that takes minutes is one nobody runs. A directory the budget
// did not reach is reported without a size, and one it cut short as a lower bound.
const unknownWalkBudget = 20 * time.Second

// UnknownSessions reports the sessions under stateDir that a restart cannot relaunch,
// or failed to at its last start, and that run no server (muster #445, #446). Like OrphanServers and OrphanDirs it runs from
// the doctor, another process. err is ErrScanUnsupported where the platform cannot
// list processes: without that, a session that is running cannot be told from one
// that is not, and nothing is reported rather than a guess.
func UnknownSessions(stateDir string) (UnknownReport, error) {
	storeDir, root := stateLayout(stateDir)
	return unknownSessionReport(storeDir, root, scanServers, unknownWalkBudget)
}

// unknownSessionReport is UnknownSessions with the process listing and the time to
// measure given, so a test on any platform can say which servers are running and how
// long the walks may take. A budget of zero measures nothing: every session is still
// reported, without a figure.
func unknownSessionReport(storeDir, root string, scan func(string) ([]serverProc, int, error), budget time.Duration) (UnknownReport, error) {
	servers, uninspected, err := scan(root)
	if err != nil {
		return UnknownReport{}, err
	}
	held := map[string]bool{}
	for _, p := range servers {
		held[p.dir] = true
	}
	rep := UnknownReport{Uninspected: uninspected}
	deadline := time.Now().Add(budget)

	var counted []*sessionRecord
	for _, it := range (&recordStore{dir: storeDir}).loadAll() {
		if it.rec == nil {
			rep.Unreadable++
			continue
		}
		callerEnv := it.rec.recreateReason() != ""
		failed := !callerEnv && it.rec.RelaunchFailedAtMs > 0
		if !callerEnv && !failed {
			continue // a restart relaunches it, and none has failed to: not a session anything is waiting on
		}
		if it.rec.Dir != "" && held[filepath.Base(filepath.Clean(it.rec.Dir))] {
			continue // a server holds its directory: the service is up and has not restarted since the create
		}
		counted = append(counted, it.rec)
	}
	sort.SliceStable(counted, func(i, j int) bool {
		if counted[i].StartedAtMs != counted[j].StartedAtMs {
			return counted[i].StartedAtMs < counted[j].StartedAtMs
		}
		return counted[i].ID < counted[j].ID
	})

	for _, rec := range counted {
		s := UnknownSession{ID: rec.ID, RelaunchFailed: rec.recreateReason() == ""}
		if rec.StartedAtMs > 0 {
			s.StartedAt = time.UnixMilli(rec.StartedAtMs)
		}
		// A record is a file on disk and could name any path; a directory is walked
		// only when it is one this driver made (the rule ownsDir applies before it
		// trusts one to start or remove anything).
		if ownedScopedDir(root, rec.Dir) {
			if left := time.Until(deadline); left > 0 {
				lim := scratchLimits{maxWalk: left}.normalized()
				fig, ok := measureScratch(rec.Dir, lim)
				s.Measured, s.Partial = true, fig.partial
				if ok {
					s.Bytes = fig.total()
				}
			}
		}
		rep.Sessions = append(rep.Sessions, s)
	}
	return rep, nil
}

// ownedScopedDir reports whether dir is a session directory of root: absolute,
// directly under root, named as sessionDirs names them. It is ownsDir without the
// driver, for a process that has no driver to ask.
func ownedScopedDir(root, dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs // a state directory given relative is compared as the absolute path a record holds
	}
	rel, err := filepath.Rel(root, filepath.Clean(dir))
	return err == nil && !strings.Contains(rel, string(filepath.Separator)) &&
		strings.HasPrefix(rel, "muster-opencode-")
}
