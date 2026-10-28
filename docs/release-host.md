# The release host

Every final release is published to npm (`release-npm.yml`, unchanged) **and** to the company release host, a
download service machines install and update from. The host carries finals only: no candidate (`-rc.N`), and the
four unix builds only (macOS arm64 and x86_64, Linux x86_64 and arm64). Windows stays on its own track.

This page is for the person who maintains the release. For installing muster from the host, see
[Install from the release host](install.md#from-the-release-host).

## What a machine does

```sh
curl -fsSL <host>/muster/install.sh | sh                  # the newest final
curl -fsSL <host>/muster/install.sh | sh -s -- 0.4.0      # an exact version
```

`install.sh` is the one in [`.github/downloads/`](../.github/downloads/install.sh), with the host's address stamped
in at publish time. It downloads the build for the machine, checks it against the version's `SHA256SUMS`, checks the
binary names the release it was asked for, and then runs the **new binary's** `muster service install`. That command
is the install: it copies itself to `~/.local/bin/muster`, creates what is missing, restarts the user service when
the binary or the unit changed, runs `muster doctor` under the unit's environment and waits for `/v1/health`. A
machine that has muster keeps its address, machine id, tokens and state; it gets a new binary under the same unit.

If the new version does not come up healthy, the copy of the previous binary the installer kept runs its own
`service install`, which puts it back; the installer exits 1 either way so a caller never mistakes the failed
version for the running one. A host that wants a token gets it from `DL_TOKEN`, sent as a header through curl's
stdin, never as an argument. `DL_URL` points the installer at another host.

## How a final gets there

```
release-auto.yml (on main)                      publish-downloads.yml (AT the tag)
  a final tagged this run ─┐
  publish-tag = a final ───┼─► dispatch-downloads ──► build ─► artifact ─► publish ─► the host
  daily catch-up ──────────┘   (gh workflow run --ref <tag>)  hosted        publish runner, environment `release`
```

- **Why a dispatch at the tag.** The host accepts a publish only from a run whose ref is `refs/tags/vX.Y.Z` and
  whose workflow is `<repo>/<workflow file>@<that ref>`. `release-auto.yml` runs on `main`, and a reusable workflow
  it called would run with `main`'s ref, so it cannot publish itself. It starts `publish-downloads.yml` at the tag
  instead (a `workflow_dispatch` started with `GITHUB_TOKEN` is the one event allowed to start another run).
- **The same three paths as npm**, because a final is a human act (`final: human`): the run that tags it,
  `publish-tag`, and the daily catch-up. The catch-up asks what the **host** answered, not npm: the newest final
  that main reaches with no successful, non-dry-run publish run is dispatched, and the host answers "already
  published" to a repeat. `scripts/release-downloads-guard.sh` holds those decisions and
  `release_downloads_test.go` runs it against a real origin and a fake `gh`.
- **A final cut before this workflow existed is never published.** The file has to be in the tag's tree. The first
  final after the change merged is the first one on the host.
- **Two jobs, so the key is held by one.** `build` runs on a hosted runner with no secret: it builds the tag with
  `scripts/build-release.sh`, `publish.mjs stage` keeps exactly the four unix binaries and writes their
  `SHA256SUMS`, and the result is a workflow artifact. `publish` runs on the publish runner in the `release`
  environment, with `id-token: write`: it re-checks every file against the sums, signs them and uploads
  (`publish.mjs publish`: begin, parts at the offsets the host reports, complete). The signing key is a secret of
  that environment and of nothing else.
- **A repeat is harmless.** A complete version answers "already published", which is success; an upload that died
  is resumed from the bytes the host already holds.

## Setup, once

None of these values is written in the repository.

1. **Repository variables:** `PUBLISH_ORIGIN` (the host's exact address, no trailing slash: it is also the OIDC
   token's audience), `PUBLISH_RUNNER` (the publish runner's labels as a JSON array), and optionally `PUBLISH_SLUG`
   (default `muster`). With `PUBLISH_ORIGIN` unset, `release-auto.yml` dispatches nothing.
2. **The `release` environment** (Settings, Environments), with any protection rules the maintainer wants.
3. **The signing key.** One command makes it and stores it without ever printing the seed; the public key goes to
   stderr:

   ```sh
   node .github/downloads/publish.mjs keygen | gh secret set PUBLISH_SIGNING_KEY --env release --repo <owner>/muster
   ```

   Rotating means running it again, then binding the new public key (below); signatures made with the old key stop
   working at once, also for uploads already open.
4. **The binding, by a host admin, once.** `PUT /api/v1/admin/items/<slug>/repo` with the repository's name and
   numeric ids (`gh api repos/<owner>/muster --jq '{id, owner: .owner.id}'`), `workflow` =
   `.github/workflows/publish-downloads.yml`, `environment` = `release`, and the public key from step 3. The ids bind,
   so a renamed or re-created repository cannot take the item over.

## Proving it without a new final

Actions, Publish downloads, Run workflow, from any branch, with `tag` = an existing final and **dry run** ticked. It
builds that tag, stages and signs it with the real key, prints the key's public half, and stops before any call to
the host. That proves the runner, the environment and the key; only a real final proves the host.

## Failure modes already met

- **`REF_NOT_FINAL_TAG` / `WORKFLOW_NOT_ALLOWED`:** the run was not at the tag. The workflow's first step refuses
  that before building; a dispatch with `--ref <branch>` is only ever a dry run.
- **`VERSION_NOT_NEWER`:** the host already has a higher version than this tag. A publish never goes backwards.
- **`REQUIRES_UNMET`, `KIND_CHANGED`:** the manifest (`.github/downloads/manifest.json`) disagrees with the host's
  catalog. The message names the item.
