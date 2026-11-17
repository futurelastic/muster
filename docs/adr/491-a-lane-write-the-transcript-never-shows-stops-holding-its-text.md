# 491 — a lane write the session's transcript never shows stops holding its text

**Issue:** #491
**Status:** decided. Keeps ADR 185's module contract and ADR 264's stall rule; adds the
one way out of the dedupe hold that a module which cannot say `rejected` never gave.

## Context

A caller sent a text to an idle session whose pane read `quota_blocked`. The text went
down the session's module lane and the answer was `unknown`: the module wrote it and
could not confirm it. A resend of the same text was answered `unknown` again, from the
ledger, without writing anything: "the module has seen no evidence it arrived. It has
NOT been sent again". The session's transcript showed that nothing had arrived, and the
next turn the session took came from a different input minutes later. The text — a
resume nudge, the one input that matters after an account wall — was lost, and the
ledger held its twin for the full `strandedRetention` (30 minutes).

Two things made that permanent:

- **The hold had one exit.** The ledger asks the module, once and without waiting, what
  became of the earlier write. `confirmed` answers `queued`; `rejected` drops the entry.
  A module that says `queued` ("enqueued, no turn started") or `silent` ("no evidence the
  runtime took it") releases nothing, and those are the two things it says about a
  message the runtime never turned into a turn.
- **The stall rule never armed.** ADR 264 degrades a lane after two enqueues on an idle
  session with no turn started. "Idle" is `idle` or `waiting_input`. A session at an
  account wall reads `quota_blocked`, so neither the before-send nor the after-wait read
  matched and the lane stayed live, enqueuing and never starting a turn.

The lane's send path reads no quota state; the status is the only way the wall reaches
it.

## Decision

The module's `confirm` is unchanged. This driver reads the transcript itself, as the
inbox path does (ADR 184), and lets the hold go only on proof.

1. **Take the position before the write.** A lane send resolves the session's transcript
   (path and size) before the module is written to, and an unconfirmed entry keeps both
   and a normalised fingerprint of the text, no longer than 1024 runes. An identical
   earlier message sits before the offset and cannot count. A transcript that cannot be
   located keeps the entry as it was: the old hold.
2. **The fingerprint is in memory only.** The ledger is persisted and holds digests,
   never a message. The fingerprint is an unexported field, so an entry reloaded after a
   restart has none and holds as before.
3. **Release only on every proof; any doubt holds.** The hold is let go when the module
   did not say `confirmed` or `rejected`, the write was whole, the entry has settled 15
   seconds past the module's own wait, the transcript is still the session's conversation
   file and no shorter than it was, the session reads quiet **now**, and a read of the
   transcript from the offset — taken after that state read — is clean and finds the text
   in no entry. The entry is then dropped, `route.guard.module_released` is counted, and
   the send proceeds as a first send: through the lane if it is live, else the built-in
   path.
4. **Quiet includes `quota_blocked`.** No turn runs at the wall, so nothing can still
   drain a queue. A session that reads `working`, or is waiting on a prompt, keeps the
   hold: a message queued behind a running turn is ordinary and may yet arrive.
5. **Found anywhere means arrived-or-queued, and the hold stays.** What the runtime
   writes for a lane message is not measured, so any entry that carries the text — a
   turn, an enqueue, a wrapped copy — keeps the hold, whatever its origin. Only the
   assistant's own output is not read: it cannot record an input, and a short text such as
   "continue" would otherwise be found in its prose. An entry that is found is not
   promoted to `queued` either: only the module's own confirmation says that.
6. **A half-written last line is not a silence.** The runtime may be writing the very line
   that carries the text; the scan reports it as "could not tell".

## Not decided here

- **Whether the stall rule should count `quota_blocked` as idle.** It would degrade the
  lane at the wall and move input to the terminal path, a behaviour change of its own
  with its own measurement to make.
- **Refusing input at once while the account block is remembered.** Whether the runtime
  queues input at the wall, and for how long, is not measured; a refusal would also
  hold a nudge back the moment the wall lifts.
- **The first call's wait.** It runs to the caller's window by design (ADR 185).

## Consequence

A resume nudge sent to a session at the wall is no longer shut out for half an hour by an
earlier send of the same words that provably never arrived. A caller that resends sees a
fresh first-send answer — typically `unknown` again, since the lane still cannot get the
session to start a turn — and the stall rule or the wall lifting finishes the job. A
write that **did** arrive but that the transcript records in a shape this change does not
anticipate is not released: the transcript has to be *silent* about it, never merely
unfamiliar. The counter `route.guard.module_released` against `route.guard.module_unconfirmed`
says how often the proof was available.

`TestLaneRelease_*` and `TestLaneTailRecords` pin each condition above; every guard has a
test that fails without it.
