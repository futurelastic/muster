# Session identity — a machine declaring what its sessions carry

Reference for `sessionEnv`, the `FLEET_CONFIG` entry from muster issue
#94. If you are looking for the wire shape of a create request, that is
[`api.md`](api.md) and [`spec/api-http.md`](spec/api-http.md) — this feature
adds no field there (see "Why this needed no wire change" below). This page is
for the person writing the machine's configuration file and the person
debugging why a session did, or did not, come up holding a credential.

---

## The problem this closes

A create's `env` map is what a caller passes explicitly. Nothing before this
let an *operator* say "every session this machine starts also carries X" —
so the variable had to be repeated on every create, forever, by every caller,
and the one caller that forgot did not get an error. It got a session that
looked healthy and quietly fell back to whatever ambient identity the
consuming tool found lying around on disk. Wrong identity, exit code zero,
nothing on screen to say so.

`sessionEnv` is the fix: a machine-local declaration, read at session-creation
time, of what that machine's sessions always hold.

## Configuration shape

A list under `sessionEnv` in the same JSON file `principals`, `peers`,
`trustRoots` and `defaultRuntime` already live in (`FLEET_CONFIG`):

```json
{
  "principals": [{"name": "op", "token": "…", "grants": ["create", "send"]}],
  "sessionEnv": [
    {
      "name": "FLEET_IDENTITY",
      "fromFile": "/absolute/path/an/operator/controls",
      "required": true,
      "appliesTo": {"agents": ["some-agent"], "markers": []}
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `name` | The variable a session's process will see. Same shape rule every env name in this project follows: letters, digits, underscore, not starting with a digit. |
| `fromFile` | An absolute path this machine reads **fresh on every create** — see "Restart enables it; rotation needs nothing" below. |
| `fromSession` | The other source: the created session's own identity, `"ref"` (`<machine>/<id>`) or `"id"` (the bare id). Exactly one of `fromFile` / `fromSession` per entry — see "Exporting the session's own identity" below. |
| `required` | Fail closed (`true`) or silently omit (`false`) when the file cannot be read. Per entry, not machine-wide — see "Fail-closed, per entry". |
| `appliesTo` | Optional. `runtimes`, `agents` and/or `markers`: `runtimes` is matched against the runtime the session is created on, `agents` / `markers` against `SessionSpec.Agent` / `SessionSpec.Marker`. Absent or all-empty matches every session on every runtime the machine registers — see "Scoping: `appliesTo`". |

Nothing here is optional to *validate*: every entry is checked once, at daemon
start, against the same rule set a Go test exercises directly
(`ValidateSessionEnv` in `internal/drivers/tmux`) — an entry with neither or
both of `fromFile` / `fromSession`, a relative path, a `fromSession` other than
`ref` / `id`, a bad variable name, or the same `name` declared twice all fail
startup with a message naming the entry, rather than becoming a create-time
refusal every later caller meets.

### Why this is config-file-only, with no `FLEET_` environment variable

`trustRoots` and `defaultRuntime` set the precedent: which runtimes exist and
which directories are pre-trusted are facts about *this machine*, not the
fleet, so they belong in the file an operator already edits per machine. A
`sessionEnv` entry is a structured record — name, path, a flag, a scope — and
a comma-separated environment variable has no honest way to carry that shape
without inventing a second delimiter grammar. The file an operator already
diffs is the right place for a fourth structured field, not a reason to widen
the number of state sources this daemon reads at startup.

### The config loader refuses unknown fields — so the order of operations is fixed

`loadConfig` decodes with `DisallowUnknownFields`. A `sessionEnv` key placed in
a machine's configuration file **before** the code understands it does not get
ignored — the whole file fails to parse and the daemon refuses to start. So the
order is always: ship the code, then add the config entry. Never the reverse,
and never both machines in the same step — see "Rollout" below.

## Restart enables it; rotation needs nothing

Two different operations, and the split is the entire feature:

| Operation | What it needs |
|---|---|
| Adding or changing a `sessionEnv` entry (the **declaration**) | A daemon restart. Configuration is read once, at startup; there is no reload path. |
| Rotating the credential a `fromFile` points at (the **value**) | Nothing. The next session created picks up the new content — no restart, no coordination. |

This is a requirement, not an implementation shortcut. Caching the value at
daemon start was the obvious first implementation and the wrong one: rotate
the file, and every session created afterward would keep receiving the stale
value until somebody restarted the service, with nothing on screen to explain
why. Reading `fromFile` fresh on every `Create` is what makes rotation a file
write instead of a deploy.

## Fail-closed, per entry

`required` answers "what happens when `fromFile` is missing, unreadable, or
empty" — and it is per entry because both answers are legitimate on the same
machine for different variables. An identity credential almost certainly wants
`required: true`; a nice-to-have almost certainly does not.

- `required: true`, file unusable → the create refuses (see "Two different
  refusals" below for which wire answer).
- `required: false`, file unusable → the variable is silently omitted, exactly
  as if the entry were not declared for this session at all.

Silent omission is not on the table for a required entry, because it is
indistinguishable from success at creation time and only becomes visible as an
action taken under the wrong identity later, somewhere else — the same failure
shape this whole feature exists to close.

## Precedence against the caller

Recorded here because it is the one part of this design that has already been
wrong once, and it is implemented exactly as written
(`provisionSessionEnv` in `internal/drivers/tmux/sessionenv.go`):

| Entry | Caller | Result |
|---|---|---|
| required | silent | configuration provides the value |
| required | supplies ANY value | **refuse the create, uncompared** |
| non-required | supplies a value | the caller's value wins |
| non-required | silent | configuration provides the value |

A required entry never looks at what the caller supplied beyond "did it
supply anything at all" — it does not read the configured file and does not
compare, whatever the caller's value is. "The caller wins" is not on the
table for a required entry either: any caller could otherwise hand a session
a different credential and quietly downgrade a required identity, which
defeats the point of marking it required. Refusing every caller-supplied
value is the option that is both fail-closed and legible about it.

### Why "the same value proceeds" was removed — it was an equality oracle

An earlier revision of this table had a third required-entry row: *"caller
supplies the SAME value → proceed, not a conflict."* That reasoning sounds
right — agreement is not a disagreement — and it shipped, was implemented
faithfully, and was wrong. It turns the create endpoint into a way to confirm
or deny a **candidate** value for a secret the caller has no read access to:
a caller holding only the `create` grant can supply a guess for a configured
variable and learn, from whether the create succeeds or is refused, whether
the guess matched. Brute-forcing a long credential this way is infeasible and
was never the threat; confirming a candidate is. "Is this stale copy still
the live credential" is exactly the question someone holding a leaked or
outdated value wants answered, and the old rule answered it through a
documented, authorized path with no failed-auth trace anywhere.

The fix removes the comparison entirely rather than tightening it: a required
entry refuses any caller-supplied value for that name **before the configured
file is even read**. Reading the file first and comparing afterward would
still make the outcome depend on the secret — at minimum through a timing
difference between the match and no-match code paths, which is the same
oracle with extra steps. The refusal message says only that the name is
required on this machine and must be omitted; it never says whether the
value the caller sent would have matched.

Nobody loses a capability from this fix. A caller that wants the configured
identity omits the field, which was always the intended path — the removed
row was only ever a convenience for a caller that happened to already hold
the right value, and that convenience was the leak.

Because this whole feature is opt-in through configuration, the refusal can
never affect an existing caller until an operator adds a `required` entry —
and `appliesTo` is the escape hatch when one genuinely needs to differ
(below).

## Scoping: `appliesTo`

`appliesTo.agents` and `appliesTo.markers` match `SessionSpec.Agent` and
`SessionSpec.Marker`; `appliesTo.runtimes` matches the runtime the session is
created on. A scope naming none of them is the common case — an operator
declaring an identity for the whole machine — and a session outside a scoped
entry's reach never meets that entry at all: not provisioned, not required,
not compared against.

This exists so the first session that must deliberately act as something
other than the configured identity costs a configuration edit, not a code
change and a deploy on the exact path where a bug means every new session on
the machine refuses.

### How the three selectors combine

`runtimes` is a gate in front of `agents`/`markers`, not a third alternative
beside them:

```
runtimes  AND  ( agents  OR  markers )
```

An empty list on any axis means "any" for that axis. So:

| `appliesTo` | Reaches |
|---|---|
| *(absent)* | every session on every runtime |
| `{"runtimes": ["opencode"]}` | every session on that runtime, whatever its agent or marker |
| `{"agents": ["sid"]}` | sessions of that agent, on every runtime — unchanged from before `runtimes` existed |
| `{"runtimes": ["opencode"], "agents": ["sid"]}` | that agent's sessions on that runtime only |
| `{"runtimes": ["opencode"], "agents": ["sid"], "markers": ["filing"]}` | on that runtime, sessions of that agent **or** carrying that marker |

`agents` and `markers` alternate with each other exactly as they always did;
adding `runtimes` narrows what they reach and never widens it.

The runtime matched is **the one serving the create** — the driver's own id —
not the `runtime` a caller put in the request, which is empty whenever the
caller took the default and which a caller must not be able to use to pull a
credential into a runtime it was not scoped to.

### Hazard: an unscoped entry reaches every runtime, including ones registered later

A `sessionEnv` entry with no `appliesTo.runtimes` is delivered to the sessions
of **every** runtime this machine registers. That includes a runtime enabled
long after the entry was written: switching on a second runtime does not ask
the operator again about the credentials the machine already declares, and a
third-party agent process in that runtime then starts with them in its
environment. A required entry cannot be removed by a caller's `env` either —
that is what required means.

So a credential that belongs to one runtime's sessions should say so:

```json
{"name": "SOME_API_TOKEN", "fromFile": "/absolute/path", "required": true,
 "appliesTo": {"runtimes": ["claude-code-tmux"]}}
```

Runtime ids are the `runtime` values `GET /v1/runtimes` reports. Each id is
checked once, at daemon start (and by `muster doctor`), against the runtimes
this build can register — a misspelled id is a startup message, not an entry
that silently never applies. The check is against what the build *knows*, not
what is running, so one configuration file can be shared by machines that do
not all enable the optional runtime.

## Exporting the session's own identity

A tool running inside a session often has to name that session to a third
party — a service that must later deliver an answer back into the session
through this API. The session's ref is known only to this service, at create
time: the create's `env` cannot carry it (the caller learns the id from the
create's answer), and a `required` entry refuses a caller-supplied value anyway.
So an entry may take the identity itself instead of a file:

```json
{"name": "FLEET_SESSION_REF", "fromSession": "ref",
 "appliesTo": {"runtimes": ["claude-code-tmux"]}}
```

`"ref"` is `<machine>/<id>` — the form `/v1` paths and peers use — and `"id"` is
the bare id. The variable name is the operator's; this service names no tool
that consumes it. Everything else is unchanged: the value is staged like every
other (never on a command line; a non-ASCII marker in the id survives byte for
byte), `appliesTo` scopes it, and the precedence table above applies — a
`fromSession` entry is always available, so `required` only decides whether a
caller-supplied value for that name is refused.

The value is the id the session has when it is created. A rename changes the
id afterwards and does not change what the process already holds.

**Only runtimes that know the id before the process starts can honour it.** The
terminal-multiplexer runtime names the session itself, so it does. The opencode
runtime is handed its id by the runtime after the server is up, when the
environment is already fixed — a `fromSession` entry that could reach it is
refused at startup, with a message pointing at `appliesTo.runtimes`, rather than
left to produce sessions that silently lack the variable.

## Two different refusals, and why they answer differently on the wire

Two distinct things can go wrong inside `provisionSessionEnv`, and they must
not read alike to a caller:

- **The caller supplied ANY value for a name a required entry owns** —
  matching or not; the two are not distinguished (see "Why 'the same value
  proceeds' was removed" above). Correctable by the caller — omit the field,
  there is no other fix — so this is an ordinary error that reaches the wire
  as `invalid` / 400, same as every other malformed-create refusal in this
  driver.
- **This machine cannot back a required entry** — the file is missing,
  unreadable, or empty. No correction to the request body fixes an absent
  file on this machine; a caller that retries is retrying forever against a
  condition only an operator can clear. This is reported as a typed error with
  `Kind: unsupported` (HTTP 501), addressed to the operator in its message,
  not to the caller.

The second choice closes a pre-existing asymmetry rather than inventing a
third answer: this driver's own plain env refusal used to fall through to
`invalid` by default, while the *other* local driver in this repository
(`internal/drivers/opencode`) already answers its own env refusal with
`unsupported`. Same category of refusal, two different wire answers, before
this. `driver.ErrUnsupported`'s own contract — "nothing will change by asking
again" — is precisely true of a create that keeps failing until an operator
repairs a file, which is why this case uses it rather than a bespoke kind.

*Open item, not yet made normative:* `spec/api-http.md` does not yet document
this refusal — it is deferred until the naming/marker work already in flight
on this file lands, to avoid two sessions editing one uncommitted document.
Proposed wording, for whoever picks that up:

> A `create` may fail with `unsupported` (501) for a reason that has nothing
> to do with the request body: an operator-declared `sessionEnv` entry is
> `required` on the target machine and its backing file is missing,
> unreadable, or empty. The message names the entry and addresses the
> operator; retrying the same request will not help until the file is
> repaired.

## Why this needed no wire change at all

`sessionEnv` feeds values into `SessionSpec.Env`, the field a caller can
already set — through the same staging file, consumed by the same
login-shell wrap. No new field reaches the wire, so:

- `spec_fields_test.go`'s conformance check (every `SessionSpec` field appears
  in the normative spec documents) is untouched — nothing was added to check.
- The implementation stays off the files another in-flight change on this
  repository is holding (`session.go`, the naming driver and its test, this
  spec document, `.gitignore`).
- The feature is honestly described as what it is: an operator-side
  declaration, not a new thing for a caller to learn.

## Why provisioning happens in the driver, never in the HTTP handler

A create aimed at this machine can arrive two ways: served locally, or relayed
here by a peer that forwards the request body — `env` included — verbatim
(`internal/drivers/remote`). If configuration were merged into the spec in the
HTTP handler, a create this service only **relays** onward to a third machine
would read this machine's files and ship the value over the network to a
session that will actually run somewhere else — then get merged a *second*
time against that other machine's own configuration. "Per-machine identity"
has to mean the machine that runs the session, not the machine that took the
call, so provisioning runs inside `Driver.Create` itself, strictly after the
relay decision has already routed the request to whichever driver instance
will actually serve it.

## Secret discipline — inherited, not reinvented

A configured value goes through exactly the channel a caller's own `Env`
value already does: staged to a 0600 file, applied by the login-shell wrap,
unlinked before the agent runs. Nothing new was built:

- Never reaches an argv, a log line, or a response body.
- Bounded to no newline and no NUL — checked against the configured value
  specifically, before the merge, so a bad file is attributed to the operator
  who owns it rather than reported as the caller's malformed input.
- An empty file is treated as no value at all, not as a value that happens to
  be the empty string.

## Two traps this feature inherits, not new ones

Both are properties of the driver `sessionEnv` feeds into, not of this
feature specifically, and both are measured in
`internal/drivers/tmux/environment.go`:

1. **Never add the variable to the process-manager unit.** Sessions are
   wrapped in a login-and-interactive shell precisely so a caller's `Env`
   reaches the process without ever touching the service's own environment;
   putting a `sessionEnv` value on the unit instead would leak it to every
   session, including ones `appliesTo` was configured to exclude.
2. **A clean shell has a four-entry search path**
   (`/bin:/usr/bin:/usr/ucb:/usr/local/bin`). Anything this daemon itself
   shells out on — outside the login-shell wrap a created session gets — must
   use an absolute path or construct its own search path; a bare command name
   can fail with a message that reads as "not installed" rather than "wrong
   PATH".

## Verifying an enabled entry

An exit code is not evidence, and the environment record's names-only shape
(§5.7; see `fleet.SessionEnvironment`) cannot distinguish a correct value from
a wrong one — it only shows that a name is present. The claim this feature
makes — a session came up holding a credential *because configuration put it
there* — is proven only by asking the tool the credential authenticates
**from inside the session** which identity it holds.

Checklist for enabling an entry on a machine:

1. Restart the daemon with the entry in place; confirm it started (a bad
   entry fails startup with a message naming it — see `ValidateSessionEnv`).
2. Create a session that falls inside the entry's `appliesTo` scope (or the
   whole machine, if unscoped). From inside it, run the tool the credential
   authenticates and read back the identity it reports.
3. Create a session **outside** the scope (a different `agent`/`marker`, or a
   session on a different runtime when the entry names `runtimes`) and
   confirm the variable is genuinely absent — the negative case, and the one
   `appliesTo` exists to make possible.
4. For a `required` entry, point `fromFile` at a path that does not exist and
   confirm the create refuses as `unsupported`, not as a hang or a
   healthy-looking session.
5. Rotate the file's contents with no restart and confirm the *next* session
   created picks up the new value — the property that motivated reading it
   fresh rather than caching it at startup.

## Rollout

Enable and verify on whichever machine hosts the smaller share of sessions
first — this touches the session-creation path, and a bug in a `required`
entry's plumbing means fail-closed refuses **every** new session on that
machine, so the ordering is about blast radius, not convenience. Sessions
already running are unaffected either way: they keep the identity they
booted with until they are themselves restarted, and there is no cutover
moment where every session on a machine changes identity at once.

Enabling this on a second machine is a separate decision on that machine's
own configuration; nothing here couples the two.
