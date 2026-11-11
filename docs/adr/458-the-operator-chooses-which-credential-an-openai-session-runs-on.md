# 458 — the operator chooses which credential an openai session runs on

## Context

Since #451 a machine can hold two credentials for the `openai` provider: a subscription
sign-in the service renews, and an API key in a session's environment. They are separate
accounts with separate metering. The subscription has plan usage windows shared by every
session on it; the key is billed per token. The owner ruled (on #451, 2026-11-10) that
running one subscription across many automated sessions is accepted, that every session
on it shares the plan's limit by design, that with both kinds on one provider id the
operator owns an explicit rule, and that usage reporting keeps the two kinds apart.

Nothing in the service decided which credential a session ran on. The runtime did.

## What was measured (runtime 1.18.3, the real binary, an isolated session directory)

Each run started `opencode serve` with its own HOME/XDG directories, the provider enabled
as muster writes it, and a logging forward proxy as the only way out. The credentials were
fake strings, so every turn failed at the proxy; the point is which host a turn connects to,
because the two credential kinds go to different hosts.

| `auth.json` `openai` OAuth entry | `OPENAI_API_KEY` in env | the turn connected to | `openai` models offered |
|---|---|---|---|
| yes | no | the subscription backend | 13 |
| no | yes | the API host | 66 |
| yes | yes | **the subscription backend** | **13** |

1. **With both present the runtime uses the sign-in and ignores the key**, and the provider's
   model list becomes the sign-in's (13 models against the key's 66). Nothing reports it:
   the listing still names the environment as the credential source and the turn ends
   normally on the sign-in's account. So today the key is dead weight on a signed-in
   machine, and the subscription pays for every such session.
2. **A usage-limit answer is not a failed turn.** Pointing the provider at a local server
   that answers HTTP 429 with a usage-limit body, a turn does not end: the runtime retries
   it (attempt 2, 3, 4 … 7 at a doubling delay, watched for over three and a half minutes with no end in sight) and the session sits in its
   `retry` status carrying the provider's message, the attempt and the time of the next
   try. Seen from outside it is a session that is slow to finish, with no error record to
   read. That is the symptom the issue described as "an unexplained failed turn per session".
3. **Not measured:** what a real exhausted ChatGPT subscription window looks like (the body,
   the status, whether a reset time is stated). It needs a live subscription; it is a row on
   #273. Nothing below depends on the number or the wording.

## Decision

**The operator's rule chooses, and the service enforces it by what is absent.**

- `providerCredentials` in the configuration file: entries `{provider, use, appliesTo}`,
  `use` being `subscription` or `api-key`, `appliesTo` the `sessionEnv` scope shape
  (`runtimes`, `agents`, `markers`). **First match wins, in file order**: a rule is a
  decision, so the order the operator wrote is the operator's own statement of which one
  applies. An entry that can never apply (written after one that matches everything it
  could) is refused at start, as is a misspelt runtime id, an unknown kind or provider.
- At every create and relaunch, before any directory or process exists:
  - `subscription`: the session's `auth.json` gets the sign-in's access token and the key
    is removed from its environment.
  - `api-key`: the session's `auth.json` gets no sign-in (an entry an earlier launch of the
    same directory wrote is removed, so a changed rule applies at the next relaunch) and
    the key stays.
  - The credential that is not named never reaches the session. That is what makes "no
    silent fallback" true by construction rather than by a check: there is nothing to fall
    back to, and a runtime release that changed its lookup order cannot change the account.
- **A named credential that is missing refuses the create with the reason**, when the
  session is certain to need the provider (a pinned `openai/…` model, or a policy naming
  `openai` alone). A session that can run on another provider is not refused for an account
  it may never use; it gets neither credential. The refusal says the other account is not
  used in its place.
- **A create's own `env` carrying the key is refused under a `subscription` rule.** The rule
  is the operator's and a create does not override it; a variable that silently vanished is
  the surprise this work removes.
- **No matching rule: a usable sign-in is used and the key withheld; no usable sign-in: the
  key.** This is the runtime's measured behaviour, decided here instead of left to it. It
  was chosen over "the key wins" because it changes nothing for a machine as it runs today,
  adds no per-token spend nobody asked for, and the shared plan is the accepted design. The
  doctor warns when both credentials reach sessions and nothing chooses.
- **The provider policy applies unchanged.** The rule chooses between credentials; where
  `FLEET_OPENCODE_PROVIDERS` does not permit `openai` the rule applies to no session.
- **Usage names the kind.** `usage.credential` is `subscription` or `api-key`, the account
  the service gave the session, so a client summing cost keeps the two apart. It is the
  service's own decision, not something read back out of the runtime.
- **An exhausted plan window is one state of the grant.** Sessions on the subscription are
  tracked by the runtime's own `retry` status: one at attempt 3 or later has been refused
  repeatedly, which is not a blip. While at least one is, the grant carries
  `quota: {state: "limited", since, observedAt, sessions, message}` on `GET /v1/runtimes`,
  and `runtime.opencode.grant` warns; when none is, it is gone. The mark is also kept in the
  grant store, written only through the refresher and refreshed every minute while true, so
  the offline doctor can read it and tell a live episode from one a stopped service left
  (a start clears whatever an earlier process left, because nothing can say it is still
  true). A new sign-in clears it; a refresh does not.

## Consequences

- The signal is the runtime's retry loop, not a cause. An exhausted plan window and any
  other persistent refusal of the sign-in read the same; `message` is the provider's own
  wording, passed through unparsed (this driver never branches on message text), and the
  doctor and the docs say so. Calibrating against a real exhausted window is #273's row.
- Sessions on an API key are not counted toward the grant's quota: their limits are per token
  and theirs alone.
- A change to the rule needs a restart, like `sessionEnv` (a reload that edits it is
  refused whole). A running session keeps its credential until it is relaunched; the rule
  is read again then.
- The credential is named on usage, which is absent until a first figure is read, and only
  while the newest request ran on `openai`. A session that has spent nothing yet does not say
  which account it will spend on.
- A create's own `env` values are not kept across a relaunch (only the names are recorded),
  so a key supplied that way is gone when the session is relaunched; an `api-key` rule then
  refuses a session bound for `openai`, naming the variable. The operator's `sessionEnv`
  entry is read again at every relaunch and is how the key is supplied under a rule.

## Not done

- A per-session `credential` outside `usage`, readable before the first figure.
- A reset time on the quota state: the runtime states none structurally, and the provider's
  wording is not parsed.
- A key rule for providers other than `openai`: it is the only provider with two ways in
  today, and a rule naming another is refused rather than silently inert.
