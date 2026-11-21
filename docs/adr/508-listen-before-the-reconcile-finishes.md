# ADR: the service listens before the startup reconcile finishes, holds the record store while it runs, and refuses changes to sessions until it is done

**Issue:** #508
**Status:** decided

## Context

On a machine with about a hundred terminal sessions, start-up opened its listeners
only after the reconcile (spec §12) had returned, and the reconcile took 69–79 s:
the port was closed for over a minute on every start (process start to listening:
93–101 s, of which about 18 s were fixed steps and the rest the reconcile). The
reconcile ran under a 15 s deadline that ended only its start-time read, which then
failed at once on an already-spent context, so every session came back `unjudged`:
the whole wait bought no identity check at all.

Two ways out were on the issue: open the listeners before the reconcile and answer
reads while it runs, or make the 15 s bound cover the whole reconcile and say which
step used the time.

## Decision

1. **Listen first.** The fixed steps (trust-seed pass, opencode first attempt, peers,
   the mux) run as before, then the listeners open, then one background goroutine
   runs the reconcile, the delivery-lane pass it decides, the modules and the sweep,
   in the order they always ran. Time to listening is the fixed steps only.

2. **F30 is kept by ordering, on purpose.** An ordinary read records the live set, so
   a client's read served before the reconcile finishes would make the reconcile
   compare the world against a record its own client had just written. The driver
   reads the baseline before the first request can be accepted
   (`BeginReconcile`) and the reconcile classifies against it. Until the reconcile
   has saved its result, a listing persists nothing: the reconcile is the record
   store's one writer, so its final plain overwrite cannot lose, or be lost to, a
   write made by a read. The hold ends on every way out of `Reconcile`.

3. **Changes are refused, reads are answered.** A call that would change one of this
   machine's own sessions answers `unreachable` (504), `retryable: true`,
   `reason: "reconciling"`, with `Retry-After`, from the one check in `mutating()`
   (after the grant check, so a caller who may not do the thing still hears that).
   A call relayed to a peer is not held. No new error kind: `unreachable` already
   carries `retryable` reasons (`runtime-unavailable`).

4. **The reconcile is not cut at a deadline.** `main` passes a context that only
   cancels at shutdown. Each read inside has the driver's own 30 s bound, so the
   start-time read now gets a fresh one instead of an exhausted one.

5. **The line says where the time went.** `reconciled — … took=<ms> phase.records=…
   phase.list.enumerate=… phase.list=… phase.starttimes=… phase.classify=…
   phase.save=…`, using the existing `driver.Phases` recorder. A context with no
   recorder makes every note a no-op.

## Alternatives rejected

- **Bound the whole reconcile at 15 s and drop its result.** Cannot reach the target
  (the fixed steps alone are about 18 s), and a dropped result skips the lane pass
  and the save: adopted sessions' lanes would not be re-attached.
- **Open the port but answer only `/v1/health`.** Meets "an answer, not a refusal"
  but refuses every read for a minute that is provably safe under the hold.
- **Serve everything, mutations included.** A create, rename or close in the window
  writes the records the reconcile is classifying against; making that safe needs a
  merging final save and a lock over the store, correct against rename re-keying and
  recycled ids. More code and more risk for a window that happens once per restart.
  Gating is one check in one wrapper; the merge can still be done later.
- **A per-runtime gate.** The runtime is resolved inside each handler; one
  machine-wide gate also holds local opencode changes, which is accepted.

## Consequences

- A client that treats a 504 on create as fatal shows an error for as long as the
  reconcile runs. `reason` and `Retry-After` let it do better; the refusal is typed.
- Reads in the window are answered but a cold first listing is as slow as the
  reconcile's own, and two of them run at once.
- A name drift seen in the window is repaired on the first read after it.
- `StartDeliveryModules` starts nothing once the host is stopped, and shutdown waits
  up to 5 s for the startup goroutine, since start-up can now still be running at stop.
- Not done: the start-time read is still a full capture of every pane; attributing the
  time inside a cold listing waits on the `phase.list.*` fields from a real restart.
