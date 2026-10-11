package tmux

import (
	"time"

	fleet "github.com/futurelastic/muster"
)

// Remembering what a create asked for and what became of it (muster
// #84, #85, #86), so a later listing — and the create response itself — can
// report what was APPLIED rather than only what was REQUESTED.
//
// # Why one record, not three
//
// #84 (a dropped pin reported as applied), #85 (a runtime-hosted surface
// with no field), and #86 (a create-time prompt with no delivery receipt)
// are three fields on the same Session, filled from the same create. A
// caller reading the 201 body and the first 200 body of one session must
// see the same answer for all three, and the only way to guarantee that is
// for both to be computed from the same stored fact — one write in Create,
// one read in List, not three parallel tables drifting independently.
//
// # Same shape as resumeIntentRecord, for the same reasons
//
// Durable when a state store is configured, in memory otherwise (honest for
// a throwaway instance); keyed on the session id with the cwd carried
// alongside for corroboration (§5.4: an id alone is recyclable); swept by
// age rather than kept forever, because a record nobody has checked in
// createRecordRetention is not evidence about a caller who is still
// watching — the same reasoning resumeIntentRecord's own sweep states.

// createRecordRetention bounds how long this driver remembers a create's own
// facts about itself. A prompt delivery ordinarily resolves within moments
// of creation, and a runtime surface registers shortly after — this only has
// to survive long enough for a caller to check, not forever. Matches
// resumeIntentRetention: both are "long enough for a realistic poll,
// bounded so an abandoned record does not grow the table without limit."
const createRecordRetention = 30 * time.Minute

// createRecord is what noteCreateRecord persists: the create-time facts a
// later List needs to answer #84, #85 and #86 for this session.
type createRecord struct {
	Cwd string    `json:"cwd"`
	At  time.Time `json:"at"`

	// #84: what the create asked to pin. The APPLIED side is never stored
	// here — this driver has no way to read it back (see pinOutcomeFor), and
	// a stored copy of the request sitting next to a field named "applied"
	// would be one more place for a request to be mistaken for a fact.
	Agent  string `json:"agent,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`

	// #85: whether a runtime surface was requested, and whether one has ever
	// been corroborated. SurfaceSeen is latched true and never unset while
	// the record lives — identity, not liveness (see runtimeSurfaceFor).
	SurfaceRequested bool `json:"surfaceRequested,omitempty"`
	SurfaceSeen      bool `json:"surfaceSeen,omitempty"`

	// #86: set when the create carried a prompt. Outcome is empty until
	// delivery resolves; every settleNewSession exit must set it to
	// something (see notePromptDelivered) — a permanently-empty Outcome
	// alongside PromptCarried true is a worse false negative than the one
	// this record exists to close, because it never clears.
	//
	// #125: while Outcome is still empty, PromptEvidence is no longer a
	// single static sentence written once — settleNewSession's poll loop
	// updates it (via notePromptPending) every time the reason the prompt
	// has not landed yet actually changes, so a caller reading this record
	// MID-WAIT sees a live diagnosis ("parked on a dialog awaiting a
	// keypress"), not just the fact that it is still pending. The field is
	// overwritten again, with different meaning, the moment Outcome is set —
	// promptDeliveryFor tells the two eras apart by Outcome alone.
	PromptCarried  bool   `json:"promptCarried,omitempty"`
	PromptOutcome  string `json:"promptOutcome,omitempty"`
	PromptEvidence string `json:"promptEvidence,omitempty"`

	// PromptWaitingOn (muster #126) is the machine-readable class
	// alongside PromptEvidence's prose, written by the SAME call
	// (notePromptPending) that writes the evidence, so the two answers to
	// "why is this prompt still pending" cannot disagree. Same two-era rule
	// as PromptEvidence: meaningful while PromptOutcome is empty, and never
	// written once it is set. Stored as plain string, the same choice
	// PromptOutcome above makes and for the same reason — fleet.WaitingReason
	// carries no round-trip risk either way (it validates nothing), so this
	// is for pattern consistency with its sibling field, not necessity.
	PromptWaitingOn string `json:"promptWaitingOn,omitempty"`
}

// createRecordFile is the durable document, one entry per session with a
// create record still worth checking. Its own file, never folded into
// idempotency.json or resume-intent — a create key, a resume-honesty check
// and this record are three different concerns with three different shapes.
type createRecordFile struct {
	Records map[string]createRecord `json:"records"`
}

const createRecordFileName = "create-record"

// noteCreateRecord records a create's own facts about itself, before there
// is any way yet to tell what became of the pin, the surface, or the
// prompt. Called once, from Create, for every session — even one that asked
// for none of the three, so createRecordFor has cwd to corroborate against
// regardless (matched against, never trusted alone — §5.4).
func (d *Driver) noteCreateRecord(id, cwd string, spec fleet.SessionSpec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createRecords == nil {
		d.createRecords = map[string]createRecord{}
	}
	rec := createRecord{
		Cwd: cwd, At: d.now(),
		Agent: string(spec.Agent), Model: spec.Model, Effort: spec.Effort,
		SurfaceRequested: spec.RemoteControl == nil || *spec.RemoteControl,
		PromptCarried:    spec.Prompt != "",
	}
	d.createRecords[id] = rec
	d.saveCreateRecordsLocked()
}

// createRecordFor reports a session's create record, if one exists, has not
// expired, and was made for the same working directory (§5.4 again: an id
// match alone is not enough — a recycled id must not inherit a stranger's
// record).
func (d *Driver) createRecordFor(id, cwd string) (createRecord, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.createRecordForLocked(id, cwd)
}

// createRecordForLocked is createRecordFor for a caller that already holds
// d.mu — List's own row loop does, and calling the locking form from inside
// it would deadlock against itself (sync.Mutex is not reentrant).
func (d *Driver) createRecordForLocked(id, cwd string) (createRecord, bool) {
	d.sweepCreateRecordsLocked()
	rec, ok := d.createRecords[id]
	if !ok || rec.Cwd != cwd {
		return createRecord{}, false
	}
	return rec, true
}

// notePromptDelivered resolves a create record's pending prompt outcome.
// Called from every settleNewSession/deliverInitialPrompt exit that could
// otherwise leave PromptCarried true with no Outcome — see tmux.go's own
// call sites for the full table of exits this must cover.
func (d *Driver) notePromptDelivered(id string, outcome fleet.Outcome, evidence string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.createRecords[id]
	if !ok {
		// The record already expired or was never made (e.g. a driver built
		// with WithState pointed at a store that lost it). Nothing to
		// resolve; List will report PromptDelivery absent for this session,
		// which is honest — a record this driver no longer holds is
		// indistinguishable from one that was never made.
		return
	}
	rec.PromptOutcome = string(outcome)
	rec.PromptEvidence = evidence
	d.createRecords[id] = rec
	d.saveCreateRecordsLocked()
}

// notePromptPending updates the LIVE reason a still-undelivered prompt has
// not landed yet (muster #125) — called from settleNewSession's poll
// loop every time that reason changes, never when delivery has resolved.
// waitingOn is #126's machine-readable class for the same wait evidence
// describes, computed by the same caller from the same observation; empty is
// a legitimate "unclassified" (see fleet.WaitingReason's own doc), never a
// guess.
//
// Refuses to write once PromptOutcome is set: a stale poll iteration racing
// behind deliverInitialPrompt/the session-gone exit must never overwrite a
// terminal evidence string with a pending one — the two eras of this field
// (documented on createRecord.PromptEvidence) must not blend.
func (d *Driver) notePromptPending(id string, waitingOn fleet.WaitingReason, evidence string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.createRecords[id]
	if !ok || rec.PromptOutcome != "" {
		// Either the record already expired/was never made (same "nothing to
		// resolve" case notePromptDelivered documents), or delivery already
		// reached a terminal outcome and this write is racing behind it.
		return
	}
	rec.PromptEvidence = evidence
	rec.PromptWaitingOn = string(waitingOn)
	d.createRecords[id] = rec
	d.saveCreateRecordsLocked()
}

// noteSurfaceSeenLocked latches SurfaceSeen. Caller holds d.mu. Never unset
// once true — identity, not liveness (see runtimeSurfaceFor's own doc).
func (d *Driver) noteSurfaceSeenLocked(id string) {
	rec, ok := d.createRecords[id]
	if !ok || rec.SurfaceSeen {
		return
	}
	rec.SurfaceSeen = true
	d.createRecords[id] = rec
	d.saveCreateRecordsLocked()
}

// sweepCreateRecordsLocked drops records older than createRecordRetention.
// Caller holds d.mu.
func (d *Driver) sweepCreateRecordsLocked() {
	if len(d.createRecords) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.createRecords {
		if now.Sub(rec.At) > createRecordRetention {
			delete(d.createRecords, id)
		}
	}
}

func (d *Driver) saveCreateRecordsLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save(createRecordFileName, createRecordFile{Records: d.createRecords})
}

// loadCreateRecords restores create records at startup, sweeping anything
// already past createRecordRetention — the same "sweep on load" shape
// loadResumeIntents already uses.
func (d *Driver) loadCreateRecords() {
	if d.store == nil {
		return
	}
	var f createRecordFile
	found, err := d.store.Load(createRecordFileName, &f)
	if err != nil || !found || len(f.Records) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.createRecords = f.Records
	d.sweepCreateRecordsLocked()
	d.saveCreateRecordsLocked()
}

// sessionFactsFor is the one place that turns a createRecordFor/
// createRecordForLocked lookup into the three response fields it feeds —
// Pins, RuntimeSurface, PromptDelivery — so every call site applies the
// same "found" guard. runtimeSurfaceFor, unlike pinOutcomeFor and
// promptDeliveryFor, cannot infer "no record" from its own zero value: a
// zero createRecord and a genuine settled-no both read SurfaceRequested
// false, and only the caller knows which one this is. Calling
// runtimeSurfaceFor directly against an unguarded lookup was exactly this
// bug, caught before it shipped — found this function's own reason to
// exist.
func sessionFactsFor(cr createRecord, found bool, name string) (pins *fleet.PinOutcome, surface *fleet.RuntimeSurfaceRef, prompt *fleet.PromptDelivery) {
	pins = pinOutcomeFor(cr)
	prompt = promptDeliveryFor(cr)
	if found {
		surface = runtimeSurfaceFor(cr, name)
	}
	return
}

// promptDeliveryFor turns a create record into a fleet.PromptDelivery, or
// nil when this create carried no prompt at all — the absent-field state
// §5.7 asks for, never a claim that a prompt was carried and delivered.
//
// #125: while pending, PromptEvidence is preferred over the generic sentence
// below whenever notePromptPending has written a live diagnosis — the
// generic sentence only fires in the brief window between noteCreateRecord
// and this session's very first readiness poll, before any diagnosis exists
// yet to report. #126: PromptWaitingOn travels alongside it unconditionally
// — it is empty ("unclassified") in that same brief window, which is the
// honest answer there too, not a special case to guard.
func promptDeliveryFor(rec createRecord) *fleet.PromptDelivery {
	if !rec.PromptCarried {
		return nil
	}
	if rec.PromptOutcome == "" {
		evidence := rec.PromptEvidence
		if evidence == "" {
			evidence = "the prompt was accepted at creation and has not been " +
				"delivered yet; the session has not finished painting a composer to receive it"
		}
		return fleet.PromptPending(fleet.WaitingReason(rec.PromptWaitingOn), evidence)
	}
	return fleet.PromptDelivered(fleet.Outcome(rec.PromptOutcome), rec.PromptEvidence)
}

// pinUnresolvedEvidence explains, per field, why this driver cannot say
// whether the runtime is honouring a pin it was asked to apply. Not one
// shared sentence: agent and effort have no on-screen trace at all, while
// model is the one case worth a sharper note, because the footer's model
// name LOOKS like it should answer this and does not (muster #84's own
// caution against the unsound inference — the same discipline #65 already
// declined for a different field).
var pinUnresolvedEvidence = map[string]string{
	"agent": "this driver passes the agent pin on the command line and has no " +
		"channel back from the runtime to confirm what it is actually running",
	"model": "this driver passes the model pin on the command line; the runtime's " +
		"own footer names a model in display form, which cannot be soundly compared " +
		"to the value that was passed",
	"effort": "this driver passes the effort pin on the command line and has no " +
		"channel back from the runtime to confirm what it is actually running with",
}

// pinOutcomeFor turns a create record into a fleet.PinOutcome, or nil when
// none of the three pins was requested — the absent-field state §5.7 asks
// for. Every requested pin comes back unresolved: this driver has no way to
// read any of the three back once they are on the command line (see
// pinUnresolvedEvidence), which is a real, standing answer, never a stand-in
// for "dropped" — a value that WOULD have been misread as a flag is refused
// at creation instead (Create's own guard, before this record is even
// written), so every pin reaching this record did reach the argv intact.
func pinOutcomeFor(rec createRecord) *fleet.PinOutcome {
	return pinOutcomeObserving(rec, "")
}

// pinAgentObservedEvidence says where a resolved agent pin's answer came
// from: the runtime's own command line, read back from the process table.
const pinAgentObservedEvidence = "read from the running process's own command line " +
	"(--agent), not from the create request"

// pinOutcomeObserving is pinOutcomeFor for a caller that has also read the
// agent off the session's runtime process (muster #301). An observed value
// resolves the agent pin — honoured when it equals what was requested,
// not honoured when it does not, with the runtime's value as Applied and
// source `observed`. Without an observation (the option is off, the table
// was unreadable, or the runtime shows none) the pin stays unresolved: a
// missing reading is never read as "dropped".
func pinOutcomeObserving(rec createRecord, observedAgent string) *fleet.PinOutcome {
	out := &fleet.PinOutcome{}
	if rec.Agent != "" {
		if observedAgent != "" {
			out.Agent = fleet.PinResolved(rec.Agent, observedAgent, observedAgent == rec.Agent,
				fleet.PinObserved, pinAgentObservedEvidence)
		} else {
			out.Agent = fleet.PinUnresolved(rec.Agent, pinUnresolvedEvidence["agent"])
		}
	}
	if rec.Model != "" {
		out.Model = fleet.PinUnresolved(rec.Model, pinUnresolvedEvidence["model"])
	}
	if rec.Effort != "" {
		out.Effort = fleet.PinUnresolved(rec.Effort, pinUnresolvedEvidence["effort"])
	}
	if out.Agent == nil && out.Model == nil && out.Effort == nil {
		return nil
	}
	return out
}
