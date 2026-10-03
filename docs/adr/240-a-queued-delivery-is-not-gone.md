# ADR: a delivery confirmed without a started turn is not forgotten

**Issue:** #240
**Status:** decided

## Context

A message sent to a session by another driver was found sitting **unsent** in
the receiver's composer — five times on one machine in one morning, including
a coordinating session whose stuck input stalled a whole lane for half an
hour. In every case the sender had been told `queued`, had no reason to
retry, and the driver refused every attempt to finish the delivery ("the
composer holds text this driver has no record of placing"); a third party
recovered each one by reading the screen and sending `replaceIfStranded` with
the composer's digest.

The issue was filed as "a session with no turns yet", and a later comment
corrected it: the sessions had been running for tens of minutes. `turns: 0`
was real but meant "nothing completed since the last delivery" (#111), not
"just created". The common factor was a message arriving while the receiver
was busy.

Two earlier fixes cover neighbouring hops and left this one open: #25 refuses
a send to a pane with no composer yet, and #112 trusts the driver's record
when one exists. Neither helps when there is no record, and there is none
because the driver deliberately deletes it.

## What the code does, and which hop was open

On the terminal path every failure after the paste — the text not landing,
the submit failing, the submit not registering — writes a stranded record and
returns `unknown`, so the sender is told. The one success outcome is
`queued`, after which the record is forgotten, and no tombstone is left for it
because "that text is gone".

That premise holds for a submit confirmed by a turn: a user entry or a command
entry in the runtime's own transcript. It does **not** hold for the other
evidence the driver accepts:

- a transcript `enqueue` — the runtime took the text into its queue because a
  turn was running; nothing has started on it;
- the composer reading empty (or the delivery's paste marker clearing) on a
  screen, when the transcript stays silent.

Both show the text left the composer. A runtime that queued it can return it
(a queued message handed back after the running turn was interrupted or
failed). Nothing in the driver then remembered it, so the text looked like a
person's draft and was refused, and the sender had no reason to look.

**Not measured:** what the runtime does to the queue in each case, and which
of the two weak signals produced the five incidents. The change below is
correct under either, and adds the counters that settle it.

## Decision

1. **Keep a provisional entry when the confirmation is weak.** A confirmation
   that does not show a turn started (`confirmSubmittedFromSourceTurn`
   reporting `turnProven == false`) records a *provisional tombstone*: the
   labelled text, the working directory, and the transcript position taken
   before the submit. It is the same record the draft rule already uses for a
   lapsed strand (#180 M2), so it proves the composer's text is the driver's
   own under the same two tests and acts through the same doors
   (`resumeIfStranded`, `replaceIfStranded`) with no new way to clear a
   composer. One entry per (directory, text) per session; the existing cap and
   24-hour retention apply; it persists with the other tombstones.
2. **Proof requires the text not to have run.** When a transcript was
   resolved, a provisional entry proves nothing once a user turn carrying the
   same text appears after the recorded position: the runtime ran it, and the
   same words in the composer are a person's or a recall from history.
3. **Say so on the receiving side.** `SessionState.strandedDelivery` is true
   only with `waitingOn: unsent-input` while the driver's own memory (a live
   record or a tombstone) proves the composer's text is its own delivery. It
   is part of `MateriallyDiffers`, so a subscriber hears about it. The text
   itself is never published.
4. **Say so on the sending side.** A `queued` receipt resting on a weak
   confirmation names what the sender will see if the text is handed back and
   what to do about it; the busy-composer refusal says whose text is there.
5. **Count what is left unexplained.** `submit_confirm.by_enqueue`,
   `stranded.provisional_kept`, `stranded.provisional_handed_back`, and
   `stranded.unexplained_labelled_composer` — an unsent composer opening with
   the driver's sender label that nothing it remembers explains (logged with
   whether a record exists under another working directory). The last is
   diagnostic only and never proof. If it fires after this change, another hop
   is open.

## Why surface the strand, not submit it

The issue allowed either: land the text, or make the strand visible to the
sender. Landing would need the driver to press submit unattended, later. The
hand-back this change guards follows a stopped turn — an interrupt or a
failure — and re-submitting would override a stop the driver cannot attribute.
A send also cannot stay open across a busy turn of unbounded length (§4.4). The
draft rule and the stranded-delivery machinery already finish a strand when the
sender asks; the minimal fix is to stop discarding what they need. Landing
remains open as a follow-up, to be decided on the counters above.

## Not covered

- The delivery-module and inbox lanes keep no record, on purpose; see
  "Module and inbox lanes" below.
- A hand-back rendered as a collapsed paste marker, or several queued messages
  returned joined, will not match the recorded text; they stay refused (fail
  safe) and are counted as unexplained when they carry the sender label.

## Module and inbox lanes (#249, re-cut after #257)

This ADR first listed both lanes as a gap: a module confirming a queue entry
was assumed to have the same shape as the terminal path's weak confirmation.
That premise does not hold, and after #257 the record would have no consumer.
Nothing is built on either lane. The reasons, so the question is not filed
again:

**Module lane.**
- A `queued` receipt on this lane follows only the module's `confirmed`
  verdict: a user-origin turn the runtime accepted. That is the terminal path's
  turn-proven case, where this ADR keeps no record either.
- The weak case, the module's own `queued` verdict (enqueued, the turn has not
  started), is answered `unknown` with a ledger entry, never `queued`. No
  `queued` receipt on this lane rests on weak evidence.
- On a live lane `resumeIfStranded` and `replaceIfStranded` are refused before
  any record is read (#257), so a record would feed a path that is closed. The
  recovery is `discard` with `expect`, then a fresh send.
- The module's transcript path is carried but never opened by the driver, so a
  record kept here would lack the "did the transcript show a turn start" check
  this ADR's proof depends on. It would then prove any later recall of the same
  words to be the driver's own, including after a turn that did start.
- A degraded lane sends to the terminal, where this ADR's record applies. It
  exists only after a weak `queued`, which the module lane does not produce.

**Inbox lane.**
- `auto` never tries the inbox since #257; it is used only when named.
- Its one success outcome, `delivered`, needs the receiver's transcript to
  record the exact envelope. The weak case is `unknown` with a ledger hold, and
  a resume is not how that is retried.
- Even if a runtime handed a queued peer message back, it would not return in
  the form a record keys on. A record holds the terminal-labelled text, while
  the inbox carries the label in the envelope's sender-name field, so a record
  could never match. This is the same fail-safe as the collapsed-paste case
  above.

**Reopen when** a session whose `delivery.clientConnected` is true reads
`waitingOn: unsent-input` with a composer opening `[from: `, or a module is
found to report `confirmed` for a bare enqueue. The module side then has the
same shape as the terminal path and this section is wrong.

**The field signal does not see these sessions.**
`stranded.unexplained_labelled_composer` fires only from the terminal send path.
After #257 no `/input` reaches that path on a session with a live lane, so the
counter cannot fire for them. Watch `state` for the pattern above instead.

`TestModuleLaneKeepsNoProvisionalRecord` pins the module-lane facts.
