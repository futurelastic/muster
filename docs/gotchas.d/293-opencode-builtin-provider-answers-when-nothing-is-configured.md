# An opencode session with no provider credential runs on the runtime's own hosted provider, and reports a normal idle

**Issue:** #293

## What happened

A trial created an opencode session on a machine with no provider credential and
sent it one task. The task completed. The session's own database showed the turn
ran on the runtime's built-in hosted provider. Create returned 201, `/input`
returned `queued`, state read `idle`: nothing in muster's answers distinguished it
from a session on a provider an operator chose, while its prompts, file contents
and tool output went to a third-party service.

## Measured (runtime v1.18.35, an isolated per-session directory, no credential)

| session config | `GET /config/providers` |
|---|---|
| none | one provider, the built-in hosted one, with public key and free models |
| `disabled_providers: ["<built-in>"]` | `{"providers":[],"default":{}}` |
| `enabled_providers: ["<some other>"]` | `{"providers":[],"default":{}}` |

So "nothing usable" is a positive answer in the runtime's own shape: the list is
present and empty. An endpoint that is absent, errors, or returns a body without
the list is a different thing (could not tell), and the driver keeps them apart.

## The rule going forward

- Fail closed by configuration: every session's own `opencode.json` carries the
  provider policy, written at create and at relaunch. Default disables the
  built-in provider; `FLEET_OPENCODE_PROVIDERS` is the whole allowed set.
- `enabled_providers` is a whitelist and wins over `disabled_providers`, so only
  one of the two is ever written.
- A catalog that lists nothing is a refusal, not a pass. The old reading ("could
  not tell, proceed") was right for an absent endpoint and wrong for an empty list.
- A turn's `providerID` on its assistant message is the only place the provider a
  turn actually used is visible; it is reported as a failed `lastTurn` when it is
  outside the set.
- Shared mode (one runtime the driver does not own) has no per-session file; the
  catalog filter and the turn report are its whole enforcement.

## Single models (#401)

The policy can also name models: `provider/<model>`, `provider/<glob>` and
`!provider/<glob>` (a deny that beats any allow), in the same variable. What that
changed, and what it could not:

- **Allow exact models; do not deny by name.** The built-in provider serves paid
  models, free ones whose terms allow retention or training on what is submitted,
  and zero-cost ones that carry no `-free` suffix at all (its own default, measured
  with no credential, is one). So `deepseek,opencode/deepseek-v4.1-flash,opencode/deepseek-v4-pro`
  keeps all the others out, and `!opencode/*-free` does not.
- **The session's own `opencode.json` cannot carry model rules.** `enabled_providers`
  is the only key the policy can write, and a glob could only be expanded against a
  catalog read from the very server that file configures. A provider allowed for some
  models is still enabled there; the model rules are enforced where this driver can
  act on them — the create's pinned model, a create that pins none, and the turn's
  record.
- **A create with no model pin lands on whatever the runtime picks.** Under model
  rules that pick is learned first: the model the session's merged configuration
  names for its directory (`GET /config`), else the `default` the catalog answer names
  for each provider it lists (measured: one entry per listed provider, e.g. the
  built-in provider's zero-cost default, with no credential). Every candidate must be
  allowed, and one that cannot be learned is refused, saying to pin a model. A policy
  of providers alone never makes this check. That a fresh session then runs on the
  default the catalog names has not been measured with a real turn.
- **A relaunched session is not re-checked at relaunch.** The session record stores no
  model, so a model pinned under an older policy is caught by the turn-time check only.
- Shared mode has no per-session file, so for model rules too the catalog filter, the
  create checks and the turn report are the whole of it.
