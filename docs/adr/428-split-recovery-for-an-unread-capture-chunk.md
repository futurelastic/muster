# 428 — read the unread panes of a killed capture chunk again in smaller chunks

## Context

[156](156-time-sliced-capture.md) gave each capture invocation a slice of the
call's budget, and one same-size retry per enumeration when a chunk came back
with nothing parseable because its own slice expired. It rejected splitting the
chunk: that "doubles process spawns against a server that is already slow, and
gains nothing when the stall covers the whole server".

A machine with one multiplexer server holding about 140 panes still showed the
failure afterwards. Its service log for one day had 156 "returned nothing
parseable" lines, 44 of them for 64-pane chunks. By cause: 111
`caller-cancelled`, 29 `slice-expired`, 16 `call-budget-exhausted`. Only the 29
are a chunk killed at its own slice, and they are the only ones a second look
can change. Two measurements in that log bear on the decision:

- A same-size retry recovered 64 of 64 panes in 312 ms, so a stall can be
  brief and the panes are readable seconds later.
- Retries that "recovered" 5 of 64 and 27 of 64 left the rest unread without a
  failure line. A killed invocation hands back what it printed before the kill,
  and the old check only asked whether anything at all had come back.

What is not shown is that chunk size is the lever. 156 measured three- and
four-pane machines failing, and a listing with no captures took 14 s in the
same minute that 64-pane chunks were killed.

## Decision

For a caller that classifies from the captures, a chunk whose invocation
expired its own slice with panes unread has **those panes** read again in
smaller chunks, halving down to one pane, in a **recovery pass**. The
trigger is the cause 156 already named (`slice-expired`), now also for a chunk
that came back short. A multiplexer exit, a caller that went away and a spent
budget are unchanged: asking again does not change them.

The pass is bounded in four ways, which are the answer to 156's objection:

- It holds **at most half of what the call has left** when it starts, as one
  child context. The verb's own work and the chunks still to come keep the other
  half.
- Each piece gets `captureSlice` of what is left in the pass, as every
  invocation does, so a smaller piece gets a smaller slice. If the small
  pieces are slow too, they are killed at their slice and the pass ends when its
  half is used. Nothing runs past the call's deadline.
- At most `captureRecoveryMaxInvocations` (16) invocations per pass. Isolating
  one stuck pane among 64 takes 12.
- **One pass per enumeration**, as 156 had one retry, so a stall across the whole
  server does not spend the deadline chunk by chunk.

Pieces run in first-in first-out order so the halves that read cleanly are read
before a stuck one is split further. A lone pane is asked for again as it is,
which is the same-size retry a one-pane machine had.

**Callers that discard the captures do not get the pass.** Thirteen call sites
use `rows, _, err := d.enumerate(ctx)`; ten read the captures and call
`enumerateScreens`. A pass for the thirteen would spend deadline on panes nobody
reads, so `enumerate` keeps the single same-size retry and nothing more.

Counters: `capture.chunk_partial` (a chunk came back short),
`capture.split_invocation` (one per invocation of a pass). `chunk_failed` still
means wholly empty, and `retry_recovered` / `retry_failed` now say whether the
pass read at least one pane.

## Alternatives rejected

- **Keep the single same-size retry.** It recovers a brief stall, and a pass
  does as well with one more spawn, but it cannot recover a chunk that came
  back short, and it spends a whole slice on a size the server may be slow at.
- **Split into a fixed ladder (16, then 4, then 1).** More invocations when one
  pane is stuck, and no better when the stall is brief.
- **Skip the captures for the thirteen callers that discard them.** It removes
  waste that predates this change, but it changes thirteen verbs for a defect
  none of them has. It is the better fix for that waste and belongs in its own
  issue.

## Consequences

- If the stall covers the whole server, the pass buys nothing: it costs up to 16
  killed processes inside half of the remaining budget, and the panes still read
  as a malfunction. `capture.split_invocation` against `capture.retry_recovered`
  says which of the two this is in production. If the first dominates, lower the
  cap or drop the pass.
- A piece below the first level gets a smaller slice than the chunk did. A
  healthy capture takes tens of milliseconds, so the trade 156 took stands.
- The last pane in a short answer may itself be cut off at the kill, and is kept
  as read. Nothing in the output marks where the kill landed. This was already
  true before this change and is not addressed here.
- A session create does not depend on any of this. `Create` reaches the
  enumeration only through a replayed key, and both callers discard the captures.
