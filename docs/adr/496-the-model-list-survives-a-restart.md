# ADR: the model list survives a restart, and the service reads the catalogue when it starts

**Issue:** #496
**Status:** decided — the owner's direction (2026-11-17): this service is the only component that starts a
runtime, so it stores and serves the runtime's facts; clients store only what people choose. The issue left
how to this plan; the choices below are the implementing session's and are reversible.

## Context

`GET /v1/runtimes` lists the models a create on the opencode runtime would accept, from the last catalogue
read (ADR 400). That read lived only in memory and only a create made one, so every restart — every update —
put the row back at `source: "none"` until somebody created a session through the API. A client that offers
no picker for a runtime with no list never made that create. Measured: after an update that relaunched six
sessions, the row still read `none` more than an hour later; one create filled it; a second machine showed
the same gap after its own restart.

## Decision

- **Every usable read is kept** in the state directory (`opencode/catalog.json`: ids, the runtime's own
  default per provider, the time). It is written by the one function that records a read (`noteCatalog`),
  under the lock that guards the in-memory copy, so the file always says what the memory says. It sits beside
  the session records and the session directories and in neither, because the sweeps of those two read every
  entry as a session's.
- **At start-up the stored read is the list** until a read of this run replaces it. It is served as the same
  `source: "runtime"`, with its **original** `readAt`, plus an additive `stored: true`. A new `source` value
  was rejected for the reason the issue gives: the set is open on read and an unknown source reads as `none`,
  which is the bug. `stored` is omitted when false, so a client that never heard of it sees what it always saw.
- **The policy is applied when the list is built, not when it is stored** (`within`, the one filter ADR 400
  names). The file holds the runtime's raw answer, so a policy narrowed across a restart narrows the stored
  list, and widened again restores it; nothing is lost from the file by a narrower policy.
- **The service reads the catalogue once it is up**, in the background: from a relaunched session's server
  when one is running, else from one short-lived server of its own started like a create with no agent and
  no model (the same credentials a create would have), with no session on it, so no prompt and no spend. It
  is stopped as soon as it has answered, and Shutdown ends the read and waits for it. Sessions that are
  parked are never started for it. Background, so a slow runtime cannot hold the service's start, and the
  stored list is already being served meanwhile.
- **The read settles**: it asks again until two answers agree (or the settle window ends), because a server
  that has only just started can answer with the list built into its binary before it has fetched a newer
  one (#384). A create keeps its own settle, which exists for a different question.
- **A start-up read that lists no model replaces nothing that lists some** ("a restart never empties a runtime
  that had a list"). Where nothing was held, a positive empty answer is recorded, as a create's is. The
  create-time check, which reads for itself, stays the authority and refuses a model the runtime no longer has.
- **A stored read always yields to a fresh one**, whatever its time says: it is another run's clock, and a
  clock that was ahead must not hold the list for ever.
- **A file that cannot be used** — not readable, not JSON, another version, an id that is not
  `provider/model`, no time, over 4 MiB — reads as `none`, one notice is logged, and the next read replaces
  it. It is never an error. A write that fails is logged once when it starts failing and once when it stops,
  and never fails the read that was made.
- **With no state directory** nothing is stored and nothing is read at start-up (as for every other thing the
  driver does at a start); the list is as it was before this change.

## Rejected

- *A new `source` such as `"stored"`*: reads as `none` to every client that predates it.
- *Reading at start-up before the service is up*: holds the start behind a runtime that may be slow, to
  produce a list the stored one already gave.
- *Asking a parked session's server*: it has none, and starting it to ask would undo the park.
- *Expiring the stored list*: ADR 400's reason holds — a picker that vanishes on an idle machine is worse
  than a dated one, and `readAt` is honest.
- *Writing the file only when the ids change*: the time would go stale, and `readAt` is what lets a client
  show an honest age.

## Consequences

- ADR 400's two *Rejected* items "a probe server" and "a persisted file" are reversed, for one read at
  start-up and not a timer.
- A peer's row carries `stored` through unchanged (the row is passed through as a value); the peer's
  `readAt` stays on the peer's clock.
