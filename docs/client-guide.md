# Client guide — using muster from your own program

For someone **writing a client**: a supervisor, a dashboard, a CLI, a script.

If you want to know *why* the API is shaped this way, read
[`spec/session-abstraction.md`](spec/session-abstraction.md). This document
only tells you what to call and what you must handle. Where the two disagree,
the spec wins and this file is the bug.

Every response below is copied from a running service, with names and paths
replaced.

---

## 1. The mental model, in one picture

```
your program ──HTTP──▶ muster on YOUR machine ──HTTP──▶ muster on another machine
                                                    ──HTTP──▶ ... and another
```

**You talk to one service: the one on your own machine.** You never address a
peer directly. Ask for `scope=fleet` and your local service fans out, applies
deadlines, carries your identity, and reports which machines answered.

This is the single most important thing to get right, because the wrong version
looks like it works:

> **Do not keep a list of machines and call each one.** You would be
> reimplementing federation, deadlines, partial-failure reporting and
> authorization — badly, and in a place where adding a machine means changing
> your code instead of one config file.

There is no client library and none is needed. Every call is JSON over HTTP,
and the event stream is Server-Sent Events. In Node, that is `fetch` with no
dependencies.

## 2. Connecting

| what | value |
|---|---|
| Address | operational config, not a constant — read it from your own config |
| Auth | `Authorization: Bearer <token>`, required on **every** request |
| Content type | `application/json` |

There is no unauthenticated mode. Not on loopback, not in development.

**Get your own principal.** Do not reuse a token that happens to be on the
machine — the one lying around is rarely the one with the right grants, and
borrowing it is how a read-only client quietly acquires the ability to destroy
sessions. Ask the operator to run:

```sh
muster principal add my-supervisor --grants=read
# → token written to …/my-supervisor.token (0600)
```

It mints the credential, validates the grants before writing, and leaves the
running service untouched until it is reloaded. `muster principal list`
shows who holds what, without printing anyone's token.

Grants are per verb, so you can be given exactly what you need:

| grant | lets you |
|---|---|
| `read` | list, read a session, subscribe to events |
| `create` | start sessions on this machine |
| `send` | deliver input, and answer prompts |
| `interrupt` | interrupt a running turn |
| `close` | destroy sessions |
| `relay` | have any of the above forwarded to a **peer** |

`relay` is separate on purpose: "may mutate sessions here" and "may ask another
machine to mutate its own" are different questions, and a hardened host can
still be a full-featured client.

Every response carries `Fleet-Clock` (RFC3339, the answering machine's clock).
Use it to compute skew rather than assuming clocks agree.

## 3. Finding out what is there

Three endpoints answer "what am I talking to", and a client that skips them
ends up hardcoding answers that differ per machine.

```
GET /v1/health     → epoch, cursor, startedAt, build, drivers
GET /v1/machines   → which machines this service can see, and which one is you
GET /v1/runtimes   → what each machine's driver can actually DO
```

`/v1/health`'s `drivers` is an array of the same `{machine, runtime,
capabilities}` rows `/v1/runtimes` returns, for this machine only. If you want
capabilities, ask `/v1/runtimes` — it covers peers too.

`/v1/machines` tells you who is out there, and **which one you are**:

```json
{ "items": [ { "machine": "aurora",   "self": true,  "status": "ok", "observedAt": "…" },
             { "machine": "borealis", "self": false, "status": "ok", "observedAt": "…" } ],
  "sources": [ … ], "complete": true }
```

**`self` is the field you need before you attach anything.** A session on the
`self` machine is reachable from the terminal you are already in; a session on
any other machine needs whatever remoting you use. Without checking it, a
client either SSHes to its own host for every local session, or tries to run a
remote machine's attach command locally. Both look like they work until they
do not.

**`/v1/health` is your liveness check** — it is the question "is the session
layer up", which is not the same question as "are there sessions". It also
carries `build`, a version-control stamp of the running code:

```json
"build": { "known": true, "revision": "0f0d390…", "modified": false, "go": "go1.26.5",
           "version": "v0.1.0-2-g0f0d390" }
```

Show it somewhere. Two machines running different builds is a normal condition
during a rollout and an invisible one otherwise — it has already cost one
debugging session where the symptom made no sense against the source. When
comparing builds, treat `known: false` or `modified: true` as **unverifiable**,
never as equal.

**Need a minimum service version? Compare `build.version`, not `revision`.** It
is `git describe --tags` at the built commit: the tag is the release, and a
`-N-g<sha>` suffix means N commits after it. `null` (unstamped) or a missing
field (an older service) means **you cannot tell** — say that, rather than
reporting the service as too old. Full comparison rule: `docs/api.md`,
`GET /v1/health`.

**`/v1/runtimes` you MUST consult before relying on a capability**, and degrade
rather than assume when one is missing:

```json
"capabilities": { "observesState": true, "deliversRawKeys": true,
                  "confirmsDelivery": true, "supportsResume": true,
                  "supportsPin": { "model": true, "effort": true, "agent": true },
                  "source": "observed", "deadlineMs": 30000 }
```

Read `source` too. `assumed` means **nobody has confirmed these** — they are a
conservative floor for a peer that has not answered yet, not that peer's real
answer. Treating `assumed` as fact is how a briefly unreachable machine becomes
a permanently incapable one in your UI.

**A fourth endpoint answers a question about yourself, not the fleet:**

```
GET /v1/whoami[?machine=<id>]
→ 200 { "principal": "...", "machine": "...", "grants": [...], "source": "observed" }
```

Call it before a mutating request rather than after its refusal — every
precondition a route enforces is otherwise discoverable only by trying and
reading whichever refusal comes back first. Omit `machine` (or name your own)
for a direct answer: `source: "observed"`, `grants` exactly what your
credential holds here, including an empty list if it holds nothing — that is
a real answer, not a fault. Name a peer and you get `source: "assumed"`,
`grants: []`, always — this service has no channel to ask a peer what it has
granted your credential, so it never guesses. See "Getting an answer back
from a dispatched session" below for why that matters: a relayed operation
needs a grant on each of two machines, and this route can only ever confirm
one of them for you.

## 4. Reading the fleet

```
GET /v1/sessions?scope=fleet
```

Optional filters: `machine`, `status`, `agent`, `cwdPrefix`.

```json
{
  "items": [
    {
      "machine": "aurora",
      "id": "alpha💬",
      "name": "alpha💬",
      "runtime": "claude-code-tmux",
      "cwd": "/work/alpha",
      "startedAt": "2026-08-01T23:14:01+07:00",
      "attach": {
        "kind": "multiplexer",
        "target": "alpha💬",
        "command": ["/opt/homebrew/bin/tmux", "attach-session", "-t", "alpha💬"],
        "readOnly": ["/opt/homebrew/bin/tmux", "attach-session", "-r", "-t", "alpha💬"],
        "shared": true
      },
      "conversation": {
        "known": true,
        "id": "0f5c2e18-…",
        "source": "derived",
        "evidence": "the only record in this session's working directory carrying the name this service gave the session"
      },
      "state": {
        "status": "idle",
        "confidence": "inferred",
        "evidence": "spinner line in finished form; composer empty",
        "since": "2026-08-05T10:12:28+07:00"
      }
    }
  ],
  "sources": [
    { "machine": "aurora",   "status": "ok", "count": 25, "observedAt": "…" },
    { "machine": "borealis", "status": "ok", "count": 68, "observedAt": "…" }
  ],
  "complete": true
}
```

**`conversation` has three states and you must not merge two of them.** The
field **absent** means nobody looked — the driver has no record store, or the
lookup is switched off. `"known": false` means somebody looked and could not
tell, and its `evidence` says why (several records could be this conversation;
every candidate predates the session; nothing has been recorded here yet).
`"known": true` names the record the runtime itself keeps, and `source` tells
you whether the identifier was *matched* (`derived`) or observed at creation
(`captured`). Corroborating against a matched value while believing it was read
is the one mistake this field was added to make impossible — so branch on
`source`, and treat an absent field as "I don't know", never as "there is none".

`evidence` is for people, but it does tell you which rule answered. A session that
*continued* a conversation begun before it (a resumed session), or that has no name to
match, is identified from the runtime's own record of the running process — the
evidence begins `per-process record, start time corroborated` — because the name-based
match cannot answer for either. `source` reads `derived` for both rules. When the two
name different conversations the answer is `"known": false` and the evidence names
both; neither is chosen, so do not treat that as "no conversation".

`conversation` describes the process running in the session **now**, not the one that
started it. A session whose runtime is relaunched in place (a recovery that mints a new
conversation, say) keeps its name and its pane and changes conversation; the next read
reports the new one. In between, `"known": false` means the new process has not written
its record yet — wait and read again; never keep acting on the id you read before the
relaunch. The same holds when the runtime starts a new conversation inside the running
process (a `/clear`): the next read names the new conversation, never the previous one.

**This costs one round trip and, on the multiplexer driver, a constant number
of subprocess spawns regardless of session count.** You are not being charged
per session, so do not build a per-session read loop to "avoid a big response".

### You MUST read `sources` and `complete`

`items` is not the whole answer. A machine that did not respond contributes a
`SourceStatus` — it never silently drops out of `items`.

**This applies to every plural response, not just this one.** `/v1/sessions`,
`/v1/machines` and `/v1/runtimes` all return the same envelope, and all three
can be partial for the same reason.

```json
{ "machine": "borealis", "status": "unreachable", "error": "no answer within 3s" }
```

`complete: false` means at least one machine failed to answer, and **you are
looking at a partial fleet**. A UI that renders `items` and ignores this will
tell someone their sessions are gone when the truth is that a machine is
unreachable. If you implement one rule from this document, implement that one.

`status` values: `ok`, `degraded`, `unreachable`.

**To find a session when you only know its id, list and scan.** There is no
id filter, and you do not need one: a fleet-wide listing is a single round trip
that already contains every session, so scanning `items` is the intended route
rather than a workaround. The single-session endpoint (§5) needs a machine,
which is exactly what you do not have yet.

**Ids are opaque and hostile to naive formatting.** They routinely contain
spaces and emoji. Never build a space- or tab-delimited line out of them, never
`cut`/`awk` such a line back apart, and always percent-encode an id when you put
it in a URL path — on *every* endpoint, not just the ones whose examples happen
to show it.

**"Does session X exist?" has three answers, not two.** If you did not find it
and `complete` is true, it does not exist. If you did not find it and
`complete` is false, **you do not know** — it may be sitting on the machine
that failed to answer. Any function of yours that returns a plain yes/no is
throwing that third case away, so make it return the third case, or make it
loud. Collapsing "I could not see it" into "it is gone" is how a supervisor
starts recreating work that is already running.

### Session state

| field | meaning |
|---|---|
| `status` | `starting`, `idle`, `working`, `waiting_input`, `quota_blocked`, `dead`, `unknown` |
| `confidence` | `observed` (measured) or `inferred` (deduced from a screen) |
| `evidence` | human-readable reason — show it, do not parse it |
| `since` | when this status was **first observed to hold** |
| `prompt` | present when the session is blocked on a question (§6) |

Two rules that will save you an incident:

- **`quota_blocked` is an ACCOUNT fact, so it applies to sessions that look
  perfectly idle.** The service remembers it from whichever session announced
  it, keeps it across restarts, and clears it when any session is seen working
  again. `state.quota.resetHint` carries the runtime's own words about when it
  lifts — **display it, do not parse it**; it is scraped prose and another
  consumer of the same line ended up with the next widget glued onto the end.
- **`quota_blocked` means the provider refused it** — alive, but out of quota.
  Nothing you send will unblock it; it clears with time or a different account.
  Do not dispatch work to it and do not treat it as stuck.
- **`waiting_input` has two causes — branch on `waitingOn`, never on the
  evidence text.** `prompt` (a question is on screen, answer it) and
  `unsent-input` (the composer holds text nobody submitted — do NOT send more;
  `since` is its age). Empty means the service could not tell, which is "go
  look", not any particular cause.
- **`idle` does not mean the last turn succeeded.** Check `state.lastTurn`: a
  session whose turn died on a transient error looks exactly like one that
  finished — same empty composer, same settled status line. If `lastTurn.outcome`
  is `failed` and `retryable` is true, sending anything resumes it; no human is
  needed. Ignore this and abandoned work sits looking available.
- **`unknown` is not `dead`.** It means the service could not determine the
  state. Never clean up, kill or restart on `unknown`.
- **`since` is your stall detector, and it needs no probe.** `waiting_input`
  held for 12 seconds is a human typing; held for 14 hours it is a session
  nobody is coming back to. Do not type into a pane to find out — that is
  exactly the invasive test this field exists to replace.

## 5. Reading one session

```
GET /v1/machines/{machine}/sessions/{id}
```

Returns the same `Session` shape (not wrapped in an envelope). Use it when you
already know the id; use the listing when you want many, since a listing costs
about the same as a single read.

**404 and 504 mean opposite things and must never be conflated.**

| you get | it means | do |
|---|---|---|
| `404 not_found` | the machine answered; there is no such session, and there never was | treat as gone |
| `200` with `state.status: "dead"` | the machine had this session and it has ended | treat as finished |
| `504 unreachable` | **the machine did not answer. Nothing is known.** | retry; never report the work as gone |

A client that treats 504 as 404 will confidently report live work as lost.

## 6. Answering a question the agent is blocked on

When a session is `waiting_input` because a menu is on screen, `state.prompt`
is populated:

```json
"prompt": {
  "kind": "folder-trust",
  "question": "Do you trust the files in this folder?",
  "options": ["1. Yes, I trust this folder", "2. No, continue without these"],
  "selected": 1,
  "nonce": "b6f0…"
}
```

Answer it:

```
POST /v1/machines/{machine}/sessions/{id}/respond
{ "choice": 1, "nonce": "b6f0…" }
```

- `nonce` covers the question and its options — and, on a tabbed dialog, the
  header row and which tab is current, so two tabs that ask the same thing still
  differ. If the screen changed since you read it, your answer is refused rather
  than applied to a different question.
  **Always send it** — it is optional only for a human answering something they
  are looking at this second.
- `question` is what the screen asks, joined onto one line. Show it to a person;
  do not parse it. On a tabbed dialog the tmux driver reports the whole question,
  however many rows it wraps over, up to 32 — and the rows nearest the options
  when it is longer. On a menu with no tab bar it reports only the last three
  rows above the options, because rows further up may be the transcript the menu
  was drawn under.
- `choice` is 1-based. Omit it to accept the highlighted default, and `cancel:
  true` dismisses instead of answering.
- `kind` names the question when the service recognises it — `resume-chooser`,
  `folder-trust`, `external-imports`, `settings-trust`, `tool-permission`,
  `feedback-review` (never auto-answer that one: see below).
  Filter on it if you
  automate answers, so you only ever answer questions you know. **An absent
  kind is not permission**: it means the service did not recognise the prompt,
  and answering it blind is how an automation kills a session.
- An unrecognised prompt with nothing else corroborating it (no `kind`, and no
  runtime footer on screen) is not reported the instant it appears. The tmux
  driver holds it at `unknown` until the same screen has been seen unchanged
  for a short grace window, because a single such read is exactly the shape
  that misclassified ordinary transcript text as a menu and refused input to a
  healthy session. A `waiting_input` with no `kind` may therefore lag its first
  appearance by a couple of seconds — poll again rather than treating the
  interim `unknown` as a dead end. The window runs from when that screen
  first appeared, not from your previous read, so polling faster never delays
  it, and once reported an unchanged screen stays reported. While it is held,
  `keys` accepts that screen too; it refuses only once a read has given you
  the prompt and its nonce.
- The review screen that ends every multi-question dialog ("Ready to submit
  your answers?" over `Submit answers` / `Cancel`) is recognised by that fixed
  chrome and reported at once — still with no `kind`, because submitting answers
  somebody else chose is not a question to automate.
- A **multi-select** question reports `multiSelect: true`, and its checkbox
  options carry their tick in the text (`[ ] Apple`, `[✔] Cherry`). Answer it
  with a set, never with `choice`:
  ```
  { "choices": [1, 3], "nonce": "…" }   → submitted, "…moved on to the dialog's review screen…"
  { "choice": 1, "nonce": "<the review screen's nonce>" }   → submitted: now it is answered
  ```
  The first call leaves exactly those boxes ticked and moves the dialog one step
  on; the answers reach the agent only on the second. Re-read state between the
  two — the review screen is a new prompt with a new nonce. On `unknown`, read
  again and resend the SAME set with the new nonce: it names the end state, so
  nothing gets flipped twice. Send `choices` only when `multiSelect` is true.
- A question an agent asked also offers a **free-text row**, and reports
  `freeText: true` when the driver can answer through it. That is how a person
  says something the agent did not list:
  ```
  { "text": "a flat white, oat milk", "nonce": "…" }   → submitted, "…typed 22 byte(s) into the free-text row…"
  { "choices": [1, 3], "text": "durian", "nonce": "…" }   → on a multi-select question: those boxes AND the text
  ```
  Draw `freeText` as an input rather than as another option — the row is still
  in `options`, at its own index, so nothing you already number by moves — and
  send `text` only when it is true (an older peer would ignore it and accept the
  highlighted option). Never send an empty answer: the runtime reads an empty
  free-text field as declining the whole dialog, so the service refuses it with a
  `400`. If a receipt is `unknown`, read the state again: a row that already holds
  text is no longer offered, and is answered with `choice` (`0` accepts what is
  there) or `cancel`. Keep answers short — a long one can push the dialog out of
  what the driver reads.
- A **tabbed** dialog reports `prompt.tabs` (each question's `header` and
  `state`: `answered`, `current`, `pending`) and `prompt.tab` (0-based index of
  the current one), so a card can show progress before the dialog has been
  answered tab by tab. Only the current tab's question and options are on the
  screen, so only they are in the prompt. Both are absent on a single-question
  dialog and on a peer that predates them — draw the single-question card then —
  and `tab` is absent on the review screen, where none of the tabs is current.
  `tab: 0` is the first tab, not a missing value.
- **Never blindly accept the default.** A real prompt in the wild highlights
  `No, exit` — a client that reflexively confirms would kill the session it was
  trying to start. That is why `options` and `selected` are both on the wire.

The receipt tells you what happened: `submitted` means the prompt cleared;
`refused` means the session was not at a prompt (a keypress sent to a busy
session is swallowed by whatever it is doing); `unknown` means the answer went
in but the prompt did not clear — re-read rather than retry blindly.

### When the dialog is one nobody recognised

Some full-screen dialogs are navigated with arrow keys and have no `prompt` for
you to answer — `respond` will refuse them, correctly, because it has nothing to
answer by index. For those:

**Check `deliversRawKeys` in `/v1/runtimes` first.** A driver over a runtime
with no screen to capture declares it `false` and this endpoint answers `501
unsupported` rather than approximate a keypress — degrade the same way you
would for any other missing capability, not by retrying.

```
POST /v1/machines/{machine}/sessions/{id}/keys?expect=<screenDigest>
{ "key": "Down" }
```

- **`expect` is required**, and it is `state.screenDigest` from the read you
  just did. It does the nonce's job: if the screen moved since you looked, you
  get a `409` rather than a key applied to a screen you never saw.
- **Seven keys**: `Up` `Down` `Left` `Right` `Enter` `Escape` `BTab`. Nothing
  else — no characters (that is `input`), no control keys (`C-c` is `interrupt`,
  `C-u` is `discard`), no plain `Tab`. `BTab` is Shift+Tab, and it is not a dialog
  key: see *Changing a session's permission mode* below.
- **One key per request.** `Down Down Enter` is three calls with a read between
  each, because after the first key your digest describes a screen that no
  longer exists.
- **`submitted` means the screen changed under the key.** `unknown` means it did
  not — either the dialog swallowed it, or it had nothing to do. Do not read
  `unknown` as failure and hammer it; re-read and decide.
- It needs the **`keys` grant**, which is separate from `send` and denied by
  default. If you are cutting over to this API for everything, ask for it in the
  same breath as the rest — otherwise your first modal is where you find out.

Prefer `respond` whenever a `prompt` is present. It verifies a nonce, picks by
index, and tells you which option it took; arrow keys can do none of that, and
the endpoint refuses rather than let you trade it away by accident.

### Changing a session's permission mode

On an idle composer the runtime cycles its permission mode on Shift+Tab, and
`BTab` is how you press it (muster #188). Read this before you build a mode
dropdown on it:

- **It is one press, not "go to mode X".** `POST …/keys` with `{"key":"BTab"}`
  presses once, and the receipt never says which mode the session is in
  afterwards. `submitted` means the screen changed under the key; `unknown` means
  it did not (the press was swallowed, or there was nothing to cycle). The mode is
  in `state.permissionMode` (muster #194): one of `default`, `acceptEdits`,
  `plan`, `auto`, `bypass`, or `unknown`. A control that must land on a named mode
  presses, reads that field, and repeats — re-reading `state` for a fresh
  `screenDigest` before **each** press, the same one-key-per-request rule as
  every other key. The loop, precisely:
  1. Read `state`. If `permissionMode` is the target, stop.
  2. If it is **absent**, nothing was read (a dialog owns the screen, or this
     driver does not look — check `observesPermissionMode`): read again, do not
     press. If it is **`unknown`**, stop and tell the user: the mode indicator
     was there and named no mode you know, so you cannot say what the next press
     does.
  3. Otherwise press once with `?expect=<the digest from that read>`, then go to 1.
  4. **Bound it.** The runtime's cycle is a ring of at most six stops and skips
     the ones a build or model does not offer (`auto` is not on every model), so
     never count presses; give up after a full lap (six presses) without meeting
     the target — it is not offered here.
- **It can escalate the session, and so can anyone holding `keys`.** Accept-edits
  and auto let the agent act unattended with less asking; the runtime's cycle
  order decides whether a press loosens or tightens, and this API does not
  separate the two. Do not put a `BTab` control in front of anyone you would not
  trust with `keys`, and do not hand `keys` out as if it were only about arrow
  keys.
- **Where it works.** An idle, empty composer — the one place the arrow keys are
  refused. It is refused where any key is: unsent text in the composer, a prompt
  `respond` can answer, a composer taller than the capture window. A screen you
  did not just read (`expect` missing or stale) is a `409`.
- **An older service answers `400`** and names the keys it does deliver. Treat
  that as "this machine cannot do it yet", not as a fault to retry.

## 7. Driving a session

```
POST   /v1/machines/{machine}/sessions/{id}/input       { "text": "..." }   (submit defaults to true)
POST   /v1/machines/{machine}/sessions/{id}/interrupt
DELETE /v1/machines/{machine}/sessions/{id}?startedAt=<from the read>
POST   /v1/machines/{machine}/sessions                  (create; see below)
```

**`input` can legitimately refuse.** If a human has typed something into the
composer and not sent it, delivering more text would corrupt their line, so the
service refuses and tells you. That refusal is a feature; do not retry through
it. Check the receipt rather than assuming success.

**Delivery is confirmed before submit** — and that is narrower than it sounds.
The service checks the text RENDERED before it submits, so text that never
arrives gets an `unknown` naming it rather than a cheerful success. What the
check does not cover is the submit itself.

**So read `queued` as exactly this: the service pasted your text, watched it
appear, and issued a submit it cannot verify.** It is not a promise the line
went in. A `queued` receipt does not entitle you to assume the composer is now
empty — if that matters to you, re-read the session's state.

The gap is not theoretical. A session that has been created but has not finished
starting will accept the paste, render it, and drop the submit, leaving the text
sitting in the composer — measured at two runs in three — and the receipt is
`queued` throughout. **Wait for a session to report `idle` before sending to it**,
rather than sending as soon as `create` returns.

**Exception: a session you just created with a `prompt`.** That advice is for a
session you did not create — for one you did, `idle` is not the signal to wait
for, and reading it as one is exactly the mistake muster #86 measured: a
session created with a `prompt`, polled ~12s later, read `idle` with
`"interface painted, composer empty, no turn yet"` — the correct classification
for "nothing was ever sent", and indistinguishable from "the create-time prompt
has not been delivered yet" without a further field. The caller concluded the
prompt was lost and re-sent it through `input` four times. Read `promptDelivery`
instead ("Creating a session", below): absent means the create carried no prompt at all; present
with `outcome: null` means it is still in flight and `idle` says nothing about
it; present with an `outcome` means delivery has resolved, the same vocabulary
`send`'s own receipt uses. Never re-send a create-time prompt through `input`
while `promptDelivery.outcome` is still `null`.

One consequence worth planning for: this class of stranding leaves no record, so
a plain retry with `resumeIfStranded`/`replaceIfStranded` used to not apply to it
at all, and every later `send` to that session was refused for holding unsent
input with no automated way back in — re-reading state and calling `discard`
yourself was the only move. Colab-fleet #135 closed that gap, and #180 bounded
it with the draft rule, described below.

**A live lane is the session's only input path (#257).** Read the session
first if you can: `delivery.clientConnected: true` means its lane is live, and
then every `input` is delivered through that lane or refused with nothing
written. It never falls back to the terminal. So on such a session **send the
text and nothing else**: `{"text": "…"}` is delivered (and `submit` defaults to
`true`, so it is submitted). Do **not** add `route: "terminal"`, `submit: false`,
`resumeIfStranded` or `replaceIfStranded`: each is `refused`, and the `reason`
names the lane. If text from an earlier terminal delivery is still in the
composer, clear it with `discard` (with `expect`) and send again; `discard`,
`keys` and `respond` are unchanged and are how you handle dialogs. A refusal
here is information, never a cue to drive the terminal yourself: a caller that
types into the multiplexer when this service refuses defeats the rule and can
double-deliver.

**Choosing a route (#184, #257).** `input` takes an optional `route`: `auto`
(the default — send nothing and you get it), `terminal` or `inbox`. Under `auto`
the service labels a sender that is not a human relay (`[from: …]`, with the
principal you authenticated as if you send no `from`), and the session's own
state decides the path: its live lane when it has one, else the terminal. A
principal holding the `human-relay` grant arrives unlabelled, as the user's own
turn, and its `auto` crosses a peer relay as `auto` so the machine that owns the
session decides. `auto` never tries the inbox. Read `delivery.route` on the
receipt to see which path carried it. Reach for `route: "inbox"` only when the
message must not go through the composer: it is **refused, with nothing
written**, when the session cannot take it — never downgraded. And do not try to
look like a human: no header, no `from` and no `relayOfHuman` changes which path
a message takes; only the principal's own grant does.

**Two behaviours changed in #257 (breaking for some callers).** An absent
`submit` used to mean `false`: a caller that relied on that to stage text
without submitting must now send `"submit": false`. And a caller that asks for
the terminal, `submit: false`, or a resume or replace on a session with a live
lane is now refused.

**If `send` answers `unknown` on `delivery.route: "terminal"` (or with no
route), retry it with `resumeIfStranded: true`** (on a session with no live lane; a live lane refuses it, above). That
outcome means the text reached the composer but could not be confirmed in time,
so it is sitting there unsent — and a plain retry is refused, correctly, by the
rule that stops anything appending to a busy composer. When the service can
establish from its own record that the composer holds the text it delivered, the
resume submits it — send the *same* text: this finishes one delivery, it does
not start another.

**A `queued` answer is not a promise that a turn started.** When the receiving
session was busy, the runtime may only have queued the text, and can hand it back
to the composer unsent (a queued message returned after the running turn was
interrupted or failed). The receipt's `reason` says when this applies. The session
then reads `waiting_input` with `waitingOn: "unsent-input"` and
`strandedDelivery: true` — the service's own record says that text is a message it
delivered, not a person's draft. Finish it the same way as an `unknown`: send the
same text, with the same `from`, and `resumeIfStranded: true`. A sender that wants
to be sure of an important message watches for that state after a `queued`.

**If it answers `unknown` on `delivery.route: "inbox"`, do not retry — wait and
read the transcript.** Nothing is sitting in a composer: the message was written
to the session's inbox and the receiver's transcript has not recorded it yet, so
it may have arrived, may be held, or may have been dropped. The service will not
send it again: for 30 minutes, or until it is recorded, the same text from the
same sender to the same session is answered with the same `unknown` on every
path, `resumeIfStranded` included, because sending it again could deliver it
twice. Different text is a different message and goes through.

**The draft rule (#180): the service never clears or submits composer text it
cannot prove is its own, unless you prove you saw it.** When no live record
backs the composer — the create-time-prompt case above, a record that outlived
`strandedRetention` (30 minutes), or a session this service never touched —
the flags alone are not enough, because the text there may be a person's
draft:

- If the service placed that text itself and its record merely lapsed, it
  still recognises the text (it keeps a longer-lived record for exactly this),
  and `resumeIfStranded` clears it and delivers your text.
- Otherwise, read the session, and if the composer's content is yours to
  replace, send again with `replaceIfStranded` and `expect: <composerDigest>`
  from that read. The service clears exactly what you saw and delivers your
  text; if the composer changed since you read it, the call is refused and
  the text kept.

Send whatever text you actually want delivered. The composer's own foreign
content is discarded, never resubmitted. A bare `send` with **neither** flag
set still refuses untouched, exactly as it always has.

**To clear text without sending anything, use `discard`.** `POST …/discard?expect=<composerDigest>`
— the digest comes from the same read that told you `waitingOn: unsent-input`.
It is refused if the composer changed since (somebody may be typing), and
refused if you omit it. Reach for this when you want the composer emptied and
have nothing to deliver in its place; reach for `resumeIfStranded`/
`replaceIfStranded` on `send` instead when you do — closing the session to be
rid of a line is not a trade anyone should make either way.

**If `discard` comes back `409` naming this exact residue proven futile —
not the ordinary "unchanged, first pass" 409, a distinct message saying a
prior full clear pass already left it unmoved — retry the SAME call with
`&force=true` added** (`POST …/discard?expect=<the same composerDigest>&force=true`),
muster#136. This reaches for a stronger, character-budgeted clear
mechanism past whatever defeated the ordinary pass. It is not a looser
corroboration path — `expect` is still required and still checked exactly as
before, `force` only changes what happens once that check has already passed
— so do not add `force` pre-emptively on a first call; it does nothing there
and the ordinary pass deserves the chance to work first. **Do not reach for
`DELETE` here** — closing the session to clear one stuck composer destroys
the conversation, the bridge, and any in-flight work along with it, and
`force` exists specifically so that trade is no longer necessary.

**`rename` changes the id.** `POST …/sessions/{id}/rename` with `{"name":"…"}`,
and carry `?startedAt=` exactly as you would for a delete — renaming the wrong
session does not fail loudly, it succeeds and leaves that session named after
somebody else's work. Afterwards, **the id you hold is stale**: use the new one,
and if you are subscribed, re-key on `session.renamed` rather than concluding
the old id died.

Read the `202` body's `title` field (muster#222) if you ever reconcile a
session's name against anything the runtime itself reports — its own title is
brought along automatically, but honestly: `"pending"`/`"failed"` mean it has
not (yet), and re-`POST`ing the identical `name` retries just that half. Do
not build your own reconciler that trusts the runtime's title over this API's
`to` without reading this field first — that is exactly the loop muster#222
closed.

**`DELETE` should carry `?startedAt=`** from the session you read. Ids are
recyclable; without corroboration you may destroy a *different* session that
inherited the id. A mismatch answers `409 conflict`, which means your belief is
stale — re-read and decide, do not retry.

Send the timestamp exactly as the read returned it, URL-encoded (it contains
`:` and `+`):

```
DELETE /v1/machines/aurora/sessions/alpha?startedAt=2026-08-01T23%3A14%3A01%2B07%3A00
```

Omitting it is allowed and gives you a weaker guarantee — the driver
corroborates against its own sightings instead of against what *you* saw — and
the refusal tells you which one you got.

### Creating a session

```
POST /v1/machines/{machine}/sessions
Idempotency-Key: 4f1c9e2a-…          ← REQUIRED

{ "cwd":  "/work/alpha",             ← required, absolute
  "name": "alpha",                   ← what you want it called; a REQUEST, not a guarantee
  "runtime": "claude-code-tmux",     ← optional; omitted means the machine's only runtime
  "agent": "…", "model": "…", "effort": "…",   ← optional hints (see below)
  "marker": "…",                     ← optional session-type stamp appended to the name
  "remoteControl": true,             ← optional; OMITTED IS NOT false (see below)
  "prompt": "first instruction",     ← optional, delivered once the agent is ready
  "trustCwd": true,                  ← optional consent to the folder-trust question (see below)
  "consents": ["folder-trust"],      ← optional, the general form of the line above; also "external-imports"
  "env": {"MY_SESSION_ID": "…"},     ← optional, delivered out of band — never argv
  "resume": "<conversation id>",     ← optional, continue a prior conversation
  "conversationId": "<caller uuid>", ← optional, START a new one under this id; mutually exclusive with resume
  "permissionMode": "bypass",        ← optional, needs the send grant
  "contextRef": "/abs/path" }        ← optional, a PATH — never inline content
```

```json
→ 201 { "machine": "aurora", "id": "alpha", "name": "alpha",
        "runtime": "claude-code-tmux", "cwd": "/work/alpha",
        "pins": { "model": { "requested": "opus", "honoured": null, "evidence": "…" } }, ← only for a requested pin
        "runtimeSurface": { "known": null, "evidence": "…" },  ← usually still resolving at create time
        "promptDelivery": { "outcome": null, "evidence": "…" },  ← only when you sent a prompt
        "state": { "status": "starting", "confidence": "inferred", … } }
```

**Read `id` out of the response and key everything afterwards on it.** Do not
assume it equals the `name` you asked for. On the multiplexer driver they
happen to coincide today; that is a property of one driver, not of the API, and
a client that hardcodes the equality breaks on the first driver that sanitises
or de-duplicates names.

Fields the body does **not** have: `machine` (it is in the path) and `id` (it
is the server's answer, not your input — sending one is ignored, not honoured).

`agent`, `model` and `effort` are **hints**. A driver that cannot pin one says
so rather than silently substituting a default — check `supportsPin` in
`/v1/runtimes` before relying on any of them. **A value that WOULD be
mistaken for a flag is refused outright at creation** (`invalid`, naming the
field) rather than silently dropped; a value that reaches the runtime intact
can still be defaulted or ignored there, which is what the response's `agent`
and `model` top-level fields, plus `pins`, are for (muster #84). Read the
top-level `agent`/`model` as the APPLIED values — what the driver actually
observed, empty when it does not know — never as an echo of what you asked
for; the request lives in `pins.<field>.requested`, and whether it was
honoured is `pins.<field>.honoured` (`null` until the driver can tell, which
on most drivers today is never — a driver that passes a pin on a command line
has no channel back from the runtime to confirm it, so `honoured: null` is
the honest, standing answer, not a sign of anything wrong).

**The `Idempotency-Key` header is required**, and a create without one is
rejected with `invalid` before any driver is consulted. A federated create that
times out and gets retried without one produces two agents in the same working
directory, and nothing afterwards can detect it. Same key + same body returns
the original session; same key + different body is a `409`.

**Same key + same body, but that session has since ended, is also a `409`**
(muster #234) — not the plain `201` it used to be. Before this fix, a
replay of a key whose session had already died came back with the identical
shape a live create returns, so a client that only checked the status code had
no way to learn its session was gone. Now the body carries `reason:
"replay-of-ended-session"`, the ended session's own `session` ref, and
`closedAt` (when this machine noticed, not necessarily the true end — see
`GET .../sessions/closed` for that distinction). **The fix here is different
from the general `409` advice below: mint a FRESH `Idempotency-Key` and create
again.** The old key is not cleared by this — it stays pointed at the same
dead session for its normal retention, so retrying it gets the same 409 again,
not a new session.

On the ordinary `409` (a different body, or a destructive call whose belief is
stale), the guide's advice is "re-read and decide", and the decision is
genuinely yours — but it is a narrow one. The conflict means the session at
that id is not the session you looked at. Re-read it: if the working directory
and start time describe something you did not mean to touch, **stop** and
surface it, because the alternative is destroying a stranger's work. Do not
loop retrying with a fresh `startedAt` each time; that turns a safety
mechanism into a slower way of doing the dangerous thing.

Deleting a session already reported `dead` is harmless — the driver is
reconciling something it has already lost, and you get the same answer. Skip it
if you like; do not build logic that depends on the distinction.

`state.status` on a fresh session is usually `starting` — the agent is not
ready yet. If you passed a `prompt`, the service delivers it once the runtime
is up; you do not need to wait and send it yourself. A question the runtime
puts up on the way there **delays** that delivery, it does not cancel it: the
service keeps waiting, and the prompt goes in once the session is receptive.

**Prefer `contextRef` to a long `prompt` — the composer is the fragile
surface, not a load-bearing one.** `prompt` is capped by this machine's
`maxInputBytes` (1024 bytes by default; over it the create is refused
outright rather than accepted and left to strand — see api-http.md §3.1 and
§3.2), and even under that cap it still travels through the same composer
keystrokes `input` does. Measured on live fleet operation: a roughly
1800-byte brief stranded in a composer and the prompt was simply lost
(muster #128). None of that fragility is a property this API needs you
to accept — `contextRef` (below) hands the runtime a PATH instead, and a
caller with more than a short instruction to give should write it to a file
and pass only a one-line pointer here: "read `.claude/briefs/issue-N.md` and
follow it end to end" is a `prompt`; the brief itself belongs in the file
`contextRef` points at, never in the composer. This is not a new mechanism,
only a choice of which existing field carries the payload — see §5.3 of
session-abstraction.md for why a path travels better than inline content in
general.

**If you passed a `prompt`, read `promptDelivery` — never `input` — to check
on it.** It carries the same outcome vocabulary `send`'s own receipt does
(`submitted`/`queued`/`refused`/`unknown` — though no driver in this fleet can
currently return `submitted` on either path; api.md's known-gaps section
tracks it), but resolves after the 201:
`outcome: null` means delivery has not resolved yet, and — this is the part
muster #86 exists to say plainly — **that is not the same fact as "no
prompt was sent."** A session polled moments after this create may well read
`idle` with "composer empty, no turn yet"; that is the correct classification
for a session with nothing sent, and it looks identical to one whose prompt
is still on its way in. Wait for `promptDelivery.outcome` to stop being
`null` before concluding anything was lost, and never re-send through `input`
while it still is — a re-send racing an in-flight delivery is refused by the
same busy-composer rule §7 already describes, but by the time you notice,
you have already sent the same instruction twice as far as anyone watching
the transcript can tell.

**While `outcome` is `null`, `promptDelivery.waitingOn` (muster #126)
tells you WHY without parsing `evidence`.** `prompt` means a dialog is up —
`state.prompt` on the same session names the question; `unsent-input` means
something is already sitting in the composer; `starting` means the runtime
has not painted an interface at all yet. Absent means this driver has not
classified this particular wait — not that nothing is wrong, just that
nothing here has an answer for it yet.

**Answer boot questions here, at creation, rather than after the session has
already parked on one.** `trustCwd` and `consents` below are the same idea as
`contextRef` above, applied to boot dialogs instead of instructions: give the
driver what it needs in the request that creates the session, before the
fragile part — the composer, and a prompt parked behind an unanswered dialog
— is ever reached. A caller that only ever answers through `respond` (§6)
after the fact is choosing the slower, more fragile path by default rather
than by decision.

**`trustCwd` is your consent to one question, about the directory you just
named.** Some runtimes ask, on the first session in a directory, whether you
trust it — and they ask before the agent can do anything at all. On a fleet
nobody is sitting at that terminal, so the session boots, parks, and does no
work; measured on a live fleet, one sat on that question for two days while
reading as an ordinary `waiting_input`.

Send `trustCwd: true` and the driver answers it for you, by finding the option
that grants trust and choosing it by index — never by accepting the highlight,
which on a neighbouring boot screen means `No, exit`. If the wording has moved
far enough that the granting option cannot be identified unambiguously, nothing
is answered and the question stays on screen for a human.

Two things it is not:

- **Not a standing permission.** It is scoped to the `folder-trust` question on
  the one session being created. Nothing else is auto-answered — a
  tool-permission dialog or the bypass-acceptance screen asks something else,
  and neither is reachable this way.
- **Not free.** It requires the `send` grant on top of `create`, because it
  produces a keypress. A principal that may start sessions but not drive them
  gets `unauthorized`, and that is the point: otherwise `create` becomes a
  second way to answer dialogs that nobody reviewing grants would notice.

If you do not send it, nothing changes: the driver answers nothing, and
`state.prompt` reports the question in full for you to answer through
`respond` (§6) whenever you decide to.

**`consents` is the general form**, and `trustCwd` is now shorthand for
`["folder-trust"]` — both work, and sending both is agreement rather than
conflict. A consent names a question, so it answers that question and no other:
`["folder-trust"]` leaves an imports question standing, and the reverse. Send
only `consents` — no `prompt` and no `trustCwd` — and it is still answered.

**`external-imports` is the runtime's second question about a directory.** When
the instruction files its working directory loads import a file from outside it,
the runtime asks "allow external imports" before the agent can do anything, and
holds the session on it exactly as it does the trust question — with no bridge
and no conversation id, so a host cannot even link to the session. Its
highlight defaults to the decline; the driver picks the affirmative row by
index. It is a wider agreement than folder trust: you vouch for the directory,
not for a list of files you never saw, and the question's own warning is never
to allow it for a repository you do not own. Consent to it only for a directory
that is yours.

On a machine whose operator configured `trustRoots`, you rarely need to: every
repository and worktree under a root has both questions answered ahead of time,
whoever started the session and whether or not it went through this service. A
directory outside every root still shows the question, and
`state.prompt.kind` reads `external-imports`.

**`feedback-review` is a person's decision, and a client only carries it.** When
an agent drafts product feedback the runtime paints a card above the composer:
`1 to review · 2 to send · 0 to dismiss`. On a short pane — the 24 rows a created
session gets — the card pushes the composer off the bottom, and until someone
answers it nothing can be delivered: `send` is refused, naming the card, and the
state is `waiting_input` with a prompt of this kind. Show it to a person; do not
answer it for them, because *send* puts a draft of the session's own words in front
of a third party. `respond` takes `choice` 1, 2 or 3 (review, send, dismiss — 3 is
delivered as the key `0`) and **requires the nonce**, since the options never
change from one draft to the next. It refuses `cancel`. Choosing `send` does not
send: the runtime asks to confirm — a second guard that a person gives, not you —
and the receipt says what the session shows now. On a taller pane the card blocks
nothing: the session reads `idle`, and you can send to it. (A message that is only
`1`, `2` or `0` would be read by the card as its shortcut, so `send` refuses one
while the card, or the runtime's question about turning drafts off, is up.)

What comes after the card is a person's too, and none of it is a prompt you can
answer. The confirmation, the `Sending…` line and the send error read `unknown`
when they hide the composer, with the state named in `evidence`, and `send`,
`keys`, `discard` and `respond` refuse by name; the draft stays queued after an
error. `review` opens the runtime's own feedback panel, which replaces the
composer: `unknown`, every delivery refused, because Enter there opens or sends a
draft. **`keys` with `Escape` is the one key accepted** — it closes the panel, steps
back from the confirmation, and dismisses the card or the error, and the receipt
says which. Escape on the card removes only the card; the draft is not discarded.

The other consentable question is `bypass-permissions`, the
acceptance screen a non-default `permissionMode` raises — and it comes with a
condition worth understanding, because it explains something you will otherwise
find puzzling in `state.prompt`.

**That screen never carries a `kind`, and the consent still works.** Read out of
the runtime's own binary, its options are `Yes, I accept` and `No, exit`; the
words that identify it — "Bypass Permissions mode" — appear only in its
question. This service classifies prompts from their OPTIONS and never from
their question, because the question is written by the agent and is therefore
something an agent can forge: a ship decision was once labelled with this very
kind because an agent had typed "No auth bypass" into its own prompt.

So the driver identifies that screen a different way — by the fact that it
passed the flag which raises it, to this session, moments earlier. Consent to
`bypass-permissions` therefore only takes effect **when the same request also
set `permissionMode`**. Sent on its own it does nothing, deliberately: without
that provenance the only evidence available is generic wording, and generic
wording is how an automation accepts a dialog nobody has seen.

The consequence for you: an unclassified boot screen may well be this one, so
**do not read an absent `kind` here as "some prompt I have never heard of"** the
way you safely can elsewhere. It is the one screen whose absence of a kind is
expected rather than informative.

`resume-chooser` is deliberately **not** consentable and a create asking for it
is refused. The others ask yes/no about something you described in your own
request; the chooser asks *which* conversation, and its options are summaries of
prior sessions with nothing in the text identifying yours. Read `state.prompt`
and answer that one by index.

### Giving the session what it needs to be itself

These fields exist because a session created through this API used to be a
lesser session than one a supervisor started directly — and the difference did
not show up at creation, only later, somewhere else.

**`env`** is how the agent inside identifies itself to your tooling: which
session it is, where its context is staged, which bridge it is re-attaching to.
Values are staged in a 0600 file the session reads and unlinks — **never in a
command line**, because the payload most likely to be a credential must not be
the one exception to that rule. Two bounds, both refusals rather than
truncations: names must look like variable names, and a value may not contain a
newline or NUL (the format is line-oriented, and a newline would arrive as a
second variable invented out of your value).

**`resume`** continues a prior conversation. Do not confuse it with
`supportsResume` in `/v1/runtimes`, which reports whether sessions survive a
service restart — same word, different question.

**`conversationId`** (muster #224) starts a NEW conversation under a UUID
*you* choose, instead of one the driver derives afterward — so the `201`
already carries `conversation` (`known: true, source: "captured"`) and you
never have to poll `GET .../sessions/{id}` waiting for the runtime's own
record to show up. Mutually exclusive with `resume` — send one or the other,
never both (`invalid` if you do). Must be a UUID, and refused `invalid` if it
already names a conversation this machine has on record for the same `cwd` —
two conversations must never share one transcript. A runtime with no way to
launch under your id, or a peer on a build old enough to not know the field,
answers `unsupported` rather than silently starting a conversation under an id
of its own. If a later read ever disagrees with the id you asked for, that
shows up as a fresh, named mismatch on `conversation` — never a quiet
overwrite.

**`permissionMode`** takes `bypass`, or `default` for the runtime's ordinary
mode. `bypass` needs the `send` grant like a consent does: a session in that mode
acts without asking, which is a larger authority than starting one. An
unrecognised value is refused, not passed through to the runtime. When you
`resume` a conversation and send neither, the service launches the new session in
the mode the old one last ran in — `bypass` included — and says so in the
response's `carried`; send `default` to opt out. See `docs/api.md`, *Resume
carries the launch posture* (#256).

**`mcpConfig`** is a list of absolute PATHS to tool-server configuration files,
one flag per entry. Paths and not content, for the same reason `env` stages
values out of band: these files usually hold the credentials their servers
authenticate with, and content on a command line is readable from every process
table on the machine. Write it to a 0600 file and name the file.

A path the daemon cannot read is a refused create, not a started session — the
runtime would come up looking perfectly healthy and unable to do the work you
created it for. On a create aimed at a peer, the paths are the PEER's; they
travel verbatim and the peer checks them. Nothing here reads or validates the
contents.

It needs the `send` grant on top of `create`, like `permissionMode`: these
configurations name servers the session will launch, and starting a session that
also starts those is a larger authority than starting a session.

**`settings`** is a JSON object handed to the agent CLI at launch as
`--settings '<json>'`, for a switch the CLI reads only at boot and from no
environment variable — so `env` cannot carry it. The case it exists for: a
bypass session that must accept inbound cross-session messages,
`"permissionMode": "bypass", "settings": {"crossSessionInbound": "accept"}`.
Any key is accepted with `permissionMode: "bypass"`; without it only
`crossSessionInbound` is (any other key is a `400` naming it), which is what lets
a relaunch through `resume` keep the setting a default-mode session booted with.
It must be a JSON object of at most 4096 bytes, and needs
the `send` grant like `permissionMode`. It lands on a command line, so it is for
launch-time switches, never a credential.

One rule shared by everything that reaches the agent's own argv — `agent`,
`model`, `effort`, `resume`, `mcpConfig`: **a value may not begin with `-`**, or
the CLI reads it as a flag rather than as your value. `conversationId` lands in
the same argv too; its UUID-shape check (refused `invalid` otherwise) already
guarantees that.

Pass context by path (`contextRef`), never inline. Prompts and context never
reach a command line.

**A created session is meant to be the same kind of session the machine's own
launcher makes** — reachable from a remote client, carrying the environment its
agent needs to call a tool, named by the machine's conventions. You do not have
to ask for any of that; it is what you get by not opting out.

**`remoteControl` omitted is not `remoteControl: false`.** Omitted means "give
me a first-class session". Send `false` only when you deliberately want one that
cannot be reached remotely. This asymmetry is deliberate: a plain boolean would
make every client that has never heard of the field silently create sessions
nobody can reach from a phone.

**Whether that request actually succeeded is `runtimeSurface`, read later.**
The runtime registers the surface asynchronously, after the process starts, so
the create response usually shows it still resolving (`known: null`) — poll a
read and check again rather than treating the create response as the final
word. `remoteControl: false` is the one case that resolves immediately, to a
settled `known: false`, because there was never anything to wait for.

**`marker` stamps the session type onto the name**, because on some substrates
the name is the only channel there is — it is what listings, remote clients and
humans all see. The driver carries a marker and never stacks one: a name that
already ends in a marker keeps the one it has. What a marker *means* is yours;
the service has no vocabulary of its own.

**Group by `marker` on the session, not by the end of `name`.** Every read
returns `marker` — the marker your create applied — so you never need a suffix
test that cannot tell a marker from a name that merely ends in the same
characters. It is read-only, it is not a label, and it survives a rename.
Absent means the machine holds no record of one (no marker sent, the name kept
a different marker it already had, or the session predates the field) — not
that the session has no type.

**`name` may be rewritten, and this is when it matters.** The driver sanitises
it, and numbers it if a session of that name is already live — so asking twice
for `alpha` gives you `alpha` and then something else. That is the reason the
advice above ("read `id`, do not assume it equals `name`") is not theoretical.

### Did this session get what a launcher-created one gets?

```
GET /v1/machines/{machine}/sessions/{id}/environment
```

```json
→ 200 { "known": true, "shell": "…", "login": true, "interactive": true,
        "names": ["HOME", "PATH", "…"], "path": ["/usr/local/bin", "…"],
        "serviceNames": ["…"], "servicePath": ["…"] }
```

`names` are variable **names only** — never values, because the environment
being described is the one holding credentials. `path` is the exception, and a
deliberate one: a search path is not a secret, and PATH is what drifts.

The useful reading is the **difference** between `names` and `serviceNames`: the
latter is the service's own process, so what is in the first and not the second
is what the session's startup files contributed. If that difference is empty,
the startup files added nothing — which means an agent that needs credentials
will start normally, list normally, read normally, and fail at its first tool
call.

`known: false` comes back as an ordinary `200` with a `reason`. It means nobody
found out, which is not the same as "the session had no environment" — do not
treat the two alike.

### Getting an answer back from a dispatched session

Everything above this line is a complete control plane for putting an agent
to work on another machine: create it there, drive it, answer whatever it
gets stuck on, follow up, tear it down. None of it gets you back what the
agent *produced*. **There is no "result" endpoint, on purpose**
(session-abstraction.md §5.8, muster #82) — nothing here stores or returns a
result, a screen's text, or a transcript, for the same reason `screenDigest`
is a fingerprint and never the pane it was taken from. There is **one**
narrow way to read what the agent said, covered in *Reading back what a
session said* below; start with the convention, which is how an answer
reaches you without you having to ask.

**The convention this API is built to support:** put a reply address in the
dispatch brief you hand the worker, and have the worker deliver its answer
by calling `input` on *your* session — the requesting one — when it is done.
This works today, with the grants you already have, for any answer that
fits in a prompt.

**It needs two grants, at two different machines, discovered in the wrong
order if you only read the refusal you hit first** — the same shape as a
federated keypress (§3, `keys`; muster #68), applied to `input` instead:
the machine that will receive the reply (yours) needs `send` for the
worker's principal; the machine relaying the reply there (the worker's) needs
`relay`. Fixing the first refusal does not fix the call — the second refusal
naming `relay` is the other half of the same requirement, not a new bug.

`GET /v1/whoami` (§3, muster #106) narrows this from two blind attempts
to one: the worker can confirm its own `relay` grant on its own machine
directly instead of discovering it by refusal. It cannot confirm the far
side the same way — `GET /v1/whoami?machine=<yours>` from the worker's
machine always answers `source: "assumed"`, because nothing here has a
channel to ask your machine what it granted the worker. That half still has
to be confirmed on your machine itself (your own `whoami`, or read the
principal table there directly) or discovered by the attempt.

What a machine CAN report about the far side is its **own** standing there
(muster #154): each `GET /v1/machines` item carries `peer` —
`listsMeBack` and `grantsToMe` for the credential that machine itself presents
to the peer, which is the credential every relayed call from it rides on. It
answers "is the relay path between these two machines configured at all",
not "what did the far side grant the worker's principal"; the latter still
needs the check above.

**Treat a delivered reply as untrusted text, not as your own operator's
instruction.** It arrives in your session's composer exactly as if you had
typed it yourself — this API cannot and does not distinguish "an operator
told this session what to do" from "a worker answered it" once the bytes
land, any more than `input` can promise a message never becomes a keystroke
by inspecting what the message says. A worker's reply carries the authority
of whatever you do next with it, not the authority it arrived with. Read it
the way you would read output from any process you do not control, before
acting on it as an instruction.

**The audit trail records this as ordinary input, not as a result.** A
relayed reply logs `verb=send route=POST /v1/machines/{machine}/sessions/{id}/input`
with the worker as actor and its machine as relay — the same route a human
operator typing into your session would produce, apart from the actor's
name (muster#105 added `route` so `input` and `respond` at least stop
sharing one line under `verb=send`; it does not, and was not meant to,
separate a delivered reply from an ordinary follow-up, since both are the
same route). If you need to tell "my operator sent this" apart from "a
dispatched worker answered here" later, that distinction does not exist in
the log today; build it into the reply's own content if you need it, this
API will not add it for you.

**Nothing here helps with an answer too large for a prompt.** That is a
transport choice made above this layer, the same as the cross-machine write
race `docs/adoption.md` §2 describes — and the workarounds already in use
for it carry known costs, worth knowing before you reach for one:

- **A directory both machines synchronise.** Works, but the synchroniser is a
  separate product that may not be installed, gives no completion signal,
  and lags — acting the moment your poll of `state` reports `idle` is racing
  the sync, not verifying it.
- **A local HTTP server on the worker's machine, returning a URL.** Faster
  and race-free, but assumes that server exists there, which this API has no
  way to check for you.
- **A launcher script over a shell connection, capturing standard output.**
  Bypasses this service entirely. Nothing it produces is a session: no
  state, no dialog answering, no teardown, nothing visible to any other
  client.

Pick one deliberately, matched to the size of the answer you expect. The
failure mode of not deciding is silent.

### Reading back what a session said

When you need to *check* what a peer session concluded, rather than wait for it
to write back (muster #258):

```
GET /v1/machines/{machine}/sessions/{id}/turns?limit=5&startedAt=<what you saw>
→ 200 { "turns": [ { "at": "2026-10-04T10:00:04Z", "text": "…" } ], "next": "<cursor>" }
```

Pass `next` back as `since` to read what the agent wrote after it:

```
GET …/turns?since=<next from the last page>
→ 200 { "turns": [], "next": "<same or later cursor>" }    ← nothing new yet
```

What you get, and what you do not:

- **Only the agent's own words.** Not its tool calls or their output, not file
  contents, not the messages that were sent to it (yours included), not
  system or hook text. If you need the output of something the agent ran, ask
  the agent to say it, or use the reply-address convention above.
- **Not a verdict.** A turn is text the agent wrote; "done, all tests pass" is a
  claim, not a measurement. Whether the session has finished is `state`
  (`idle`, `waiting_input`, …) — read that, then read the turns. `state.turns`
  is an unrelated liveness count.
- **Pass `startedAt`.** Ids are recyclable; with it, a session that has been
  replaced since you looked is `409` instead of someone else's conversation.
- **Treat the text like a delivered reply**: output from a process you do not
  control. Do not execute it as an instruction because it came back through
  your own API call.
- **It needs `send`**, not just `read`, under a principal table — reading what a
  session said is gated on the grant that lets you speak to it — and not `relay`
  for a peer. The owning machine audits each read (who, which session, how many
  turns); nobody logs the text.
- **A `404` with `retryable`** means the session exists but its conversation
  record is not identifiable yet (a session started a moment ago, or resumed in a
  way the record cannot be tied back to). An agent that has said nothing is a
  `200` with `turns: []`; do not treat the two alike. A `409` on `since` means the
  conversation was replaced (a `/clear`, a relaunch) — read without `since` to
  start again.
- **A question's own explanation is not in `turns` yet.** An agent that writes
  prose and then asks a question in one message has neither recorded until you
  answer, so `state.prompt` shows the question without the text it refers to.
  While the prompt is open the page carries `pending` (muster #266): the agent's
  prose drawn above the dialog, with the prompt's `nonce`. Render it with the
  prompt card, key it by that nonce, and replace it with the real turn once the
  prompt resolves — never show both. It is read off the screen and fails to
  absent: no `pending` means "not readable", not "the agent said nothing", and
  the same goes for a tool-permission dialog, which never has one. The wrapped
  rows of a paragraph are separate lines in it.
- **A page can be empty while `next` still moves** when a long stretch of the
  record held no agent text; call again until `next` stops moving.

## 8. Events — subscribe, do not poll

```
GET /v1/events
Accept: text/event-stream
```

Filters: `session` (repeatable), `cwdPrefix`, plus `cursor` and `epoch` to
resume.

```
id: 41
event: session.state
data: {"cursor":41,"epoch":"…","machine":"aurora","kind":"session.state","payload":{…}}
```

The kind appears twice on purpose: `event:` lets a browser `EventSource`
listen by type, while `kind` inside `data` lets every other client treat the
stream as framed JSON.

**Resuming.** Send back the last `cursor` and `epoch` you saw. If your cursor
is too old, or the service restarted (different `epoch`), you receive
`control.resync` — which means *your view is stale, refetch the list*. It never
resumes you silently from an arbitrary point, because a subscriber that
believes it has complete history and does not is unrecoverable.

A browser gets this free: `id:` makes `EventSource` send `Last-Event-ID` on
reconnect, and the server honours it.

### What produces an event, and what does not

This is the part to get right before you build a UI on it.

| happens | event? |
|---|---|
| a session appears | `session.created` |
| a session ends | `session.closed` |
| a session's **status** changes (idle → working, → waiting_input) | `session.state` |
| a session's prompt, nonce, `waitingOn`, composer digest, quota or `lastTurn` changes | `session.state` |
| the agent prints output, the spinner ticks, the screen scrolls | **no** |
| the `evidence` prose is reworded | **no** |
| this service's own view of a machine goes deaf, and comes back | `source.status`, then `control.resync` with `feed_gap` |
| a machine becomes unreachable | `source.status` |
| this machine's **account** starts or stops refusing work | `machine.quota` |
| this machine's local **credential material** changes | `machine.account` |

Events fire on **transitions**, not on content. A session that stays `working`
for twenty minutes produces one event at the start, not a stream — which is
what you want, and also means **a quiet stream is normal**. Do not treat
silence as a broken connection; on a fleet of ~100 sessions, minutes can pass
with nothing to say.

"Transition" means any field you branch on, not `status` alone. The prompt on
screen, its **nonce**, whether the composer holds unsent text, whether the last
turn failed — all of them fire `session.state`. What does not fire is
`evidence`: it is prose for humans, the runtime repaints it constantly, and
§2.3 tells you not to parse it anyway. So the `evidence` in your mirror is as
fresh as the last real change; read the session directly if you want the
current words.

### Long-polling instead, if a stream is the wrong shape

```
GET /v1/sessions/watch?since=<cursor>&epoch=<epoch>&wait=25000
→ 200 { "cursor": 12931, "epoch": "…", "events": [ … ] }
```

Same hub, same cursors, same envelope — each entry of `events` is exactly what
an SSE `data:` line carries. Reach for it when you want a request you can retry
and log, or when your process must survive its own restart without a
reconnect state machine.

Two rules to get right:

- **Send back the response's `cursor`, not the service's.** An empty batch hands
  your own cursor straight back, because the sequence advances for everybody
  while your filter selects for you.
- **A stale cursor is a `200`**, carrying `control.resync` in the batch. Do not
  look for a 4xx; do not retry the same `since`. Re-list, and take the next
  cursor from the listing.

### Building a mirror instead of polling

If you keep a materialized copy of session state, this is the shape — and the
order matters:

```
1. GET /v1/sessions/watch?wait=0     → cursor C, epoch E    (this arms the feed)
2. GET /v1/sessions                  → items + "feed": {cursor, epoch}
3. loop: GET /v1/sessions/watch?since=C&epoch=E
         apply in order; C := response cursor
4. on control.resync (any reason)    → back to 2
```

Watch **before** you list. The service observes the fleet only while somebody
is subscribed, so the first watch is what makes the sequence live. A listing
taken before that carries no `feed` field at all — which is the service telling
you your ordering is wrong, rather than handing you a number that looks
resumable and would silently skip everything up to your first subscription.

The listing's cursor is read before its enumeration, so replaying from it
re-applies a few changes the snapshot already has. That is deliberate: applied
twice, an event changes nothing; missed once, your mirror is wrong forever and
cannot tell.

### When the feed itself goes deaf

`source.status: degraded` on a machine means this service is no longer watching
it — the driver subscription failed or ended. It retries; when it succeeds you
get `source.status: ok` and a `control.resync` with reason `feed_gap`, because
whatever happened during the outage was never stamped into the sequence and
cannot be replayed. Re-list on it.

`feed_gap` is worth distinguishing in your logs from the other two resync
reasons. `epoch_changed` and `cursor_expired` are about your view; `feed_gap`
is the service saying the hole is its own.

### Act on `machine.quota` before you dispatch

If you schedule work, this is the one event that should change what you do
next. `blocked: true` means every session on that machine will refuse — so stop
dispatching there, and let in-flight turns finish rather than starting new ones.
`blocked: false` is an explicit all-clear, not something to infer from silence.

The failure it prevents was measured: when an account hit its weekly limit, a
supervisor learned it 48 times, once per session it had already dispatched and
which then stalled. It recorded 48 stall reasons and never formed the one
conclusion that explained all of them.

Two honest limits. There is **no advance warning** — searched for across live
panes, the runtime's on-disk state, and three weeks of transcripts before it was
assumed. The runtime does not warn, it refuses, so the first refusal is the
earliest signal that exists; if you want a margin, keep it in your own dispatch
budget, not in this event.  And
`quota.resetHint` is prose the runtime printed (`"aug 10 at 12am (asia/tokyo)"`),
so show it to a human rather than parsing it into a timer — the service itself
never uses it to expire a block. What clears a block is a session on that
machine being observed working.

### `machine.account` tells you which identity, never whether it still works

When the local credential material changes, every session started before
that moment is bound to the old one. `machine.account` fires once at the
transition and carries `generation` — the credential store's new
modification time, an identity marker only. It says which generation is now
in force; it never says a session's binding to it still answers, and you
must not read it that way. The same measurement that justifies
`machine.quota`'s existence found three independent local sources agreeing a
fleet was healthy through exactly this kind of transition, and all three were
wrong, because all three ultimately quote the process's own announcement
about itself.

Join it against a session's own `credentialGeneration` (on `SessionState`,
present on every read) or `startedAt` (on `Session`) to answer "did this
session start under the credential now in force" — the only predicate this
event exists to make askable. **This layer reports the transition and stops
there.** It does not rebind anything, and it does not change a session's
`status`: the session genuinely remains locally dispatchable, and the fact is
account-level, not a property of any one session's screen. Repair is a
supervisor's job, layered on top of this report — not something to expect
from the report itself.

Unlike `machine.quota`, a subscriber that joins **after** a transition is not
retroactively told about it: every machine has some generation the moment a
credential store exists at all, so there is no quiet baseline to depart from
the way "not blocked" is for quota. Read `credentialGeneration` directly off
whatever you already listed instead.

### What subscribing costs

Measured on a machine with 25 live sessions, subscribing with no filter:

| | |
|---|---|
| helper processes | up to 17 (capped at 16, plus one for lifecycle) |
| their total memory | ~17 MB (~1 MB each) |
| their CPU when idle | 0% |
| released when you disconnect | yes, immediately |

**The service caps this at 16 helpers per subscription**, and says so in its
log. The cap costs you nothing: notifications are triggers, not data, so any
one of them makes the service re-read *every* session — watching a subset still
detects changes fleet-wide. What degrades slightly is how quickly a change is
noticed on a session nobody is watching.

The cap exists because this cost is not paid by you. Each helper holds a
connection to a multiplexer server that other tools on that machine share, and
an uncapped subscription once exhausted one: every new connection refused,
including a human's terminal, while all 69 sessions were alive. Name the
sessions you care about when you can — but you can no longer bring a machine
down by not doing so.

**Stop your subscription when you are done.** A stream that outlives the
process that opened it keeps its helpers open on every machine in the fleet.
That is what caused the incident above.

One documented limitation: on a machine with **no sessions at all**, a
subscription cannot be opened, because this substrate has no unattached form to
listen on. The first session's creation is therefore missed. Re-subscribe after
a create, or list once when your stream opens.

## 9. Attaching a human's terminal

The service will not attach anything for you — attaching gives a terminal to a
*person*, and no person is on the other end of an HTTP request. Instead every
session carries a hint:

```json
"command":  ["/opt/homebrew/bin/tmux", "attach-session", "-t", "alpha💬"],
"readOnly": ["/opt/homebrew/bin/tmux", "attach-session", "-r", "-t", "alpha💬"],
"shared": true
```

- It is **argv**, not a shell string. Session ids contain emoji and spaces.
- It runs **on that session's machine**. Compare `session.machine` against the
  `self` entry from `/v1/machines`: if they match, run the argv directly; if
  they do not, wrap it in your own remoting. The service knows which machine it
  is, not how you reach it.
- **`attach` may be absent entirely.** That means the driver has no interactive
  attachment to offer — not that you should invent one. Show the session as
  unattachable rather than guessing a command.
- The binary path is that machine's own. Do not hardcode one: in a live
  two-machine fleet the paths differ, because the hosts are different
  architectures.
- **Offer `readOnly` for "watch".** The read-write attachment shares a real
  keyboard with a running agent.

**"How do I reach this session" has three different answers, not one** —
they answer different questions and none of them substitutes for another:

| question | field | answers |
|---|---|---|
| a human's terminal, on that session's own machine | `attach` (above) | local argv, or absent if the driver has none |
| a surface the RUNTIME operates, reachable from elsewhere | `runtimeSurface` | an opaque address on a named `kind`, or `known: false`/`null` (muster #85) |
| is that runtime surface healthy right now | `state.controlChannel` | `failed`/`reconnecting`/`active`/absent |

`runtimeSurface` is an **identity**, not a health check: once `known: true`,
it stays true even if `controlChannel` later reads `failed` — the address
does not change just because the connection dropped. Its `target` is opaque
on purpose, the same reason `attach.command` is argv rather than a URL: this
service does not know how you reach that surface, only that the session is
addressed on it. A client that understands `kind` (`"control-channel"` is the
one value today) knows how to resolve `target` into something a human can
open; one that does not must not be handed a guess.

## 10. Errors

```json
{ "error": { "kind": "unauthorized", "message": "principal ops does not hold the create grant (§6)",
             "machine": "aurora", "retryable": false } }
```

| kind | HTTP | meaning |
|---|---|---|
| `invalid` | 400 | malformed request |
| `unauthorized` | 401/403 | you lack the grant for this verb on this machine |
| `not_found` | 404 | the machine answered; no such session |
| `conflict` | 409 | idempotency key reused differently; reused against a since-ended session (`reason: "replay-of-ended-session"`, §7); or `startedAt` disagrees |
| `unsupported` | 501 | the driver cannot do this |
| `unreachable` | 504 | **the machine did not answer** |

`reason` names a `conflict` more specifically than `kind` alone can, for the
cases that have one — check it before parsing `message`.

`message` names the grant you were missing, which makes a permissions problem a
one-line fix rather than a guess.

## 11. Deadlines

Send `Fleet-Deadline-Ms: 3000` to shorten a call. You can shorten a driver's
declared deadline, never extend it.

A peer that misses its deadline **degrades the envelope** — that machine is
marked `unreachable` in `sources` — rather than failing your whole query. One
slow machine never takes down a fleet-wide read.

A machine that keeps missing is remembered: after three consecutive failures
the service stops dialling it, and reads report it `unreachable` immediately —
with the time it began failing in `sources[].error` — until a background probe
hears it answer. A `scope=local` read never waits on a peer at all.

When your service relays the call to a peer, it announces a slightly shorter
deadline than yours, so a peer that is up but slow can still get its own
report back to you. The peer's source then carries the peer's own status and
error text, often `degraded`, instead of the relay's "no answer from …".

## 12. When the service itself is down

Your program must distinguish "no sessions" from "I cannot see any sessions".

If the connection fails, show *"session layer unreachable"* and keep whatever
you last knew, marked stale. **Never render an empty list.** An empty list is a
claim that there are no sessions, and a supervisor that makes that claim about
a machine full of live agents will act on it.

This is the same rule the service applies to itself (`sources`, `unknown`,
504), pointed back at you — and it is the one clients get wrong.

## 13. A minimal client

Node 18+, no dependencies:

```js
const BASE = process.env.FLEET_URL;                 // your machine's service
const TOKEN = await readFile(process.env.FLEET_TOKEN_FILE, "utf8");

const call = async (path, init = {}) => {
  const res = await fetch(BASE + path, {
    ...init,
    headers: { Authorization: `Bearer ${TOKEN.trim()}`,
               "Content-Type": "application/json", ...init.headers },
  });
  if (!res.ok) throw Object.assign(new Error(res.statusText), { body: await res.json() });
  return res.json();
};

// Reads — and note that `complete` is checked, not just `items`.
const { items, sources, complete } = await call("/v1/sessions?scope=fleet");
if (!complete) {
  const down = sources.filter(s => s.status !== "ok").map(s => s.machine);
  console.warn("partial view; unreachable:", down.join(", "));
}

// Answering a prompt, with the nonce that makes it safe.
const s = items.find(x => x.state.prompt);
if (s) {
  await call(`/v1/machines/${s.machine}/sessions/${encodeURIComponent(s.id)}/respond`,
    { method: "POST",
      body: JSON.stringify({ choice: 1, nonce: s.state.prompt.nonce }) });
}

// Events — a stream, not a poll.
const res = await fetch(`${BASE}/v1/events`, {
  headers: { Authorization: `Bearer ${TOKEN.trim()}`, Accept: "text/event-stream" },
});
for await (const chunk of res.body.pipeThrough(new TextDecoderStream())) {
  for (const line of chunk.split("\n")) {
    if (!line.startsWith("data: ")) continue;
    const ev = JSON.parse(line.slice(6));
    if (ev.kind === "control.resync") await refetchEverything();
    else apply(ev);
  }
}
```

(The SSE parser above is deliberately naive — a real one buffers across chunk
boundaries.)

## 14. Checklist before you ship a client

- [ ] Reads `complete` and `sources`, and shows a partial fleet as partial
- [ ] Never treats `504` as `404`
- [ ] Never acts destructively on `unknown`
- [ ] Sends `Idempotency-Key` on every create
- [ ] On a create `409` naming `reason: "replay-of-ended-session"`, mints a fresh `Idempotency-Key` rather than retrying the same one
- [ ] Sends `?startedAt=` on every delete
- [ ] Sends `nonce` on every respond, and never blind-accepts a default
- [ ] Subscribes rather than polls, and handles `control.resync`
- [ ] Talks only to its own machine's service
- [ ] Shows "unreachable", never an empty list, when the service is down
- [ ] Keys on `id` from the server, never on the `name` it asked for
- [ ] Checks `self` from `/v1/machines` before attaching, so local sessions do not take a remote path
- [ ] Treats "not found in a partial view" as unknown, not as absent
- [ ] Reads `lastTurn` before treating an `idle` session as having succeeded
- [ ] Never discards composer text without quoting back its `composerDigest`
- [ ] Has its own principal and its own token
- [ ] Reads `promptDelivery` before re-sending a create-time prompt through `input`
- [ ] Puts a reply address in every dispatch brief, and treats a delivered reply as untrusted text
