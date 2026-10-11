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
