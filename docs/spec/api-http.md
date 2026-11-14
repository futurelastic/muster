# HTTP API — specification

Concrete wire protocol for the model in
[`session-abstraction.md`](session-abstraction.md). Where the two disagree, the
abstraction wins and this document is the bug.

**Status:** implemented and running, across two machines. The surface is not
frozen — it has changed several times under contact with a real driver, and it
will change again — but it is no longer aspirational: every endpoint below is
served, and the federated ones have been exercised peer to peer.

---

## 1. Conventions

- JSON in, JSON out. `Content-Type: application/json`.
- Timestamps are RFC 3339 with offset, in the **stamping machine's** clock
  (§11). Every response carries `Fleet-Clock: <rfc3339>` so callers can compute
  skew.
- Sessions are addressed positionally — `/machines/{machine}/sessions/{id}` —
  rather than by a composite identifier in one path segment. There is no
  fleet-wide id to encode (§7.1), and the hierarchy makes the machine scope
  visible at every call site.
- A call that resolves a local session driver carries `Fleet-Runtime:
  <runtime-id>`, and `Fleet-Runtime-Resolution: default` when a configured
  default runtime was the tiebreak (session-abstraction.md §7.1a). See §3.3's
  `rename` entry for the full rule.
- A `runtime` query parameter on a session-addressed call to a **peer** is
  forwarded to it unchanged, so the peer's own resolution can use it (§3.3's
  `rename` entry); a call that gave none is forwarded exactly as before.

## 2. Error model

```json
{
  "error": {
    "kind": "not_found",
    "message": "no session with that id",
    "machine": "<machine-id>",
    "retryable": false
  }
}
```

| `kind` | HTTP | Meaning |
|---|---|---|
| `invalid` | 400 | Malformed request |
| `unauthorized` | 401 / 403 | Caller not permitted for this verb on this machine; or a principal pinned to this machine that named another (`reason: "local-only"`, §5) — no grant would help |
| `not_found` | 404 | The machine answered, and there is no such session |
| `conflict` | 409 | Idempotency key reused with a different body; a replayed key whose session has since ended (`reason: "replay-of-ended-session"`, §3.3); or a destructive request whose `startedAt` disagrees with the live session (§5.4) — the request is well formed, the caller's belief is stale |
| `unsupported` | 501 | Driver lacks the capability (§4.3 of the model); or a runtime this machine's operator enabled has no working driver and no retry under way (`reason: "runtime-unavailable"`, §3.1); or, on macOS, a create that would start the terminal runtime's session server from a context other than `Background` (`reason: "unsafe_server_context"`, `retryable: false`, §3.1); or a create whose `posture` the resolved runtime cannot enforce (`reason: "posture-unenforceable"`, `retryable: false`, §3.2); or the record route on a machine whose operator refused it (`reason: "record-off"`, §3.2) — no grant would help |
| `unreachable` | 504 | **The machine did not answer.** Nothing is known. Also a runtime that is enabled and still being retried (`reason: "runtime-unavailable"`, `retryable: true`, §3.1); and a call that ran out of time — the request's deadline, a step's own budget (the message names it, §3.3), or a caller that hung up — with `retryable: true`, **never `invalid`**: running out of time refuses nothing the caller asked for |

**`not_found` and `unreachable` must never be conflated.** One means the fleet
knows the session does not exist; the other means the fleet knows nothing at
all. This is §5.7 expressed at the wire, and it is the single most important
line in this document: a client that treats 504 as 404 will confidently report
work as gone while it is running fine on an unreachable host.

**`reason` further classifies some bodies beyond what `kind` alone
distinguishes**, so a caller can branch without parsing `message`. Absent
unless a case documents one. Two cases do today:

- `replay-of-ended-session` on a `conflict` — `create`'s own (§3.3). It also
  carries `session` (the ended session's own ref) and `closedAt` (when this
  machine observed the absence, an upper bound on the true end — the same rule
  `GET .../sessions/closed`'s own `closedAt` follows for a `ClosedByAbsent`
  tombstone, §3.2).
- `runtime-unavailable` on an `unreachable` or an `unsupported` (muster #297)
  — a call named, or was routed to, a runtime the operator enabled and that has
  no working driver at this moment. See §3.1 for which kind carries it and why.
- `unsafe_server_context` on an `unsupported` (muster #336) — a `create` that
  would start the terminal runtime's session server from a macOS context other
  than `Background`. `retryable: false`; see §3.1.
- `posture-unenforceable` on an `unsupported` (muster #344) — a `create` whose
  `posture` the runtime it resolved cannot enforce; `message` names what is
  missing. `retryable: false`; see §3.2.
- `machine-list-*` on `invalid`, `unsupported`, `not_found` and `conflict`
  (muster #352) — the machine-list routes' own refusals, one reason per failed
  check (§3.1, *The machine list*). The kind says whose fault it is; the reason
  says which check, so a caller does not parse `message` to learn whether to
  re-sign the list, ask an operator, or try again.

**An id that is a live session's conversation id (muster #268).** `{id}` is
always this service's session `id`; a session's *conversation* id (the runtime's
own, `conversation.id` on a session read) is not an address and is never
accepted as one — it is not a stable handle, because a runtime can start a new
conversation inside the same process and two live sessions can hold one
conversation after a resume. But a session knows its own conversation id and not
its session id, so a reply-to address written by one session tends to carry the
wrong kind, and a bare "no session with this id" reads as "the session is gone".
So when `{id}` matches no session id, is UUID-shaped, and equals the
`conversation.id` of one or more **live** sessions on that machine, the refusal
says so and names the id to use. It applies to every route keyed by a session id.

- A **send** (`/input`, `/respond`) is still `200 { "outcome": "refused" }`
  with nothing written. `reason` is prose (do not parse it) saying the value is
  a conversation id and naming the session id(s); the same ids arrive in
  `sessionIds` (sorted, every candidate when several sessions hold the
  conversation). Retry with one of them.
- A **read** (the single-session GET, `/turns`, `/keys`, close, rename) is still
  `404 not_found`, with `reason: "conversation-id"` and `sessionIds`.
- An id that matches neither a session id nor a live session's conversation —
  including a UUID that is nobody's — keeps today's answer: reason
  `"no session with this id"` on a send, plain `not_found` on a read, and
  neither `sessionIds` nor the `conversation-id` reason. A client may rely on
  that: the session-gone-or-never-existed answer is the only one without them.

## 3. Endpoints

### 3.1 Service and topology

```
GET /v1/health
→ 200 { "epoch": "...", "cursor": 12904, "startedAt": "...",
        "build": { "known": true, "revision": "...", "modified": false,
                   "time": "...", "go": "go1.26.5",
                   "version": "v0.1.0-2-g3ce7e27" },  ← null when unstamped
        "maxInputBytes": 32768,
        "labels": { "maxKeys": 16, "maxKeyBytes": 128, "maxValueBytes": 128 },
        "supportsConversationId": true,
        "supportsIsolateEnvironment": true,
        "supportsIsolateConfiguration": true,
        "supportsSandbox": true,
        "supportsLaunchSettings": true,
        "launchSettingsOutsideBypass": ["crossSessionInbound"],
        "drivers": [ { "machine": "...", "runtime": "...",
                       "status": "available",
                       "capabilities": { ... } },
                     { "machine": "...", "runtime": "...",   ← enabled, no working driver
                       "status": "unavailable",
                       "cause": "probe-timeout",
                       "reason": "the ... runtime is enabled, but ...",
                       "capabilities": { "deadlineMs": 30000,
                                         "source": "assumed" } } ],
        "listSnapshot": { "served": 0, "built": 0, "bypassed": 0 },   ← §3.2, counted since start
        "modules": { "enabled": ["relay-a", "relay-b"],   ← only when a delivery module is configured (#414)
                     "loaded": ["relay-a"],
                     "skipped": [ { "name": "relay-b",
                                    "reason": "no executable installed" } ] } }

GET /v1/machines[?verify=1]
// A principal pinned to this machine (§5, #395) is answered with the self item
// and its one source only, and no peer is probed — `verify` included.
→ 200 { "items": [ { "machine": "...", "self": true, "status": "ok",
                     "observedAt": "...",
                     "build": { "known": true, "revision": "...",
                                "modified": false, "time": "...",
                                "go": "go1.26.5" },
                     "maxInputBytes": 32768,
                     "address": "http://...",   ← a peer configured with several only
                     "peer": { "listsMeBack": true,
                               "grantsToMe": ["read", "send"],
                               "source": "observed",
                               "observedAt": "...",
                               "auth": "key" } } ],   ← "key" | "token"; absent on self (#350)
        "sources": [...], "complete": true }

GET /v1/runtimes
→ 200 { "items": [ { "machine": "...", "runtime": "...",
                     "status": "available",
                     "capabilities": { "observesState": true,
                                       "deliversRawKeys": true,
                                       "confirmsDelivery": true,
                                       "supportsResume": false,
                                       "deliversToInbox": false,
                                       "isolatesEnvironment": false,
                                       "isolatesConfiguration": true,
                                       "sandbox": { "mechanism": "macos-sandbox-exec",   ← or "linux-landlock-seccomp" (#439)
                                                    "denies": ["files", "unixSockets", "systemServices"],
                                                    "network": ["open", "closed"],
                                                    "packageCache": ["shared", "private"],
                                                    "packageManagers": ["cargo", "go", "npm"] },  ← absent where cannot (#432)
                                       "sandboxUnavailable": "…",   ← why it is absent, where it could in principle be offered (#345)
                                       "supportsPin": { "model": true,
                                                        "effort": false,
                                                        "agent": true },
                                       "validatesModel": true,
                                       "remoteControl": { "toggle": true,
                                                          "off": true },
                                       "readsTurns": true,          ← these six are absent when the runtime
                                       "readsRecord": true,            did not declare them: NOT DECLARED,
                                       "recordFormat": 1,              never "cannot" (#379, #441, #444);
                                       "reportsConversation": true,    recordFormat is present exactly when
                                                                       readsRecord is true
                                       "respond": { "kinds": ["tool-permission"],
                                                    "unclassified": false },
                                       "permissionModeChange": { "live": false },
                                       "source": "observed",
                                       "observedAt": "..." },
                     "posture": { "unattended": true, "files": true,   ← what `posture` can ask of it (#344);
                                  "network": ["open", "closed"],        absent on a peer not yet reached
                                  "isolatedEnvironment": true,
                                  "packageCache": ["shared", "private"],   ← the modes and managers a posture's
                                  "packageManagers": ["cargo", "go", "npm"] },  package-cache seed may use (#432)
                     "models": { "source": "runtime",           ← "runtime" | "declared" | "none" (#400)
                                 "readAt": "...",                ← only when "runtime"
                                 "ids": ["deepseek/deepseek-flash"],
                                 "default": null },
                     "grants": [ { "provider": "openai",        ← a provider sign-in this machine holds (#451);
                                   "state": "ok",                 absent when none; this machine's own rows only
                                   "accessExpiresAt": "...",
                                   "refreshedAt": "...",
                                   "quota": { "state": "limited", ... } } ] },   ← only while the provider refuses the sign-in (#458)
                   { "machine": "...", "runtime": "...",         ← enabled, no working driver
                     "status": "unavailable",
                     "cause": "probe-timeout",
                     "reason": "the ... runtime is enabled, but its version check did not answer in time; retrying in the background (attempt 3 failed)",
                     "capabilities": { "deadlineMs": 30000,
                                       "source": "assumed" },
                     "models": { "source": "none", "ids": [], "default": null } } ],
        "sources": [...], "complete": true }

POST /v1/posture/dry-run                                        ← needs only `read` (#344)
{ "posture": { "attendance": "unattended", "files": {"writePaths": ["/abs"]}, "network": "closed" } }
→ 200 { "runtimes": [ { "machine": "...", "runtime": "...", "enforceable": true, "known": true,
                        "mapping": { "permissionMode": "bypass",
                                     "sandbox": {"writePaths": ["/abs"], "network": "closed"},
                                     "isolateEnvironment": false,
                                     "isolateConfiguration": false } },
                      { "machine": "...", "runtime": "...", "enforceable": false, "known": true,
                        "reason": "create: posture cannot be enforced by this runtime: ..." } ] }

GET /v1/whoami[?machine=<id>][&peer=<id>]
→ 200 { "principal": "...", "machine": "...", "grants": ["read", "send"],
        "source": "observed", "scope": "fleet", "listsYou": null }
```

`scope` is `"local"` for a principal pinned to this machine (§5, muster #395)
and `"fleet"` for every other, including a machine in single-token mode. It is
present when the report is about the machine that answered and omitted when it
names a peer, where this service cannot know. A pinned principal that names a
peer is refused (`401`, `reason: "local-only"`), not given the `assumed` answer.

**`/v1/whoami` reports the presented credential's own grants, and nothing
about any other principal** (muster #106, session-abstraction.md §7.7).
Unlike every other read route, it does not require the `read` grant — see §5
— because a principal holding no grants at all is exactly the caller who
needs it most. `machine` defaults to this service's own id; naming a peer
always answers `source: "assumed"`, `grants: []`, the same conservative-floor
meaning `/v1/runtimes` already gives an unreached peer's capabilities —
reused rather than reinvented, because it is the identical "nobody has told
me anything" fact. This service has no mechanism to learn what a peer has
granted a credential (unlike a peer's driver capabilities, which are probed
and cached), so a peer's real answer must be read from that machine directly.

**`peer=<id>` is how a service asks a machine whether it is listed there**
(muster #154). When the report is about this machine and the credential
holds `read`, `listsYou` is `true` or `false` — whether `<id>` is in this
machine's own peer roster. Otherwise it is `null`: nothing asked, a report
about another machine, or a credential without `read`, because the roster is
this machine's configuration and `read` is what guards it. The key is
**always present** on a build that has it, so an absent key means only "this
build predates the field". The id is the caller's assertion; that is
acceptable because the answer is a report and grants nothing.

Clients **must** consult `/v1/runtimes` before relying on a capability, and
degrade rather than assume. A driver never emulates (§5.6).

**A peer is listed as one row per runtime it runs** (muster #459). A machine
that runs more than one runtime shows up here as several items with the same
`machine` and different `runtime`, in runtime-id order, each with its own
`capabilities`, `posture` and `models`; one that runs a single runtime shows one
row, as before. The rows of a peer come from the one read of that peer's own
`/v1/runtimes` that this machine caches, so they are observed together and go
stale together: past the cache's age every row of that peer reads
`source: "assumed"` with no `posture` and `models` of `none`, and its one entry
in `sources` is `degraded`. A runtime the peer lists as `unavailable` (#297) is
not a row of that peer here, and a peer that has not answered has no row at
all, only its `degraded` source — a client never sees a runtime nobody named.
What is true of the machine rather than of a runtime — whether it answers, which
build it runs, what this machine may do there — is reported once, in
`/v1/machines`.

**`models` is which models a create on that runtime may name** (muster #400), so a
client can offer a picker instead of a free-text box.

```
RuntimeModels {
  source   : "runtime" | "declared" | "none"
  readAt?  : Timestamp           // only for "runtime": when the runtime last answered with this list
  ids      : string[]            // the runtime's own spelling; [] (never null) when source is "none"
  default  : string | null       // what the runtime uses when a create names no model, if known
}
```

- `runtime` — the runtime itself listed these when it was last asked. The ids are
  exactly the ones a create on this machine would accept at the time of that
  read, so a client may offer them as the choices. The create stays the
  authority: the list can lag a change of credentials or of the runtime's own
  catalogue, and it is empty of any model the machine's operator does not
  permit.
- `declared` — the runtime has no catalogue to ask, so the operator's
  configuration, or the aliases the runtime documents, name these. A value
  outside the list may still work, and **a client must still accept one**.
- `none` — this runtime cannot say, or has not yet (nothing has been read since
  the service started). `ids` is `[]`. A client keeps free text.
- **The set of `source` values is open on read:** a client must treat one it
  does not know as `none`. A row from a build that predates the field has no
  `models` at all, and that reads as `none`, never as "no model works".
- No request is made to build the list: it is answered from what the service
  already holds, as the rest of the row is. For a peer it comes from the same
  cached row as its `capabilities`, so it is `none` while that cache is unreached
  or stale. A peer's `readAt` is on **that peer's** clock and is passed through
  unchanged.
- `readAt` is omitted, not null, where nothing was read. `default` is always
  present and is null when it is not known with certainty.

**`grants` is where a provider sign-in this machine holds for the runtime stands** (muster
#451), so "every turn on this provider fails" has a reason a client can read.

```
RuntimeGrant {
  provider         : string       // the runtime's own id for the provider ("openai")
  state            : "ok" | "signed-out" | "degraded" | "revoked"
  accessExpiresAt? : Timestamp    // when the stored access token stops working; absent when signed out
  refreshedAt?     : Timestamp    // when the sign-in was last renewed, else when it was made
  reason?          : string       // one sentence for a person; present unless state is "ok"
  quota?           : GrantQuota   // present only while the provider keeps refusing requests made with the sign-in (#458)
}

GrantQuota {
  state      : "limited"    // the set is open on read: treat one you do not know as limited
  since      : Timestamp    // when the first session on the sign-in was seen refused, in this episode
  observedAt : Timestamp    // when this service last saw it still so
  sessions?  : integer      // how many sessions on the sign-in are refused right now; absent when the answer is read from a stored mark
  message?   : string       // the provider's own message as the runtime reported it, cut short; never parsed
}
```

- `ok` — signed in and renewing; a session created now gets a working token.
  `degraded` — the last renewal failed for a reason that is not the sign-in's fault (the
  provider unreachable or failing) and is being retried; the token in sessions may still
  be good until `accessExpiresAt`. `revoked` — the provider refused the stored refresh
  token; nothing renews it and a person signs in again. `signed-out` — none is stored.
- The key is **absent**, not `[]`, on a runtime that holds no sign-in, on a machine whose
  provider policy does not allow the provider, and on every peer's row (nobody reported).
  Absent never reads "signed out". **The set of `state` values is open on read:** treat
  one you do not know as not usable.
- It never carries a credential, and it is answered from what the service holds: no
  request is made to a provider to build it.
- **`quota` is a state of the sign-in, not of any one session** (muster #458). Every
  session on a subscription shares one plan and its usage windows, so when the window
  runs out it runs out for all of them at once and they recover together. The runtime
  does not fail such a turn: it keeps retrying a usage-limit answer with a growing delay, so a session
  waits in its own retry loop (`status: "working"`, `lastTurn.retryable`). `quota` is
  that wait, said once for the account: it is present while at least one session on the
  subscription has been refused repeatedly (the runtime's retry attempt is 3 or more), and
  absent the moment none is. The signal is what the runtime states and the cause is **not
  parsed**: an exhausted plan window is the case it exists for, and `message` is the
  provider's own words for which it was. It does not change `state` — a limited plan is
  still a good sign-in — and it is not the session-level `quota` block of a `quota_blocked`
  session, which is one session's own report. Sessions on an API key are not
  counted: their limits are per token and theirs alone.

**A runtime the operator enabled is listed even when it has no working
driver** (muster #297). It used to be omitted — a runtime whose version check
was killed by its own deadline, on a machine busy restarting, simply was not
there until the next restart, and from outside that read exactly like a
runtime nobody had ever configured. Now:

- `status` is `available` or `unavailable`. A row from a build that predates
  the field has no `status`, and **an absent `status` means available**: such a
  build only listed runtimes with a working driver.
- An `unavailable` row also carries `cause` and `reason`. `cause` is one of
  `not-installed` (the runtime's binary was not found), `not-executable` (it is
  there and cannot be run), `probe-timeout` (its version check did not answer
  within its deadline), `probe-failed` (the check ran and failed some other
  way), `start-failed` (the runtime was refused for a reason that is not its
  binary: its configuration, or the platform). **The set is open on read:** a
  client must treat a `cause` it does not know as "unavailable, reason
  unstated". `reason` is a sentence for people and never carries a path.
- **Only `probe-timeout` is retried.** The service tries again in the
  background, with a doubling wait, without an attempt limit; when the check
  answers, the runtime is registered and its row becomes `available` — no
  restart. Every other cause waits for somebody to change something, and the
  row stays until the service restarts.
- **`available` means a session can start** (muster #342). A runtime whose
  driver registers without the program each session runs — the terminal
  runtime needs only its multiplexer — is listed `unavailable` with
  `cause: "not-installed"` while that program is not found where a session
  starts it, after the rows that work. This cause is not remembered: the
  service re-checks (briefly cached), so the row is `available` again once the
  program is installed, with no restart. A check that could not be made leaves
  the row `available`. A create naming or routed to it is refused with `501`
  `unsupported`, `reason: "runtime-unavailable"`, before any session exists.
- An unavailable row's `capabilities` are the conservative floor — every flag
  false, `source: "assumed"`, the deadline its driver will declare. They
  describe nothing the runtime can do; do not act on them.
- **Row order is part of the contract:** rows for runtimes with a working
  driver come first, then unavailable rows, then peer rows. A reader that takes
  the first row for a machine — an older peer driver does — therefore never
  takes an unavailable one.

**A call that names, or is routed to, an unavailable runtime says so**,
instead of the `not_found "unknown runtime"` an id nobody enabled gets. The
error carries `reason: "runtime-unavailable"`: `unreachable` (504) with
`retryable: true` while the runtime is being retried — nothing is known yet,
ask again — and `unsupported` (501) with `retryable: false` for a cause that
needs somebody's hands. The runtime's `cause` is on `GET /v1/runtimes`.

**`drivers` on `GET /v1/health` lists an enabled runtime with no working
driver too** (muster #300), with the same `status`, `cause` and `reason`, in
the same order (registered first, then unavailable, each by runtime id), and
the same conservative `capabilities`. Absence there would read as "never
enabled" for the same reason it did on `GET /v1/runtimes`. A `drivers` row from
a build that predates #300 has no `status` and is available; a reader that
needs to know whether a runtime can serve reads `status`, never the row's
presence.

**`modules` on `GET /v1/health` says what became of the external delivery modules
the operator enabled** (muster #414; `FLEET_DELIVERY_MODULES`, #185). A module
that is enabled and not found is not an error: the daemon logs one line, answers
`ok`, and sends through the built-in terminal path. Before this key, nothing but
that line told an updater or a monitor that a delivery lane had gone. `enabled`
is every valid name the operator listed, in their order of preference; `loaded`
is the subset whose executable was found and passed the checks at startup; and
`skipped` is each name that was not loaded with a short `reason` — a fixed phrase
(`no executable installed`, `modules directory absent`, …), never a path. A listed
name that is not a legal module name is never in `enabled`, but is in `skipped`
(reason `not a legal module name`): it was configured and dropped the same way. A
reader that wants "something configured is not running" tests `skipped` for
non-empty. The three are arrays, never `null`. **The key is absent when no module
is configured**, so a machine that does not use the feature answers as it always
did, and absence reads "none configured", never "none skipped". `loaded` is a
statement about discovery at startup, not about whether the module process is
answering now; a send's receipt names the route that delivered it
(`delivery.route`, §3.2). A build that
predates #414 has no key and a reader treats that as "unknown", not as "fine".

**`capabilities.serverContext` on a macOS host's terminal runtime names the
security-session context its sessions run in** (muster #336). Every process in a
session server inherits the context of whoever started the server, and an agent
runtime that moves its saved login into the login Keychain loses it when the
same sessions are reached from the `Background` context. The field is
`{ "service": "Background", "server": { "pid": 4242, "context": "Background" },
"warning": "..." }`: `service` is this service's own context (`launchctl
managername`), `server` is the running session server's (absent when none is
running), and each is `"unknown"` when it could not be read. `warning` is set
whenever either is not `Background`. **A server already running in a desktop
(`Aqua`) context is a warning even when the service is fine**, because the
service attaches to an existing server and cannot change its context. The
field is absent on every other platform and runtime, which means nothing was
measured, not that the context is safe.

**A create never starts the session server from a context other than exactly
`Background`.** When no server is running and this service's context is
`Aqua`, another launchd session, or one it cannot read, the create is refused
`unsupported` (501), `retryable: false`, `reason: "unsafe_server_context"`,
before any side effect. A create into a server that already exists starts
nothing and is not refused. An operator may override the refusal only with an
explicit setting (`FLEET_TMUX_ALLOW_DESKTOP_SERVER=1`), never by default.

**`deliversToInbox` is how an operator confirms muster #119's inbox
delivery path is actually live on a machine, without inferring it from a
delivery receipt's wording** (muster #122). `false` means every send on
that runtime falls through to the pane path unconditionally; `true` means a
resolver is configured, and *some* targets may take the inbox path — which
ones is still decided per call and is not this field's job to say. A merged-
but-unconfigured deployment reads `false` here exactly like a machine that
never adopted #119 at all — the honest answer, since from a caller's side
those two states behave identically.

⚠️ **`true` is a statement about wiring, never a prediction about a given
send, and muster #148 widened that gap deliberately.** A send now also
needs the target's permission-mode class, supplied per target by the same
machine-local index the address comes from; without it the send cannot be
attested, so a named inbox send is refused (`auto` never tries the inbox since #257). So a machine whose index writer
does not yet emit that class reads `true` here while using the inbox for
nothing at all. That is the intended state — sending unattested is what #148
measured as 206 sends reported delivered and in fact held by the receiver and
dropped — but a reader treating this flag as "the inbox will be used" will
misread it. The field's contract is unchanged; only the number of ways a send
can decline the path has grown.

They must also consult `source`. `assumed` means nobody has confirmed these
values and they are a conservative floor — reading them as the runtime's answer
is how a temporarily unreachable peer becomes a permanently incapable one.

**`build` identifies the code, and `known: false` is not a match.** Two
services that disagree may be two services running different vintages, and
those need opposite responses: one is a bug, the other is a deploy. A client
comparing builds **must** treat an unstamped or `modified` build as
*unverifiable* rather than as equal — an unmodified pair of equal revisions is
the only comparison that means anything, and the failure this field exists to
catch is precisely a confident conclusion drawn from an absent measurement.

**`build.version` answers a different question than `revision`** (muster
#161): not "is this exact commit running?" but "is this at least release X?".
It is `git describe --tags` at the built commit, stamped at link time — the
toolchain records no tag, so it cannot be derived at runtime. A service
**must** report `null` when unstamped and **must not** substitute a default
such as `v0.0.0`, which a version floor would compare against on no evidence.
A client enforcing a floor **must** treat `null` or an absent field as
*unverifiable*, distinct from "below the floor". The release is the tag part;
a trailing `-N-g<sha>` means N commits after it. `version` never participates
in build equality.

**`/v1/machines` carries the same `build` object per entry** (muster
#121) — self is always known (read once at startup), a peer is whatever the
last successful probe learned. This is the answer to "is a merged guard live
here": compare `revision` against the commit that added the guard, or two
peers' `build` objects against each other with `Build.SameAs` — never by
sending the input the guard exists to reject just to observe whether it is
rejected. Before a peer has ever been reached, or for a peer driver that
cannot report one, its entry carries the zero value (`known: false`) — that
means "not yet observed", the same meaning `assumed` carries elsewhere on
this page, not "running old code."

**`maxInputBytes` is this machine's effective limit on `prompt` (create) and
`text` (input)** (muster #130) — the same ask-do-not-infer move #121
made for `build`, applied to the length cap §3.3 documents below: a caller
sizing a dispatch brief can ask rather than discover the boundary by
exceeding it. It matters more once the value is machine-local (§3.3 again)
and can differ across the fleet — a caller talking to two machines cannot
assume one number. `GET /v1/health` reports this machine's own value
directly; `/v1/machines` carries it per entry the same way it carries
`build`: self is always known and positive, a peer is whatever the last
successful probe learned. Unlike `build`, an unanswered peer's entry needs
no separate `known` flag — a real effective limit is never zero, so the
zero value is unambiguous on its own as "not yet observed."

**`address` on a `/v1/machines` item is the address this service is using to
reach that machine** (muster #335): the one that answered last, for a peer
configured with more than one. It is omitted — not empty — for self and for a
peer with a single address, which has nothing to say its configuration does not
already. A request is tried on the next address only when no connection could be
made to the current one; once connected it is never replayed elsewhere, because
the peer may already have acted on it.

**`peer` on each `/v1/machines` item is this service's own standing on that
machine** (muster #154) — the one credential a service holds that a peer
knows, its own, and whether that peer's roster lists it back. Federation is
two hand-kept halves of configuration, and before this their disagreement was
discoverable only by a 403 at the moment of need.

```
PeerStanding {
  listsMeBack : boolean | null   // that machine's roster names this service
  grantsToMe  : string[]         // grants that machine gives the credential this service presents
  source      : "observed" | "assumed"
  observedAt? : Timestamp        // this machine's clock; absent under "assumed"
  auth?       : "key" | "token"  // how this service authenticates to that machine (#350); a fact of
                                 // this machine's own configuration, so present under "assumed" too
}
```

It is gathered on the peer probe that already learns `build` and
`maxInputBytes`, by calling that peer's `GET /v1/whoami?peer=<self>` with the
credential this service presents there — so it adds no request class a peer
has not already accepted, and needs no grant beyond `read` to be read here.
Three answers **must not** collapse into one another:

- `observed`, `listsMeBack: false` — the peer answered, and does not list this
  service. Its fleet reads and relays never reach this machine.
- `observed`, `grantsToMe: []` — the peer answered, and this credential holds
  nothing there. A `401` from that whoami is this answer: whoami requires only
  authentication, so the only thing a 401 can mean is that the credential
  matches no principal there. A real negative.
- `assumed`, `listsMeBack: null`, `grantsToMe: []` — **nobody could tell**: the
  peer was never reached; the last answer is older than the capability
  staleness bound; this service presents no credential of its own there
  (single-token mode, where the credential is whichever caller's request
  triggered the probe); or the peer runs a build that predates the field. A
  whoami answer without `listsYou` **must** read as `assumed`, never as
  `listsMeBack: false` — that would report a registration problem no one has.

The `self` item reports `listsMeBack: true` and, as `grantsToMe`, what this
machine's own table grants the credential it presents to its peers —
`observed`, and `[]` when that credential matches nothing here — so the same
fact is comparable when read from either machine. `?verify=1` re-probes every
peer before answering instead of using the cached cycle; concurrent verifies
are serialized, so a caller holding only `read` cannot multiply probes.

**`labels` on `/v1/health` is the bounds this machine enforces on session
labels** (muster #153; §3.3's `labels`), and its presence is how a
relaying service learns that a peer stores labels at all. A service relaying a
create that carries labels **must** refuse it `unsupported`, before sending
anything, to a peer whose health omits this field: that peer would decode the
body, ignore the field it does not know, and answer `201` for a session bound to
nothing. Only a positive answer may be cached; a relaying service asks again
before refusing, so a peer that has since upgraded is not refused on stale
evidence.

### 3.1.1 The machine list

An owner-signed list of the machines in a fleet (docs/machine-list.md) can be
handed from one machine to another over this API. The list is signed, so the
channel proves nothing about it; what these routes decide is who may use the
channel and in what order. Neither route is relayed: they always mean the machine
that was addressed, so there is no `{machine}` segment.

```
GET /v1/machine-list
→ 200 <the list this machine has IN FORCE, byte for byte as it was signed;
       Content-Type: application/json>
→ 404 not_found    reason "machine-list-none"  — a mode is on and nothing is in force
→ 501 unsupported  reason "machine-list-off"   — this machine uses no list
→ 401 unauthorized reason "local-only"         — a principal pinned to this machine (§5);
                   the same answer for PUT

PUT /v1/machine-list
   <body: the signed envelope, byte for byte, at most 1 MiB>
→ 200 MachineListReceipt
→ 400 invalid      reasons "machine-list-too-large" | "machine-list-malformed" |
                   "machine-list-signer-not-pinned" | "machine-list-signature" |
                   "machine-list-other-fleet" | "machine-list-grant"
→ 401 unauthorized — no credential, or the principal lacks `push-list`
→ 409 conflict     reason "machine-list-not-newer"
→ 501 unsupported  reasons "machine-list-off" | "machine-list-no-state-dir" |
                   "machine-list-unsupported" | "machine-list-not-applied" |
                   "machine-list-not-writable"

MachineListReceipt {
  fleet       : string
  seq         : integer   // the sequence now in force on this machine
  previousSeq : integer   // the highest this machine had accepted before; 0 when none
  mode        : "shadow" | "prefer" | "only"   // shadow: in force for reporting only
  machines    : integer   // how many machines the list names
}
```

**Serving.** `GET` needs only the `read` grant. It answers with the list in
force, not the file on disk (which may be a candidate this machine refused), so
what a peer fetches is what this machine is running on.

**Accepting.** `PUT` needs the `push-list` grant (§5) and checks, in this order,
refusing at the first that fails: the credential; the grant; whether this machine
uses a list at all and, in `prefer` and `only`, has a state directory to remember
the highest accepted sequence in; the size (`Content-Length`, then a read bounded
at 1 MiB). **Nothing up to the size check reads the body**, signed callers
included (§5). Only then is the list verified, against the signer an operator
pinned on THIS machine, and only if its `seq` is **strictly higher** than the
highest this machine has accepted is it written (atomically, temp file and
rename) and put in force through the same reload a changed file gets, without a
restart. `200` therefore means written **and** in force; any other answer means
nothing changed, on disk or in memory. A list is never trusted for having been
delivered by a peer, and a push cannot change the pinned signers, the mode or the
path.

**The highest accepted sequence** is the higher of the mark recorded in the state
directory and the sequence of the list in force. Equal is refused, not accepted
as a no-op: a list is replaced only by a newer one.

### 3.1.2 The profiling endpoint

A service that is slow cannot be sampled from outside: the process-table CPU
figure is a decaying average on some platforms and has pointed an investigation
at the wrong process. These routes let an operator take a sample from inside a
running service, release build included, without rebuilding it. They read the
service's own process; they change nothing and return no session content. Like
the machine-list routes they are never relayed: they always mean the machine
that was addressed, so there is no `{machine}` segment.

```
GET /v1/debug/pprof/
→ 200 { "profiles": [ { "name": string, "description": string } ] }

GET /v1/debug/pprof/{name}
→ 200 <the profile, application/octet-stream unless noted below>
→ 400 invalid      — `seconds` outside 1..120 or not a whole number; `debug` not 0, 1 or 2
→ 401 unauthorized — no credential, no principal table, or the principal lacks `debug`
→ 404 not_found    — `name` is not one of the profiles below
→ 409 conflict     — a profile or trace is already being taken; retryable
→ 501 unsupported  — this machine has not switched the endpoint on
```

| `name` | What it is | Query |
|---|---|---|
| `profile` | CPU profile, gzip'd pprof protobuf | `seconds` 1..120, default 30 |
| `trace` | execution trace | `seconds` 1..120, default 1 |
| `heap` | live heap allocations, pprof | `gc=1` collects first; `debug=1` for text |
| `allocs` | every allocation since start, pprof | `debug=1` for text |
| `goroutine` | goroutine stacks, pprof | `debug=1` or `2` for text |
| `threadcreate` | what led to a new OS thread, pprof | `debug=1` for text |

Nothing else is served: not the process's command line (configuration an
operator did not agree to publish) and not a symbol lookup (a profile this
service writes already carries its function names, resolved inside the process
from the runtime's own tables, so a stripped release binary needs no symbol file
shipped beside it).

**Two independent locks, both closed by default.** The caller needs the
`debug` grant (§5), **and** the machine's operator must have switched the
endpoint on (`profilingEndpoint` in the config file, read at start; off when
absent). The grant is checked first, so a caller without it learns nothing about
how the machine is configured; a holder on a machine that has not switched it on
is told, in the `501`, which switch is missing. A machine with no principal
table refuses every request: nobody can hold the grant.

**The cost is bounded and visible.** `profile` and `trace` occupy the process's
single profiler or tracer for up to the `seconds` asked, at most 120, and a
second request while one runs is a `409`, not a queue. A caller that disconnects
ends the sampling at once. Every request, served or refused, is in the audit
trail (§5) as `verb=debug`, and a served one carries `took=` — for a timed profile,
the length of the sample.

The credential travels as an `Authorization` header like every other request,
which a bare `go tool pprof <url>` cannot send. Fetch the file with the header,
then open it: `curl -H "Authorization: Bearer …" -o cpu.pb.gz
"$BASE/v1/debug/pprof/profile?seconds=30"`, then `go tool pprof cpu.pb.gz`.

### 3.2 Sessions

```
GET /v1/sessions?scope=fleet|local&machine=&status=&agent=&cwdPrefix=&label=key:value
→ 200 Collection<Session>
```

Returns the envelope of §9 — `items`, `sources`, `complete` — never a bare
array. An unreachable machine appears in `sources` with `status: "unreachable"`;
it never contributes silence to `items`.

**`scope` defaults to `fleet` for clients and MUST be `local` for proxied
calls** (§13.1). A service querying a peer always asks `scope=local`; a peer
receiving `scope=local` answers for itself and never forwards. This is what
keeps fan-out one hop deep, and it is the only thing preventing two
mutually-configured peers from querying each other forever.

**The default is a default for a principal, not a promise to every caller.** A
principal configured `"scope": "local"` (§5, muster #395) is answered as if it
had asked `scope=local`, whatever it asked and whether it asked at all: the
`scope` it sends is validated (`400` when malformed) and then ignored in favour
of `local`. The same holds for `GET /v1/sessions/closed`, `GET
/v1/sessions/watch` and `GET /v1/events`. A fleet-scoped read is clamped rather
than refused because it is what a client that never set `scope` sent; a path
naming another machine is refused (§5).

A `scope=local` response carries exactly one `SourceStatus`. The proxying
service **adopts** that record rather than synthesizing a fresh one (§13.2) — a
peer that answers promptly while reporting itself `degraded` must not be
relayed as `ok`.

**A runtime that is enabled and still being retried (§3.1, muster #300) makes
the list say so.** Such a runtime has no driver, so no driver listed it — and a
runtime that persists its sessions has them on disk the whole time. The list
therefore carries one more source for this machine, `status: "degraded"`, per
runtime in that state, whose `error` names the runtime, its `cause` and the
same `reason` sentence `GET /v1/runtimes` gives. Its sessions are absent from
`items`; the source is how a reader knows, and `complete` is `false` for as
long as it is there — "could not list this runtime yet" is a different fact
from "has no sessions" (§5.7). The source disappears when the runtime
registers, with no restart. A runtime whose cause is not retried (not
installed, not executable, a refused configuration) is **not** reported here:
nothing is on its way, and a source that stays degraded for ever would stop
`complete` meaning anything on a machine that merely has a runtime enabled it
does not have. That runtime is on `GET /v1/runtimes` and `GET /v1/health` with
its cause. With no registered driver at all, the service's own `ok` source
(*"nothing here"*) is withheld while a runtime is pending: the one source is
the degraded one.

**`label=key:value` keeps only sessions carrying that pair** (muster
#153). Repeatable, and every pair must match; exact on key and value; the split
is on the first `:`, which a key may not contain. A pair with no `:`, an invalid
key, or one key named twice with different values is `400 invalid` — the last
because it could never match anything. A proxying service forwards the filter
so each peer narrows its own answer, **and applies it again** to what comes
back. A peer on a build that predates labels ignores the parameter and returns
every session it has, none carrying `labels`: the proxying service **must**
report that source `degraded` with no items, never count its sessions as
matches and never report it as an empty `ok` — "could not answer this
question" is a different fact from "has no such session" (§5.7). The fleet
answer is therefore `complete: false` whenever any machine could not apply the
filter, exactly as when it could not be reached.

**`agent=name` keeps only sessions whose `agent` is that value** (muster
#301). `agent` on a session is the persona its runtime was started with, read
back from the runtime's own process — so it holds for a session this service did
not create and for one far older than its create record, and the filter matches
the same value. A session whose runtime was started without an agent omits the
field (it is never an empty string) and matches no `agent=` filter. Whether a
machine can report it at all is `reportsAgent` in `/v1/runtimes`; on a machine
that cannot, `agent=` matches nothing however many sessions run that persona,
and an unreached peer reports the flag `assumed`. When a machine that can report
it fails to read it for one listing, that source is `degraded` and the fleet
answer `complete: false` — an empty `agent=` result then means "could not say",
never "none".

**The envelope may also carry `feed`, and that is how a snapshot becomes a
mirror:**

```json
{ "items": [...], "sources": [...], "complete": true,
  "feed": { "cursor": 12904, "epoch": "..." } }
```

`feed` is where this snapshot sits in the event sequence (§4). A client lists
once, then watches from that cursor, and never polls again.

The cursor is read **before** the enumeration begins, deliberately. A snapshot
may therefore already contain changes newer than the cursor it carries, and
replaying from that cursor re-applies them. That overlap is the point: applying
an event twice to a mirror keyed by session id changes nothing, while missing
one leaves a mirror that is wrong permanently and cannot tell. The design
produces the recoverable failure, the same way §7.3 insists a gap be announced
rather than skipped.

**`feed` is ABSENT when the cursor is not a resume point**, and absent is a real
answer (§5.7), not zero. A service advances its sequence only while it is
actually observing a driver; with nothing subscribed the cursor is frozen while
the fleet keeps moving, so a number handed out then would look resumable and
would silently skip everything before the first subscription. A client that
finds no `feed` has its ordering backwards: **watch first, list second.**

**The list carries a validator when the service can keep its answer current**
(muster #464).

```
GET /v1/sessions?...                          If-None-Match: "<validator>"
→ 200 Collection<Session>   ETag: "<validator>"
→ 304                       ETag: "<validator>"      (no body)
```

A response MAY carry a strong `ETag`. It is opaque — send it back, do not read
it — and it names one built answer to one question: the scope and the filter are
part of it, so a request with another `status`, `agent`, `cwdPrefix` or `label`
set has another validator, and one question's validator never matches another's.
A request whose `If-None-Match` names the **current** validator is answered `304
Not Modified` with the `ETag` and no body, and the service does not enumerate
its drivers to say so. The header may be a list (`"a", "b"`), `*` matches any
current answer, and a `W/` prefix is ignored — the weak comparison RFC 9110
§13.1.2 prescribes for this header. A validator that is not current is not an
error: the answer is a `200` with the body and the new validator. Both carry
`Cache-Control: private, no-cache` (a stored copy must be revalidated before it
is used) and `Vary: Authorization` (a principal pinned to this machine is
answered locally for the same URL, see above).

**What a validator promises:** that nothing this service can observe has changed
since the body was built, and that the fields nothing observes are at most five
seconds old. The service keeps its last answer for each question and drops it
when any of these happens:

- an event is stamped into the sequence of §4 — any event, for any session;
- a request that creates, changes or ends a session is admitted: the answer is
  dropped before that request's response can be read, so a client that closes a
  session and then lists is never told it is still there, whether or not the
  driver's own event has arrived yet;
- a runtime registers, or the retry state of one that has not registered
  changes (§3.1, #300);
- a peer is configured or removed;
- this service's watch of its own machine starts, stops, or loses a driver's
  subscription;
- five seconds have passed.

The five seconds are not decoration. Some fields of a row change without an
event — a driver reports a state change only when it is material, and
`conversation`, `model` and the like are read when the listing is built — so a
kept answer can be up to five seconds behind in exactly those. `observedAt` in
`sources` is when it was built. A client that needs the changes themselves, not
a recent list, should build a mirror (§4.1) rather than poll with a validator.

**When there is no validator.** The `ETag` is absent, and the read is exactly
what it was before validators existed — every driver enumerated, every row
built — when:

- **nothing is watching this service.** Its sequence is frozen while the world
  moves (the reason `feed` is absent, above), so it has no way to know that an
  answer is unchanged. A client that wants validators from a service it does not
  otherwise watch has to watch it (§4.1);
- **the watch does not cover every local runtime**: it is narrowed to named
  sessions, or a driver's subscription is down, unsupported or not yet attached
  (a runtime that registers while the watch runs is subscribed at once, §4, and is
  read without a validator only until that subscription is up);
- **a driver that was asked did not answer `ok`**, including one the caller's
  `Fleet-Deadline-Ms` cost it. Only a clean answer is kept; a later reader never
  inherits a degraded one;
- **`scope=fleet` and at least one peer is configured.** A peer's rows are
  fetched with the caller's own credential (§13), so one kept answer cannot be
  shared between callers, and this service hears a peer's changes only while a
  fleet-scoped watcher exists, so it could not promise that the validator moves
  when a peer's row does. With no peer configured a fleet read **is** the local
  read, and carries the same validator as `scope=local`.

An answer that has a validator always carries `feed` as well, and a `304` means
the `feed` the client already holds is still the current position: nothing has
been stamped since. `GET /v1/health` reports `listSnapshot` — reads answered
from a kept answer, answers built, and reads that could not use one — counted
since start, so a deployment can read its own hit rate.

#### Closed sessions

```
GET /v1/sessions/closed?scope=fleet|local&since=<RFC3339>
→ 200 Collection<ClosedSession>
```

A service keeps **one record per local session that ended**, for a configured
retention period, across restarts (muster #179). It is deliberately not a
persisted event window: the record carries only the metadata the live record
already carried as last seen (`machine`, `id`, `name`, `runtime`, `cwd`,
`startedAt`, `agent` when a sighting reported one, and `conversation` when it
was known), never content, plus
`lastSeenAt`, `closedAt`, `closedBy`, `evidence`, and — only for
`closedBy: "exit"` — `exit` (#235, below).

`closedBy` separates observation from inference (session-abstraction.md §5.2):

- `close` — a close through this service was accepted by the driver;
  `closedAt` is that moment. Withdrawn if a later read finds the same session
  (same id and `startedAt`) still running.
- `absent` — the session was missing from a **complete, unfiltered** read of
  its runtime, or its id now names a session with a different `startedAt`
  (§5.4). `closedAt` is when the absence was observed; the end lies after
  `lastSeenAt`. A filtered or partial read never ends anything (§5.7).
- `exit` — a driver capable of it (an `ExitReporter`) captured the session's
  own process exit — status and last screen — before removing the session,
  and reported it in the same pass (#235). `closedAt` is exact, like `close`:
  it is the moment the driver captured the pane, not an upper bound inferred
  from a later listing. The record carries `exit: {status, at, screenPath}`:
  `status` is the process's own exit status; `screenPath` names a file kept
  on that machine's own disk holding the pane's last lines — never the text
  itself, which this route never serves (pane text can hold anything the
  runtime printed, and putting it on the API is a data-exposure surface this
  route deliberately does not open). `exit` is absent for every other
  `closedBy`.

A session missing under its old id while exactly one newly-seen session of the
same runtime carries its `startedAt` is a **rename**, not an end; with more than
one such candidate nothing is carried and the old id is recorded as ended —
guessing would attribute one session's history to another.

Records older than the retention period are neither returned nor kept. `since`
keeps records with `closedAt` at or after it; a malformed value is
`400 invalid`. `scope` has the live list's meaning and the same one-hop rule
(§13.1). A peer whose router has no such route answers a bare `404`; the
proxying service **must** report that source `degraded` with no items, never as
an `ok` peer with nothing closed.

#### Input audit

```
GET /v1/machines/{machine}/input-audit?session=&sha256=&since=&until=&principal=&limit=
→ 200 {records[], truncated?, coveredFrom, durable}
```

One record per `input` call that reached a driver (muster #330): `id`, `at`,
`principal`, `machine`, `session`, `sha256` and `bytes` of the delivered text
(never the text), `humanRelay`, `route`, `direction`, `outcome`, and, when the
caller named the person it acted for, `person` or `personDropped` (§5, *Acting
person*). It exists so a
client can show that a prompt in a session was, or was not, sent by an agent
principal: the engine's transcript marks every user turn alike, and the sender
controls any label in the text.

- `humanRelay` is the service's own finding (§5, `human-relay`), never a caller
  statement. `principal` is the credential that authenticated; for a relayed
  call it is `<asserted principal> via <relaying peer>`, and an assertion from a
  caller that is not a configured peer is not repeated.
- `direction` is `local`, `relayed-out` or `relayed-in`. A relayed call is
  recorded on both ends; the machine it entered holds the original principal.
- A record is appended **before** the driver is asked to deliver and completed
  after. A call the process died in the middle of reads `outcome: "unknown"`; a
  driver failure after the attempt reads `error`. A request refused for its own
  shape never reached a driver and leaves no record.
- `coveredFrom` is the earliest instant the record is complete for. An empty
  answer to a window that begins before it is **not** an absence proof. It moves
  forward with retention, with a record cap, and with any write the service
  failed to make; a failed write never fails the input.
- The route is machine-local (`400 invalid` for another machine), needs `read`,
  and requires at least one of `session`, `sha256` or `since`.

### 3.3 Deadlines

Every request carries an effective deadline:

```
Fleet-Deadline-Ms: 3000        (request header, optional)
```

A caller may shorten a driver's declared `deadlineMs`, never extend it. On
expiry the service returns the envelope with that source marked
`unreachable`, `error` naming the elapsed time — **not** an open connection and
not a 5xx for the whole call. One unresponsive peer degrades an envelope; it
never fails a fleet-wide query.

**A peer that stops answering is remembered** (#237). A deadline bounds one
call; it does not stop the next call from dialling a machine that has just
failed, and a sleeping peer would otherwise cost every read the full bound.
After **3 consecutive** transport failures (a dial error, or a deadline this
service itself enforced) the peer is marked down: a call that would have
dialled it fails at once with the same retryable `unreachable`, the
`sources[].error` names when the peer began failing, and `GET /v1/machines`
reports the peer `unreachable` from that memory without dialling. A background
probe (`GET /v1/health`) retries on a doubling backoff from 2 s to 30 s, and
the first answer of any kind — even a refusal — clears the state. A deadline
the *caller* shortened below the service's own bound, and a caller that hangs
up, are not evidence about the peer and never count by themselves. They do
prompt the service to ask for itself: it sends its own `GET /v1/health` under
its own budget (at most one in flight per peer) and counts *that* outcome, so
a peer that went dark is marked down even when every caller gives up sooner
than the service's bound, while a slow peer that answers inside the service's
own budget is never marked down, whatever its callers' timeouts. `scope=local`
never touches a peer in any state. The wire shape is unchanged; only the wait is.
While a peer is marked down, mutations to it are refused the same way, so a
false positive costs at most one backoff interval.

A service relaying a call to a peer announces *less* than it enforces: its
remaining budget minus a transit reserve (the smaller of 250 ms and one fifth
of what remains), never below 1 ms. The peer therefore sees a smaller value
than the original caller sent. This is deliberate: a peer told the exact bound
the relay enforces runs out at the same instant the relay does, so its own
answer — often an honest `degraded` or `unreachable` report about its local
source — is still in transit when the relay gives up, and the relay reports a
machine that answered as one that did not. A relayed call whose context has a
deadline always carries the header; a remaining budget too small to announce
is sent as 1 ms rather than dropped, because no header means no bound at all.

**The message names the budget that ended the call** (muster #476). Most calls
are bounded only by the effective deadline, and the answer then reads `no answer
within <deadline>`. A step that has a budget of its own shorter than the
deadline — the wait for a runtime to become ready after it was started — can end
a call first, and the answer then names that step and its budget (`... ran past
its own budget of 10s (the request's deadline of 30s had not run out)`) instead
of blaming the deadline; the operator looks at the step, not at the caller. A
deadline that ended a call during a named step says which (`no answer within
30s (the deadline ran out during ...)`), and a caller that hangs up before the
answer is told so (`the request was cancelled ...`). All three are `unreachable`,
504 and `retryable: true`: a client that treats a 400 as final must not meet one
for a create that only needed more time.

Absent the header, the driver's declared `deadlineMs` applies. There is no
configuration in which a call has no deadline: measured against a stopped peer,
an undeadlined request was still blocked after seven seconds with no result,
and no mainstream HTTP client defaults to protecting you from that.

```
POST /v1/machines/{machine}/sessions
Idempotency-Key: <caller-supplied, required>
{ "runtime": "...", "cwd": "/abs/path", "agent": "...", "model": "...",
  "effort": "...", "name": "...", "marker": "...", "remoteControl": true,
  "prompt": "...", "contextRef": "/abs/path", "trustCwd": false,
  "env": {"NAME": "value"}, "isolateEnvironment": true, "isolateConfiguration": true,
  "sandbox": {"readPaths": ["/abs"], "writePaths": ["/abs"], "network": "open",
              "packageCache": {"path": "/abs/cache", "mode": "private"}},
  "posture": {"attendance": "unattended", "files": {"writePaths": ["/abs"], "readPaths": ["/abs"]},
              "network": "closed", "isolatedEnvironment": true,
              "configuration": "given"},   ← instead of the fields above (#344, #392)
  "resume": "<conversation id>", "conversationId": "<caller-chosen uuid>",
  "permissionMode": "bypass", "consents": ["folder-trust"],
  "mcpConfig": ["/abs/servers.json"],
  "settings": {"crossSessionInbound": "accept"}, "labels": {"issue": "153"} }

→ 201 { "machine": "...", "id": "...", "name": "...", "runtime": "...",
        "marker": "...",
        "runtimeSurface": {"known": null, "evidence": "..."},
        "promptDelivery": {"outcome": null, "evidence": "..."},
        "identityAssertion": {"asserted": "...", "drifted": null, "evidence": "..."},
        "state": {...} }
→ 200 (same body) if the key was already seen — the existing session
→ 409 if the key was seen with a different body
→ 409 { "kind": "conflict", "reason": "replay-of-ended-session",
        "session": {"machine": "...", "id": "...", "name": "..."},
        "closedAt": "..." } if the key was already seen with the SAME body and
        that session no longer exists
```

`Idempotency-Key` is **required, not optional**. A create without one is
rejected with `invalid`. The rationale is §10: a timed-out federated create
that gets retried produces two agents writing to the same working directory,
and the caller cannot detect it afterwards.

**A replay whose recorded session has since ended is `409`, not the ordinary
`201` (muster #234).** Before this, a same-key-same-body replay answered
201 with the dead session's own id — the identical shape a live create
returns — so a caller checking only the status code could not tell "your
session is running" from "your session died earlier and this key produced
nothing just now". `reason` is `"replay-of-ended-session"`; `session` is the
ended session's own ref; `closedAt` is when THIS machine observed the
absence, an upper bound on the true end (the same rule `GET
.../sessions/closed`'s own tombstones follow for `ClosedByAbsent`, §3.2 —
this driver does not poll for a precise end time on your behalf). The key
itself is untouched by this: it stays spent for its normal retention, so a
third call with the same key gets the same 409, not a silently fresh session.
The caller's fix is to mint a **new** `Idempotency-Key` — retrying this one is
asking the same already-answered question again.

**`prompt` is capped by this machine's effective limit** — 32768 bytes by
default, and a machine setting rather than a compiled-in constant since
muster #130 (see `maxInputBytes` in §3.1 for how to read it without
triggering it). Over that, the create is rejected outright — `invalid`
(400) naming the limit and the caller's actual size — instead of being
accepted and left to strand in the composer with no delivery receipt to
explain why (muster #114, #110, #112: the creation path measurably
strands even shorter prompts than `input` does). The default was 1024 until
muster #412: a conservative guess, not the bisected true failure boundary,
which #114 leaves as open work and #130 argues may be startup timing rather
than size at all. The field settled the number instead — a client that opens
sessions with a real briefing sends 8–15 KB, every one of them was refused
on a fresh install, and the machines that had set 32768 by hand ran that way
without the stranding the cap was written to prevent. The cap and the refusal
are unchanged, and the setting still moves either way: a smaller value
refuses more, and `muster doctor` still validates it. A caller with more to
send than a briefing should write it somewhere the agent can read
deliberately and pass only a short pointer here — the same workaround #112
already adopted ad hoc.

**The driver that served the create builds this response, not the service
layer relaying the caller's own request back at it** (muster #84, #85,
#86) — `agent`/`model` on it are what the runtime is actually using, when the
driver can tell, never an echo of what was requested; the requested values
live in `pins` alongside whether they were honoured. The three paragraphs
below are one recurring shape, not three unrelated notes: a 201 is a receipt
for the CREATE call succeeding, never proof that everything the create asked
for was applied.

**A 201 for a `resume` create is not proof the resume was honoured** — a
concurrent burst can have the runtime silently start a fresh conversation
instead, with no refusal and no degraded status (muster #72). Poll the
session afterward and read `resumeOutcome` (§2.10) once its `conversation`
resolves; do not infer success from the create response alone.

**A 201 for a `conversationId` create is the opposite case: `conversation`
already resolves on the create response itself** (muster #224),
`known: true` with `source: "captured"` (§2.9) — this service told the runtime
which id to start under, so there is nothing to poll for. `conversationId` and
`resume` answer the same question two incompatible ways and are refused
`invalid` together; a malformed id or one already recorded for this `cwd` is
also `invalid`; a runtime or peer that predates the field is `unsupported`
(§3.1's `supportsConversationId`) rather than silently forwarding it to be
dropped. If a *later* read's own resolution disagrees with the id that was
requested, that is reported as a fresh, named mismatch — `conversation` reads
`known: false` naming both ids — never a silent overwrite of one with the
other.

**`isolateEnvironment: true` is a requirement, and a driver that cannot meet it answers
`unsupported`** (muster #280; session-abstraction.md §2.1 and §4.3). A runtime that starts each
session in a process of its own with a built environment reports `isolatesEnvironment: true` in
`/v1/runtimes`; against any other the create is refused before anything is started, and a relay to a
peer whose `/v1/health` lacks `supportsIsolateEnvironment` is refused on this side, before the peer
sees it. The flag needs only `create`: it narrows what the session gets and grants nothing. Be exact
about what a `201` then means — the session holds *only* the variables the create (and this
machine's `sessionEnv`) named, **including the model-provider key it needs, which isolation cannot
hide from the session's own tool shell**; give each lane its own spend-capped key.

**`isolateConfiguration: true` is a requirement, and a driver that cannot meet it answers
`unsupported`** (muster #392; session-abstraction.md §2.1 and §4.3). The session then loads only the
configuration the create names — its `settings`, `mcpConfig` and `contextRef` plus the working
directory's own project settings (`.claude/settings.json`, not the per-machine
`.claude/settings.local.json`) — and nothing from the account the service runs as: no user-level
hooks, plugins, permission rules or enabled tools, and no tool server the create did not list. A
runtime that can start a session so reports `isolatesConfiguration: true` in `/v1/runtimes`; against
any other the create is refused before anything is started, and a relay to a peer whose `/v1/health`
lacks `supportsIsolateConfiguration` is refused on this side, before the peer sees it. It needs only
`create`: it narrows what the session loads and grants nothing. Three things to plan for. An `agent`
the create names must be defined in the working directory's `.claude/agents`; one defined only at
user level is not loaded and the terminal runtime's agent refuses to start (visible, not a degraded
session). `unattended` lowers to `bypass`, whose acceptance screen the user setting that suppresses
it no longer reaches, so a client that combines the two sends `consents: ["bypass-permissions"]`. And
a `resume` that names no `posture.configuration` carries `given` forward from the conversation's
previous session (a resume must not come back loading the owner's tool servers); `posture.configuration:
"inherit"` is the explicit way back, and a runtime that cannot give it refuses the resume instead.

**`sandbox` is a requirement, and a driver that cannot enforce it answers `unsupported`** (muster
#281; session-abstraction.md §2.1 and §4.3). A runtime that can confine a session reports a
`sandbox` object in `/v1/runtimes`; against any other the create is refused before anything is
started, naming what is missing, and a relay to a peer whose `/v1/health` lacks `supportsSandbox` is
refused on this side — an older peer would drop the field and answer `201` for a session with
nothing confined. A malformed `sandbox` (a relative or unclean path, a writable shared temporary
directory, an unknown posture) is `400` on any machine; a grant that is or contains the service
user's home directory is `400` where the session runs. It needs only `create`: it narrows what the
session reaches and grants nothing. A `201` then means the session runs under the profile that
`state.sandbox` reports on every read; it does **not** make the working directory trustworthy
afterwards (§2.1: run any git against it inside the same profile). The mechanism is named in
`sandbox.mechanism`; where it is `linux-landlock-seccomp` (muster #439) a `closed` network is
stricter than "loopback only" (§2.1), and a `readPaths`/`writePaths` entry that does not exist is
`400`, because that mechanism grants a path by opening it.

**`posture` is the engine-neutral spelling of `permissionMode`, `sandbox`, `isolateEnvironment`
and `isolateConfiguration`** (muster #344, #392). It says what the work needs and never how a runtime
spells it: `attendance` (`attended`, the default, or `unattended` — no permission prompts),
`files` (present: file access confined to the working directory and the session's private
directory plus the `writePaths` and `readPaths` named), `network` (`open` or `closed`),
`isolatedEnvironment`, `configuration` (`inherit`, the default, or `given`: the session loads
only the configuration the create names) and `packageCache` (muster #432: a `seed` directory, a `mode`
of `private` — the default, a per-session copy — or `shared`, and the `managers` of `cargo`, `go`,
`npm` the session uses, each handed the variable it reads, pointing into the session's copy; and
`allowFullCopy` (muster #448, default `false`), without which a seed on another volume than the
session's directory is refused `invalid` before it is copied; naming a
seed implies a confined session, like `unattended`; see api.md). Every field present is a requirement. The service lowers
it onto the per-runtime fields using only what the resolved runtime declares in `/v1/runtimes`
(`posture` on each row says which parts it can enforce), and **refuses with `unsupported`,
`reason: "posture-unenforceable"`, before anything is started** when it cannot enforce all of
it — never a weaker posture under the same name. Two rules shape the mapping. **`unattended`
always implies a confined session** (`permissionMode: "bypass"` inside the sandbox, with
the `files` and `network` asked for on top): a prompt-free session that can read the whole user
account is the posture this field exists to prevent, so a runtime without a sandbox refuses it;
`permissionMode: "bypass"` stays for a caller that really wants prompt-free and unconfined. And
the terminal runtime, whose sessions run in the user's own multiplexer, declares a sandbox **only
on a host where its agent CLI was measured to start and authenticate inside the profile** (muster
#345; session-abstraction.md §4.3): the multiplexer's server stays outside the profile and only the
agent program inside the pane is wrapped. On any other host it enforces only an empty or `attended`
posture and refuses the rest, and the refusal carries the reason the measurement failed
(`sandboxUnavailable` on that runtime's capabilities). A peer that has not been
reached is not refused locally: the lowered fields are forwarded and the peer's own refusals
apply. A create addressed at a peer that names a `runtime` is lowered against **that runtime's** row
of the table the peer reports on `GET /v1/runtimes` (one row per runtime it runs), not against the
first row; a body that names none keeps the first row, as before, and a named runtime the table
does not list is treated like a peer not yet reached (the peer decides, muster #460). `posture` is
**mutually exclusive** with the fields it replaces (`400`); a malformed
one (a relative or unclean path, a writable shared temporary directory, an unknown value) is `400`
on any machine. `unattended` needs the `send` grant on top of `create`, as `bypass` does; a posture
that only narrows needs `create`. `POST /v1/posture/dry-run` takes `{"posture": {...}}` and
returns, per runtime, whether it is enforceable and the fields it lowers to, changing nothing; it is
the same function a create runs. `permissionMode`, `sandbox` and `isolateEnvironment` keep working
unchanged for one release and are **deprecated** in favour of `posture`; `isolateConfiguration` is
the per-runtime spelling a relay forwards and is not deprecated, but a client states it through
`posture.configuration`.

**A 201 for a create that asked to pin `agent`/`model`/`effort` is not proof
the pin was applied** — a value this driver cannot pass through safely is
refused outright (`invalid`, naming the field), but a value that *reaches*
the runtime intact can still be defaulted or ignored there with nothing
reported back except by reading `pins` (muster #84).

**A 201 for a create that requested remote control is not proof a
runtime-hosted surface exists yet** — the runtime registers it, if it does at
all, asynchronously after the process starts, so `runtimeSurface` (§2.13) is
legitimately `known: null` on the create response and may take a read or two
to resolve. `known: false` is different and final: the create opted out
(`remoteControl: false`), or the runtime declined, and a caller polling for
one may stop (muster #85).

**A 201 for a create that carried a `prompt` is not a delivery receipt for
it** — the prompt is sent after the process starts, so the 201 is written
before delivery can be known. Read `promptDelivery` (session-abstraction.md
§2.11): `outcome: null` means still in flight, and is never evidence the
prompt was lost — a session polled moments later reading `idle` with
"composer empty, no turn yet" looked identical, before muster #111, to
one that never had a prompt at all (muster #86). Read `state.turns`
alongside it now: `turns: 0` is what a session that never took a turn looks
like, and a nonzero count is proof of the opposite — the prompt was received
and at least one turn against it has already completed. Do not re-send
through `input` while `promptDelivery` still holds. While it does,
`waitingOn` (muster #126) is the machine-readable class for `evidence` —
`prompt`, `unsent-input`, or `starting` — so a caller can branch on WHY
without parsing the prose; absent means this driver has not classified this
particular wait.

**A 201's `name` is what this machine asserted, not proof the runtime still
carries it.** Nothing has read the session back yet, so `identityAssertion`
(§2.14) legitimately reads `drifted: null` on the create response — the same
"asserted, not yet corroborated" state a rename can also produce, and the
one whose absence a prose-only sentence used to leave undetectable
(muster #97, #102).

**`runtime` on the response is the runtime that actually served this
create** — not an echo of the request body's own `runtime`, which is
commonly absent (session-abstraction.md §7.1a: a caller with one local
runtime, or one relying on the configured default, sends no hint at all).
See `Fleet-Runtime` above for the same fact carried on every other
session-addressed call, not only `create`.

`contextRef` is a path. Inline context is not accepted, and context never
reaches a command line (§5.3). The file's text is the session's system context
for its whole life, on every runtime that accepts the field; one that cannot
honour it refuses `unsupported`. A path that is not absolute is refused
`invalid` before anything starts, and so is one a driver that reads the file
itself (the opencode driver, session-abstraction.md §5.3) cannot use.

**`name` is a request, not the id.** The driver owns the naming rules
(session-abstraction.md §2.1) and may sanitize the name, number it against
sessions already live, or append `marker`. A marker is carried, never
stacked, whatever alphabet it is drawn from — a name that already ends in
the one sent keeps the one it has, so a caller resuming an already-marked
name may send the same `marker` again without growing it (muster #88).
**Read the returned `id`** — it is
the resolved string, and it is what every later call must address. A caller
that assumes the name it sent is the id it got will address the wrong session
the first time two carry the same name.

**`remoteControl` omitted is not `false`.** Omitted means "whatever a
first-class session on this substrate gets". Send `false` only to deliberately
create a session that cannot be reached remotely.

**`trustCwd` is consent to one question about the `cwd` in this same request**:
the runtime's folder-trust dialog, which on some runtimes stands between a new
session and doing anything at all. With it, the driver answers that dialog by
locating the option that grants trust and choosing it by index — never by
accepting the highlighted default, which on a neighbouring boot screen means
`No, exit` — and answers nothing if the wording is ambiguous. Absent means the
driver answers nothing, which is what every caller written before this field
already gets.

It requires the **`send` grant in addition to `create`** (§6): answering a
dialog is a keypress, it is the same blast radius as `respond`, and folding it
into `create` would make this route a second, unreviewed way to drive a
session. On a relayed create the check belongs to the peer serving it, against
the same credential — §13's "proxying does not launder authorization".

**`consents` generalises `trustCwd`**, which remains valid and means exactly
`["folder-trust"]`. Each entry is a `PromptKind` the driver can RECOGNISE; the
affirmative option is then found by reading the runtime's own option text, and an
unrecognised or ambiguously-worded screen is answered not at all. Not every kind
is consentable: `resume-chooser` is refused, because its options are summaries of
prior conversations and nothing in them identifies the one the caller named —
consent there would be a coin flip, and losing it resumes a stranger's work. Same
`send` grant as `trustCwd`.

`external-imports` is consentable: the runtime's second boot question about a
directory, "allow external imports", raised when the instruction files its
working directory loads import a file from outside it. Its highlight defaults to
the decline, so it is answered by index like every other consent. It is a wider
agreement than folder trust — the caller vouches for the directory, not for a
listed set of files, and the question's own warning is never to allow it for a
repository the caller does not own — so pass it only for a directory you own.
Its scope is unchanged: this one question, on the one session being created,
nothing standing. A consent names a question, so `["folder-trust"]` leaves this
one standing and the reverse. A create carrying only a consent, with no `prompt`
and no `trustCwd`, is answered too.

**`env` is delivered out of band, never on a command line.** Values are staged in
a 0600 file the session reads and unlinks; nothing reaches an argv, because the
payload likeliest to be a credential must not be the one exception to §5.3. Names
must look like variable names, and a value may not contain a newline or NUL — the
staging format is line-oriented, and a newline would arrive as a second variable
invented out of value content. Violations are `invalid`, never truncated. A driver
with no out-of-band channel refuses the create rather than starting a session
missing its identity.

**`resume` continues a prior conversation.** Not to be confused with
`supportsResume` in `/v1/runtimes`, which answers whether sessions survive a
service restart — a different question with an unfortunately similar name. A
resumed session commonly meets the resume chooser; see `consents` for why that
one is yours to answer.

**`conversationId` starts a NEW one under a caller-chosen id** (muster
#224) — `resume`'s mirror, and mutually exclusive with it. Must be
UUID-shaped, and refused `invalid` if it already names a conversation this
machine has on record for the same `cwd`: starting a second conversation under
it would make two conversations share one transcript. Unlike `resume`, a
driver that honours it reports the id back as `conversation` on the `201`
itself, `source: "captured"` — see the create endpoint's own note above for
why that is the whole point of the field.

**`permissionMode` requests the permission posture the session starts in** (muster #329).
The create set is `default`, `acceptEdits`, `plan`, `auto` and `bypass` — the five modes
`state.permissionMode` reports back, so a created session reads as what it was created as. A
runtime accepts the subset it lists in `createPermissionModes` (`/v1/runtimes`; absent means the
runtime did not say, never "none") and refuses the rest `unsupported`; a value outside the set is a
`400` whatever the runtime, rather than being passed through. `default` named explicitly starts the
runtime in its ordinary mode even where its own configuration names another; omitting the field
asks for nothing.

The **`send` grant** is required, on top of `create`, for the modes that let the agent do more
unattended than a plain create does: `acceptEdits`, `auto` and `bypass`. `default` and `plan`
narrow or keep the ordinary posture and need only `create`. The line is escalation, not
"anything but the default": a principal permitted only to start sessions could not otherwise
reach `acceptEdits` or `auto` (reaching them on a live session takes the `keys` grant, ADR 188),
and the grant must not be quietly wider at create than it is afterwards.

**A driver may honour `bypass` only inside a boundary of its own** (muster #283). The local
opencode driver pre-approves every tool *only when the same create asks for a `sandbox` it
enforces*; with none, or in a mode that cannot confine one session, the create is refused
`unsupported` and the reason says so — approving everything is safe exactly when something other
than the approval is the boundary, and a weaker posture is never started under the same name. The
posture is applied through that session's own runtime configuration and never a file another
session would read, and `state.permissionMode` reads `bypass` for it, including after a service
restart. A session that nevertheless stops on an ask the configuration did not cover reads
`waiting_input` with a `tool-permission` prompt, never `working`; the prompt's options are the three
answers `respond` takes (allow once, allow always, reject) and its nonce is the ask's own id, so
`respond` answers it through the runtime's permission-reply endpoint and refuses an answer meant
for an ask that has since been resolved (muster #296; a runtime "question" is not mapped and stays
`unsupported` with the reason); a list of asks that could not be read leaves the status as it was and says so in
`evidence`, because an unasked question is not the answer "none".

**A path outside the working directory is decided by the boundary, not asked** (muster #298). A
`bypass` session inside an enforced sandbox is configured so that a tool naming a path the sandbox
does not open is denied with an error the model can read, and a path the sandbox does open is
allowed, so an unattended session never parks on an ask that protects nothing: the sandbox already
refuses the same access through a shell. The rules are the sandbox plan's own paths, so a create's
`readPaths` and `writePaths` are covered, and they are written again at relaunch. Without `bypass` the driver writes no such rule and an
ask still reads as `waiting_input`.

**A driver whose runtime ships a hosted provider of its own may not let a session fall back to
it** (muster #293). Measured: a session on a machine with no provider credential ran its first
turn on the runtime's built-in hosted provider and reported an ordinary idle — prompts, file
contents and tool output went to a third party nobody chose. The local opencode driver therefore
fails closed, by operator configuration and never by a request field: that provider is disabled
in each session's own runtime configuration by default (at create and again at relaunch), and
`FLEET_OPENCODE_PROVIDERS` names the only providers a session may use instead (naming the
built-in one is how an operator asks for it). An entry may also name single models: `provider/<model>`
or `provider/<glob>` allows only the matching models of that provider, and `!provider/<glob>` denies
them whatever else allows them (a deny beats any allow). The form to prefer is an allowlist of exact
models, because a name pattern is not a reliable mark of a model's terms — a hosted provider's
zero-cost models do not share a suffix, so a suffix deny does not keep them out. A malformed entry
stops the start, naming it. A create the policy leaves with no usable provider —
the runtime lists none that is allowed, or lists nothing at all — is refused `unsupported` with the
reason, before any session exists; so is one whose `model` is outside the policy, the reason naming
the rule that refused it. A create that names **no** model is refused when the model the runtime would
pick for it is outside a policy that names models, or cannot be learned (the policy then fails closed
and the refusal says to pin a model); a policy of providers alone never makes this check. Model rules
are enforced at these checks and on the turn's own record; they are not in the session's own runtime
configuration, which enables whole providers only. A
runtime that does not answer the catalog read at all is still "could not tell", and the create
proceeds. A turn that nevertheless ran on a provider outside the set reads as `idle` with
`lastTurn: {"outcome":"failed","reason":"…"}` naming the provider and model, never as an
unremarkable idle.

**`mcpConfig` names tool-server configuration files, by PATH.** Each entry is
absolute, and the flag is emitted once per entry rather than joined — a joined
list reaches the runtime as a single filename containing a separator, which
surfaces as a session missing its tools rather than as an error anybody can
read.

Paths, never inline configuration, and for a sharper reason than `contextRef`'s:
these files commonly hold the credentials their servers authenticate with, so
inline content would put a secret in an argv every process table on the machine
can read. A caller holding one in memory writes it to a 0600 file and names the
file — the same move `env` already forces.

A path the driver cannot read is a **refusal, not a start**. The runtime would
come up, fail to load it, and present a session that lists, reads and accepts
input while being unable to do the work it was created for — the same failure
`env` is already refused for, in the same words. A 201 for a session that is
quietly not what was asked for is worse than an error at the call site. On a
relayed create the paths belong to the PEER's filesystem and travel verbatim;
the machine serving the create is the one that checks them.

Nothing here reads, merges, validates or interprets the contents. A service that
parsed these would have begun to hold opinions about what a session may talk to,
which is a supervisor's judgement and §6's non-goal.

It requires the **`send` grant in addition to `create`**, like `permissionMode`
and for the same shape of reason: these configurations name servers the session
will LAUNCH, and between "may start a session" and "may start a session that
also starts these", the second is plainly the larger authority. The refusal names
the field, so a caller fixes one line rather than re-reading its whole request.

**`settings` carries launch-time runtime settings for the agent CLI** (muster
#247): a JSON object serialized onto argv as the CLI's `--settings '<json>'`. It
exists for the settings the CLI reads only at boot and from no environment
variable, so `env` cannot carry them — without a field a client that needs one
starts the process itself and misses everything the service wires in at launch.

With `permissionMode: "bypass"` any key is carried. **Without it only the keys on
an allow-list are** (#254; today `crossSessionInbound`, advertised as §3.1's
`launchSettingsOutsideBypass`): any other key is refused `invalid`, naming it, so
a session that still asks before acting cannot be widened by accident — and a
default-mode session relaunched with `resume` can still carry the setting it
booted with. It must be a JSON object (invalid JSON, `[…]`, a scalar, or more
than 4096 bytes compacted is `invalid`; `null` means absent); outside the
allow-list, keys are not interpreted. It requires the **`send` grant in addition to
`create`**, like `permissionMode`. Because the value is an argv element, it is
for launch-time switches only — never a credential; those go in `env` or a file
named by `mcpConfig`. A runtime with no CLI to hand it to answers `unsupported`,
and so does a peer that predates the field (§3.1's `supportsLaunchSettings`) —
or, for a non-bypass create, one whose `launchSettingsOutsideBypass` does not
list every key sent — before anything is started there.

Caller-supplied values that land in the agent's argv (`agent`, `model`, `effort`,
`resume`, `mcpConfig`) may not begin with `-`: the CLI would read them as flags,
which would turn a create grant into "run the agent with arguments of my
choosing". That guard is what a caller with no field for its tool servers used to
hit while smuggling the flag through a pin. `conversationId` lands in the same
argv and is covered the same way, structurally rather than by the same
leading-dash check: its UUID-shape validation (refused `invalid` otherwise)
already rules out anything that could be read as a flag.

```
GET /v1/machines/{machine}/sessions/{id}/environment?runtime=
→ 200 { "known": true, "shell": "...", "login": true, "interactive": true,
        "names": ["..."], "path": ["...", "..."],
        "serviceNames": ["..."], "servicePath": ["..."],
        "capturedAt": "..." }
→ 200 { "known": false, "reason": "..." }   ← a real answer, not an error
→ 501 unsupported, if the runtime cannot report one
```

What environment a session's process actually received. **`names` carries
variable names and never values** — the environment in question is the one
holding credentials, and a read that returned them would be a worse defect than
any it diagnoses. `path` is the one value present, because a search path is not
a secret and is the drift this endpoint was added for.

`serviceNames`/`servicePath` are the same enumeration for the **service's own**
process, so a reader can see what the session's startup contributed rather than
only what it ended up with. An empty difference is the interesting case: it
means the startup files added nothing, which means an agent needing credentials
will start normally and fail at its first tool call.

`known: false` is an ordinary 200 (§5.7). "The session had no environment" and
"we never found out" are opposite answers, and a driver must not collapse them.

```
GET /v1/machines/{machine}/sessions/{id}/turns?since=&limit=&startedAt=&runtime=
→ 200 { "turns": [ { "at": "...", "text": "..." } ],     ← oldest first
        "next": "<opaque cursor>" }
→ 200 { "turns": [], "next": "..." }      ← nothing (new): a normal answer to a poll
→ 400 invalid        limit outside 1..100 (refused, not clamped); startedAt not RFC 3339
→ 401 unauthorized   a principal table is configured and the caller lacks `send`
→ 404 not_found      no such session; or the session exists but no readable record
                     of its conversation could be identified (retryable: true)
→ 409 conflict       startedAt disagrees with the live session; or `since` was issued
                     for another conversation / points past the end of the record;
                     or the session is parked (muster #421): no read starts it
→ 501 unsupported    the runtime, or the peer's build, cannot read turns
```

Implemented by the terminal runtime and by `opencode` (muster #369). The unit is
one assistant entry of the runtime's own record — a line holding the agent's text
for the terminal runtime, an assistant message for `opencode` — and `pending`
exists only where there is a screen to read it from. A page from either is the
same shape and the same boundary. Only `opencode` can answer `409` for a `since`
whose message the record no longer holds, and `404 retryable` is the terminal
runtime's alone.

```
GET /v1/machines/{machine}/sessions/{id}/composer?startedAt=&runtime=
→ 200 { "text": "...", "composerDigest": "..." }   ← the unsent text, and the digest `discard` accepts for it
→ 200 { "text": "", "composerDigest": "" }         ← the composer was read and holds nothing
→ 400 invalid        startedAt not RFC 3339
→ 401 unauthorized   a principal table is configured and the caller lacks `read`
→ 404 not_found      no such session
→ 409 conflict       the composer cannot be read as a whole (clipped by the window, covered
                     by a dialog or a feedback panel) — never answered as "empty"; or startedAt
                     disagrees with the live session
→ 501 unsupported    the runtime, or the peer's build, cannot read a composer
```

The text is what `discard` compares against, and `composerDigest` is the same value
`state.composerDigest` publishes, so a read followed by a `discard` agree by
construction and a composer that changed in between is refused by `discard`'s own
digest check (muster #276). The text is served **only** here: never on the listing,
a single-session read or the event stream. It requires `read` and nothing else
(ruled 2026-10-09); each read is audited (who, which session, how many characters
left, never the text) and the response is `Cache-Control: no-store`.

What the session's own agent wrote — **assistant turns only** (muster #258;
session-abstraction.md §5.8 as narrowed by
`docs/adr/258-assistant-turns-read.md`). A turn is the text of one entry the
runtime recorded as a top-level assistant message. **Never returned, whatever the
record holds:** tool calls and tool results, file contents, the messages a human
or another session sent in, system and hook output, reasoning, a sub-agent's
entries, and the runtime's own synthetic notices. An entry that cannot be
classified with certainty is left out. The wire type has no `status`, `kind` or
`result`: a turn is what the agent said, not the service vouching for it, and
whether the session finished is `state`.

Without `since` the most recent `limit` turns (default 20, at most 100) are
returned. `next` resumes after what was returned — also when `turns` is empty, so
a poller always has a place to resume. A page may be empty while `next` still
moves forward when a long stretch of the record held no agent text; call again
until `next` stops moving. A turn longer than 64 KiB is cut and carries
`"truncated": true`.

`startedAt` is corroborated against the live session before anything is read, as
on `DELETE`; omitted, the read has the weaker id-only guarantee. **Authority:**
with a principal table the `send` grant — the one `input` and `respond` require —
and not `relay` for a peer target; the owning machine applies its own table to the
asserted caller. Each read is audited (caller, session, number of turns, outcome),
never the text. Unrelated to `SessionState.turns`, the liveness count.

```
GET /v1/machines/{machine}/sessions/{id}/record?since=&limit=&startedAt=&runtime=
→ 200 { "messages": [ { "at": "...", "kind": "text" | "reasoning" | "tool" | "image" | "summary",
                        "role": "user" | "assistant", "text": "..."?, "tool": {...}?, "parent": "..."?,
                        "model": "..."?, "media": "..."?, "truncated": true? } ],   ← oldest first
        "next": "<opaque cursor>", "format": 1 }
→ 200 { "messages": [], "next": "...", "format": 1 }   ← nothing (new): a normal answer to a poll
→ 400 invalid        limit outside 1..100 (refused, not clamped); startedAt not RFC 3339;
                     `since` is not a record cursor (a `turns` cursor is not one)
→ 401 unauthorized   a principal table is configured and the caller lacks `record`
→ 404 not_found      no such session; or the session exists but no readable record
                     of its conversation could be identified (retryable: true)
→ 409 conflict       startedAt disagrees with the live session; or `since` was issued
                     for another conversation / points past the end of the record
→ 501 unsupported    the runtime, or the peer's build, cannot read a record
→ 501 unsupported    reason "record-off" — this machine's operator refused the route
                     (`refuseRecordReads`); no grant would help
```

A session's record, oldest first (muster #441; session-abstraction.md §2.16 as
settled by `docs/adr/441-a-sessions-full-record-is-read-under-its-own-grant.md`).
It is a route of its own beside `turns`, with its own wire type, its own cursor
and its own grant, and `turns` is unchanged. A message is `{at, kind, role, text?,
tool?, parent?, model?, media?, truncated?}`; `format` is `1` and changes only when
a key is renamed or removed, so a client skips a `kind` it does not know.

**What a runtime emits is the allow-list of its own reader**, held where the record
is read and nowhere downstream. The terminal runtime emits (muster #442, ADR 442):
`text` in both directions (the assistant's prose by the same predicate as `turns`,
with `model`; what a person typed, a top-level user entry of human origin that is not
the runtime's own markup, with or without images); `reasoning` when the runtime wrote
it out; `tool` for any tool's call, paired by exact id with its result; `summary` for
a compaction; `image` carrying a `media` reference (the person's attachments and the
images a tool returned); and `parent`, the id of the call that started a delegated
sub-task, on every entry the sub-task wrote. Meta and hook output, attachments, queued
placeholders, a message another session sent in, a user message that mixes text with a
block that is neither text nor an image, and any entry the reader cannot classify with
certainty are left out. The `opencode` runtime reads a record too (muster #443): the same
kinds, one entry per part of a message rather than per message, no `parent` (its sub-tasks are
sessions of their own), the settled parts of a message still being written, and a running call
sent once and again, complete, under the same `tool.id`; it declares `readsRecord: true`. The
test stub declares `readsRecord: false` and answers `501`.

**A `tool` message is updated by id.** A call goes out where it was recorded with
`tool.status: "running"` and no result unless the same page reads the result too; a
later page that reads the result carries the same message again, complete — the same
`tool.id`, the call's own `at`, in the position of the result. **A `tool` message
whose `tool.id` the client holds replaces it**; `at` is therefore not monotonic within
a page. `status` is `running`, `completed` or `error`. A result is attached only to
the call with exactly its id (the first result only); one that cannot be tied to its
call is omitted. `input` is the raw JSON the runtime recorded; over 64 KiB it is a JSON
string of its leading part and the message is `truncated`.

The cursor is opaque, bound to the conversation it was issued against, and its own:
it is `400` on `turns`, and a `turns` cursor is `400` here. It holds a position in
the conversation's record and in each sub-task that still needs one, so it can be a
few hundred bytes (it names at most 64 sub-tasks); a page may end inside a line, and
`limit` bounds what a page carries. Without `since` the most recent `limit` entries
(default 20, at most 100) are returned across the conversation and its sub-tasks,
merged by time; `next` is returned even when `messages` is empty. A text field over
64 KiB is cut on a rune boundary and carries `"truncated": true`. `startedAt` is
corroborated against the live session before anything is read, exactly as for `turns`.

```
GET /v1/machines/{machine}/sessions/{id}/record/media/{ref}?startedAt=&runtime=
→ 200 <the image's bytes>   Content-Type: image/png | image/jpeg | image/gif | image/webp
                            Cache-Control: no-store, X-Content-Type-Options: nosniff
→ 400 invalid        startedAt not RFC 3339
→ 401 unauthorized   a principal table is configured and the caller lacks `record`
→ 404 not_found      no such session; or its record cannot be identified yet (retryable: true);
                     or the reference is unknown, malformed, issued for another conversation,
                     expired, or names something that is not an image the reader admits
                     (retryable: false — one answer for all of those)
→ 409 conflict       startedAt disagrees with the live session
→ 501 unsupported    the runtime, or the peer's build, carries no images
→ 501 unsupported    reason "record-off" — this machine's operator refused the route
```

One image of a session's record (muster #442; session-abstraction.md §2.17), by the
`media` reference an `image` entry of the record carries. It is the record route's
sibling and shares everything with it: the **`record` grant** (nothing else stands in
for it, and a principal that holds only `record` is served), **the switch** (`refuseRecordReads`),
`startedAt` corroboration and the relay (the owning machine applies its own table to the
asserted caller, and a relay passes the bytes only after checking they are an image of
a type named above, at most 16 MiB). **Audit:** every read writes one `audit: content-read`
line with `route=GET …/record/media/{ref}` — the route pattern, never the reference —
and `entries=1` when an image left, `0` otherwise. A reference is a place in the
runtime's record bound to the conversation it was read from, holds no content or path,
and is valid as long as the record holds the entry; it stops working when the
conversation is replaced. A reference handed to another session is `404`, as is every
other way it can fail.

**Authority.** With a principal table the `record` grant (§5) — not `read`, not
`send`, not `relay` — and nothing is implied by anything else. Without one a valid
token is the credential, as for `turns`. The owning machine applies its own table to
the asserted caller. **The switch:** `"refuseRecordReads": true` in the config file
(read at start) refuses the route wholesale on that machine — local, peer target and
relayed in alike, whatever the caller holds — with `501`, `reason: "record-off"`; the
grant is checked first. **Audit:** every read writes one `audit: content-read` line —
caller, session, number of entries, outcome, `took=` — never the content, a cursor or
a model; a refused caller leaves a `DENIED` line, and a read relayed to a peer is
audited on each machine. The response is `Cache-Control: no-store`.

```
GET /v1/machines/{machine}/sessions/{id}?runtime=
→ 200 { "machine": "...", "id": "...", "name": "...", "runtime": "...",
        "cwd": "...", "agent": "...", "model": "...", "startedAt": "...",
        "attach": { "kind": "multiplexer", "target": "...",
                    "command": ["...", "..."], "readOnly": ["...", "..."],
                    "shared": true },
        "conversation": { "known": true, "id": "...",
                          "source": "derived", "evidence": "..." },
        "resumeOutcome": { "requested": "...", "honoured": false,
                           "evidence": "..." },
        "identityAssertion": { "asserted": "...", "drifted": false,
                               "evidence": "..." },
        "marker": "...",
        "state": { "status": "working", "confidence": "inferred",
                   "evidence": "...", "since": "..." } }
```

`attach` (§2.8) is how a **human's** terminal reaches the session. `command` is
argv to run *on that session's machine*; the client composes any remoteness
itself, because this service knows which machine it is and not how you reach
it. Prefer `readOnly` whenever the user asked to watch rather than to take
over — the two are different attachments, and offering the wrong one shares a
live keyboard with a running agent. Absent means the driver has no answer,
which is a real answer (§5.7).

`conversation` (§2.9) names the record the **runtime** keeps of this session's
conversation — the one source here that is not the process describing itself.
It has three states and they are not interchangeable: the field **absent** means
nobody looked, `known: false` means somebody looked and could not tell (an
ordinary 200, with the evidence saying why), and `known: true` carries the
identifier plus a `source` saying whether it was matched or dictated. A caller
that reads the absent field as "this session has no record" has turned a driver
without a record store into a finding about somebody's session.

`resumeOutcome` (§2.10) is present only when this session's `create` set
`resume`, and says whether that was actually honoured — never assume it from
a create that merely returned 201, because a resume can be silently ignored
under load and the create still succeeds on a fresh conversation (muster
#72). Absent means no resume was requested; `honoured` absent (with
`evidence`) means the session's own `conversation` has not resolved yet, not
a "no"; `honoured: true`/`false` is the verdict once it has.

`identityAssertion` (§2.14) says what identity **this machine** last asserted
for the session, and whether the runtime still carries it — machine-readable,
where before this fact only reached a caller as prose inside `state.evidence`
(muster #97, #102). Absent means this machine asserted no identity for
this session at all (adopted or foreign); `drifted` absent means an identity
was asserted but not yet corroborated against a live read; `drifted: false`
means the runtime carries it as of this read; `drifted: true` carries
`carried`, naming what it holds instead. The repair, when this machine
attempts one, lands on the *next* read, not this one — a single
`drifted: true` is not a permanent condition.

`marker` (session-abstraction.md §2.1) is the marker the session's `create`
applied — sent as `marker` and ending the resolved name — so a caller groups
sessions by type without a suffix test on `name` (muster #165). It is
read-only: no route writes it, and `POST …/labels` cannot touch it, because it
is not a label. A rename keeps it. It travels through a peer relay like every
other field on the session. Absent means this machine holds no such record,
never that the session is untyped.

```
POST /v1/machines/{machine}/sessions/{id}/input
{ "text": "...", "submit": true, "resumeIfStranded": false, "replaceIfStranded": false,
  "expect": "<composerDigest>",
  "from": { "agent": "...", "session": "...", "relayOfHuman": false },
  "route": "auto" | "terminal" | "inbox" | "<module>" }
```

**The body's shape is checked before any driver is resolved (muster #320).**
`text` is required and must not be empty (a missing or `null` `text` reads as
empty): the refusal is `invalid` (400), its message says `text` is required,
and nothing is delivered. A field the request does not define is `invalid`
(400) too, with the field named and `text` named as the field a message goes
in — it is never ignored, so `{"prompt": "..."}` (the name `create` uses) cannot
be read as an empty send. The defined fields are exactly those in the shape
above. A `resumeIfStranded` retry repeats the same text and so needs no
exception.

**`submit` (muster #257) defaults to `true`.** An absent (or `null`) `submit`
means the text is submitted; only an explicit `false` stages it in the
composer without submitting. Before #257 an absent `submit` read as `false`.

**`route` (muster #184) is optional and chooses the delivery path.**
Absent, `""` and `"auto"` mean the same thing. A value outside the closed set
is `invalid` (400) naming every accepted value, before any driver is resolved.
The set is `"auto"`, `"terminal"`, `"inbox"` and — since #185 — the name of each
optional external delivery module this machine has **enabled** (`<module>`
above). With no module enabled the set, and the message, are exactly what they
were.

**A live lane is the session's only input path (muster #257).** A session
holds a *live lane* when `delivery.clientConnected` reads `true` on a session
read. While it does, every `/input` is delivered by the lane's module or
refused with nothing written; it never reaches the terminal. The terminal path
remains for a session with no live lane. `/discard`, `/keys` and `/respond` are
unchanged: dialogs and recovery still go through this service.

| `route` | Who | Session with a **live lane** | Session with **no live lane** |
|---|---|---|---|
| `auto` (default) | a principal holding the **human-relay** grant, or a trusted relay's assertion of it | that module — the message arrives as the user's own turn, **unlabelled** | the terminal, **unlabelled** |
| `auto` | anyone else | that module, **with the sender label** | the terminal, **with the sender label** |
| `terminal` | any caller | **refused**, nothing written | the terminal (labelled unless the caller is a human relay) |
| `inbox` | any caller | the inbox — or a **refusal**, never a downgrade | the inbox — or a **refusal**, never a downgrade |
| `<module>` | any caller | that module — or a **refusal**, never a downgrade | **refused**, nothing written |

On a session with a live lane these shapes are `refused` with nothing written
and a `reason` that names the lane and says to send without them: an explicit
`route: "terminal"` (from any caller, a human relay included), `submit: false`,
`resumeIfStranded` and `replaceIfStranded`. The same refusal answers a send
whose text this service itself left stranded in the composer: it is not sent
through the lane as well (it would arrive twice) and not resumed. The reason
says to clear the composer with `discard` and send again. A send whose lane
stopped being usable between the check and the write is refused too ("send
again"); the next send, finding no live lane, takes the terminal. Each refusal
is counted under `route.refused.lane_live` (split by the shape asked for) and
`route.refused.lane_lost`, so a deployment can be watched for callers still
asking for the terminal.

- **`auto` never tries the inbox (#257).** The inbox is used only when a caller
  names it, so an inbox that is unreachable can no longer cost an `auto` send
  anything. The `route.auto_fallback` counter is retired.
- **The label is mandatory for everyone but a human relay, on every route.** A
  caller that does not name itself is labelled with the one fact the service
  holds — the authenticated principal (on a trusted relay, the principal the
  request was made on behalf of), then the machine where the request entered. A
  caller's own `agent` and `session` are never replaced. This holds for an
  explicit `terminal` and for a module route as well (since #257 they are
  labelled, not refused): an agent's send is never recorded as human-typed
  input, which is what #180 M8 guaranteed.
- **The human-relay fact is never inferred from anything a caller sets** — not a
  header, not `from`, not `relayOfHuman`. It is the principal's own configured
  grant, or a trusted peer's assertion of it (session-abstraction.md §13, #180
  L3). ⚠️ With no principal table configured every caller presents the one
  shared token, nothing tells a relay from anyone else, and the relay assertions
  are honoured as they always were: on such a machine the rule above holds only
  as far as the token does.
- **An explicit `inbox` is refused when the session cannot take it, and nothing
  is written.** The receipt is `refused`, names `delivery.route: "inbox"`, and
  its `reason` says what stopped it: no inbox configured, no index entry, no
  permission-mode class, a body that cannot be carried intact, no transcript to
  confirm a delivery against, an unreachable socket, or a write that failed
  before any byte was sent. `route: "inbox"` with `submit: false`,
  `resumeIfStranded` or `replaceIfStranded` is `invalid` (400): those name a
  composer the inbox does not have.
- **A decline before any byte is written never becomes a different path.** Once
  any byte has reached the inbox the message may be in the receiver's hands, and
  it is **never sent down the other path**: see the receipt below.
- **`route` travels through a peer relay** as `""` for auto and as itself
  otherwise. **A human relay's `auto` crosses as `auto`** (#257), together with
  the human-relay assertion, and the machine that owns the session decides: its
  live lane, else its terminal. (Before #257 the entering machine rewrote it to
  `terminal`, so it could never reach a lane on another machine.) A peer built
  before #257 receives the same `auto` plus the assertion; an entering machine
  built before #257 still sends `terminal`, which a new owner refuses on a live
  lane, so upgrade peers together. A peer built before #184 answers an explicit
  `"inbox"` with its own `invalid`; the caller gets that answer, never a
  downgrade.
- **A module route (muster #185)**: `submit: false`, `resumeIfStranded` or
  `replaceIfStranded` is `invalid` (400), because a module has no composer; and
  a forced module the session cannot use right now — not live for this session,
  unavailable, refused the runtime build — is a **refusal with nothing written**,
  never a fall back to the terminal. A module that reports it cannot verify its
  peer (`peerCheck: false`) is treated as not live: nothing is delivered without
  that check. `auto` on a session whose lane is *not* live (degraded, or the
  session never had one) is carried by the built-in path.

**`from` (muster #158) is optional and labels the message with its
sender.** Absent means the service labels the message itself for any caller that
is not a human relay (`route`, above, #184) and leaves it unlabelled for one that
is — before #184 absent meant unlabelled for everyone. The
label is `agent · session · machine`, with empty parts skipped, and it reaches
the receiving session as follows:

- **On the inbox path**, it is the envelope's sender-name attribute. The
  receiver rebuilds the envelope and compares bytes, so the service emits only
  a name that the receiver's own normalisation leaves unchanged. A name it
  cannot guarantee is **dropped, never the message**: the name is optional to
  the receiver, while the permission-mode class the same envelope carries is
  not, and losing the envelope would hold the message silently.
- **On the terminal path**, which has no envelope, the same label goes on as
  one short first line, `[from: …]`, added after the runtime-syntax guard has
  judged the caller's own text.

The three caller fields are **unverified statements**, and the label must not
be read otherwise:

- `agent` and `session` are the caller's own claim. Under a single shared
  token nothing distinguishes one bearer from another (`Caller.Principal` is
  provenance, not identity), so a service carries them and cannot check them.
- `relayOfHuman: true` adds one line to the text saying the sender states it
  is relaying an instruction from the human operator. It is a **label, never
  authority**: it must not change what the receiver is allowed to do, and no
  grant, policy or routing decision in this service reads it. Making it
  verifiable would need a credential only a human holds — out of scope here.

**`machine` is stamped by the service; a caller-supplied value is ignored.**
It is the machine where the request entered the fleet. On a relayed hop the
entering machine forwards its own stamp as `from.machine`, and the owning peer
keeps it only because the request arrives as a relay (it carries the
on-behalf-of assertion) and only when it names one of that peer's configured
peers — the same trust bound as the on-behalf-of assertion itself. Otherwise
the machine is omitted, never guessed.

A `resumeIfStranded` retry repeats the same `from` as well as the same text:
on the terminal path the record of what was delivered includes the label line.

`resumeIfStranded` completes a delivery that returned `unknown` — the text
reached the composer and could not be confirmed. The service submits it only if
its own record says that text is what it delivered there; text it did not place
is never submitted. Send the same text: this finishes one delivery rather than
starting another.

**`replaceIfStranded` (muster #112) is the door out when the caller wants
DIFFERENT text instead of finishing the old delivery.** `resumeIfStranded`
only ever completes the delivery already sitting in the composer — a caller
that wants to send something else has no use for it, and until #112 had no
other way in either: the busy-composer refusal below applied even though the
service's own record showed the "human typed" attribution was wrong. With
`replaceIfStranded` set, and when the service's own record shows the
composer holds a delivery **this service itself placed** there (never on the
strength of any other evidence — a human may have attached and typed since,
and the service cannot compare pasted bytes to rule that out), it clears that
text and delivers the new text in its place. Both flags set at once is a
contradiction — asking to finish the old delivery and replace it in the same
call — and is refused outright rather than picking one silently.

A refusal that reaches this path distinguishes three cases a caller could not
tell apart before #112, all previously collapsed into one message asserting a
human was typing:

- **the service's own record shows this composer holds a delivery it placed,
  and the new text is identical to it** — resend with `resumeIfStranded` to
  finish that same delivery, not `replaceIfStranded`.
- **the service's own record shows this composer holds a delivery it placed,
  and the new text is different** — this is the case that used to have no
  answer. `resumeIfStranded` would finish the OLD delivery; `replaceIfStranded`
  discards it and delivers the new text; `discard` (below) clears it without
  delivering anything.
- **no matching record exists at all** — with *neither* flag set, this is
  still the original, unchanged answer: the composer may hold a person's own
  unsent draft, and this service will not guess otherwise. With either flag
  set, the **draft rule** below decides.

**The draft rule (#180).** The service never clears or submits text sitting in
a composer unless (a) its own record proves the text is its own stranded
delivery, or (b) the call carries `expect`, the composer's current digest
(`composerDigest` on a session read), proving the caller saw what it asks to
have cleared. Otherwise it refuses and the text stays. Concretely:

- `resumeIfStranded` submits a stranded delivery only while the service's
  record matches the composer — by the digest it took at strand time, by the
  text itself read back row by row, or, for a paste the runtime collapsed to a
  `[Pasted text #N +L lines]` summary, by the marker it saw that paste land as.
- A record that lapsed (after `strandedRetention`, 30 minutes) or was
  replaced by a newer strand is kept longer as proof only: when the composer
  still holds exactly that text, either flag clears it and delivers this
  call's text — muster #135's case.
- For any other composer text, either flag needs `expect`; with a matching
  `expect` the service clears exactly that content and delivers this call's
  text. A non-matching `expect` refuses — the composer changed after it was
  read.
- `replaceIfStranded` on the service's own stranded delivery whose content
  has since changed (a person may have edited it) needs `expect` too.

Every refusal under the rule carries the composer's current digest and both
ways forward (`replaceIfStranded` with that `expect`, or `discard`). The
response still carries a refusal, never the foreign text delivered, if a
permitted clear does not fully succeed (a `#87`-proven-futile residue, or a
partial clear). `expect` has no effect without one of the two flags.

```
POST /v1/machines/{machine}/sessions/{id}/discard?expect=<composerDigest>&startedAt=&force=
→ 202 { "accepted": true }
→ 409 if the digest does not match what is there now, or none was supplied
→ 409 also if the clear could not be confirmed to have finished — the message
  says which of three things happened: the composer is unchanged and this is
  the first time (safe to retry with the same digest), the composer is
  unchanged and a PRIOR full clear pass already proved retrying does nothing
  (do not retry with the same call shape — see `force`, below), or the
  composer is now damaged (re-read before doing anything else; do not retry
  blind)
→ 409 also if the composer is taller than the driver's capture window, or its
  opening fence is above the visible pane — its content could not be read in
  full from rows the driver can trust, so neither "already clear" nor a
  corroborated clear is honest; retrying with any `expect` or `force` gets the
  same refusal (muster #149, #169)
```

Removes unsent composer text without submitting it. `expect` is
`state.composerDigest` from a read, sent as the **query parameter** shown above
— not a JSON body field, even though `composerDigest` is also the name of a
field in the read response that produced it. It is **required when there is
text**: this deletes somebody's typing, and a caller that has not seen the
current text has no business removing it. An already-empty composer returns
202, so a retry after a timeout is safe.

`force=true` (muster #136) authorises a stronger clear mechanism, but
**only** once a prior call has already been refused as proven-futile against
this exact residue (see below) — it has no effect on a first attempt, and a
driver may ignore it entirely before that point. It never relaxes `expect`:
the digest requirement above is unconditional whether `force` is set or not,
because a forced clear is *more* destructive than the ordinary pass, not
less.

A driver that cannot confirm its own clear keystroke finished reports that as
409 too, never 400: the request was well formed, and a keystroke failing to
land is not the caller's mistake to fix by resending the same bytes. Three
outcomes share that 409, and need three different next steps:

- **unchanged, first pass** — the composer reads exactly what the caller
  already corroborated. Nothing was destroyed, so retrying with the same
  digest is exactly as safe as the first attempt was.
- **unchanged, proven futile** — a *prior* call already spent a full pass
  against this exact residue and it did not move. This driver does not press
  again against text already proven not to respond; it refuses before
  touching the pane at all, and the message deliberately does not repeat
  "safe to retry" — repeating it was #87's failure mode, a caller that
  followed that advice to the letter, four times, and made zero progress
  each time. The message instead names `?force=true`: retried with that flag
  on the SAME call shape (same `?expect=`, still required — force does not
  relax corroboration, only what happens once it has already passed), the
  driver reaches for a stronger, character-budgeted clear mechanism past
  whatever structural key choice defeated the ordinary pass (muster
  #136). `DELETE /v1/machines/{machine}/sessions/{id}` still works — closing
  a session always clears its composer along with everything else — but it
  is no longer the *documented* remedy for a stuck composer alone: a session
  carries a conversation, a bridge, and in-flight work that respawning does
  not recover, and the fix belongs at the scale of the problem.
- **damaged** — the keystroke registered PARTIALLY: some of the text is
  gone, none of it cleanly, and the composer now holds neither what the
  caller saw nor nothing — worse than either extreme, and not safe to retry
  blind. The message carries the residue's current digest, so the caller's
  next legal call needs no extra re-read to learn it.

A composer taller than the driver's capture window (muster #134) is
refused before any key is pressed — and so is one whose opening fence sits
above the visible pane, because the rows above it are scrollback, not the live
screen (muster #169; ADR `169-a-composer-is-read-from-the-visible-pane`).
So is one whose closing rule is cut off by the pane's bottom edge — its opening
rule and prompt row are the last rows shown, as when a tall notice above the
composer leaves a short pane no room below it (muster #216; ADR
`216-a-composer-cut-off-by-the-pane-bottom-reads-clipped`). None of these refusals resolves by
retrying: no `expect`, no `force` and no wider read changes it, because nothing
the driver can read proves the rows it cannot see hold nothing worth keeping
(ADR `149-a-clipped-composer-has-no-in-driver-proof`). `input` and `keys` refuse
the same state with the same remedy, and the exit is a person reading or
clearing the composer at the pane itself. A driver counts each such refusal, per
verb, so how often real traffic reaches the state can be read afterwards.

A single call also stops pressing early once it has clear evidence a pass has
stalled — movement observed, then several presses in a row that changed
nothing — rather than spending the rest of its window on keystrokes already
proven to do nothing against text nobody has re-read.

```
POST /v1/machines/{machine}/sessions/{id}/rename?startedAt=&runtime=
{ "name": "new-name" }
→ 202 RenameAck (session-abstraction.md §2.5a)
→ 400 if the name is empty, or a substrate would silently mangle it as given
→ 409 if startedAt disagrees, or the new name is already in use here
```

**Renaming changes the `id`**, not a label beside it — on a substrate where the
id is the name an operator sees, anything less renames the session in this API
and leaves their terminal saying the old thing. Send `?startedAt=` for the same
reason `DELETE` wants it: acting on the wrong session here succeeds *silently*
and leaves it wearing somebody else's name.

**`name` is refused, not silently cleaned, when a driver's own substrate would
hold a different string than the one asked for** (muster#223) — a tmux
driver refuses a name it would have to mangle (a `.` tmux itself turns into
`_`, a leading `-` an argv parser downstream would read as a flag, `:`, the
multiplexer's own target separator) rather than rename to the mangled form and
announce an id that is not the multiplexer's real one. This is deliberately
NOT `create`'s behavior (session-abstraction.md's "naming rules belong to the
driver" — a created name may still be silently cleaned and numbered): a
rename's `to` is client-facing top to bottom, `session.renamed` announces it
verbatim, and `RenameAck` carries no field a caller could read the actual
applied name back from if a driver quietly changed it. The 400's message names
the clean form the caller could retry with.

Subscribers receive `session.renamed` carrying `from` and `to`. A client
filtering by id **must** re-key on it, or it stops matching a session that is
still alive — and cannot tell that from the session having died. **This event
fires more than once per rename** — §4's event-plane section covers what the
`corroboration` field on each one means, and why waiting for the second is
worth doing before treating a rename as durable.

**The `202` body's `title` field is a SEPARATE fact from `session.renamed`**
(muster#222): on a runtime that keeps its own idea of a title apart from
the id — the transcript a title-reconciling client would otherwise trust more
than this API — whether that title was brought to the new name too, in the
closed vocabulary `synced` / `pending` / `failed` / `not_applicable`
(session-abstraction.md §2.5a). `session.renamed` does **not** carry it: the
id-change announcement stays exactly as timely as it always was, and nothing
about the title half is allowed to delay it.

A `title.status` of `pending` or `failed` is retried by **`POST`ing the same
rename again with the identical `name`** — `to == ref.ID` is not an error
(§3), and re-attempts only the title half; the id has nothing left to move. A
busy or stranded composer needs no new rule here: the delivery this makes is
exactly one `/input`-shaped `/rename <name>` call, and is refused under §2.4's
existing protection like any other — `title.receipt` carries that refusal's
own `outcome`/`reason` verbatim, so a caller reading "the same rules as
`/input`" never has to take it on faith.

A `runtime` whose driver does not implement the optional title-syncing
capability at all (session-abstraction.md §4) reports `title.status:
"not_applicable"` — produced by the SERVICE, never by that driver claiming it
for its own runtime. `title` **absent** (the key missing) means nothing is
stated at all — a peer built before this field existed; never conflate the two
(§5.7).

```
POST /v1/machines/{machine}/sessions/{id}/labels?startedAt=&runtime=
{ "labels": { "issue": "153", "stale-key": null } }
→ 200 <the whole session, labels included>
→ 409 if startedAt disagrees with the live session, or is supplied and the
      live session's start time is unknown
→ 400 if the merged result exceeds the bounds, or a key is invalid
```

**Labels change by merge** (muster #153): a key with a string value is set,
a key with `null` is deleted, a key not named is left alone. The bounds apply to
the **merged result**, so a patch that deletes keys can bring a full map back
under them; a refused write changes nothing. Send `?startedAt=` for rename's
reason — labelling the wrong session succeeds silently and binds a stranger to
somebody's work. Without it the write gets the weaker guarantee of the id alone.

It needs the **`label` grant** (§5), not `rename`. Rename changes the handle
every other caller addresses a session by; a label changes nothing anyone
addresses anything by, and a session labelling *itself* once it knows its work
must not need the power to rename every session on its machine. Labels sent in
a **create** body need only `create`, like `name` and `marker`.

Subscribers receive `session.labels` with the complete map after every change,
and after a create that carried labels. A peer on a build without this route
answers with its router's bare `404`; a relaying service **must** report that as
`unsupported`, never as `not_found` — the session may be alive, and the peer
simply cannot store labels.

```
POST /v1/machines/{machine}/sessions/{id}/remote-control?runtime=
{ "enabled": true }
→ 202 { "accepted": true }
→ 400 if "enabled" is missing
→ 409 (retryable) if the driver cannot act safely: the channel's state could
      not be read, the session is not idle, a prompt is open, or the composer
      holds unsent text
→ 501 unsupported if the runtime does not declare capabilities.remoteControl,
      or enabled is false and its `off` is false
```

**Turns a RUNNING session's remote control on or off** (muster #269). `202` is
intent only, like `interrupt`: the confirmation is `state.controlChannel`
changing (§2.3), which fires `session.state`. The wire carries one concept in
three places — the `remoteControl` flag at create, `state.controlChannel`, and
this verb — and they agree: a session created with `remoteControl: false` reads
`off`, and so does one this verb turned off.

`enabled: true` on a `failed` channel is the reconnect; there is no separate
verb. A request for the state a session is already in is a no-op that answers
`202`, so the verb is safe to retry. That is more than convenience: the runtime
measured turns remote control on with the same command that, when it is already
on, opens a disconnect dialog — so a driver reads the channel first and refuses
(`409`) when it cannot, rather than send blind. A dialog the call opened is
always dismissed before it returns.

It needs the **`remote-control` grant** (§5), its own and denied by default.
Turning remote control on makes the session drivable from off the machine, which
is an exposure change and not an input, so neither `send` nor `keys` implies it
and the refusal names it. A call to a peer additionally needs `relay`, and the
peer applies its own `remote-control` grant to this service's credential. A peer
on a build without the route answers with its router's bare `404`; a relaying
service **must** report that as `unsupported`, never as `not_found`.

The runtime's own slash command (`/rc`, `/remote-control`) is the same act typed
instead of requested, so `input` closes that door too. The
`gateRemoteControlInput` setting (muster #272) governs it: with it on, `input`
delivers those two commands only to a caller holding `remote-control` or
`human-relay`, and the refusal names both grants. The setting is **on by
default** (muster #275): the key absent from the config file, or no config file
at all, means on, and only an explicit `false` is the opt-out. #272 shipped it off for one release so existing callers had time to
move to this verb; the default flipped after that notice. On a machine that has
opted out, the grant governs this verb and not the older door. The verb's own
command is not subject to the setting. A relayed `input` carries the fact that
the original caller held `remote-control` as an assertion the owning machine
honours only from a configured peer that itself holds `remote-control`; the
peer's own grant never stands in for the caller's.

`runtime` is an **optional** query parameter on every single-session endpoint
(`GET`, `input`, `respond`, `discard`, `rename`, `labels`, `keys`, `interrupt`,
`DELETE`) and on `POST …/sessions` (`create`, in the JSON body as
`"runtime"`). A session `id` is scoped to `(machine, runtime)` — not to
`machine` alone (session-abstraction.md §2.2) — so two runtimes on one
machine may legally reuse an id, which this URL cannot otherwise
disambiguate.

**When `runtime` is supplied, it is used outright** — the caller named its
own runtime and nothing here second-guesses it. Omitted, and the machine
runs exactly one local runtime, resolution is unambiguous and `runtime` is
never required; that was every machine's whole history until a second
runtime existed to register.

**Omitted with more than one local runtime registered** resolves
existence-first, THEN a configured default as tiebreak
(session-abstraction.md §7.1a, muster issue #60). A runtime the operator
enabled that is still being retried (§3.1, muster #297) counts as one of those
runtimes while it is: it cannot be asked and cannot be routed to yet, but
routing as if it did not exist would send a create to whichever runtime happens
to be up, and move a bare id's answer the moment the late runtime registers.
It cannot confirm absence, so an id no other runtime holds is `invalid` (400)
naming it; a create whose configured default is that runtime is
`runtime-unavailable` (§3.1).

- a nonempty `id` (every endpoint but `create`) is checked against every
  registered runtime's own record of what it has ever had. Exactly one
  affirms it → that runtime, full stop, even against a machine configured
  with a different default — a default must never steer an id that plainly
  belongs elsewhere into a false `not_found`. More than one affirms it, or
  the check cannot be completed for all of them → `invalid` (400) naming the
  runtimes involved; never `not_found`, which would assert something untrue
  about the fleet.
- a genuine miss — `create`'s own case, and what a nonempty `id` reaches once
  every runtime has affirmatively confirmed absence — resolves to the
  machine's configured **default runtime**, when one is configured.
  Unconfigured, this is the ambiguity's older shape: `invalid` (400) naming
  it, exactly as before a default runtime could be configured at all.

**Every 2xx and every error response from a call that reached this
resolution carries `Fleet-Runtime: <runtime-id>`**, naming the runtime that
actually served the request — the one piece of information a bare-id call
against more than one runtime previously had no way to surface. **It also
carries `Fleet-Runtime-Resolution: default` when, and only when, the
configured default runtime was the tiebreak.** Its absence is itself
informative: every other resolution — an explicit `runtime`, the sole
registered driver, or an existence match — is exactly as trustworthy as a
caller naming its own runtime, because each is either the caller's own word
or a fact this machine just confirmed by asking. Only the default is a
genuine guess wearing a configuration's authority, and a caller in a
position to care (retrying a destructive call, auditing a surprising
`not_found`) can tell the two apart without reading a log
(session-abstraction.md §5.7, §7.1a guardrail 2).

**A proxied call to a peer machine carries neither header.** Resolution
never reaches this machine's local runtimes at all for a peer-addressed
call — see session-abstraction.md §13.1 and §7.1a's federation note — so
there is no local runtime id to report and the configured default plays no
part in it.

**A `runtime` the caller supplied on a peer-addressed call is forwarded to the
peer, verbatim, as `runtime=` on the call it makes** (muster #461). This
machine does not interpret it — it holds no record of the peer's runtimes, and
the resolution stays the peer's (session-abstraction.md §13.1) — but the
caller's word is the one thing that settles an id the peer holds under two of
its runtimes, which the peer otherwise answers `invalid` (400) asking for
`?runtime=`. That answer, and a `not_found` for a runtime the peer does not
have, reach the caller as the peer's own. It is forwarded on every
session-addressed operation that accepts `runtime` here, and on none that does
not address a session (`create` carries its runtime in the body; a listing is
not addressed to one). A call that supplies no `runtime` reaches the peer
exactly as it did before: no query key is added.

On `GET …/sessions/{id}`, which this machine answers from the peer's listing,
naming a `runtime` also picks the listed row: the one whose `runtime` is the
one named. A row that states no runtime (a peer older than the field) stands
only when it is the single row with that id, and when none qualifies the call
falls through to the peer's own answer for it.

> Origin: session-abstraction.md Appendix A, F1; the default-runtime
> tiebreak and its headers: muster issue #60.

```
POST /v1/machines/{machine}/sessions/{id}/input?runtime=
{ "text": "..." }                      (submit defaults to true: §3.3)

→ 200 { "outcome": "submitted" | "queued" | "refused" | "unknown" | "stalled" | "ended",
        "reason": "prompt holds unsent input",
        "delivery": { "route": "terminal" | "inbox" | "module",
                      "module"?: "<name>" },
        "sessionIds"?: ["<session id>", ...] }   (refused: `{id}` was a conversation id, §2)
```

**`stalled` (muster #391) says the session reads `working` and has drawn nothing
for the whole stall bound, and that nothing was written.** It is an ordinary
`200` like `refused` and for the same reason — an expected domain outcome, not
a fault — but it is not a `refused` and must not be handled as one: a refusal
clears by waiting or by the caller acting on the composer, while a stall clears
only when the session is recovered, and no rewording of the send changes the
answer. The session's own state says the same (`state.stalled`, below), and
`reason` names it. It is decided before any path is chosen, so `delivery` is
absent. **A client that does not know the value must treat it as "not
delivered", never as `queued`** — the whole point of the answer is that `queued`
would have promised what cannot happen. Recovery is the client guide's section
on a stalled session: `interrupt`, then `DELETE` the session and `create` again
with `resume` if it stays stalled.

**`ended` (muster #411) says the session's own process exited while the
delivery was being confirmed.** It is an ordinary `200`, on the terminal route
and on a module lane alike, and it is the honest answer to a `/quit` (sent with
`human-relay`) that does end the session: the confirmation that was waiting for a
turn or an emptied composer can never come, and `unknown` would have said the text
is sitting there unsent and invited a retry on a session that no longer exists.
`reason` names the exit status when the service knows it (the pane's own status;
a session already gone from the listing carries the status the service captured at
its exit, within ten minutes of it) and says plainly when none was recorded. It
does not say the text was read. **Do not retry on `ended`**, `resumeIfStranded`
included: nothing is kept for the session, and the next send to it is answered
`refused`. Read the session's closed record
(`/v1/sessions/closed`) for the structured exit. A client that does not know the
value must treat it as "not delivered", as for `stalled`.

**`delivery.route` (muster #184) names the path that made the receipt.** It
is `inbox` for anything the inbox path decided — a delivery, an `unknown`, or a
refusal an explicit `route: "inbox"` earned — and `terminal` for everything the
terminal module produced. It is **absent** when the receipt names no path: a
refusal made before any path was chosen (a busy composer lock, the runtime-syntax
guard, contradictory flags), a driver with a single path, or a peer built before
the field. Absent means "not stated", never either value, and a value this build
does not recognise is read as absent: the outcome is the part that matters. It
is a `DeliveryReceipt` field (session-abstraction.md §2.4) and travels through a
peer relay untouched.

**`delivery.route: "module"` (muster #185) says an optional external
delivery module carried the send, and `delivery.module` names which.** It is
present exactly when the route is `module`. A module that confirmed the runtime
accepted the message answers `queued` — the same bar as the terminal's
transcript confirmation: a user-origin turn the runtime took is not an
acknowledgement from the agent, so it is never `submitted`. A module that wrote
the message and cannot say whether it landed answers `unknown`, and **the message
is not sent again on any path**: for `strandedRetention` (30 minutes), or until
the module confirms it, every further send of the same text from the same sender
to the same session is answered with the same `unknown`, exactly as for the
inbox. A receipt naming `module` with no module name reads as absent.

**A driver that delivers over a target session's own inbox instead of the
terminal surface (muster #119) can additionally answer `delivered` |
`held` | `denied` | `expired` | `dropped`** — session-abstraction.md §2.4 has
the full vocabulary and why those five are not folded into the four above.
Since #184 which path a send takes is also the caller's to ask for (`route`,
above); when it does not, the service chooses per sender and per target, and
`delivery.route` says what happened.

⚠️ **Of those five, only `delivered` is reachable, and since #184 it is a
statement about the receiver rather than about a socket** (muster #120,
#148). The receiving runtime's status frame is routed to a reply address that
must be a socket bound in the receiver's own namespace, which this service does
not have — so a sender reads nothing back from the inbox itself, and `held` is
still not produced. What changed is that an inbox write is now **confirmed from
the receiver's own transcript**, the same evidence the terminal path uses:

- `delivered` — the transcript recorded this message as a peer message. Read it
  as "the receiver took it", not as proof the model has acted on it.
- `unknown` on `delivery.route: "inbox"` — bytes reached the socket and the
  transcript did not record the message within the confirmation window (a
  receiver that is holding it, has not written it yet, or dropped it), or the
  write itself broke part-way. **The message may have arrived. It was not sent
  again, and it will not be:** for `strandedRetention` (30 minutes), or until the
  message is recorded, every further send of the same text from the same sender
  to the same session — including `resumeIfStranded`, a forced terminal send and
  an explicit inbox request — is answered with the same `unknown` and writes
  nothing on either path. Different text, or the same text from another sender,
  is a different message and is not held. `resumeIfStranded` is a terminal
  operation and is **not** the way to retry an inbox `unknown`: read the
  session's transcript, and wait.
- A machine whose index names no permission-mode class for a session, whose
  session has no locatable transcript, or whose text cannot be attested never
  reaches the inbox for it: a named `route: "inbox"` is refused with nothing
  written (#257: `auto` never tries the inbox, so there is no fallback to carry
  it).

**`text` is capped by the same effective limit `prompt` carries on create**
(muster #114, #130). Over that, the call is rejected outright —
`invalid` (400) naming the limit and the caller's actual size — before any
driver is even resolved, rather than reaching the composer and stranding
there with no exit but `resumeIfStranded` or destroying the session. Chunk
longer content instead: several calls, each comfortably under the limit —
the mitigation #112 already verified end to end.

`submitted` is genuinely part of this type — it is what a driver reporting
`confirmsDelivery: true` on `/v1/runtimes` would return here — but no driver
in this fleet currently declares that capability, so `input` cannot actually
produce it today; a confirmed submission reports `queued` instead. api.md's
known-gaps section tracks this.

**A refusal is `200`, not an HTTP error.** Refusal is an expected domain
outcome carrying structured information, not a fault. Mapping it to 4xx would
train clients to treat it as an exception and retry — which is precisely the
behaviour the refusal exists to prevent. HTTP errors here mean the driver could
not be reached or the caller is not permitted; they never describe what the
driver decided.

A refusal here may also describe the **text**, not the session: a driver may
refuse caller text its own runtime would read as something other than a
message, before looking at session state at all — session-abstraction.md §3,
muster issue #53. That refusal happens whether or not the addressed
session exists, and it is never a candidate for `resumeIfStranded`: the text
never reached a composer to strand.

```
POST /v1/machines/{machine}/sessions/{id}/respond?runtime=
{ "choice": 1, "nonce": "<SessionPrompt.nonce>" }
                         // or {"nonce": "..."} to accept the highlighted option
                         // or {"cancel": true, "nonce": "..."} to dismiss
                         // or {"choices": [1, 3], "nonce": "..."} on a prompt
                         //    reporting multiSelect: tick exactly these, move on one step
                         // or {"text": "...", "nonce": "..."} on a prompt reporting
                         //    freeText: type the text into the free-text row and confirm it
                         //    (with "choices" too, on a multiSelect prompt)

→ 200 { "outcome": "submitted" | "refused" | "unknown", "reason": "..." }
→ 400 invalid   // choices empty, repeated, < 1, or combined with choice/cancel;
                // text empty or blank, over the input byte limit, or combined with choice/cancel
```

The set is the same three outcomes `keys` reports, and the meanings match:

- `submitted` — the answer was delivered and the driver saw it take effect: the
  prompt left the screen, or the runtime confirmed it processed the reply. This is
  evidence `input` never has (its own confirmation is the composer emptying), which
  is why `submitted` is reachable here and `queued` is not.
- `refused` — nothing was sent. The session is not at a prompt, the `nonce` no
  longer matches, the choice names no option, or the prompt's kind does not take
  that form of answer. `reason` says which. "At a prompt" is the same reading of
  the screen that `state.prompt` is published from, so a screen whose state
  carries a prompt is a screen `respond` can answer. The two are separate
  captures, though, and the pane can move between them: when a state read of the
  session reported a prompt within the last two minutes and `respond` finds none
  on the screen, it reads the screen a few more times (a capture can land on a
  repaint), and if every read agrees the refusal opens by saying that a prompt was
  reported and is gone, rather than that nothing is being asked (muster#467). A
  session no state read ever reported a prompt on is refused exactly as before.
- `unknown` — the answer was sent, once, and the driver could not confirm it: the
  prompt is still on screen afterwards, the key landed in the composer instead of
  the prompt, or the runtime reported it did not process the reply. It is **not**
  sent again. Read the session's state before answering again, because the prompt
  may already be answered; `reason` names what was seen.

`queued` is never an outcome of `respond`: it means a send was accepted and its
submission is unconfirmed (`input`'s receipt), and an answer to a prompt is either seen to
take effect or is not. A relay to a peer returns the peer's receipt unchanged, so a
client sees exactly the set above through either machine.

`state.waitingOn` discriminates `waiting_input`, which carries two situations
needing opposite handling: `prompt` (answer it) and `unsent-input` (do not send
to it). Only the first has a `prompt` to branch on, so without this field they
are separable only by reading `evidence` — prose explicitly not to be parsed.
Absent means unclassified (§5.7), not "no reason".

A session out of quota is **not** one of these: it reports `quota_blocked`
(§2.3), because nothing a caller sends will unblock it.

`state.lastTurn` — when present — says how the most recent turn **ended**:
`{"outcome":"failed","reason":"…","retryable":true}`, or — for a session a
service restart found again — `{"outcome":"interrupted",…}`: the turn began and
never finished because the service stopped, which is not the same as having
finished. It exists because a turn
that died and a turn that finished leave the same screen: an error, a settled
status line, an empty composer. Both are honestly `idle`, and a supervisor that
cannot tell them apart silently abandons the work.

A turn that finished normally but ran on a provider, or a model, the machine's
operator did not allow (muster #293, #401) is reported here too, as `failed`, with
the provider, the model and the rule that refused it in `reason` — it is the only
way an idle session says it went somewhere it should not have.

Absent means the screen said nothing about it — **not** that the turn
succeeded (§5.7). `retryable` is the runtime's own word for the failure, not
our judgement of its error code: when true, sending anything resumes the
session and no human is required.

`state.controlChannel` — when present — is what the RUNTIME says about its own
remote-control connection: `active`, `connecting`, `reconnecting`, `failed` or
`off`. `off` (muster #269) means the driver has positive evidence the session has
no remote control — it was launched without it, or the runtime's own durable
record shows a disconnect after the last enable — and is never inferred from the
mere absence of a label. `failed` may also be read from the runtime's own
durable record (muster #270): its disconnection notice, when that is the newest
channel entry and no enable entry followed within five minutes, the notice
being the `reason`. Inside those five minutes nothing is reported, and
`connecting` and `reconnecting` are reported only when the footer shows them,
since no record entry carries either. It is the runtime describing itself, not a claim about whatever is at the far
end, and this service still does not model bridges.

It exists because `failed` is otherwise invisible here. A session whose control
channel is dead raises no prompt, blocks nothing and changes no status — it sits
at an empty composer with a healthy status line and is, through every other
field, an ordinary live session. Measured: 37 of 67 sessions came back from a
fleet-wide recovery in that state, and the only way to find them was to read
pane text.

**Absent is not `active`** (§5.7). A runtime with no such channel reports
nothing, and so does a driver that cannot look; `observesControlChannel` in
`/v1/runtimes` is what separates those, and an unreached peer reports it
`assumed` rather than `false`. It never changes `status`: a session nothing
outside can reach is still running and still able to work.

A change here fires `session.state` on the event stream (§4) like any other
material change — which is the point, since nothing else about the session
moves when a channel drops.

`state.controlChannel.reason` — when present — is why a `failed` channel
failed, in the runtime's own words, sourced from its own durable record
rather than from a screen (muster #69): `{"state":"failed","reason":"Remote
Control disconnected — this session was ended or archived from another
device or app (code 4090)"}`. It carries a close code when the runtime put
one in the sentence, but classifies nothing — no field here says whether a
retry helps; that mapping was never measured (#65) and this endpoint does
not guess it. Absent whenever `state` is not `failed`, or is `failed` but no
record, no readable one, or no matching entry can explain why — the same
§5.7 discipline `controlChannel` itself already applies one field up.

`state.controlChannel.bridgeId` — always present on a channel, a string or an
explicit `null` (muster #276) — is the id the runtime's bridge published for the
session when it brought the channel up: the last segment of the link the runtime
itself printed, which is what a web viewer is opened with. It is read from the
runtime's own durable record, never from a screen, and is an opaque token — do not
branch on its shape. `null` is the whole of "none": the channel is off, failed or
not read, the runtime wrote no link, or the record is unavailable to the driver. A
later enable replaces it, and a change in it is a material state change (it fires
`session.state`), because a caller holding the old one holds a stale link. A peer
built before the field decodes to `null`. Readable by any principal holding `read`,
like the rest of the listing.

`state.permissionMode` — when present — is the permission mode the runtime shows
the session to be in (muster #194): one of `default`, `acceptEdits`, `plan`,
`auto`, `bypass`, or `unknown`. It is what makes `BTab` (§3.3, `keys`) usable for
a control that must land on a NAMED mode: press, read this, stop when it matches.
`bypass` is the word `permissionMode` takes at create time, so a session created
with it reads back as it.

**Absent is not `unknown`, and neither is a guess** (§5.7). *Absent* means nothing
was read — the driver does not look (`observesPermissionMode` in `/v1/runtimes`;
an unreached peer reports it `assumed`), a dialog owns the screen, or nothing is
painted under the composer yet — and the client reads again. `unknown` means the
indicator area was read and named no mode this build recognises (a reworded
label, a mode not in the list, a hint painted in its place, two modes at once),
and a client cycling toward a target **stops** on it: it cannot know which press
lands where. The value is a closed set with a strict decoder, like `status`; it
carries no conversation and no screen text. A change fires `session.state` on the
event stream (§4). It never changes `status`.

It is read from the runtime's own chrome — the row under the composer's closing
fence — never from the transcript, so a session whose scrollback mentions `plan
mode on` does not read as being in plan mode. The machine-local session index's
permission-mode class (#148) is deliberately not a source: it has two values
(`bypass` or `prompting`), is written once at launch, and so can neither tell the
prompting modes apart nor follow a press of `BTab`.

`state.warnings` — when present — lists footer notices the driver read below
the composer's closing fence (muster #230), the same region
`controlChannel` and `permissionMode` are read from: `[{"kind":
"transcript-unreliable","text":"Transcript writes are failing (disk full —
ENOSPC) · recent messages may …"}]`. It exists for the same reason those two
do — a notice there is otherwise invisible through every other field. #229
found that this exact notice can satisfy the shape test the runtime's real
turn-status line uses, and fixed the misread by bounding that scan at the
composer; this is the other half, surfacing what #229 only had to rule out.

**Absent means no notice-shaped line was found — not merely unobserved**
(§5.7). Only one driver reads footers today and it always looks, so there is
no `observes…` flag in `/v1/runtimes` for this field the way there is for
`controlChannel` and `permissionMode`; a second driver reading footers a
different way should add one rather than reuse this absence. `kind` absent on
one entry is a milder, different absence: the notice was found (`text`
carries it) and this driver does not yet recognise its wording — never a
reason to drop the finding. The vocabulary is expected to grow as new notices
are measured; it is not claimed exhaustive by having one member today. A
change fires `session.state` on the event stream (§4) like any other material
change. It never changes `status`.

`state.stalled` — when present — says the session reads `working` and its screen
has not changed for the whole stall bound (muster #391):
`{"since":"…","reason":"…"}`. `since` is when the screen was first seen in the
state it has held since — the start of the quiet stretch, and the verdict was
reached `since` plus the bound; `reason` is prose for humans and logs and is
never branched on. It never changes `status`, which stays `working`: the screen
says so, and the driver does not know more. A running turn cannot hold still —
its spinner animates and its elapsed counter ticks — so stillness for the whole
bound is a runtime that stopped drawing, not one thinking for a long time, and
nothing else (no guess at how long a turn should take, no read of the
transcript) is used.

**Absent is not "healthy"** (§5.7). It means not judged stalled: the status is
not `working`, the service has not yet watched the screen hold still for the
whole bound, or the driver does not look — `observesStall` in `/v1/runtimes`
says which driver does, and an unreached peer reports it `assumed`. The clock is
the service's own and is not carried across a restart of it, so a session that
was hung before one reads stalled only once the bound has elapsed since the
restart. The bound is configured on the service (`FLEET_STALL_AFTER`, default
30 minutes; `0` turns the judgement off), and the field clears the moment the
screen changes or the status leaves `working`. It fires `session.state` (§4)
when it appears, clears, or begins a new quiet stretch — compared by `since`,
not `reason`, so a stall that merely continues is silent. A peer built before
the field sends no key, and a relay passes it through untouched.

`state.usage` — when present — is what the session has spent, as its runtime
reports it (muster #285): `{"input":311,"output":42,"cacheRead":3013,"cacheWrite":60,
"cost":0.76,"source":"observed","asOf":"…","lastTurn":{"at":"…","input":311,…}}`.
The cumulative figures are the **sum over every request**, not the last; `lastTurn`
is the same figures for the most recent completed turn and is the turn-end figure
a subscriber receives — a turn ending is a `session.state` carrying it, there is no
separate event. `reasoning` (a runtime's separately reported tokens, additional to
`output`) and `cost` (what the runtime itself reported) are present only when the
runtime reports them; nothing is priced by this service. **Absent means not yet
known, never zero**: `reportsUsage` in `/v1/runtimes` says whether it can ever
arrive (an unreached peer reports it `assumed`). `credential` (muster #458) is which of
the provider's two accounts the figures were spent on, `"subscription"` (a sign-in whose
plan every session on it shares) or `"api-key"` (billed per token): the one the service
gave the session at its start, by the operator's rule or the documented default, so a
consumer summing cost keeps the two apart. It is named only while the session's newest
request ran on that provider; absent means the service chose none or the request was
elsewhere, never unknown. A peer built before the field
sends no key, and a relay passes the block through untouched. A change in any
figure fires `session.state` (§4); it never changes `status`.

`state.scratch` — when present — is how much the session's scratch directory
holds (muster #419, #431): `{"bytes":49200000000,"areas":{"tmp":11300000000,"home":21400000000,"xdg":16500000000},"asOf":"…","partial":true}`.
`bytes` is the sum of the sizes of the regular files under the session's **whole**
directory, a link not followed, and equals the sum of `areas`; `areas` says where
they sit (the `opencode` driver names `tmp`, `home` and `xdg`, and `other` only when
the directory holds something outside them). It counts apparent size, so it can
overstate the space in use: copy-on-write clones count at full size. `asOf` is when a
driver last walked the directory (at most every few minutes, off the request path),
not when the state was read; `partial` is present and true when the walk stopped at
its bound or could not read part of the tree, and `bytes` and the areas are then lower
bounds. A session that is `dead` still has a figure while its directory exists.
**Absent means no figure, never zero.** A peer built before the field sends no key
(and one built before `areas` sends none of those), and a relay passes the block
through untouched. It is **not** a material change (§4): it moves with every file
written, so a feed that fired on it would storm.

**A parked session** (muster #421) is an `opencode` session whose runtime process the
service stopped on purpose, after the configured bound (`FLEET_OPENCODE_IDLE_STOP`)
with nothing to do, to free its memory. Its `state` is `idle`, `observed`, and its
`evidence` begins `parked:`. It is not `dead` and not `unknown`; the service emits no
`session.closed`, no process-gone event and no gap for it, and the session stays in
every listing. **No read starts it** — a single-session GET, a listing and a
subscription are answered from what was last read, so a client's polling cannot undo
the park. A `send`, an `interrupt` or a `respond` starts the runtime on the same
directory first, and then does what was asked; if it cannot start, the call fails
`unreachable` with `retryable: true`, nothing was sent, and the session remains
parked with the failure in `evidence`. `close` removes it without starting anything.
`GET …/turns` is `409` while it is parked. A service restart (muster #423) does not
start it: while the bound is still set and not lower than the one that parked it, the
session is listed parked after the restart, `evidence` adds that the service restarted
since, `usage` is absent and `conversation` is unconfirmed until the input that starts
it; with no bound, or a lower one, it is started like every other session. A peer built
before this sends none of this and reads as it always did.

`state.usage.context` — when present — is how large the context was at the
session's most recent request (muster #313):
`{"tokens":142000,"window":null,"model":"…","at":"…"}`. It is **not** a sum:
`tokens` is the input, cache-read and cache-write tokens of the newest single
request, where the cumulative figures and `lastTurn` add up every request of the
session or turn — a turn of twenty requests sums to roughly twenty times the
context. `window` is the model's context window **only when the runtime's own
record states it**, and an explicit `null` otherwise; this service keeps no table
of model windows, so a client that wants a fill ratio supplies its own. `at` is
the runtime's own timestamp on that request, not the time of the read. **Absent
means not yet known, never `tokens: 0`**: `reportsContext` in `/v1/runtimes` says
whether it can ever arrive, and is separate from `reportsUsage` (an unreached peer
reports it `assumed`). No threshold or "full" judgement is made here. A change in
`tokens`, `model` or `window` fires `session.state` (§4); `at` alone does not.

`state.prompt.kind` — when present — names what is being asked
(`resume-chooser`, `folder-trust`, `external-imports`, `settings-trust`,
`tool-permission`, `feedback-review`). `bypass-permissions` is deliberately absent from what CLASSIFICATION can produce:
its options are generic and its identifying words sit in the question, which this
service does not read. See the client guide.
It is **advisory and fails to absent**: an unrecognised prompt carries no kind.

`feedback-review` is the runtime's feedback-draft card, and it is the one kind
that is **never auto-answered by anything**: it is not consentable, and a
client puts it in front of a person. Its options are `["review", "send",
"dismiss"]` with nothing highlighted, and `POST .../respond` answers it with the
card's own keys — `choice` 3 is delivered as the key `0`. It refuses `cancel`, an
absent `choice`, `choices`, `text` and an absent `nonce` (required for this kind:
its options are the same on every draft), delivers one key exactly once, and
reports `unknown` when the card is still up afterwards or the key was typed into
the composer instead. Choosing `send` does not send: the runtime asks to confirm
first, on a screen the driver names and leaves to a person. The kind is reported
only while the card's key row hides the composer and the composer's `❯` row is
visible and empty — on a short pane; on a taller one the session is `idle` and
takes messages — see session-abstraction.md §2.7.

The card's other states are not prompts and have no answer through `respond`. Over
a composer the driver cannot read as a whole, the send confirmation, the sending
line, the send error and the runtime's `/feedback` panel (which `review` opens and
which replaces the composer) read `unknown` with the evidence naming the state,
and `send`, `keys`, `discard` and `respond` refuse by name. `keys` still accepts
`Escape` — the way out of the panel, the confirmation, the error and the question
about turning drafts off — and its receipt says what it did; every other key is
refused on the panel, and Enter and the arrows over an unreadable card. Escape on the
card dismisses only the card: the draft stays queued. A `send` of a lone `0`, `1` or
`2` is refused while any of these, or that question, is on screen. `interrupt` is
unchanged.

A client may auto-answer a kind it knows. It must **never** treat an absent
kind as safe: a real prompt in this fleet highlights `No, exit`, so answering
what you cannot read eventually kills the session you meant to rescue. The
service deliberately does not choose for you — deciding what to answer is a
supervisor's judgement, and a session service that made it would have become
one.

`state.prompt.multiSelect` — when true — says the prompt is a multi-select
question: its leading options are checkboxes, painted as `[ ] Label` /
`[✔] Label` in `options` on the one runtime measured, and answering it means
sending a SET (muster issue #176). A `choice` there would only flip one
box, so a driver refuses `choice` on a checkbox row, and refuses accepting the
highlighted row, rather than report a flipped box as an answer. The rows after
the checkboxes (free text, chat) still take `choice` as before. Absent means
not recognised as multi-select (§5.7), never "known single-select".

A multi-select question is recognised whatever its height (muster issue
#219): a question that wraps over many rows, or options with long descriptions,
can make the dialog taller than the window a driver reads, and it is then read from
the dialog's opening rule. `question` is the question's own words — the rule the
runtime draws down the left edge of a question too long for one row is not part of
it.

Answer it with `{"choices": [...], "nonce": "..."}`: exactly the options that
must end up ticked, every other checkbox clear. The driver flips only the boxes
that differ from the screen, reading each flip back, and then moves the dialog
ONE step on — to the next question, or to the dialog's review screen — and
stops. **The answers are not handed over yet**: the review screen is its own
prompt, with its own nonce, answered with `{"choice": 1, "nonce": "..."}` like
any other. So a client answering a one-question multi-select dialog makes two
calls, and the receipt of the first says which screen it reached.

Send `choices` **only** to a prompt reporting `multiSelect: true`. That is not
style: a peer built before `choices` existed never reports `multiSelect`,
would ignore the field, and would read the remaining `{"nonce": ...}` as
"accept the highlighted option". Following the rule makes the field its own
capability check.

A resend of the same `choices` with the same nonce after success is refused:
the question has moved on. After an `unknown` receipt — a flip that did not
read back — the tick state has changed, so the old nonce is refused too; read
the state again and send the same set with the new nonce. The set names the
end state, so boxes already flipped are not flipped twice.

`state.prompt.freeText` — when true — says the question offers the runtime's
free-text row (`Type something`) and that a driver can answer through it
(muster issue #206). The row is still one of `options`, at its own index, so
the numbering a caller already relies on does not move; it is not one of the
agent's choices, and a client drawing the options as a list should draw it as an
input. Answer it with `{"text": "...", "nonce": "..."}`: the driver puts the
highlight on that row, types the text, **reads the row back**, and only then
confirms; the receipt is `submitted` only once the answered question has left the
screen (the next tab, the review screen, or nothing) and never carries the text
back. On a multi-select question `text` rides with `choices` — the boxes to leave
ticked and the free-text row's own content together name the end state — and the
dialog moves ONE step on and stops, exactly as `choices` alone does; `text`
without `choices` there leaves no box ticked. `text` cannot be combined with
`choice` or `cancel`.

Send `text` **only** to a prompt reporting `freeText: true`. That is the same
rule as `choices`, and for the same reason: a peer built before `text` existed
never reports the field, would ignore it, and would read the remaining
`{"nonce": ...}` as "accept the highlighted option". `Response.text` is a
pointer on the wire type so that `{"text": ""}` stays a field when a
peer-relaying driver marshals the body onward — a plain string with `omitempty`
would arrive as `{}` — and an empty or blank text is a `400`: confirming the
runtime's free-text field while it is EMPTY declines the whole dialog, every
question in it, so an empty answer must never reach a driver. `text` is held to
the byte limit `input` is (`maxInputBytes`, muster issue #114) at the same
boundary, for a local and a relayed request alike, and is put through the same
control-byte sanitiser; a leading `!` or `/` is not refused, because the answer
field — unlike the composer — was measured to take both as plain text.

`freeText` fails to absent (§5.7): it is set only on the shapes whose key
sequence was measured — a numbered menu without a preview pane, and on a
multi-select question the row directly after the boxes — and never on a review
screen. It is absent again once the row holds text, because the row is found by
its placeholder: a person's text, or an earlier attempt's, is not typed over. A
`text` sent to such a prompt is refused, and `choice` (`0` accepts what is in the
row) or `cancel` answers it.

`state.prompt.tabs` and `state.prompt.tab` report the tab bar of a tabbed dialog
(muster issue #242). `tabs` is one `{header, state}` per question tab in the
order the bar shows them — `state` is `answered` (painted ☒), `current` (the tab
on screen, even if it was answered earlier) or `pending` (☐) — and `tab` is the
0-based index of the current one. The bar's own `Submit` tab is not a question
and is not listed. They are read from the bar's row of the same capture the
question is, so they cannot disagree with it; they carry the tab *headers* and
nothing of another tab's question or options, which the runtime does not draw
until that tab is current. Both fail to absent (§5.7): on a dialog with fewer than
two questions, on a bar whose current tab cannot be read (the others' states
would then be a guess, since the current tab is painted ☐ like a pending one),
on a tab painted with a glyph that was never measured, and on a peer that
predates them. `tab` is `0` on the first tab — a value, never omitted for being
zero — and is absent while the highlight is on `Submit` (the review screen), where
`tabs` is present and none of them is current. Walking the tabs to read every
question is not offered: it would move the dialog's focus.

A tool-permission dialog reports what is being approved (muster issue #323). The
runtime draws the tool's title and the call — the command, and its description —
above a dashed divider, and the question about it below. `state.prompt.question`
carries all of those rows, from the dialog's opening rule down to the ask, so a
client rendering an "approve this?" card can show what it is approving; the
divider itself is not part of the text. Its `kind` is `tool-permission` whether
the runtime prints the option's apostrophe as ASCII or typographic. The text is
the screen's, as every other `question` is: it is for showing to a person, and
nothing here is a reason to match on it. `nonce` digests it, so two calls that
share a leading word no longer share a nonce. A dialog with no divider reads as
before. `GET …/turns` still carries no `pending` for this dialog; `question` is
the only place the call is reported.

`state.prompt.autoDenyAt` (muster issue #353) is when the runtime will deny the
request by itself, as an RFC 3339 time, for a tool-permission dialog that shows an
unattended-session countdown ("will automatically deny this request in m:ss", or
"will automatically block this request in m:ss" in later runtime versions).
The service computes it from the countdown when the screen is read. The countdown
row is read out of `question` and is not part of `nonce`: it changes every second
while the dialog does not, so a client's `respond` with a nonce read a few seconds
earlier still matches the same dialog. Recognition is anchored on the structure
"automatically <word> this request in <m:ss or h:mm:ss>" and nothing around it —
not on the verb, which has already been reworded once (muster issue #429), so
both wordings and one not yet seen are read. The field name `autoDenyAt` is part
of the API and does not follow the runtime's wording. The field is additive and
fails to absent (§5.7): omitted when no countdown is shown, and on a peer that
predates it — absent is not "never denied".

`state.prompt.optionDetails` carries what an option is drawn with besides its
label (muster issue #324): one `{label, description?, preview?}` per entry of
`options`, in the same order, so a client can show "Path A — Full rewrite, slower
but clean". `description` is the row or rows painted under the option (a wrapped
one is a single string, rows joined by a space); `preview` is the preview pane's
text, rows joined by a newline, and is reported on the **highlighted** option
only, because the runtime draws one pane for whichever option is highlighted.
`options` itself is unchanged. The field is additive and fails to absent (§5.7):
it is omitted when no option has a description or a preview, and on a peer that
predates it, so a client checks for it rather than assuming it. It is not part of
`nonce` — a preview follows the highlight, and an answer to a question that did
not change must not be refused. The question's own prose is not in `turns` until
it is answered, so while a question is open this is the only source of what its
options mean.

A question whose options carry a preview is drawn with the option list and a
box side by side, and `state.prompt` reads the LIST only (muster issue
#204): `options` are the labels — a label that wraps across rows is one string —
and never the box's rows beside them, and `question` is the question, not the
dialog's tab bar and not the prose printed above the dialog. `nonce` follows the
prompt and not the highlight: moving the highlight repaints the box but changes
neither the question nor the options, so it does not change the nonce, while
answering one tab of a tabbed dialog, or moving to another, does. A preview
taller than the capture window no longer hides the dialog: the runtime clamps a
pane to 24 lines, which with the dialog's chrome is more rows than the classifier
scans, and a session blocked on it used to read as `idle`.

`respond` answers such a question in two keys, because on this layout a digit only
MOVES the highlight (and the box) and Enter commits the highlighted row and
advances. The two are never sent together — the runtime keeps one key of several
sent at once, so "2, Enter" recorded the default and not 2 — and the highlight is
read back on the chosen row before Enter is pressed. A receipt says which question
of a tabbed dialog it answered ("on question 1 of 2"), and `submitted` means the
answered prompt is no longer on screen: moving on to the next tab counts. If the
highlight does not arrive the receipt is `unknown`, no confirm key was sent, and
the question is still up. When the highlight sits on no option — on the row below
the list — `respond` refuses, because a digit sent from there may be typed into a
field.

**Send `nonce`.** It is `SessionPrompt.nonce` from the state you read, and it
is the whole of the protection: a caller reads a prompt, shows it to a human,
and answers a minute later — by which time the session may be showing a
DIFFERENT question in the same place, and an answer submitted by index would be
applied to it silently. With the nonce that becomes a refusal.

It is optional only for a human answering something they are looking at right
now. An automated caller that omits it is choosing to answer whatever happens
to be on screen; a driver that answers unchecked must say so in the receipt.

Answers a prompt the session is blocked on. Refused as an ordinary 200 when the
session is not at a prompt — a keypress delivered to a session that is not
asking anything is consumed by whatever it is doing.

This is not a flag on `input`, because `input` must guarantee it never produces
a keystroke: a message containing `C-c` must not interrupt the session
receiving it (§3 of the abstraction).

```
POST /v1/machines/{machine}/sessions/{id}/keys?expect=<screenDigest>&startedAt=&runtime=
{ "key": "Down" }

→ 200 { "outcome": "submitted" | "refused" | "unknown", "reason": "..." }
→ 400 if `key` is outside the vocabulary below
→ 409 if `expect` does not match the screen now, or none was supplied
→ 501 if the driver cannot deliver a key event (§4.3 `deliversRawKeys: false`)
```

Delivers ONE raw key event to a session's screen. It exists for the full-screen
dialogs a driver does not recognise — navigated with arrow keys, confirmed with
a bare Enter — which `respond` cannot answer and `input` must never learn to.
It also carries one key that is not a dialog key at all: `BTab`, which cycles the
runtime's permission mode from an idle composer (below, and muster #188).

**It is not a flag on `respond`.** `respond` refuses whenever the driver sees no
prompt, and that refusal is the whole of its safety: a keypress delivered to a
session that is not asking anything is consumed invisibly by whatever it is
doing. The screens this route exists for are exactly the unrecognised ones, so
folding it in would mean relaxing that check for the case it was written to
exclude. This route pays for its own safety instead.

**`expect` is `state.screenDigest`, and it replaces the nonce.** There is no
`prompt` on an unrecognised screen and therefore no `SessionPrompt.nonce`, so
the caller quotes back a fingerprint of the screen it read — the same discipline
`discard` uses with `composerDigest` and `DELETE` with `startedAt`. A screen
that has moved on is `409`: well-formed request, stale belief. It is
**required**; a caller that has not read the screen has no business pressing
Enter on it.

`state.screenDigest` is new and is published by any driver that can produce one.
It is a fingerprint and never the text: the pane holds a conversation, and a
read that returned it would make every listing a transcript leak. It is not
comparable across drivers or across restarts — quote it back, never compute one.

**Vocabulary, closed:** `Up` `Down` `Left` `Right` `Enter` `Escape` — move,
accept, dismiss — and `BTab` (Shift+Tab, spelled as the multiplexer spells it),
admitted by ruling rather than by that argument; see the next section. Anything
else is `invalid`, rejected before any driver is consulted, plain `Tab` and
every other spelling of Shift+Tab included: names are matched exactly. Absent by
design: every character key, which is `input`'s job and whose guarantee is that
a message never becomes a keystroke; and every control key — `C-c` is
`interrupt`, `C-u` is `discard` — each of which has corroboration and
confirmation a blind keypress cannot offer. An endpoint accepting arbitrary key
names would quietly become a second, unreviewed way to do everything else here.

**`BTab` changes what the session may do (muster #188).** On an idle
composer the runtime cycles its permission mode on Shift+Tab (default, accept
edits, plan, auto…), and for most of those modes that is the only way to reach
them: a client with no terminal in front of it can move a live session between
modes through this route and no other. Some of those modes let the agent act
unattended with less asking, so a press can **escalate** a session — and which
mode a press lands in is the runtime's own cycle, which this service neither
reads nor chooses.

The decision, recorded so no reader has to infer it: `BTab` is under the
**existing `keys` grant**. There is no grant of its own for changing a mode and
none that separates escalating from de-escalating, so **any principal holding
`keys` can escalate any session it can reach** — granting `keys` is granting
that. A deployment that wants the arrow keys without the escalation cannot have
it from this version; that is a change to the grants table (§5) and is a new
ruling, not a configuration.

What `BTab` promises, and what it does not:

- It is **not a mode setter.** One request is one press. `submitted` means the
  screen changed under the key — not that the mode changed, and not which mode
  the session is now in, and this route never claims to know it. A client that
  wants a named mode reads `state.permissionMode` (above; muster #194) after
  each press and repeats, re-reading `state` for a fresh `screenDigest` between
  presses — and stops on `unknown` or on a mode it did not expect, since the
  runtime's cycle skips modes a build does not offer and a press cannot be counted.
- It is **exempt from one refusal the arrows have**: the arrow keys are refused
  on an idle, empty composer because there they drive the runtime's own
  interface (measured: `Left` opens its agent view); an idle, empty composer is
  the one place `BTab` is meant to be pressed.
- It is **subject to every other refusal**, unchanged — `expect` required and
  current, a composer holding unsent text, a composer taller than the capture
  window, a recognised prompt (answered through `respond`). It waits for the
  session's composer lock like every key but `Escape`.
- A service that predates it answers `400` naming the keys it does deliver;
  across a peer relay the machine that runs the session decides, so a caller
  sees that `400` from the far end.

**One key per request**, and no sequence field. After the first key the screen
is different, so every later key in a batch would be delivered against a digest
describing something that no longer exists — reintroducing exactly what `expect`
prevents. `Down Down Enter` is three requests with a read between each. That is
the honest price of three corroborated keypresses.

**Two refusals hold regardless of the digest**, as ordinary 200 outcomes:

- the composer holds unsent text — `Enter` would submit somebody's half-typed
  message, which is the harm `send` already refuses to cause;
- the session is at a prompt the driver DID recognise — answer it through
  `respond`, which verifies a nonce and can say which option it chose. Falling
  back to a blind arrow key is a downgrade dressed as a capability.
  "Recognised" means exactly what a state read of the same screen publishes:
  this refusal fires if and only if that read would hand the caller a `prompt`
  and its `nonce`, never on a screen the read still reports without one. Two
  endpoints classifying one screen differently leave a caller with no verified
  move — `respond` has no nonce to quote, `keys` refuses — so a driver must
  decide both from one classification (muster #159).

**`submitted` means the screen changed under the key.** A key a dialog swallows
leaves the session exactly as stuck as before, so an unchanged screen is
reported `unknown` with the reason saying so — never `submitted`. A legitimate
no-op (`Down` at the bottom of a list) reports the same way, because from
outside the dialog the two are the same observation and inventing a distinction
would mean claiming to know what the dialog is.

It requires its own **`keys` grant** (§6), not `send`. `respond` shares `send`
on a same-blast-radius argument that does not survive here: `respond` is gated
by a recognised prompt and this deliberately is not, so an operator may permit
one and withhold the other — and, since `BTab` (above), `keys` is also the grant
that can escalate a session. Absent means denied, so no existing principal gains
it by upgrading — which means a fresh deployment cannot press a key until an
operator explicitly grants it, on purpose, not as an oversight (muster
#68). `deliversRawKeys: true` on a runtime is a statement about the DRIVER;
whether any caller may reach it is a separate, orthogonal fact this grant
alone controls, and a capability that reads as present while every caller is
refused looks identical to the endpoint not existing.

**A federated keypress needs two grants, at two different machines, discovered
in the wrong order if you only read the refusal you hit first (#68).** The
machine that will actually run the key needs `keys`; the machine relaying the
request there needs `relay` — the ordinary grant §13 already requires for any
proxied mutation, nothing special to `keys`. A caller debugging the relayed
path who fixes the `keys` refusal first (because that is the grant named in
the first 401) will find the call still refused, now naming `relay` — not a
second bug, the other half of the same requirement.

```
POST   /v1/machines/{machine}/sessions/{id}/interrupt?runtime=   → 202
DELETE /v1/machines/{machine}/sessions/{id}?runtime=              → 202
```

Both are `202 Accepted`: they express intent, and confirmation arrives as a
state change on the event stream. A driver may not be able to promise
synchronous completion, and pretending otherwise would be emulation.

**A close whose runtime cannot be asked still goes ahead (muster #473).** A
session this service started in a process of its own is removed by stopping that
process and dropping the record, and the runtime's own read of the session is
only a confirmation of what the service already knows. When that read fails — the
runtime answers with an error of its own, or does not answer — the close is still
`202`, and the answer carries one more field, `note`, in prose for a person: the
runtime's own confirmation was not available, and why. The field is absent on every
ordinary close and a caller never parses it. Only a runtime that says the session
does not exist makes a close `404`. A runtime whose one server serves every session
keeps its refusal: a failed read of it says nothing about one session's fate. A
bare id (no `?runtime=`) is resolved to the runtime whose own record holds it, even
when that runtime cannot be read — "could not be confirmed against all of them"
remains the answer only when no runtime holds the id and one could not be asked.

## 4. Events

```
GET /v1/events?cursor=<last-seen>&epoch=<last-seen>&session=&cwdPrefix=
Accept: text/event-stream
```

`session` may be repeated to name several sessions. Selectors narrow and
compose with AND. Naming is not sugar over `cwdPrefix`: a substrate may charge
per watched session, in which case a caller that can only describe what it
wants pays for every match (§5.5).

Both selectors apply to every frame that is about one session —
`session.created`, `session.state`, `session.closed`, `session.labels`,
`session.record` — whether that session runs on this machine or the frame was
relayed from a peer. Only
`session.created` carries the session's working directory, so a service
remembers the directory it has seen for each session (from created frames and
from its own listings, and by asking the source when a frame names a session it
has never seen) and applies `cwdPrefix` to the frames that name the session
alone. A frame for a session whose directory the service cannot learn is
delivered, not dropped: an extra frame is recoverable, a silent hole is not
(§7.3). Frames that are not about one session — `source.status`,
`machine.quota`, `machine.account`, `control.resync`, and `session.renamed` —
are never narrowed by either selector.

Server-sent events. Every event carries `cursor`, `epoch`, and the `machine` it
originated from — including events proxied from peers (§13).

Each frame carries the kind twice, deliberately:

```
id: 41
event: session.state
data: {"cursor":41,"epoch":"...","machine":"...","kind":"session.state","payload":{...}}
```

An event relayed from a peer additionally carries `origin`:

```
data: {"cursor":41,"epoch":"<this service>","machine":"<peer>","kind":"session.state",
       "origin":{"cursor":7,"epoch":"<the peer>"},"payload":{...}}
```

`cursor` and `epoch` always belong to the service being talked to, so
resumption is never ambiguous; `origin` preserves the peer's own coordinates so
a caller that later talks to that peer directly can resume there. Proxied
subscriptions ask the peer for `scope=local` (§13.1).

`event:` lets a browser `EventSource` listen by kind; the `kind` property lets
every other client read the stream as framed JSON without parsing SSE; and
`id:` makes a reconnecting browser send `Last-Event-ID` on its own, so
resumption needs no client code. The server honours that header when no
`cursor` parameter is given.

| Event | Payload |
|---|---|
| `session.created` | full session |
| `session.state` | ref + `SessionState` — fired on any **material** change, not only a change of `status` |
| `session.closed` | ref + final state |
| `session.renamed` | `{ "machine", "from", "to", "startedAt"?, "corroboration" }` — a session's **id** changed (session-abstraction.md §3's `rename`); a subscriber filtering by id must re-key on `to` or it silently stops matching a session that is still alive |
| `session.labels` | `{ "ref", "startedAt"?, "labels" }` — a session's labels changed, or a create carried some; `labels` is the **whole** map after the change, never the patch (muster #153). A driver's own `session.created` can be observed before a create's labels are stored and carry `{}`; this event is the guarantee |
| `session.record` | `{ "ref", "startedAt"?, "cursor" }` — a session's **record** grew (muster #444). A reference and the cursor `GET …/record` would answer as `next` at that moment; **never content**, and these are the only keys |
| `source.status` | a machine's reachability changed |
| `machine.quota` | `{ "machine", "blocked": bool, "quota"? }` — this machine's **account** started or stopped refusing work |
| `machine.account` | `{ "machine", "generation" }` — this machine's local **credential material** changed |
| `control.resync` | `{ "reason": "epoch_changed" \| "cursor_expired" \| "feed_gap" }` |

**`session.record` says a record grew, with a cursor and never content** (muster #444;
`docs/adr/444-a-record-that-grew-is-said-with-a-cursor-and-never-content.md`). A client
that holds a cursor from the record route (§3.2) pulls from **its own last cursor** to
read what the event announced; the event's `cursor` is where the record had reached, so
passing it as `since` answers what is recorded *after* the event, which is empty until
the next append. It is opaque and not comparable with another cursor.

- **Coalesced.** At most one event per session per window, the window being the
  driver's own (one pass of the terminal runtime's stream; about 250 ms for `opencode`);
  a client must not rely on a number. A burst of appends is one event.
- **A hint.** The record can grow by something the reader leaves out or has not yet
  settled, and the pull is then an empty page, a normal answer (§2.16). `opencode` sends
  no event whose cursor equals the last it sent for the session.
- **A replaced conversation is an event** with a cursor of the new one: a client's old
  cursor is a conflict against it (§2.16) and it starts again without `since`.
- **Authority.** The feed is read under `read`, so the event is; the content stays under
  `record`. While the operator's `refuseRecordReads` switch is on this machine publishes
  no `session.record` of its own, because the route answers `501 record-off` and pointing
  a client at it would mislead; a frame relayed from a peer is that peer's own statement
  and passes unchanged.
- **Where growth is noticed.** In each driver's own stream, not in the service: the
  terminal runtime on the pass its multiplexer notifications wake (no timer of its own,
  so a record that grows behind a screen that never repaints is reported on the next
  wake), `opencode` off the runtime's bus.

`source.status` exists so a client learns a peer went away as an **event**,
rather than inferring it from data that stopped arriving. Inferring absence
from silence is the failure mode this whole specification is organised against.

**`session.state` fires on any material change**, which is every structured
field a caller branches on: `status`, `confidence`, `waitingOn`,
`composerDigest`, `strandedDelivery`, the prompt (its options, highlight and **nonce**), `quota`,
`lastTurn`, `stalled` (muster #391; by `since`, so a stall that continues is silent), `turns`, `usage` (muster #285; once per completed request, and its `asOf` alone is not a change), `credentialGeneration`. It began firing on `status` alone, and
everything else then moved underneath a silent feed — including the nonce,
whose entire job is to make an answer submitted against a replaced question
refusable. A feed that under-reports does not merely go stale; it manufactures
the failure `respond` was built to refuse.

Two things are deliberately **not** material, and a client should know it.
`evidence` is prose for humans that §2.3 forbids parsing, and the runtime
repaints it continuously — treating it as a change would emit an event per
keystroke while saying nothing anyone may act on. `since` on its own is a
driver re-stamping when it first observed a status, not the session doing
something different. So a mirror's `evidence` is as fresh as the last material
change; re-read the session when you want the current prose.

**`session.renamed` fires more than once for the same rename** (muster
#103). The first is always `"corroboration": "accepted"`, published the
instant `POST …/rename` returns `202` — the same fact this event always
carried, named honestly now as provisional rather than left to be read as a
durability claim it never was. A rename that reverts (§3.3's own measured
case: a `202`, a correct read for roughly half an hour, then a silent revert,
with nothing on the stream saying so for that whole window) used to leave a
subscriber holding a name that had stopped being true with no way to learn it.
It cannot anymore: exactly one further `session.renamed`, for the same `from`
and `to`, always follows — never silently omitted — carrying one of:

- **`"corroborated"`** — a later, independent observation found the new id
  still resolving, with no sign of a revert.
- **`"contested"`** — the new id stopped resolving **and** the old id's
  identity came back, matched by `startedAt` rather than by name alone
  (§5.4) — the exact shape #97 measured.
- **`"unconfirmed"`** — this service cannot say either way: the new id
  stopped resolving without the old id's identity reappearing to corroborate
  a revert (as consistent with an ordinary `DELETE` of the freshly-renamed
  session as with an unattributable revert), or the stream watching for it had
  a gap of its own (a `source.status` degradation, a `control.resync`)
  somewhere in the window. Not a claim the rename held, and not a claim it
  reverted — said plainly rather than by omission.

A client that only ever acts on the first `session.renamed` it sees for a
given `to` gets exactly today's behaviour. One that wants to know whether a
rename actually held waits for the second.

`machine.quota` is the only event whose subject is not a session, and the only
one a scheduler should act on by **not** doing something. It fires once at the
transition, carries the reset time when the runtime printed one, and is
announced to a subscriber that connects while a block is already in force —
joining late must not mean learning nothing.

The alternative, measured: an account hit its weekly limit and 48 autopilot
sessions each discovered it separately, by being dispatched work and stalling.
Every discovery cost a session that had already been sent. There is no earlier
signal available — the runtime prints no warning before it refuses, so the
first refusal is the notice.

`machine.account` is the sibling of `machine.quota` for a different
account-level fact (#12): the local credential material itself changed, so
every session started before that moment is bound to an identity that is no
longer the one in force. It fires once at the transition. Unlike
`machine.quota` it is **not** re-announced to a subscriber that joins after
the fact — every machine has some generation the moment a credential store
exists, so there is no "already in force and worth repeating" case the way a
block has; a joining subscriber instead reads `generation` directly off each
session (`SessionState.CredentialGeneration`).

`generation` is an identity marker, not a health claim: it says which
credential this machine now has, never that any particular session's binding
to it still answers. This layer **reports the transition only** — a rebind is
a supervisor's operation, layered on top, not something this event triggers or
performs.

On `control.resync` the client refetches state and resubscribes. The service
never resumes silently from an arbitrary point (§7.3) — an announced gap is
recoverable, a silent one is not.

`control.resync` carries one of three reasons, and they are three different
statements about whose view is stale:

| `reason` | what happened |
|---|---|
| `epoch_changed` | you hold another instance's cursors |
| `cursor_expired` | your cursor is older than what is retained — which after a restart is every cursor below the one the service restarted at, because the window is not kept — or newer than anything this service has stamped, which it equally cannot supply |
| `feed_gap` | **the sequence is intact and this service stopped watching.** Its subscription to a driver dropped and was re-established, or the service itself was stopped and started again, so changes in between were never stamped at all |

**A restart is a gap, and is announced** (muster #454). The epoch and the cursor
continue across a restart of a service that keeps state (that is what lets a
subscriber that was caught up be resumed, not resynced), but while the service
is down nothing watches any driver. The first driver subscription after the start
takes a fresh baseline that absorbs whatever changed in between: a session that
closed or changed state during the downtime is in no event, then or later. A
service that restored its sequence therefore announces, on its **first
subscription to each machine** in that run, `source.status: ok` for the machine
and a `control.resync` with reason `feed_gap`, stamped in the continuing
sequence; a subscriber that resumes later replays them from the window. It is
made **once per machine per run**, not on every subscription: a driver that drops
and is re-established afterwards is the case above, and a stream the hub itself
stops and starts again is not a restart. A service that started a new sequence
announces nothing extra, because no cursor of the old one survives (`epoch_changed`)
and a subscriber with no cursor holds nothing stale. The cost is one refetch
per subscriber per restart, which is the price of the sequence never being
resumed silently from a point the service was not watching (§7.3).

**A runtime that joins mid-run is announced** (muster #465). A runtime this
machine's operator enabled can be unavailable when the service starts and
register on a later retry (§3.1). The driver subscription the service holds
while somebody watches is opened per registered runtime when it starts, so a
runtime that registers afterwards is subscribed **at that moment**: only that
runtime, under the same filter as the running subscription, without restarting
the others (a restart gives every runtime a fresh baseline that absorbs a
change without stamping it). The sessions the runtime already has are in no
event, so a watcher holds none of them from before; on the first successful
subscription the service says what it says after an outage: `source.status: ok`
for the machine and a `control.resync` with reason `feed_gap`, in the continuing
sequence. If that subscription cannot be opened at once, the machine is
announced `degraded` and the same pair arrives when it is, like any other
subscription that failed. A runtime that registers while nobody watches is
subscribed by the next start with every other, and announces nothing. After the
resync a re-list contains the runtime, and from then on its changes arrive as
events. Registering a runtime a second time does not subscribe it a second time.

`feed_gap` is not a politer `cursor_expired`. One says the caller fell behind;
the other is this service admitting the hole is its own, and telling a caller
its cursor is too old when the cursor is perfectly current sends it hunting a
slowness problem it does not have. The action is the same for all three —
refetch and resubscribe — which is why a client that already handles
`control.resync` needs no new code, only a better log line.

**`source.status` also reports the feed itself.** A driver subscription that
fails or ends is announced `degraded` and retried; when it comes back, `ok`
arrives with a `feed_gap` resync beside it. Both edges, on the transition only.
The first subscription after a restart that kept the sequence says `ok` and
`feed_gap` the same way, without a `degraded` before it, because the outage was
the service's own.
The alternative was measured and is the worst shape available: the pump gave up
after a single failure, and every subscriber then held a stream that was open,
healthy-looking, and permanently empty. A machine that is momentarily empty
reaches that state on an ordinary path — the first driver's control mode has no
unattached form, so with no sessions there is nothing to attach to — which
means a client subscribing to an idle machine was never told about the first
session it started. A subscriber told it is deaf can re-list; one told nothing
cannot tell deaf from quiet.

### 4.1 The same feed as ordinary request/response

```
GET /v1/sessions/watch?since=<cursor>&epoch=<epoch>&wait=<ms>
                      &scope=fleet|local&session=&cwdPrefix=

→ 200 { "cursor": 12931, "epoch": "...", "events": [ {...}, {...} ] }
```

A long poll over the same hub, the same sequence, and the same envelope: each
entry of `events` is exactly what an SSE frame's `data:` line carries. This is a
**transport, not a second event model** — nothing is expressible in one and not
the other, because the moment they diverge there are two answers to one
question. Use it when you want a request you can retry, log and reason about,
and when your client must survive its own restart without a stream-reconnect
state machine.

- Returns as soon as at least one selected event exists, or at `wait` with
  `events: []`. Default `wait` is 25s, capped at 60s, and a shorter
  `Fleet-Deadline-Ms` wins — §3.3's rule that a caller may shorten and never
  extend.
- **`cursor` in the response is what to send as the next `since`**: the last
  event in the batch, or — for an empty batch — exactly what you sent. It is
  deliberately NOT the service's current cursor, which advances for every
  subscriber while your filter selects for you; handing it back would advance
  you past events you never saw.
- `since` omitted means **from now**. Never "from the beginning": the oldest
  entry in the retained window is an arbitrary point, and resuming from an
  arbitrary point is §7.3's silent gap.
- `epoch` omitted alongside a `since` means "the instance I was already talking
  to", the same reading the stream gives a browser's `Last-Event-ID`. If that
  assertion is wrong you get `epoch_changed` rather than a bad resume.
- **A stale cursor is a `200`, with `control.resync` in the batch** — the same
  rule as a refused `input` (§3.3). The request was well formed; what the
  service has to say is domain information to act on, not a fault to retry.
  After a resync the response's `cursor` is the one you sent, unchanged: a
  resync is not a position to resume from, so re-list and take the next cursor
  from the listing's `feed`.
- Requires the `read` grant (§6) and grants nothing further.

**Building a mirror**, in full:

```
1. GET /v1/sessions/watch?wait=0            → cursor C, epoch E   (arms the feed)
2. GET /v1/sessions                         → items, and a feed position ≥ C
3. loop: GET /v1/sessions/watch?since=C&epoch=E
         apply events in order; C := response cursor
4. on control.resync (any reason)           → back to 2
```

Step 1 before step 2 is not decoration. The service watches only while somebody
is subscribed, and the first watch is what makes the sequence live; a listing
taken before it carries no `feed` at all, which is the service telling you the
ordering is wrong rather than handing you a cursor that would skip.

A client that would otherwise poll `GET /v1/sessions` to learn what changed
should build a mirror instead. A conditional list (§3.2) saves the body, not
the question: every unchanged poll is still a request, and every changed one is
a full listing. The watch is the one delta transport this API has, deliberately
the only one, which is why the list has no `since=`.

## 5. Authorization

- Every request carries `Authorization: Bearer <token>`.
- **There is no unauthenticated mode.** A service that can start processes and
  read paths is a remote-execution surface whatever its intent, so there is no
  configuration in which authentication is off — not for loopback, not for
  development.
- Permissions are **per verb, per machine**. `list` and `state` may be granted
  broadly; `create`, `input`, `interrupt`, `close`, `rename`, `discard`, `keys`,
  `label`, `remote-control`, `push-list`, `debug`, `record` and `acting-person` are granted per peer and default to denied. That default is deliberate
  and applies to a fresh deployment as much as an established one — no grant is
  implied by anything else, including a runtime advertising the capability the
  grant gates (§3, `keys`; muster #68).
- **`keys` is the grant that can escalate a session (muster #188).** The
  key vocabulary includes `BTab`, which cycles the runtime's permission mode
  (§3, `keys`), so any principal holding `keys` can move any session it can reach
  into a looser mode. It was ruled to stay under `keys` rather than take a grant
  of its own; a reader deciding whom to grant `keys` is deciding that.
- **`human-relay` is not a verb; it is a statement about the caller** (muster
  #180, #184). A principal holding it is a human-facing relay: what it sends is a
  person's own message, so `route: auto` carries it through the session's live
  lane or else the terminal, unlabelled, arriving as the user's own turn, and
  it may send a leading `/` — including `/rc` and `/remote-control`, which with
  `gateRemoteControlInput` on (§3.3 `remote-control`, muster #272; on unless the
  machine sets it to `false`, muster #275) are otherwise delivered only to a
  `remote-control` holder.
  Nothing else confers this — no header, no `from`, no `relayOfHuman` — and it
  crosses a peer relay only as an assertion the owning machine honours from one
  of its configured peers (or from anyone, on a machine with no principal table:
  §3.3, `route`). It defaults to denied
  like every other grant, and it is not something to hand to an agent: it is the
  grant that lets a message skip the label.
- **`push-list` hands this machine a new machine list** (muster #352,
  `PUT /v1/machine-list`, §3.1.1). It is the one grant whose holder can replace
  this machine's peer table and the machine principals in it, bounded: the list
  must verify against the signer an operator pinned here, its `seq` must be
  strictly higher than the highest accepted, and it cannot change the pins, the
  mode or the path. Serving the list is `read`. Denied by default like every
  grant, not implied by the legacy all-verbs flags, and a machine with no
  principal table refuses every push (nobody can hold it). A signed list cannot
  give it either: a list that names it is refused whole, because who may hand a
  machine its next list is a local decision, like the pinned signer.
- **`debug` reads this service's own process** (muster #450,
  `GET /v1/debug/pprof/…`, §3.1.2). It shows how the process is built and what it
  is busy with, and a timed profile occupies the profiler for up to two minutes
  per request; it changes nothing and returns no session content. Its own grant,
  not a rider on `read`: a supervisor that may watch sessions has not thereby been
  trusted to look inside the service that serves them. Denied by default like
  every grant, not implied by the legacy all-verbs flags (a machine with no
  principal table refuses every request), and a signed machine list cannot give
  it: a list that names it is refused whole, because whether this machine can be
  profiled, and by whom, is a local decision. A grant alone is not enough: the
  machine's operator must also switch the endpoint on, and it is off by default.
- **`record` reads a session's whole record** (muster #441,
  `GET …/{id}/record`, §3.2). It is the widest read this API has: a record carries
  whatever the session did and was told — file bodies and command output once tool
  results are carried, credentials an agent printed, the messages people and other
  sessions sent in. Its own grant, ruled by the owner (2026-11-11), and not a rider
  on `send`: `turns` stays on `send` (muster #258) and reads the agent's own prose
  only, so holding `send` does not reveal a record. Denied by default like every
  grant, and nothing implies it: not `read`, `send` or `relay`, not the legacy
  all-verbs flags, and not the set `muster service install` enrols the local
  service principal with — nobody holds it until an operator writes it down. A
  signed machine list may name it for a peer: an explicit, owner-signed act, and
  how one machine of a fleet is allowed to read another's records. For a peer
  target it does not also require `relay` (muster #81); the machine that owns the
  session applies its own table to the asserted caller, so a relayed read needs
  `record` on the entry machine's principal and on the owner, for the principal the
  entry machine relays as. A grant is not the only lock: the machine's operator may
  refuse the route wholesale (`refuseRecordReads`, §3.2), which on a machine with no
  principal table — where a valid token is the credential — is the only one.
- **`acting-person` is attribution, not a verb** (muster #349). A mutating
  request may carry `Fleet-Acting-Person` (an opaque id) and
  `Fleet-Acting-Issuer` (the id of whatever minted it), both required together,
  at most 128 bytes each, valid UTF-8, no control characters. The claim is
  honoured only when the authenticated principal holds `acting-person`; across a
  relay, only when the relaying machine is a configured peer of the receiver
  **and** holds `acting-person` there (the entering machine having already
  judged its own caller). Otherwise it is **dropped and the drop is recorded**,
  never trusted silently and never refused: the call proceeds as if no person
  had been named. A machine with no principal table drops every claim. A
  honoured claim is recorded as `person` (`<id>@<issuer>`) in the input audit
  and leads the audit actor, `<person> · <principal> via <peer>`; a dropped one
  as `personDropped`. The field is **never an input to authorization** — the
  authenticated principal's verb grants decide what is allowed, whoever is
  named. Denied by default like every grant, and not implied by the legacy
  all-verbs flags.
- **A principal authenticates by token or by signature** (muster #350). One
  configured with a public key (`ed25519:<base64url>`) and no token sends
  `Authorization: Muster-Sig v1 machine=<id>, ts=<unix>, nonce=<n>, sig=<s>` and
  `Fleet-Content-Digest: sha256=<hex>`. The signed text is
  `muster-sig-v1`, then one per line: method, request URI (path and query),
  lowercase hex sha256 of the body, the **receiving** machine's id, the sender's
  id, ts, nonce, and the values (empty when absent) of `Fleet-On-Behalf-Of`,
  `Fleet-Human-Relay`, `Fleet-Remote-Control`, `Fleet-Acting-Person`,
  `Fleet-Acting-Issuer`. A verifier MUST refuse, with `401 unauthorized` and
  without performing the request: a ts more than 60 s from its own clock in
  either direction; a nonce already used by that sender within the window (a
  verifier that cannot remember another nonce MUST fail closed, not forget); a
  body that does not hash to the digest; a signature that does not verify against
  the sender's current key or a retired key within its grace period. An unknown
  sender and a bad signature are indistinguishable to the caller. A verified
  sender is the principal named by `machine`, with that principal's grants; a
  request is judged exactly as one authenticated by token. It does not encrypt
  and does not authenticate responses.
- **A route that must judge the caller before it reads the body** (muster #352,
  `PUT /v1/machine-list` only) verifies a signed request from its headers: the
  signature is over the digest the sender **claims** in `Fleet-Content-Digest`,
  which sits inside the signed text, so the claim is authenticated. The verifier
  then reads the body, bounded, and MUST refuse with `401 unauthorized` a body
  that does not hash to the claimed digest before acting on any byte of it. The
  refusal the previous bullet requires still holds; on this route it happens
  after the grant check rather than before. The claim MUST be `sha256=` and
  exactly 64 lowercase hex characters.
- **Key rotation.** `POST /v1/peer-key {"publicKey": "ed25519:…"}`, signed with
  the sender's **current** key, replaces the key the sender is verified with. The
  replaced key is accepted for at most ten minutes and MUST NOT be able to rotate
  to a different key, so that a retired key is never a way to take a principal
  over. A bearer token MUST NOT be accepted for it. A service with no state
  directory MUST refuse it (`unsupported`): a rotation it would forget at restart
  locks the peer out. A machine MAY have peers of both kinds at once.
- Each caller presents its own credential and holds per-verb grants (§6).
- **`GET /v1/whoami` is the one read exempt from needing the `read` grant
  itself** (§3.1, session-abstraction.md §7.7, muster #106). It reports
  only the presented credential's own grants, never another principal's, so
  the risk `read` gates elsewhere — reading someone else's data — does not
  apply; and gating it on `read` would make it unusable by the principal who
  needs it most, the one holding none.
- When proxying (§13), the relaying service authenticates as **itself** with the
  credential it holds on that peer, and asserts the original principal in
  `Fleet-On-Behalf-Of`. A caller's own credential is not meaningful on another
  machine once credentials are per peer, so authority travels as identity plus
  assertion; the peer trusts the assertion as far as it trusts the relay, and a
  relay never obtains more than it was granted. A peer authorizes the principal who initiated the request. A
  service that substituted its own identity would make every machine a confused
  deputy for every other.
- **A principal can be pinned to this machine (`"scope": "local"`, muster
  #395).** It is a field on the principal, not a grant: a grant adds a
  permission and defaults to denied, so one that removed reach would read
  backwards and would need every existing principal migrated; a field that only
  narrows leaves every principal already configured as it was. `"fleet"` or no
  `scope` is that unrestricted behaviour. For a pinned principal, whatever
  grants it holds:
  - a fleet-scoped read is **answered as `scope=local`** (§3.2) — clamped, not
    refused, because the default is what a client that never set `scope` sends;
  - a request naming a machine other than this one — a `{machine}` path segment
    or `GET /v1/whoami?machine=` — is refused for reads and mutations alike:
    `401 unauthorized`, `reason: "local-only"`. The answer is the same for a
    peer and for a machine id that exists nowhere. `GET`/`PUT /v1/machine-list`,
    which is the fleet's roster, is refused too;
  - `GET /v1/machines` lists the `self` item only and `GET /v1/runtimes` this
    machine's runtimes only; no peer is dialled for it;
  - `GET /v1/whoami` reports `scope: "local"`.
  A reload that pins a principal closes the fleet-scoped streams it had open
  (§4.1) so the client reconnects into the local view; a stream that already
  read locally is left alone. The pin applies to the principal as a caller of
  THIS service and says nothing of what this service does for others.
- **A read that reaches beyond this machine — `scope=fleet`, or a path naming a
  specific peer — needs only the `read` grant, never `relay`.** This is a
  deliberate asymmetry with the proxying rule above, not an oversight: a
  relayed mutation changes state on a machine the caller is not talking to,
  while a relayed read does not, so requiring one grant for both would treat
  reaching and changing as the same act. The symmetric rule was considered and
  not taken — at least one principal this fleet is observed through holds
  `read` without `relay` today, and folding `relay` into the read check would
  have refused that call silently the moment it landed, with no error a caller
  could act on. A third, narrower grant for cross-machine reach alone would be
  the most precise separation and was not rejected on its merits; it costs a
  new grant in the model plus a migration for every existing principal, which
  is not worth buying against a distinction nothing has yet been harmed by
  (muster #81).
- Every remote-originated mutation is logged: actor, verb, target, outcome. Every
  `input` is additionally recorded durably, with a hash of its text, and can be
  read back by a `read` principal (§3.2, *Input audit*; muster #330).
- **A logged mutation says how long it took** (muster #450). The line ends with
  `took=<ms>`: whole milliseconds from the moment the verb was admitted to the
  moment its handler returned. Where a step inside it was measured, one
  `phase.<name>=<ms>` follows per step, in the order they finished; a verb that
  measured none prints none, never a zero. A `create` names `resolve` (choosing
  the driver), `create` (the driver's whole create — for a relayed one, the round
  trip to the peer), `state` (the first state read) and, where the driver reports
  it, `terminal` (starting the session in the terminal multiplexer, which is
  **inside** `create`, so the two overlap and are not meant to be added). A create
  on the `opencode` runtime also names the steps of its own start, each
  **inside** `create` and noted whether or not it succeeded, so a slow one can be
  attributed: `oc.measure` (measuring whether the runtime starts under the
  requested sandbox, only for a create that asks for one), `oc.credential`
  (choosing, and where needed renewing, the sign-in), `oc.dirs` (the session's
  own directories), `oc.sandbox` (planning the sandbox and its launcher, only when
  one was asked for), `oc.config` (writing the session's configuration, sign-in
  and log), `oc.start` (a free port, the launcher, the exec), `oc.ready` (the wait
  for the runtime to answer) and `oc.catalog` (reading its model catalog,
  including the wait for a model that is not listed yet). `oc.start` and
  `oc.ready` come once per start attempt, so a start that took three attempts
  prints three of each, and an `input` that restarts a parked session carries them
  too. The fields
  are appended after `status=`, so a reader that stops there still reads the line;
  a refusal that ran nothing (`outcome=DENIED`) carries neither.

## 6. What this API deliberately lacks

No endpoint exposes version control, worktrees, issues, claims, or work
planning (§1 non-goals). If such an endpoint ever looks necessary, the
supervisor is asking the wrong service, or this service has begun to grow into
a second supervisor.

No endpoint returns a session's screen text, a raw transcript, or content a
session produced, **except two owner-ruled routes, each on its own grant**:
`GET …/{id}/turns` (§3.2, muster #258) returns what the session's own agent
wrote — its assistant turns — and nothing else: no tool output, no inbound
message, no system text; and `GET …/{id}/record` (§3.2, muster #441) returns a
session's record under the `record` grant, in the kinds the runtime's reader
admits (today, text in both directions). None stores a result on a session's
behalf (session-abstraction.md §5.8, muster #82, as narrowed by
`docs/adr/258-assistant-turns-read.md` and
`docs/adr/441-a-sessions-full-record-is-read-under-its-own-grant.md`). The data
class is still one this service declines to carry in general, not a domain it
doesn't understand; the exceptions are bounded by provenance and by grant, not by
sensitivity. A dispatched agent's
answer may still be delivered by the agent, to a reply address the caller
supplied at dispatch, over `input`; the two are complementary — one pushes, one
pulls.
