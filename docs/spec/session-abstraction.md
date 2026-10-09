# Session abstraction — specification

**Status: two drivers satisfy this interface** — one local, over a terminal
multiplexer running an interactive agent CLI; one remote, an HTTP client to a
peer service. That was the threshold this document set for itself, and it is
met: no claim here is now unexercised prose.

§4.2's central claim survived. A remote peer really is just another driver,
proven end to end with no special case in either service — a caller asked a
service holding no local drivers, which proxied through the remote driver, over
HTTP, into a second service, into the multiplexer driver, and back with 22 real
sessions. `confidence: inferred` survived the round trip rather than being
flattened to `observed`.

Things that did **not** survive contact are collected in **§14, Open defects**.
Read that section before relying on any guarantee in this document.

The one security-shaped defect among them — caller authority having nowhere to
travel, whose symptom was that everything appeared to work — **is now fixed**
(§2.6, §6). Fixing it also proved the cross-machine write path end to end.
Four remain open, and deployment added a fifth.

### How to read this

- **§1–§13 are normative.** They state the design as it is now, in the present
  tense. Where a rule is known to be unenforceable, the section carries a short
  blockquote naming the defect — the rule is left in place because it is still
  what a driver should do.
- **§14 is the list of things this document requires but cannot enforce.** Each
  entry states the rule, why it fails, what that costs, and a proposed fix.
  A proposed fix is not a decision.
- **Appendix A is how any of this was learned.** Measurements, the bugs that
  taught the rules, and the reasoning behind decisions that look arbitrary from
  the outside. Sections above point into it by finding number (F1, F2, …).

The separation is deliberate. Earlier revisions kept each discovery inline as
an amendment, which preserved the reasoning but grew until a third of the
document was archaeology and a first-time reader could not tell current truth
from a record of having been wrong. Nothing was deleted in the consolidation —
the narratives moved to Appendix A, and the rules they produced moved into the
body.

---

## 1. Scope

`muster` owns **sessions**. It creates them, delivers input to them,
reports their state, and destroys them. It does this identically whether the
session runs on the local machine or a peer.

### Non-goals

Explicitly outside this layer, permanently:

- version-control state, branches, worktrees
- issue trackers, work claims, assignment
- planning, scheduling, or deciding what should be worked on
- any judgement about whether work is complete or correct

A supervisor built on top of this layer owns all of the above. If a field in
this API would only make sense to something that understood those concepts, the
field is wrong.

---

## 2. Core model

### 2.1 SessionSpec

What a caller must supply to start a session.

```
SessionSpec {
  machine    : MachineId          // which host should run this
  runtime    : RuntimeId          // which driver
  cwd        : AbsolutePath       // working directory
  agent?     : AgentId            // named persona/config
  model?     : string
  effort?    : string
  name?      : string             // human-facing label
  marker?    : string             // session-type stamp appended to the name
  labels?    : map<string,string> // caller facts about the session, opaque; bounded (see below)
  remoteControl?: boolean         // reachable by remote clients; absent ≠ false
  prompt?    : string             // initial input
  contextRef?: AbsolutePath       // see §5.3 — never inline, never argv
  env?       : map<string,string> // out of band only, never argv (§5.3)
  isolateEnvironment?: boolean    // the session's process carries ONLY a built environment, never the service's own; a driver that cannot refuses `unsupported` (§4.3)
  sandbox?   : SandboxSpec        // an OS sandbox around the session's process: files, unix sockets and system services denied outside what is granted; a driver that cannot enforce it refuses `unsupported` (§4.3)
  resume?    : string             // a prior conversation this session continues
  conversationId?: string         // a caller-chosen UUID this session starts a NEW conversation under; mutually exclusive with resume
  permissionMode?: string         // a non-default permission posture; "bypass" is the only value
  mcpConfig? : AbsolutePath[]     // tool-server configuration, by path, never inline (§5.3)
  settings?  : JSON object        // launch-time runtime settings for the agent CLI; bypass-only, never secrets (api-http.md §3.3)
  consents?  : PromptKind[]       // boot questions the caller pre-answers (§2.7)
  trustCwd?  : boolean            // superseded by consents; means exactly ["folder-trust"]
}
```

Six of these were undocumented here until muster issue #57's audit — found alongside §2.3's
drift, and by the same mechanism: each is already fully specified in this repository, just not in
this block. `env`, `resume`, `permissionMode`, `mcpConfig` and `settings` are **hints**, the same family as
`agent`/`model`/`effort` above: a driver that cannot honour one must say so at creation rather than
silently substitute a default (§4.3). `consents` and `trustCwd` are not hints — they are a caller's
standing answer to a boot question the driver would otherwise leave the session parked in front of;
see §2.7's `Consents`/`TrustCwd` paragraph for why that is still the caller's decision and not this
service's. Full field-level rationale — including why each value is staged out of band rather than
placed on a command line, and the exact validation each one gets — lives in `session.go`'s
`SessionSpec` doc comment, which this block summarizes rather than duplicates.

`agent`, `model` and `effort` are **hints**, not guarantees. A driver that
cannot honour one must say so at creation rather than silently substituting a
default; see §4.3. `remoteControl` is a hint in the same family.

**`conversationId` asks a create to START a new conversation under a
caller-chosen id, `resume` asks it to CONTINUE one — the same question
answered two incompatible ways** (muster #224). Sending both is refused
`invalid`. It exists so a client that opens a viewer keyed by conversation id
does not have to poll `GET .../sessions/{id}` until the runtime's own
per-process record shows up (measured at ~2s on a fresh session): a driver
that honours it reports the id back as `conversation` on the `201` itself,
`known: true` with `source: captured` (§2.9) — a fact this service
established by *telling* the runtime which id to use, not by matching a
title. It is a hint like `resume`: a driver with no way to launch its runtime
under a caller-chosen id refuses the create `unsupported` rather than start
one under an id of its own choosing that merely looks right, and a relay to a
peer that predates the field is refused the same way, before the peer ever
sees it (api-http.md §3.3). The id must be UUID-shaped, and refused as
`invalid` if it already names a conversation this machine has on record for
the same `cwd` — starting a second conversation under it would make two
conversations share one transcript. A later read whose own resolution
disagrees with the requested id is a loud, named mismatch, never a silent
overwrite of one with the other — the same invariant §2.9 already states for
a replaced process's stale identifier.

**`isolateEnvironment` is a requirement, not a hint** (muster #280). It asks that the
session's process carry ONLY a built environment — a small documented base, this machine's
`sessionEnv` entries that apply (#94), and the create's own `env` — and nothing inherited from the
service. A driver that cannot deliver that (`isolatesEnvironment: false`, §4.3) refuses the create
`unsupported`, before any side effect, rather than start a session that leaks the service's
environment into a caller that asked for less (§5.6); a relay to a peer that predates the field is
refused the same way (api-http.md §3.3). It is a plain boolean: there is no honest request for the
opposite, and a driver that always isolates simply honours it. What it does and does not promise is
in §4.3.

**`sandbox` is a requirement, and it is refused rather than degraded** (muster #281). Environment
isolation decides what a session is *handed*; it does not decide what the session can *reach*. A
process started with a clean environment still reads the service user's home directory (git and
forge CLI configuration, SSH keys, other projects), connects to that user's multiplexer and agent
sockets, and talks to the desktop's system services. `sandbox` asks the driver to wrap the session's
process — and so every tool the session starts — in the host's own sandbox facility. Its absence is
no sandbox; its presence, even as `{}`, is the default profile.

```
SandboxSpec {
  readPaths?    : AbsolutePath[]   // readable in addition to the defaults
  writePaths?   : AbsolutePath[]   // readable and writable in addition to the defaults; never a shared temporary directory
  network?      : "open" | "closed"  // default open; closed = loopback only
  packageCache? : { path: AbsolutePath, mode?: "shared" | "private" }   // default private
}
```

The defaults are deliberately narrow. Readable: the session's working directory and its own private
directory, the runtime's own install, and the system's program directories. Writable: the working
directory and the session's private directory (which holds its `TMPDIR`), and a few device files.
Everything else is denied, and the caller can only *add* paths — the allow-lists that keep the
runtime working are carried in the driver and no create can widen them. A path must be absolute and
clean; a grant that is, or contains, the service user's home directory is refused `invalid`
(it would put back exactly what the profile removes), as is a writable grant on a shared temporary
directory. The shape of a malformed request is a `400` on any machine.

**What "denied" means is three classes, together** — because each is a way out of the other two,
and a platform that cannot deny all three does not offer the capability (§4.3) rather than a weaker
profile under the same name:

- *Files.* Reads and writes are denied outside the granted paths. Metadata (does it exist, how big)
  stays readable so a program can walk to a directory it was granted.
- *Unix-domain sockets.* A path-based file deny does not cover a **connect**. Measured: inside a
  profile that denied the home directory, the multiplexer's server socket was still connectable —
  and a command started through it runs *outside* the profile, with the user's full home — and so
  was the SSH agent's socket. Connects are denied outright, with one exception: the system name
  resolver, while the network is open. A file grant on a directory does not make a socket inside it
  connectable.
- *System services.* With the first two closed, the clipboard was still readable, `open` still
  launched applications outside the profile, and scripting events still drove other applications.
  Service lookups are denied except a short allow-list a process needs to resolve users and names
  and write a log line.

**The temporary directory is the session's own.** Each session has a private `TMPDIR` inside its own
directory (§4.3, `isolatesEnvironment`); writes to the shared system temporary directories are
denied, because concurrent sessions would otherwise share a writable directory — a cross-session
side channel.

**The profile replaces a harness's own sandbox; it never nests inside it.** A macOS profile cannot be
applied from inside another (`Operation not permitted`), so a harness that ships its own sandbox
fails closed under this one and refuses every shell command. A driver whose runtime has one must
tell it to stand down. (The opencode driver's runtime has none.)

**Network posture is open or closed, nothing finer.** The platform sandbox cannot filter by
hostname, and the model API a session talks to needs the network. `closed` leaves loopback only (the
runtime's own server lives there), so a closed session works only against a model that is reachable
over loopback. The capability lists the postures a driver can enforce and implies no host filtering.

**A package cache is part of the profile's shape**, because a dependency install is impractical
without one and the home directory is closed. `private` (default) gives the session its own copy of
the named directory, made when the session starts — copy-on-write where the filesystem has it, a
plain copy otherwise — so no write path is shared between concurrent sessions and what the session
installs never reaches the original. `shared` grants the named directory itself, writable.
Either way the session finds the directory it should use in `MUSTER_PACKAGE_CACHE`, and
`state.sandbox.packageCache` says which mode and which kind of copy was used.

**What a `201` does not mean, and what the caller must do.**

- *The working directory is untrusted once a sandboxed session has run in it.* The session controls
  any repository configuration inside it (filter drivers, external diff programs, file-system
  monitors, hooks). A git command run **outside** the profile on that directory afterwards — by this
  service or by its caller — executes code the session chose: a sandbox escape. This service never
  runs git on a session's working directory. A caller extracting the result (a diff, artifacts) must
  do it inside the same profile, or with every exec-capable setting overridden.
- *Setting up the working directory is the caller's job, and it comes first.* A test loop that diffs
  against the repository's trunk needs the trunk reference to exist before the session starts (a
  clone fetched at one commit has none, and diff-based gates refuse to run); it cannot be fetched
  from inside the profile once the credentials are out of reach. Seed the reference, pointing at the
  start commit, before creating the session.
- *Tests that start their own multiplexer fixtures* fail inside the profile unless pointed at a
  private socket under the session's own directory — their connect to the default socket is
  exactly what the profile denies.
- *The model-provider key is still in the session's environment*, and the profile does not hide it
  from the session's own tools (§4.3, `isolatesEnvironment`).

**`labels` are caller facts, not hints and not configuration** (muster #153). A caller
attaches what IT knows about a session — the unit of work it serves, the working tree it uses, what
kind of session it is — and the service stores them, returns them on every read of that session,
filters on them, and assigns them no meaning, exactly as it assigns none to `marker`. They exist so
"is any machine already working on X" is answered by reading a fact rather than by pattern-matching
names, which are mutable, collide across machines (#19) and cannot carry a session's type once a
marker is creative. Labels are **not** a lock, a claim or a lease: two sessions may carry the same
pair and the service says nothing about it. Bounded to 16 pairs, keys of 1–128 bytes that do not
contain `:`, values of at most 128 bytes; a map outside those bounds is refused `invalid` naming the
limit, never truncated. Labels may also be changed after creation (api-http.md §3.3, `labels`).

**`marker` is also reported back, read-only, as `marker` on `Session`** (muster #165). It is
the marker this session's create carried **and** the resolved name ended in, recorded by the driver
at the instant it applied it, so a consumer grouping sessions by type reads a fact instead of running
a suffix test on the name — the test #90 and #96 found ambiguous whenever a marker shares the name
body's alphabet. No operation writes it. It is **not a label**: the label namespace stays entirely
the caller's, and a label keyed `marker` is only a label. It survives a rename, because a rename
changes what a session is called and not what kind of session it is. Absent means this machine has
no such record — no marker was asked for, the name kept a *different* marker it already carried, the
driver does not record markers, or the session predates the field. It is never a claim that the
session is untyped.

**A created session must be the same KIND of session the substrate's own
launcher produces.** This is normative, and it is the rule the three fields
above exist to make satisfiable. A service that creates a second-class session
— one that cannot be reached remotely, or lacks the environment an agent needs
to call a tool, or does not carry the naming its ecosystem keys on — has forked
the model, and a fork in the model is what this specification exists to remove.
The failure is worth stating precisely because of its shape: such a session
**starts, lists, reads and drives perfectly**, and the divergence appears later,
elsewhere, looking like an agent fault rather than a creation fault.

**`remoteControl` absent is not `remoteControl: false`.** Absent means "whatever
a first-class session on this substrate gets", which is what a caller who has
never heard of the field wants. Only an explicit `false` opts out. A boolean
whose zero value silently meant "off" would make every unaware caller produce
the second-class shape — which is precisely the defect.

**Naming rules belong to the driver, not to a client.** A driver that applies
conventions — sanitizing, numbering a collision, stamping a type — must apply
them to every creation path, not leave them to whichever client happens to know
about them. Rules enforced by one client are not rules; they are a convention
that holds until a second client exists. Where a driver derives a name, the
resolved string is what the session carries **everywhere its identity appears**
— the id, any remote-control binding, the agent's own name — from birth.
Resolving the name after building the invocation, so that one of those is bound
to a name the session does not have, is the same invisible-until-later failure
as above.

> Origin: Appendix A, F51.

### 2.2 SessionRef

```
SessionRef {
  machine : MachineId
  id      : string        // opaque, scoped to (machine, runtime)
  name?   : string
}
```

Ids are **machine-scoped and potentially recyclable**. A caller must never
treat an id as a globally unique identity, and must never act destructively on
an id match alone — see §5.4.

### 2.3 SessionState

```
SessionState {
  status     : Status
  confidence : "observed" | "inferred"
  evidence   : string             // human-readable provenance
  since?     : Timestamp | null
  prompt?         : SessionPrompt // the question this session is blocked on (§2.7)
  waitingOn?      : WaitingReason // WHY status is waiting_input
  composerDigest? : string        // fingerprint of unsent composer text
  strandedDelivery? : boolean     // true only with waitingOn unsent-input: the unsent text is a message THIS driver delivered and reported queued, handed back by the runtime (#240)
  screenDigest?   : string        // fingerprint of the screen read — corroborates `keys` (§3); only from a driver declaring deliversRawKeys (§4.3)
  quota?          : QuotaBlock    // set when status is quota_blocked
  lastTurn?       : TurnEnd       // how the most recent turn ended, if the driver knows
  turns?          : integer       // agent turns completed since the most recent prompt delivery (#111); 0 is a finding, absent means the driver could not count
  credentialGeneration?: Timestamp // this machine's local credential generation at read time (#12)
  controlChannel?: ControlChannel  // what the RUNTIME says about its own remote-control channel (#48)
  permissionMode?: PermissionMode  // the permission mode the runtime shows the session to be in, when the driver can read it (#194); absent = nothing was read
  sandbox?       : SandboxState    // the profile the session runs under (#281): mechanism, denied classes, network, granted paths (defaults included), package cache; absent = not sandboxed
  warnings?      : Warning[]       // footer notices the driver read below the composer (#230); absent = none found, not merely unobserved
  usage?         : Usage           // what the session has spent, as its RUNTIME reports it (#285): summed over every request; absent = not yet known, never zero
}

Usage {
  input      : integer      // tokens read that were not served from a cache
  output     : integer      // tokens written, as the runtime counts them
  cacheRead  : integer      // tokens served from the runtime's prompt cache
  cacheWrite : integer      // tokens written into the runtime's prompt cache
  reasoning? : integer      // reasoning tokens the runtime reports APART from `output`, additional to it; absent = it does not split them out
  cost?      : number       // what the runtime itself reports the requests cost; absent = it reports none — never priced here
  source     : "observed" | "inferred"   // from a structured API / inferred from a record the runtime writes
  asOf       : Timestamp    // the runtime's own newest entry counted in these figures, not the time of the read
  lastTurn?  : TurnUsage    // the same figures for the most recent COMPLETED turn
}

TurnUsage {
  at         : Timestamp    // when the runtime recorded the turn as finished
  input      : integer
  output     : integer
  cacheRead  : integer
  cacheWrite : integer
  reasoning? : integer
  cost?      : number
}

Warning {
  kind?: string  // closed-so-far vocabulary naming what the notice is about (today: "transcript-unreliable"); absent = notice-shaped line found, not yet named
  text : string  // the runtime's own words, trimmed; for humans and logs, never branched on
}

ControlChannel {
  state   : "active" | "connecting" | "reconnecting" | "failed" | "off"
  reason? : string  // the runtime's own words for why `failed` (muster #69); for humans, never branched on
  bridgeId : string | null  // the id the runtime's bridge published when the channel came up (muster #276); an opaque token, null when off, failed, not read or the runtime wrote none
}

PermissionMode =
  | "default"         // the runtime's ordinary mode (its own indicator calls it "manual")
  | "acceptEdits"     // file edits accepted without asking
  | "plan"            // read and plan, do not act
  | "auto"            // the runtime decides what is safe to do without asking; not offered on every model
  | "bypass"          // stops asking before it acts — the same word `SessionSpec.permissionMode` takes at create time
  | "unknown"         // an indicator area was read and named no mode — a real answer, never a guess

Status =
  | "starting"        // spawned, not yet accepting input
  | "working"         // actively producing
  | "waiting_input"   // blocked on a human or caller
  | "idle"            // alive, finished, awaiting more work
  | "quota_blocked"   // alive but refused by its provider
  | "dead"            // process gone
  | "unknown"         // driver cannot determine — a real answer, not an error

WaitingReason =
  | "prompt"          // a question is on screen; `prompt` carries it
  | "unsent-input"    // the composer holds text nobody submitted
  | "starting"        // muster #126: no composer painted at all yet — PromptDelivery.waitingOn only

QuotaBlock {
  since     : Timestamp   // when the refusal happened, per a driver able to say so; otherwise when this driver first saw the notice — evidence states which (#56)
  resetHint?: string      // the runtime's own words on when it lifts; display, never parse
}

TurnEnd {
  outcome   : string      // "failed" is the only value produced today; no TurnEnd at all is the ordinary case
  reason?   : string      // the runtime's own words; for humans, never branched on
  retryable?: boolean     // the runtime's own claim that sending anything resumes the session
}
```

> **`screenDigest` is present only from a driver that declares
> `deliversRawKeys` (§4.3).** It fingerprints the whole screen this state was
> read from and is the corroboration token `keys` (§3) quotes back — the same
> discipline `close` uses with `startedAt` and `discard` with
> `composerDigest`. A driver over a runtime with no screen to capture — no
> terminal, nothing to fingerprint — leaves it absent and declares
> `deliversRawKeys: false`; that absence is a real, honest answer (§5.7), the
> same shape `AttachHint` already uses for a substrate with no interactive
> attachment (§2.8). Decided by muster issue #59: the field was left out
> of this block on purpose, pending exactly this choice between a
> capability-gated first-class field and a permanently wire-only one; see
> §3's `keys` entry for what the decision costs and why it does not close the
> question it looks like it closes.

> **`controlChannel` is what the RUNTIME says about its own remote-control
> connection — never what the far end thinks (muster issue #48).** This
> layer does not model bridges and should not; whether a far end is listening,
> who owns it, whether it was archived elsewhere are all outside it. A runtime
> describing its own connection is not: it is the same kind of self-report as
> every other field here, and it is read the same way.
>
> It exists because that state is otherwise INVISIBLE. A dead control channel
> raises no prompt, blocks nothing, and changes no status — the session sits at
> an empty composer with a healthy status line and is, through every other
> field, an ordinary live session. Measured: 37 of 67 sessions came back from a
> fleet-wide recovery in exactly that state, and the supervisor that had to find
> them read pane text instead, because nothing here carried it.
>
> **`off` is the one state that is not read off the runtime's status label**
> (muster issue #269). A session without remote control renders no label, so a
> label can never say "off"; a driver reports `off` only on positive evidence it
> holds — the session was launched without remote control, or the runtime's own
> durable record shows the last enable was followed by a disconnect — and never
> from the mere absence of a label. A driver with neither leaves the field
> absent, and absent still means "not read".
>
> **Absent never means connected** (§5.7). A runtime with no such channel
> reports nothing, and so does a driver that cannot look; `observesControlChannel`
> (§4.3) is the field that tells those two apart, and an unreached peer reports
> it `assumed` rather than a bare false. It never rewrites `status`, unlike
> `quota`: a session nothing outside can reach is still running and still able
> to work, and folding one fact into the other is a precedence mistake this
> specification has already made once.
>
> A driver reading this off a screen must read it from the runtime's own chrome
> and not from the transcript. The transcript is whatever the agent chose to
> print, and the measured consequence is specific: a supervisor grepping panes
> for the disconnection notice classified ITSELF as disconnected, because its
> own tool output contained the strings it was searching for.

> **`permissionMode` is the permission mode the runtime SHOWS the session to be in
> (muster issue #194) — the read side of `keys`' `BTab`.** `BTab` cycles the
> mode and the receipt for a press says only that the screen changed under it
> (ADR 188); which mode a press lands in is the runtime's own cycle order, and a
> client that wants a named mode has to press, read, and repeat. Until this field
> existed the "read" had no answer anywhere in this API, and a client had to keep a
> handle on the terminal multiplexer to get one.
>
> **Two different absences and one explicit answer — none of them a guess
> (§5.7).** The field is *absent* when nothing was read: the driver does not look
> (`observesPermissionMode`, §4.3, is what says so, and an unreached peer reports
> it `assumed`), or no composer was on screen to anchor the indicator area to (a
> dialog owns it), or nothing was painted under the composer yet. It is `unknown`
> when the indicator area WAS read and named no mode this build recognises — a
> reworded label, a mode this list does not have, a hint painted in its place, or
> two modes at once. A client cycling toward a target stops on `unknown` rather
> than pressing on, and reads again on absence.
>
> The set is closed and a value outside it is a decode error, like `status`. Its
> members are the modes measured on a real runtime build, not the ones a settings
> file lists — including `bypass`, the mode most of an unattended fleet runs in.
> It carries no conversation and no screen text: it is one of six fixed words, so
> publishing it does not reopen the rule that `state` publishes fingerprints of the
> screen and never the screen.
>
> A driver reading it off a screen must read it from the runtime's own chrome —
> the row under the composer's closing fence — and never from the transcript, for
> the reason given for `controlChannel` above: a session whose transcript mentions
> `plan mode on` must not read as being in plan mode, or a client that stops
> cycling at `plan` stops on a lie. A change fires `session.state` (§4) like any
> other material change. It never changes `status`.

> **`usage` is what the session has spent, as its runtime reports it (muster issue
> #285).** A consumer that wants activity and cost for every session, whatever its
> engine, should not have to read each runtime's private records itself. The
> figures are the runtime's own account of itself (§5.8) — counts and a cost it
> chose to report — and never content the session produced.
>
> **It is the SUM over every request, never the last one.** A runtime reports
> usage per request and a turn is many requests (one per tool round); the last
> understates a run by an order of magnitude (measured 2–20×; one run logged 63
> thousand against 1.19 million true). `cost` is per request as well and is summed
> the same way. A runtime that writes one message as several record entries
> repeats the usage on each, and a driver counts a message once.
>
> **Absent means not yet known, never zero** (§5.7), field by field. A driver
> declaring `reportsUsage` (§4.3) that has read nothing yet leaves the block off —
> a record not found, no entry yet carrying usage, a large record still being
> read in the background. Zero is a finding: the runtime reported, and nothing was
> spent. `reasoning` and `cost` are absent when the runtime does not report that
> figure and present — zero included — when it does. **`cost` is never computed
> from a price table**: a driver that would have to guess leaves it off, and a
> runtime that reports tokens only (the tmux driver's record carries no cost)
> yields tokens only. `reasoning` is a runtime's separately reported count and is
> *additional* to `output`; a runtime that counts reasoning inside its output
> figure leaves `reasoning` absent rather than double-counting.
>
> **`source` and `asOf`.** `observed` is read from a structured API, `inferred`
> from a record the runtime writes to disk — the same distinction `confidence`
> makes. `asOf` is the runtime's own timestamp on the newest entry counted, so a
> consumer seeing figures that stopped moving can tell a session that stopped
> spending from a reader that stopped reading.
>
> **Per-turn figures ride on the state, not on a separate event.** No turn-end
> event exists; a turn ending is a `session.state` carrying `usage.lastTurn`
> beside the cumulative figures. A consumer summing off the feed needs no poll and
> no new event kind, and a poller reads the same field. A turn is the span between
> two of the runtime's own turn-boundary markers (the tmux driver) or the
> assistant messages after one user message (the opencode driver); a turn in
> progress is in the cumulative figures and is not yet `lastTurn`. A change in any
> figure fires `session.state` (§4 of api-http.md): once per completed request,
> never per repaint. It never changes `status`.

> **`warnings` lists footer notices the driver read below the composer's closing
> rule (muster issue #230), the same region `controlChannel` and
> `permissionMode` are read from and for the same reason: independent of
> `status`, and otherwise invisible through every other field here.** #229 found
> that a footer notice — "⚠ Transcript writes are failing (disk full — ENOSPC) ·
> recent messages may …" — can satisfy the shape test the TUI's real turn-status
> line uses, and fixed the misread by bounding the spinner scan at the
> composer's closing rule. This field is the other half: a session whose
> transcript is not being recorded is worth a caller knowing about on its own
> terms, not only worth not being misread as `working`.
>
> **Absent means no notice-shaped line was found — a positive finding, not
> merely unobserved (§5.7).** Only one driver reads footers today and it
> always looks, so there is no separate `observes…` capability the way
> `controlChannel` and `permissionMode` each have one; a second driver reading
> footers by a different method should add one rather than silently reuse this
> absence.
>
> **`kind` absent on an individual `Warning` is a different, milder absence than
> `status` or `permissionMode`'s `unknown`.** The entry already existing in the
> list is the driver's claim "a notice-shaped line is here" (the same shape
> test #229 built); `kind` empty says only that this driver does not yet
> recognise the wording, and `text` still carries the runtime's own words
> rather than the line being dropped for lack of a name. The vocabulary is
> expected to grow as new notices are measured, the same way `waitingOn`'s set
> is not claimed to be exhaustive by having only three members.

> **`failed` may be established from the record as well as the footer (muster
> issue #270).** A runtime may draw the label of a healthy channel in a place a
> driver cannot read safely, and the label of a failed one is then invisible.
> The runtime's own disconnection notice is a durable, structured entry an agent
> cannot write, so a driver may read it, but not as `failed` at once: measured
> over a large set of records, a notice about a recoverable cause was followed
> by a fresh enable entry within two minutes every time, while a terminal one
> was not followed by one for hours. A driver reports `failed` only once the
> notice has stood unanswered for a settle window, and nothing before that. The
> record carries no entry for `connecting` or `reconnecting`, so those are
> reported only when the screen shows them.

> **`ControlChannel.reason` is sourced from the runtime's own durable record,
> never from a screen (muster issue #69).** `state` alone cannot say
> WHY a channel is `failed` — some failures are the runtime retrying on its
> own after a drop it will recover from, some are terminal and only a fresh
> session can follow — and that difference decided the response during the
> incident behind #48. Promoting it out of the transcript was deferred for
> exactly the reason the paragraph above states: the transcript is
> forgeable, and an agent's own tool output can contain the same words a
> supervisor is searching for.
>
> The record turned out to carry the same notice, structured rather than
> rendered — a `system`/`informational` entry the runtime itself appends —
> and reading THAT is sound in a way reading the transcript is not, on the
> same reasoning §5's `QuotaBlock`/`TurnEnd` upgrade already relies on: a
> `user`-role entry can hold captured command output containing the identical
> phrase, a region an agent's own actions populate, so a driver must check
> the entry's own `type`/`subtype` before ever comparing its content against
> the phrase — never a substring search across every entry in the record.
> That ordering is the entire safety property; a phrase match across entry
> types reintroduces exactly the forgeability the footer-only rule exists to
> end, one layer lower and harder to see.
>
> `reason` carries the runtime's own sentence whole, close code included when
> the runtime put one in it, and stops there: which close codes are
> transient versus terminal has never been measured, and this specification
> does not classify them (muster #65 declined that inference once
> already). A caller sees the runtime's own words and decides for itself.
> Absent whenever `state` is not `failed`, or is `failed` but no record, no
> readable record, or no matching entry can explain why (§5.7) — the same
> "we don't know" that an absent `controlChannel` itself already means, one
> field down.

> **`QuotaBlock.since` and `TurnEnd.retryable`/`reason` are, where a driver
> can manage it, sourced from the runtime's own durable record rather than
> from the screen (muster issue #56).** The tmux driver's screen path
> stays legitimate and stays **upgrade-only**: a limit notice or a failure
> banner on screen may promote a session INTO `quota_blocked` or attach a
> `TurnEnd`, but never takes one back OUT — the record wins in that
> direction, because the screen's evidence for "no longer true" is the
> absence of evidence for "still true", which §5.6's degrade-not-emulate
> rule and this section's own unrecognised-evidence rule both forbid as a
> route to a positive answer. When no such record can be resolved, `since`
> falls back to the time this driver first observed the notice — a real
> fact, just not the one the field name promises — and `evidence` says so
> explicitly rather than presenting one as the other.

> **`turns` is the liveness fact muster #111 asked for, and it answers
> a question no other field here can.** A caller that dispatched a worker
> session and later reads `status: idle`, no `screenDigest`/`composerDigest`,
> no pending prompt, cannot tell "the agent ran and decided nothing was
> warranted" from "the agent never took a turn at all" — both produce that
> exact reading. `turns: 0` alongside `idle` is the first case reachable only
> by the second: a delivery was made into this session and zero turns have
> completed since. This is also what resolves §2.11's `PromptDelivery`, whose
> `outcome` can otherwise rest permanently at `queued`/`unknown` on a
> substrate where receipt is not directly observable — a turn taken after
> delivery **is** observable receipt.
>
> The denominator is **the most recent delivery this driver made into this
> session's composer**, not the session's lifetime. It resets when a new
> delivery lands and does **not** move when `resumeIfStranded` (§3.3's
> `input`) finishes an earlier, already-counted delivery — that is the same
> delivery, not a new one. Absent means this driver holds no delivery mark
> for the session, or could not read far enough back into the runtime's own
> record to answer honestly; `0` is a positive finding and must never stand
> in for "could not tell" (§5.7).
>
> **Why this does not reopen muster #82 or #107's ruling on it.** §5.8,
> immediately below, states the provenance test this field is held to; the
> short version is that `turns` is counted from the same unprompted,
> runtime-written record §2.9's `ConversationRef` already treats as safe to
> name, and it carries strictly less information than `screenDigest`, a
> field already in this block. See
> `docs/adr/111-turns-is-a-liveness-fact-not-a-result-channel.md` for the
> full argument — this note is deliberately short because that ADR is where
> a future reopening must engage, not here.

Six fields above (`prompt` through `credentialGeneration`) were undocumented
in this block until muster issue #57, which found the drift: the Go
`SessionState` (`state.go`) carried ten fields — eleven once
`credentialGeneration` is counted — against the four this block used to
show. Each of the six is exercised elsewhere in this specification already —
`waitingOn` discriminates `waiting_input` at api-http.md §3.3's `respond`;
`quota` and `lastTurn` are both named, with worked JSON examples, at the same
section; all six are on the `session.state` materiality list at api-http.md
§4 — so the type block, not the design, was what was behind. `waitingOn`'s
own design story (why `waiting_input` needed a reason once a second cause of
blocking existed) is Appendix A's own account of reaching it; `lastTurn`'s is
F51.

Three rules govern this type, and they are the point of the whole
specification:

**`unknown` is a valid answer.** Some runtimes genuinely cannot report their
own state. A driver must return `unknown` rather than guessing, and callers
must handle it as an ordinary case rather than a fault.

**`confidence` separates knowing from guessing.** A driver that reads a
structured status from an API reports `observed`. A driver that infers state
from terminal output, process tables, or file mtimes reports `inferred`. Both
are legitimate. Collapsing the distinction is how a precise runtime gets
flattened to an imprecise one's accuracy — the interface would then destroy the
exact advantage it exists to expose.

**`evidence` is for humans.** It carries whatever the driver actually saw. It
is never parsed by callers, and its format is not stable.

Two further rules follow from the first, and they are normative rather than
advisory — a driver that violates either produces confident wrong answers:

**Reach `unknown` from unrecognised evidence, not only from absent evidence.**
When a driver sees a signal it does not recognise, the answer is `unknown`.
"No positive evidence of working" must never decay to `idle`: a wrong `idle`
for a session that is working is silent, and an `unknown` is not.

**Fail toward `unknown`, never toward the plausible answer.** §5.6 states this
for capabilities; it holds field by field.

> Why `inferred` is load-bearing rather than decorative on a real substrate,
> and how these two rules were earned: Appendix A, F6.

### 2.4 DeliveryReceipt

```
DeliveryReceipt {
  outcome : "submitted" | "queued" | "refused" | "unknown"
          | "delivered" | "held" | "denied" | "expired" | "dropped"
  reason? : string
  delivery? : DeliveryPath
  sessionIds? : string[]     // refused only: the address was a conversation id, and these are the live sessions holding it (api-http.md §2)
}

DeliveryPath {
  route   : "terminal" | "inbox" | "module"
  module? : string          // the delivery module that carried it; present exactly when route is "module"
}
```

Input delivery is **not** fire-and-forget, and not a boolean.

- `submitted` — the driver confirmed the agent received it
- `queued` — accepted by the driver, submission unconfirmed
- `refused` — the driver declined; `reason` explains why
- `unknown` — sent, outcome unverifiable

`delivery` names the path that made the receipt (#184): `terminal` (the
session's composer — the text arrives as the user's own turn) or `inbox` (the
runtime's own messaging socket — the text arrives as a peer message). It is
**absent** when the receipt names no path — a refusal made before any path was
chosen, a driver with a single path, or a peer that predates the field — and a
consumer must read absent as "not stated", never as either value. A value this
build does not recognise is read as absent, not as an error: the outcome is the
part that matters.

`refused` is the important one. A driver is expected to protect a session from
input that would corrupt it — for example, injecting text into a prompt that
already holds unsent input the human typed. That protection belongs in the
contract, not in each caller's memory of a past incident.

The five values on the second line are muster #119's own: a driver that
delivers over a target session's own inbox instead of the terminal surface,
when it capability-detects one, reports that surface's own richer vocabulary
rather than collapsing it onto the four above — `refused` is reused there
(same word, same shape), but `held`/`denied`/`expired`/`dropped` have no
honest match among the terminal-surface four and must never be flattened
into one:

- `delivered` — the target's own inbox confirmed the message reached the
  session as a genuine turn
- `held` — the target's own inbox is holding the message for a
  human-approval step before it reaches the session — that surface's own
  last human checkpoint in the path; never reported as a success
- `denied` — the target's own inbox rejected the message outright, a
  decision made by the remote side after the driver already attempted
  delivery (unlike `refused`, decided locally before anything was sent)
- `expired` — the target's own inbox accepted the message and it aged out
  unconsumed
- `dropped` — the target's own inbox discarded the message for a reason
  attributed to neither denial nor expiry

### 2.5 Ack

```
Ack {
  accepted : boolean
}
```

`interrupt` and `close` express intent only (§3; api-http.md §3.3's 202
Accepted). Confirmation of what actually happened arrives later as a state
change on the event stream (§4), never as this call's return value. An `Ack`
carrying a status of its own would be a driver promising synchronous
completion it may not be able to deliver, which §5.6 forbids.

> Origin: Appendix A, F2.

### 2.5a RenameAck

```
RenameAck {
  accepted : boolean
  title?   : TitleSync
}

TitleSync {
  status   : "synced" | "pending" | "failed" | "not_applicable"
  evidence : string           // prose, never parsed (§2.3)
  receipt? : DeliveryReceipt  // the title-sync delivery's own receipt, when one was attempted
}
```

`rename` (§3) does not return the bare `Ack` above, for a reason that is not
symmetry-breaking for its own sake: `rename`'s id half is not intent-only.
By the time a driver's `rename` returns, the multiplexer-level id change has
already happened or it has not — unlike `interrupt`/`close`, whose real
completion is confirmed later, only, on the event stream (§4). Squeezing a
second, genuinely-synchronous fact into `Ack` would violate §2.5's own
doctrine; giving `rename` its own response type, with a field that can
honestly say "pending", does not.

`title` is that second fact (muster #222): on a substrate whose runtime
keeps its own idea of a title apart from the id — the transcript a
title-reconciling client would otherwise trust more than this API — this is
whether that title was brought to the new name too. Four states, and they
are not interchangeable:

- `synced` — the runtime's own record, never the screen, now carries the
  new name as its title.
- `pending` — a title-sync delivery was attempted and neither confirmed nor
  refused within the time this call had. A caller may retry by renaming to
  the SAME name again, which re-attempts only the title half.
- `failed` — the title half did not happen and will not without the caller
  acting again: a busy or stranded composer refused the delivery under the
  same rules `send` already applies (nothing a person typed is ever
  overwritten), the runtime recorded a DIFFERENT title than the one asked
  for, or the name itself was unusable and never sent. This is never the
  multiplexer rename itself failing — that is reported through `accepted`
  being false, or the call erroring outright, before a title sync is ever
  attempted.
- `not_applicable` — this session's runtime keeps no title of its own apart
  from its id, so there is nothing for this half to do. Produced only by
  the service, for a driver that does not implement the optional
  title-syncing capability — never by a driver claiming it for its own
  runtime, which always resolves to one of the three states above.

`title` absent (not merely one of the four states) means nothing is stated
about the runtime's own title at all — a peer built before this field
existed. A consumer reads absent as "not stated" (§5.7), never as a claim
that the title half does not apply.

`receipt` is the title-sync delivery's own `DeliveryReceipt` (§2.4), carried
verbatim rather than re-encoded into `status` — a caller reading "the same
rules as `/input` govern this" needs the same `outcome`/`reason` an ordinary
send already returns, not a paraphrase of them. Absent exactly when nothing
was ever sent: `not_applicable` always, and `failed` when the name itself
was refused before any delivery was attempted.

> Origin: muster issue #222.

### 2.6 Request

```
Request {
  caller : Caller
  expect : Expectation
}

Caller {
  principal  : string       // who is asking — audit trail (§6)
  credential : string       // authority to present onward when proxying (§13)
}

Expectation {
  startedAt? : Timestamp    // the session start time the CALLER observed (§5.4)
}
```

The caller-side context of an operation: everything about *who is asking and
what they believe*, as opposed to what they are asking about. It is a
parameter of every operation in §3.

**Why one type rather than separate parameters.** Two defects in this document
turned out to be the same defect. §13 needed operations to carry the caller's
authority; §5.4 needed them to carry the caller's observation. Neither was
expressible, for the same reason — the operations took domain arguments only.
Both were then moved out of band, where one produced a silent security defect
and the other produced a rule nobody could enforce.

So caller-side context is one parameter with room to grow. The next thing a
caller must tell an operation is a field here, not another break of every
signature in §3.

`credential` is what a remote driver presents to a peer. **A driver that finds
it empty refuses**, and never substitutes its own — the strongest form of that
rule, which the first remote driver adopts, is to hold no credential at all.

`expect` is optional, and its absence is meaningful. A caller that supplies
`startedAt` gets §5.4's real guarantee: *destroy the session I looked at.* A
caller that omits it gets whatever weaker check a driver can offer from its own
sightings — and must be **told which it got** when the operation refuses, so a
weak check is never mistaken for a strong one.

**What does not belong here:** deadlines, which the context already carries and
§4.4 already governs — a second field would be a second source of truth about
the same fact. And anything about the *target*, which stays an argument.

### 2.7 SessionPrompt

```
SessionPrompt {
  question?: string
  options  : string[]      // in order, 1-based when referenced
  selected?: number        // the highlighted option
  nonce    : string        // changes when the prompt changes
  kind?    : PromptKind    // what the driver thinks is being asked; advisory, fails to absent
  multiSelect? : boolean   // the leading options are checkboxes; answer with Response.choices
  freeText?    : boolean   // one option is the free-text row; answer in your own words with Response.text
  tabs?        : PromptTab[]  // a tabbed dialog's question tabs in bar order, Submit excluded; fails to absent
  tab?         : number    // 0-based index of the current tab; absent on the review screen
}

PromptTab {
  header : string          // the tab's label as painted, without its glyph
  state  : "answered" | "current" | "pending"
}

PromptKind =
  | "resume-chooser"      // how to resume a prior session
  | "folder-trust"        // "do you trust the files in this folder"
  | "external-imports"    // "allow external CLAUDE.md file imports" — a directory's instruction
                          // files import a file from outside it; the highlight defaults to the decline
  | "settings-trust"      // an administrator's managed-policy payload asking to be approved
  | "tool-permission"     // a tool asking to run something
  | "bypass-permissions"  // the permission-mode acceptance screen; never produced by option
                          // matching alone — see prompt.go's PromptBypassAcceptance
  | "feedback-review"     // the runtime's feedback-draft card ("1 to review · 2 to send · 0 to
                          // dismiss"); reported only while it hides the composer, never consentable
```

The question a session is blocked on, carried **on `SessionState`** so every
path that reports state reports the question too — a single read, a listing,
and an event. A subscriber learns that a session blocked and what it is asking
in one message rather than having to turn around and ask.

**Options are enumerated, not described.** Evidence prose naming the
highlighted option explains a state; it does not let a caller act on one. Two
boot prompts observed on one fleet:

```
❯ 1. Yes, I trust this folder        ❯ 1. No, exit
  2. No, continue without these        2. Yes, I accept
```

Same shape, same footer, and the safe answer is at a different index. A caller
accepting the highlighted default proceeds in one case and kills the session in
the other. Enumerating is what makes an answer a choice rather than a guess.

**Options are recognised by their numbering, not their wording**, so a prompt
from a future release is enumerated without a new matcher — otherwise every new
screen needs new code, which is how this class of stall stays permanently one
release behind.

**`nonce` exists because an option index is not an option.** A caller reads a
prompt, shows it to a human, and answers seconds or minutes later; in between,
the session may be showing a different question in the same place. Answering by
index would answer that one instead. Supplying the nonce turns a stale answer
into a refusal — §5.4's "a proxy for identity is not identity", in a third
operation.

**Parsing is bounded.** The screen is written by an agent that can print
anything, so an index parsed from it is untrusted input. See Appendix A, F35.

**`kind` names what is being asked, when the driver can recognise it, and is
advisory only.** Nothing in the service behaves differently because of it —
`options` and `selected` remain the answer a caller acts on; `kind` exists so
every caller does not independently re-derive the same classification by
matching option text, which is the fragility Appendix A records paying for
twice already. It fails to empty rather than to a guess (§5.7 again): an
unrecognised prompt carries no `kind`, and empty must never be read as safe to
auto-answer — the default option on a real prompt in this fleet is `No, exit`.
Deciding what to answer stays a caller's judgement; see `prompt.go`'s
`PromptKind` doc comment for the full reasoning and why `bypass-permissions`
is deliberately unreachable by option matching. (Was undocumented in this
block until muster issue #57; see api-http.md §3.3 for its wire history.)

**`multiSelect` says a question is answered with a set, not an index**
(muster issue #176). On a multi-select question the leading options are
checkboxes, their tick state painted into the option text itself, and a single
index flips one box rather than answering anything. So a driver that can
corroborate the shape sets `multiSelect`, and the caller answers with
`Response.choices` (§2.7a). Like `kind` it fails to absent: `false` means "not
recognised as multi-select", and a caller must never send `choices` to a prompt
that does not carry it. That rule also makes the field the capability signal —
a driver that predates `choices` never reports `multiSelect` either, so a caller
following it cannot send a set to a driver that would ignore the set and read
the rest of the body as "accept the highlighted option". Because the tick state
is part of the option text, it is part of what `nonce` digests: an answer to a
tick state that has since changed is refused like any other stale answer.

The shape is recognised whatever the dialog's height (muster issue #219). A
question that wraps over many rows, or options with long descriptions, make a
dialog taller than the fixed window a driver reads, and the tab bar that
corroborates the shape sits above it; the window is then widened to the dialog's
opening rule, and no further, so that a tall question reports `multiSelect` and
`freeText` exactly as a short one does. `question` is the question's own words: the
rule the runtime draws down the left edge of a question too long for one row is not
part of it.

**`freeText` says a question can be answered in the caller's own words**
(muster issue #206). The runtime appends a free-text row (`Type something`)
to every question an agent asks; the row stays in `options` at its own index, so
no numbering moves, but it is not one of the agent's choices, and a caller
drawing the options as a list should draw it as an input. Like `multiSelect` it
fails to absent: `false` means "not recognised as answerable this way", and a
caller must never send `Response.text` to a prompt that does not carry it. That
rule also makes the field the capability signal — a driver that predates `text`
never reports `freeText` either, so a caller following it cannot send text to a
driver that would ignore it and read the rest of the body as "accept the
highlighted option". It is set only on a shape whose key sequence has been
measured, and it is absent again once the row holds text: the row is found by its
placeholder, and after an answer has been typed there is nothing left to find it
by. The nonce digests the options, so typing into the row changes it.

**`tabs` and `tab` say where a tabbed dialog stands** (muster issue #242). A
dialog of two or more questions draws a bar of one tab per question, and the
driver reports it as it is drawn: each tab's header with its state (`answered`,
`current` or `pending`) and the 0-based index of the current one. The bar's
own `Submit` tab is not a question and is not listed; another tab's question and
options are not drawn until it is current, so they are not reported. Like the
fields above they fail to absent — on a dialog with fewer than two questions, on
a bar whose current tab cannot be read, and on a driver that predates them — and
`tab` is absent while the highlight is on `Submit`, where no listed tab is current.
Nothing about answering changes.

**`feedback-review` is a notice, and it is reported only while it is in the way**
(muster issue #215). When an agent drafts product feedback the runtime
paints a bordered card directly above the composer offering three keys — `1` to
review the draft, `2` to send it, `0` to dismiss it. It is not a dialog: it owns
no focus, its keys act only while the composer is empty, and a message pasted
into the composer is delivered as ever. What it does is take rows. A session this
service creates is a 24-row pane (the multiplexer's detached default) and the card
is about seven of them, so the composer's closing rule and mode row fall off the
bottom, the composer can no longer be read, and nothing can be delivered or
confirmed until a person acts on the card. That is the one case in which the
driver reports it — `waiting_input`, a `prompt` of kind `feedback-review`, a nonce
— because only there is "waiting on a person" true from a caller's side. On a
pane tall enough to show the composer whole the session is idle and no prompt is
reported. Text on the composer's row under the card is neither: the state is
`unknown`, the evidence names the card, and `send`, `keys`, `discard` and
`respond` refuse by name, because a digit would be appended to that text.

The options are the runtime's own verbs, `["review", "send", "dismiss"]`, and no
option is highlighted, so there is no default to accept. The keys that answer
them are the card's, not the options' positions: `choice` 3 is delivered as the
key `0`. `respond` refuses `cancel` (Escape on an empty composer dismisses the card
and is sometimes followed by a question about turning drafts off), a missing
`choice`, `choices` and `text`, and — for this kind alone — a missing `nonce`,
because the options are identical on every draft and only the nonce ties an
answer to this one. The key goes once, alone, and the receipt reads what came of
it. Choosing `send` does not send: the runtime asks to confirm first, in a state
the driver names and does not offer to answer (below). The kind is deliberately not
one a create can consent to; whether to review, send or dismiss feedback is a
person's decision about what leaves their machine, and a client puts the prompt in
front of a person and does nothing else with it.

**The card's other states are recognised and are a person's to answer**
(muster issue #217; ADR 217). The card is one box whose *status text* changes
as it is answered, and each state was measured on a real 80-by-24 pane. Read from the
bottom of the box as a whole text (a status that wraps is one text), the four are the
key row above; the send confirmation, `Send without reviewing (full draft + env, no
transcript)? 2 to send · Esc to back`; `Sending…`; and the send error, `✘ Couldn't
send feedback (<reason>). The draft is still queued. Try again later.` with `1 to
review & retry · Esc to dismiss` as the tail of the same text. The confirmation and
the error are a row taller than the key row, so on a short pane even the composer's
`❯` row is off the bottom. Over a composer that cannot be read as a whole, all
three read `unknown` — never `idle` — with the evidence naming the state, and
`send`, `keys`, `discard` and `respond` refuse by name. Over a composer that reads
whole they are idle, like the key row. None is a prompt: the confirmation is the
runtime's own second guard on sending, `Sending…` settles by itself, and the error
asks whether to retry or dismiss; each is answered at the terminal. The key row
itself is a prompt only when its `❯` row is visible, empty and last; with that row
off the bottom the hidden composer's contents cannot be read, and it reads
`unknown` too.

`1` opens the runtime's `/feedback` panel, which replaces the composer (a heavy
rule, the title `Feedback drafts`, the queued drafts, or one draft's editor whose
highlighted `Send feedback` row sends it, conversation attached). It reads
`unknown`, naming the panel, and every delivery is refused: Enter opens a draft or
sends it, `d` discards one. **Escape alone is accepted through `keys`**, since it
decides nothing and is the way out. The question `Turn off Claude-drafted
feedback? 0 to turn off · Esc to keep` is a plain row that hides nothing, so the
session stays idle; it is recognised so that `send` can refuse a message that is
only `0`, which would answer it. A lone `1`, `2` or `0` is refused while the card in
any state, or the question, is on screen.

**`keys(Escape)` is not refused by the card, and `interrupt` is not guarded.**
Measured: with the composer empty Escape dismisses the card and only the card — the
draft stays queued, its file and the footer's count unchanged, and `/feedback` still
lists it — and is sometimes followed by the question about turning drafts off; with
text in the composer it does nothing to the card. It is reversible, the caller has
quoted the screen it read, and Escape is the way out of the confirmation, the error
and the question. Over a card the driver cannot read past, `keys` accepts Escape in the
key row, confirmation and error, refuses it while sending (not measured), and refuses
Enter and the arrows; the receipt of an accepted Escape says what it did. `respond`
receipts name the screen an answer led to. The sent line was not captured and is not
recognised.

**A directory-trust question can also be pre-answered, standing outside a
create request entirely.** (There are two such questions about a directory:
"do you trust this folder" and, when its instruction files import a file
from outside it, "allow external imports". Both are answered the same ways
below, and the one list of roots covers both.) `Consents`/`TrustCwd` scope a caller's answer to
the one session it is creating — the caller named that directory in the same
request, so agreeing to the runtime's question about it is still the
caller's decision, this layer only carries it out. Most sessions on a real
fleet are not created through this service at all, so no request exists for
that consent to travel on. The tmux driver's trust-seed maintainer
(`internal/trustseed`, muster issue #47) closes that gap by writing the
runtime's own record of the answer — per-directory keys in its own state
file — ahead of time, under a fixed, operator-configured set of roots, so
the question is never raised for any session under one, whoever started it.
It seeds the two questions' keys independently and counts them apart
(`trust_seed.granted`, `trust_seed.imports_granted`), so a reader can tell
which is being written. It covers exactly the directories the runtime keys
its answer under — a repository or worktree root, and a directory in no
repository — and never widens that.

This is still the SAME decision the consent table already commits to on
create, only standing rather than per-request: an operator, not this
service, says once which directories are trusted, and the maintainer only
carries that out — it never decides, at runtime, that some other directory
should count as trusted because of anything it observed. See prompt.go's
`PromptFolderTrust` doc comment for where exactly that line is drawn, and
`internal/trustseed`'s package doc for the add-only, race-tolerant mechanism
that lets it write into a file the runtime itself rewrites wholesale while
sessions hold it open.

### 2.7a Response

```
Response {
  choice?  : number     // 1-based option; absent means the highlighted default
  choices? : number[]   // multi-select only: exactly the options to leave ticked
  text?    : string     // an answer in the caller's own words, typed into the free-text row
  cancel?  : boolean    // dismiss rather than answer
  nonce?   : string     // the SessionPrompt.nonce being answered
}
```

What `respond` (§3) delivers. Absent `choice` means "accept whatever is
highlighted", which is what a human pressing Enter gets and what a caller
usually means. `cancel` exists because a caller that likes none of the options
needs a way to say so other than picking one anyway.

**`nonce` is §2.7's corroboration, arriving at the call that answers it.** A
caller reads a prompt, shows it to a human, and answers seconds or minutes
later; by then the session may be showing a *different* question in the same
place, and an answer submitted by index alone would be applied to it
silently. Quoting the nonce back turns that into a refusal. It is optional
only for a human answering something they are looking at right now; an
automated caller that omits it is choosing to answer whatever happens to be
on screen, and a driver that answers unchecked must say so in the receipt.
(Undocumented in this block until muster issue #57 — it was already on
the wire, api-http.md §3.3's `respond`, and on `response.go`'s `Response`
since before this block was last touched.)

**`choices` answers a multi-select question with a set** (muster issue
#176), and only a prompt carrying `multiSelect` (§2.7). It names the END state
— these boxes ticked, every other box clear — so the driver flips only the
boxes that differ from the screen, and sending the same set twice asks for the
same result twice; a sequence of toggles would not have that property, since a
resent toggle undoes itself. With a nonce, a resend after the first call
succeeded is refused outright: the question has moved on. After a partial
failure the tick state has changed, so a resend is refused too, and the caller
reads the state again and sends the same set with the new nonce.

`choices` then moves the dialog ONE step on — to the next question, or to the
dialog's review screen — and stops. It never confirms that review screen:
handing the answers over is its own prompt with its own nonce, answered with
`choice`, so the step that commits is never taken on the strength of an
earlier read. `choices` cannot be combined with `choice` or `cancel`, and
cannot be empty — an empty set, marshalled onward with `omitempty`, would
reach the next hop as an empty body, which means "accept the highlighted
option". Those are faults in the request, rejected before any driver sees it.

**`text` answers through the free-text row** (muster issue #206), and only
a prompt carrying `freeText` (§2.7). The driver puts the highlight on that row,
types the text, reads the row back, and only then confirms; the receipt is
`submitted` only once the answered question has left the screen, and it reports
the size of what was typed, never the text. On a single-select question — alone,
or one tab of a multi-question dialog — the text is the whole answer and
confirming it moves the dialog on as `choice` does. On a multi-select question it
is sent with `choices`: the boxes to leave ticked and the free-text row's own
content together name the end state, so `text` alone means no box is ticked, and
the dialog moves ONE step on and stops, never confirming its review screen.
`text` cannot be combined with `choice` or `cancel`.

It is the one field of `Response` a wire type carries as a pointer, on purpose.
Every field is `omitempty`, and a plain string would drop `"text": ""` when a
driver that relays to a peer marshals the body onward: the peer would receive a
body with no answer in it, and no answer means "accept the highlighted option" —
the trap an empty `choices` is refused for, closed the same way. Measured on the
one runtime a driver exists for: confirming the free-text field while it is EMPTY
declines the WHOLE dialog, every question in it, not one. So an empty or blank
`text` is a fault in the request, rejected before any driver sees it; a driver
also refuses text that is empty once control characters and surrounding
whitespace are removed, never confirms the field before reading the text back on
its row, and reports `unknown` — confirming nothing — when the text did not land
or landed altered. `text` is held to the same byte limit and the same control-byte
sanitising as `input`; the composer's own syntax refusals (a leading `!` or `/`)
do not apply, because the answer field was measured not to read them.

### 2.8 AttachHint

```
AttachHint {
  kind      : string      // "multiplexer"; unknown kinds are unsupported, not guessed
  target?   : string      // the substrate's own handle for this session
  command?  : string[]    // argv, run ON THIS SESSION'S MACHINE, to take over
  readOnly? : string[]    // the same attachment, without a keyboard
  shared?   : boolean     // attaching does not evict another viewer
}
```

Optional on `Session`. Absent means the driver has no answer, which is a real
answer (§5.7) and the correct one for a substrate with no interactive
attachment.

**Why this is in the model at all.** Every other thing a supervisor does to a
session — read it, drive it, answer it, end it — is expressible without knowing
what the substrate is. One was not: giving a *human* a terminal. A supervisor
that still shells out to a multiplexer for that has not been freed of the
substrate, it has been freed of it everywhere except where its users touch.
This is the difference between a driver boundary and a leak.

**Why a hint and not an operation.** There is deliberately no `attach` in §3.
Attaching gives a terminal to a person, and no person is on the far end of this
API — an HTTP request is. A service that "attached" could only attach something
of its own, which is either useless or an impersonation.

**Why local argv and no remote form.** The service knows the machine it runs
on; it does not know how a caller reaches that machine. Synthesising a remote
invocation would assert a network topology it cannot see — the same reason §7.2
requires a peer's address to be one the operator confirmed rather than the
peer's own idea of its name. The client composes remoteness, because the client
is the one that knows it.

Argv rather than a command string, because session ids are operator-chosen and
routinely contain emoji and spaces; a string invites interpolation into a shell
and the quoting bug that follows.

**`readOnly` is not a nicety.** A supervisor offering "watch" and "take over"
as the same button will corrupt somebody's session by leaning on a keyboard.
A client that cannot tell the two apart offers the dangerous one.

**This is the local answer to "how do I reach this session"; it is not the
only one.** A session can also be reachable on a surface the RUNTIME
operates — see §2.13 `RuntimeSurfaceRef` — which is a different question
(identity on a remote surface, not a local terminal invocation) with a
different answer shape. §2.3's `controlChannel` is a third, narrower
question again: not where the surface is, but whether it is healthy right
now.

### 2.9 ConversationRef

```
ConversationRef {
  known    : boolean     // a lookup happened and produced an answer
  id?      : string      // the RUNTIME's identifier for the conversation
  source?  : string      // "derived" | "captured"; required when known
  evidence : string      // prose for humans, present either way; never parse it
}
```

Optional on `Session`. Three states, and the difference between the first two
is the whole point:

| shape | means |
|---|---|
| field absent | **nobody looked** — no such record on this substrate, or the lookup is not configured |
| `known: false` + `evidence` | we looked and could not tell, and the evidence says why |
| `known: true` + `id` + `source` | we can name the record, and `source` says how that was learned |

**Why the model carries this at all.** Everything else a driver reports about a
session ultimately comes from the process describing itself — the screen it
chose to paint, read once for status and again for a receipt. When the runtime
is the thing that is wrong, all of those readings agree and all of them are
wrong: 51 of 52 sessions on one machine read healthy while the account beneath
every one of them was refusing work. A record the runtime writes for its own
purposes, unasked, is an **independent witness**, and it is the first source in
this model that is not an echo. Knowing *which* record belongs to a session is
worth having before anything ever opens one: two sessions claiming one record,
or a live session with none, are facts about identity no screen read produces.

**Why `source` is mandatory when known.** A caller that cannot tell a value
*read* from a value *matched* will corroborate against a guess and believe it
is evidence — which is this field's own motivating failure, one level up. §2.3
already separates a structured read from a screen guess, and §4.3 separates a
peer's declaration from an unconfirmed floor; this is the third instance of the
same problem and deliberately takes the same shape rather than inventing a
fourth. `derived` means the service matched the record to the session;
`captured` means a driver observed the identifier as the session was created.

**Why `evidence` is present on success too**, unlike the `known`-plus-`reason`
pairs elsewhere: a reason only exists for a "no", but two derivations are not
equally strong. "The only record carrying this session's name" and "the only
one left after two were ruled out" are different answers, and a caller deciding
whether to act on the identifier is entitled to know which it got.

**A driver must refuse rather than choose.** Where several records could be the
session's, `known` is false and the evidence says how many were not chosen
between. Picking the most recently written one is the failure this section
exists to prevent: a guess shaped like a reading, right often enough that
nobody checks it.

**A second witness, when the runtime keeps one.** Matching a record to a session by
name and date is right for a session that began its conversation and wrong for one
that continued a conversation begun before it (a resumed session), or that was never
given a name. Where the runtime also keeps a record of each running process naming
the conversation that process is in, a driver may identify the conversation from it —
provided the record is corroborated as belonging to the process running now (its start
time equals the live process's), names the session's own working directory, and carries
an identifier that can only be one file's name inside the record root. A record that
cannot be corroborated is not used, and the answer is the name-based one with the
reason it was not. Where the two sources name different conversations the driver
refuses, exactly as above: `known` is false and the evidence names both. `source` is
unchanged — the identifier was matched to the session by this service, not dictated —
and the evidence says which rule answered (muster #182).

**An answer belongs to one run of a process, not to the life of a session** (muster
#202). The process in a session's pane can be replaced while the session — its name, its
pane, its creation time — stays as it was: a recovery tool that relaunches the runtime in
place starts a new process, and that process may be in a different conversation.
`conversation` describes the process running now. A driver that remembers an answer must
remember which process it was established against, and must not serve it for another: it
answers afresh whenever the process has provably changed (a different pid, or the same pid
with a different start time where one was measured), and an absent or unreadable pid is
never taken as a change. The name-based match is no evidence about the new process — it
names the record the session's *first* process titled, which is still there, still carries
the name and still looks like the only candidate — so once a process has been replaced, a
name-derived answer that names a conversation an earlier process held is set aside. What is
left is the new process's own record; until the runtime has written it, `known` is false
with evidence saying so, never the predecessor's identifier. A relaunch that continues the
same conversation reports it exactly when its own record says so. The limit: a replacement
the driver did not observe — the service restarted after the relaunch — cannot be retired,
and the session is answered as one that was never replaced would be.

**...nor to one conversation inside that process** (muster #203). The runtime can
start a new conversation in the *same* process — the pid and its start time do not move —
and the only thing that changes is the conversation the runtime's own record of the process
names. A driver that remembers an answer must therefore check the remembered conversation
against that record on every read it answers from memory, and answer afresh when the
process's own record names another one, treating the earlier identifier exactly as a
replaced process's is treated: retired, and no longer evidence for the session. The check
is a file read — it must not spawn a subprocess, so a listing stays a constant number of
spawns however many sessions it names — and it acts only on a record it can tie to the
process the answer was established against, by the start time the answer was corroborated
with. A record that cannot be read, that names a different working directory, or that
carries a different start time is absence of evidence and never a change. The limit: an
answer established with no start time (from the name alone, while the record was unusable)
was never tied to a process instant, and is left as it is.

### 2.10 ResumeOutcome

```
ResumeOutcome {
  requested : string      // the conversation id `create` asked the runtime to resume
  honoured? : boolean     // nil until the session's own ConversationRef resolves
  evidence  : string      // prose for humans, present either way; never parse it
}
```

Optional on `Session`, and present only when the session's `create` set
`resume`. Three states, the same shape §2.9 already takes and for the same
reason:

| shape | means |
|---|---|
| field absent | no resume was requested at creation — never a claim that one was requested and honoured |
| `honoured` absent + `evidence` | a resume WAS requested, but this session's own `ConversationRef` has not resolved yet — too early to say either way |
| `honoured: true`/`false` + `evidence` | the conversation resolved, and this states whether it is the one that was asked for |

**Why this exists (muster #72).** A `create`'s `resume` is a single
request field with no receipt of its own — the runtime is free to ignore it
and start a fresh conversation instead, and measured under a concurrent
burst it does: silently, no refusal and no degraded status, the created
session reading as an ordinary healthy start on a conversation nobody asked
for. Every other write in this API carries the opposite discipline —
`DeliveryReceipt` reports `unknown` rather than claiming a delivery it
cannot confirm, `keys` refuses an uncorroborated screen, a capability read
distinguishes `observed` from `assumed`. `create`'s `resume` was the one
write that did not, and the recovery this model exists to make survivable
is exactly a concurrent burst by construction.

**Why it is not folded into `ConversationRef`.** "Which record identifies
this session" and "did the create that made this session get what it
asked for" are different questions — a session that never requested a
resume still answers the first one every time it is listed, and carries no
`ResumeOutcome` at all.

### 2.11 PromptDelivery

```
PromptDelivery {
  outcome?   : string        // "submitted" | "queued" | "refused" | "unknown"; nil until resolved
  evidence   : string        // prose for humans, present either way; never parse it
  waitingOn? : WaitingReason // muster #126: machine-readable class for `evidence`, while pending
}
```

Optional on `Session`, and present only when the session's `create` carried a
`prompt`. Three states, the same shape §2.9 and §2.10 already take:

| shape | means |
|---|---|
| field absent | this create carried no prompt — never a claim that one was carried and delivered |
| `outcome` absent + `evidence` | a prompt WAS accepted at creation and delivery has not resolved yet — `idle` is not evidence of loss while this holds, and a caller must not re-send |
| `outcome` + `evidence` | resolved, in the same closed set `send`'s `DeliveryReceipt` answers with |

**Why this exists (muster #86).** `create`'s `prompt` is delivered AFTER
the process starts and after the runtime has painted a composer to receive
it, so the 201 response is written before delivery can be known — unlike
`send`, which answers only once delivery has finished. Measured: a session
created with a prompt, polled ~12s later, read `status: idle, evidence:
"interface painted, composer empty, no turn yet"` — the correct
classification for "up and waiting with nothing sent", and indistinguishable
from "up and waiting, and an accepted prompt has not been delivered yet".
The caller concluded the prompt was lost and re-sent it four times. The
natural client loop — create, then poll until `idle` or `waiting_input` —
returns on exactly this window by construction.

**The pending `evidence` is LIVE, not a static placeholder (muster
#125).** While `outcome` is still absent, a driver trying as hard as possible
to deliver — holding the prompt and retrying rather than racing a fixed
timer and dropping it — must keep `evidence` current: it names WHY the
prompt has not landed yet ("still starting", "parked on a folder-trust
dialog awaiting a keypress"), updated as that reason changes, not only a
generic "accepted at creation" sentence that never moves. A caller polling
mid-wait sees a diagnosis, not a mystery. This does not add a fourth state —
`outcome` absent + `evidence` is still the one pending state — it says what
a driver owes the `evidence` string while it holds.

**`waitingOn` is that same diagnosis, machine-readable (muster #126).**
#125 fixed the case where nothing at all told a caller why a prompt was
pending; #126 is the finding that "why" as prose only helps a human. Seven
distinct causes measured on one fleet in one day all read identically through
this API as "the session did not start" — a dialog on screen, a composer
already occupied, a runtime not yet ready — and a control surface could not
branch on any of them without parsing English a driver is free to reword.
`waitingOn` carries `WaitingReason`'s same closed vocabulary — `prompt` for a
dialog (`prompt` itself, §2.7, still names the specific question, exactly as
it already does for an ordinary `waiting_input` session), `unsent-input` for
an occupied composer, or `starting` for no composer painted at all yet — set
from the SAME observation that produced `evidence` in the same write, so the
two can never disagree about one wait. Absent means unclassified (§5.7): a
driver that has not resolved the initial "which of these" question, or a
cause outside the three above, leaves it out rather than guessing. Always
absent once `outcome` resolves — it answers a question that only exists while
pending.

**Why this needed its own field rather than reusing `SessionState.waitingOn`.**
`waitingOn` on `SessionState` is documented as populated only when the
session is `waiting_input`; the measured harm above is a session correctly
reading `idle`. Writing that field here would mean either lying about the
present status or breaking its own stated contract — the same reasoning
already applied to `SessionState.LastTurn` in the Go doc comment: a fact that
no status member carries without lying about the present gets its own field.
`PromptDelivery.waitingOn` is that field: same vocabulary, deliberately not
the same slot on the session.

**A caller reading this after a create that carried a prompt should wait for
`outcome` to resolve, not treat `idle` as the signal** — see the client
guide's polling section, which used to advise the opposite.

### 2.12 PinOutcome

```
PinOutcome {
  agent?  : PinResult
  model?  : PinResult
  effort? : PinResult
}
PinResult {
  requested : string      // the value `create` asked to pin
  honoured? : boolean     // nil until this driver can compare requested against applied
  applied?  : string      // what the runtime is actually using, when it can be named
  source?   : string      // "observed" | "declared"; required when honoured is known
  evidence  : string      // prose for humans, present either way; never parse it
}
```

Optional on `Session`, one field per pin `create` asked for. Three states per
pin, the same shape §2.9-§2.11 already take:

| shape | means |
|---|---|
| the field absent | this pin was not requested at creation — never a claim that one was requested and honoured |
| `honoured` absent + `evidence` | requested, and this driver cannot say what the runtime applied — not yet, or not ever |
| `honoured: true`/`false` + `evidence` | resolved: `source` says how, `applied` names what the runtime is actually using when it can be named |

**Why this exists (muster #84).** `agent`, `model` and `effort` are
already documented as hints a driver must refuse rather than silently drop
(§2.1). Measured: a value beginning with `-` failed an argv-injection guard
before that rule existed, the flag was never appended, and the create
response echoed the REQUESTED value back on `Session.agent`/`.model` — the
one caller in a position to notice was told the pin had been applied. An echo
is not a weak answer; it is a fabricated one.

Two changes follow from this, not one: a value that would be misread as a
flag is now refused outright at creation (`invalid`, naming the field) rather
than silently dropped — closing the *detectable* case. But a value that
reaches the runtime intact can still be defaulted or ignored there with
nothing to refuse, which is what `pins` exists to make answerable: a driver
that passes a pin on a command line and has no channel back from the runtime
reports `honoured` unresolved rather than asserting success it never
confirmed.

**`Session.agent`/`.model` are the APPLIED values now**, reported only when
the driver observed them — the same real-answer rule `startedAt` already
follows — never an echo of what was requested. What was requested lives in
`pins.*.requested` instead, correctly labelled as a request rather than a
fact.

### 2.13 RuntimeSurfaceRef

```
RuntimeSurfaceRef {
  known?   : boolean     // nil = requested, unresolved; false = settled none; true = the address below is real
  kind?    : string      // the mechanism, in the abstract; required when known — "control-channel" today
  target?  : string      // opaque identifier on that surface; required when known
  source?  : string      // "observed" | "derived"; required when known
  evidence : string      // prose for humans, present in every state; never parse it
}
```

Optional on `Session`. **Four** states — one more than §2.9-§2.12 — because
here "not yet" and "never" are opposite facts rather than the same
operational one:

| shape | means |
|---|---|
| the field absent | nobody looked: this driver does not report a runtime surface, or its runtime operates none — see `reportsRuntimeSurface` (§4.3) |
| `known` absent + `evidence` | a surface WAS requested at creation and has not resolved yet. The runtime registers asynchronously, after the process starts. Never read as "no" |
| `known: false` + `evidence` | settled: this session has no such surface — the create opted out, or the runtime declined. A caller polling for one may stop |
| `known: true` + `kind` + `target` + `source` | the address, and `source` says how it was learned |

**Why this exists, and why not folded into `attach` or `conversation`
(muster #85).** A session created with the default remote-control
setting really does register with the runtime's own hosted surface, moments
after the process starts — measured on a live fleet. Nothing in any response
said so, and nothing said how to reach it. `attach` (§2.8) answers a
different question: a human's terminal, already on this session's own
machine. `conversation` (§2.9) names the transcript record, a different
identifier with a different lifetime — conflating the two is a recorded
source of false negatives elsewhere in this model. What remained was an
identity question with nowhere to live, so this takes `conversation`'s own
three-state shape, extended to four for the reason above.

**No vendor or product name appears in this section, in `kind`'s values, or
in any evidence string produced for it** — deliberately. The field is
addressed at "a surface the runtime operates", vendor-neutral, so a driver
whose runtime hosts nothing can still speak the vocabulary honestly (`false`
+ `reportsRuntimeSurface: false`, or `known: false` naming the opt-out) and
a future driver over a different runtime is not left lying or leaving the
field permanently empty for a mechanism it does have, under a different name.

**`known: true` requires corroboration, not just a dictated identifier.** The
identifier a create supplies is a value THIS SERVICE chose; publishing it as
`target` before the runtime has confirmed the surface came up under it would
be muster #84's defect arriving in a second field — a request reported
as a fact. A driver must observe the runtime's own report of the surface
before setting `known: true`.

**Identity, latched, not liveness, polled.** Once corroborated, `known` stays
true for the life of the record even if the surface later goes quiet — that
is `state.controlChannel`'s question (§2.3), which already distinguishes
`failed`/`reconnecting`/`active`. Unresolving `runtimeSurface` on every
dropped connection would make an identity field flicker with a health signal.

### 2.14 IdentityAssertion

```
IdentityAssertion {
  asserted    : string      // the identity this machine last asserted for this session
  drifted?    : boolean     // nil = asserted, not yet corroborated against a live read;
                            // false = the runtime carries `asserted`; true = it carries `carried` instead
  carried?    : string      // what the runtime carries right now; present only when drifted is true
  assertedAt? : Timestamp   // when `asserted` was last SET; optional in every state
  evidence    : string      // prose for humans, present in every state; never parse it
}
```

Optional on `Session`. **Four** states, one more than §2.9-§2.12, for a
specific reason rather than symmetry with §2.13:

| shape | means |
|---|---|
| the field absent | this machine has asserted no identity for this session at all — an adopted, foreign or cold-store session it never named, or a driver with no state store. Never a claim the identity agrees |
| `drifted` absent + `evidence` | an identity WAS asserted, and no read has yet matched the durable record to a live run to corroborate it. Clears on the next read. Never read as "no drift" |
| `drifted: false` + `evidence` | settled, as of THIS read: the runtime carries exactly what this machine asserted |
| `drifted: true` + `carried` + `evidence` | this read found them disagreeing — muster #97's defect, machine-readable |

**Why this exists (muster #102, on the back of #96/#97).** #97 measured a
rename that returned `202 accepted`, read back correct for roughly half an
hour, then silently reverted — id, name and attach target all restored, with
nothing in any response saying so for that whole window. #97's own fix made
the disagreement detectable and self-repairing: a driver's `List` now compares
what it just enumerated against a durable record of what it last asserted, and
puts a name back when the two disagree. But the only place that fact surfaced
was a prose sentence appended to `state.evidence` — a field §2.3 tells every
caller never to parse. A fact that exists, is durable, and is reachable only
by pattern-matching prose is not on the wire; this field is.

**Why the third state is real, not invented for symmetry.** An asserted
identity is recorded the instant a create or a rename resolves it — before
any read has had a chance to corroborate it against a live run, matched by
`(pane, created)` so a later rename does not orphan the match. A record still
missing that pairing cannot yet be told apart from a drifted one, so this
state reports it as unresolved rather than guessing either way. It clears on
the very next read, the same "clears in seconds, never read as no" property
§2.13 already establishes for `runtimeSurface`.

**Not latched, unlike `runtimeSurface`.** §2.13's `known: true` stays true for
the life of the record once corroborated, because it names an address, not a
health check. `drifted: false` here is the opposite: a claim about THIS read
alone. The runtime's name for a session can change again the moment after a
read reports agreement, and it is the *next* read that says so — never a
cached verdict from an earlier one.

**The repair, if there is one, lands on the next read, not this one.** The
read that first discovers a drift reports `drifted: true`; the driver's own
repair of it is attempted afterward, once this response already exists. A
caller polling twice in the ordinary case sees `drifted: true` then
`drifted: false`. A single `drifted: true` is not a permanent condition, and a
`drifted: false` is not proof no drift ever happened.

**Why there is no `source` field.** `pins` and `runtimeSurface` each fork a
real question — observed against declared, observed against derived. Here
there is exactly one source in every state: `asserted` comes from this
machine's own durable record, `carried` from the live enumeration in the same
read. A field with one possible value would teach a caller nothing.

**Why "contested" is not a state here.** A driver that gives up repairing a
drift — the wanted name is live under another session, or a bounded number of
attempts is already spent — is reporting a fact about its own repair policy,
not about what this read observed; folding a decision about a future action
into a field describing the present is the same category of error `agent`/
`model` are documented to avoid (muster #84). It is also not a stable
fact: a name taken by another session becomes free the moment that session
closes. The operational need is met elsewhere, outside this wire contract. A
field added to a published contract later is additive; retracting one is not
— so the narrower shape is the one that ships.

### 2.15 DeliveryLane

Which delivery lane a session's input takes, when an optional external delivery
module is configured on the machine that owns the session (muster #185).
Optional on `Session` as `delivery`.

```
DeliveryLane {
  lane            : string    // the module's name while its lane is live, else "terminal"
  clientConnected : boolean   // a module holds a live channel to this session's agent process
  evidence        : string    // prose for humans: what the answer rests on — do not parse
  since           : Timestamp // when `lane` last changed value
}

DeliveryModuleStatus {
  name      : string          // the module's name; also the value a caller writes in `route` to force it
  status    : "starting" | "available" | "unavailable" | "disabled"
  reason?   : string          // why status is not "available"
  protocol? : number          // what the module reported in its handshake
  version?  : string
  platform? : string
  peerCheck?: boolean         // the module can verify who is on the other end of its channel
  lanes?    : { [state: string]: number }   // sessions by lane state
}
```

**Absent is not `terminal`.** The `delivery` field is absent when nothing is
configured or nothing has been probed for this session: a machine with no module
enabled, a session this machine did not launch itself, or a peer built before the
field existed. A consumer reads absent as "not stated". This is the rule
`DeliveryReceipt.delivery` and `Session.attach` already follow (§5.7).

**A module that cannot check its peer is not live.** A module that reports
`peerCheck: false` is treated as not live for every session, and sends fall back
to the built-in path. Nothing is delivered over a channel whose far end the
module cannot verify.

---

## 3. Operations

```
create(req, spec)              -> Session
send(req, ref, text, opts?)    -> DeliveryReceipt
respond(req, ref, response)    -> DeliveryReceipt
state(req, ref)                -> SessionState
interrupt(req, ref)            -> Ack
close(req, ref)                -> Ack
rename(req, ref, to)           -> RenameAck
discard(req, ref, digest, force?) -> Ack
keys(req, ref, key, expect)    -> DeliveryReceipt
list(req, filter?)             -> Collection<Session>
subscribe(req, filter?)        -> EventStream
```

**Labels are not an operation, and not a driver's concern** (muster #153). No substrate has a
place to keep them and no driver observes them, so the service stores them itself, keyed by the
session's `(runtime, id)` and corroborated by its `startedAt` (§5.4): a new session under a recycled
id never inherits the old one's labels. They follow a rename, and they are forgotten when the
session is closed through the service or no longer appears in a complete, unfiltered listing — never
on a filtered or failed one (§5.7). A `Session` read from a service that stores labels always
carries `labels`, `{}` when there are none; one read from a service that predates them carries no
such key, and that absence is how a relaying service tells the two apart.

**`keys` delivers one raw key event** — `Up`/`Down`/`Left`/`Right`/`Enter`/
`Escape`, and `BTab` (Shift+Tab, which cycles the runtime's permission mode and
is not a dialog key; ADR 188 — its effect is readable in
`SessionState.permissionMode`, ADR 194) (api-http.md §3.3, `POST …/keys`) — to the
full-screen dialogs `respond` cannot answer, corroborated by `SessionState.screenDigest` (§2.3)
the same way `discard` corroborates against `composerDigest`. A driver
declares whether it can do this at all through
`DriverCapabilities.deliversRawKeys` (§4.3); one that lacks a screen to
capture answers `unsupported` (§5.6) rather than approximate one, the same
shape `AttachHint` already uses for a substrate with no interactive
attachment (§2.8) — except here the model has a place for the absence to be
absent *from*, which is what muster issue #59 decided was missing. It
joins this table as that ruling: wire-only and undecided on purpose, until
#59 decided it.

> **This does not resolve §5.1, and the table above should not be read as if
> it did.** §5.1 asks every operation to express a question, never a
> mechanism — `state()`, not `readScreen()`; `send()`, not `typeKeys()`.
> `keys` delivers literal `Up`/`Down`/`Enter`: it IS the mechanism, and no
> amount of capability-gating changes what it hands a caller. What the gate
> buys is not compliance with §5.1 but honesty about the violation. An escape
> hatch every driver is silently assumed to support is a fork in the model
> nobody decided; one that must be declared, and that a driver may honestly
> refuse, is a documented asymmetry instead — declared and bounded, not
> resolved, in the same sense §14's open defects are named rather than hidden.
> `keys` exists because `respond` — the question-shaped answer to a prompt —
> cannot reach a full-screen dialog the classifier never recognised as a
> prompt at all; the long-term aim stays what it always was, for `respond` to
> grow until it covers those dialogs too (muster issue #49, the same
> surface). Ruling #59 is a decision to stop pretending the hatch is not part
> of the model while it exists — it is not a decision to stop trying to make
> it unnecessary.

`respond` answers a prompt a session is blocked on, and is a separate
operation rather than a flag on `send` for a reason that is not stylistic:
`send` must guarantee it never produces a keystroke, so that a message
containing something like `C-c` cannot interrupt the session receiving it. That
guarantee is what makes a prompt unanswerable by `send`. A driver must refuse
`respond` when nothing is being asked — a keypress delivered to a session that
is not at a prompt is consumed by whatever it was doing.

`Response` names a **choice**, not a key (§2.7). §5.1 says the interface
expresses questions rather than mechanisms, and "press Enter" would bind every
future driver to this substrate's idea of confirmation.

**`send`'s no-keystroke guarantee covers the substrate a driver uses to
deliver text; it says nothing about what the runtime on the far end does with
the bytes once they arrive intact.** Some runtimes read certain caller text as
their own local syntax rather than as a message — a command to run directly,
for instance — and a driver whose runtime does this must refuse that text
outright: never escaped, never mangled, never delivered and hoped about. A
driver declares the patterns its own runtime treats this way; a pattern
enforced by the service rather than by the driver that knows the runtime is
not a pattern, it is a convention that holds until a second driver forgets it
— §2.1 makes the identical argument for naming. This refusal describes the
TEXT, not a session's state, so it precedes every session-state check a
driver makes, whether or not the addressed session exists, and
`resumeIfStranded` cannot complete it: refused text never reaches a composer
to strand. See muster issue #53, and the driver that adds a second
runtime brings its own patterns rather than inheriting this one's.

Every operation carries a `Request` (§2.6), reads included. A driver cannot
compile without deciding what to do with it, which is the point: the rule it
serves — §13's "a proxy presents the original caller's authority, never its
own" — is unenforceable if the operations have nowhere to carry a principal.

`send` may carry **resumeIfStranded**, which completes a delivery this service
already made and could not confirm. §2.4's refusal is what makes it necessary:
after an unconfirmed delivery the text is in the composer, a second send is
refused by the very rule protecting it, and nothing else submits.

A driver may honour it by establishing, from its own record of what it
delivered, that the text is its own: the composer digest it took when the
delivery stranded, the recorded text read back against the composer's rows,
or — for a long message the runtime collapsed to a summary (F49) — the
summary marker it saw that paste land as. Text the driver did not place is
never submitted: composer contents are not evidence that anybody meant to
send them.

**The draft rule (#180)** governs every clear and every submit of composer
text, for `resumeIfStranded` and its sibling **replaceIfStranded** (muster
#112) alike: a driver never clears or submits text in a composer unless (a)
its own record proves the text is its own stranded delivery — a live record,
or one kept after the live record lapsed or was replaced, used only as this
proof — or (b) the caller supplies the composer's current digest (`send`'s
expect, the same `ComposerDigest` `discard` quotes back below), proving it saw
what it asks to have cleared. Otherwise the driver refuses and keeps the text:
it may be a person's draft, and a flag on the request is a wish, not proof.
When either proof holds and no live record backs the composer, both flags
converge on one door (muster #135): clear the composer, then deliver THIS
call's text. The foreign text itself is never submitted, and a caller that sets
neither flag still gets the original, unqualified refusal.

**A `queued` receipt is not proof a turn started (#240).** A submit confirmed
by the runtime queueing the text, or by the composer reading empty, shows the
text left the composer; a runtime that queued it can hand it back, unsent. For
such a confirmation the driver keeps a provisional record of the delivery — the
same kind the draft rule keeps for a lapsed strand, and proof only while the
runtime's own transcript does not show a turn started on the text. If the text
returns, the session reports `waiting_input` with `waitingOn: unsent-input` and
`strandedDelivery: true`, and the sender's own `resumeIfStranded` (same text,
same `from`) finishes it. See `docs/adr/240-a-queued-delivery-is-not-gone.md`.

This record belongs to the terminal path. A delivery module answers `unknown`
for a bare enqueue and `queued` only for a turn the runtime accepted, so no
provisional record is kept on that lane; and on a live lane a resume is refused
(#257), the recovery being `discard` with `expect`, then a fresh send. The inbox
lane keeps none for the same reasons. See the ADR's "Module and inbox lanes".

`discard` removes unsent composer text **without submitting it** — the verb
between "run it" and "destroy the session holding it", which was missing. `send`
refuses to append to a busy composer (§2.4), which is right and left a caller
with nowhere to go: text it did not write, must not submit, and could only
escape by killing the session.

It destroys typing, so it corroborates like a destroy. The caller quotes back
`ComposerDigest` from a read; a mismatch means the composer changed since — most
likely somebody typing this second — and is refused. **Discarding blind is
refused outright rather than treated as permission**: "I do not know what is
there, remove it" is precisely the request this must not honour.

Clearing an already-empty composer succeeds. A caller that timed out and retried
must not be told it failed for having worked, and nothing is destroyed by
clearing nothing.

Colab-fleet #136: a residue a driver has already proven, by evidence, that its
ordinary clear pass cannot move used to dead-end at one documented remedy —
destroy the session holding it. Disproportionate: a session carries a
conversation, a bridge, in-flight work, and for a caller that binds them, a
claim and a worktree, none of which respawning recovers. `force` is the door
past that dead end — set only once a prior call has already been refused as
proven-futile against this exact residue, it authorises a driver to reach for
a stronger clear mechanism than its ordinary pass. It is not a second, looser
corroboration path: `ComposerDigest` is required exactly as it always was,
because a forced clear is *more* destructive than the ordinary one, not less
— the caller must still prove it saw what it is asking to force-clear. A
driver may ignore `force` (treat it as unset) before a prior call has proven
this residue futile; forcing is a remedy for proven futility, not a shortcut
around attempting the ordinary pass first.

`rename` changes a session's **id**, and that is not a slip of wording. On a
substrate where the id is the name an operator sees and every command targets,
a "label" stored beside it would rename the session in this API and leave the
human's terminal saying the old thing — solving nothing. So the handle itself
moves.

That makes ids **mutable as well as recyclable**, which sounds alarming until
you notice §5.4 already forbids acting on an id alone. `startedAt` is the
stable identity and it survives a rename; a mutable id is the same rule with a
sharper edge, not a new hazard.

Three consequences are normative:

- **`rename` is corroborated exactly as `close` is.** Its failure is quieter
  than a wrong close and no less bad: it succeeds silently on a session the
  caller never meant, and leaves that session wearing a name belonging to
  somebody else's work.
- **The service emits `session.renamed` carrying both ids.** Without it, a
  subscriber filtering by id sees the old id go quiet and a stranger appear —
  indistinguishable from a death and a birth, which is the one reading a
  rename must never produce.
- **A driver whose runtime keeps its own title follows the id with it**
  (muster #222; RenameAck.Title above). The three names a session
  carries from birth — its multiplexer id, its remote-control binding, and
  the runtime's own idea of its title — are one string by construction
  (`internal/drivers/tmux/naming.go`'s own invariant). A rename that moves
  only the multiplexer's half of that leaves the other two drifted apart,
  and a client that trusts the runtime's own record over the multiplexer's —
  a title-reconciling client is exactly this — "corrects" the disagreement
  by reverting the rename it never saw confirmed. `rename` bringing the
  title along too is what keeps that invariant true across a rename and not
  merely at birth.

A driver on a substrate with no renaming returns `unsupported` (§5.6).

`subscribe` is not optional garnish. Federated callers must be able to learn
about state changes without polling — see §5.5.

> The table above once read `list(filter?) -> SessionRef[]`. Why a bare array
> cannot satisfy §9's envelope rule or §13.2's adopt-don't-resynthesize rule:
> Appendix A, F3.

---

## 4. Drivers

A driver implements the operations above for one runtime on one machine.

### 4.1 Local drivers

Wrap whatever actually runs an agent: a terminal multiplexer session, a managed
subprocess, a runtime with its own HTTP server.

### 4.2 The remote driver

A driver whose implementation is an HTTP client to a peer `muster`.

This is the entire federation design. Cross-machine operation is not a feature
layered on top of the abstraction — it is one implementation of it. If the
interface cannot express "a session on another machine," the interface is
wrong, and that is the cheapest available test of it.

### 4.3 Capability declaration

Drivers differ in what they can do. A driver declares its capabilities, and
callers must degrade rather than assume:

```
DriverCapabilities {
  observesState   : boolean   // can report status without inference
  deliversRawKeys : boolean   // can deliver a raw key event to a screen (§3 `keys`) and populate `screenDigest` (§2.3)
  observesControlChannel: boolean // can report `controlChannel` (§2.3); absent state is answerable only against this
  observesPermissionMode: boolean // can report `permissionMode` (§2.3); absent state is answerable only against this. A driver may report it from a posture it configured itself (the local opencode driver reports `bypass`, muster #283) and leave it absent for a session it set nothing on; and a driver may honour `permissionMode: "bypass"` only together with an enforced `sandbox` (§2.1), refusing `unsupported` otherwise
  reportsUsage    : boolean   // can read `usage` (§2.3, muster #285) from its runtime; absent usage is answerable only against this: true = "not yet known", false = never
  reportsRuntimeSurface: boolean // can say anything about `runtimeSurface` (§2.13); absent state is answerable only against this
  isolatesEnvironment: boolean // starts a session's process with a BUILT environment, nothing inherited from the service (§2.1 `isolateEnvironment`, muster #280)
  sandbox?        : SandboxSupport  // can wrap a session in an OS sandbox (§2.1 `sandbox`, muster #281): { mechanism, denies[], network[], packageCache[] }; absent = cannot, and a create asking for one is refused `unsupported`
  confirmsDelivery: boolean   // can distinguish submitted from queued
  supportsResume  : boolean   // sessions survive a service restart
  deliversToInbox : boolean   // has an inbox delivery path wired for at least some targets
  supportsPin     : { model: boolean, effort: boolean, agent: boolean }
  validatesModel  : boolean   // checks `model` against the runtime's own list at create and refuses an unknown id `invalid` (§4.3, muster #287)
  remoteControl?  : { toggle: boolean, off: boolean }   // can turn a RUNNING session's remote control on (and off); absent = cannot (muster #269)
  deliveryModules? : DeliveryModuleStatus[]   // optional external delivery modules enabled here (§2.15); absent = none
  deadlineMs      : number    // declared upper bound on any single call
  source          : "observed" | "assumed"
  observedAt?     : Timestamp | null
}
```

**`observesState` had never been exercised by a local driver until muster
issue #55.** Every local driver before it inferred status from a screen; the
argument in §2.3 for keeping `observed` and `inferred` distinct was, until
then, carried entirely by the remote driver relaying a peer's own report. #55
added a second local driver — `internal/drivers/opencode` — over a runtime
whose status is a structured API response rather than terminal output, so it
declares `observesState: true` honestly rather than by relay. The distinction
this field exists to make is no longer only a federation concern.

**`deliversRawKeys` gates `keys` and `screenDigest`** — muster issue
#59's capability-gated resolution, shaped after `observesState` but not the
same kind of thing: `observesState` distinguishes two ways of answering a
question the model already asks; `deliversRawKeys` gates whether a driver
supports an operation §5.1 asks every operation not to need. The tmux driver
declares `true` — its whole substrate is the screen this describes. A driver
over a runtime with no screen to capture — nothing to fingerprint, nowhere
for an arrow key to land — declares `false` and answers `unsupported` (§5.6)
for `keys`, never emulating a screen that is not there.

Like every field in this block, `deliversRawKeys` is covered by `source`
below rather than exempt from it: a peer that has never answered reports
`deliversRawKeys: false` under `source: assumed`, which is a statement that
nobody has said anything, never a claim that the peer cannot do this. A
capability that silently read `false` on an unreachable peer, with nothing
to mark the value as unconfirmed, is precisely the defect `source` exists to
prevent (see below), and adding a flag is not a license to re-introduce it
by accident.

**`sandbox` answers "can a session here be confined by an operating-system profile"** (muster #281).
Present means the driver enforces **every** class in `denies` — `files`, `unixSockets`,
`systemServices` — with the mechanism it names, and honours the `network` postures and
`packageCache` modes it lists. Absent means it cannot, and a create carrying `sandbox` is refused
`unsupported`, naming what is missing, before anything is started (§5.6). It is absent, not
half-offered, where the platform cannot deny all three classes. For the opencode driver it is
present on macOS only, enforced with the system's `sandbox-exec` and a profile the driver generates;
on any other platform, in the test-only shared mode (one server cannot confine one of its sessions),
or when the service itself already runs inside a sandbox (a profile cannot be nested), it is absent
and the reason is in the refusal. The tmux driver never declares it: its sessions live in the user's
own multiplexer, whose server socket is exactly what a sandbox must keep a session from. A session
created with `sandbox` reports the profile in force in `state.sandbox` on every read and in every
listing — the paths and posture actually granted, defaults included — so a reader confirms the
profile rather than assumes it from the request. Like every flag here it inherits `source: assumed`
from an unreached peer.

**`validatesModel` answers "is a wrong `model` caught at create, or only at the first turn"**
(muster #287). `supportsPin.model` says the hint is passed to the runtime; the same model carries
different ids on different runtimes, so a wrong spelling is otherwise accepted and the session
reads as healthy until it is given work. `true` means the driver reads the runtime's own list of
usable models on every create, never a list compiled into this service (catalogs change with
runtime releases and with the credentials a session was given), and refuses an id that is not on it
as `invalid`, naming the id and, when something on the list is a near spelling, the closest ids. The
refusal comes before any session exists: nothing is left behind and the process started for it is
stopped. `false` means a wrong id is accepted and fails later, as before. Two limits: a runtime
that returns no usable list (an older release without the endpoint, an error, or an empty list)
leaves that one create unchecked, because "could not tell" is not "wrong"; and a listed id is
accepted by the check, not guaranteed to succeed at the first turn. Like every flag here it
inherits `source: assumed` from an unreached peer.

**`isolatesEnvironment` answers "can a session here be started with ONLY the environment its
create asked for"** (muster #280). `true` means the driver starts each session in a process of its
own with a *built* environment; `false` means sessions share a process or a shell that inherits the
service's. A create that carries `isolateEnvironment: true` against a `false` driver is refused
`unsupported`. For the opencode driver the built environment is exactly:

- `PATH`: system directories only (`/usr/local/bin`, `/usr/bin`, `/bin`, `/usr/sbin`, `/sbin`, plus
  `/opt/homebrew/bin` on darwin), after any directories the operator allow-listed (never one under the
  service user's home);
- `LANG`, copied from the service only when it is set;
- `HOME`, `TMPDIR`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME` and `XDG_CACHE_HOME`, all
  inside a directory owned by that one session and removed with it, so opencode reads no shared user
  configuration and keeps no database shared across sessions;
- `OPENCODE_DISABLE_CLAUDE_CODE=1`, because by default opencode loads the user's Claude Code
  instructions and skills;
- this machine's `sessionEnv` entries that apply, then the create's `env`;
- the session's own server credential, which reaches only that session's process.

Close and shutdown kill the session's whole process group, not just the server, so nothing the
session's tools started outlives it. A descendant that detaches into a new session of its own
escapes a group kill; confining the filesystem and process tree is the sandbox's job, not this
flag's.

**What isolation does not do, stated plainly.** (1) The model-provider key a session needs is *in
its environment*, and the runtime exposes that environment to its tool shell (`env` lists it).
Isolation scopes every *other* credential; it cannot protect the model key itself, so give each lane
its own spend-capped key. (2) It does not confine the filesystem: a session can still read what the
service user can. (3) A repository's own instruction files reach the runtime: it loads the working
directory's `AGENTS.md` and `CLAUDE.md` (up to 64 KB each; `@path` imports are not followed by one of
the two harnesses measured). (4) The interpreters a session's tools use must be on a system or
allow-listed directory — an interpreter under the user's home stops being readable once a sandbox
confines the home.

**`deliversToInbox` answers "is the inbox path even wired here", never "will
this call use it"** (muster #119, #122). Send's own per-target capability
detection (`InboxResolver`, unchanged by this field) still decides that,
target by target, on every call. This field exists because #119 shipped and
deployed with no resolver ever configured, and the only way anyone could tell
was reading a delivery receipt's wording and recognising which surface it
named — every test passed, every constraint held, and the new path never
engaged. `false` means every delivery on this driver falls through to the
pane path unconditionally, the same as before #119 existed. `true` means a
resolver is configured, and at least some targets can take the inbox path;
it says nothing about which ones. Same argument as `build` (#121) — "a
caller should be able to ask" — applied to a different question, and
answered the same way: extend an existing read instead of adding a route.

**A declaration carries its own provenance.** `observed` means the driver these
describe reported them — a local driver is always this, since it is describing
itself. `assumed` means nobody has reported them and the values are a
conservative floor.

The distinction is not decoration. Every flag false means "this driver supports
nothing"; a peer that has never answered produces exactly that value, meaning
"nobody has told me anything". Without `source` those are one value, and a
permanently misconfigured peer is indistinguishable from a deliberately minimal
one — §5.7 in its fourth location.

`observedAt` is when the declaration was obtained. Freshness is left for the
caller to judge rather than collapsed into a boolean, for the same reason §11
reports clocks instead of deciding about skew: the component that has the
information is rarely the one that knows what counts as too old.

A caller acting on a capability **must** consult `source`. Reading `assumed`
values as an answer is how a temporarily unreachable peer gets treated as a
permanently incapable one.

**A cached `observed` still has a shelf life, enforced by the driver itself
rather than left entirely to the caller (muster #67).** The remote
driver's cache was found surviving a peer restart for as long as the
observing process happened to run — under a keep-alive supervisor, that is
weeks — still reporting `observed`, still wrong. Past a bounded window with
no fresh evidence, the remote driver degrades its own cache to `assumed`
rather than let `observedAt`'s age be the only thing standing between a
caller and a stale claim nobody re-checked. This does not contradict the
paragraph above: `observedAt` still travels on the wire for a caller with its
own, possibly stricter, notion of "too old"; the driver's bound is a
backstop for callers who only look at `source`, which is most of them,
including this repo's own capability-gated resolution.

**That degrade is paired with a re-probe triggered by ordinary traffic, not
a schedule.** Every session operation the remote driver relays already
reaches the peer and gets a domain answer back — a create, a keypress, even
a guarded refusal. That answer is proof the peer is up and speaking the
protocol right now, and is used to opportunistically refresh the cached
capabilities when they are unseen or past the bound above, rather than
waiting for something to probe on a timer nobody is driving. A peer that
never receives an operation still degrades to `assumed` on the schedule
above; a peer under real traffic reconverges for free, off the round trips
that were happening anyway.

A driver must never silently emulate a capability it lacks.

> **Partly resolved (D3).** `source` now separates "nobody has told me" from
> "supports nothing". What remains is that the declaration is still synchronous
> and infallible, so a remote driver cannot *fetch* it in the course of
> answering — it can only report what it happens to have cached. See §14 D3.

### 4.4 Every driver declares a deadline

**`deadlineMs` is mandatory. A driver that can block without a bound is a
specification violation, not a slow driver.**

This was found empirically rather than reasoned about, and it is the sharpest
lesson the first two drivers taught: the interface is symmetric, but **the
hazard profile underneath it is not.**

A local driver talks to a subprocess. If that hangs it is rare, local, and
diagnosable. A remote driver talks to a machine that may be powered off,
firewalled, or — worst of all — *stopped mid-syscall*, which is neither alive
nor dead. Measured directly: a caller with no deadline, querying a peer that had
been SIGSTOPped, was **still blocked with no result after seven seconds** and
would have waited indefinitely.

Note carefully what did *not* save us there: no mainstream language's HTTP
client defaults to a finite timeout. The protection cannot come from the
runtime, and therefore has to come from the contract.

Earlier drafts described `unreachable` as an **outcome** while saying nothing
about how a caller ever *reaches* that outcome. An outcome nobody is obliged to
produce is not a guarantee — it is a hope. Hence:

- Every driver declares `deadlineMs` and honours it.
- Exceeding it produces `unreachable` with the elapsed time as evidence.
- A caller may supply a shorter deadline; never a longer one.

---

## 5. Design rules

These are stated as rules because each one was learned by violating it.

### 5.1 Express questions, not mechanisms

The interface says `state()`, never `readScreen()`; `send()`, never
`typeKeys()`. Mechanism-shaped interfaces bind every future driver to the first
driver's substrate.

### 5.2 Uncertainty travels

If a driver cannot determine something, that must survive all the way to the
caller. An interface that forces a boolean where the truth is a guess produces
confident wrong answers, which are worse than admitted ignorance.

### 5.3 Context by reference, never by argv

Session context is passed as a path (`contextRef`), never inlined into a
command line.

Rationale, generally applicable: process command lines are a shared namespace.
Anything that matches processes by name — a cleanup command, a monitoring
script, an agent tidying up after itself — can match a session whose argv
merely *contains* the string it was hunting for, and terminate it. Passing
context by file removes the session from that namespace entirely.

### 5.4 Ids are recyclable; require consensus before destruction

Before any destructive operation, a driver must confirm that the session at an
id is the session the caller meant — by corroborating at least one independent
attribute (working directory, start time, name). Matching an id alone is not
identification.

**The operand this rule needs is `Request.Expect.StartedAt` (§2.6).** A caller
quotes the start time it observed; the driver compares the live session against
that, and refuses on mismatch. This closes the window between the *caller's*
observation and the destroy — the long one, the one a human is standing inside
of, and the one that contains a round trip when the session is on another
machine.

A caller may omit it, and then a driver falls back to comparing against its own
last sighting. That is strictly weaker: it proves only that nothing changed
since the *driver* looked. A driver applying the weak check must say so when it
refuses.

A proxy forwards the expectation and corroborates nothing itself. Checking on
the relaying machine would compare against something a third party believes,
which puts one more layer between the caller's observation and the destroy —
the opposite of what this section asks for. This was open defect D2; see
Appendix A, F16.

### 5.5 State and events, never polling

Federated callers may be many network round-trips away. An API that requires
polling to stay current becomes unusable at exactly the distance federation is
for. State is readable on demand; changes are pushed.

**A subscription filter can name sessions, not only describe them.** On a
substrate that charges one connection per watched session, granularity is a
cost parameter rather than a convenience: a caller that can only say
"everything under this directory" makes a driver attach to every match, while a
caller that can name what it means attaches to one.

Selectors narrow and compose with AND, matching the rule plural reads already
follow, so a caller does not carry two conventions.

Naming an id inherits §5.4's recyclability — a subscription to an id that dies
and is recreated will carry events for the new session. That is safe only
because the discontinuity is **announced**: the subscriber sees `session.closed`
then `session.created` and can tell. A stream that silently swapped subjects
would be §7.3's silent gap in another costume. This was open defect D4; see
Appendix A, F22.

### 5.6 Degrade, never emulate

A driver that cannot observe state reports `inferred` or `unknown`. It does not
manufacture a plausible `observed`. Emulation makes capability differences
invisible at precisely the layer built to expose them.

### 5.7 Absence and failure are different answers

**A failed read must never render as an empty result.**

"There are no sessions on that machine" and "I could not reach that machine to
ask" are different facts with opposite implications, and they are trivially
easy to collapse into the same empty list — at which point every caller
downstream draws a confident conclusion from a failure.

This is the general form of the `unknown` status in §2.3, and it governs every
plural response in this API. It is why §9 forbids returning a bare array from
any operation that spans machines: there is nowhere in a bare array to say
*"and one source didn't answer."*

**This rule applies inside a driver, not only across machines.** The wording
above is about sources and envelopes, but the same collapse is available
between a driver and a single session, and it manufactures the same confident
wrong answer. A driver must distinguish **"I read this and could not tell"**
from **"I failed to read this."** Both may surface as `unknown`, but they must
not carry the same `evidence` — that field is the only place the difference can
survive (§2.3).

The general form, worth stating once: *a component that cannot report its own
failure to observe will report its ignorance as the world's.*

> How this was found — a driver that could read nothing returned a complete,
> error-free view of 22 sessions and passed its entire test suite:
> Appendix A, F5.

> Found a second time on a structurally different substrate — muster
> issue #55: the second local driver's status endpoint omits an idle session
> from its response map entirely, and a network failure or a rejected
> credential renders as the exact same empty-looking absence at the HTTP
> layer. `internal/drivers/opencode`'s reads therefore only ever call the
> classifier that turns "present in the map" into busy/retry when the read is
> known to have succeeded; a failed read returns a Go error and is never
> handed to it. See `internal/drivers/opencode/state.go` and `ops.go`'s
> `State`/`List`.

### 5.8 Report facts about content, never content the session produced

> **Narrowed once, by one ruling (muster #258, 2026-10-04).** The owner ruled
> that a caller holding the credential `input`/`respond` already require may
> read **what the session's own agent wrote — its assistant turns — and
> nothing else**, through `GET …/{id}/turns` (api-http.md §3.3). Everything
> below still holds for every other class of session content: tool calls and
> results, file contents, the messages sent in by a human or another session,
> system and hook output, reasoning, and the screen. The exception is bounded
> by provenance and by an allow-list at the point the record is read, not by
> sensitivity, and it adds no result field, slot or status: a turn is what the
> agent said, and whether the session finished is still what the runtime
> reports about itself. The premises below were not outweighed; the thing they
> were applied to was narrowed. Full argument and what it costs:
> `docs/adr/258-assistant-turns-read.md`.

**The boundary this API draws around content is provenance, not
sensitivity.** A field may carry the runtime's own words about itself —
`ControlChannel.reason`, `TurnEnd.reason`, `QuotaBlock.resetHint` all do this
today, and none of them is a violation of this rule. What this API never
does, and what muster #82 asked it to reconsider, is report content the
**session** produced — the text an agent read, decided, or wrote for a
reader, as opposed to the runtime's own structured account of its own
condition.

This is not a new decision; it is three independent ones, restated as one
rule so a fourth proposal is answered by derivation rather than
re-litigated from nothing:

- **`screenDigest` (§2.3) is a fingerprint, never the text it was taken
  from.** A read that returned pane content would make every listing a
  transcript leak (`docs/spec/api-http.md` §4, the `keys` route).
- **`environment.names` carries variable names and never values**
  (`docs/spec/api-http.md`, the `environment` route) — the same instinct
  applied to a different kind of content.
- **§2.9's `ConversationRef` names a transcript record without ever opening
  it.** The record it names is one the *runtime* wrote unasked, which is
  exactly why §2.9 calls it "an independent witness... the first source in
  this model that is not an echo." That provenance is what makes naming it
  safe, and it is also what a session's own *result* does not have: a result
  is written by the agent, on purpose, for whoever reads it back — the
  forgeable class §2.3's `ControlChannel.reason` note (muster #69)
  already warned about, twice measured: a supervisor grepping panes for a
  disconnection notice classified *itself* as disconnected because its own
  tool output contained the string it was searching for, and a prompt
  classifier was once fooled into reading "No auth bypass" — typed by the
  agent into its own prompt — as the runtime's own account of the decision.

A "result" a caller wants back from a dispatched session is definitionally
session-authored. It does not become the runtime's own account of itself by
being written to a field or a slot instead of a screen, so this API does not
carry one — and the assistant-turns route above is not one: it returns turns,
not a result, and a reader decides what a turn means. See `docs/adr/82-session-result-belongs-above-this-layer.md` for
the alternatives this ruled out and why.

**`SessionState.turns` (§2.3, muster #111) was measured against this
rule before being added, not after.** It is a count of runtime-written
turn-boundary markers in the same unprompted record §2.9's `ConversationRef`
already names — provenance identical to a field this rule already permits —
and it carries strictly less information than `screenDigest`, which is
already in this block. It answers *whether* the session ran, never *what* it
produced, so it does not reopen muster #107's ruling (kept: this section
stays a derived §5 rule, and a reopening must name which of this section's
three underlying premises it is challenging) — this addition challenges none
of them. Full argument:
`docs/adr/111-turns-is-a-liveness-fact-not-a-result-channel.md`.

---

## 6. Authorization

Where a session runs is no longer a physical constraint, so it must become an
explicit one.

A single-machine fleet has an accidental safety property: a supervisor can only
destroy sessions on its own host, because it has no way to reach any other.
Federation removes that property. It has to be rebuilt deliberately rather than
mourned.

**Requirements:**

1. **Bind narrowly.** Default to loopback. Exposure beyond it is explicit
   configuration, never a side effect of enabling federation.
2. **Authenticate peers.** No unauthenticated network surface, ever. A service
   that can start processes and read files is a remote-execution surface
   regardless of intent.
3. **Separate read from destroy.** Peer authorization is per-verb. `list` and
   `state` from a peer may be permitted by default; `close`, `interrupt` and
   `create` are opt-in per peer.
4. **Log every remote-originated mutation** — actor, verb, target, outcome.
   This is the audit trail that replaces "it could only ever have been me."

**Caller authority is a parameter of every operation (§2.6, §3).** Requirement
3 above and §13's "proxying does not launder authorization" are enforceable
because the authority cannot be omitted: a driver does not compile without it,
and a proxy that holds no credential of its own has nothing to substitute.
This was open defect D1; how it failed before, and what proving the fix
required, is Appendix A, F14.

**Authorization is per principal, per verb.** Each caller — peer or client —
presents its own credential and holds a set of grants: `read`, and one per
mutating verb, plus `relay` for having a mutation forwarded to a peer on its
behalf. Grants default to none, because requirement 3's default is denied.

That granularity is what requirement 3 asked for and a shared secret could not
express: "may watch my sessions" and "may kill my sessions" are exactly the
distinction an operator wants when opening a machine to a peer at all, and one
mutate bit forces them together. `relay` keeps §14 D6's host/client split, now
per caller rather than per service.

**Requirement 4 becomes implementable at the same moment.** An audit trail
wants an actor, and the best a shared token can name is an address — which
answers *where from* and never *who*. With principals the actor is a name, and
a relayed request names both the original asker and the machine that relayed
it, because a line reading "the peer did it" cannot answer who asked the peer.

**How a proxied request presents authority, revised.** §13 requires the
original caller's authority to reach the peer. Under one shared secret that was
literal — forward the caller's credential, and the peer accepts it because it
is the peer's credential too. Per-peer credentials remove that coincidence: a
caller's token means nothing on another machine.

So a proxied request carries authority in two parts. The relaying machine
authenticates as **itself**, with the credential it holds on that peer, and
that is what the peer authorizes. The original principal travels **as an
assertion**, which the peer records. The peer trusts that assertion exactly as
far as it trusts the relay, which is the honest bound: a relay can never obtain
more than it was granted, whatever principal it names, and what the assertion
buys is the audit trail requirement 4 asks for. See Appendix A, F27.

---

## 7. Resolved decisions

### 7.1 Addressing is `(machine, id)`

No fleet-wide identifier. A session is addressed by the machine that runs it
plus an id scoped to that machine. `name` is a human label — unique per machine
by convention, never an identifier, never used for routing.

*Rationale:* a fleet-wide id would need an allocator, and an allocator is a
single point of failure for the one operation that must keep working when a
peer is unreachable. `(machine, id)` needs no coordination at all.

### 7.1a A configured default runtime resolves a bare id, existence first

`(machine, id)` is complete only while a machine runs one runtime. The moment
it runs two, a bare id is ambiguous between them and needs a third
coordinate — `runtime` (api-http.md §3.3) — that no caller anywhere sends,
because until a second local driver exists nothing has ever required it
(muster issue #60).

**⚖ Ruling:** a machine may configure a **default runtime**, an
operator-edited, machine-local setting (`cmd/muster/config.go`'s
`defaultRuntime`, beside `peers` and `trustRoots`) that keeps bare-id
addressing working for every caller that has never had to name one. Absent
means the older behaviour: bare-id addressing among more than one local
runtime is refused rather than guessed.

A default is a quiet way to reach the wrong runtime, not merely a way to
fail, so three guardrails govern it:

1. **A default naming an unregistered runtime fails at startup**, never on
   the first request that needs it. Otherwise a typo becomes a fleet-wide
   `not_found` indistinguishable from every session having vanished.
2. **Resolving via the default is visible in the response**, not only in a
   log. A caller that named its own runtime and a caller that received the
   default are in different epistemic positions, and §5.7 already forbids
   rendering those alike.
3. **Existence first, default as tiebreak.** A bare id is resolved by asking
   every registered local driver whether it has ever had that id — before
   the default is ever consulted:
   - exactly one driver affirms it → that driver, regardless of what the
     default names. A default must never steer an id that plainly belongs
     to another runtime into a false `not_found` — the sharpest form of the
     absence/failure confusion §5.7 forbids, applied to routing instead of a
     single read.
   - more than one driver affirms it → refused, naming every runtime that
     claims the id. §5.4 already requires consensus before acting on a
     recycled id; two runtimes claiming the same one is exactly that case,
     and it is surfaced rather than silently resolved by the default.
   - the check cannot be completed for every driver (one errors for a
     reason other than "never had this id") and nothing else affirms the id
     → refused rather than guessed, for the same reason as the case above:
     an unreachable driver might be the one that actually holds it.
   - every driver affirmatively confirms absence → a genuine miss, the same
     shape `create`'s own ambiguous case has (there is equally nothing to
     route TO) — only here does the configured default apply.

   `create` has no existing id to check and reaches the genuine-miss
   handling directly: an ambiguous `create` with no `runtime` hint resolves
   to the configured default, or is refused absent one.

**Federation is unaffected.** A proxied request to a peer machine never
reaches this resolution at all — the peer resolves its own runtimes locally
and this service does not recurse into asking it to disambiguate (§13.1).
The default is machine-local in the sharpest sense: applying THIS machine's
default to an id addressed at a peer would make one bare id mean different
sessions depending on which machine answered it, exactly the fork this
section's `(machine, id)` addressing exists to prevent.

### 7.2 Peers are statically configured

No announcement, no discovery protocol, no broadcast.

*Rationale:* automatic discovery means a machine can join the fleet without
anyone deciding it should — an anti-feature for something that starts
processes. For a fleet of a handful of machines, the configuration cost is
negligible and the audit trail is worth more than the convenience.

**Peer addresses are operator-verified, never inherited from a machine's own
idea of its name.** A hostname can resolve to different addresses depending on
who is asking — split-horizon DNS, overlay networks, and multi-homed hosts all
produce this, and it was observed on the first two machines this ran on. A peer
entry that stores a bare hostname and trusts the peer's self-resolution will
misconfigure silently, and present as an unreachable peer that pings fine.
Configuration stores an address the *operator* has confirmed reachable **from
the machine that will be doing the reaching**.

### 7.3 Events carry a cursor and an epoch

Each service instance assigns events a monotonic cursor and stamps them with an
**epoch** identifying the instance. A subscriber reconnects with its last
cursor. If the cursor is older than the retained buffer, or the epoch has
changed (the service restarted), the service returns `resync_required` and the
subscriber refetches state.

*Rationale:* the alternative — silently resuming from the oldest available
event — produces a subscriber that believes it has a complete history and does
not. Announced gaps are recoverable; silent gaps are not.

Two rules follow, both normative:

**Cursor and epoch are assigned by the service, never by a driver.** A driver
has access to neither, and two drivers under one service must not mint
competing sequences. A driver leaves them unset; the service stamps them on the
way out.

**The baseline snapshot is taken before `subscribe` returns.** If a
subscription takes its first reading asynchronously, everything occurring
between the call returning and that reading is folded into the baseline and
never reported — a gap that cannot even be announced, because no cursor covers
it and no epoch changed. Taking it synchronously makes the guarantee stateable:
every change after `subscribe` returns is either delivered or is a bug.

> Both were found the hard way: Appendix A, F8.

### 7.4 Plural responses are envelopes, never bare arrays

Any operation that spans machines returns per-source status alongside the data,
plus an explicit completeness flag. See §9.

### 7.5 Restart adopts; it never silently drops

On restart the service re-discovers sessions its drivers can still see and
adopts them. Anything it finds but cannot confidently identify is surfaced as
`unknown`, never dropped from listings and never destroyed. See §12.

### 7.6 A session's result is delivered by the session, not by this service

> **Amended by muster #258.** The decision below stands as written for a result
> *slot* and for any result-carrying field. What changed is the separate question
> of whether a caller may *read back* what a session's agent said: it may, for
> assistant turns only (§5.8's note; `docs/adr/258-assistant-turns-read.md`).
> The reply-address convention remains the way to push an answer; the turns
> route is the way to pull one.

Dispatching a named agent to a peer through this API is a complete round
trip for everything except the answer: create, drive, answer a dialog,
follow up, and destroy all work end to end, but nothing here returns what
the dispatched agent produced (muster #82). Three out-of-band
workarounds grew to close that gap — a directory both machines
synchronise, a local HTTP server on the worker's machine, a shell
connection capturing stdout — each substituting a dependency this API does
not have, and none of them a session this service can see, answer, or tear
down.

**Decided: no result endpoint, no result-carrying field, no service-held
result slot.** The reply address belongs in the dispatch brief, and the
worker delivers its answer by calling `input` on the requesting session —
the endorsed shape of what was already happening, written down rather than
left to be rediscovered by the next caller. §5.8 states why a result field
specifically is out of bounds; `docs/adr/82-session-result-belongs-above-this-layer.md`
records the alternatives this considered and rejected, including why a
`ConversationRef`-shaped reference does not transfer to this case. A large
answer that does not fit in a prompt remains a transport choice made above
this layer, the same shape `docs/adoption.md` §2 already uses for the
cross-machine write race: neither is a defect in this service, and neither
is fixed here.

**This is not silent on authority.** A reply delivered this way lands in
the requesting session's composer exactly as if its own operator had typed
it — see `docs/client-guide.md`, "Getting an answer back from a dispatched
session," for what that costs and the grants it requires on each machine.

### 7.7 A caller can read its own grants; a peer's are never observed

§6 makes every precondition for a mutating call a per-verb grant, and a
relayed one needs two: one on the machine receiving the call, a different one
on the machine performing it (§7.6's reply convention, and muster #68's
federated-keypress precedent, are the same shape twice). Nothing before this
let a caller read either ahead of time — every precondition was discovered by
attempting the call and reading whichever refusal came back first, and fixing
the first refusal only ever revealed the second (muster #106).

**Decided: `GET /v1/whoami` (api-http.md §3.1) reports the presented
credential's own grants, and nothing about any other principal.** It sits
behind authentication (§6 requirement 2 — no unauthenticated mode, no
exception) but deliberately not behind the `read` grant every other read
route requires. A principal holding no grants at all is exactly the caller
most in need of this route, and gating it on `read` would make it unusable by
that caller. Reporting a credential's own authority back to itself is not the
risk the `read` gate exists to hold off — that gate protects OTHER
principals' data (every session, every peer, the full event stream) from a
credential nobody granted it to; this route names only the credential that
authenticated the request, and never enumerates the principal table.

**A peer machine's answer is always the conservative floor, never an
observation.** This service probes and caches what a peer's DRIVER can do
(§4.3, `RefreshCapabilities`) because that is a runtime fact worth asking
about; it has no equivalent for what a peer has GRANTED a given credential,
because that is per-machine configuration nobody here has a channel to ask
for, and building one to answer a read route would be new federation surface
for a fact that rarely changes. So `GET /v1/whoami?machine=<peer>` reuses
`CapabilitySource`'s existing "assumed" meaning — nothing confirmed, a floor,
never that machine's real table — rather than inventing a second vocabulary
for the identical "nobody has told me anything" fact §4.3 already named. The
second grant a relay needs still has to be learned by asking that machine
directly: run its own `whoami` there, or read its principal table locally
(`muster principal list` on that machine) — neither of which this
service can do on a caller's behalf.

**Narrowed by muster #154: this service's OWN standing on a peer is
observed.** The paragraph above stays true of a *caller's* credential, which the
peer does not know. But a service holds exactly one credential every peer does
know — its own, the one each relayed read and write rides on — and asking about
it is `whoami` asked by the service about itself, not about anyone else. So the
peer probe (§4.3, `RefreshCapabilities`) now also calls the peer's
`whoami?peer=<self>` and caches what comes back: whether that peer's roster lists
this service, and what it grants the presented credential. `GET /v1/machines`
reports it per machine as `peer` (api-http.md §3.1), with the same
observed/assumed provenance, and a peer that cannot answer — unreached, stale, or
on an older build — is `assumed`, never a negative. This is a report of drift,
not a fix for it: the roster stays hand-configured (§7.2).

## 7a. Still open

- **Input ordering under concurrency.** If two callers `send()` to one session
  simultaneously, is ordering defined, or is that the caller's problem?
- ~~**Backpressure.**~~ Answered by implementing it, and the rest of the design
  left only one option: a subscriber that cannot keep up is **marked and
  resynced**, never silently skipped. Dropping quietly would hand a subscriber
  a hole it has no way to detect, which is §7.3's silent gap; blocking would
  let one slow reader stall the machine's whole event plane. Retention is a
  bounded window, and falling off it is announced like any other gap.
- **Whether `create` should be able to target "any machine"** under a policy,
  rather than requiring the caller to name one. Deferred: it needs a scheduler,
  and a scheduler is a supervisor concern (§1 non-goals).

---

## 8. Lifecycle

Legal transitions. Anything not listed is a driver bug.

```
        ┌──────────┐
        │ starting │
        └────┬─────┘
             ▼
  ┌──────► working ◄────────┐
  │          │  ▲           │
  │          ▼  │           │
  │    waiting_input        │
  │          │              │
  │          ▼              │
  └──────── idle ───────────┘
             │
             ▼
           dead
```

- `starting` → `working` | `idle` | `dead`
- `working` → `waiting_input` | `idle` | `quota_blocked` | `dead`
- `waiting_input` → `working` | `idle` | `dead`
- `idle` → `working` | `dead`
- `quota_blocked` → `working` | `idle` | `dead`
- `dead` → terminal. A `dead` session never becomes live again; a resumed
  session is a **new** session with a new id.

`unknown` is **outside** this machine. It may be entered from any state and
exited to any state, because it does not describe the session — it describes
the driver's knowledge of it. A caller must not infer that a transition
occurred merely because the state changed to or from `unknown`.

**`since` is the time the status was first observed to hold**, not the time it
began. For `inferred` states those differ, sometimes by a lot. A caller
computing "how long has this been stuck" is computing a lower bound, and should
present it as one.

**It must not restart on every read.** A driver that stamps the current time
each time it looks makes `since` useless — it would always read "just now", and
the field's only purpose is duration. A driver therefore carries the timestamp
forward while the status is unchanged and resets it when the status changes.

**A read that observed nothing is not a status change.** `unknown` is outside
the state machine (above), so a driver whose read failed — a capture that came
back empty — has learned nothing about the session and must not let that read
end the status it last observed. When the next good read finds the same status
as before the failed one, `since` is carried across the gap. While the gap
lasts, the `unknown` it reports has its own `since`, the first failed read, and
that does not move on each further failed read. Otherwise one failed capture
would make every idle session read as active a moment ago (#278).

**Duration is the passive discriminator for a class of stall that otherwise
requires touching the session.** A pane holding unsent input looks identical
whether a human is mid-sentence or the pane has stopped accepting input
entirely — and the correct policy for the first ("do not evict, an operator has
text pending") becomes permanent for the second, because no human can come
back to a pane that ignores typing. A sibling project measured exactly that:
fourteen hours of the same unsent line behind a veto that never expired.

Its discriminator was to type a character and see whether it appeared, which is
not something to do to a live session. `since` gives the same answer without
touching anything: text unchanged for hours is not a sentence somebody is still
composing. See Appendix A, F34.

---

## 9. Plural responses

Every operation that spans more than one machine returns:

```
Collection<T> {
  items    : T[]
  sources  : SourceStatus[]
  complete : boolean       // false if any source failed to answer
}

SourceStatus {
  machine   : MachineId
  status    : "ok" | "unreachable" | "unauthorized" | "degraded"
  count?    : number
  error?    : string
  observedAt: Timestamp
  quota?    : QuotaBlock   // this machine's ACCOUNT-level refusal, if any (§2.3, #10)
}
```

`complete` is redundant with `sources` and exists anyway, deliberately: the
common bug is a caller that never looks at `sources`. A single boolean at the
top level is hard to not notice, and a caller that ignores it has made a choice
rather than an oversight.

**Never** return `items: []` for a source that failed. An unreachable machine
contributes a `SourceStatus`, not an absence.

**`quota` is a per-machine sibling of `SessionState.quota`, not the same
field relocated.** A machine can be reachable and answering — `status: "ok"` —
while every session on its account is refused: reachable-and-answering and
willing-to-work are different facts, and folding this into `status` would
force a caller to enumerate every session on a machine just to learn whether
dispatching to it is pointless. This is the field a scheduler reads to answer
"where can work run right now" without listing a single session. (Undocumented
in this block until muster issue #57's audit; present on `collection.go`'s
`SourceStatus` since #10, and populated by the tmux driver's own remembered
`quotaBlock`.)

**`complete` is derived, never supplied.** It is true iff every
`SourceStatus.status` is `ok`. A caller-supplied boolean is exactly the kind of
value that drifts from what `sources` actually says — the same class of bug
this field exists to catch, one level up. `degraded` flips `complete` to false
on the same footing as `unreachable` and `unauthorized`: a degraded source's
data is present but not to be trusted at face value (§13.2), and treating it as
"answered cleanly" would reintroduce the confidence-flattening §5.6 forbids.

> Origin: Appendix A, F4.

---

## 10. Idempotency

`create` accepts a caller-supplied **idempotency key**. A driver that receives
a repeat key within the retention window returns the *existing* `SessionRef`
instead of creating a second session.

This is not a nicety. The failure it prevents is specific and expensive:

> A federated `create` times out in transit. The caller cannot distinguish "the
> session was never created" from "the session was created and the reply was
> lost", so it retries. Two agent sessions are now running in the same working
> directory, both writing to the same files, neither aware of the other.

Retention must outlive the caller's retry window. Keys are scoped per machine.

**Retention that does not outlive the *service* does not satisfy this rule**,
because a service restart falls inside the caller's retry window — and is one
very good reason a reply went missing in the first place. Either persist keys,
or declare that they are not persisted.

§4.3's `supportsResume` does **not** answer this question. It asks whether
*sessions* survive a restart, which is a different fact: a driver whose
sessions are owned by an external process can honestly declare
`supportsResume: true` while its idempotency keys live in memory and do not
survive at all.

> Known non-compliance in the current implementation, and how the two were
> conflated: §14 D5, Appendix A, F7.

---

## 11. Time

Every machine stamps timestamps in **its own clock**, and every response
carries that machine's current clock reading.

Callers compute skew rather than assuming synchronisation. No attempt is made
to establish a fleet-wide ordering of events across machines — there is no
requirement for one, and providing a fake one would be worse than providing
none.

Durations reported by a machine (`since`, silence timers) are computed
locally and are therefore internally consistent even when clocks disagree.
Comparing durations across machines is safe; comparing timestamps is not.

---

## 12. Adoption and restart

Sessions may outlive the service that manages them — a session inside a
terminal multiplexer survives the supervisor being restarted, upgraded, or
killed. The service must therefore treat startup as **reconciliation**, not
initialisation.

At startup, per driver:

1. Enumerate what actually exists on the machine.
2. Match against persisted records.
3. Classify each into one of:
   - **adopted** — matched with confidence; resumes normal management
   - **orphaned** — exists but no record; surfaced with `confidence: inferred`
     and whatever identifying evidence the driver has
   - **vanished** — record exists but nothing found; marked `dead` with
     evidence noting it disappeared while unobserved
4. Never destroy anything during reconciliation. A session the service cannot
   explain is a session for a human to look at, not one to clean up.

Rule 4 is absolute. Automated destruction during a phase whose entire premise
is *incomplete knowledge* is how a fleet eats its own work.

**A recycled id is two facts, not one.** An id that is remembered but now holds
a session started at a different time means the recorded session vanished AND a
new one is present under its name. Reporting only "adopted" attributes one
session's history to another — §5.4's recyclability, arriving in a third
operation.

**Records are written when the set changes, not on every read.** A read happens
on every event trigger; persisting each would turn a cheap enumeration into a
write amplifier for no benefit, since what reconciliation needs — which sessions
exist and when they were first seen — only changes when one appears or leaves.

See Appendix A, F30 for the ordering bug that made an earlier version report
everything as adopted, always.

---

## 13. Federation topology

A client talks to **one** service — normally the one on its own machine — and
that service **proxies** to peers on the client's behalf.

The rejected alternative is redirection, where the service tells the client
which peer to ask and the client asks directly. Redirection is leaner, but it
pushes topology, authentication and reachability into every client, which
defeats the purpose: a supervisor should be able to ask about the fleet without
knowing its shape.

**Costs, accepted explicitly:**

- The local service becomes a dependency for remote operations. It is already a
  dependency for local ones.
- Latency is additive. Acceptable for control-plane calls; this is another
  reason §5.5 forbids polling.
- Events from peers are multiplexed through the local service's stream, and
  carry their originating machine.

**The local half of this exists; the federated half does not.** A service now
streams its own drivers' events with §7.3's cursor, epoch, retention and
resync. A peer's events do not yet arrive, because a remote driver cannot
subscribe — see §14 D8. Until it can, the event plane delivers push exactly
where §5.5 says it matters least.

**Proxying does not launder authorization.** A service forwarding a peer's
request presents the *original* caller's authority, never its own. Otherwise
every machine becomes a confused deputy for every other.

### 13.1 A proxied request asks for the peer's LOCAL view only

**Fan-out is one hop deep, always. Peers do not recurse.**

The service a client asks is responsible for querying every peer it knows. Each
peer answers for **itself alone** and never forwards further.

Without this rule, two mutually-configured peers each answer by asking the
other, and the result is an infinite loop or — more insidiously — a fleet that
merely double-counts and looks fine. The first spike avoided this only by being
a star, and by the implementer noticing they were avoiding it. A topology whose
correctness depends on nobody adding a second edge is not a design.

**Consequence, stated because it is a real limit rather than an oversight:**
with a partially-connected peer graph, different entry points yield different
views of the fleet. A fleet that wants every node to see everything must be
fully meshed in configuration. This is acceptable at the scale this system
targets, and it is preferable to recursion — recursion buys transitive
visibility at the cost of cycle detection, hop limits, and a distributed
join that no operator can reason about at three in the morning.

### 13.2 Adopt a peer's SourceStatus; never re-synthesize it

A peer answering locally returns an envelope containing exactly one
`SourceStatus` — its own. The proxying service **adopts that record** into its
own envelope.

It must not discard the peer's status and manufacture a fresh `"ok"` from the
mere fact that the call succeeded. A peer can answer promptly *and* report
itself `degraded`; flattening that into "ok, count N" produces a confident
envelope built on a self-declared unreliable source — §5.7's failure, one layer
in.

The reachability of a peer and the health of a peer are different facts. The
proxy observes the first and must **relay**, not overwrite, the second.

---

## 14. Open defects in this specification

Places where this document requires something it cannot enforce, or describes
something the interface has no room for. Every one was found by an
implementation, and none is fixed. They are listed here rather than left as
marginal notes because a reader who takes the sections above at face value will
believe guarantees that do not hold.

Each entry states the rule, why it cannot be satisfied, what that costs, and
the proposed fix. **A proposed fix is not a decision.** Each changes a shape
that two implementations and an HTTP surface already depend on, which is
exactly why they are written down instead of applied in passing.

### D1 — Caller authority has nowhere to travel · §6, §13 — **RESOLVED**

Kept in place, rather than deleted, so the numbering the code cites stays
stable and so the entry that once said "this cannot be enforced" now says how
it was.

`Caller` (§2.6) is a parameter of every operation in §3. A driver cannot
compile without deciding what to do with it, and the first remote driver holds
no credential of its own — so the confused-deputy fallback is not merely
forbidden, it is unrepresentable. Reads are included, because "which sessions
exist, in which directories, on which machine" is exactly the reconnaissance an
unauthorized caller wants.

Full account, including what the fix cost and what proving it required:
Appendix A, F14.

### D2 — `close` cannot corroborate · §5.4 — **RESOLVED**

Kept in place so the numbering the code cites stays stable.

`Request.Expect.StartedAt` (§2.6) is the missing operand: the caller quotes
the start time it observed, and a driver refuses when the live session
disagrees. Omitting it is permitted and yields an explicitly weaker check
against the driver's own last sighting, named as such in any refusal.

D1 and D2 were the same defect wearing different clothes — an operation
needing caller-side context with nowhere to carry it — and the envelope in
§2.6 is the fix for the class rather than for either instance. See
Appendix A, F16.

### D3 — Capability declaration is synchronous and infallible · §4.3 — **NARROWED**

The half that is fixed: `DriverCapabilities.source` (§4.3) distinguishes
`observed` from `assumed`, so an unreached peer is no longer indistinguishable
from a peer that genuinely supports nothing. That was the part with real cost,
because it made a misconfiguration look permanent and unremarkable. See
Appendix A, F21.

The half that remains: `capabilities()` still cannot fail and cannot take a
context, so a remote driver can only ever report a cache. Something out of band
must populate it, and until something does, every answer is `assumed` — which
is now honest, but still not the peer's answer.

The consequence is concrete and visible in D7: a proxy derives its deadline
from the peer's declared one, so until the peer's capabilities are known the
proxy uses a floor it has no reason to believe.

- **Proposed fix:** capability discovery becomes an operation like any other
  cross-machine question — fallible, context-taking, and refreshable — rather
  than a property read.

**Narrowed further (muster #67).** "Something out of band must populate
it" turned out to mean, in practice, "exactly once, at peer registration,
never again" — a gap the type could not express and nothing was closing. Two
measured bad states followed from that: a cache correctly labelled `observed`
at startup stayed labelled `observed` after the peer it describes restarted
onto different code; and, separately, a cache that missed at startup stayed
`assumed` through an entire successful session's worth of relayed traffic,
never once re-asked. Both are now bounded rather than open-ended: `source`
degrades on its own past a fixed staleness window (closing the first), and
every ordinary operation that reaches the peer opportunistically re-probes
capabilities that are unseen or past that window (closing the second, and in
the common case pre-empting the first). See Appendix A, F58.

The proposed fix above is not this — capability discovery is still a cached
property read, not a fallible, context-taking operation a caller can invoke
directly. What changed is what *drives* the cache: ordinary traffic now
does, where before only a single startup call did.

### D4 — `subscribe`'s filter cannot name a session · §5.5 — **RESOLVED**

Kept for numbering stability.

A filter may now name session ids as well as describe a working-directory
prefix, and the two compose with AND. Measured on the first driver: naming two
sessions out of forty that share a directory opens three connections
(lifecycle plus one each) rather than forty-one.

The residual limitation is not this defect. A subscription still cannot span
machines, because no service implements the event stream and the remote driver
answers `unsupported` — so a filter naming sessions narrows what one machine
watches, not what a fleet does. That is the event plane's missing federation
design, not the filter's shape.

### D5 — Idempotency retention does not outlive the service · §10 — **RESOLVED**

Keys are durable when a state directory is configured, so a caller retrying a
`create` across a restart receives the session it already has rather than a
second one.

**Intent is recorded before the side effect**, which is the part worth stating.
Persisting a key after starting the session closes the restart window and
leaves a narrower one: crash in between, and a retry starts a second agent in
the same working directory — the same disaster through a smaller door. So a key
is reserved first, and completed with the resulting reference afterwards.

A reservation found at startup means exactly one thing, and the response is to
look rather than assume. If a session matching the recorded name **and working
directory** exists, it is adopted and the record completed; if nothing matches,
the create did not take effect and proceeding is safe because there is nothing
to duplicate. Both branches are safe, which is the property worth having.
Matching on the name alone would adopt a recycled name — §5.4's lesson, in a
new operation.

In-memory remains a legitimate configuration for a throwaway instance. What is
no longer possible is a service that looks durable and is not: an unreadable
key table is fatal at startup rather than absorbed into an empty one, because
starting fresh silently discards precisely what §10 exists to keep.

### D6 — Mutation permission cannot distinguish host from client · §6 — **RESOLVED**

Two independent grants, decided by whether the request targets this machine or
a peer:

- **host** — may mutate sessions on this machine (what this host exposes);
- **relay** — may forward a mutation to a peer (what this instance may do as a
  client, which exposes nothing here — the peer takes the risk, and the peer
  has its own gate).

Both default closed. The configuration that was previously unreachable, and is
the one a fleet actually wants, is now expressible: a hardened host that is
still a full-featured client.

Found by deploying, not by reasoning — the first cross-machine mutation was
refused by the wrong machine. See Appendix A, F18.

### D7 — Deadline composition across a hop is unspecified · §4.4

§4.4 makes a deadline mandatory and governs a *caller* shortening a driver's
declared bound: "a caller may supply a shorter deadline; never a longer one."
It says nothing about what happens when the caller is itself a proxy.

The gap has a sharp edge. If a proxy waits less time than a peer has declared
it may take, the proxy abandons calls the peer would have completed — and
reports each one as `unreachable`. A machine that answered is described as one
that did not, which is §5.7's confusion produced by a timer rather than by a
missing field.

Measured, not reasoned: a peer declaring 5s, behind a proxy waiting 3s, on a
host loaded enough that a single subprocess spawn cost over a second. The peer
was healthy and answering throughout.

- **Mitigation, implemented:** a remote driver treats its configured deadline
  as a *floor*, and once the peer's capabilities are known waits at least the
  peer's declared deadline plus a transit margin. Before they are known it can
  only use the floor — which is one more consequence of D3, capability
  declaration being unable to say "not yet known".
- **A second edge, found while fixing the first:** the bootstrap is circular.
  The deadline a proxy should enforce is derived from the peer's declared one,
  but learning it requires a call — and bounding that call by the too-short
  floor is exactly what the derivation exists to correct. Resolved by treating
  capability discovery as out-of-band metadata that honours only the caller's
  context: §4.4 governs *session operations*, whose point is that they must
  not block unboundedly, and a probe whose purpose is to discover bounds is
  not one of them.
- **Mitigation, implemented (#175): announce less than you enforce.** The
  same confusion has a second source on the announcing side. A proxy that
  tells the peer the exact bound it enforces makes both timers expire
  together, so a peer that is up but slow answers honestly and is still
  recorded as `unreachable`, because its answer is in transit when the proxy
  gives up. A remote driver now announces its remaining budget minus a
  transit reserve (capped at 250 ms and at one fifth of what remains, never
  below 1 ms). The waiting side's margin and the announcing side's reserve
  are separate allowances and are not merged.
- **Still open:** the general rule. A fleet more than two machines deep, or
  one where a peer raises its deadline at runtime, needs deadline composition
  stated in this document rather than implemented in one driver. The transit
  reserve does make each hop's announced budget *shrink*, which is the
  direction a chain of proxies needs, but only as an implementation choice in
  one driver, not as a rule this document states. How the reserve should
  compound across more than one hop is unanswered, and §13.1's one-hop rule is
  currently what keeps the question from arising.

### D8 — Events do not cross machines · §5.5, §13 — **RESOLVED**

A relayed event keeps the originating machine and the origin's own
`(cursor, epoch)` as provenance, and receives the relaying service's cursor for
local ordering. So "resume from cursor N" is never ambiguous about whose N,
while a caller that later talks to that peer directly can still resume there
rather than refetch. This is the same "adopt what the peer said, add only what
you are uniquely positioned to know" split §13.2 uses for source status and F20
for error kinds.

A proxied subscription asks the peer for `scope=local`, and a service serving a
`scope=local` subscription neither streams from its own peers nor delivers peer
events. §13.1 applies to subscriptions, and violating it here is worse than in
a unary call: two mutually-configured machines would each hold an open stream
to the other indefinitely, and a long-lived loop does not announce itself the
way a failed request does.

An interrupted peer stream is announced with a `source.status` before any
reconnection, then resumed from the last cursor seen. Reconnecting quietly
would leave a caller unable to distinguish "this peer has nothing to say" from
"we stopped listening". See Appendix A, F25.

### D9 — A shared stream cannot present per-caller authority · §6, §13 — **RESOLVED**

The observation stands and is now bounded rather than unbounded.

A multiplexed subscription still has many callers at once and outlives any of
them, so it cannot present any one caller's authority; the service subscribes
to peers as itself. What has changed is what "itself" means. It is now a
distinct principal with its own credential and its own grants, so a peer can
grant this service read access to its events without granting it anything else,
and without that being the same authority every caller holds.

Under one shared token the widening was total and invisible: "as the service"
and "as any caller" were the same string. It is now explicit, bounded by the
grants that principal was given, and visible in the peer's audit log.

Residual, and inherent rather than fixable: a subscriber reads a peer under the
service's authority rather than its own. Multiplexing means the stream cannot
be per-caller without becoming per-caller streams, which reintroduces duplicate
events with competing cursors (see the event plane's design). An operator who
needs per-caller peer reads must give that caller its own service, which is a
real answer even if it is not a cheap one.

---

## Appendix A. Findings log

How the rules above were learned. This appendix exists because the reasoning is
worth more than the conclusions: a reader who knows only the rule will restate
it, while a reader who knows how it was violated will recognise the next
instance.

Kept deliberately, per this repository's standing preference — *knowing a
design was wrong once, and how it was found out, is worth more than a clean
document.*

### Phase 1 — transcribing the spec into types

Nothing ran yet. These are places the prose admitted more than one reading, or
where the document's own pseudocode did not survive being made to compile.

**F1 · A session id is scoped to `(machine, runtime)`, and the URL had no room
for the runtime.** Two runtimes on one machine may legally reuse an id;
`/machines/{machine}/sessions/{id}` cannot disambiguate. api-http.md gained an
optional `?runtime=` parameter, on that document's own rule that where the two
disagree, the abstraction wins and the wire document is the bug.

**F2 · `Ack` was named but never shaped.** §3's table returned it from
`interrupt` and `close`; unlike `DeliveryReceipt`, it was never defined. Shaped
to carry acceptance only — anything more would be a driver promising
synchronous completion it may not be able to deliver.

**F3 · `list` could not return a bare array.** §3 wrote `-> SessionRef[]`. Two
independent rules forbid it: §9 requires every plural response to be an
envelope with `sources`, and §13.2 requires a proxy to adopt a peer's own
`SourceStatus` — for which a slice has nowhere to put it. The item type became
`Session` rather than `SessionRef` for a third reason, confirmed later by
measurement (F10): a batch operation whose natural shape is cheap must not
force per-item follow-up calls.

**F4 · Nobody owned `complete`.** §9 said it was "false if any source failed to
answer" without saying who computes it, or whether `degraded` counts. Settled
as derived-never-supplied, with `degraded` flipping it false.

### Phase 2 — the first working driver

A local driver over a terminal multiplexer, developed against a machine running
22 concurrent live sessions.

**F5 · A driver that could read nothing reported a healthy fleet.** The batched
screen-capture markers were built from pane identifiers, and the command
emitting them passes its argument through `strftime` — which consumes `%`, the
character every pane identifier begins with. Every capture was misfiled, so
every session was classified from an empty string.

The driver then returned a complete, well-formed, **error-free** view of 22
sessions, all `unknown`, and passed its entire unit suite. Nothing anywhere
said *"the driver failed to read."* It said *"the sessions are unknowable"* — a
claim about the fleet rather than about itself, and false.

This is §5.7 operating one level below where §5.7 states it, and it is why that
section now governs the inside of a driver too.

**F6 · The `working`/`idle` distinction rests on the tense of a randomly chosen
verb.** The runtime's interface signals a turn in progress with a spinner whose
verb is drawn at random per turn, distinguishing running from finished by that
verb's grammatical tense and suffix shape:

```
✻ Zigzagging… (5m 57s · ↓ 21.3k tokens)   <- running
✻ Worked for 2m 7s                         <- finished
```

A driver keying on that is keying on the tense of a random English word in an
interface with no compatibility contract. It works today; it is one release
note away from being wrong, and wrong *silently*, because a missing spinner
reads exactly like a finished turn.

`confidence: inferred` is therefore not modesty on this substrate — it is the
literal truth, and §2.3's `unknown` earns its place as a first-class answer.

**F7 · `supportsResume: true` was honest while idempotency keys evaporated.**
Sessions here are owned by the multiplexer, so they genuinely survive a service
restart. Keys lived in memory. The capability flag was being read as covering
both. See D5.

**F8 · Two silent gaps in the event stream.** First: `Event` carries `cursor`
and `epoch`, which §7.3 assigns per service instance — a driver has neither,
and one that helpfully invented a cursor would produce a stream that looks
correct until a subscriber reconnects and the resync comparison misses the gap
it exists to catch. Second: taking the baseline snapshot inside the engine
goroutine let everything between `subscribe` returning and that snapshot be
absorbed into the baseline, unreported and *unannounceable*. Found by writing
the race and then watching a test absorb the very change it was asserting on.

**F9 · Push exists, but is scoped per attachment.** Measured on the substrate
rather than assumed:

| notification | delivered to a client attached elsewhere? |
|---|---|
| content (`%output`) for the attached session | yes |
| content for a sibling session | **no** |
| format subscription, per-pane | attached only |
| format subscription targeting a sibling's pane by id | **no** |
| session appearing / disappearing | **yes — fleet-wide** |

Content is per-attachment; lifecycle is global. One always-on client therefore
covers every session appearing and disappearing, while watching a session's
content costs a connection. That asymmetry is what makes filter granularity a
cost parameter (D4), and it is why notifications are used as change *triggers*
feeding the ordinary batched read rather than as a second interpretation of
screen bytes — two sources of truth about status would disagree only under
load.

**F10 · Enumeration cost is structural, not incremental.** On 22 live sessions:

| approach | subprocess spawns | wall clock |
|---|---|---|
| per-session capture loop | N+1 (23) | 119 ms |
| one batched invocation | 1 | 18 ms |

Constant in session count rather than linear — about 5 ms per session becomes
about 0.15 ms. This is why `list` returns everything in one call, and why a
driver that implements `list` by looping `state` has reproduced the cost the
interface exists to avoid.

### Phase 3 — the second driver

An HTTP client to a peer service, satisfying the same interface. It found a
different *class* of problem: where the first driver exposed places the model
was imprecise, this one exposed places the interface has **no room for a
concept it requires**.

**F11 · The confused-deputy fallback.** See D1. Worth restating once here
because of how it presents: the bug's symptom is that everything works.

**F12 · A remote driver cannot answer a synchronous question about a peer.**
See D3.

**F13 · What did survive, and it is the point of the exercise.** §4.2's claim —
that a remote peer is just another driver — held end to end, with no special
case in either service: a caller asked a service holding no local drivers,
which proxied through the remote driver, over HTTP, into a second service, into
the multiplexer driver, and back with 22 real sessions. `confidence: inferred`
survived the round trip rather than being flattened to `observed`, which is the
single easiest thing for a federation layer to destroy and the one §5.6 exists
to protect.

### Phase 4 — fixing the security defect, and deploying

**F14 · Caller authority became a parameter, and the fix is the type rather
than the policy.** D1's failure had a specific shape: authority travelled in an
out-of-band context value, a service could forget to attach it, and a remote
driver missing it reached for the one credential it certainly had — its own.
The request succeeded. The tests passed. Authorization silently widened.

Two changes, and the second matters more than the first. Authority is now an
argument of every §3 operation, so it cannot be omitted. And the remote driver
was stripped of any credential of its own, so there is nothing left to
substitute — a policy a driver could get wrong became a property of the type.

The compiler is the enforcement: changing the interface broke every driver, the
service, and every test in one pass, and each break was a place that had to
decide what authority it was acting under. That is exactly what an out-of-band
value cannot do.

Proven on two machines afterwards, because a fix to an authorization path that
has never carried a real request is a hypothesis. Input sent from one machine
landed in a pane on the other; a session was destroyed across the network; and
§5.4's corroboration refused an id the far side had never observed, which is
the protection working at the exact distance it is hardest to get right.

**F15 · The reverse direction needed a bind change, not a config change.** One
machine bound loopback, so it was unreachable regardless of how the other was
configured — a reminder that "can A call B" and "can B call A" are independent
facts, and only one of them had been tested.

Making both machines peers of each other also produced the first real test of
§13.1. With a genuine 2-cycle in the configuration, a `scope=local` query
returns one source and does not forward; a fleet query from either side returns
identical counts. The document admits the first spike "avoided this only by
being a star" — this one did not avoid it.

**F16 · Two defects, one shape, one fix.** D1 (authority) and D2
(corroboration) were logged separately and read as unrelated: one a security
problem, one a correctness problem. Fixing D1 by adding a parameter made the
similarity obvious — both were *an operation needing caller-side context with
nowhere to carry it*, and both had been "solved" by moving the value out of
band, where D1 could be silently forgotten and D2 could not be enforced at all.

So the second fix generalised the first rather than repeating it: one envelope
(§2.6) carrying authority now and expectation next, with room for whatever the
third instance turns out to need. The test that matters is the one that was
previously unwritable — a driver whose *own* sighting is current and would pass
its weak check must still refuse when the *caller* is quoting a session that no
longer exists at that id.

Two smaller things fell out. Reads had to start returning `startedAt`, because
a caller cannot quote a value it was never given — a guarantee is only as
reachable as the data needed to invoke it. And a proxy had to be made to
forward the expectation rather than evaluate it, since corroborating on the
relaying machine would insert a third party's belief between the caller's
observation and the destroy.

**F17 · Deadlines were deliberately left out of the envelope.** They were in
the first sketch and removed on the same principle the design applies
elsewhere: the context already carries them, §4.4 already governs them, and a
second field would be a second source of truth free to disagree with the first
— the failure §9's `complete` and §13.2's source status both exist to prevent.
Recorded because "we considered it and did not" is worth more than silence when
the next reader wonders why the obvious field is missing.

**F18 · The first cross-machine mutation was refused by the wrong machine.**
Sending input from one host to a session on another returned "this instance is
configured read-only" — from the *relaying* machine, which was not being asked
to mutate anything of its own.

One flag had been governing two questions: what this host exposes, and what
this instance may do as a client. The only way to relay was to open this
machine's own sessions to mutation, which is precisely backwards — the machine
taking no risk had to accept all of it.

Worth recording as a category, not an incident: a permission that reads
naturally as one sentence ("may this service mutate?") can still be two
questions, and deployment is what separates them. No amount of reading the
specification produced this; one `curl` did.

**F19 · Subprocess spawn cost is not constant; it degrades with load.** The
enumeration measurements in F10 were taken on an idle machine. On a peer
carrying 79 agent sessions at load average 63, the raw multiplexer work still
took 0.24s — but the same read through the service took 1.87s. The difference
is fork/exec latency under load, not the driver.

Two consequences. It strengthens the case for minimising spawns rather than
weakening it: the machines that most need fleet visibility are exactly the busy
ones, and that is where per-session spawning would be most catastrophic. And it
is what exposed D7 — deadlines tuned against an idle machine are not deadlines
at all.

**F20 · A proxy was quietly downgrading its peer's error classification.** The
peer correctly answered `conflict` for a destroy whose expectation was stale
(§5.4). The relaying service re-derived a kind from the Go error it held and
produced `invalid` — telling the caller to fix its syntax when what it should
do is re-read and decide.

This is §13.2 — "adopt a peer's SourceStatus; never re-synthesize it" — applied
to errors, and nobody had noticed the rule generalised. The peer had already
classified the failure; a second opinion downstream can only lose information.
A proxying service now relays a classified error verbatim.

Three instances of the same rule now exist (source status, error kind, and the
peer's `count`), which is enough to state it generally: **a proxy relays what a
peer said about itself and derives nothing.** The reachability of a peer is the
proxy's observation to make; everything the peer reported about its own answer
is the peer's.

**F21 · §5.7, found for the fourth time, and fixed by copying §2.3 rather than
inventing.** Capability declaration had the same collapse the design had
already solved twice: an all-false value meaning both "supports nothing" and
"nobody has said". The fix borrows the shape §2.3 uses for session state —
a value plus its provenance plus when it was obtained — instead of designing a
third mechanism for the same problem.

That is worth stating as a working rule. When this design meets absence again,
the answer is not a new type; it is `(value, provenance, observed-at)`, because
that trio is what the two previous instances converged on independently.

The immediate payoff was in D7's fix, which derives a proxy's deadline from its
peer's. Before provenance, "the peer declares 0ms" and "we have never asked"
were the same reading, and the derivation could not tell whether it was
applying a floor because the peer was minimal or because nobody had checked.

**F22 · Naming a thing costs less than describing it, when watching is
metered.** The filter originally carried only a working-directory prefix, which
reads like a reasonable minimum until you notice what a driver must do with it:
attach to every session that matches, because it cannot know which one the
caller actually meant. Forty sessions sharing a directory cost forty
connections to serve a caller interested in one.

The general form is worth keeping. **Where an interface offers only a
descriptive selector, the implementation must satisfy the description — and
pays for the gap between what the caller said and what the caller wanted.** An
identifying selector closes that gap. This is §5.4's lesson ("a proxy for
identity is not identity") arriving in a second operation, where it costs
connections rather than correctness.

One consequence had to be reasoned about rather than measured: naming an id
inherits recyclability, so a subscription can silently change subject when an
id is reused. It is acceptable here only because the stream announces the
change — closed, then created — which is the same property §7.3 demands of
reconnection. Had the stream not already been obliged to announce, this fix
would have introduced a silent gap while closing a cost problem.

**F23 · Two harness faults found while proving F22, both of the same kind.**
Neither was in the driver, and both would have quietly devalued the tests that
guard it.

A data race in the fake multiplexer: the test goroutine mutated it while a live
subscription's engine goroutine read it. Latent for as long as subscriptions
have been tested, and surfaced only when a new test shifted the timing. Every
subscription test was therefore trustworthy by luck rather than by
construction.

And an equality assertion across two reads of a live machine — federated count
versus direct count — on a host where sessions are created and destroyed while
the test runs. It failed once, passed on retry, and that is the worst outcome
available: a test that fails at random teaches people to ignore failures, which
costs more than the test was ever worth.

Recorded because the pattern generalises past this repository: **when a test
asserts on a moving system, decide what must hold and assert that, not what
happened to be true when it was written.** What matters here is that federation
carries sessions faithfully; that the fleet stands still is not a property
anyone claimed.

**F24 · Implementing the stream answered two questions the document had left
open, and both answers were forced rather than chosen.**

The SSE framing question — does `kind` travel as the `event:` line, a JSON
property, or both — turned out to have no defensible single answer. `event:`
is what makes a browser `EventSource` able to listen by kind; the JSON property
is what spares every other client from parsing SSE framing to learn what it
received. Picking one makes the stream awkward for half its consumers, so it
carries both, plus the cursor as `id:` so a reconnecting browser sends
Last-Event-ID without any client code. Redundancy chosen deliberately, at the
cost of a short string per event.

Backpressure (§7a) had exactly one answer consistent with the rest of the
design. Dropping silently hands a subscriber a hole it cannot detect, which is
the failure this specification is organised against; blocking lets one slow
reader stall the machine's event plane. So a subscriber that overflows is
marked and resynced — the same announcement §7.3 already required for a cursor
that falls off the retained window.

Neither answer required a judgement call. Both were determined by rules already
written down, which is the clearest sign so far that the design has become
self-consistent enough to decide things on its own.

**F25 · D1's rule met a case it did not anticipate, and the failure was
silent.** The hub's peer pump called `subscribe` with a system request carrying
no credential; the remote driver correctly refused, the pump returned, and the
event plane was local-only with nothing logged and every test passing. The
first symptom was a live cross-machine subscription that simply produced
nothing.

The lesson is not "add a credential". It is that **a rule phrased in terms of
"the original caller" quietly assumes one caller per request**, and a
multiplexed stream violates that assumption without violating the words. See
D9.

**F26 · A field was added to the event type and forgotten in its wire form.**
`Origin` existed in memory, survived every unit test, and vanished at the
encoder — the live cross-machine test showed events arriving correctly
attributed with `origin: null`.

The separate wire envelope exists precisely so the stream's shape is stated in
one place, and it still drifted, because "stated in one place" only helps if
something checks the two against each other. There is now a test that reads a
frame off the wire and looks for the field. Worth generalising: **a type that
mirrors another needs a test that crosses the boundary between them, or it
mirrors it only until someone edits one side.**

**F27 · D1 was right about where authority must travel, and its mechanism only
worked by coincidence.** Forwarding the caller's literal credential to a peer
was correct under one shared secret — and that shared secret was exactly what
§6 requirement 3 needed removed. Introducing per-peer credentials therefore
broke the fix for D1, which had been verified working across machines a few
hours earlier.

The rule survived; the implementation did not. Authority now travels as
transport identity plus an asserted principal, which is what "present the
original caller's authority" has to mean once the caller's credential is not
meaningful on the far machine.

Worth generalising, because it is easy to mistake one for the other: **a fix
verified end to end proves the mechanism worked under the conditions that
existed, not that the mechanism is what the rule requires.** The conditions
here were a deployment convenience nobody had chosen deliberately, and removing
it invalidated a working, tested, deployed path.

**F28 · The refusal that protects a session can also strand it, and the same
function failed both ways.** §2.4's refusal exists so input is never
concatenated into a message a human was still typing. Its detector reads a
terminal, and a detector that reads terminals is wrong in two directions with
very different consequences:

- **False positive** — text that is not pending input is read as pending, so
  every send to that session is refused, permanently, for text nobody typed.
  The session simply stops responding to its supervisor, with no error anywhere
  and a reason that names input that does not exist.
- **False negative** — real pending input is missed, and the next delivery
  concatenates into a half-typed message. Invisible when it happens.

Both were live. A selection menu marks its highlighted option with the same
glyph as the composer prompt, so a session sitting on a menu read as holding
input — found by running the detector across every session on a real machine
rather than by reasoning about it. And the fix for that (requiring the composer
to be fenced by rules on both sides) then depended on recognising a fence whose
label can be longer than its dashes, where two successive thresholds failed on
real screens.

The rule that survived is deliberately generous about what counts as a fence,
because the two errors are not symmetric: **a refused send is visible and
recoverable; a corrupted message is neither.** Where a detector must be wrong,
it should be wrong in the direction somebody notices.

Recorded at length because the failure mode generalises past this driver: any
component that infers intent from a rendered interface will eventually read
that interface's own furniture as content, and the first symptom is a
correctly-functioning system that has quietly stopped doing anything.

**F29 · Persisting the epoch is only honest if the cursor persists with it.**
§7.3's epoch tells a subscriber whether its cursors still mean anything. The
obvious way to stop every restart resyncing every subscriber is to keep the
epoch — and doing only that would be a lie, because a service reusing numbers
it had already issued is worse than one announcing a new instance.

So the cursor high-water mark is persisted alongside. The retained event
*window* deliberately is not: a subscriber resuming from an old cursor still
gets `resync_required`, but now with the truthful reason — the sequence
continued, this service simply cannot replay that far back. Persisting the
window would buy transparent restarts at the cost of durably storing every
event, a much larger mechanism than the problem justifies.

Verified across a real restart: same epoch, and the next events issued cursors
3 and 4 rather than starting again at 1.

The general shape is worth keeping. **Durability decisions come in sets.** A
field that identifies a sequence and a field that positions you within it are
one decision wearing two names, and persisting either alone produces a service
that describes itself incorrectly.

**F30 · Reconciliation read the records its own first read had just written.**
An ordinary read records the live set when it changes. Reconciliation enumerated
first and loaded records afterwards, so it compared the world against a snapshot
it had itself produced moments earlier: every session adopted, nothing ever
orphaned or vanished.

The classification still ran. It still produced an answer. The answer was that
everything was fine, always — which is the shape of failure this project keeps
meeting: not an error, but a confident report built on evidence the reporter
manufactured.

Fixed by reading what was remembered before looking at what exists. Worth
stating as a rule, because the same trap is available anywhere state is both
read and written on a common path: **a process that compares "before" against
"after" must capture "before" prior to anything that can write it — including
its own instrumentation.**

### Phase 5 — answering, and driving a live agent on another machine

The read path was federated and the write path worked locally. What remained
was the case the whole layer exists for: an operator on one machine starting an
agent on another, and getting it past every question it asks before it will do
any work. Every finding below came from attempting exactly that.

**F31 · A session can be lost to a dialog nobody can reach, and there were
three of them.** In one working session a supervisor met a folder-trust
question on every newly created session, a resume-from-summary question on a
session being reattached, and a menu inside a running conversation. None could
be answered, because `send` is built to guarantee it never produces a
keystroke — the property that makes it safe for messages is exactly what makes
it useless for control.

The consequence was not a degraded session but an unreachable one: an agent
could be started and then never got past its first question. §3 gained
`respond` for this.

Two detection failures compounded it. The menu detector knew one footer
(`Enter to select`) and both real prompts used another (`Enter to confirm`), so
they classified as `unknown` — which reads as "cannot determine" rather than
"blocked on a human", and a supervisor waits forever on something that will
never move by itself. And a fresh session renders a **placeholder hint** in its
composer, which the composer detector read as typed input, refusing every send
to a session nobody had ever spoken to.

The placeholder is separable only by how it is painted: the hint is rendered
dim (SGR 2) and typed input is not. Matching the hint's words would have
repeated the spinner-verb mistake — prose in an interface with no compatibility
contract. **Where an interface distinguishes two things visually, the
distinction is in the rendering, and reading the text instead is guessing.**

**F32 · Create manufactured the stuck session it exists to avoid.** §2.1 lets a
spec carry an initial prompt; §4.4 bounds every call by the driver's declared
deadline. On a runtime that takes far longer to paint its interface than any
sane deadline, those two requirements do not fit — so Create delivered
immediately, the paste landed, and the submit keystroke was swallowed during
startup.

The prompt then sat unsent in the composer, indistinguishable from a human's
half-typed message, and every later send was refused to protect text the
session had put there itself. Delivery now happens after Create returns,
bounded, and only once the interface is ready — and stops if a prompt is
waiting, because clicking through a trust question is a consent decision a
driver must not make on a caller's behalf.

**F33 · A sibling project had already measured this family, and two of its
findings were bugs here.** A supervisor built on the same substrate has been
tracking "text arrived but was never submitted" for months. Reading its issue
tracker was worth more than any amount of further testing, because it had
counted things a single session cannot: eight stranded operator instructions in
one day, and 37 of 39 panes fleet-wide holding the same unsent line.

Two of its results applied directly:

- **Submitting immediately after delivering loses a race.** The submit can win,
  the prompt is submitted empty, and the text lands afterwards — where it sits
  unsent forever. Delivery is now confirmed on screen before submitting, and a
  failure to confirm is reported as `unknown` naming the stranded text rather
  than silently dropped.
- **`Enter` is not reliably the same as `C-m`.** The same pane, seconds apart,
  ignored `Enter` and submitted on `C-m`. Both are "the same character" in
  principle; only one has been observed to work when the other did not.

Also worth recording: that project found a prompt whose highlighted default is
`No, exit`. A caller that reflexively accepts the default would kill the
session it was trying to start — which is why §2.7's `choice` is explicit, and
why §2.3's evidence now names the highlighted option rather than merely
reporting that something is blocked.

**The general lesson is about where to look.** A design can be argued about
indefinitely; a system that has been run in anger has counted its failures. When
one exists next door, its bug tracker is evidence, and evidence outranks
reasoning.

**F34 · The field that had been filled with nil the whole time was the answer.**
`since` existed in §2.3 from the first draft and every driver passed nil, because
nothing had needed it. It turns out to resolve a stall that a sibling project
could only diagnose by typing into the pane to see whether characters appeared.

Duration distinguishes "an operator is mid-sentence" from "this pane stopped
accepting input", and those demand opposite responses while presenting
identically in a single reading. One reading cannot tell them apart; two
readings and a clock can.

Two things worth carrying forward. **A spec field that every implementation
fills with nil is not necessarily unnecessary — it may be unused because
nothing has yet needed the question it answers.** And the cheapest new signal
is usually not a new probe but a second look: this needed no extra call, no
extra permission, and nothing done to the session at all.

**F35 · A parser over screen content allocated without a bound, and hung the
service.** `parsePrompt` padded its option list up to whatever index it read,
so a transcript line reading `1000000. something` allocated a million entries.
The live service stopped answering — including its own health endpoint — and it
looked like a network fault until loopback proved otherwise.

The pane is written by an agent that can print anything. Anything parsed out of
it is attacker-influenced input in the general case and arbitrary input in every
case, and the code treated a number on screen as a size. Both the index and the
digit count are now bounded.

Worth stating because the same shape recurs wherever a system reads a rendered
interface: **a value parsed from a display is input, and sizing an allocation
from input is the oldest bug there is.** It is easy to forget when the "input"
looks like a menu.

**F36 · Binding only to a tunnel interface makes a service unreachable from its
own machine.** When the VPN dropped, the service was still listening on its
tunnel address and answering nothing — not even to a client on the same host,
because the route to that address went with the tunnel. It presented exactly
like a hung process: `launchctl` showed it alive, `lsof` showed it listening,
and every request timed out.

§6.1 says exposure beyond loopback is explicit configuration, and that remains
right. What is missing is that a service which can only be reached over a
tunnel has no local fallback when the tunnel is what failed — so diagnosing it
requires knowing to try a different address, which is precisely what nobody
thinks to do while it looks like the process is wedged.

*Fixed:* loopback is now bound automatically, on the configured port, whenever
the configured addresses do not already cover it. The general form is worth
stating, because it is not about tunnels: **a service must remain reachable
over a path that cannot be taken down by the failure being diagnosed.**
Configuring an interface is a statement about who ELSE may reach the service,
never about whether its own machine may.

**F37 · Footer matching was always going to lose, and four variants proved it.**
The prompt detector recognised menus by their footer text. One runtime produced
`Enter to select · Tab/Arrow keys to navigate`, then `Enter to confirm · Esc to
cancel` on two different boot screens, then `Esc to cancel · Tab to amend` on a
tool-permission dialog. Each new screen needed a new matcher, which is how this
class of stall stays permanently one release behind the thing it watches.

What every variant shares is the question itself: a run of numbered options
near the bottom with one marked as highlighted. Detection is now structural
first, with the footer kept as a second signal for the case structure misses —
a long menu whose highlighted marker has scrolled above the captured window.

Neither signal alone is sufficient, and that is the point: **when an interface
has no compatibility contract, match what the interface is FOR rather than how
it happens to be decorated** — and keep the decoration as a fallback, because
the thing it is for can also fall off the edge of the screen.

**F38 · The endpoint a caller reads before destroying did not return what
destroying requires.** A single-session read returned only an id and a state,
because the driver operation behind it returns a state. But §5.4's strong
corroboration needs the caller to quote back the session's start time — and
that field was absent from exactly the response a caller would read first.

F16 already said a guarantee is only as reachable as the data needed to invoke
it. It said so about listings, and the same omission reappeared one endpoint
over. **A rule learned about one surface is not learned until it is checked on
every surface that could break it.**

### Phase 6 — the preconditions for being adopted

Planning a supervisor's migration onto this service produced a short list of
things that had to be true first. None was a missing feature; each was a way
the service could be wrong without being able to say so.

**F39 · A participant that cannot state which code it is running turns every
disagreement into a mystery.** Two machines in one fleet silently ran different
builds. The older one still had a bug the newer had fixed, and the symptom — a
session stranded at a question the newer code answers — made no sense against
the source anyone was reading. The entire diagnosis was spent looking for a
defect that had already been fixed.

Every surface reported health, and each was right by its own standard: the
service was running, answering, and correct for the code it happened to be.
What no surface could express is the distinction that mattered. **"We disagree"
and "we are different vintages" need opposite responses — the first is a bug,
the second is a deploy** — and nothing in the API could tell them apart.

`GET /v1/health` now carries a build identity, and a peer's is learned on the
same probe that learns its deadline. Two details are load-bearing:

- The stamp comes from version control, not a hand-maintained constant. A
  constant records what somebody remembered to bump.
- **An unknown or locally-modified build never compares equal to anything,
  including an identical-looking counterpart.** This is §5.7 again, and the
  asymmetry is deliberate: this comparison exists to raise a warning, and a
  false "same" suppresses precisely the warning worth having. A false
  "different" costs a log line.

The comparison also names *why* it failed, because "different revisions" sends
an operator looking for a lagging deploy, and saying that about a comparison
that could not be made wastes the same diagnosis this finding is about.

**F40 · An unverified deploy is a deploy that can silently not have happened.**
F39's skew was produced by cross-compiling and copying a binary by hand. The
copy never failed loudly; what happened is that a service kept serving the old
binary afterwards, and nothing checked.

The deploy path now asks the running service what it is, *after* restarting it,
and fails if the answer is not what was just installed. It also refuses to
build from a modified tree by default — a binary with no identity cannot
participate in F39's check at all, so shipping one quietly disables the
mechanism that catches the problem.

Worth stating generally: **a deployment step that does not read back the
deployed state is a copy, not a deploy.** The failure mode is never the loud
one.

**F41 · The largest single source of `unknown` was settled by looking twice.**
A fleet-wide read across two machines returned 91 sessions, of which 10 were
`unknown` — and every one carried the same evidence: *no spinner line; composer
present and empty.*

That branch was written deliberately. From one capture, a session sitting at a
fresh prompt and a turn that began too recently to have painted its spinner are
byte-for-byte identical, and §5.6 says degrade rather than emulate. The
classifier was right to refuse.

But the refusal was permanent, and 11% of a fleet reading `unknown` is not a
safe default — it is the state a supervisor's rescue ladder triggers on, so
honest uncertainty at rest becomes intervention against sessions that are
merely idle.

What settles it is not a better screen-reading rule but a **second look**. A
turn that had just begun paints within a second; a screen unchanged thirty
seconds later was not mid-anything. The driver already remembers each pane
between observations — that is where §8's `since` comes from — so the
resolution costs no extra capture and never touches the session. F34's lesson
repeated: the answer was in a field that already existed.

Three details are load-bearing:

- **It resolves only toward less activity.** A screen that CHANGED between
  observations is left `unknown`, not called `working`. Content moves for
  reasons other than a turn, and the wrong direction here interrupts a session
  that was doing nothing.
- **A first sighting still answers `unknown`.** The floor is unchanged; only a
  comparison can lower it.
- **A failed capture yields no fingerprint.** Otherwise two consecutive
  failures compare equal and get read as a stable screen — F5's driver
  malfunction laundered into an observation about the session.

The general form: **when a single sample is genuinely ambiguous, the fix is
usually another sample rather than a cleverer reading of the first** — and the
sample you need is often one you already took.

**F42 · The glyph was an animation frame, and matching it was matching one
frame of five.** F41's fix left a residue of `unknown` on one machine, so the
next step was to open a pane and look. It was 21 minutes into a turn, with a
running status line on screen, and the classifier could not see it: the line
began with `✽` and the detector matched `✻`.

A sweep of every session on that machine found **five glyphs in use at the same
instant** — `✻ ✽ ✢ ✶ ✳` — because the leading character is animated. So a
session's status line was legible or invisible depending on which frame the
capture happened to catch, at random, refreshing several times a second. 16% of
one machine's sessions were `unknown` for this reason alone, and the number
would have moved on its own between any two readings.

This is F37 again, one level down, and the repetition is the point: **the
footer was decoration, and so was the glyph.** What the line is FOR is
announcing a turn, and the parts that carry that meaning — the ellipsis for
running, `for <duration>` for finished — were already being matched. The glyph
only ever needed to be recognised as *a symbol rather than text*.

Widening it immediately produced a second bug worth recording, because it is
the cost of every loosened matcher: the composer's own `❯` is a symbol too, and
sits below the status line. The old scan stopped at the first symbol-led line
it met and reported "found nothing usable", so **every screen went from
one-frame-in-five detection to none at all.** The tests caught it; the fix was
to scan for the status line's *shape* rather than stopping at the first
candidate. Chrome is full of symbols — `❯`, `⏵⏵`, `▸`, `⎿` — and a matcher
loose enough to survive an animation must not treat the first symbol it meets
as decisive.

Worth stating as a rule for anything that reads an interface with no
compatibility contract:

> **Every constant you match against is a bet about what will not change.
> Prefer the ones that carry meaning — a structure, a role — over the ones that
> carry style, because style is exactly what a UI is free to animate.**

**F43 · The attach hint immediately justified itself: the two machines do not
agree on where the binary is.** The first fleet-wide read carrying §2.8 hints
showed one machine answering `/opt/homebrew/bin/tmux` and the other
`/usr/local/bin/tmux` — different package-manager prefixes, because the two
hosts are different architectures.

A client composing its own attach command would have had to know that, per
machine, and would have been silently wrong on one of them the moment the fleet
stopped being homogeneous. The service knows because it is *on* that machine
and already had to resolve the binary to run at all.

Which is the general argument for putting this in the model rather than leaving
it to callers: **the facts a client would have to hardcode are exactly the
facts the machine already knows about itself.** Every one it hardcodes is a
place the fleet is not allowed to be heterogeneous.

**F44 · There are two absences, and one answer was being given for both.**
Writing the consumer-facing guide meant probing every endpoint as a client
would, and a single-session read for an id that does not exist answered
`200 dead`.

`dead` is a claim about history — it existed, and it ended. For an id the
machine has never had, there is no history, so the claim is manufactured. A
caller that mistypes an id would be told its session had **died**, which is
both false and alarming in a way that invites the wrong follow-up.

The driver could always tell the difference and was not asked to: it remembers
what it has seen, which is the same memory §8's `since` and §12's
reconciliation are built on. Seen and now absent is `dead`; never seen is
`not_found`.

Note where this was found. Not by a test, not by the implementation, but by
**writing the documentation for someone else** — and the same exercise is what
surfaced F38 one endpoint over. Explaining an interface to a stranger exercises
it differently from building it, because the builder knows which ids exist.

The original test had encoded the old behaviour under the name "absence is an
answer, not an error". That principle was never wrong; it was applied to a
question with two answers as if it had one — which is §5.7 turned inward, on a
rule §5.7 itself produced.

**F45 · Writing the client guide found three defects, and none of them were
findable from inside.** F44 was one. The other two were the same shape:

- **The nonce was undocumented on the wire.** `respond` accepts a nonce that
  makes an answer refuse rather than land on a question that changed underneath
  it — the entire protection §2.7's design exists for — and the HTTP document
  showed `{ "choice": 1 }` and never mentioned it. Every client written from
  that document would have been unprotected, and nothing would have failed
  until the day it mattered. `Response` also carried no JSON tags: decoding
  worked because Go matches field names case-insensitively, so the omission was
  invisible from the server while making it the one type in the package that
  would MARSHAL as `Choice`.
- **A peer's runtime id was reported as the empty string.** The peer names it
  in the very row the driver reads its capabilities from; the value was
  discarded and a placeholder `""` shipped. A client cannot use `?runtime=` to
  disambiguate a session on a peer if a peer's runtime is never reported.

Each was invisible from the inside for the same reason. **The implementer knows
which ids exist, which fields are load-bearing, and what the server will accept
— so the implementer never sends the request that exposes the gap.** Writing
the guide meant calling the API as a stranger: every endpoint, with wrong
inputs, reading only what came back.

The generalisation, which is now three findings deep (F38, F44, this):

> **Documentation for a consumer is a test suite that runs against the parts of
> a design tests do not reach — the affordances.** A test asserts that what you
> called does what you meant. A guide has to state what a stranger should call
> and what they will get, and the sentences that cannot be written truthfully
> are the defects.

**F46 · The client guide was tested by having someone build from it, twice.**
F45 argued that documentation is a test suite for the affordances. That claim
was itself testable, so it was tested: an agent was given the guide, forbidden
from reading any other file in the repository or calling the service, and asked
to implement a session-management client. It reported per-function confidence
and, more usefully, every question the guide had failed to answer.

The first run failed in one specific place. `create` — the operation the client
existed for — scored *low confidence*, because the guide said "see below" and
had no below. The implementer reconstructed a request body from the *read*
shape and guessed field names. Two of the guesses were wrong in the worst
available way: the server ignores unknown fields, so the create would have
half-worked, producing sessions named by the driver instead of by the caller,
with nothing failing.

The same run also found that a client had no documented way to learn **which
machine it is**. `/v1/machines` carries a `self` flag; the guide never showed
the response. The implementer therefore routed *every* attach — including local
sessions — through SSH to the machine it was already running on.

The guide was corrected and a second, independent implementer given the same
task. `create` moved from low confidence to "no gaps"; the attach path used
`self`; the code ran against the live service and listed 99 sessions, handled
emoji ids, distinguished alive from gone, and surfaced a permission error
verbatim.

Three things this technique is good at, which review is not:

1. **It finds absences.** A reviewer reads what is present. An implementer
   stops at what is missing, and has to say so.
2. **It grades by confidence, not correctness.** "I did this and I am not sure"
   locates a weak passage precisely; a correct-looking implementation hides it.
3. **It is honest about the reader's ignorance**, which the author cannot
   simulate. The author knows which ids exist.

What it does not test is judgement about the *host* language. The generated
client declared `local path=` in zsh, where `path` is tied to `PATH`, and
destroyed its own environment inside every request — a bug the guide could not
have prevented and should not try to.

**F47 · A subscription exhausted the machine it was watching, and the
incumbent supervisor read the result as "everything is gone".** A forgotten
client — one `curl` that outlived the shell that started it — held a
fleet-wide subscription for two hours. Unfiltered, so the driver opened one
control client per session: 62 of them on a 69-session host.

The cost was not paid where it was incurred. Each client is a connection to a
multiplexer server that launchers, supervisors and a human's terminal also use,
and that server has one descriptor budget shared by everything. It reached 262
descriptors and began refusing new clients. Every subsequent connection —
including a plain `list-sessions` — failed with *"server exited unexpectedly"*,
while all 69 sessions and their agents were alive and healthy.

**The incumbent supervisor then logged: `MASS-VANISH BURST — 67 sessions gone
in one tick with no paired kill`.** It had asked, been refused, and recorded
the refusal as an observation about the world. Its own guard — a threshold on
implausible disappearance — is the only reason it paused instead of reaping 67
live sessions.

Three lessons, and the middle one is uncomfortable.

**1. This is §5.7 with real consequences, and it is the strongest evidence in
this document.** Asked the same question in the same conditions, this service
answered `unreachable` carrying the peer's own error text, and its client
printed *"sessions there are NOT shown and are NOT known to be gone"*. The
distinction this specification is built around is not academic: one system
concluded the machine was empty, the other concluded it could not see. Only a
threshold heuristic stopped the first from acting on it.

**2. The design that caused it was already documented, and documenting a cost
is not bounding it.** §5.5's "a vague subscriber pays" was written, measured
(26 clients, 26 MB, released on disconnect) and published in the client guide.
Every word was true and it still took the machine down, because *"the
subscriber pays"* was false: **the subscriber pays in a resource the machine
shares with everything else on it.** A cost borne by a shared substrate is not
a cost, it is a hazard, and hazards need bounds rather than documentation.

**3. Bound the accumulation, not the snapshot.** The obvious cap is on the
initial pass. The path that matters over hours is the one that attaches to
sessions appearing *later*: it bounds the fleet's accumulated history rather
than its size at any instant, and capping only the first pass would have made
the leak slower instead of impossible.

The cap is 16 per subscription, and it costs correctness nothing, because
notifications here are triggers rather than data: any one of them causes a full
enumerate-and-diff across every session. Verified while capped — a session
created afterwards, outside the watched set, still produced `created`, `state`
and `closed`. What degrades is latency on quiet sessions, and the cap is
logged rather than silently applied.

> **A limit that only appears in documentation is a limit the system does not
> have.** If exceeding it damages something outside the component, the
> component must refuse, not describe.

**F48 · A convenience field became injectable twenty minutes after it
shipped.** §2.7 gained `kind` so a client could auto-answer questions it
recognises without matching prose itself — the matching quarantined in one
place, failing to empty. Sound reasoning, and the first live read mislabelled a
**ship decision** as `bypass-permissions`.

The trigger was in the question text, written by the agent:

> "…No auth **bypass**, all 9 security fixes verified real, 29/29 green…"

The classifier had joined question and options and matched a substring. A
client filtering on `bypass-permissions` to auto-answer would have answered
somebody's merge decision.

**The distinction that was missing is who wrote the words.** A runtime dialog's
options are fixed strings the runtime emits. A question is written by the
agent, and the agent writes whatever it likes — so matching it is not merely
fragile, it is **injectable**: an agent that writes "resume from summary" in
its own question could have its decision auto-answered by any client that
trusts the kind. Classification now reads the runtime's option text only.

Two further rules fell out of the same fix:

- **All needles must land in ONE option.** Spread across the set — "Resume the
  deployment" plus "Show me the summary" — they are a coincidence, not a
  question.
- **An agent's own question is disqualified structurally.** When an agent asks,
  the runtime appends affordances no runtime dialog has (an option to type
  freely, one to chat instead). Their presence ends classification rather than
  being weighed — which is a structural tell, not more prose.

Worth stating because the failure direction inverted: every other finding here
is about degrading to `unknown` too readily. This one degraded to a **confident
wrong label**, and confident-wrong is the direction that gets acted on.

> **When a field exists so that automation can act on it, ask who authored the
> bytes it is derived from.** Data an agent controls can be shaped to trigger
> whatever the automation does next.

**F49 · The runtime does not echo a long message, so confirming delivery by
reading it back fails on exactly the messages worth sending.** F33's rule —
confirm on screen before submitting — has held since it was adopted. The first
time a genuinely long message was sent to a live session it produced:

    outcome: unknown
    reason:  text was delivered to the composer but did not render in time to
             be submitted safely; it is sitting there unsent

The text was there. The composer read `[Pasted text #1 +8 lines]`: a
**multi-line paste is collapsed into a summary**, so the bytes just delivered
appear nowhere on screen, and a confirmer looking for them waits out its window
every time.

The failure was safe and honest — it said precisely where the text was, which
is why it took one capture to diagnose — but it would have stranded **every**
multi-line message, and long messages are the ones with something to say.

The collapsed marker is itself positive evidence: it says the composer accepted
a paste. So it is accepted, and matched **structurally** — a bracketed marker
on the composer line naming a count of lines — rather than by its wording. The
runtime is free to reword "Pasted text"; it is not free to stop saying how many
lines it swallowed, because that is the only thing the summary is for.

Worth noting what this is an instance of. Every previous finding about reading
a screen was about the screen being ambiguous. This one is about the screen
being **incomplete on purpose**: the interface deliberately does not show what
it holds. A verifier built on "read back what you wrote" has no answer for an
interface that summarises, and the fix is to recognise the summary rather than
to demand the text.

> **When you verify by reading back, ask what the interface does with inputs
> too large to show. Every UI has a threshold beyond which it stops echoing and
> starts describing.**

**F50 · A session with no quota left reads as the most available thing on the
machine.** A usage-limited session paints exactly like a healthy one waiting for
work — the notice, then an empty composer, no spinner. The classifier had no
rule for it, so the second-look resolution landed on `idle` with evidence that
was true and entirely misleading: *no spinner, and the screen is unchanged*.

`idle` is not merely an under-description here. It is the one status that means
**available, send it work** — so a supervisor picking the least-loaded session
preferentially chooses the ones that cannot do anything, because they are the
stillest things on the machine. Nothing self-corrects either: the block clears
only when a human waits it out or switches accounts.

Reported as `waiting_input` with the limit named and the reset time when the
screen states one — **which was wrong, and corrected within hours by F52**:
§2.3 has had `quota_blocked` since the first commit, and no driver had ever
produced it. The reasoning below about not adding a status member was sound
and rested on a false premise, because nobody checked the enum.

**The detection rule is the interesting part.** After a resume the runtime
re-renders the transcript, so an old limit notice scrolls past again with the
session's later work beneath it. A tail window of two or three lines reads that
as blocked. The only formulation that survives replay is the one an operational
runbook had already written down for humans: **judge by the LAST live line** —
anything printed after the notice means the session carried on, and the notice
is history.

Worth noting where the rule came from. It was not derived; it was **already
written**, in a runbook telling an operator how not to be fooled. A service
absorbing screen-reading from humans should read what those humans wrote down
about being fooled, because they have been fooled longer.

**F51 · A turn that died looks exactly like a turn that finished.** A live
session was reported stuck. Its pane:

    ⏺ API Error: 529 Overloaded. This is a server-side issue, usually
      temporary — try again in a moment.
    ✻ Sautéed for 3m 24s
    ❯

The service said `idle` — *"spinner line in finished form; composer empty"* —
which was true and useless. The turn had **died**, its work was abandoned, and
`idle` is the status that means *available, send it work*.

**Neither classifier caught it, and the supervisor next door has a detector for
exactly this class.** Its stall rule looks for a session that stopped while
BUSY; this one settled its status line into the tidy finished form first, so it
read as a clean end. In 86,723 shadow comparisons there was not one `errored`
classification from either side.

This is the third member of a family, and the family is worth naming more than
any of its members:

| the session | how it paints | the truth |
|---|---|---|
| out of quota (F50) | idle, empty composer | cannot proceed at all |
| turn died on a server error | idle, finished spinner | its work is abandoned |
| message stranded (F49) | idle-ish | what it was told never arrived |

> **A session that failed and a session that succeeded end the same way: quietly,
> with an empty composer.** Every "the agent is stuck" report so far has been an
> instance of that, and the screen alone cannot separate them.

**The fix deliberately did not touch the status.** `idle` is honest here — the
session is up and will accept input, and unlike the quota case any caller can
resume it by sending anything. Reporting `waiting_input` would have been wrong
twice: nothing is being asked, and no human is required.

What was missing is not the current state but a fact about the **last turn**,
so §2.3 gained an optional `lastTurn` — outcome, the runtime's own words, and
whether the runtime itself called the failure temporary. `retryable` is read
from what the screen SAYS rather than inferred from a status code, because
deciding which of somebody else's error codes deserve a retry is exactly the
policy this service refuses to own.

The general rule, which is the third time a variant of it has been recorded
here: **when a status cannot express something without lying about the present,
the missing thing is usually history, and history belongs in a field.**

**F52 · The status I decided not to add had been in the type since the first
commit.** F50 reported a quota-blocked session as `waiting_input`, and the
reasoning was explicit: *"adding a status member would have been a breaking
change for every client to express something an existing member already
covers."*

`quota_blocked` was already there. §2.3 has listed it since the spec was
written — *"alive but refused by its provider"* — §8 gives its transitions
(`working → quota_blocked`, `quota_blocked → working | idle | dead`), the Go
type declares and validates it, and **no driver had ever produced it.**

So the trade-off was argued carefully and against a false premise. Nobody
checked the enum. And `waiting_input` was never quite true here: that status
means blocked on a HUMAN, and a quota block is waiting on a clock or an
account — no answer from a caller unblocks it.

The cost compounded, which is the part worth recording. Reporting quota as
`waiting_input` gave that status a third meaning, which created a real
ambiguity, which needed a new `waitingOn` field to resolve — a field, its
constants, its tests and its documentation, all to disambiguate a status that
should never have carried the meaning. Correcting the status left `waitingOn`
doing the one job that genuinely needs it: separating a question to answer
from text nobody submitted.

> **Before deciding what a design cannot afford to add, check whether it is
> already there.** A closed set that nothing produces is invisible in every
> way except the one that matters — it is in the contract, and clients are
> entitled to it.

The general shape has a name here already: this is the same failure as F34,
where the answer was a field that had been filled with nil the whole time.
Twice now, the fix was to use something the design had and the implementation
had forgotten.

**F53 · Loosening the glyph test cost a second false positive, and this one
only a live session could find.** F42 widened spinner detection from one
character to "a non-ASCII symbol, a space, a capitalised word, and a tense
marker", because the glyph turned out to be animated. The composer's own `❯`
immediately met that description — caught by tests, fixed, recorded.

The runtime's **welcome panel** meets it too:

    │         Opus 5 · Claude Max ·          │ Fixed worktree-isolate… │

Box-drawing `│`, a space, a capitalised word, an ellipsis. So a session sitting
at its own splash screen classified as `working`, and stayed that way — the
panel does not change, so nothing ever re-classified it.

Box drawing and block elements are now excluded: they are chrome, never a
status glyph. But the lesson is about how it was found, not what it was.

**No fixture contained a splash screen**, because every fixture was captured
from a session that had been running for a while. The test suite could not have
caught this, and did not; it took creating a session and watching it boot. The
same is true of the earlier one in this family — the `❯` case was caught by
tests only because the composer is in every fixture.

> **A test suite built from captures of steady state cannot see the states a
> system passes through on the way there.** Startup screens, first paints and
> transitional panels are exactly where a screen-reader is least tested and
> most wrong.

**F54 · The condition lasted four days; our detection lasted one screen.** An
account hit its weekly limit and every autopilot session on that machine
stopped. Hours later, measured live:

| | |
|---|---|
| sessions on the machine | 51 |
| panes still showing the limit notice | 2 |
| sessions we reported `quota_blocked` | **0** |
| sessions we reported `idle` | **48** |

F50 taught that a quota-blocked session must not read as `idle`, and F52 moved
it to the status the spec had defined. Both were about the instant the notice is
on screen. **Neither noticed that the notice is transient and the condition is
not** — the runtime prints it once, anything else printing scrolls it away, and
the account stays refusing for days.

So the fix worked exactly as designed and protected almost nothing. A
supervisor reading the fleet would dispatch to 48 sessions that could not run,
which is the failure F50 was written to prevent, arriving by a different route.

**A usage limit is a property of the ACCOUNT, not of a pane.** It is now
remembered per machine, persisted (a weekly limit outlives any restart, and
restarting is how this service is deployed), and applied to sessions that would
otherwise read `idle`. Only `idle` is rewritten: a session mid-turn, at a
prompt, or holding unsent text has a more specific truth observed just now, and
a remembered fact must not overwrite it.

**It clears on evidence, not on a clock.** One session observed WORKING proves
the account works. The scraped reset time is deliberately not used to expire it
— a supervisor next door parsed the same line into `"Aug 10 at 12am
(Asia/Tokyo)      /usage-"`, with the next widget glued on. That hint is worth
showing a human and is not worth acting on, so it travels as a field a caller
may display and must not parse.

> **Ask how long the condition lasts, then ask how long its evidence stays on
> screen.** Where those differ, reading the screen answers a question nobody
> asked — and a detector that is right only while the announcement is visible
> is a detector that is wrong for as long as it matters.

**F55 · F54's fix was correct and still reported 48 sessions idle.** Deployed,
verified on the machine that inspired it — and the peer, the one actually
blocked, did not move. The reason was a line of code written before a lesson
this same file had already recorded an hour earlier: the notice was matched only
as the LAST live line. On the real screen it never is.

```
  ⎿  You've hit your weekly limit · resets Aug 10 at 12am (Asia/Tokyo)
     /usage-credits to finish what you're working on.
  ✻ Churned for 15s
```

The runtime prints the notice, finishes its own sentence, then settles its
status line — precisely the shape `lastTurnFailed` was written for. **A finding
recorded in prose does not propagate to the code that needed it.** F54 widened
where the fact lives (pane → account) and left untouched where the fact is
*seen*, and one unexamined line kept the whole mechanism at zero.

What separates a live block from a replayed one is what comes AFTER: a session
that carried on has agent output below the notice, and agent output carries the
response bullet. Chrome does not. So the window is eight lines and the divider
is the bullet. Two bugs fell out of looking: scanning upward met the
continuation line first, matched it, and returned — finding the block and
discarding the reset time, the one detail an operator wants.

**And the same corroboration must run in reverse.** A notice sits on screen long
after the limit lifts, because nobody types into a session that refused them, so
nothing overwrites it. Measured on a working machine: two sessions reporting
`quota_blocked` from expired notices while two others on that account worked. A
session working NOW outranks a screen that has not changed since it was refused
— the same divider as above, with the evidence in another pane instead of a
lower line.

> **A rule about freshness needs a clock on both ends.** F54 asked how long the
> condition outlives its evidence. It did not ask how long the evidence outlives
> the condition, and both errors put a session in the wrong state.

**F56 · The supervisor learned one fact 48 times, and never concluded it.** The
same outage, from the consumer's side: autopilot recorded 48 stall reasons and
never formed the single statement that explained all of them. It could not.
Nothing told it — it learned by dispatching work and watching it fail, so every
discovery cost a session already sent.

Reporting state correctly is necessary and does not fix this. A scheduler that
must POLL to discover it is refused has already paid before it can act. So the
account's state is now an **event** — `machine.quota`, the only event here whose
subject is not a session, and the only one whose right response is to stop doing
something. It fires at the transition, announces a block already in force to a
subscriber that joins mid-outage, and retracts explicitly rather than by
silence.

The honest limit, searched for before it was assumed: **there is no advance
warning to escalate on.** Three corpora, chosen because they fail differently —
400 lines of scrollback across 51 panes on two machines (what is on screen now),
the runtime's own on-disk state (which records the rate tier and never
consumption against it), and 475 session transcripts from the preceding three
weeks (what was ever on screen). No approaching-limit notice in any of them.
The runtime does not warn, it refuses, so the first refusal is the earliest
signal that exists.

This is worth stating as a negative result rather than a gap, because the
tempting response to "escalate before the limit" is a detector for warning text,
and the text is not there. A margin has to live in the scheduler's own dispatch
budget. What this service can honestly offer is the transition, delivered the
moment it happens, to something that has not yet spent a session finding out.

**F57 · `unknown` was excluded from the account block for being a truth. It is
the opposite.** F54 rewrote only `idle`, reasoning that any other status "has a
more specific truth to tell". That is right for `working`, `waiting_input` and
unsent text, and exactly wrong for `unknown` — which is this driver stating it
could not determine a truth at all (§5.7).

The cost was visible within a minute of watching the blocked machine: four
sessions flapping `unknown` → `quota_blocked` → `unknown` across consecutive
reads. Their panes redraw a footer counter, so the screen digest changed, so
the ambiguity that resolves to `idle` never settled — and a session that never
reaches `idle` never reached the rewrite. Eight spurious state events per cycle
on a fleet where nothing was happening.

> **A rule that lists which states outrank a remembered fact must say where
> `unknown` sits, explicitly.** It reads like a status and behaves like one in
> a switch, and it is the one value in the set that asserts nothing — so every
> precedence rule written without naming it will place it wrongly, in whichever
> direction the code happened to fall.

> **When a consumer must ask to find out, the answer arrives after the cost.**
> A fact worth acting on is worth pushing — and reporting it accurately N times
> is not the same as telling someone once.

**F58 · A capability answer correctly labelled `observed` at the moment it was
cached stayed labelled `observed` forever, because nothing ever asked again.**
D3 (§4.3) fixed the label separating "nobody has told me" from "supports
nothing" — but `source: observed` only asserts that the driver these
capabilities describe reported them *at some point*, and the remote driver's
one and only probe ran at peer registration. Under a keep-alive supervisor
that process lives for weeks, so the assertion's actual shelf life was "until
someone happens to restart the machine reading it" — unbounded in practice,
and the wrong machine to look at besides: the workaround is a restart of the
*observer*, which is right about itself and wrong about its peer, not of the
peer everyone would think to check first.

Two bad states came out of the same root cause, reproduced on purpose rather
than found by accident:

- A capability-adding release, deployed peer-second per the documented order,
  leaves the peer that upgraded *first* describing the second one exactly as
  it was before the release — `observed`, and inverted on both flags that
  gate behaviour.
- Restarting the observer *after* the peer had already upgraded produces the
  opposite failure: capabilities never populate at all, labelled `assumed`
  honestly, and — this is the part that would not have been guessed — that
  never converges. A create (`201`), a keypress (`200`), two guarded refusals
  (`409`) and a close (`202`) all reached the peer's driver and came back
  with a domain answer in between, and not one of them touched the cache.

The instinct the first bad state suggests — schedule a re-probe — turned out
to be the wrong fix to reach for. What resolves both bad states is not a
timer nobody would tune correctly anyway, but the fact already sitting in
every one of those five successful calls: **an ordinary verb that returns a
domain answer is proof of exactly what a capability probe is trying to
establish**, obtained for the cost of a request that was going to be made
regardless. The fix piggybacks on it — refresh when the cache is unseen or
past a bounded age, off the back of whatever operation a caller happened to
send — and adds a synchronous degrade to `assumed` past that same bound, so
the first bad state cannot recur even in the gap before a refresh completes.

> **An `observed` label answers "did anyone ever tell me," not "is this still
> true."** The two questions coincide at the moment of observation and
> silently diverge afterward, for exactly as long as the cache lives — which,
> for a value nothing ever re-checks, is the life of the process. A label
> that means the first thing will eventually be read as meaning the second.

**F59 · A runtime taking the whole screen reads exactly like a pane running
the wrong thing, and both refusals a caller could try next assert a cause
neither one had actually established.** #48 asked whether a control-channel
command raises a *prompt*; it does something the model handles worse. The
runtime takes the whole screen — the composer disappears along with
everything else the classifier reads by — and the driver loses its anchor
entirely rather than seeing a dialog it merely fails to recognise.

Measured live, not reproduced from a description: a session sent its own
control-channel command, settled through `starting` while young, and then —
still with no composer — crossed into `unknown` with an evidence sentence
that named one cause of the missing composer as though it were the only one:
*"pane may not be running the expected runtime."* The runtime was running
exactly the expected thing. It was showing a screen the classifier has no
furniture to read.

Both refusals a caller would reach for next made the same mistake in the
opposite direction — not silent about the cause, confidently wrong about it:

- `input` said the runtime "is still starting or is not listening." It was
  listening, to a dialog.
- `respond` said a keypress "would be consumed by whatever it is doing
  instead." What it was doing was waiting for exactly that keypress.

Each message is correct for the failure it was written for and false for
this one, and a session in this state was unreachable by both text verbs at
once — `keys` (#49/#50) is the only surface that reaches it, and nothing in
any of the three responses said so.

The fix does not add a new status or a `PromptKind` — there is no
options-prompt here to classify, and #48 already warned that inventing one
is how a name gets added that nothing emits. What it adds is honesty about
what was actually established versus guessed, using a discriminator this
codebase already had rather than a new detector: `young`, the same signal
`starting` vs. `unknown` uses for the identical shape one branch up. A young
pane with no composer reads as starting, unhedged — that guess is earned. An
old one now names both real explanations instead of picking the one that
makes the pane sound broken, and both text-verb refusals point at `keys()`
once age stops making "still starting" the likely story.

> **A refusal that names a cause it only inferred teaches a caller the wrong
> lesson as confidently as one that names no cause at all.** `unknown`
> saying nothing would have been honest; `unknown` naming the wrong thing
> sent whoever read it looking at the pane instead of at the screen it was
> actually showing.

**F60 · A menu without numbers was a menu nobody could answer.** Every prompt
the detector had recognised since F37 was found by its numbered options. The
folder-trust question, on a session created in a directory outside every
trust root, now paints with none (`❯ No, exit` / `Yes, I trust this folder`
under `Enter to confirm`), and in the reverse order from the numbered form
this driver's fixtures held.

Measured through the service, not a bare multiplexer (#171): the state read
said `unknown` (the dialog's top rule, with no marker row above it, read as a
clipped composer), `respond` refused because nothing was being asked, and
`keys` refused on the clipped composer. All three verbs were closed at once,
so a caller that had consented to trust sat on the screen until a person
cleared it at the pane. Pressing keys into a scratch session answered the
second half of the question: a digit changed nothing (the screen digest was
identical), `Down` moved the highlight, `Space` changed nothing, and `C-m`
confirmed the highlighted row.

So an unnumbered menu is now recognised by layout (a runtime footer, one
block of two or more rows directly above it, exactly one highlighted, the
rest indented to its text column), and every piece is required, because each
alone is paintable by transcript. `respond` delivers the same 1-based choice
differently there: arrow presses, then a read proving the highlight arrived,
then `C-m`. If the highlight does not arrive, nothing is confirmed.

> **An answer's index is part of the contract; the keys that deliver it are
> not.** #168 had just measured that a digit commits on a numbered menu. The
> same digit on this menu is inert, so a delivery rule measured on one shape
> of menu is evidence about that shape only.

**F61 · A multi-select question could be ticked but never answered.** On a
question asking for several options, `respond` with a `choice` flipped one
checkbox and reported `submitted` — the flipped tick changed the option text,
so the nonce changed, and "the prompt changed" read as "the prompt cleared".
The control that finishes the question is not an option at all: a `Submit`
tab in the dialog's tab bar, reached by arrow key. The question stayed open
until a person pressed keys into the pane (#176).

Measured live, on a single-question and a two-question dialog: the boxes
paint as `[ ] Label` / `[✔] Label`, and the free-text row carries a box too; a
digit flips that box without moving the highlight or advancing; `C-m` flips
the highlighted box; `Right` moves to the next tab with the ticks kept, and
after the last question that tab is the same review screen #159
recognised, whose `1` hands the answers over. And one trap: `Right` is
swallowed while the highlight sits on the free-text row, the unnumbered
`Submit` row below it, or the chat row — the text field keeps focus — and so
is a digit, which is TYPED into that field instead of flipping a box. The
unit model missed the second half; the live end-to-end run caught it, and
the per-key read-back turned it into an `unknown` with nothing further sent.
So the driver first walks the highlight up onto a checkbox, one press at a
time, before any other key.

So a multi-select question is now reported (`multiSelect`), answered with a
set (`choices`) that names the end state rather than the moves, and advanced
one step — never confirmed. A single `choice` on a checkbox, and accepting
the highlighted row, are refused instead of reported as answers.

> **A changed nonce proves the screen changed, not that the question was
> answered.** `respond` read "different prompt" as "cleared", which is right
> when a digit commits and wrong when it toggles. The receipt was built on the
> first kind of menu and trusted on the second.

**F62 · A question drawn beside a preview pane was read as its labels plus the
box, and the usual keys recorded the wrong answer.** When a question's options
carry a preview, the runtime draws the list and a box side by side. Read whole,
every option row was its label, padding, and that row of the box — 64 to 73
characters each, on a measured run — the question was the tail of the agent's
own prose plus the tab bar plus the question, and the nonce changed each time the
highlight moved, because the box repaints with it. `respond` sent a digit and
got `unknown`: the answer did not land (#204).

Measured live on one runtime build, in a disposable session: a digit only MOVES
the highlight on this layout, and Enter commits the highlighted row. The old key
shape — the digit and Enter in one call — recorded the second question's DEFAULT,
not the digit, and reported `submitted`: the runtime kept one of the two keys.
Three `Down` in one call moved one row. One key per call worked every time. A
single-question dialog has a lone `☐ Header` chip where a multi-question one has
the tab bar, a long label wraps inside the list column, the box can be shorter
than the list (option 4 hangs below its bottom border), and a preview taller than
24 lines is clamped with a divider row — 27 rows of pane, which pushed the option
list out of the classifier's window and made a blocked session read as `idle`. A
question has at most four options.

So the classifier reads the list column alone (the pane is found by its closed
box beside an option, and cut at the gap in front of it, never at a column: a wide
character takes two screen columns and a capture emits it once), widens its window
to the pane's top, keeps the header out of the question and puts it, with the
current tab, into the nonce. `respond` moves the highlight, reads it back, and
only then confirms, one key per call.

> **A key's meaning belongs to the layout, and several keys in one call are not
> several keys.** #168 measured that a digit commits; the same digit here only
> moves. And "the prompt changed" read as "my answer landed" for the third time
> (#176, #168 before it): the receipt was correct only while every layout
> behaved like the one it had been built on.

**F63 · A card that pushes the composer off a short pane read as "idle" and
refused every send as a startup problem.** The runtime's feedback-draft card is a
notice of about seven rows above the composer. On the 24-row pane a created session
gets, the composer's closing rule and mode row fell below the pane, so the screen
ended on the composer's opening rule and its prompt row. `composerSpan` takes the
last rule on screen as the closing fence, walked up from the labelled opening rule,
met the card's bottom border before any prompt row, and ruled the composer
*absent*. Two readings then disagreed about the same screen. The state read went
through the finished-spinner branch, which never looks at the composer, and said
`idle` with the composer empty. The send path asked whether a composer was painted,
said no, and refused with a message about a runtime still starting or showing a
full-screen interface — which also pointed the caller at `keys`, where Escape on
an empty composer dismisses the draft. A supervisor's pings were refused eight
times, identically, and it backed off; the session sat with the card up for hours
and nothing on the API said why or who could clear it (#215).

Read out of the runtime's own binary: the card is non-modal. Its keys are read
through the composer's input value and act only while that value is empty, so on a
taller pane a message is delivered normally and the session really is idle. That
is why the card is a prompt only while it hides the composer, and why the refusals
under it now name it. Two hazards the same reading turned up are guarded: a message
that is only `1`, `2` or `0` would be read by the card as its shortcut (send
refuses it), and the card's body — the agent's own draft — sits exactly where the
usage-limit and failed-turn scans read the runtime's notices from, so those scans
now end above the card.

> **When one screen has two readers, they must not be able to disagree about
> whether a composer is there.** The state read and the send gate each had a
> private answer; a screen the classifier had not seen was the first place they
> differed. What is left of the disagreement — a composer whose closing rule is
> merely cut off the bottom of the pane still reads as absent for `keys` and
> `discard` — is recorded on #215 as a follow-up, not fixed here.

**F64 · The feedback card's other states were the same false idle, and one of them
sent a key that could send.** #215 recognised the card by its key row alone, and the
card is one box whose status text changes as it is answered. Captured on real 80-by-24
panes — with the send held or refused at a local proxy so nothing was submitted —
the send confirmation, the sending line and the send error, and the `/feedback`
panel `1` opens, were each answered wrongly — `idle` with the composer empty until
#216, `unknown` with evidence calling them a full-screen interface since — and
every send was refused as if the runtime were starting. The confirmation and the error wrap and are a
row taller than the key row, which pushes even the composer's `❯` row off the bottom;
the panel replaces the composer outright. The panel was the sharper case: with no
composer to find, `keys(Enter)` passed every guard, and on the panel's editor
Enter sends the draft with the conversation attached (#217).

Two premises did not survive contact with a screen. Escape on the card does not
dismiss the *draft* — only the card; the draft stays queued — so guarding it was
weaker than it looked. And the redactor that prepares captures for a public corpus
kept a tool call whole when its arguments said "for", because `statusLine` is a
shape and the tool call is drawn behind the same bullet (ADR 217; the redactor now
takes the bullet first).

> **A state you have only read the words of is a state you have not seen.** The
> binary holds a notice's wording, not its wrapping, the row a key hint lands on, or
> how many rows it takes. Get every state onto a screen before recognising any of
> them, and when reaching one has an external effect, find the seam that stops the
> effect and leaves the screen — and test the seam before pressing the key.

### The pattern worth naming

§5.7 — *absence and failure are different answers* — has now been discovered
independently at five different altitudes:

1. **A status field.** A driver that cannot determine state must say `unknown`,
   not guess (§2.3).
2. **A plural response.** A machine that did not answer contributes a
   `SourceStatus`, not an absence from `items` (§9).
3. **Inside a driver.** A pane that could not be read is not a pane that was
   read and found empty (F5).
4. **A capability declaration.** A peer that has not answered is not a peer
   that supports nothing (D3).
5. **A build identity.** A peer whose build is unstamped is not a peer whose
   build matches — and here the rule had to be enforced in a *predicate*
   rather than a field, because the tempting default was to let an unknown
   compare equal and stay quiet (F39).

Five instances, found separately, each initially looking like a local detail.
The generalisation is worth stating as a design rule for anything added later:

> **Every field in this API that can be absent needs to distinguish
> absent-because-no from absent-because-unknown — and if it cannot, it will
> eventually report someone's ignorance as a fact about the world.**
