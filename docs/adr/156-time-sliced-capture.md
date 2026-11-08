# 156 — time-slice the batched capture inside the call's deadline, retry one stalled chunk

## Context

`enumerate` reads every pane on a machine through one batched
`display-message`/`capture-pane` invocation per chunk. #141 split that into
chunks to stay under the multiplexer's command-length wall. #156 hit the other
wall, time. Some invocations got killed (`signal: killed`) and came back
empty, and every pane in the chunk then read as a driver malfunction. This
happened 20+ times a day per machine, at 3 to 75 panes per failure.

Two facts from the code settled the direction:

- **The declared deadline is per call, not per invocation.** A verb takes
  `bounded(ctx)` once. The listing, every capture chunk and the verb's own
  work afterwards (Send's paste and submit, Discard's clear loop) all share
  that one budget. So a capture killed "at 30 s" had spent the whole call,
  and a retry inside the same call was impossible.
- **Pane count is not the lever.** A machine with only three or four panes
  was killed too, so the multiplexer itself was slow to answer during those
  minutes. A budget scaled by pane count would not have helped it.

## Decision

Each capture invocation runs under a **slice** of the call's remaining
budget. The slice is `remaining / (invocations still to run + 1)`. The extra
share is held back for the verb's own work, so no invocation ever gets more
than half of what is left (`captureSlice`). Every slice is a child of the
caller's context. A caller's shorter deadline or cancellation still ends the
invocation at once, and no call runs longer than it did before.

**One retry per enumeration**, not per chunk. It fires only when a chunk comes
back with nothing parseable **and** its own slice expired while the call still
had budget (`slice-expired`). It does not fire for:

- `caller-cancelled`: nobody is waiting for the answer;
- `call-budget-exhausted`: there is nothing left to spend;
- `multiplexer-exit`: the process exited with an error, and asking again
  returns the same answer.

The failure line keeps #141's prefix word for word, so existing searches of
the log still match. It now adds the invocation's wall time, its slice, the
call budget left, the listing's wall time, the cause, and the retry's outcome.
An invocation that succeeds but takes longer than `deadline/15` is logged too,
so wall times get recorded before the next failure and not only from failures.
Counters: `capture.chunk_failed`, `capture.retry_recovered`,
`capture.retry_failed`, `enumerate.slow_invocation`.

## Alternatives rejected

- **Raise `defaultDeadlineMs`.** It is the declared upper bound on any single
  call (session-abstraction §4.4), and it is reported to callers. It is also
  every verb's "wedged" detection, and the service caps each call at it anyway.
- **A capture budget longer than the call deadline.** It breaks the same
  contract ("a caller may shorten the deadline, never extend it").
- **Budget scaled by pane count.** Contradicted by the 3–4-pane failures.
- **Retry by splitting the chunk in half.** It doubles process spawns against
  a server that is already slow, and gains nothing when the stall covers the
  whole server. *(Revisited in [428](428-split-recovery-for-an-unread-capture-chunk.md),
  which adds it for callers that read the captures, with the spawns capped.)*

## Consequences

- One stalled invocation now costs at most its slice, about 10 s at the
  default with a single chunk, instead of the whole call.
- A capture that would have succeeded in 10–30 s is now killed. A healthy
  batched capture takes tens of milliseconds, so this is judged acceptable.
  The slow-success line will show if it ever happens. `captureSlice` is the
  single place to tune it.
- A stall that lasts minutes still fails, but the failure line now names the
  cause and the retry's outcome. If `capture.retry_failed` dominates
  `capture.retry_recovered` in production, the in-driver fix has done all it
  can, and the rest of the answer belongs to whoever reads the status.
- Out of scope: Send still does a whole-fleet capture to reach one pane.
  Narrowing it would give Send its own short exposure, and that is a separate
  change.
