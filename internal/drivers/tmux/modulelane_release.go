package tmux

// Letting go of a lane write that provably never arrived (#491).
//
// # The hole
//
// A write to a session's module lane that the module cannot confirm ends
// `unknown`, and the ledger (route.go) holds every further send of the same text
// for strandedRetention. The hold had exactly one way out: the module answering
// `rejected` to a later ask. A module that keeps saying `queued` or `silent` —
// which is what it says about a message the runtime never turned into a turn —
// held the text for the full half hour, and the caller was told "read the
// transcript" when the transcript showed nothing had arrived. A resume nudge sent
// to a session at an account wall is exactly such a message, and it is the one
// input that matters when the wall lifts.
//
// A session in that state reads `quota_blocked`, which is neither idle nor
// waiting_input, so the stall detector of #264 (two enqueues on an idle session
// with no turn start degrade the lane) never arms for it either. Nothing was
// ever going to end the hold.
//
// # What lets go, and what never does
//
// The same transcript the inbox path reads (#184) answers it, and this driver
// reads it itself: the module's own `confirm` is unchanged (#185). The transcript's
// position and the text's fingerprint are taken BEFORE the lane write, so an
// identical earlier message sits before the offset and cannot count.
//
// The hold is released only when EVERY one of these holds, and a doubt about any
// of them keeps the hold, which is the cheap side to be wrong on:
//
//   - the module did not say confirmed (that arrived) or rejected (already
//     released) — it could not say;
//   - the write was whole: a frame that may have been partly written is a module
//     that may still be processing it;
//   - the entry has settled: laneReleaseSettle past the module's own wait;
//   - the transcript is still the same conversation file and no shorter than it
//     was (a /clear or a replaced file says nothing about the old one);
//   - the session reads quiet NOW — no turn is running that could still drain a
//     queue — which includes quota_blocked, because a session at the wall runs
//     none;
//   - and a read of the transcript from the offset, taken AFTER that, is clean and
//     finds the text in no entry at all.
//
// "In no entry at all" is deliberately wider than "as a user turn": what the
// runtime writes for a lane message is not measured, so any entry that carries
// the text — a turn, an enqueue, a wrapped copy — counts as a sign it arrived and
// keeps the hold. Only the assistant's own output is not read: it cannot be the
// record of an input, and a short text such as "continue" would otherwise be
// found in the assistant's prose.
//
// The text is kept in memory only, as a normalised fingerprint no longer than
// laneNeedleRunes. The ledger's persisted form holds digests, never a message, so
// after a restart of this service there is nothing to look for and the hold
// stands: the old behaviour.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
)

// laneReleaseSettle is how long an unconfirmed lane write must have been
// recorded before its hold may be let go. The module's own confirmation window
// has already passed by the time the entry is written, so this is the margin on
// top of it: a runtime that is merely slow to write its transcript has had that
// long to do so.
const laneReleaseSettle = 15 * time.Second

// laneSourceBound bounds the work of locating the session's transcript so that a
// slow multiplexer cannot hold a lane send up.
const laneSourceBound = 3 * time.Second

// laneNeedleRunes bounds the fingerprint kept in memory for the later look. A
// prefix is a weaker needle than the whole text and so only ever holds more.
const laneNeedleRunes = 1024

const counterRouteGuardModuleReleased = "route.guard.module_released"

// laneNeedle is the fingerprint of a text for laneTailRecords.
func laneNeedle(text string) string {
	n := normalizeForMatch(text)
	if r := []rune(n); len(r) > laneNeedleRunes {
		n = string(r[:laneNeedleRunes])
	}
	return n
}

// quietStatus says no turn is running: a finished turn reads idle or
// waiting_input, and a session at an account wall reads quota_blocked while its
// runtime does nothing. Nothing is running that could still take a queued message.
func quietStatus(s fleet.Status) bool {
	return idleStatus(s) || s == fleet.StatusQuotaBlocked
}

// laneTranscript locates the session's transcript and its current size. ok=false
// means it could not be established, never that there is none.
func (d *Driver) laneTranscript(ctx context.Context, ref fleet.SessionRef) (transcriptSource, bool) {
	ctx, cancel := context.WithTimeout(ctx, laneSourceBound)
	defer cancel()
	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return transcriptSource{}, false
	}
	for i := range rows {
		if rows[i].session == ref.ID && !rows[i].dead {
			return d.resolveTranscriptSource(ctx, ref, &rows[i])
		}
	}
	return transcriptSource{}, false
}

// laneWriteNeverArrived reports whether the session's own records prove that an
// unconfirmed lane write did not arrive, so the hold on its text may be let go.
// Every doubt answers false.
func (d *Driver) laneWriteNeverArrived(ctx context.Context, req fleet.Request, ref fleet.SessionRef, e unconfirmedEntry) bool {
	if e.Partial || e.TranscriptPath == "" || e.needle == "" {
		return false
	}
	if d.now().Sub(e.At) < laneReleaseSettle {
		return false
	}
	// The same conversation: a /clear or a replaced file says nothing about the
	// one the write was aimed at. (That it has not shrunk is laneTailRecords's.)
	if src, ok := d.laneTranscript(ctx, ref); !ok || src.path != e.TranscriptPath {
		return false
	}
	// The session is read BEFORE the transcript: a turn that starts between the
	// two reads is then seen by the scan, never missed by it.
	st, err := d.State(ctx, req, ref)
	if err != nil || !quietStatus(st.Status) || st.WaitingOn == fleet.WaitingPrompt {
		return false
	}
	recorded, err := laneTailRecords(e.TranscriptPath, e.TranscriptOffset, e.needle)
	if err != nil {
		d.counters.incr(counterTranscriptScannerUnreadable)
		return false
	}
	return !recorded
}

// releaseLaneHold is the last look before a lane write's hold stands. It reports
// whether the hold was let go; the entry is dropped when it was.
func (d *Driver) releaseLaneHold(ctx context.Context, req fleet.Request, ref fleet.SessionRef, key string, e unconfirmedEntry) bool {
	if !d.laneWriteNeverArrived(ctx, req, ref, e) {
		return false
	}
	d.dropUnconfirmed(ref.ID, key)
	d.counters.incr(counterRouteGuardModuleReleased)
	log.Printf("route: session %s: a write to module %q's lane from %s was never recorded in the session's transcript; its hold is released and a resend goes out",
		ref.ID, e.Module, e.At.UTC().Format(time.RFC3339))
	return true
}

// errPartialTranscriptLine says the last line of the transcript is not complete
// JSON: the runtime is writing it, so nothing can be said about what it holds.
var errPartialTranscriptLine = errors.New("the transcript's last line is not complete")

// laneTailRecords reads path from offset to the end, once, and reports whether
// any entry other than the assistant's own output carries needle (a laneNeedle).
// An error — an unreadable file, one shorter than offset, a line too long to
// read, an unfinished last line — is "could not tell", never "no".
func laneTailRecords(path string, offset int64, needle string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < offset {
		return false, errors.New("the transcript is shorter than it was")
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	unfinished := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		obj, ok := parseTranscriptObject(line)
		unfinished = !ok
		if !ok {
			continue
		}
		if t, _ := obj["type"].(string); t == "assistant" {
			continue
		}
		if walkStrings(obj, func(s string) bool { return strings.Contains(normalizeForMatch(s), needle) }) {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	if unfinished {
		return false, errPartialTranscriptLine
	}
	return false, nil
}
