package tmux

// This file is muster #184's routing layer: which path a send takes, what
// is counted about it, and the ledger that keeps one message from arriving
// twice when the two paths meet.
//
// # The rule the ledger exists to keep
//
// A send goes down ONE path. The inbox may decline before it has written a
// byte (a NAMED inbox, since #257, is then refused rather than carried by the
// terminal; `auto` never tries the inbox). Once ANY byte has reached the inbox the
// message may be in the receiver's hands, and from then on the same text must
// not be sent down the other path. Inside a single Send that is a matter of
// control flow. Across Sends it is not: a send that ends `unknown` invites its
// caller to try again, and the documented way to try again (`resumeIfStranded`)
// is a terminal operation. Left alone, a retry of an inbox `unknown` would
// paste the same text into the composer, and the receiver would take it twice.
//
// So every inbox write that could not be confirmed leaves an entry in a small
// ledger, and every Send consults it before it chooses a path. An entry is
// evidence about ONE delivery — the same text from the same sender to the same
// session — and lapses after strandedRetention, like a stranded record. It
// holds digests, never the message.
//
// The opposite direction needs no ledger of its own: a terminal send that
// could not be confirmed already leaves a stranded record (noteStranded), and a
// later send of the same text finds it (terminalUnconfirmed).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/inboxclient"
)

// unconfirmedPerSession bounds the ledger: a session that is sent many
// distinct unconfirmed messages keeps only the most recent ones, which are the
// ones a caller could still be retrying.
const unconfirmedPerSession = 8

// unconfirmedEntry is what is remembered about one inbox write that could not
// be confirmed.
type unconfirmedEntry struct {
	// Key identifies the delivery: a digest of the text, who the sender said it
	// is (never the machine the request entered through) and whether the sender
	// declared a human relay (deliveryKey).
	Key string `json:"key"`
	// Cwd corroborates the session (§5.4): an id is recyclable, and an entry
	// made for one session must never gate an unrelated one that reused the
	// name.
	Cwd string    `json:"cwd"`
	At  time.Time `json:"at"`
	// TranscriptPath and TranscriptOffset are where the delivery would have been
	// recorded and the size the file had before the write — enough to look again
	// later, which is how a follow-up learns the message did arrive after all.
	TranscriptPath   string `json:"transcriptPath,omitempty"`
	TranscriptOffset int64  `json:"transcriptOffset,omitempty"`
	// Probe fingerprints the delivery for that later look.
	Probe inboxProbe `json:"probe"`
	// Partial is true when the write itself broke part-way, as opposed to a
	// complete write nothing recorded.
	Partial bool `json:"partial,omitempty"`

	// Module, LaneKey and SendID are set instead of the transcript fields when
	// the unconfirmed write went to an external delivery module's lane (#185):
	// they are how the module is asked, later, whether it arrived. LaneKey is
	// the module's opaque handle and is never put in a path.
	Module  string `json:"module,omitempty"`
	LaneKey string `json:"laneKey,omitempty"`
	SendID  string `json:"sendId,omitempty"`
}

// deliveryKey identifies one delivery for the ledger. The text is the
// sanitised text the driver acts on; the sender is who the caller said it is
// (agent and session), in the label's normalised form, so a retry that repeats
// the same `from` matches and a message from a different sender does not. The
// relay declaration is part of the key because it changes the body that was
// written.
//
// The machine the request entered through is deliberately NOT part of the key
// (#191). The service stamps it, so it names where a request arrived and not who
// sent it, and it is exactly what changes when a caller retries: a client that
// fails over to the peer enters through a different machine with the same text
// and the same identity. Keyed on the label as the receiver sees it, that retry
// would miss the entry, and an inbox that then declined with nothing written
// would let the terminal carry the message a second time. The cost of leaving
// the machine out is on the safe side: two callers that give the same identity
// and send the same text to the same session inside the retention window are
// held as one, which reads as "not sent again" rather than as a duplicate.
//
// Whether the message carried a label at all stays in the key, so a sender that
// named nothing and was labelled by the service (which leaves only the machine,
// dropped above) is not confused with an unlabelled one — a human relay's words
// go to the terminal with no label and must never be held for an anonymous
// peer's identical text.
func deliveryKey(text string, from *fleet.MessageFrom) string {
	h := sha256.New()
	h.Write([]byte(text))
	h.Write([]byte{0})
	h.Write([]byte(inboxclient.SenderName(driver.SenderLabel(withoutMachine(from)))))
	h.Write([]byte{0})
	if driver.SenderLabel(from) != "" {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write([]byte{0})
	if from != nil && from.RelayOfHuman {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// withoutMachine returns from with the machine the request entered through
// removed, leaving what the caller said about itself. A copy: the caller's
// value is also what the send itself labels the message with, and that keeps
// the machine.
func withoutMachine(from *fleet.MessageFrom) *fleet.MessageFrom {
	if from == nil {
		return nil
	}
	id := *from
	id.Machine = ""
	return &id
}

// noteUnconfirmed records an inbox write that could not be confirmed. An entry
// for the same delivery already there is replaced, so the ledger never holds
// two answers about one message.
func (d *Driver) noteUnconfirmed(id string, e unconfirmedEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unconfirmed == nil {
		d.unconfirmed = map[string][]unconfirmedEntry{}
	}
	e.At = d.now()
	list := d.unconfirmed[id][:0:0]
	for _, old := range d.unconfirmed[id] {
		if old.Key != e.Key {
			list = append(list, old)
		}
	}
	list = append(list, e)
	if len(list) > unconfirmedPerSession {
		list = list[len(list)-unconfirmedPerSession:]
	}
	d.unconfirmed[id] = list
	d.saveStrandedLocked()
	if e.Module != "" {
		d.counters.incr(counterModuleLedgerEntryWritten)
	} else {
		d.counters.incr(counterInboxLedgerEntryWritten)
	}
}

// unconfirmedFor returns the live entry for this delivery, if there is one,
// after dropping every entry that has lapsed.
func (d *Driver) unconfirmedFor(id, key string) (unconfirmedEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepUnconfirmedLocked()
	for _, e := range d.unconfirmed[id] {
		if e.Key == key {
			return e, true
		}
	}
	return unconfirmedEntry{}, false
}

// dropUnconfirmed forgets the entry for this delivery: its message was
// recorded after all, or the session it described is gone.
func (d *Driver) dropUnconfirmed(id, key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.unconfirmed[id]
	kept := list[:0:0]
	for _, e := range list {
		if e.Key != key {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(list) {
		return
	}
	if len(kept) == 0 {
		delete(d.unconfirmed, id)
	} else {
		d.unconfirmed[id] = kept
	}
	d.saveStrandedLocked()
}

// sweepUnconfirmedLocked drops entries older than strandedRetention. Caller
// holds d.mu. It reports whether it changed anything so a loader can persist.
func (d *Driver) sweepUnconfirmedLocked() bool {
	changed := false
	now := d.now()
	for id, list := range d.unconfirmed {
		kept := list[:0:0]
		for _, e := range list {
			if now.Sub(e.At) <= strandedRetention {
				kept = append(kept, e)
			}
		}
		if len(kept) != len(list) {
			changed = true
			if len(kept) == 0 {
				delete(d.unconfirmed, id)
			} else {
				d.unconfirmed[id] = kept
			}
		}
	}
	return changed
}

// answerFromLedger is the guard every Send runs before it chooses a path. When
// an earlier inbox write of this same delivery is still unconfirmed, nothing is
// written on either path: the transcript is looked at again, and the answer is
// either "it arrived" or "still unknown — not sent again". ok=false means there
// is no such entry and the Send proceeds normally.
//
// The session is looked up first, for §5.4: an entry whose working directory no
// longer matches describes a session that has since been replaced under the same
// name, and gates nothing. If the multiplexer cannot be read the entry is
// honoured — refusing a possible duplicate is the cheap side to be wrong on.
func (d *Driver) answerFromLedger(ctx context.Context, ref fleet.SessionRef, key string) (fleet.DeliveryReceipt, bool) {
	entry, ok := d.unconfirmedFor(ref.ID, key)
	if !ok {
		return fleet.DeliveryReceipt{}, false
	}
	if cwd, known, err := d.sessionCwd(ctx, ref.ID); err == nil {
		if !known || cwd != entry.Cwd {
			d.dropUnconfirmed(ref.ID, key)
			return fleet.DeliveryReceipt{}, false
		}
	}

	if entry.Module != "" {
		// #185: the earlier write went to a module's lane; the module, not a
		// transcript this driver reads, is who can say whether it arrived.
		return d.answerFromModuleLedger(ctx, ref, key, entry)
	}

	if entry.TranscriptPath != "" {
		res, err := inboxTailScan(entry.TranscriptPath, entry.TranscriptOffset, entry.Probe)
		if err != nil {
			d.counters.incr(counterTranscriptScannerUnreadable)
		}
		if res != inboxScanSilent {
			d.dropUnconfirmed(ref.ID, key)
			d.counters.incr(counterRouteGuardLateConfirmed)
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeDelivered,
				Reason: "an earlier send of this same text was written to the session's inbox and the " +
					"receiver's transcript has since recorded it; it was not sent again",
			}.WithRoute(fleet.RouteInbox), true
		}
	}
	d.counters.incr(counterRouteGuardInboxUnconfirmed)
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason: fmt.Sprintf("an earlier send of this same text was written to the session's inbox at %s and the "+
			"receiver's transcript has not recorded it. It has NOT been sent again on any path: sending it "+
			"again could deliver it twice. Read the session's transcript before deciding; this holds for %s "+
			"or until the message is recorded, and text that differs is not held",
			entry.At.UTC().Format(time.RFC3339), strandedRetention),
	}.WithRoute(fleet.RouteInbox), true
}

// sessionCwd reports the working directory of session id. known=false means the
// multiplexer answered and the session is not there.
func (d *Driver) sessionCwd(ctx context.Context, id string) (cwd string, known bool, err error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return "", false, err
	}
	for _, r := range rows {
		if r.session == id {
			return r.cwd, true, nil
		}
	}
	return "", false, nil
}

// terminalUnconfirmed reports whether the session's composer already holds THIS
// text as a stranded delivery of ours. While it does, sending the same text down
// the inbox would deliver it once as a peer message and leave a copy waiting in
// the composer for someone to submit — twice.
//
// The comparison is the labelled text, because that is what a stranded record
// stores. The session's working directory is deliberately not corroborated here:
// a mismatch means the id was recycled, and the only cost of treating that as a
// hit is that an auto send takes the terminal path it would have taken with no
// inbox at all.
func (d *Driver) terminalUnconfirmed(id, labelled string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepStrandedLocked()
	rec, ok := d.stranded[id]
	return ok && rec.Text == labelled
}

// observeRoute counts one finished Send under the route.* family, exactly once.
func (d *Driver) observeRoute(requested fleet.Route, r fleet.DeliveryReceipt, err error) {
	if requested == "" {
		requested = fleet.RouteAuto
	}
	taken := string(r.RouteOf())
	switch {
	case err != nil:
		taken = "error"
	case taken == "":
		taken = "none"
	}
	d.counters.incr("route.decided." + string(requested) + "." + taken)
	if err == nil && r.Delivery != nil && r.Outcome != "" {
		d.counters.incr("route.outcome." + taken + "." + string(r.Outcome))
	}
}

// routeNote carries what a Send decided about its path that the receipt does
// not say (#368). Send allocates one per call and the routing steps fill it in;
// logSend reads it once the call has finished. It exists because the reason a
// send took the built-in path lives where the decision is made — a lane that
// declined, a shape no lane can take — and the receipt of a send the built-in
// path then carried names the path, not why the lane did not. The zero value
// says nothing, and every method is safe on a nil note, so a path that does not
// care never has to check.
type routeNote struct {
	// reason is why the send took the path it took, in a sentence a person
	// reading a log can act on. Empty means the receipt says it all.
	reason string
	// fallback is true when a lane was offered to this session and the send was
	// handed back to the built-in path with nothing written — the case a
	// per-send record exists to make traceable (#368). A session that never had a
	// lane, or a send whose shape no lane can take, is not a fallback.
	fallback bool
}

// path records why the send is not on a lane. It is not a fallback.
func (n *routeNote) path(reason string) {
	if n != nil {
		n.reason = reason
	}
}

// fellBack records that the lane was offered, handed the send back with nothing
// written, and the built-in path carried it instead.
func (n *routeNote) fellBack(reason string) {
	if n != nil {
		n.reason, n.fallback = reason, true
	}
}

// sendLogReasonMax bounds the reason and the detail in a per-send log line. A
// refusal carries a paragraph of advice meant for a caller; a log line needs
// only enough to find the cause.
const sendLogReasonMax = 200

// logSend writes one line for a finished Send: which session, which path it
// asked for, which path it took, what came of it, and why (#368). It is the
// per-send half of what observeRoute counts in total — the counters say how
// many sends fell back, this line says which session's did.
//
// `reason` is why the send was on this path. `detail` is added only when the
// send did not end well (refused, or unknown) and is the receipt's own words,
// so a refusal and the path that was never taken are both readable from the
// line. The text of the message is never logged, and neither is who sent it:
// the line is about the path, and a log is read by more people than a receipt is.
func (d *Driver) logSend(ref fleet.SessionRef, requested fleet.Route, r fleet.DeliveryReceipt, err error, n *routeNote) {
	if requested == "" {
		requested = fleet.RouteAuto
	}
	taken := string(r.RouteOf())
	if m := r.ModuleOf(); m != "" {
		taken = string(fleet.RouteModule) + "/" + m
	}
	outcome := string(r.Outcome)
	var note routeNote
	if n != nil {
		note = *n
	}
	reason, detail := note.reason, ""
	switch {
	case err != nil:
		taken, outcome = "error", "error"
		detail = err.Error()
	case taken == "":
		taken = "none"
	}
	if err == nil && (r.Outcome == fleet.OutcomeRefused || r.Outcome == fleet.OutcomeUnknown) {
		detail = r.Reason
	}
	if reason == "" {
		switch {
		case r.ModuleOf() != "":
			reason = "the session's delivery lane was live"
		case r.RouteOf() == fleet.RouteTerminal:
			reason = "the built-in path carried it"
		case r.RouteOf() == fleet.RouteInbox:
			reason = "the inbox route was named"
		default:
			reason = "no path was taken"
		}
	}
	line := fmt.Sprintf("tmux: send session=%s machine=%s requested=%s route=%s outcome=%s fallback=%t reason=%q",
		ref.ID, d.machine, requested, taken, outcome, note.fallback, sanitizeModuleText(reason, sendLogReasonMax))
	if detail != "" {
		line += fmt.Sprintf(" detail=%q", sanitizeModuleText(detail, sendLogReasonMax))
	}
	log.Print(line)
}
