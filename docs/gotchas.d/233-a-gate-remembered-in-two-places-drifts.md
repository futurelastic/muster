# A gate remembered in two places drifts, and the drift reaches trunk

**Issue:** #233 (the local gate skipped gofmt; a gofmt-dirty file reached trunk)

## What happened

`bcc32c2` (#230) shipped green: the wrap session ran `go build ./...`,
`go vet ./...` and `go test -race ./...`, all passed, and it merged. CI then
failed at its very first step, `gofmt -l`, 11 seconds into the run — before
build, vet or tests even started. #231 fixed it with a whitespace-only diff.

Nothing here was a mistake in judgement. `.github/project.yml` declared no
gate, so "the gate" was whatever a session decided to run from memory, and
`ci.yml`'s `go` job was a *second*, independently maintained list of the same
four steps. The two lists agreed by coincidence until the day they didn't —
and the one that reached trunk was the one nobody was running locally.

The second gap this issue found: a pushed branch gets no CI run of its own
(`ci.yml` triggers on `push` to trunk and on `pull_request` only), so even a
session that *did* run the full CI step list locally had no independent
confirmation before merging. The fix branch for #231 needed a throwaway PR
(#232) just to obtain the green run the cure rule requires.

## What to do

Declare the gate exactly once, as a file both the human/agent workflow and CI
call — never as a list of steps duplicated in prose or in two workflow files.
This repo's fix: `scripts/gate.sh` runs gofmt → build → vet → test -race in
CI's own order; `ci.yml`'s `go` job's only step is `./scripts/gate.sh`;
`.github/project.yml` carries `gate: scripts/gate.sh` so a wrap session finds
it without reading the workflow file.

Any repo with a `go` job (or equivalent) that lists its checks as separate
inline `run:` steps, with no single script backing them, has this exact gap
latent — it just hasn't been measured yet.

The second gap (no CI on a branch push) was deliberately left open here — see
`CONVENTIONS.md`'s reasoning in `ci.yml` itself and #199 — because it trades
against secret-scan-on-every-push cost and -race flakiness blocking every
ship. It only reopens if it recurs after the gate-script fix; re-measure
before reviving `push: branches: ['**']`.

Reopened by #299: the owner ruled that tests run on runners, so ci.yml now fires
on a session-branch push and the `go` job (`scripts/gate.sh`) runs there. See
`docs/internals.md`, "Secret scanning, and fixtures that look like secrets".
