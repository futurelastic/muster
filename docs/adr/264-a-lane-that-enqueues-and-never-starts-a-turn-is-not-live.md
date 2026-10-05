# 264 — a lane that enqueues on an idle session and never starts a turn is treated as not live

**Issue:** #264
**Status:** decided. Keeps ADR 257's rule; supplies the evidence that makes a live
lane stop being live.

## Context

A session whose only input path was a live delivery-module lane stayed idle through
about 25 minutes and four different messages. Every send answered `unknown` ("the
module saw the message enqueued but the turn had not started"), the session's status
and `since` never moved, and the composer stayed empty. ADR 257 makes a live lane the
only input path and refuses `resumeIfStranded`, `replaceIfStranded` and an explicit
`terminal` there, so the caller had no route left, and the lane still read live.

## Decision

1. **The signal is local and passive.** A send is a *stall* when the module reports
   `queued` (enqueued, no turn) AND the session was idle before the send AND, read
   again after the module's window, is still idle with the same `since`. An enqueue on
   a busy session, or behind a dialog waiting on an answer, is ordinary and never
   counts.
2. **Two in a row degrade the lane.** One stall is not a fault (a runtime can be slow
   to begin). After `laneStallLimit` (2) consecutive stalls with no confirmed send
   between, the lane is degraded exactly as a `silent` verdict degrades it. A
   confirmed send or a fresh live attach resets the run.
3. **This does not weaken ADR 257.** Degraded is "not live": the terminal is the path
   only for a session with no live lane, and that is now true of this one. The
   session's `delivery` field reports the lane as terminal with the reason, so the
   caller is told plainly, and the next send is carried by the built-in path.
4. **Double delivery.** Texts the module already enqueued may still arrive later. Each
   was already receipted `unknown` and the identical text stays held by the ledger;
   the receipt that triggers the degrade says the earlier messages may still arrive.
   A different text sent afterwards by the terminal is a new message, which is what
   the caller asked for.
5. Counters: `lane.stall`, `lane.stall_degraded`.

## Not decided

Why the module enqueues without starting a turn is not measured here (module-side
stall, or the service-side wait). This change bounds the damage and makes the
condition visible; it does not claim the cause.
