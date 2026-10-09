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
| `unauthorized` | 401 / 403 | Caller not permitted for this verb on this machine |
| `not_found` | 404 | The machine answered, and there is no such session |
| `conflict` | 409 | Idempotency key reused with a different body; a replayed key whose session has since ended (`reason: "replay-of-ended-session"`, §3.3); or a destructive request whose `startedAt` disagrees with the live session (§5.4) — the request is well formed, the caller's belief is stale |
| `unsupported` | 501 | Driver lacks the capability (§4.3 of the model) |
| `unreachable` | 504 | **The machine did not answer.** Nothing is known. |

**`not_found` and `unreachable` must never be conflated.** One means the fleet
knows the session does not exist; the other means the fleet knows nothing at
all. This is §5.7 expressed at the wire, and it is the single most important
line in this document: a client that treats 504 as 404 will confidently report
work as gone while it is running fine on an unreachable host.

**`reason` further classifies some `conflict` bodies beyond what `kind` alone
distinguishes**, so a caller can branch without parsing `message`. Absent
unless a case documents one — today the only one is `create`'s own
`replay-of-ended-session` (§3.3), which also carries `session` (the ended
session's own ref) and `closedAt` (when this machine observed the absence, an
upper bound on the true end — the same rule `GET .../sessions/closed`'s own
`closedAt` follows for a `ClosedByAbsent` tombstone, §3.2).

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
        "maxInputBytes": 1024,
        "labels": { "maxKeys": 16, "maxKeyBytes": 128, "maxValueBytes": 128 },
        "supportsConversationId": true,
        "supportsLaunchSettings": true,
        "launchSettingsOutsideBypass": ["crossSessionInbound"],
        "drivers": [...] }

GET /v1/machines[?verify=1]
→ 200 { "items": [ { "machine": "...", "self": true, "status": "ok",
                     "observedAt": "...",
                     "build": { "known": true, "revision": "...",
                                "modified": false, "time": "...",
                                "go": "go1.26.5" },
                     "maxInputBytes": 1024,
                     "peer": { "listsMeBack": true,
                               "grantsToMe": ["read", "send"],
                               "source": "observed",
                               "observedAt": "..." } } ],
        "sources": [...], "complete": true }

GET /v1/runtimes
→ 200 { "items": [ { "machine": "...", "runtime": "...",
                     "capabilities": { "observesState": true,
                                       "deliversRawKeys": true,
                                       "confirmsDelivery": true,
                                       "supportsResume": false,
                                       "deliversToInbox": false,
                                       "supportsPin": { "model": true,
                                                        "effort": false,
                                                        "agent": true },
                                       "remoteControl": { "toggle": true,
                                                          "off": true },
                                       "source": "observed",
                                       "observedAt": "..." } } ],
        "sources": [...], "complete": true }

GET /v1/whoami[?machine=<id>][&peer=<id>]
→ 200 { "principal": "...", "machine": "...", "grants": ["read", "send"],
        "source": "observed", "listsYou": null }
```

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

A `scope=local` response carries exactly one `SourceStatus`. The proxying
service **adopts** that record rather than synthesizing a fresh one (§13.2) — a
peer that answers promptly while reporting itself `degraded` must not be
relayed as `ok`.

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

#### Closed sessions

```
GET /v1/sessions/closed?scope=fleet|local&since=<RFC3339>
→ 200 Collection<ClosedSession>
```

A service keeps **one record per local session that ended**, for a configured
retention period, across restarts (muster #179). It is deliberately not a
persisted event window: the record carries only the metadata the live record
already carried as last seen (`machine`, `id`, `name`, `runtime`, `cwd`,
`startedAt`, and `conversation` when it was known), never content, plus
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
up, are not evidence about the peer and never count. `scope=local` never
touches a peer in any state. The wire shape is unchanged; only the wait is.
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
  "env": {"NAME": "value"}, "resume": "<conversation id>",
  "conversationId": "<caller-chosen uuid>",
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

**`prompt` is capped by this machine's effective limit** — 1024 bytes by
default, and a machine setting rather than a compiled-in constant since
muster #130 (see `maxInputBytes` in §3.1 for how to read it without
triggering it). Over that, the create is rejected outright — `invalid`
(400) naming the limit and the caller's actual size — instead of being
accepted and left to strand in the composer with no delivery receipt to
explain why (muster #114, #110, #112: the creation path measurably
strands even shorter prompts than `input` does). 1024 is a conservative
default, not the bisected true failure boundary, which #114 leaves as open
work; #130 argues the mechanism behind that boundary may be startup timing
rather than size at all, and deliberately keeps the default unchanged while
making the limit configurable — raising it is a separate decision that
follows a bisect (#129), on evidence, per machine. A caller with more to
send should write it somewhere the agent can read deliberately and pass
only a short pointer here — the same workaround #112 already adopted ad
hoc.

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
reaches a command line (§5.3).

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

**`permissionMode` requests a non-default permission posture.** One value:
`bypass`. It requires the **`send` grant** too — a session in that mode acts
without asking, and between "may start a session" and "may start a session that
needs no permission for anything", the second is plainly the larger authority.
An unrecognised value is refused rather than passed through.

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
                     for another conversation / points past the end of the record
→ 501 unsupported    the runtime, or the peer's build, cannot read turns
```

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
instead of requested, and `input` delivers it for any caller holding `send`
(§3.3, the session-management commands). A machine closes that door with the
`gateRemoteControlInput` setting (muster #272): with it on, `input` delivers
those two commands only to a caller holding `remote-control` or `human-relay`,
and the refusal names both grants. The setting is **off by default for one
release** — the earlier behaviour — so existing callers have a release to move
to this verb, after which the default flips; until a machine sets it, the grant
governs this verb and not the older door. The verb's own command is not subject
to the setting. A relayed `input` carries the fact that the original caller held
`remote-control` as an assertion the owning machine honours only from a
configured peer that itself holds `remote-control`; the peer's own grant never
stands in for the caller's.

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
(session-abstraction.md §7.1a, muster issue #60):

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

> Origin: session-abstraction.md Appendix A, F1; the default-runtime
> tiebreak and its headers: muster issue #60.

```
POST /v1/machines/{machine}/sessions/{id}/input?runtime=
{ "text": "..." }                      (submit defaults to true: §3.3)

→ 200 { "outcome": "submitted" | "queued" | "refused" | "unknown",
        "reason": "prompt holds unsent input",
        "delivery": { "route": "terminal" | "inbox" | "module",
                      "module"?: "<name>" },
        "sessionIds"?: ["<session id>", ...] }   (refused: `{id}` was a conversation id, §2)
```

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
known-gaps section tracks this alongside the equivalent, opposite-direction
gap already recorded for `respond`.

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

→ 200 { "outcome": "queued" | "refused", "reason": "..." }
→ 400 invalid   // choices empty, repeated, < 1, or combined with choice/cancel;
                // text empty or blank, over the input byte limit, or combined with choice/cancel
```

`state.waitingOn` discriminates `waiting_input`, which carries two situations
needing opposite handling: `prompt` (answer it) and `unsent-input` (do not send
to it). Only the first has a `prompt` to branch on, so without this field they
are separable only by reading `evidence` — prose explicitly not to be parsed.
Absent means unclassified (§5.7), not "no reason".

A session out of quota is **not** one of these: it reports `quota_blocked`
(§2.3), because nothing a caller sends will unblock it.

`state.lastTurn` — when present — says how the most recent turn **ended**:
`{"outcome":"failed","reason":"…","retryable":true}`. It exists because a turn
that died and a turn that finished leave the same screen: an error, a settled
status line, an empty composer. Both are honestly `idle`, and a supervisor that
cannot tell them apart silently abandons the work.

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

## 4. Events

```
GET /v1/events?cursor=<last-seen>&epoch=<last-seen>&session=&cwdPrefix=
Accept: text/event-stream
```

`session` may be repeated to name several sessions. Selectors narrow and
compose with AND. Naming is not sugar over `cwdPrefix`: a substrate may charge
per watched session, in which case a caller that can only describe what it
wants pays for every match (§5.5).

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
| `source.status` | a machine's reachability changed |
| `machine.quota` | `{ "machine", "blocked": bool, "quota"? }` — this machine's **account** started or stopped refusing work |
| `machine.account` | `{ "machine", "generation" }` — this machine's local **credential material** changed |
| `control.resync` | `{ "reason": "epoch_changed" \| "cursor_expired" }` |

`source.status` exists so a client learns a peer went away as an **event**,
rather than inferring it from data that stopped arriving. Inferring absence
from silence is the failure mode this whole specification is organised against.

**`session.state` fires on any material change**, which is every structured
field a caller branches on: `status`, `confidence`, `waitingOn`,
`composerDigest`, `strandedDelivery`, the prompt (its options, highlight and **nonce**), `quota`,
`lastTurn`, `turns`, `credentialGeneration`. It began firing on `status` alone, and
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
| `cursor_expired` | your cursor is older than what is retained — or newer than anything this service has stamped, which it equally cannot supply |
| `feed_gap` | **the sequence is intact and this service stopped watching.** Its subscription to a driver dropped and was re-established, so changes in between were never stamped at all |

`feed_gap` is not a politer `cursor_expired`. One says the caller fell behind;
the other is this service admitting the hole is its own, and telling a caller
its cursor is too old when the cursor is perfectly current sends it hunting a
slowness problem it does not have. The action is the same for all three —
refetch and resubscribe — which is why a client that already handles
`control.resync` needs no new code, only a better log line.

**`source.status` also reports the feed itself.** A driver subscription that
fails or ends is announced `degraded` and retried; when it comes back, `ok`
arrives with a `feed_gap` resync beside it. Both edges, on the transition only.
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

## 5. Authorization

- Every request carries `Authorization: Bearer <token>`.
- **There is no unauthenticated mode.** A service that can start processes and
  read paths is a remote-execution surface whatever its intent, so there is no
  configuration in which authentication is off — not for loopback, not for
  development.
- Permissions are **per verb, per machine**. `list` and `state` may be granted
  broadly; `create`, `input`, `interrupt`, `close`, `rename`, `discard`, `keys`,
  `label` and `remote-control` are granted per peer and default to denied. That default is deliberate
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
  `gateRemoteControlInput` on (§3.3 `remote-control`, muster #272) are
  otherwise delivered only to a `remote-control` holder.
  Nothing else confers this — no header, no `from`, no `relayOfHuman` — and it
  crosses a peer relay only as an assertion the owning machine honours from one
  of its configured peers (or from anyone, on a machine with no principal table:
  §3.3, `route`). It defaults to denied
  like every other grant, and it is not something to hand to an agent: it is the
  grant that lets a message skip the label.
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
- Every remote-originated mutation is logged: actor, verb, target, outcome.

## 6. What this API deliberately lacks

No endpoint exposes version control, worktrees, issues, claims, or work
planning (§1 non-goals). If such an endpoint ever looks necessary, the
supervisor is asking the wrong service, or this service has begun to grow into
a second supervisor.

No endpoint returns a session's screen text, a raw transcript, or content a
session produced, **except one narrow, owner-ruled route**:
`GET …/{id}/turns` (§3.3, muster #258) returns what the session's own agent
wrote — its assistant turns — and nothing else: no tool output, no inbound
message, no system text. None stores a result on a session's behalf
(session-abstraction.md §5.8, muster #82, as narrowed by
`docs/adr/258-assistant-turns-read.md`). The data class is still one this
service declines to carry in general, not a domain it doesn't understand; the
exception is bounded by provenance, not by sensitivity. A dispatched agent's
answer may still be delivered by the agent, to a reply address the caller
supplied at dispatch, over `input`; the two are complementary — one pushes, one
pulls.
