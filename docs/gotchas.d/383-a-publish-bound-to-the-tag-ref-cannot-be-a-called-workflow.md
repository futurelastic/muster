# A publish the host binds to the tag ref cannot be a job the release workflow calls

**Issue:** #383 (publishing each final to the company release host as well as npm)

## What the host checks

The host's CI publish API accepts a run only when the GitHub OIDC token's `ref` is `refs/tags/vX.Y.Z` (a final) and
its `job_workflow_ref` is `<repo>/<bound workflow file>@<that same ref>`. Both claims describe the run that holds
the token, not the file that wrote the job.

## Why the npm shape does not carry over

npm's publish is a reusable workflow (`release-npm.yml`) called from `release-auto.yml`. That works there because
npm matches the workflow that STARTS the run, and the run starts on `main`. For the host the same shape fails twice:
the run's `ref` is `refs/heads/main` (no final tag), and `job_workflow_ref` of a called workflow names the callee at
the caller's ref. There is no spelling of "call it, but at the tag".

## The rule

Publish from a run that really starts at the tag: `release-auto.yml` DISPATCHES `publish-downloads.yml` with
`gh workflow run --ref <tag>` (the one event `GITHUB_TOKEN` may use to start another run), and
`scripts/release-downloads-guard.sh` decides which tags. Consequences, all deliberate and documented in
`docs/release-host.md`:

- the workflow file must be in the tag's tree, so a final cut before it merged is never published (the guard skips it
  with a line saying so rather than dispatching into a 404);
- a dry run is the only thing allowed from a branch, and its run title says so, so the catch-up never mistakes a
  green dry run for a published version;
- a test pins that `release-auto.yml` never `uses:` the publish workflow
  (`TestReleaseAutoDispatchesThePublishWorkflowAtTheTagNeverCallsIt`).

## Added with #422: a final a person tags is not one of those dispatches

`release.final` is `human`, so the final is tagged by the CLI with a person's credentials, not by `release-auto.yml`.
None of the dispatch paths runs for it until the daily catch-up, up to about a day later (measured on one real final:
the host kept serving the previous version until a person dispatched by hand). The fix is a second way to start the
same run at the tag, not a second workflow: `publish-downloads.yml` also has `on.push.tags: 'v[0-9]+.[0-9]+.[0-9]+'`
(a final only; GitHub reads the filter against the whole tag name, so a `-rc.N` does not match). What that trigger
needs, all in the workflow and pinned by `TestPublishDownloadsStartsOnTheTagPushOfAFinalOnly`:

- a push run has no `inputs`, so every `inputs.tag` read (the concurrency group, `TAG`, the source checkout, the
  artifact name, the run title) falls back to `github.ref_name`;
- the push run is gated on `vars.PUBLISH_ORIGIN` (the build job's `if`), because a clone that never configured the
  host would otherwise go red on every final; release-auto's dispatch has always been off without the variable;
- no double run from the run that tags a final itself: a tag pushed with `GITHUB_TOKEN` starts no workflow, and a
  repeat is answered "already published" by the host in any case.
