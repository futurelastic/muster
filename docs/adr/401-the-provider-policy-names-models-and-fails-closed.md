# ADR: the provider policy can name single models, enforces them where the runtime cannot, and fails closed on an unknown default

**Issue:** #401
**Status:** decided

## Context

The policy (`FLEET_OPENCODE_PROVIDERS`, #293) held provider ids only. An operator
who wanted one paid model on a provider had to allow the whole provider and with it
every model it serves, including models whose terms allow retention or training on
what is submitted. The first example used a `-free` suffix deny; the owner then
corrected it: the built-in provider also serves zero-cost models with no such suffix
(its own default, measured with no credential, is one), so a suffix is not a reliable
mark of anything.

## Decision

- **The preferred form is an allowlist of exact models**
  (`deepseek,opencode/deepseek-v4.1-flash,opencode/deepseek-v4-pro`). Globs and
  `!provider/<glob>` denies exist and a deny beats any allow, but the docs, the doctor
  row and the tests say plainly that a name pattern is not a free-tier or
  data-retention filter.
- **Grammar:** `[!]provider[/pattern]`; `*` is the only wildcard and it runs over `/`;
  `?`, `[`, `]`, `\` are refused so they can gain a meaning later without changing an
  existing entry. A deny names models; `!provider` alone is refused. The model part is
  everything after the first `/`. The standard library's matchers were rejected: their
  `*` stops at `/` and they fail only when a match is attempted.
- **Model rules never reach the session's own `opencode.json`.** The file can enable
  whole providers only, there is no measured per-model key, and a glob could only be
  expanded against a catalog read from the very server the file configures. A provider
  allowed for some models is still enabled.
- **They are enforced at three checkpoints**: a pinned model at create (before any
  process exists), a create that pins none (below), and the turn's own record.
- **An unpinned create under model rules learns the runtime's pick first**: the model
  the session's merged configuration names for its directory (`GET /config`), else the
  default the catalog answer names for every provider it lists. Every candidate must be
  allowed. A pick that cannot be learned is refused, saying to pin a model: this policy
  exists to stop a session landing on a model nobody chose. A policy of providers alone
  makes no such check and no extra request.
- **A malformed entry stops the start**, naming it, through the shared start-up
  validation, so the doctor and a reload's refusal agree.

## Rejected

- *Writing a guessed per-model key into the session's configuration*: an unmeasured key
  the runtime may ignore would claim an enforcement it does not have.
- *Refusing every unpinned create under model rules*: it refuses defaults that are allowed.
- *Muster choosing and pinning an allowed model*: that puts a model nobody chose on the session.
- *Waiting out the settle window for defaults*: it costs seconds on every unpinned create.
- *Recording the model in the session record to re-check at relaunch*: it changes the
  record format and relaunch's failure modes.

## Consequences

- A session relaunched after a stricter rule is caught by the turn-time check only.
- An agent definition that pins its own model overrides the default at turn time and is
  not judged before the turn; the runtime's title or summary model is never seen by the
  policy. Both are noted as open (follow-up issues).
- That a fresh unpinned session then runs on the default the catalog names was measured
  for the shapes of the two reads (against the real runtime, with no credential) but not
  with a real turn.
