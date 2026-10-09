package tmux

// The send half of an external delivery module (#185): choosing whether a send
// goes to the session's module lane, delivering it, and turning the module's
// answer into a receipt that says no more than was established.
//
// # What the driver decides, and what the module does
//
// Send has already made every decision about the REQUEST — the per-session
// lock, sanitising, the runtime-syntax guard, contradictory flags, the route's
// closed set, the cross-path ledger, the sender label — before this file runs.
// Nothing here weakens any of them: the module receives the LABELLED text, so a
// message from anyone but a human relay begins with its sender's label and can
// never begin with `!` or `/`; the module enforces its own floor as well, and
// the two are defence in depth, not one wall.
//
// # Which sends may use a module
//
//	route            who                          module?
//	auto             any sender (#257)            yes — the live lane first; the
//	                                              built-in path only when the
//	                                              lane was not live (the inbox
//	                                              is never tried for auto)
//	terminal         anyone                       never; refused on a live lane
//	                                              for a caller's /input
//	inbox            —                            never
//	<module name>    anyone eligible for it       that module or a refusal
//
// # The rule the outcomes keep
//
// A send goes down ONE path. A module that has written nothing may hand the
// send back (Declined) and the built-in path then carries it — for `auto` only;
// a forced route is refused, never downgraded. Once a byte has reached the
// module the send is its own: it ends `queued` (confirmed), `refused` (the
// runtime rejected it) or `unknown`, and an `unknown` is remembered so the
// retry a caller is invited to make cannot deliver the text a second time by
// any path (the same ledger #184 keeps for the inbox).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/delivery/modclient"
	"github.com/futurelastic/muster/internal/driver"
)

// confirmSlice bounds one confirm call's module-side wait: shorter than the
// client's per-request deadline, so a slow module surfaces as a verdict and not
// as a missed deadline that would mark the whole module suspect.
const confirmSlice = 8 * time.Second

// confirmReserve is held back from the caller's deadline so the receipt can be
// written before the caller gives up.
const confirmReserve = time.Second

const counterRouteGuardModuleUnconfirmed = "route.guard.module_unconfirmed"

// #264: sends a module enqueued on an idle session that never started a turn, and
// the lanes degraded for it.
const (
	counterLaneStall         = "lane.stall"
	counterLaneStallDegraded = "lane.stall_degraded"
)

// idleStatus is the two statuses that say no turn is running and none is being
// answered: a finished turn reads idle or waiting_input depending on how the
// screen settled.
func idleStatus(s fleet.Status) bool {
	return s == fleet.StatusIdle || s == fleet.StatusWaitingInput
}

// idleBeforeSend reports whether the last observation of the session was idle,
// and since when. No observation reads as not idle: the check
// then never fires, which keeps the lane rather than degrade it on a guess.
func (d *Driver) idleBeforeSend(id string) (bool, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.observed[id]
	// A pane whose latest read observed nothing (gapSince) is not known idle:
	// its kept status is the last real one, which may be stale (#278).
	if !ok || !idleStatus(o.status) || !o.gapSince.IsZero() {
		return false, time.Time{}
	}
	return true, o.statusSince
}

// stillIdleSince re-reads the session and reports whether it is still idle with
// the same `since`. A dialog waiting on an answer is not idle: a message queued
// behind one is ordinary. A failed read says no.
func (d *Driver) stillIdleSince(ctx context.Context, req fleet.Request, ref fleet.SessionRef, since time.Time) bool {
	st, err := d.State(ctx, req, ref)
	if err != nil || st.Since == nil {
		return false
	}
	return idleStatus(st.Status) && st.WaitingOn != fleet.WaitingPrompt && st.Since.Equal(since)
}

// extModule is one external module seen through the delivery seam.
type extModule struct {
	d    *Driver
	h    *moduleHost
	name string
}

var _ delivery.Module = extModule{}

func (m extModule) Name() string { return m.name }

// ReservedEnv is empty: a module reserves prefixes, which it declares itself
// and Driver.ReservedEnvPrefixes reports.
func (m extModule) ReservedEnv() []string { return nil }

// moduleTrace is what one delivery left behind beyond its receipt: enough to
// look again later.
type moduleTrace struct {
	laneKey string
	sendID  string
	// lost says the send frame may have been partly or wholly written and no
	// answer came, so nothing can be asked about it afterwards.
	lost bool
}

func (m extModule) Deliver(ctx context.Context, in delivery.Delivery) (delivery.Result, error) {
	var tr moduleTrace
	return m.deliver(ctx, in, &tr)
}

// deliver runs one send against the session's lane. It returns a Result with
// Declined set when it wrote nothing; otherwise the Result carries the receipt.
func (m extModule) deliver(ctx context.Context, in delivery.Delivery, tr *moduleTrace) (delivery.Result, error) {
	_, key, c, why := m.h.liveLane(in.Ref.ID, m.name)
	if why != "" {
		return delivery.Result{Declined: why}, nil
	}
	tr.laneKey = key

	// The module's own floor forbids a leading `!`; this driver's slash policy
	// lets a leading `/` through only for a human relay, which is a person at
	// a keyboard. Everyone else's text is labelled before it gets here.
	allowSlash := in.Opts.HumanRelay && strings.HasPrefix(strings.TrimLeftFunc(in.Text, runtimeTrimCutset), "/")

	res, err := c.Send(ctx, modclient.SendArgs{LaneKey: key, Text: in.Text, AllowLeadingSlash: allowSlash})
	if err != nil {
		return m.sendFailed(in.Ref.ID, err, tr), nil
	}
	if !res.Written {
		return delivery.Result{Declined: fmt.Sprintf("module %q reported it wrote nothing", m.name)}, nil
	}
	m.h.count(m.name, "send_written")
	tr.sendID = res.SendID

	// #264: whether the session was idle when this send went in. An enqueue is
	// ordinary on a busy session; only an idle one that stays idle says the
	// lane takes messages and never starts a turn.
	idleBefore, sinceBefore := m.d.idleBeforeSend(in.Ref.ID)

	verdict, err := m.awaitVerdict(ctx, c, key, res)
	switch {
	case err != nil:
		// The message is in the module's hands and nothing can be said about
		// what became of it. Never resent, on any path.
		m.h.count(m.name, "send_unknown")
		m.h.degrade(in.Ref.ID, "a confirm request got no answer", false)
		tr.lost = errors.Is(err, modclient.ErrLost)
		return unknownResult(m.name, "the module did not answer the confirm request"), nil
	case verdict == "confirmed":
		m.h.count(m.name, "send_confirmed")
		m.h.clearStalls(in.Ref.ID)
		// The SAME bar as the built-in transcript confirmation: the runtime
		// accepted a user-origin turn. That is not an acknowledgement from the
		// agent, so it is never reported as submitted.
		return delivery.Result{
			Receipt: fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeQueued,
				Reason: "the delivery module confirmed the runtime accepted this message as a user turn; " +
					"the agent has not acknowledged it",
			},
			Class:       delivery.Delivered,
			ConfirmedBy: delivery.SignalModule,
		}, nil
	case verdict == "rejected":
		m.h.count(m.name, "send_rejected")
		m.h.degrade(in.Ref.ID, "the runtime rejected a message the module delivered", false)
		return delivery.Result{
			Receipt: fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "the runtime rejected the message the delivery module delivered, so the session did not " +
					"take it. The lane is marked degraded and later sends use the built-in path until it is " +
					"re-attached",
			},
			Class: delivery.Refused,
		}, nil
	case verdict == "queued":
		// Enqueued; the turn had not started when the window ended. One of
		// these is not a fault of the lane, so it is not degraded by itself.
		m.h.count(m.name, "send_unknown")
		why := "the module saw the message enqueued but the turn had not started when the window ended"
		if idleBefore && m.d.stillIdleSince(ctx, in.Req, in.Ref, sinceBefore) {
			// #264: the session was idle before the send and still is, with the
			// same `since`: nothing started. Repeated, the lane is a path that
			// takes messages and delivers none, and as the only input path
			// (#257) it would make the session unreachable while it looks
			// healthy. After laneStallLimit in a row it is treated as not live,
			// so the next send takes the built-in path.
			m.d.counters.incr(counterLaneStall)
			if n, degraded := m.h.noteStall(in.Ref.ID); degraded {
				m.d.counters.incr(counterLaneStallDegraded)
				why += fmt.Sprintf(". This is the %dth enqueued send on this idle session with no turn started, so the "+
					"lane is now marked not live and the next send takes the built-in path. The messages already "+
					"enqueued may still arrive later", n)
			} else {
				why += ". The session was idle before the send and still is"
			}
		} else {
			m.h.clearStalls(in.Ref.ID)
		}
		return unknownResult(m.name, why), nil
	default: // silent
		m.h.count(m.name, "send_unknown")
		m.h.degrade(in.Ref.ID, "the module saw no evidence the runtime took a message", false)
		return unknownResult(m.name, "the module saw no evidence the runtime took the message"), nil
	}
}

// awaitVerdict returns the module's verdict for a send it wrote: confirmed and
// rejected are final; queued and silent are not, so it asks again until the
// caller's window (less a reserve) ends and returns what the last answer was.
func (m extModule) awaitVerdict(ctx context.Context, c *modclient.Client, key string, sent modclient.SendResult) (string, error) {
	if sent.Confirm != nil && sent.Confirm.Final {
		return sent.Confirm.Verdict, nil
	}
	end := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		end = dl
	}
	end = end.Add(-confirmReserve)
	last := "silent"
	if sent.Confirm != nil && sent.Confirm.Verdict != "" {
		last = sent.Confirm.Verdict
	}
	for {
		remaining := time.Until(end)
		if remaining <= 0 {
			return last, nil
		}
		wait := remaining
		if wait > confirmSlice {
			wait = confirmSlice
		}
		r, err := c.Confirm(ctx, modclient.ConfirmArgs{LaneKey: key, SendID: sent.SendID, WaitMs: int(wait / time.Millisecond)})
		if err != nil {
			return "", err
		}
		if r.Final || r.Verdict == "confirmed" || r.Verdict == "rejected" {
			return r.Verdict, nil
		}
		last = r.Verdict
	}
}

// sendFailed maps a failed `send` call. Nothing written means the send is
// handed back; anything that may have written means unknown, and never resent.
func (m extModule) sendFailed(id string, err error, tr *moduleTrace) delivery.Result {
	var me *modclient.Error
	switch {
	case errors.As(err, &me):
		switch me.Code {
		case "not-live", "unknown-lane", "unsupported-version", "version-unknown":
			// Nothing was written. `unknown-lane` says the module has lost the
			// lane entirely; the others say it cannot take this send now.
			m.h.degrade(id, "the module answered "+me.Code, me.Code == "unknown-lane")
			return delivery.Result{Declined: fmt.Sprintf("module %q answered %s", m.name, sanitizeModuleText(me.Code, 40))}
		case "refused", "too-large", "bad-request":
			// The same refusals the built-in path would give.
			return delivery.Result{
				Receipt: fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: fmt.Sprintf("the delivery module refused the message (%s): %s. Nothing was written",
						sanitizeModuleText(me.Code, 40), sanitizeModuleText(me.Message, 200)),
				},
				Class: delivery.Refused,
			}
		}
		// write-failed, internal, unknown-send, or a code this build does not
		// know: the frame may have been partly written. Unknown, no resend.
		m.h.count(m.name, "send_unknown")
		m.h.degrade(id, "the module answered "+sanitizeModuleText(me.Code, 40)+" to a send", false)
		return unknownResult(m.name, "the module could not say whether the message was written ("+
			sanitizeModuleText(me.Code, 40)+")")
	case errors.Is(err, modclient.ErrNotSent):
		return delivery.Result{Declined: fmt.Sprintf("module %q could not be reached", m.name)}
	default:
		// ErrLost, a missed deadline, anything else: bytes may have gone out.
		tr.lost = true
		m.h.count(m.name, "send_unknown")
		m.h.degrade(id, "a send got no answer", false)
		return unknownResult(m.name, "the module did not answer the send")
	}
}

func unknownResult(module, why string) delivery.Result {
	return delivery.Result{
		Receipt: fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: fmt.Sprintf("the message was written to delivery module %q's lane and could not be confirmed: %s. "+
				"It has NOT been sent again on any path: sending it again could deliver it twice. Read the "+
				"session's transcript before deciding; this holds for %s, and text that differs is not held",
				module, why, strandedRetention),
		},
		Class: delivery.Unknown,
	}
}

// --- the send hook --------------------------------------------------------

// moduleRouteChoice reports whether this send may use a module and, when the
// caller forced one, which. It is the whole eligibility table above.
func (d *Driver) moduleRouteChoice(route fleet.Route, opts driver.SendOptions) (forced string, eligible bool) {
	if d.mods == nil {
		return "", false
	}
	if d.deliveryModuleEnabled(string(route)) {
		return string(route), true
	}
	composerFree := opts.Submit && !opts.ResumeIfStranded && !opts.ReplaceIfStranded
	// #257: auto is eligible for every sender; an explicit terminal never is.
	return "", route == fleet.RouteAuto && composerFree
}

// sendViaModule is Send's module step, run after the inbox and before the
// built-in path. handled=false means the built-in path carries this send:
// nothing was written. text is the caller's sanitised text (the ledger's key is
// computed from it, as the inbox's is); labelled is what the module receives.
func (d *Driver) sendViaModule(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text, labelled string, opts driver.SendOptions, forced, pinned string) (fleet.DeliveryReceipt, bool) {
	h := d.mods
	refuse := func(module, why string) fleet.DeliveryReceipt {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  fmt.Sprintf("route %q was requested but %s. Nothing was written", module, why),
		}.WithModule(module)
	}

	// The composer already holds this very text as a stranded terminal
	// delivery of ours: a module would deliver it once and leave a copy waiting
	// to be submitted — twice. Auto goes on to the terminal, whose stranded-
	// record rules decide; a forced module is refused, never sent to the
	// terminal for it.
	if d.terminalUnconfirmed(ref.ID, labelled) {
		d.counters.incr(counterRouteGuardTerminalUnconfirm)
		if forced != "" {
			return refuse(forced, "an earlier terminal delivery of this same text is unconfirmed and still in the session's composer, so it was not also sent through the module"), true
		}
		if pinned != "" {
			// #257: the lane is live, so the terminal is not a path for this send
			// either, and resumeIfStranded is refused on a live lane. The text is
			// sitting in the composer; the caller clears it and sends again.
			d.counters.incr(counterRouteRefusedLaneLive)
			d.counters.incr(counterRouteRefusedLaneLivePrefix + "stranded_text")
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: fmt.Sprintf("this session's delivery lane (module %q) is live, but an earlier terminal delivery "+
					"of this same text is unconfirmed and still in the session's composer, so it was not also sent "+
					"through the lane. Clear the composer with discard (expect) and send again. Nothing was written", pinned),
			}, true
		}
		return fleet.DeliveryReceipt{}, false
	}

	name := forced
	if name == "" {
		name = h.laneModule(ref.ID)
		if name == "" {
			// No lane was ever offered to this session: the built-in path is
			// its only path, and that is not a fallback.
			return fleet.DeliveryReceipt{}, false
		}
	}
	mod := extModule{d: d, h: h, name: name}
	var tr moduleTrace
	res, err := mod.deliver(ctx, delivery.Delivery{Req: req, Ref: ref, Text: labelled, Opts: opts}, &tr)
	if err != nil {
		d.counters.incr(delivery.CounterPrefix + name + ".error")
		if pinned != "" {
			return laneLost(d, pinned, "the lane returned an error"), true
		}
		return fleet.DeliveryReceipt{}, false
	}
	if res.Declined != "" {
		if forced != "" {
			h.count(name, "send_refused_no_live_lane")
			return refuse(forced, "the session's lane cannot take it: "+sanitizeModuleText(res.Declined, 240)), true
		}
		if pinned != "" {
			h.count(name, "send_refused_lane_lost")
			return laneLost(d, pinned, sanitizeModuleText(res.Declined, 240)), true
		}
		h.count(name, "fallback_to_builtin")
		return fleet.DeliveryReceipt{}, false
	}
	delivery.Observe(d.counters.incr, name, res)
	if res.Receipt.Outcome == fleet.OutcomeUnknown {
		snap, _ := h.snapshot(ref.ID)
		d.noteUnconfirmed(ref.ID, unconfirmedEntry{
			Key: deliveryKey(text, opts.From), Cwd: snap.Cwd,
			Module: name, LaneKey: tr.laneKey, SendID: tr.sendID, Partial: tr.lost,
		})
	}
	return res.Receipt.WithModule(name), true
}

// laneLost is the refusal for a send that read a live lane at the gate (#257)
// and then found it unusable before a byte was written. The lane is the
// session's only input path while it is live, so the send is refused, not
// carried by the terminal; whatever degraded the lane makes the next send see
// no live lane and take the terminal honestly.
func laneLost(d *Driver, module, why string) fleet.DeliveryReceipt {
	d.counters.incr(counterRouteRefusedLaneLost)
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeRefused,
		Reason: fmt.Sprintf("this session's delivery lane (module %q) stopped being usable while this send was in "+
			"flight (%s). Send again. Nothing was written", module, why),
	}.WithModule(module)
}

// laneModule is the module that holds a usable-in-principle lane for this
// session ("" when none was ever offered, or it is closing). Whether the lane
// is live right now is liveLane's question; this only says a lane exists, so a
// send that could not use it is counted as a fallback rather than not counted.
func (h *moduleHost) laneModule(id string) string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.lanes[id]
	if rec == nil || rec.Module == "" || rec.LaneKey == "" || rec.Closing {
		return ""
	}
	return rec.Module
}

func (h *moduleHost) client(name string) *modclient.Client {
	if h == nil {
		return nil
	}
	return h.clients[name]
}

// answerFromModuleLedger is the ledger's answer for an earlier send that went
// to a module's lane and could not be confirmed. The module is asked once, with
// no wait: confirmed means the text arrived after all (answered as queued, the
// same bar as a live confirmation, and not sent again); rejected means it did
// not, and the send proceeds; anything else holds — nothing is written on
// either path.
func (d *Driver) answerFromModuleLedger(ctx context.Context, ref fleet.SessionRef, key string, e unconfirmedEntry) (fleet.DeliveryReceipt, bool) {
	held := func(why string) (fleet.DeliveryReceipt, bool) {
		d.counters.incr(counterRouteGuardModuleUnconfirmed)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: fmt.Sprintf("an earlier send of this same text was written to delivery module %q's lane at %s and "+
				"could not be confirmed (%s). It has NOT been sent again on any path: sending it again could "+
				"deliver it twice. Read the session's transcript before deciding; this holds for %s or until the "+
				"message is recorded, and text that differs is not held",
				e.Module, e.At.UTC().Format(time.RFC3339), why, strandedRetention),
		}.WithModule(e.Module), true
	}
	c := d.mods.client(e.Module)
	if c == nil || !c.Usable() {
		return held("the module cannot be asked right now")
	}
	if e.SendID == "" || e.LaneKey == "" {
		return held("no send handle was kept for it")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := c.Confirm(ctx, modclient.ConfirmArgs{LaneKey: e.LaneKey, SendID: e.SendID})
	if err != nil {
		return held("the module could not say: " + moduleErrorClass(err))
	}
	switch r.Verdict {
	case "confirmed":
		d.dropUnconfirmed(ref.ID, key)
		d.counters.incr(counterRouteGuardLateConfirmed)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeQueued,
			Reason: "an earlier send of this same text was written to the session's module lane and the runtime has " +
				"since accepted it; it was not sent again",
		}.WithModule(e.Module), true
	case "rejected":
		d.dropUnconfirmed(ref.ID, key)
		return fleet.DeliveryReceipt{}, false
	}
	return held("the module has seen no evidence it arrived")
}
