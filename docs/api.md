# API reference

Every endpoint, at a glance. This is a **reference**, not the specification —
[`spec/api-http.md`](spec/api-http.md) is normative and wins any disagreement.
If you are writing a client and want a walkthrough rather than a lookup table,
read [`client-guide.md`](client-guide.md) first and come back here.

- **Base path:** `/v1`
- **Content type:** `application/json` on every request and response except the
  SSE stream.
- **Addressing:** a session is `(machine, id)`. There is no fleet-wide id.

---

## Conventions that apply everywhere

**Authentication.** `Authorization: Bearer <token>` on **every** route, with no
exemptions and no unauthenticated mode — not on loopback, not in development.
A service with no token configured refuses to start rather than falling back to
open.

**Grants.** With a principal table configured, each caller is a named identity
with its own credential and its own list of grants: `read`, `create`, `send`,
`interrupt`, `close`, `rename`, `discard`, `keys`, `label`, `remote-control`, `relay`.
Every grant defaults to denied. ⚠️ `keys` is the one that can **escalate** a session (it delivers
`BTab`, which cycles the permission mode — see `POST …/keys` below), so grant it
as that, not as "arrow keys". Without a principal table the service runs in
single-token mode and two booleans stand in: mutations against local sessions,
and relaying mutations to a peer.

**Relaying.** A mutation aimed at a machine other than the one you are talking
to requires the `relay` grant on the service you called *and* the verb grant on
the machine that actually performs it. Those are two separate refusals and you
will meet them one at a time.

> A read aimed at a peer — a fleet-scoped listing, or a path naming another
> machine — needs only `read`, on the same principal table, whether the target
> is local or a peer. It does **not** also cost `relay`, unlike a relayed
> mutation. Ruled deliberately, not left as an oversight: a relayed mutation
> changes state on a machine the caller is not talking to, a relayed read does
> not, and requiring the same grant for both would treat reaching and changing
> as one act. The symmetric rule was measured and rejected — at least one
> principal this fleet is observed through holds `read` without `relay`, and
> that call would have broken silently the moment `relay` was required for it
> too. A separate, narrower cross-machine-reach grant would be the most
> precise separation and was not rejected on its merits — it costs a new
> grant plus a migration for every existing principal, which is not worth
> buying against a distinction nothing has yet been harmed by (muster
> #81).

**Deadlines.** `Fleet-Deadline-Ms: <ms>` on any request. A caller may only
shorten a driver's declared deadline, never extend it. A peer that fails
three calls in a row is remembered as down: reads then answer at once with it
`unreachable` (and say since when) instead of waiting out the deadline again,
and it is re-probed in the background (#237; `docs/spec/api-http.md` §3.3).

**Corroboration.** Any session-addressed operation accepts `?startedAt=` — the
value from a prior read. A destructive operation uses it to refuse acting on a
session that has been replaced since you looked.

**Runtime disambiguation.** `?runtime=` on any session-addressed operation, and
`"runtime"` in the create body, picks between several local drivers on one
machine.

**Scope.** `?scope=fleet` (the default) or `?scope=local`. Fleet fans out to
every configured peer, exactly one hop — peers never recurse.

---

## At a glance

| Method | Path | Does | Grant | Relays |
|---|---|---|---|---|
| `GET` | `/v1/health` | Build stamp, uptime, drivers, current event cursor | — ⚠️ | no |
| `GET` | `/v1/machines` | Known machines and whether they answered | — ⚠️ | always |
| `GET` | `/v1/whoami` | What the presented credential may do | none — authentication only | no |
| `GET` | `/v1/runtimes` | Drivers present and the capabilities they declare | — ⚠️ | always |
| `GET` | `/v1/sessions` | List sessions, filtered | — ⚠️ | `scope` |
| `GET` | `/v1/sessions/closed` | Sessions that ended within the retention window | — ⚠️ | `scope` |
| `GET` | `/v1/sessions/watch` | Long-poll the event feed | — ⚠️ | `scope` |
| `GET` | `/v1/events` | Same feed as SSE | — ⚠️ | `scope` |
| `POST` | `/v1/machines/{machine}/sessions` | Start a session | `create` | yes |
| `GET` | `/v1/machines/{machine}/sessions/{id}` | Read one session | — ⚠️ | yes |
| `GET` | `…/{id}/environment` | What environment the process actually got | — ⚠️ | yes |
| `GET` | `…/{id}/turns` | What the session's agent wrote — assistant turns only | `send` | yes |
| `POST` | `…/{id}/input` | Deliver text to the composer | `send` | yes |
| `POST` | `…/{id}/respond` | Answer a prompt the session is blocked on | `send` | yes |
| `POST` | `…/{id}/keys` | Deliver one raw key to the screen (incl. `BTab`: cycles the permission mode) | `keys` ⚠️ | yes |
| `POST` | `…/{id}/interrupt` | The equivalent of Ctrl-C | `interrupt` | yes |
| `POST` | `…/{id}/discard` | Clear unsent composer text without sending it | `discard` | yes |
| `POST` | `…/{id}/rename` | Change the session's id, and the runtime's own title if it keeps one | `rename` | yes |
| `POST` | `…/{id}/labels` | Set or delete caller-supplied labels | `label` | yes |
| `POST` | `…/{id}/remote-control` | Turn a running session's remote control on or off | `remote-control` ⚠️ | yes |
| `DELETE` | `/v1/machines/{machine}/sessions/{id}` | Destroy the session | `close` | yes |

⚠️ = the specification requires `read`; the implementation does not check it yet.

---

## Reads

### `GET /v1/health`

Liveness and identity. Returns `{epoch, cursor, startedAt, build,
maxInputBytes, labels, drivers, counters}`. `labels` is the bounds this machine
enforces on session labels, `{maxKeys, maxKeyBytes, maxValueBytes}`; a service
that omits it predates labels. The `build` is a version-control stamp: an unknown or
locally-modified build never compares equal to anything, so "we disagree" stays
distinguishable from "we are different vintages".

`build` fields: `known`, `revision` (a commit sha), `modified`, `time`, `go`,
and `version`.

**`build.version` is the release the running code descends from** — the output
of `git describe --tags` at the built commit, stamped at link time by
`scripts/deploy.sh` (#161). It is what a client checks to enforce a minimum
supported service version; `revision` cannot do that, because a sha is not
ordered.

| Value | Means |
|---|---|
| `"v0.1.0"` | built exactly at release `v0.1.0` |
| `"v0.1.0-2-g3ce7e27"` | two commits **after** `v0.1.0`, at commit `3ce7e27` |
| `"v0.1.0-2-g3ce7e27-dirty"` | as above, plus uncommitted changes (`modified: true`) |
| `null` | not stamped — a plain `go build`, or no release tag reachable |
| absent | the service predates this field |

Comparing against a floor `vX.Y.Z`: strip a trailing `-dirty`, then a
trailing `-<digits>-g<hex>` group; what remains is the release tag, compared as
semver. A stripped `-N-g<sha>` means "after that release", so it satisfies a
floor equal to its tag. Strip from the end rather than splitting on the first
`-`, so a pre-release tag (`v0.2.0-rc.1-3-gabc1234`) keeps its own hyphen. **`null` and absent are
"cannot verify", never "too old" and never "new enough"** — refuse with a message
saying the service did not report a version, which is a different problem to
solve than a version that is too low. A version never participates in build
equality: two builds at one clean revision are the same code whatever their
stamps say.

### `GET /v1/machines`

`{items: [{machine, self, status, observedAt, build, maxInputBytes, peer}],
sources, complete}`. Always probes peers; there is no `scope` here.

`peer` is this service's own standing on each machine:
`{listsMeBack, grantsToMe, source, observedAt?}` — whether that machine lists
this one back, and what it grants the credential this machine presents there.
Read it before relying on a cross-machine write, instead of learning from a
`403`. `source: "assumed"` with `listsMeBack: null` means nobody could tell
(unreached, stale, an older build, or no per-peer credential) — not "not
listed". `observed` with `grantsToMe: []` is a real negative. The `self` item
reports `listsMeBack: true` and what this machine's own table grants the
credential it presents to peers. `?verify=1` re-probes every peer first.

### `GET /v1/whoami`

`{principal, machine, grants, source, listsYou}` — what the credential you
presented may do. It needs authentication but **no grant**, not even `read`:
a credential holding nothing can still learn that it holds nothing, instead of
finding out one `403` at a time. It reports only your own credential, never
another principal.

`?machine=` defaults to this machine, answered `source: "observed"` from its own
table. Naming a peer does **not** relay: it answers `source: "assumed"` with
`grants: []`, because this service cannot know what a peer grants. Ask that
machine directly. A relayed write needs a grant on both machines, and this
route can confirm only the first.

`?peer=<id>` fills `listsYou`: `true` or `false` for whether `<id>` is in this
machine's peer roster. It is a question only a service asks. A service probing a
peer names itself there, and the answer becomes that peer's `listsMeBack` on
`GET /v1/machines`. `listsYou` is `null` when no `peer` was asked, when the
report is about another machine, or when the credential lacks `read`, because
`read` guards the roster. The key is always present on a build that has it; an
absent key means an older build, never "not listed".

### `GET /v1/runtimes`

`{items: [{machine, runtime, capabilities}], sources, complete}`. Consult this
before relying on a capability — a driver that cannot do something says so here
rather than failing at the call.

`capabilities.deliveryModules` (#185) lists the optional external delivery
modules this machine enabled — `name`, `status` (`starting`, `available`,
`unavailable` or `disabled`), a `reason` when it is not available, the module's
`protocol`, `version`, `platform` and `peerCheck`, and per-lane state counts.
It is **absent** when none is enabled, which is the ordinary state, not a fault.

`capabilities.remoteControl` (#269) — `{ "toggle": true, "off": true }` — is
declared only by a runtime that can turn a **running** session's remote control
on through `POST …/remote-control`; today that is the Claude Code runtime, and the
key is absent for every other. `off: false` would mean it can turn it on but has
no way to turn it off, and `enabled:false` is then refused `unsupported`. Read
this before offering the control. Like every capability it comes with `source`:
an unreached peer reports it `assumed` and absent, which says nobody has
answered, not that the peer cannot.

### `GET /v1/sessions`

Filters: `status`, `agent`, `cwdPrefix`, `label`, and `scope`.

`label=key:value` keeps sessions carrying that exact pair; repeat it and every
pair must match. It is the answer to "is any machine already working on X" —
put X in a label at create and ask for it here, instead of encoding it in the
name. A peer too old to apply the filter shows up in `sources` as `degraded`
with no items, so the list is `complete: false` rather than silently wrong.

Returns `{items, sources, complete, feed?}`.

> **You must read `sources` and `complete`.** A fleet list where one machine did
> not answer is still a `200`. `complete: false` means the list is partial, and
> `sources` says which machine failed you. Treating a partial list as the whole
> fleet is how a session gets declared gone when its machine was merely
> unreachable.

`feed: {cursor, epoch}` appears **only** once something is subscribed to the
feed. Its absence is the service telling you that you are doing the sequence
backwards — see *Events* below.

### `GET /v1/sessions/closed`

Parameters: `since` (RFC 3339; keeps records that closed at or after it) and
`scope`. Returns `{items, sources, complete}` of closed-session records, newest
first (#179).

The answer to "which sessions ran here in the last N days, and when did each
end" — without keeping your own copy of session state. Each machine keeps one
record per ended session for `closedRetentionDays` (config file, default 14),
across restarts. A record carries the live record's metadata as last seen —
`machine`, `id`, `name`, `runtime`, `cwd`, `startedAt`, `conversation` when it
was known — plus:

- `closedBy: "close"` — closed through this service; `closedAt` is exact.
- `closedBy: "absent"` — the session ended on its own and was found missing by
  the next complete read (a live event stream, a listing, or the service's own
  start-up read). The end lies after `lastSeenAt` and no later than `closedAt`;
  read the two together, never `closedAt` alone as the moment it died.
- `closedBy: "exit"` — a driver capable of it captured the session's own
  process exit before removing the session, in the same pass (#235);
  `closedAt` is exact, like `"close"`. The record carries `exit: {status, at,
  screenPath}`: `status` is the process's own exit status, `at` is when it was
  captured, and `screenPath` names a file on THAT machine holding the pane's
  last lines — never the text itself, which is never served over this API.
  `exit` is absent for every other `closedBy`.

A rename is not an end. A peer on a build that predates the route shows up in
`sources` as `degraded`, so a fleet read is `complete: false` rather than
claiming that machine closed nothing. Content is never included.

### `GET /v1/machines/{machine}/sessions/{id}`

One session, in full. See *The session object*.

### `GET /v1/machines/{machine}/sessions/{id}/environment`

What environment variables and `PATH` the session's process actually received —
names only, never values. `{known: false}` is an ordinary `200`, not an error.
This exists because a session that inherits the wrong `PATH` fails in a way
nothing else in the API can explain.

### `GET /v1/machines/{machine}/sessions/{id}/turns`

What the session's own agent wrote — **assistant turns only**, oldest first:
`{"turns": [{"at", "text"}], "next": "<cursor>"}`. This is the one place the API
returns content a session produced, and it is narrow on purpose: tool calls and
results, file contents, the messages a human or another session sent in, system
and hook output and reasoning are never in it, and a turn that cannot be
classified with certainty is left out. It carries no `status` or `result` — a turn
is what the agent said, not the service vouching for it; whether the session
finished is `state`. Not to be confused with `state.turns`, the liveness count.

| Query | |
|---|---|
| `since` | A `next` from an earlier page. Omit it for the most recent `limit` turns. |
| `limit` | 1–100, default 20. Over 100 is refused, not clamped. |
| `startedAt` | The `startedAt` you saw. A recycled id is `409` instead of someone else's words. |

`next` is returned even when `turns` is empty, so a poller always has a place to
resume; call again until it stops moving. A cursor from another conversation (a
`/clear`, a relaunch) is `409` — read without `since` to start again. A session
whose conversation record cannot be identified yet is `404` with `retryable`,
which is not the same as an agent that has said nothing (`200`, `turns: []`).
A turn over 64 KiB is cut and marked `"truncated": true`.

**`pending` — the text above an open prompt (muster #266).** A message that holds
prose and then an open question is written to the record only once the question
is answered, so while the dialog is up `turns` holds neither. When a prompt is
open and its question dialog is on screen, the page also carries
`"pending": {"observedAt", "text", "source": "screen", "nonce", "truncated"?}`:
the agent's own prose drawn directly above the dialog, read off the screen.

- **Same boundary as `turns`, read from a different place.** Only the contiguous
  block of the agent's own text immediately above the dialog's opening rule. A
  block holding a tool call, tool output, an inbound message, a status line or
  anything else not tied to the agent's own writing is left out whole — `pending`
  fails to absent, never to a guess. A screen the classifier cannot read, a
  prompt with no tabbed header (a tool-permission dialog) and an unnumbered or
  preview-pane menu carry none, and that is not an error.
- **`nonce` is the prompt's own** (`state.prompt.nonce`): tie the text to the
  card it explains, and drop it when the nonce changes. It is absent from the
  page — the field is omitted, not `null` — the moment the prompt resolves, and
  the real turn then arrives through `turns`. Replace it with that turn; never
  merge the two.
- **It is the screen's text, not the record's.** One line per screen row, the
  runtime's indentation removed, so a paragraph the pane wrapped is several lines
  and the recorded turn can differ in its line breaks. `observedAt` is when this
  service read the screen — the runtime wrote no timestamp for text it has not
  recorded, which is why it is not called `at`.
- **`truncated: true`** says the block is not the whole of it: its head ran above
  the captured window (what is visible is returned) or it was cut at 64 KiB.
- A peer built before this field simply omits it.

Needs the `send` grant under a principal table (the one `input` and `respond`
use; there is no separate grant) and not `relay` for a peer target. Each read is
audited — who, which session, how many turns, whether a `pending` entry left
(`pending=0|1`), never the text. **The text is
whatever the agent chose to write**: an agent that echoes a secret into its own
prose has put it where this route can return it. Reason it is allowed, and what
it costs: `docs/adr/258-assistant-turns-read.md`.

---

## Writes

### `POST /v1/machines/{machine}/sessions` — create

`Idempotency-Key` header is **required**. Body:

```json
{
  "runtime": "", "cwd": "/abs/path", "agent": "", "model": "", "effort": "",
  "name": "", "prompt": "", "contextRef": "/abs/path", "marker": "",
  "remoteControl": true, "trustCwd": false, "env": {}, "resume": "",
  "conversationId": "", "permissionMode": "", "consents": [], "mcpConfig": [],
  "settings": {}, "labels": {}
}
```

`201` with the session. Five fields — `trustCwd`, `consents`, `permissionMode`,
`mcpConfig`, `settings` — additionally require the `send` grant on top of `create`, because
each one hands the new session authority its creator would otherwise have to
grant interactively.

**`settings`** (#247) is a JSON object passed to the agent CLI at launch as
`--settings '<json>'`, for settings the CLI reads only at boot and from no
environment variable — so `env` cannot carry them. The motivating case is a
bypass-mode session that must accept inbound cross-session messages:
`{"permissionMode": "bypass", "settings": {"crossSessionInbound": "accept"}}`.
Rules:

- **Any key in bypass; an allow-list outside it** (#254). With
  `permissionMode: "bypass"` every key is carried. Without it only
  `crossSessionInbound` is; any other key is `400`, naming the first offender,
  so a session that still asks before acting cannot be widened by accident. The
  list is allow, not deny, because this service holds no opinion about which of
  the CLI's settings widen a session — a new key is refused until someone adds
  it on purpose. It exists so that a client relaunching a default-mode session
  with `create` + `resume` can reproduce the argv the session booted with:
  `{"resume": "<conversation id>", "settings": {"crossSessionInbound": "accept"}}`.
  A non-bypass session with `settings` is still not a bypass session — no
  `--dangerously-skip-permissions` is added.
- **A JSON object, at most 4096 bytes once compacted.** Invalid JSON, an array,
  a scalar, or an oversize value is `400` naming which. `null` is the same as
  absent. Keys are not interpreted: the CLI owns what its settings may switch on.
- **Not for secrets.** The compacted JSON is one argv element, readable from any
  process table on the machine. Launch-time switches only; credentials go in
  `env` or a file named by `mcpConfig`.
- **Needs `send`** on top of `create`, like `permissionMode` — in every mode:
  the grant is by field, not by key.
- A session without `settings` is created exactly as before. A runtime with no CLI to hand it to
  (the opencode driver) answers `unsupported`.
- Relayed to a peer that predates the field, the create is refused
  `unsupported` before anything is started there (`GET /v1/health` on the peer
  reports `supportsLaunchSettings: true` when it carries it). The two
  behaviours are told apart on the same endpoint: `launchSettingsOutsideBypass`
  lists the keys the peer accepts on a non-bypass session (`["crossSessionInbound"]`).
  A peer that carries only #247 lacks the list, and a non-bypass create is
  refused `unsupported` before it is sent there rather than relayed into a `400`.

**Resume carries the launch posture** (#256). A create with `resume: <conversation id>`
that names no `permissionMode` launches with the mode the conversation's previous
session last reported (`state.permissionMode`), and one that names no `settings`
launches with the settings that session was created with. The service holds that
record itself, so a restart tool does not have to remember to resend it, and it
outlives the session: it is kept for the closed-session retention period.

- **Only `bypass` is carried.** It is the one non-ordinary mode a create can ask
  for. A conversation whose last session read back as any other mode (the ordinary
  one, `acceptEdits`, `plan`, `auto`) carries nothing: the new session starts the
  way an absent `permissionMode` always starts. A mode the driver could not read
  (`unknown`, or absent) is silence, not a report — the launch's own mode stands.
- **Settings outside bypass keep only the allow-listed keys.** A bypass launch's
  other keys stop travelling with the mode.
- **An explicit value wins.** `permissionMode: "bypass"` over a conversation that
  ran in the ordinary mode launches in bypass; **`permissionMode: "default"`** asks
  for the ordinary mode over one that ran in bypass. `default` is only that
  request — it needs no `send`, and no driver is ever handed it. Explicit
  `settings` replace the carried ones.
- **The response says what was carried:** `"carried": {"permissionMode": "bypass",
  "settings": {…}}`, each key present only when it was carried and not named by the
  request. The field is absent when nothing was.
- **`send` still gates the widening.** Carrying bypass or settings forward is
  allowed when the caller holds `send`, or when the original launch was made
  through this service by a principal that did. A bypass session this service did
  not launch has no such record, so a caller without `send` resuming it is `401`
  naming the grant and `permissionMode: "default"` as the way to proceed — never a
  silently ordinary session.
- **Consents are not carried.** A bypass launch raises the runtime's acceptance
  screen on a machine that has not accepted it yet; answering it is still the
  `consents` field's job, on every create that needs it.
- **Relayed creates** are carried by the machine that holds the conversation: the
  peer applies the same rule and its response comes back unchanged. `"default"`
  reaches a peer that predates this as an unknown mode, refused before anything is
  started there.
- An `Idempotency-Key` replay returns the session the first call made; its
  `carried` is whatever that first call reported, not a fresh decision.

Replaying a spent `Idempotency-Key` against a session that has since ended is
`409` (`reason: "replay-of-ended-session"`), not `201` — muster #234. The
key stays spent regardless; mint a new one rather than retrying this call.

`labels` is a map of up to 16 caller facts about the session — keys 1–128 bytes
without `:`, values up to 128 bytes. Opaque to the service: it stores them and
filters on them, and never interprets them. Over the bounds is a `400` naming
the limit. Sending them needs only `create`. Relayed to a peer that predates
labels, the create is refused `unsupported` before anything is started there.

**`conversationId`** (#224) is a caller-chosen UUID that asks the runtime to
start a NEW conversation under it, instead of the driver deriving one after
the fact — so the `201` already carries `conversation` (`known: true`,
`source: "captured"`) rather than making the caller poll `GET .../sessions/{id}`
until the runtime's own record shows up. Mutually exclusive with `resume`
(one starts, the other continues) — sending both is `400`. Must be
UUID-shaped, and `400` if it already names a conversation this machine has on
record for the same `cwd`, so two sessions never end up sharing one
transcript. A runtime with no way to launch under a caller-chosen id, or a
peer that predates the field, answers `unsupported` before anything is
started — the same rule `labels` follows one paragraph up. If a later read's
own resolution disagrees with the id that was requested, `conversation` flips
to a named mismatch (`known: false`) rather than silently keeping either
answer.

### `POST …/{id}/input` — send text

```json
{ "text": "…", "submit": true, "resumeIfStranded": false,
  "replaceIfStranded": false, "expect": "<composerDigest>",
  "from": { "agent": "…", "session": "…", "relayOfHuman": false },
  "route": "auto" }   // or "terminal", "inbox", or an enabled module's name
```

Returns `200` with a **delivery receipt** — always `200`, even on refusal.

```json
{ "outcome": "queued", "reason": "…", "delivery": { "route": "terminal" } }
```

**`route`** (optional, #184) chooses the delivery path: `"auto"` (the default —
omitted and `""` mean the same), `"terminal"`, `"inbox"` or — when the machine
has enabled an optional external delivery module (#185) — that module's name.
Anything else is a `400` naming every accepted value, before any driver is
resolved.

- **A live lane is the session's only input path (#257).** A session whose
  `delivery.clientConnected` reads `true` has a live lane: every `/input` is
  delivered by its module or refused with nothing written, and never reaches the
  terminal. `auto` goes to that module for every sender (labelled, except a human
  relay's). `route: "terminal"`, `submit: false`, `resumeIfStranded` and
  `replaceIfStranded` are **`refused`** on such a session, with a `reason` that
  names the lane; send again without them. If the composer holds text that was
  left there, clear it with `discard` (with `expect`) and send again.
  `/discard`, `/keys` and `/respond` are unchanged. On a session with no live
  lane, `auto` goes to the terminal: with the sender label, except from a human
  relay (the message arrives as the user's own turn).
- **`submit` defaults to `true` (#257).** Leave it out and the text is
  submitted; send `"submit": false` only to stage text in the composer, which a
  session with a live lane refuses.
- **`auto` never tries the inbox (#257).** The inbox is used only when you name
  it with `route: "inbox"`.
- **The label is mandatory for everyone but a human relay, on every route.** If
  you send no `from`, the service labels the message with the one fact it holds —
  the principal you authenticated as, and the machine your request entered — so
  an agent's text is never indistinguishable from a person's. A `from` you send
  is kept as you wrote it. This holds for `route: "terminal"` and a module route
  too: they are labelled, not refused.
- **The human-relay fact is never inferred from anything you set.** No header, no
  `from`, no `relayOfHuman` moves your send onto the human-relay path; it is the
  principal's own configured grant. Across a peer relay it is an assertion the
  owning machine honours only from one of its configured peers — or from anyone,
  on a machine with no principal table, where nothing distinguishes a relay from
  any other bearer. A human relay's `auto` crosses the peer as `auto` and the
  machine that owns the session decides.
- **`route: "terminal"`** forces the composer. It is refused on a session with a
  live lane (above). A caller holding the `human-relay` grant is not labelled; any
  other caller is labelled with its principal when it sends no `from` that prints.
- **`route: "inbox"`** insists on the inbox. When the session cannot take it the
  receipt is **`refused`**, `delivery.route` is `inbox`, and the `reason` says
  why and that **nothing was written** — the request is never quietly downgraded
  to the terminal. `route: "inbox"` combined with `submit: false`,
  `resumeIfStranded` or `replaceIfStranded` is a `400`: those name a composer the
  inbox does not have.
- **`route: "<module>"`** (#185) forces one enabled external delivery module.
  `submit: false`, `resumeIfStranded` or `replaceIfStranded` is a `400`, and when
  the session's module lane is not live right now the receipt is **`refused`**,
  says why and that **nothing was written** — never a quiet fall back to the
  terminal.
- **A session is inbox-eligible** only when the machine has an inbox configured
  and an index entry for the session, the entry names its permission-mode class
  (#148), the text can be carried in a peer-message envelope that is guaranteed
  to arrive intact, and the session's transcript can be located to confirm a
  delivery against. `deliversToInbox` on `GET /v1/runtimes` is a statement about
  wiring, not a promise about a send.

**`delivery.route`** names the path that produced the receipt: `inbox`,
`terminal`, or `module` (#185) — in which case `delivery.module` names which. A
module that confirmed the runtime took the message answers **`queued`**, never
`submitted`, and one that wrote it and cannot say whether it landed answers
**`unknown` and is never followed by a second send of the same text on any
path**, for the same 30 minutes the inbox holds. A lane that keeps enqueuing on an idle
session without a turn ever starting (two sends in a row, #264) is treated as not live:
the session's `delivery` field then reads `terminal` with the reason, and the next send is
carried by the built-in path; messages the module already enqueued may still arrive. It is **absent** when the receipt names no path — a refusal made
before any path was chosen (a busy composer, the runtime-syntax guard,
contradictory flags, a live lane refusing a composer shape), or a peer built
before the field. Treat absent as "not stated"; it is never either value.

**Nothing is sent down a second path.** An inbox that has written *any* byte and
cannot confirm the message ends `unknown` and is **never followed by a terminal
send of the same text.** See the `unknown` row below.

`from` (optional) labels the message with who it comes from, so the receiving
session sees `agent · session · machine` instead of an anonymous peer. Leave it
out and — unless you hold the `human-relay` grant, whose messages are never
labelled — the service labels the message for you with the principal you
authenticated as and the machine your request entered (#184): the label is
mandatory for everyone but a human relay, and only its *filling in* is the
service's.

- **`agent` and `session` are your own statement.** The service carries them
  but cannot verify them — under a shared token nothing tells one caller from
  another. Do not treat a label as proof of who sent something.
- **`machine` is not yours to set.** The service stamps the machine where your
  request entered the fleet and ignores any value you send. If it cannot
  establish one across a relay, it leaves the machine out.
- **`relayOfHuman: true` adds one line of text and nothing else.** The line says
  the sender *states* it is relaying an instruction from the human operator. It
  is unverified, it grants nothing, and no permission, policy or routing
  decision reads it.
- On the inbox path the label goes in the envelope's sender-name field, and a
  name the service cannot guarantee intact is dropped — never the message. On
  the terminal path it goes on as the first line of the text, as
  `[from: …]`.
- A `resumeIfStranded` retry must repeat the same `from` as well as the same
  text: on the terminal path the stranded text includes the label line.

| `outcome` | Meaning | What to do |
|---|---|---|
| `submitted` | The agent received it — reachable only when the driver's `confirmsDelivery` capability is `true` (see below) | Done |
| `queued` | Accepted, submission unconfirmed | Done |
| `refused` | The driver actively declined; `reason` says why | Read the reason — this is information, not a fault |
| `unknown` | Sent, outcome unverifiable. On `delivery.route: "terminal"` **the text may be sitting unsent** | On the terminal: retry with `resumeIfStranded: true`. On **`inbox`: do not** — the message may have arrived; see below |
| `delivered` | The receiver's own transcript recorded the message as a peer message — reachable only on `delivery.route: "inbox"` | Done |

**`unknown` on the inbox (#184) means the message may have arrived, and it will
not be sent again.** The inbox has no reply channel, so a clean write only proves
that bytes reached a socket. The service confirms the message from the
receiver's own transcript, the way the terminal path does; when the transcript
does not record it inside the confirmation window (or the write broke part-way),
the outcome is `unknown` and the receiver may be holding it, may not have written
it yet, or may have dropped it. For 30 minutes, or until the message is recorded,
every further send of the **same text from the same sender to the same session**
— a `resumeIfStranded` retry, a forced terminal send, an explicit inbox request —
is answered with the same `unknown` and writes nothing on either path, because
sending it again could deliver it twice. **`resumeIfStranded` is a terminal
operation; it is not the way to retry an inbox `unknown`.** Read the session's
transcript and wait. Different text, or the same text from another sender, is a
different message and is not held.

**Two refusals worth recognising by their `reason` (#180):**

- A `reason` beginning **`composer busy, retryable: `** means another
  delivery, respond or discard held the session's composer until your own
  deadline ran out. Nothing was done; the condition is transient — retry.
  A `respond` takes priority over a send waiting on the same composer.
- Text beginning with **`/`** is a command to the runtime, not a message, and
  is refused — except `/rename`, `/rc` and `/remote-control`, which any caller
  may send, and any command from a caller holding the `human-relay` grant.
  Like the `!` refusal, it is judged after leading invisible characters are
  skipped.

**`submitted` is not one of the outcomes `input` can return today.** Every driver
in this fleet reports `confirmsDelivery: false` on `/v1/runtimes` — none can
distinguish "the agent received it" from "the runtime accepted it" — so a
confirmed delivery through `input`, including a confirmed `resumeIfStranded`
retry, reports `queued` instead. `submitted` stays real: it is what `respond`
and `keys()` report for a keystroke that visibly changed the screen, which is
evidence `input`'s own confirmation (the composer emptying) does not have. A
client that keys on `submitted` from `input`, or loops until it sees one,
waits forever — check `confirmsDelivery` before writing that client rule, not
this table alone.

`resumeIfStranded` completes a delivery the service itself attempted and lost
confirmation of. It only ever resubmits text the service's own record says it
placed there — never text a human typed.

**The draft rule (#180).** The service never clears or submits text sitting in
a session's composer unless **(a)** its own record proves the text is its own
stranded delivery, or **(b)** the call carries `expect` — the composer's
current digest, as a session read reports it in `composerDigest` — proving the
caller saw exactly what it is asking to have cleared. Otherwise the call is
`refused` and the text stays: it may be a person's draft. The flags alone are
a wish, never proof.

- `resumeIfStranded` finishes the service's own stranded delivery when its
  record still matches the composer. When the live record has lapsed (it is
  kept 30 minutes) the service keeps a longer-lived record of the text it
  placed, and if the composer still holds exactly that text, the call clears
  it and delivers this call's text — muster #135's case, still covered.
- `replaceIfStranded` clears the service's own stranded delivery and
  delivers this call's text instead. For any composer the service cannot
  prove is its own, it needs `expect`.
- An `expect` that does not match the composer as it is now refuses — it
  changed after it was read, possibly because a person typed into it.

A refusal under this rule names the composer's current digest and both ways
forward: send again with `replaceIfStranded` and that `expect`, or `discard`
it. `expect` has no effect without one of the two flags, and it never
permits anything by itself — it is compared with the composer at the moment
of acting.

> A `POST` to `/input` is not the same thing as an instruction delivered. If you
> write one client rule from this document, make it: read the outcome.

### `POST …/{id}/respond` — answer a blocked session

```json
{ "choice": 2, "cancel": false, "nonce": "…" }
```

`choice` is **1-based**, matching the order of `prompt.options`; `0` accepts
whatever is currently highlighted. Returns the same delivery receipt as `input`.

The `nonce` comes from `state.prompt.nonce` on the session you just read, and
changes whenever the prompt changes. Send it always. If it no longer matches,
the driver refuses rather than applying your answer by index to a question that
has changed underneath you — which is the entire reason it exists.

On a numbered menu the choice is delivered as its digit. On a menu whose options
carry no numbers (the runtime paints the folder-trust question that way), a digit
is inert, so the highlight is moved with arrow keys, re-read, and confirmed only
once it sits on the chosen row. If the highlight does not arrive, the receipt is
`unknown`, nothing is confirmed, and the prompt stays up (the highlight may have
moved).

A question drawn beside a preview pane is answered in two keys, because a digit
there only moves the highlight: the driver presses the digit, reads that the
highlight sits on the chosen row, and only then presses Enter, each key in its own
call (the runtime keeps one key of several sent together). `options` are the list's
labels alone — the pane is cut off and a wrapped label is one string — and the
receipt names which question of a tabbed dialog it answered. If the highlight does
not arrive the receipt is `unknown` and nothing is confirmed.

**Tabbed dialogs** (two or more questions) report their tab bar, so a client can
draw "question 2 of 4" and what the others are called:

```json
"tabs": [ {"header": "Layout",  "state": "answered"},
          {"header": "Theme",   "state": "current"},
          {"header": "Density", "state": "pending"} ],
"tab": 1
```

`tabs` lists the question tabs in bar order (the runtime's own `Submit` tab is not
a question and is left out); `state` is `answered`, `current` or `pending`; `tab`
is the 0-based index of the current one — `0` is a value, not an absence. This is
only the bar: another tab's question and options are not drawn until it is
current, so they are not here. Both fields fail to absent — on a single-question
dialog, on a bar whose highlighted tab cannot be read, and on a peer built before
they existed — so check for `tabs` before drawing progress. On the review screen
(the highlight on `Submit`) `tabs` is present and `tab` is not. Answering is
unchanged: `respond` answers the current tab, with the nonce the same read gave.

**Multi-select questions** report `prompt.multiSelect: true`; their leading
options are checkboxes, painted `[ ] Label` / `[✔] Label`. Answer them with a set
instead of `choice`:

```json
{ "choices": [1, 3], "nonce": "…" }
```

The driver ticks exactly those boxes and clears the rest, flipping only the
ones that differ and reading each flip back, then moves the dialog one step on
— to the next question, or to its review screen — and stops there. The answers
are handed over only when you answer that review screen (`Ready to submit your
answers?`) with `{"choice": 1, "nonce": "…"}` and its own nonce. `choice` on a
checkbox row, and accepting the highlighted row, are refused on a multi-select
question: each would flip one box and answer nothing. `choices` is a `400` when
empty, repeated, below 1, or combined with `choice` or `cancel`. Send it only to
a prompt reporting `multiSelect` — an older peer never reports the field, and
would read the rest of the body as "accept the highlighted option".

**Answering in your own words.** Every question an agent asks also offers a
free-text row, the runtime's `Type something`. A question that offers it
reports `prompt.freeText: true`, and `respond` takes the text to type into it:

```json
{ "text": "a flat white, oat milk", "nonce": "…" }
```

The driver puts the highlight on that row, types `text` into it, reads the row
back to prove the text arrived, and only then confirms. `submitted` means the
answered question has left the screen — the next question of a tabbed dialog, its
review screen, or nothing. The receipt reports how many bytes were typed and
never the text itself. It works on all three shapes:

- **A single-select question** (alone, or one tab of a multi-question dialog):
  the text is the whole answer, and confirming it moves the dialog on exactly as
  a `choice` does — after the last question that is the review screen, which is
  answered with `{"choice": 1, "nonce": "…"}` like any other.
- **A multi-select question**: send `text` together with `choices` to tick those
  boxes *and* fill the free-text row (`{"choices": [1, 3], "text": "durian",
  "nonce": "…"}`), or `text` alone for an answer with no box ticked — the set
  names the end state, so a box that was ticked is cleared. The dialog moves one
  step on and stops there, as `choices` does. Typing ticks the free-text row's
  own box; that is what hands the text over.
- **`text` cannot be combined with `choice` or `cancel`**; that, an empty or
  blank `text`, and a `text` over the same byte limit `input` is held to
  (`maxInputBytes`, 1024 by default) are each a `400`, before any driver sees
  the body.

Rules that follow from how the row behaves on the one runtime measured:

- **Send `text` only to a prompt reporting `freeText: true`.** A peer built
  before `text` existed never reports the field, would ignore `text`, and would
  read the rest of the body as "accept the highlighted option". Following the
  rule makes the field its own capability check. It is reported only for the
  shapes measured: not on an unnumbered menu, beside a preview pane, on a review
  screen, or on a list of checkboxes the driver does not recognise as
  multi-select.
- **An empty answer is never sent.** Confirming the free-text field while it is
  empty does not answer with nothing: the runtime treats it as declining the
  *whole* dialog, every question in it. That is why an empty or blank `text` is a
  `400`, why text that is empty once control characters and surrounding
  whitespace are removed is refused, and why Enter is only ever pressed after the
  row has been read back showing the text. A paste that did not land — or landed
  altered — is `unknown`, with nothing confirmed.
- **Once a row holds text it is no longer offered.** The row can only be found by
  its placeholder, so after text is typed — by a person, or by an earlier attempt
  that ended `unknown` — `freeText` is absent and `text` is refused rather than
  typed over. Answer that prompt with `choice` (`0` accepts the highlighted row,
  which submits the text that is there) or `cancel`.
- **Escaping.** The text goes through the same sanitiser `input` uses — control
  bytes and paste-bracket escapes are dropped — and surrounding whitespace is
  trimmed; newlines inside the text are kept, and continue on rows of the field.
  Unlike `input`, a leading `!` or `/` is *not* refused: the composer reads those
  as its own syntax, and the answer field was measured not to (both arrive as
  plain text — `/var/log/app` is an ordinary answer).
- **Long text.** The field grows with the text and the dialog is read from the
  bottom of the screen, so an answer that wraps over many rows can push the
  question out of what the driver reads. The read-back then fails, nothing is
  confirmed, and the field may still hold the text; `keys` can reach a dialog
  `respond` cannot classify. Keep answers short.

`respond` refuses when it sees no prompt it recognises. That refusal is its
safety property, and it is why raw keys are a separate endpoint rather than a
flag here.

### `POST …/{id}/keys` — one raw key

```json
{ "key": "Down" }
```

One of `Up`, `Down`, `Left`, `Right`, `Enter`, `Escape`, `BTab` — anything else
is a `400` that names the valid set. Requires `?expect=<digest>` from a prior read; a
stale digest is a `409`. Which digest depends on what the composer holds **right
now**, decided before the check runs: `?expect=<composerDigest>` when the
composer holds unsent text (the same value a read publishes as
`state.composerDigest`, and the same one `discard` corroborates against) or
`?expect=<screenDigest>` when it is empty (`state.screenDigest`). Sending the
wrong one back is indistinguishable from a genuine race — both fail the same
`409` — so read `state` immediately beforehand and use whichever digest it
reports for the composer's current state, not whichever one you last happened to
have. For full-screen dialogs `respond` cannot classify. Its own grant,
deliberately not folded into `send`.

**`BTab` — Shift+Tab — changes what the session may do, and any principal
holding `keys` can send it.** On an idle composer the runtime uses it to cycle
its permission mode (default, accept edits, plan, auto…), which is the only way
to reach most of them without a person at the terminal (muster #188). Some
of those modes let the agent act **unattended** with less asking, so pressing
`BTab` can *escalate* a session, and which mode a press lands in is the
runtime's own cycle — this service neither reads nor chooses it. There is
deliberately **no separate grant** for it, and none that tells escalating from
de-escalating: `keys` is the whole permission. Grant `keys` only to a principal
you would trust to loosen any session it can reach — over a peer relay that is
the principal holding `keys` on the machine that runs the session.

What to expect from a `BTab`, since it is not a dialog key:

- **It is not a mode setter.** One request is one press. `submitted` means the
  screen changed under the key — not that the mode changed, and not which mode
  the session is in now. `unknown` (the receipt outcome) means the screen did not
  change (the press was swallowed, or there was nothing to cycle). To reach a
  named mode, read `state.permissionMode` after each press and repeat, re-reading
  `state` for a fresh digest between presses; stop on a `permissionMode` of
  `unknown`, and never count presses — the runtime's cycle skips modes a build
  does not offer (#194).
- **It is not refused on an idle, empty composer** — unlike the four arrow keys,
  which are, because there they drive the runtime's own interface. An idle
  composer is exactly where `BTab` is for.
- **Every other refusal applies to it**, unchanged: a composer holding unsent
  text (the refusal is by screen state, not by which key could do harm there),
  a prompt `respond` can answer, a composer taller than the capture window, a
  missing or stale `expect`. A prompt is answered through `respond`, so a mode
  is never reached by pressing `BTab` at a prompt this service recognises.
- **An older service answers `400`** for `BTab` and names the keys it does
  deliver — across a peer relay, the machine that runs the session decides.

### `POST …/{id}/discard`

Clears unsent composer text without submitting it. Requires
`?expect=<composerDigest>` when the composer is non-empty. `202`.

If a prior call already came back `409` naming this exact residue
proven-futile, retry with `&force=true` on the SAME `?expect=` — this reaches
for a stronger clear mechanism than the ordinary pass. `force` never relaxes
`expect`; it has no effect before a prior call has actually proven this
residue futile. `DELETE …/{id}` still works but should not be needed for a
stuck composer alone (muster#136).

A `409` saying the composer is **taller than the capture window or reaches
above the visible pane** is different: retrying with any `expect` or `force`
gets the same refusal, and `input`/`keys` refuse the same state. Stop retrying
and have a person read or clear the composer at the pane (muster#149,
#169).

### `POST …/{id}/rename`

```json
{ "name": "new-id" }
```

Changes the session's **id**, not a display label. Announced as
`session.renamed` so subscribers can re-key. `202`, with the id half and the
title half reported separately:

```json
{
  "accepted": true,
  "title": { "status": "synced", "evidence": "…", "receipt": { "outcome": "queued" } }
}
```

The id half (`accepted`) is unconditional and synchronous. `title` is a
SEPARATE fact (muster#222): on a runtime that keeps its own idea of a
title apart from the id — the transcript a title-reconciling client would
otherwise trust more than this API — whether that title was brought to the
new name too, confirmed from the runtime's own record, never the screen.
Four states, and a caller must treat them differently:

- `"synced"` — done; nothing further to do.
- `"pending"` — sent, not yet confirmed either way within this call's own
  time budget. Retry by **renaming to the same name again** — that
  re-attempts only the title half, since the id has nothing left to change.
- `"failed"` — did not happen and will not without acting again: most often
  a busy or stranded composer refused the delivery under the same rules
  `/input` already applies (nothing a person typed is ever overwritten —
  see `receipt.reason`). Retry the same way as `pending`.
- `"not_applicable"` — this session's runtime keeps no title of its own
  apart from its id; there is nothing for this half to do.

`title` **absent** (the key missing entirely) means nothing is stated about
the runtime's title at all — a peer built before this field existed. Never
read absence as `"not_applicable"`; a consumer must tell the two apart
(§5.7).

`receipt` is the title-sync delivery's own `outcome`/`reason`, in the exact
vocabulary an ordinary `/input` call already returns — present whenever a
delivery was actually attempted, absent when nothing was ever sent (an
unusable name, or no session found to address).

### `POST …/{id}/labels`

```json
{ "labels": { "issue": "153", "old": null } }
```

Merges: a string sets a key, `null` deletes it, anything not named stays. The
bounds apply to the result. Send `?startedAt=` so a recycled id is refused
`409` instead of silently labelled. Needs the `label` grant — not `rename`.
`200` with the whole session. Announced as `session.labels` with the complete
map. Labels follow a rename and are forgotten when the session closes.

### `POST …/{id}/remote-control`

```json
{ "enabled": true }
```

Turns a **running** session's remote control on or off. `enabled` is required — a
missing one is `400`, because a default here would be a default for publishing a
session off the machine. `202`, `{"accepted": true}`: intent only. The
confirmation is the session's `state.controlChannel` changing, delivered as a
`session.state` event.

- **`enabled: true` on a `failed` channel is the reconnect** — there is no
  separate verb. ⚠️ That path (disconnect, then enable again) joins two steps that
  were each measured live, but a `failed` channel could not be induced to measure
  the whole of it.
- **Already in the requested state is a no-op that answers `202`.** This is not
  politeness: on the runtime measured, the command that turns remote control on
  *opens a disconnect dialog* when it is already on, so the driver reads the
  channel first and never sends it blind.
- **`409` (retryable) when it cannot act safely:** the channel's state could not
  be read, the session is not idle, a prompt is open, or the composer holds
  unsent text. Nothing was changed; read the session and try again. A dialog the
  call opened is always dismissed before it returns.
- **`501` `unsupported`** from a runtime that does not declare
  `capabilities.remoteControl`, and for `enabled:false` where `off` is `false`.
  A peer built before the route answers the same way, never `404`.
- **Grant: `remote-control`, its own, denied by default.** Turning it on makes
  the session drivable from off the machine — an exposure change, not an input —
  so neither `send` nor `keys` implies it, and the refusal names it. A call to a
  peer additionally needs `relay` here, and the `remote-control` grant on the
  peer.
- ⚠️ **`/input` still carries the older workaround.** The runtime's own slash
  command (`/rc`, `/remote-control`) is deliverable through `input` for any caller
  holding `send` (see above), and that path is *not* gated by the `remote-control`
  grant. The grant governs this verb; it does not close that door.

### `POST …/{id}/interrupt` and `DELETE …/{id}`

Both express intent and return `202`. Confirmation arrives on the event stream,
not in the response.

---

## The session object

```json
{
  "machine": "machine-b", "id": "s42", "name": "…",
  "runtime": "tmux", "cwd": "/abs/path", "agent": "…", "model": "…",
  "startedAt": "…", "attach": {…}, "conversation": {…}, "resumeOutcome": {…},
  "marker": "…",
  "labels": { "issue": "153" },
  "delivery": { "lane": "terminal", "clientConnected": false, "evidence": "…", "since": "…" },
  "state": {
    "status": "waiting_input",
    "confidence": "observed",
    "evidence": "prose — display it, never parse it",
    "since": "…",
    "prompt": { "question": "…", "options": ["…"], "selected": 1, "kind": "tool-permission", "nonce": "…" },
    "waitingOn": "prompt",
    "composerDigest": "…", "screenDigest": "…",
    "quota": { "since": "…", "resetHint": "…" },
    "lastTurn": { "outcome": "failed", "reason": "…", "retryable": true },
    "controlChannel": { "state": "active", "reason": "" },
    "permissionMode": "acceptEdits"
  }
}
```

**`controlChannel`** — what the runtime says about its own remote-control
channel: `active`, `connecting`, `reconnecting`, `failed`, or `off` (#269). `off`
means the driver has **positive evidence** there is no remote control — the
session was launched without it, or the runtime's own record shows a disconnect
after the last enable — and is never inferred from a missing label. **Absent
still means "not read"**, never connected and never off: it is what a session
this service did not start, or one whose record could not be matched, reports.
A session created with `remoteControl: false` reads `off`. One caveat the claim
carries: a user's own runtime settings can start remote control without the
launch flag, in which case the session reads `off` only until the runtime writes
its enable entry. On a runtime that draws the label of a healthy channel somewhere the footer
reader does not look, `failed` is also read from the runtime's own record
(#270): the runtime's disconnection notice, when it is the newest channel entry
and no recovery has followed within five minutes, with the notice as `reason`.
Inside those five minutes nothing is reported. `connecting` and `reconnecting`
have no record entry and are reported only when the footer shows them. A change fires `session.state`.

**`status`** — `starting`, `working`, `waiting_input`, `idle`, `quota_blocked`,
`dead`, `unknown`. A closed set with a strict decoder: an unrecognised value is
a decode error, never a silent default.

**`confidence`** — `observed` (read from a structured API) or `inferred`
(deduced from a screen). It survives a relay rather than being flattened, so a
proxied answer never looks more certain than the original.

**`permissionMode`** (#194) — the permission mode the runtime shows the session
to be in: `default`, `acceptEdits`, `plan`, `auto`, `bypass` (the same word
`create` takes) or `unknown`. A closed set with a strict decoder. **Absent means
nothing was read** — this driver does not look (`observesPermissionMode` in
`/v1/runtimes`) or a dialog owns the screen — so read again; **`unknown` means the
mode indicator was read and named no mode this build recognises** — stop, do not
guess. It is what lets a client press `BTab` until it sees the mode it wants
(see `POST …/keys` below). It carries no screen text.

**`waitingOn`** — `prompt` (a dialog is attached) or `unsent-input` (the
composer holds text nobody submitted; do not send to it).

**`delivery`** (#185) — which delivery lane the session's input takes when an
optional external module is enabled on the machine that owns it: `lane` is the
module's name while its lane is live and `"terminal"` otherwise, with
`clientConnected` and prose `evidence`. **Absent means "not configured or not
probed", never "terminal"** — a machine with no module enabled, a session this
service did not launch itself, or a peer on an older build has no `delivery` key.

**`labels`** — always present, `{}` when there are none. A session read
through a peer on an older build has no `labels` key at all.

**Absence is not failure.** A `null` is the service saying nobody looked, which
is a different fact from a negative answer. `conversation: null` means nothing
resolved it, not that there is no conversation. This distinction is the
invariant the rest of the design serves.

---

## Events

Two transports, one feed.

- **`GET /v1/events`** — SSE. Frames are `id: <cursor>`, `event: <kind>`,
  `data: <envelope>`. Resume with `?cursor=&epoch=`, or the `Last-Event-ID`
  header on a browser reconnect.
- **`GET /v1/sessions/watch`** — long poll, for clients that would rather retry
  a request than hold a stream. `?since=&epoch=&wait=` (default 25s, max 60s).
  Returns a batch, not one event per poll.

Filters on both: `session` (repeatable — name the ones you care about),
`cwdPrefix`, `scope`.

**Kinds:** `session.created`, `session.state`, `session.closed`,
`session.renamed`, `session.labels`, `source.status`, `machine.quota`, `machine.account`,
`control.resync`.

**Cursor and epoch.** The epoch identifies a service *instance*; a restart gets
a new one. The cursor is monotonic within an epoch. A relayed event keeps the
originating machine's own cursor and epoch in `origin`, and takes the relaying
service's cursor for local ordering — so resumption is never ambiguous about
whose sequence you are holding.

**Resync** arrives in-band as a `control.resync` event, never as an error, with
one of three reasons: `epoch_changed` (you hold another instance's cursors),
`cursor_expired` (older than retained), `feed_gap` (the sequence is intact but
*this* service's own subscription dropped and reconnected — a different party is
at fault). All three prescribe the same recovery.

**Build a mirror in this order.** Getting it wrong is the most common client
bug:

1. `GET /v1/sessions/watch?wait=0` — **arm the feed first.** The service only
   advances its sequence while something is subscribed.
2. `GET /v1/sessions` — take the snapshot *and* the `feed{cursor, epoch}`.
3. Loop `watch?since=&epoch=` (or hold the SSE stream).
4. On any resync, go back to step 2.

Listing before ever watching returns no `feed` at all. That absence is the
answer, not a degraded response.

---

## Errors

```json
{ "error": { "kind": "not_found", "message": "…", "machine": "machine-b", "retryable": false } }
```

| `kind` | HTTP | Meaning |
|---|---|---|
| `invalid` | 400 | Malformed request |
| `unauthorized` | 401 | Caller not permitted |
| `not_found` | 404 | The machine answered; there is no such session |
| `conflict` | 409 | Well-formed, but your belief is stale |
| `unsupported` | 501 | The driver cannot do this |
| `unreachable` | 504 | The machine did not answer at all |

`not_found` and `unreachable` must never be conflated. One is an answer; the
other is the absence of one.

A `conflict` may carry `reason` for finer classification than `kind` alone
gives — e.g. `create`'s own `"replay-of-ended-session"` — plus whatever fields
that reason needs; check it before parsing `message`.

---

## What this API deliberately lacks

No endpoint exposes version control, worktrees, issues, work claims, or
planning. If you need one of those here, either the caller is asking the wrong
service, or this service has begun growing into a second supervisor.

> **muster knows a session has a working directory.
> It does not know what a worktree is.**

Nor does any endpoint return a session's screen text, a raw transcript, or
content a session produced — with **one** exception: `GET …/{id}/turns` returns
what the session's own agent wrote and nothing else (muster #258, narrowing
#82). There is still no "give me the result" route, and nothing stores a result
on a session's behalf. A dispatched agent's answer can still travel the way its
input did — the caller names a reply address at dispatch time and the worker
delivers its answer there itself — and a coordinator can also read it back with
`turns`. One pushes, one pulls.

---

## Known gaps between this document and the code

Recorded rather than smoothed over, because a reference that quietly disagrees
with the implementation is worse than one that admits where it does.

- **`GET /v1/sessions?machine=`** appears in the specification. The filter has no
  such field; the parameter is silently ignored. Use `scope` and filter
  client-side.
- **`respond`'s outcome vocabulary** is documented in the specification as
  `queued | refused`. It shares its type with `input` and can also return
  `submitted` or `unknown`.
- **`input`'s outcome vocabulary** is documented in the specification as
  including `submitted`, and the type genuinely allows it — but no driver in
  this fleet currently reports `confirmsDelivery: true`, so `input` cannot
  actually return it today. A confirmed submission, including a confirmed
  `resumeIfStranded` retry, reports `queued` instead. `submitted` remains
  reachable through `respond` and `keys()`.
- **`input`'s inbox outcomes are documented as five values; four of them cannot
  occur.** Only `delivered` is reachable. `held`, `denied`, `expired` and
  `dropped` would all require reading a receipt the protocol routes to a reply
  address this service does not hold (#120). `held` is the consequential one:
  #148 measured a receiver holding 206 messages for a human who never came and
  then dropping them, while this endpoint answered `delivered` every time. That
  is addressed at the source rather than by producing `held`: a send this
  service cannot attest declines the inbox path and falls back to the terminal
  one, and (#184) an inbox write is confirmed from the receiver's own transcript,
  so `delivered` now means "the receiver's transcript recorded the message" and a
  held-then-dropped message reads `unknown`, counted as `inbox.unconfirmed`. It
  still does not mean the model has acted on the turn. **Which transcript entry a
  peer message produces has not been measured on a live fleet** (the inbox is
  switched off wherever it was measured): until it has, `inbox.unconfirmed`
  close to `inbox.written` means the matcher does not recognise what the runtime
  writes, not that messages are being lost.
- **`deliversToInbox: true` does not imply any send will use the inbox.** Since
  #148 a send also needs the target's permission-mode class from the
  machine-local index; without it every send falls back while this flag still
  reads true. On a machine whose index writer does not emit that field yet, that
  is every send.
- **`/v1/machines` and `/v1/runtimes` take no `scope`.** Every other plural
  endpoint does.
