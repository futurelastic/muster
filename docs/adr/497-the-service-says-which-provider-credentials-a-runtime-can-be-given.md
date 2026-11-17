# 497 — the service says which provider credentials a runtime can be given

## Context

This service is the only component that starts an engine. It holds the binary path, the
key files wired through `sessionEnv`, the subscription grant store (#451), the credential
rule (#458) and the provider policy (#293, #401). A client that wanted to know whether a
runtime could use a provider had no way to ask it, so it guessed: it ran the engine's own
`auth list` from its own `PATH` (a different program, home and account from the one this
service starts) and copied this service's `sessionEnv` names as placeholders. The only
credential report was `grants`, which covers the subscription sign-in and nothing else.

The direction was ruled by the owner: this service stores and serves the engine facts, and
clients store only what people choose and check it against this service at create time.

## Decision

`GET /v1/runtimes` carries, per runtime, an additive `credentials` list of
`{provider, kind, source, state, reason?}`. The full contract is
`docs/spec/api-http.md` (§3.1). The points a reader of the code would not guess:

- **Names and states only.** A `reason` names the declared variable (`OPENAI_API_KEY`),
  never the file behind it or any content. It is answered from what the driver already
  holds; the only read is of each declared key file, made fresh as a create makes it, so a
  rotation shows at the next listing and the answer is the same with the engine's program
  on no `PATH`. Measured: the real service, the engine binary on no `PATH`, an operator
  openai key and no grant listed `openai · api-key · operator-env · usable`.
- **The grant row is built by the same function as `grants`** (`grantRow`), so the two
  listings cannot disagree about where the sign-in stands. `grants` stays as it is and
  keeps what only a sign-in has (`accessExpiresAt`, `refreshedAt`, `quota`).
- **A variable is matched to a provider by a short table, not by a guess.** The runtime's
  own catalogue (its `env` field per provider) shows why it cannot be derived: one name is
  read by several providers (`DASHSCOPE_API_KEY`, `MINIMAX_API_KEY`, `ZHIPU_API_KEY`, …),
  several providers need more than one variable, and the list moves weekly. So
  `keyEnvProviders` (`internal/drivers/opencode/credview.go`) holds only the names that
  exactly one provider reads, checked against that catalogue; a name it does not hold is
  **not listed** rather than attributed to a provider that might not read it. Extending it
  needs the same check. The alternative, reading the catalogue file the engine caches in
  the service user's home, was rejected: it is not the service's data, its place and shape
  are the engine's to change, and a machine without a cache would answer differently.
- **`engine-store` is a source on the wire this driver does not report.** Every session
  runs with an `XDG_DATA_HOME` of its own, so the store a person signed in to on the
  machine is under no session; listing it would promise a credential no session has.
  Shared mode (a server this driver did not start) lists no credentials at all, for the
  same reason: neither the operator's environment nor the sign-in reaches it.
- **The provider policy applies exactly as it does to `grants`.** A credential for a
  provider the policy does not permit is absent; a sign-in makes a provider usable, it
  does not allow it.
- **An entry scoped to particular agents or markers is listed; one scoped to other
  runtimes is not.** The row is of the runtime, so only the runtime axis of the scope is
  asked (`Scope.ReachesRuntime`, which `Matches` now shares).
- **A missing key file reads `degraded` whether the entry is required or not.** The reason
  says which it is for a create: a required entry refuses a create until repaired, an
  optional one lets sessions start without it.

## Consequences

- A client can offer and check a provider choice against the machine that will start the
  session, without running the engine's programs.
- The terminal runtime reports no credentials (absent, never "none held"), and a peer's
  row carries none: a peer's credentials are that peer's to say.
- The listing says what is held, not which a session gets. With both an API key and a
  sign-in for a provider, the `providerCredentials` rule chooses per session, and
  `usage.credential` says what a session ran on.

## Not done

- A credentials view for the terminal runtime (its own sign-in and `ANTHROPIC_API_KEY`).
- Passing a peer's `credentials` through on its row; a client asks that machine.
- A rule-aware field saying which listed credential a session in a given scope would get.
