# ADR: create accepts every permission mode the runtime supports, and the grant follows escalation

**Issue:** #329
**Status:** decided — ruled on the issue's plan (the issue left the grant question open)

## Context

`permissionMode` at create was a closed set of one, `bypass`, because the only
non-default posture a caller needed was unattended sessions. The runtime's CLI
takes `--permission-mode` with `default`, `acceptEdits`, `plan`, `auto` and
`bypassPermissions`, and the service already reports those states
(`state.permissionMode`). A caller offering its users a choice of mode could
honour none of them except bypass.

Two questions had no answer in the issue: which of the new modes need the `send`
grant that every non-default mode needed so far, and what a resume does with a
non-bypass mode it recorded.

## Decision

1. **The create set is the five reportable modes.** A driver lists the subset it
   takes in `createPermissionModes` (capabilities); a value outside the set is a
   `400` whatever the runtime; a value the runtime does not list is refused
   `unsupported`. An absent list is "the driver did not say", never "none".
2. **The grant follows escalation against a plain create.** `acceptEdits`,
   `auto` and `bypass` need `send` on top of `create`; `default` and `plan` need
   `create` alone. The issue's argument that every mode is narrower than bypass
   compares against the wrong baseline: the grant protects the step from "may
   start a session", and a plain create starts in the ordinary mode. ADR 188
   already ruled that `acceptEdits` and `auto` escalate and `plan` and `default`
   de-escalate; a create-only principal could not reach the first two on a live
   session without the `keys` grant, and starting one directly must not be a
   quieter route to the same place. The check fails closed (an unrecognised value
   needs the grant), and stays on `send`, not `keys`, so the create body keeps one
   grant for all its elevated fields.
3. **`default` named explicitly reaches the driver as itself.** A runtime told
   nothing starts in whatever mode its own configuration names, which need not be
   the ordinary one, so "create with `default` reads back `default`" requires the
   flag. Launch records still store the ordinary mode as the absence of one.
4. **Resume carries every mode the resolved runtime lists.** Without the grant,
   `bypass` is refused as before (#256: refused, not downgraded). `acceptEdits`
   and `auto` are *not carried* instead of refused: the recorded mode is what the
   last session was read in, and a runtime configured to start in `auto` records
   it for sessions nobody asked to widen, so refusing would break resumes that
   worked before. `plan` carries freely.

## Not decided here

- No version gate on `auto` in code: whether a build offers it depends on the
  account and model as much as the CLI version, and a table would be an opinion
  about the runtime. A session that comes up in another mode reads its real mode
  in `state.permissionMode`.
- The remote driver forwards the mode verbatim; the owning peer is the authority
  and an older one refuses a mode it does not know, loudly.
