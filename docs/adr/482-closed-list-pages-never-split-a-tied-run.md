# 482 — a bounded read of the closed list, and a page that never splits a tied run

**Issue:** #482

## Context

`GET /v1/sessions/closed` returned every record in the retention window, and
`limit` was not a parameter it had. Measured on one busy fleet (about 1,300
closes a day, 14-day retention, about 13,000 rows): the unbounded read was
11.8 MB and took 2.3–4.0 s; the same read since an hour ago was 21 KB and took
under 0.1 s. A client that reads incrementally pays the full read once per view
open, per reconnect and per fresh client, to show the newest rows first.

The owner ruled the shape on the issue: `limit` plus `before=<closedAt>`, newest
first, the mirror of `since`; a session id breaks ties; a fleet read merges the
peers' pages and cuts locally, so a peer that ignores the parameters still gives
a correct, only slower, answer.

## Decision

### 1. `before` is exclusive, `since` stays inclusive

Together they tile the timeline half-open, `[since, before)`, so continuing from
the last row's `closedAt` never reads a record twice.

### 2. A page never splits records that closed at one instant

This is the part the ruling left implicit. A timestamp cursor with an exclusive
bound cannot name a position *inside* a run of records sharing one `closedAt`:
if a page ended in the middle of such a run, the next `before=<that closedAt>`
would skip the rest of it. Such runs are not rare — one sweep that finds several
sessions gone stamps them all with a single time.

Options weighed:

- *Inclusive `before`, client drops what it already has.* Never skips, but a run
  longer than `limit` makes the cursor stop moving: the same page, forever.
- *A second cursor field (the session id of the last row).* Correct, but a new
  public parameter the ruling did not grant and a pair every client must carry.
- *Keep the whole run (chosen).* A page is the newest `limit` records plus any
  older ones sharing the last one's `closedAt`. A page can be longer than
  `limit`; the cursor is the one value the rows already carry, and it always
  advances. The size of the overshoot is the size of one tied run, which the
  retention window bounds.

The session id still fixes the order inside a run (then machine, then runtime),
so two reads of the same data return the same sequence — before this, the order
among records with equal `closedAt` followed insertion order, and across
machines the order peers happened to answer in.

### 3. The fleet read cuts once, after the merge

Each peer is asked `scope=local` with the same `limit` and `before`; the
service concatenates, sorts and applies `before` and `limit` to the merged list
(`pageClosed`). The newest `limit` of a union are among the newest `limit` of the
source holding each one, so a peer's page is enough. The same function runs on
what an older peer returned unbounded, which is why that peer needs no special
case and is not reported as degraded.

### 4. The cursor carries nanoseconds

`closedAt` is a `time.Time` and prints with its fractional seconds. The remote
driver sends `before` in RFC 3339 with nanoseconds, unlike `since`, which it
truncates to seconds (truncating an inclusive lower bound can only include more;
truncating an exclusive upper bound would skip the records between).

### 5. No envelope field, no cap

The cursor is the last row's `closedAt`, and a page shorter than `limit` is the
last. `limit` has no maximum: retention already bounds the list, and a cap that
silently shortened a page would be mistaken for the end of it (the same reason
the record route refuses an over-large limit instead of clamping).

## Consequences

- A call naming neither parameter returns the same rows as before, in the same
  order except among records with identical `closedAt` (now fixed).
- A peer built before this keeps working and is read in full, as today.
- The unbounded read is still available and still slow; callers that want the
  speed pass `limit`.
