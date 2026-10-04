# A fail-closed guard that retries must tell a blip from an answer, or it retries away the one verdict it exists to give

**Issue:** #261 (follow-up to #260)

## What happened

The npm publish job refuses a private repository, and it reads visibility from
the hosting API to decide. It treated "could not read" as "private", which is the
right default, and so a single 5xx from the API failed a release candidate that
was otherwise fine. Re-running the failed job went green with no change.

## The rule the fix follows

Retrying a fail-closed read is safe only when the loop distinguishes three things:

- **transient** (a 5xx, a 429, no HTTP status at all): retry, bounded;
- **an answer** (`true`/`false`): take it at once. A `true` must never be retried
  away, or the guard stops guarding;
- **an answer retrying cannot improve** (401/403/404, a body that is neither
  `true` nor `false`): fail immediately, say so.

A loop that retries on "anything but `false`" fails the second case. A loop that
fails on "anything but a 5xx" fails the first. Only an explicit `false` ever counts
as public, whatever the retry count.

## The same shape for the publish itself

`npm view` answers E404 for a version that is not there; that is an answer, not a
blip. Any other failure of the read is unknown and retried. A publish that fails
with a connection error may still have landed, so the registry is re-read before
the next attempt, and a conflict ("previously published") counts as done.

## Where it lives

`scripts/release-npm-guard.sh`, called by `release-npm.yml`. It is a script, not
inline workflow shell, so `release_workflow_test.go` can run it against a fake
`gh` and `npm` on PATH, which is the only reason the retry and the fail-closed
path are both pinned by a test.
