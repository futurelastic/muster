package tmux

import (
	"context"
	"fmt"
	"os"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
)

// The rename probe and its check (muster #227, split from #223 item 4).
//
// #222 built SyncTitle, which brings the runtime's OWN title to a session's
// new name by delivering "/rename <name>" the way a human would type it, and
// degrades honestly to `pending` when no custom-title transcript entry
// follows within its own submitConfirmWindow (4s, titlesync.go). What #222
// never measured — because its own probes only ever sent ordinary text, never
// a programmatic /rename — is whether the runtime writes that entry at all
// for THIS delivery shape, or merely takes longer than four seconds to. The
// production counter SyncTitle increments either way
// (title_sync.command_without_title) gives a live RATE, and a rate cannot
// tell "slow" apart from "never". This drives one real rename against a real
// session, the same two calls the service makes in the same order
// (internal/service/rename_title.go), and keeps reading the transcript for
// compatRenameWait — far past SyncTitle's own window — so the answer is
// measured against this candidate, not inferred from a counter.

// compatRenameWait bounds how long this probe keeps reading the transcript
// after delivering a programmatic rename. It is deliberately far longer than
// submitConfirmWindow (4s) — SyncTitle itself gives up after that — so a
// runtime that is merely slow to write the custom-title is not reported the
// same as one that never does.
const compatRenameWait = 30 * time.Second

// compatRename is what driving one programmatic rename against session A
// showed.
type compatRename struct {
	ran     bool
	skipped string
	// to is the name this probe renamed session A to.
	to        string
	ack       fleet.RenameAck
	renameErr error
	// sync is SyncTitle's own verdict, returned within its own window —
	// kept so the check can report it alongside this probe's longer look.
	sync fleet.TitleSync
	// scan is the LAST transcriptTitleScan read while this probe kept polling
	// past SyncTitle's own window. scanErr is set when the last attempt to
	// read the transcript failed outright (as opposed to reading it and
	// finding nothing yet).
	scan       titleScanResult
	scanErr    error
	titleAfter time.Duration // valid only when scan.sawTitle
}

// renameInto drives session A through a real programmatic rename: the id
// half through the driver's own Rename, then the title half through the
// driver's own SyncTitle — in that order, exactly as the service calls them
// — and keeps reading the transcript well past SyncTitle's own window. A
// probe returns an error only when the environment failed; a rename or sync
// that did not go as expected is recorded as evidence for the check to read,
// same discipline as sendInto.
func (h *compatHarness) renameInto(ctx context.Context) error {
	r := &h.ev.rename
	a := h.ev.a
	switch {
	case !a.ready:
		r.skipped = "the trusted session never showed a composer (see F-COMPOSER), so nothing could be renamed"
		return nil
	case h.ev.dirty != "":
		r.skipped = "an earlier draft could not be cleared (" + h.ev.dirty + "), so nothing could be sent cleanly"
		return nil
	}
	path, ok := h.transcriptPath()
	if !ok {
		r.skipped = "no per-process record was found (see D1), so the transcript cannot be located"
		return nil
	}
	var offset int64
	if fi, err := os.Stat(path); err == nil {
		offset = fi.Size()
	}

	// Sanitize-clean by construction (lowercase, digits and hyphens only,
	// so sanitizeName leaves it unchanged and decoration finds no marker in
	// it): the fix on #223 refuses a `to` sanitizeName would change, and this
	// probe measures the title half, not that refusal.
	to := h.world.label + "-renamed"
	r.to = to
	r.ran = true

	r.ack, r.renameErr = h.world.d.Rename(ctx, compatRequest, a.ref, to)
	if r.renameErr != nil || !r.ack.Accepted {
		return nil // evidence: the id half itself did not happen
	}

	newRef := a.ref
	newRef.ID = to
	sent := "/rename " + to
	started := time.Now()
	r.sync = h.world.d.SyncTitle(ctx, compatRequest, newRef)

	deadline := started.Add(compatRenameWait)
	for {
		if scan, scanErr := transcriptTitleScan(path, offset, sent); scanErr != nil {
			r.scanErr = scanErr
		} else {
			r.scan = scan
		}
		if r.scan.sawTitle {
			r.titleAfter = time.Since(started)
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		time.Sleep(submitConfirmInterval)
	}
}

// addRename registers the probe. It needs only the trusted session and its
// first transcript — the same minimal dependency E-NAME itself declares —
// but is given its own Stage, after every ordinary send: it changes session
// A's own id, and every check reading an earlier send's evidence assumes
// that id has not moved yet.
func (h *compatHarness) addRename(s *compat.Suite) {
	s.Probes = append(s.Probes, compat.Probe{
		ID: "rename.a", Stage: 5, Needs: []string{"boot.a", "send.warm"},
		// Session boot and the one send are cheap; this probe's own wait past
		// SyncTitle's window is the expensive part.
		Budget: compatRenameWait + 30*time.Second,
		Run:    h.renameInto,
	})
}

// addRenameChecks registers H-RENAME, which reads the rename probe.
func (h *compatHarness) addRenameChecks(s *compat.Suite) {
	s.Checks = append(s.Checks, compat.Check{ID: "H-RENAME", Probes: []string{"rename.a"}, Eval: func() compat.Verdict {
		r := h.ev.rename
		if !r.ran {
			return compat.Failed(r.skipped)
		}
		if r.renameErr != nil {
			return compat.Failed(fmt.Sprintf("the id-half rename to %q failed, so no programmatic /rename could be measured: %v", r.to, r.renameErr))
		}
		if !r.ack.Accepted {
			return compat.Failed(fmt.Sprintf("the id-half rename to %q was not accepted, so no programmatic /rename could be measured", r.to))
		}
		syncNote := fmt.Sprintf("SyncTitle itself reported %q within its own window (%s)", r.sync.Status, r.sync.Evidence)
		switch {
		case r.scan.sawTitle && r.scan.title == r.to:
			return compat.Passed(fmt.Sprintf(
				"a programmatic /rename to %q was followed by a custom-title transcript entry carrying that name, %s after delivery — %s",
				r.to, r.titleAfter.Round(10*time.Millisecond), syncNote))
		case r.scan.sawTitle:
			return compat.Failed(fmt.Sprintf(
				"a programmatic /rename to %q was followed by a custom-title entry, but it names %q instead, %s after delivery — %s",
				r.to, r.scan.title, r.titleAfter.Round(10*time.Millisecond), syncNote))
		case r.scan.ran:
			return compat.Failed(fmt.Sprintf(
				"the runtime's own transcript confirms the /rename command ran, but %s of waiting past delivery saw no custom-title entry ever follow it — measured, not inferred: this build does not write one for a programmatic delivery within that time — %s",
				compatRenameWait, syncNote))
		case r.scanErr != nil:
			return compat.Failed(fmt.Sprintf("the transcript could not be read to look for a custom-title entry: %v — %s", r.scanErr, syncNote))
		default:
			return compat.Failed(fmt.Sprintf(
				"%s after the programmatic /rename delivery, the transcript recorded neither the command running nor a custom-title entry — %s",
				compatRenameWait, syncNote))
		}
	}})
}
