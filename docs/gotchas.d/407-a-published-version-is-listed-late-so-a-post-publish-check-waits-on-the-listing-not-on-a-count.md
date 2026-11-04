# A published version is listed late, so a post-publish check waits on the listing, not on a count of tries

**Issue:** #407 (follow-up to #309)

## What happened

A release run published a candidate to the public registry successfully, then
failed both Windows install checks. Each retried `npm install -g <launcher>@<rc>`
15 times, 20 s apart (about five minutes); every attempt answered `ETARGET: No
matching version found`, and the step threw its retry-budget error. A few
minutes after the run ended, `npm view <launcher> versions` listed the candidate
and the `next` dist-tag pointed at it. The publish had worked; the registry's
read path simply served it later than the loop was willing to wait. The five
runs before it passed the same step.

## What the registry answers while it lags (measured, npm 10)

| read | version not listed yet | version listed |
|---|---|---|
| `npm view <pkg>@<exact> version` | exit 1, `code E404`, "No match found for version" | exit 0, prints the version |
| `npm install -g <pkg>@<exact>` | exit 1, `code ETARGET` | exit 0 |

Anything else (`ECONNREFUSED`, `E401`, a 5xx, a launcher that installs but whose
`--version` names another build) is not the registry lagging.

## The rule the fix follows

- Wait on the **listing**, cheaply: poll `npm view` for the launcher and for the
  platform package of the machine running the check, until both list the exact
  version. A platform package that is an optional dependency and is not served
  yet is skipped by npm without an error, so the launcher "installs" and only the
  run of the binary shows it. Waiting for the platform package's own listing is
  what keeps that case out of the install.
- Bound the wait by a **deadline** (a named setting) with backoff, not by a count
  of fixed-length tries; keep the job timeout above the deadline so a long wait
  ends as the step's own error and not as a cancelled job.
- Wait on exactly one signature, "not listed yet". Every other answer fails at
  once, so a package that really is missing or broken is not hidden by the wait:
  a package that never lists still fails, when the deadline runs out.

## Where it lives

The `install-windows` job of `release-npm.yml`. The step is PowerShell on the
Windows runners, so it cannot be driven by a fake `npm` on a Unix PATH the way
`scripts/release-npm-guard.sh` is; `release_workflow_test.go` pins its shape, and
its behaviour was exercised on both Windows runners with a function shadowing
`npm` (simulated lag), a real unlisted version, an unreachable registry, and a
launcher naming the wrong build.
