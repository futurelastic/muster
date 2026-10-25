# 184 — route a send by who is sending it, and never let two paths carry one message

> Superseded in part by [257](257-a-live-lane-is-the-only-input-path.md): a human relay's `auto` is no longer rewritten to `terminal` on the entering machine, and `auto` no longer tries the inbox.
>
> Superseded in part by [368](368-the-launch-prompt-follows-a-live-lane.md): the create-time prompt is no longer pinned to the terminal on its first attempt (it is `auto`, so a live delivery lane carries it); its retry is still pinned, and a launch prompt a lane carried is never retried.

**Issue:** #184
**Status:** decided and ratified (#193, 2026-09-24); each of the six choices that
needed a person's ruling is listed at the end with its outcome.

## Context

A send has two ways in, and each gives the wrong authority to some senders.

- **The terminal.** Any sender's text arrives as a genuine **user** turn. #180
  gave a non-human sender a label so its text is recognisable, but the runtime
  still grants it a user's authority.
- **The inbox.** The runtime itself marks the text as a cross-session **peer
  message** — with its own warning that it did not come from the user and that a
  peer cannot grant escalation. That is exactly the right authority for an
  agent's message and exactly the wrong one for a person's: when the inbox was
  enabled, an operator's approval relayed through a human-facing client went out
  as a peer message and the receiving session refused it.

So the path has to follow the sender. That is small. The hard part is what it
forces: **two paths that must never both deliver one message.** Nothing in the
old design had to care — the inbox either answered or fell back, and a fallback
after a failed write was accepted as safe (#144).

## Decision

### The route, and where each half is decided

`POST …/input` takes `route`: `auto` (the default), `terminal`, `inbox`. The
decision is split by what each side can know.

- **The service** decides everything that depends on the *request and the
  principal*: it parses `route`; a human relay's `auto` becomes `terminal`
  (unlabelled); anyone else's is labelled; the shape errors are `400`s.
- **The driver** decides everything that depends on the *session*: whether the
  inbox can take the message, what to do when it cannot, what was confirmed, and
  whether an earlier attempt already put the message somewhere.

Neither can do the other's job. The driver cannot see principals, and an older
owning peer would re-decide a human's `auto` as inbox-eligible. The service does
not hold the resolver and would need an eligibility query that races the send.

`driver.SendOptions.ForceTerminalRoute` is replaced by `Route fleet.Route`. The
zero value is auto, so every existing caller keeps its meaning. An explicit
`inbox` that the session cannot take is a **refusal with nothing written**, never
a downgrade: a caller that asked for a path is entitled to know it did not get
it. A `400` was rejected for that case, because it is a fact about the session
and not a fault in the request, and a `4xx` teaches callers to retry.

### The receipt names the path

`delivery: {route}` on the receipt, `inbox` or `terminal`. Absent means the
receipt names no path (an early refusal, a driver with one path, an older peer) —
never a guess. An unrecognised route decodes to absent rather than failing the
receipt: a newer peer adding a route must not make an older machine unable to
read the outcome of a send that already happened. It is an object, not a string,
so a later module name can join it.

### The label is mandatory for everyone but a human relay

Today an unlabelled non-human send reaches the terminal as though a person typed
it. Refusing such sends would break every existing caller, so the service fills
the label in from the one fact it holds — the authenticated principal, then the
machine — and never replaces anything the caller said about itself. The label is
a pure function of the request, so a `resumeIfStranded` retry is labelled
identically and still matches the record it is resuming. A forced
`route:"terminal"` keeps #180 M8's refusal: an unlabelled non-human terminal send
is still a `400`.

### Fallback only before any byte is written

`inboxclient.Deliver` reports how many bytes the connection accepted
(`WriteError.Written`). A fallback is taken only for a decline that provably
wrote nothing: no resolver, no identity, no entry, no class, an unattestable
body, no transcript, a failed dial, or `NothingWritten`. **Any** byte — including
the auth line alone — makes the outcome `unknown` on the inbox's account. The one
case where non-delivery could be argued (a partial write inside the auth prefix)
is still `unknown`, deliberately: the rule the issue states is "before any byte",
one condition is auditable, and the cost is an occasional `unknown` for a
transport that broke mid-write.

The wire form is unchanged: two lines, two writes, counted by a wrapper rather
than collapsed into one frame. Collapsing would give the same guarantee and
change what the receiver observes for no benefit.

### A write is confirmed from the receiver's transcript

The inbox has no reply channel, so a clean write only ever proved bytes reached a
socket, and "delivered" on that basis is #148's false report. The terminal path
confirms from the transcript; the inbox now does too. `delivered` means the
transcript recorded the message; anything else is `unknown`.

The transcript shape a peer message produces has **never been observed by this
repository** — the terminal matcher rejects every non-human entry precisely
because it does not know them. So the probe holds two independent fingerprints
and either confirms: the exact **envelope** (the receiver accepts an envelope
only after a byte-for-byte rebuild, so an accepted one is identical to what was
written) anywhere in the entry, or the exact **body** as the text of a user entry
the runtime marked as not from a human. There is no "recorded a different turn"
outcome: a peer message arrives beside other traffic, so an unrelated entry
proves nothing.

A matcher that never matches is **safe**: every inbox send ends `unknown` and
nothing is sent twice. It also makes the path useless, and `inbox.unconfirmed`
close to `inbox.written` is how that would show. Which shape the runtime writes,
and how long a busy receiver takes to write it, are the two things only a live
receiver can say (`deploy.md`, live look).

A send needs a locatable transcript to use the inbox at all; without one it
declines (`inbox.fallback_no_transcript`) before dialling, the same "half a
capability is none of it" rule as the missing class. Identity is verified twice:
before attestation, where ADR 148 needs it so that a fallback cannot mask it, and
immediately before the dial, where #116 needs it, after the work that takes time.

### The cross-path ledger

Inside one `Send` the rule is control flow. Across sends it is not: an `unknown`
invites a retry, and the documented retry, `resumeIfStranded`, is a terminal
operation that — with no stranded record and an empty composer — pastes the text
fresh. Left alone, a retry of an inbox `unknown` would deliver the message twice.
The create-time prompt made it concrete: it retries with `resumeIfStranded` after
any `unknown`.

So an inbox write that put bytes on the socket and could not be confirmed leaves
an entry — digests only, never the message — keyed on the sanitised text, who
the sender said it is (its agent and session), whether the message carried a
label at all, and the relay declaration; corroborated on the session's working
directory (§5.4), persisted with the stranded records, lapsing after
`strandedRetention`. Every `Send` consults it before choosing a path. While an
entry stands, nothing is written on either path, whatever the route or flags: the
transcript is looked at once more, and the answer is `delivered` if the message
has since been recorded and `unknown` otherwise. Different text, or the same text
from another sender, is a different message and is not held.

**The key leaves out the machine the request entered through (#191).** The
service stamps `from.machine` with where a request arrived, so it says nothing
about who sent it — and it is exactly what changes when a caller's client fails
over and retries through the peer. Keyed on the label as the receiver sees it,
that retry produced a different key and missed the entry; if the inbox then
declined with nothing written, `auto` could fall back to the terminal and the
message arrive twice; the miss itself is pinned offline, where the retry wrote a
second time. The key is now built from the caller's own agent and session. A sender that named neither is labelled by the service with
only its machine, so what survives in the key is that the message *was* labelled:
it still differs from an unlabelled one, and a human relay's words, which go to
the terminal with no label, are never held for an anonymous peer's identical
text.

What that costs is on the safe side. Two callers that give the same agent and
session and send the same text to the same session inside the retention window
are held as one, and the second is told `unknown`, not sent again, instead of
being delivered. Agent and session are the caller's own unverified statement
either way (`MessageFrom`), so the check was never a claim about identity; the
machine only made it finer in the one place a retry moves. Documenting the gap
and leaving the key was the other option, and is rejected below.

**Two limits, both deliberate.**

- **Eight entries per session.** The ledger keeps the most recent
  `unconfirmedPerSession` (8) unconfirmed messages for a session; a ninth
  distinct one evicts the oldest, and a retry of the evicted message is no longer
  held, so it can be delivered a second time. The bound counts distinct messages
  still awaiting confirmation, not traffic — a confirmed send leaves no entry.
  It exists so the persisted state stays small, and the ones it keeps are the
  ones a caller could still be retrying. A test pins it.
- **The working-directory corroboration fails open.** When the multiplexer
  cannot be enumerated, the entry is honoured rather than dropped: refusing a
  possible duplicate is the cheap side to be wrong on, and the cost of a wrong
  hold is one `unknown` that lapses on its own. It is listed among the
  deviations below.

The other direction needs no new state: a terminal delivery that could not be
confirmed already leaves a stranded record, and a later send of the same text
finds it and does not use the inbox.

The create-time prompt is pinned to the terminal, on both attempts. It is the
creator's launch instruction, not a peer's message, and pinning it makes its
retry safe by construction.

### The #148 class gate stays

The issue asks whether the gate is stricter than the runtime requires and to
decide from a measurement. The measurement (runtime 2.1.281, throwaway sessions):
a peer message asserting the *prompting* class was delivered to receivers in
default, accept-edits, plan and auto modes, including mid-turn; a *mismatched*
class was delivered, not held.

Read against ADR 148 it says less than it seems to:

- All four receiver modes are prompting-class. A prompting assertion accepted by
  prompting receivers is parity **matching** — what #148 predicts.
- A mismatch that was delivered contradicts "a mismatch is held", but it is
  equally what a receiver whose remotely controlled policy gate is **off** does
  (gotcha 148: with the gate off it accepts anything while it prompts and holds
  everything while it bypasses).
- **No receiver with permission prompts bypassed was measured, and no unattested
  message was.** Those are the only two cases the gate exists to protect.

The gate also cannot be narrowed: the only way to know a receiver is
prompting-class is the index's `mode_class`, and with one we attest anyway.
**Decision: keep it, unchanged.** It would be reopened by measuring, on the
current runtime and confirmed from the receiver's transcript, idle and mid-turn
with the gate state recorded, a bypass-mode receiver sent (a) an unattested
message and (b) a prompting-class message. Even then relaxing is its own change.
What this ADR does contribute is that a future silent hold is now a counted
`unknown` rather than a false `delivered`, which is what would make relaxing
affordable.

### Counters

Every send is counted once under `route.decided.<requested>.<taken>` (taken is
the receipt's path, `none` when it names none, `error` when `Send` failed) and
`route.outcome.<path>.<outcome>`; `route.auto_fallback` is the subset where the
inbox was tried and declined before any byte; `route.guard.*` are the ledger's.
The `inbox.*` exits of ADR 150 gain `fallback_no_transcript` and
`unknown_partial_write`, `fallback_write_failed` narrows to zero-byte failures,
and `written` splits into `confirmed` and `unconfirmed`, so
`attempted = Σ exits` and `written = confirmed + unconfirmed` still hold.

## Consequences

- **Shipping this changes nothing on a machine whose index emits no class.** Every
  `auto` send falls back; the differences are the receipt's `delivery.route` and
  a label on every non-human terminal message.
- **The order of enabling matters** (`deploy.md`): the human-relay grant first,
  then class emission. A person's relayed message from a principal without the
  grant would otherwise arrive as a peer message.
- **An inbox `unknown` is not the terminal's `unknown`.** Nothing sits in a
  composer; `resumeIfStranded` is not the retry. The docs say so.
- **Same-text follow-ups are held for 30 minutes** after an unconfirmed inbox
  write. That is stricter than the issue asks; it is the price of "never twice"
  without a reply channel.
- **The doctor sees the missing grant only where a principal table exists.** It
  is offline, so it reads configuration and not callers. The row that warns when
  an inbox index is configured and no principal holds `human-relay` was not part
  of this change; it shipped afterwards (#189). It skipped single-token mode,
  where there is no grant to point an operator at; since #196 it **fails** there
  instead when an index is set, because the service now refuses to start in that
  shape (below).

## Alternatives rejected

- **Everything in the driver / everything in the service.** See above.
- **Refuse unlabelled non-human sends.** Breaks every existing caller.
- **One write frame instead of two.** Same guarantee, changes the wire.
- **Report `queued` for a confirmed inbox write.** Flattens the per-surface
  vocabulary #119 kept and changes what inbox success returns.
- **Bare-write `delivered`.** That is #148.
- **Rely on callers not to retry.** The service's own create path retries.
- **Block only `resumeIfStranded`.** A fresh `auto` send crosses paths as easily.
- **Relax the class gate now.** On evidence that never tested the failing case.
- **Stop honouring the relay headers when there is no table (#196's first
  draft).** It makes the rule true everywhere by taking the terminal relay away
  from every single-token machine, which has no other way to express "this is a
  person". Ruled out on #195 in favour of requiring the table for the inbox route.
- **Ignore `FLEET_INBOX_INDEX` quietly when there is no table.** It avoids a
  refused start, and it is the failure #122 was: an operator sets an index and
  the service silently never uses it. A refusal to start is loud, `doctor` shows
  it beforehand, and it matches the missing-token gate beside it.
- **Document the failover gap and keep the machine in the ledger key.** It leaves a
  known route to a double delivery in exchange for a finer distinction between
  senders that the caller's own unverified statement cannot support (#191).

## Needs ratification

The implementer marked six choices as needing a ruling. They were ruled on in
#193 on 2026-09-24, under the maintainer's delegation for technical rulings. The
outcome of each:

1. **The synthesised label — ratified.** It changes what an existing unlabelled
   caller's terminal message looks like: it gains a `[from: <principal> ·
   <machine>]` first line. This is what the issue's table asks for ("with the
   mandatory label"), and the wording is acceptable.
2. **With no principal table — resolved by #195 and #196: the inbox route is
   refused without one.** #180 L3 honours the relay headers from any caller, so
   "the human-relay fact is never inferred from a header" held only where a table
   exists. The ruling on #193 kept that exception until the doctor could show the
   table-mode half, and expected a one-line tightening after. It was not one: on
   a single-token machine "no human relay without a table" leaves no way to relay
   a person's message unlabelled at all (#195). #195 ruled for the other way to
   reach the same invariant — **turning on the inbox route requires a principal
   table** — and #196 built it: with `FLEET_INBOX_INDEX` set and no
   `FLEET_CONFIG`, `muster` refuses to start with a message naming the
   table, and `doctor`'s `principals.human-relay` row fails for the same shape
   (both read one function, `requireTableForInbox`, so they cannot disagree). The
   exception is **closed in the sense that mattered**: a machine with no table
   has no inbox route, so nothing it sends — with or without the header — is
   ever diverted into a peer message. Its unlabelled terminal relay keeps
   working, which is why the request-time header handling is deliberately
   **unchanged**; the pinning test in `internal/service/route_test.go` keeps
   asserting it and now says why.

   **What is left, stated so it is not mistaken for closed:** on a machine with
   no table, any holder of the one shared token can still assert the human-relay
   fact with the peer-relay headers. Its effect is now confined to the terminal
   path — the label is skipped and a leading `/` is delivered — and that is the
   cost of keeping a single-token relay at all. Refusing the headers there as
   well would strand that relay; it is a separate decision and was not made
   here. A machine that wants the strict form writes a
   table, which is what `deploy.md` step 0 says.
3. **The create-time prompt is pinned to the terminal — ratified.** It is
   consistent with the measured loss of a session from a create-time prompt.
4. **The 30-minute hold — ratified.** Same-text follow-ups after an unconfirmed
   inbox write are held for `strandedRetention`. It is stricter than the issue
   asked for and errs against double delivery. Revisit it only on a measured case
   where it blocks a legitimate follow-up.
5. **The issue closes with its live cases deferred — ratified.** They go to the
   operator step, because class emission is off wherever it was measured; the
   offline suite covers every branch, the live cases are written down in
   `deploy.md`, and the measurements are tracked in #190.
6. **No fallback after a write that put bytes on the socket — ratified.** This
   reverses ADR 119's addendum, item 3, for that case: a write that fails after
   some bytes went out no longer falls back, because falling back after a partial
   write risks a garbled or duplicated delivery and failing loudly is the correct
   behaviour.

## Deviations from the plan

Recorded at ship-time grading of #184 and accepted in the ruling on #193 as
documentation, not a code change.

- **The ledger's working-directory corroboration fails open.** `answerFromLedger`
  drops an entry whose session is gone or now has a different working directory
  (§5.4), but it learns that from one enumeration of the multiplexer, and when
  that enumeration fails the entry is honoured. The alternative, dropping it on
  an error, would let a retry through exactly when the multiplexer is unwell.
  The cost is one wrongly held `unknown` for a recycled session id, which lapses
  on its own; a duplicate is the more expensive error.
