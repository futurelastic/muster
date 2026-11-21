# ADR: the machine's sandbox policy is a key of `createPolicy`, decided per key

**Issue:** #438 (follows #281, #509)
**Status:** decided

## Context

A sandbox is opt-in per create. A client that never sends one (a sessions UI that
starts every session without it was the measured case) gets unconfined sessions
even where the runtime advertises a working sandbox, and the only machine-level
switch was `FLEET_TMUX_SANDBOX=off`, which removes the offer. The machine's
owner is the party who knows what the host can enforce and what its sessions may
reach, so the decision belongs in the configuration file, beside the other
per-machine decisions about what a create may be given.

## Decision

1. **It is a `sandbox` key on a `createPolicy` rule**, not a second list. #509 had
   already put `access` there and left the room on purpose: one list of rules, one
   scope shape (`runtimes`, `agents`, `markers`, as a `sessionEnv` entry), one
   place to learn. The key is `{mode, readPaths, writePaths, network}`; `mode` is
   `default` or `required`. A package cache is not a policy grant (it is a
   per-create request, and an unknown key in the file is refused).
2. **Each key is decided on its own: the first rule that sets it and matches a
   create decides.** The alternative, "the first matching rule decides everything",
   makes an `access` rule that matches an agent silently hide a later sandbox rule
   for that agent, which is the opposite of what an operator who wrote two rules
   meant. A rule, or one key of it, that an earlier rule already decides for
   everything it could match is refused at start: a dead half is the same fault as a
   dead rule.
3. **`default` gives the profile to a create that names none and never limits one
   that does. `required` also holds a create's own to it: narrower or equal is
   accepted, wider is `400` (`sandbox-required`).** "Narrower" is a comparison of
   grants, defined once (`fleet.SandboxSpec.Within`): network no more open; every
   writable path inside a writable one; every readable path inside a readable or
   writable one; a `shared` package cache counts as a write, a `private` one as a
   read of its seed. Paths are compared as written (both sides are absolute and
   clean), so a symbolic link inside a granted directory is not followed; that is
   the operator's to keep clean, and the docs say so.
4. **A create that names no network has not asked for an open one.** Under `required`
   it runs on the profile's network instead of being refused as wider; naming `open`
   against a closed requirement is the wider request. Without this a bare `sandbox:
   {}` or an agent definition asking for `sandboxed` would be refused on every machine
   that requires a closed network, for leaving out a field.
5. **Refuse, never degrade, in the direction the operator chose.** A `required` profile
   a runtime cannot enforce refuses the create `unsupported`
   (`sandbox-required`, `retryable: false`) and nothing starts. A `default` the
   runtime cannot enforce is **not applied**: a create that asked for nothing is not
   refused for lacking something, and the operator who wanted a refusal writes
   `required`. Both are visible before a create: `GET /v1/runtimes` carries
   `sandboxPolicy` per runtime row with `enforceable`, and `muster doctor` has a
   `sandbox.policy` row that is `fail` for a required rule that cannot be enforced
   and `warn` for a default.
6. **It is applied after the posture is lowered and only to a create this machine
   serves.** A posture that confines is the create's own sandbox and is judged like
   one; a posture that confines nothing leaves the create with none, so it gets the
   profile. A relayed create is the peer's to judge under the peer's policy. An
   agent definition's `knowledge.home` is a path grant like any other, so under
   `required` it must lie inside the profile and is refused naming `agentSpec.knowledge.home`
   otherwise.
7. **It is restart-only.** The policy is handed to the service once at start, as the
   rest of `createPolicy` is; a reload that changes it is refused whole and says so.

## Consequences

- An operator turns confinement on for a whole machine, or for one agent or marker,
  in one place, and a client that sends nothing is no longer unconfined.
- `access: sandboxed`, a `posture` and a plain `sandbox` all meet the same comparison,
  so there is one answer to "may this create run with these grants here".
- A machine that requires a sandbox its runtime cannot enforce cannot start sessions
  in scope; `sandbox.policy` says so before the first create, and the refusal says
  why after it.
- The comparison is lexical. It does not resolve links and it does not know about a
  mount: a profile that grants a directory grants whatever that directory contains.
