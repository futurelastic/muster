# A trusted publisher names the workflow that starts the run, so one workflow has to be the only way to publish

**Issue:** #263 (follow-up to #250)

## What happened

npm trusted publishing matches the workflow file that **starts** a run, and each
package holds one such publisher. After #250 that was the automated release
workflow, which publishes a tag only in the run that created it. The reusable
publish workflow's manual dispatch — documented as "publish an existing tag by
hand" — could therefore never authenticate (`ENEEDAUTH`), and its header invited
an operator to switch the setting to it.

Measured: a final tagged by hand reached no registry. The only route left was to
repoint the publisher on all five packages, dispatch, and repoint back: a manual
credential change per out-of-band release, with every automatic release broken
if the second half was forgotten.

## The rule the fix follows

A reusable workflow is not a second entry point just because it has
`workflow_dispatch`. Whatever starts the run is what the registry sees, so the
entry point that can authenticate must also be the one that accepts "publish this
existing tag" — as a dispatch input, and as a scheduled reconcile. The other
file's dispatch is dry-run only and says so before it builds anything.

## Two traps inside it

- **`github.event_name` inside a called workflow is the caller's.** The automated
  workflow dispatched with a tag to publish is a `workflow_dispatch` event for the
  called workflow too. "Refuse a manual publish" therefore also tests
  `github.workflow_ref` for the called file's own name, or it would refuse the one
  legitimate publisher.
- **A reconcile that picks an older tag moves the pointer backwards.** Only the
  newest final is ever a candidate, and a final tag that never reached `main` is
  skipped rather than allowed to hide an older real one.

## Where it lives

`scripts/release-npm-guard.sh` (`check-tag`, `unpublished-final`), pinned by
`release_workflow_test.go` against a real bare origin; the workflows are
`release-auto.yml` and `release-npm.yml`; the operator page is `docs/deploy.md`,
*Publishing to npm*.
