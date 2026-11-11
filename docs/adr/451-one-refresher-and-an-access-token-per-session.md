# 451 — one refresher holds the sign-in; a session holds only an access token

## Context

An operator wants to sign in to a ChatGPT subscription once per machine and have every
`opencode` session muster starts use the `openai` provider under it. The runtime ships that
sign-in (a browser flow and a device-code flow) and keeps the result in
`$XDG_DATA_HOME/opencode/auth.json` as an OAuth entry: access token, refresh token, expiry.
muster gives every session its own `XDG_*` directories (#280), so a session never sees a
sign-in the service user made.

Two facts make the obvious shapes wrong:

- The refresh token **rotates**: a refresh returns a new one and the old one stops working.
- So a copy of the grant per session breaks every copy but the first to refresh, and one
  shared file has separate processes racing to refresh the same token.

## What was measured (runtime 1.18.3, the real binary, an isolated data directory)

1. **A running server reads `auth.json` again before every provider request.** The runtime's
   `openai` OAuth loader is handed an auth getter and calls it at the start of each outbound
   fetch; the getter reads the file with no cache. Taken on a live server with no restart: a
   turn with one access token failed as an unauthorized API error; the file was replaced by a
   rename with an expired access token and a placeholder refresh token, and the next turn
   failed as `Token refresh failed: 401`; replaced again with a future expiry, and the turn
   failed as the unauthorized API error again. So a renewal needs **no relaunch**.
   One caveat shapes the design: whether the provider is usable at all, and which of its
   models are offered to a sign-in rather than a key, is decided when the server starts. The
   entry has to be in the file before the server starts, at a create and at a relaunch.
2. **An expired access token with an unusable refresh token is distinguishable.** The turn's
   record carries `UnknownError: Token refresh failed: <HTTP status>`; an access token the
   provider cannot parse is an API error with status 401 and its own message. The token
   endpoint answers a refresh token it does not know with HTTP 401 and an error code
   `token_expired`. The runtime never wrote the file back in any of these runs, and a session
   that refreshes with a placeholder posts a token the provider rejects: nothing rotates.
3. **How long an access token lives was not measured.** It needs a live subscription, which
   the session that wrote this did not have. Nothing depends on the number: the renewal point
   is a quarter of the token's own lifetime before it ends (between five minutes and a day),
   read from the stored expiry, and `muster grant login` prints the real figure.
4. **What a usage-limit hit looks like on a turn was not measured**, for the same reason.
   The runtime has no branch of its own for it: a quota code from the backend becomes an API
   error, and other limit hits arrive as an HTTP 429 API error. Classifying it belongs with
   the failed-last-turn work (#426) once a real one has been seen. *(Measured later, on
   #458, against a stand-in provider answering 429: the runtime keeps retrying it, so no
   failed turn arrived in over three minutes; the session waits in its `retry` status. The grant-level
   quota state in `docs/adr/458-*` is built on that, and a real exhausted window is still
   unseen.)*

## Decision

muster holds the grant and is its **only refresher**.

- The grant lives in an owner-only store under the state directory (`grants/openai.json`),
  written whole beside the target and renamed over it, under a lock file, with a generation
  counter. `muster grant login` (the device-code flow) or `muster grant import` writes it;
  a refresh keeps its result only if the generation it started from is still the stored one,
  so a sign-in made while a refresh was in flight wins.
- The service's refresher renews it before it lapses, one refresh at a time, and **persists
  the rotated refresh token before anyone is told about the new access token**. If the write
  fails the rotated grant is held in memory and the write retried, because the provider has
  already killed the old token.
- A session's own `auth.json` gets the access token, the expiry and the account id, and a
  fixed placeholder where the refresh token goes. It is written before the server starts, at
  a create and at a relaunch (a parked session wakes with a current token), and rewritten in
  place on every renewal for every live session. A start reads the token once more after its
  server is tracked, so a renewal in the gap between the two is not lost.
- The provider policy applies unchanged: the entry is written only into a session whose
  policy permits `openai`. A sign-in makes the provider usable; it does not allow it.
- A revoked sign-in is a machine state, not a run of failed turns: the refresher marks it in
  the store when the provider answers 401 (or a 400/403 naming a grant code), and
  `GET /v1/runtimes` carries `grants`, `muster doctor` has `runtime.opencode.grant`, and a
  create the missing provider refuses names the sign-in. A failure that says nothing about
  the grant (the provider unreachable, 5xx, 429, a wrong address) is `degraded`, retried with
  a backoff, and never revokes: a malformed request of our own must not kill a good grant.

## Consequences

- A session can read its access token, as it can read any model-provider key it is given;
  isolation does not claim otherwise. It cannot keep the sign-in alive: it has no refresh token.
- After `import`, the source file still holds the original refresh token; the operator signs
  it out, or it stops working the first time muster renews. `import` says so.
- A `login`, `import` or `logout` is noticed by the running service within half a minute (it
  polls one small file), and a changed sign-in is pushed to live sessions then.
- A peer's `grants` are not reported: the key is absent on a peer's row, which reads as
  "nobody said".
- Whether one personal subscription may serve many automated sessions is the provider's terms
  and the owner's decision. Nothing is signed in, and nothing is given to a session, until an
  operator chooses to.
- A machine holding both this sign-in and an API key for the same provider does not leave the
  choice to the runtime: the operator's `providerCredentials` rule decides which a session
  runs on (`docs/adr/458-*`), and only a session on the subscription is given the token.

## Not done

- A proxy that injects the token on the session's behalf, which would keep even the access
  token out of the session. It would mean reproducing the runtime's request shaping for this
  backend.
- Removing the entry from live sessions on `logout`: they keep the token they hold until it
  lapses, which `logout` says.
