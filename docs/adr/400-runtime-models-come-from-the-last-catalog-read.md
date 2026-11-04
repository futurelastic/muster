# ADR: a runtime's model list is the last catalog read a create made, filtered by the one policy filter

**Issue:** #400
**Status:** decided

## Context

A client that lets a person pick a model had nothing but a free-text box. The
opencode driver already reads its runtime's provider and model catalogue to check
a pinned model, but that read is made against a session's OWN server, with the
credentials that session was given, on every create. There was no machine-level
catalogue: `GET /v1/runtimes` makes no network call, and a peer's row is answered
from a cache.

## Decision

- **The list is the latest usable answer a create's catalog read produced**, kept
  on the driver with its read time (`readAt`). `readCatalog` stays the only
  function that asks the runtime; it records each usable answer, including every
  re-read in the settle window. There is no second fetch path.
- **It is the latest answer, not a union.** A union would list ids that only the
  runtime's built-in list carried, and a later create that sees only the refreshed
  list refuses them after the settle window.
- **One filter.** `modelCatalog.within(policy)` is the only "usable under the
  operator's policy" decision; the create-time check and the list both call it, and
  a test pins "the ids listed are exactly the ones a create accepts" across several
  policies.
- **Before any create since start-up the source is `none`**, with no `readAt`. An
  empty list with source `runtime` means something else: the runtime answered and
  listed nothing usable.
- **`default` is set only when it is certain**: exactly one provider is listed, the
  answer names a default for it, and the policy keeps it. With several providers the
  runtime's pick depends on an order this driver cannot see.
- **The Claude Code runtime has no catalogue**, so it reports `declared`: the
  operator's list (`models` in the config file, per runtime id) or the aliases its CLI
  documents. The opencode runtime refuses a declared list, because it would be a
  second source for an answer that has one.
- **A peer's block travels with its cached capabilities** and degrades to `none` on
  the same staleness rule; its `readAt` is passed through on the peer's clock.

## Rejected

- *A probe server* (start a throwaway runtime on a timer): a second fetch path, a
  process to reap, and credentials that are nobody's.
- *A persisted file*: a restart is exactly when the binary or the credentials change,
  and it would still need `readAt` to be read honestly.
- *Expiring the list after a time*: a picker that vanishes on an idle machine is worse
  than a dated one, and the create stays the authority.
- *Re-stamping a peer's `readAt` locally*: that would claim a read this machine never made.

## Consequences

- The list lags a change of credentials until the next create, and one create's
  credentials feed every caller's picker. The docs say the list is a hint and the
  create is the authority.
- A peer running both runtimes advertises one row to this machine (the remote driver
  adopts the first available row), so the opencode block of such a peer is visible on
  that peer's own `/v1/runtimes` only. This limit predates the field.
