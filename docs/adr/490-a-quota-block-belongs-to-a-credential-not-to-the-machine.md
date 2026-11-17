# 490 — a quota block belongs to a credential, not to the machine

## Context

The claude-code driver kept one quota block for the machine (`quota.json`,
`machine.quota`, `SourceStatus.quota`). A machine can run sessions on several
credentials at once: the sign-in the service runs under, and any token a create
hands a session through its `env` or the machine's declared `sessionEnv`. When
one credential reached its usage limit, every session on the machine read
`quota_blocked`, including the ones on credentials that still had quota, and a
supervisor obeying `machine.quota` stopped the whole machine.

The opencode driver already keeps its limit on the grant (#458), so only the
sessions on that grant are affected. This brings the claude-code driver to the
same shape.

## Decision

**A session's credential is named when it is created, and the block is kept per
name.**

- **The name** is derived from the environment the session is actually started
  with: the credential-bearing variables present among `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` and `CLAUDE_CONFIG_DIR`. None
  present is the constant `default`, the machine's ambient credential. Otherwise
  it is `c-` and twelve hex digits of a SHA-256 over a versioned prefix and the
  sorted `NAME=value` lines. It is opaque: comparable for equality, not for
  anything else. It is never the credential, and no value reaches state, an
  event, a log or the quota file.
- **Where it is kept**: a tmux session option, `@colab-credential`, which the
  create sets after `new-session` and the listing reads in the same call as
  everything else. It is set only for a session handed a credential of its own: an
  unset option reads back empty, and empty is `default`. A run this service did not
  create, one started before the option existed, and one whose option could not be
  set all read as `default`.
- **The block** is one per credential. It is set by a session on that credential
  showing the notice, and cleared by a session on it seen working or by any
  record on it showing a turn served after the block began (#489). Evidence from
  a session on another credential is never counted, in either direction.
- **The wire**: `QuotaBlock` gains `credential`; `machine.quota` is one event per
  credential transition, names it, and carries `quotas`, the credentials blocked
  once that change is applied, so an event is correct without the ones before it;
  `SourceStatus.quota` is replaced by `SourceStatus.quotas`, the list of blocked
  credentials.
- **The file**: `quota.json` is written as a map from credential to block. A file
  in the old single-block shape is still read, and its block is attributed to
  `default`.

## Why these, and not the alternatives

- **On the session, not in the per-session record file.** The record file is
  rewritten by every listing from the sessions it sees, and a create writes to it
  from another thread without a lock: a listing that enumerated just before the
  session existed and saved just after the create wrote its entry drops the entry
  for good. For a marker that costs a label. For a credential it would mean a
  session on a refused token reads as the ambient one for the rest of its life,
  which is the bug this fixes, delivered at random. The option has none of that: it
  is the multiplexer's own record of the session, so it lives exactly as long as
  the session, follows a rename, survives a restart of this service, needs no state
  store, and costs nothing to read.
- **Derived from the start environment, not read back from the process.** After
  the start nothing reliable says what a running process was handed: the login
  shell may add or override variables, and reading another process's environment
  differs per platform and needs privileges. What the service itself passed is
  the one fact it knows exactly.
- **A digest of all the credential variables present, not "the one the runtime
  prefers".** Which variable wins when several are set is the runtime's business
  and has changed. Digesting the set can only split sessions that share a
  credential (the block then reaches fewer sessions than it should, and each
  session's own screen still marks it); choosing wrongly could merge two
  credentials and block a session that has quota, which is the bug.
- **Unsalted.** The same token on two machines of a fleet names the same
  credential, so a supervisor reading both can see it. A salted name would need a
  secret of its own to protect, and a 48-bit prefix of SHA-256 of a long random
  token is no use to anyone holding it.
- **`SourceStatus.quota` is removed, not kept as a "first blocked credential"
  mirror.** A machine-keyed single block is the thing that was wrong. A mirror
  would keep telling a supervisor that reads only it to stop the machine, and
  would be a field whose meaning depends on how many credentials happen to be
  blocked. A client written for the old shape reads no block; the per-session
  `quota_blocked` status, which it must already act on, is unchanged for a
  session whose credential is blocked.
- **The old block goes to `default`.** It was machine-wide, so no session can be
  shown to be outside it, and the sessions handed their own credential are the
  ones that can prove it wrong. It does not outlast the first turn `default`
  serves after it began.

## What it cannot do

- **Two ambient credentials on one machine read as one.** A sign-in switched under
  the service, or a token in the service's own environment, are not visible to
  this service, so every session handed none of its own is `default`. That is the
  old behaviour, narrowed to the sessions that cannot be told apart.
- **A block on a credential no session runs on stays until the credential
  serves.** It is listed on the source, and the next session started with that
  credential is born into it. Nothing here expires a block by its reset hint,
  which is prose.
- **A rotated token is a new credential.** Sessions started with the old value
  keep the old name, and the block on it does not carry to the new one.
- **Routing is not part of the name.** A session pointed at another endpoint or
  another cloud provider through its environment may be on another account without
  its name saying so. Only the variables that carry a credential, or select a
  sign-in, are digested.
- **Machines on different versions disagree about a block.** A peer reads another
  machine's listing into the typed collection, so a source-level field the reader's
  build does not know is dropped: an older peer reads no `quotas` from this one, and
  this build reads no `quota` from an older one. The per-session `quota_blocked`
  status is unchanged on both, and is what a supervisor already acts on.
