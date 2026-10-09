# Internals — measurements, decisions and gaps

Engineering material that used to live in the README. It is kept in full,
because most of it is a measurement somebody made once and would otherwise
have to make again.

Read this if you are working **on** the service. If you are working **with**
it, you want [`api.md`](api.md) for the endpoint reference or
[`client-guide.md`](client-guide.md) for a walkthrough.

The normative documents are elsewhere and this file never overrides them:
[`spec/session-abstraction.md`](spec/session-abstraction.md) is the domain
model, [`spec/api-http.md`](spec/api-http.md) is the wire protocol.

---

## Layout

```
.                          wire and domain types only — importable by clients
internal/driver            the Driver interface and capability declaration
internal/drivers/stub      a driver that answers unsupported everywhere
internal/drivers/tmux      the first working driver — multiplexer + agent CLI
internal/drivers/remote    the second — an HTTP client to a peer (federation)
internal/drivers/opencode  the second LOCAL driver — a spawned subprocess,
                           the first able to declare observesState: true
internal/service           registry, one-hop fan-out, HTTP routing
cmd/muster           the binary
```

The root package deliberately holds nothing but types, so a third party writing
a client never has to import a driver.


## Which agent-CLI versions this is tested against

**There is no hand-written span here.** The supported range is the set of runtime
builds that have a passing compatibility report — the output of
`muster compat` ([`compat.md`](compat.md)) — and the report's
`claude.version` and `claude.sha256` are the record of which build that was. A
version written into prose goes stale the day a release ships: this section used
to say `2.1.220` through `2.1.223` and was about sixty patch releases behind the
builds actually running when this paragraph replaced it.

The report answers one question — *does this candidate build still behave the way
this driver assumes?* — before a fleet takes it. It does not answer a second one,
*which builds are running right now?*, and that second question is the one below.

A single "tested against X" line would be true and misleading anyway, because
**a session keeps the binary it was started with**. Long-lived sessions therefore
outlive upgrades, and one machine drives several versions at once. Measured on
two machines whose *installed* CLI was identical, at the time this section was
first written (builds since superseded, so read it for its shape, not its
numbers):

| | versions running concurrently |
|---|---|
| one machine | one build ×48 · the release before it ×21 |
| the other | four consecutive builds at once: ×18 · ×10 · ×4 · ×2 |

Four patch releases live at once on one box, the oldest three releases behind
what is installed. So **the installed version tells you very little about what
this driver is talking to**, and upgrading the CLI does not migrate the
sessions already running. A passing report certifies a build; it does not say
that no other build is still in use.

### Why this matters more here than it usually would

The driver does not call an API. It reads a **terminal UI**: a composer marker,
menu footers, dim (SGR 2) placeholder styling, spinner glyphs, a
running-versus-finished suffix. Every one of those is a rendering detail a patch
release may change, and a changed glyph does not raise an error — it silently
reclassifies. Detection is therefore structural wherever it can be, and no
single footer string is relied on alone.

### The status footer is LIVE STATE, not configuration and not version

Worth stating explicitly, because the natural assumption is wrong in a way that
sends people to the wrong fix.

The standing footer's tail varies between machines and between sessions on the
same machine. It is **not** explained by CLI version — the differing forms
appear under the same version — and it is **not** a settings difference. It is
composed from counts of things running right now. Three shapes observed on one
machine, at one instant, under one set of versions:

```
auto mode on (shift+tab to cycle) · ⇥ 3 agents
auto mode on · 1 monitor · ⇥ 3 agents
auto mode on · 2 monitors · ⇥ 3 agents
```

The trailing count is the machine-wide number of running **background agents**
(confirmed against the CLI's own `agents` listing: 3 background agents renders
`3 agents`), and a **monitors** segment appears alongside it — displacing the
generic hint when present.

Two consequences, and the second is the one people get wrong:

- It changes whenever a background agent or monitor starts or finishes, so it
  **can never be a sole anchor** for classification. Anything keyed on the
  footer tail is keyed on a number that moves under it.
- It **cannot be normalised away by aligning configuration**. There is no
  setting to match, because it is not a setting. Two machines with byte-identical
  configuration and the same CLI version will still render different tails
  whenever they are doing different amounts of work — which is most of the time.

### Running the checks against a live fleet

The scripts under `scripts/` drive a real service and, indirectly, the agent
CLI. One environmental fact will bite anything scripted:

**`claude` is not resolvable from a non-interactive shell.** It is installed
under a user-local `bin` that is added to `PATH` by the *interactive* startup
file, so a non-interactive login shell — which is what `ssh host '…'`, a cron
job, or a process manager gives you — does not see it. Measured on both machines
here; a plain `ssh` session gets `PATH=/usr/bin:/bin:/usr/sbin:/sbin` and the
command is simply not found.

The symptom is unhelpful: a session that starts and dies, leaving a dead pane
and no error anywhere a caller can read.

So anything scripted must either use an absolute path to the CLI or invoke it
through a **login and interactive** shell (`-lic`, not `-lc`). This is the same
distinction the driver itself has to make when it wraps a created session — see
`internal/drivers/tmux/environment.go`, which documents the measurement.

## Secret scanning, and fixtures that look like secrets

This repository is public, so a value is exposed the moment a branch is pushed.
Two things scan for secrets, and neither can take a push back:

| Guard | When it runs | What it reads |
|---|---|---|
| `.githooks/pre-commit` (`gitleaks protect --staged`) | before the commit exists | what is staged. Per **clone**: it is off until `.githooks/install.sh` has run in that clone |
| CI `Secret scan (gitleaks)` | a push to trunk, and pull requests | a trunk push reads trunk's own history; a pull request reads only the commits it adds over trunk |

The hook is the only guard that runs before a value is published, **where it is
on**. CI detects.

Nothing turns it on for you, and it cannot report that it is off. What switches
it on is `core.hooksPath`, which lives in one clone's own `.git/config`; a clone
that never ran the installer commits with no scan and no output (#201, measured:
a commit quoting a key-shaped value went through in silence in such a clone,
where the scanner run by hand refused it). So this is the guard a clone is most
likely to be missing without knowing. `muster doctor`, run from inside a
clone, has a row for it, `hooks.pre-commit`: `warn` when git would run no
pre-commit hook here, or a different one, or one it cannot run (no executable
bit, or no `gitleaks` on `PATH`, where the hook prints a notice and lets the
commit through); `skip` anywhere that is not a clone of this repository. It
answers when asked. It cannot warn at the moment of a commit, because the only
thing git runs at that moment is the hook that is missing. Why a row and not an
installer run on the operator's behalf: [ADR 201](adr/201-precommit-guard-visibility.md).

**A branch push is not scanned by CI** (#199, ruled). A scan after the push finds
nothing sooner than the hook and trunk's own run do, and it cannot un-publish
anything. Adding one also costs something either way. The ship step reads a
branch's CI as a class at its head sha, and with no run at all the class is
`none`, which is what is true here. A scan-only run would read `green` with no
test behind it; running the whole workflow would make every ship wait on the
`-race` suite, where a flake can block it. The scan step already copes with a
branch push (`origin/<trunk>..HEAD`), so reopening this is a trigger edit plus a
choice about the `go` job's `if`.

### Test fixtures that look like credentials

**Make the value not key-shaped.** Measured on the pinned gitleaks 8.30.1: a
16-character hex string assigned to a key-looking name such as `laneKey` is a
`generic-api-key` finding; readable placeholders under such names
(`"laneKey": "lane-one"`, `"token": "not-a-real-token"`) are not. A fixture is
made up, so make it look made up. The scanner reads prose as well as code, so
describe a key-shaped value here rather than quoting one — the first draft of
this very section failed the scan on its own example.

**An ignore entry repairs history; it is never the answer for a new fixture.**
`.gitleaksignore` takes one line per fingerprint, `<commit>:<file>:<rule>:<line>`,
and `gitleaks detect -v` prints the fingerprint of each finding. Add an entry
only for a fingerprint already in published history — a commit that is already
pushed and that a scan's scope still reaches. As in #185 (`e935936`): change the
value in the tree, list the earlier commit's fingerprints and nothing else, and
say why in the comment above them. Three measured properties make an entry
narrower than it looks:

- It names a commit, so it can only be written after that commit exists. It
  cannot prevent a finding. For a value that reaches trunk it is a follow-up
  commit, and trunk's scan stays red until that follow-up lands.
- Ship squashes, so a branch's intermediate commits never reach trunk: fixing the
  value on the branch is enough for trunk's scan. An entry earns its place only
  in a scope that still reaches the old commit — a pull request's range, or a
  local scan of every ref.
- Keep the commit in the line. An entry without it (`<file>:<rule>:<line>`) is
  accepted, but it exempts that line in every commit: a later, different
  key-shaped value on the same line stayed hidden, where the commit-bound entry
  did not hide it.

**Do not use an inline `gitleaks:allow` marker.** It works, and that is the
problem: a line carrying it is not reported, the marker travels with the code and
covers whatever the line later becomes, and it normalises bypassing the scanner
in source. An ignore entry is one reviewed line per fingerprint.

The hook's own block message offers `git commit --no-verify` for a certain false
positive. A key-shaped fixture is not one: the commit would land, and trunk's
scan reads it.

## Decided — pointers, not copies

Settled questions, with the reasoning where it lives. Reopen them on new
evidence, not on taste.

| Decision | Reasoning |
|---|---|
| Addressing is `(machine, id)`; no fleet-wide id | spec §7.1 |
| Peers are statically configured; no discovery | spec §7.2 |
| Fan-out is one hop deep; peers never recurse | spec §13.1 |
| A proxy relays a peer's source status, never re-synthesizes it | spec §13.2 |
| Every driver declares a mandatory deadline | spec §4.4 |
| Plural responses are envelopes, never bare arrays | spec §9 |
| Restart reconciles and adopts; it never destroys | spec §12 |
| Proxy topology, not redirect | spec §13 |
| A remote peer is just another driver | proven — spec §4.2 |
| Go, and zero dependencies | below |
| Delivery goes through a module seam; the draft rule; terminal path v2 | [ADR 180](adr/180-delivery-module-and-terminal-path-v2.md) |
| `BTab` (Shift+Tab) is a `keys` key under the `keys` grant, so `keys` can escalate a session | [ADR 188](adr/188-btab-rides-the-keys-grant.md) |
| A session's permission mode is published in `state`, read off the runtime's own indicator row; absent means nothing read, `unknown` means read and unnamed, never a guess | [ADR 194](adr/194-permission-mode-in-state.md) |
| The pre-commit guard being off is reported by a `doctor` row, from the clone; nothing installs it on the operator's behalf | [ADR 201](adr/201-precommit-guard-visibility.md) |
| An optional external delivery module is a child process speaking JSON lines; off by default; a lane is chosen at create; a send is never delivered twice | [ADR 185](adr/185-optional-external-delivery-modules.md) |
| CI does not scan a branch push; a key-shaped fixture is fixed by changing its value, and an ignore entry only repairs history already published | [Secret scanning](#secret-scanning-and-fixtures-that-look-like-secrets) (#199) |

**Go, zero dependencies.** Chosen at zero lines of code, on the reasoning that
language cost is lowest at the start and compounds afterwards. A static binary
removes an entire failure class — nothing to install on the target, no runtime
PATH to get right, no version skew at run time. The standard library covers
routing, HTTP, JSON and concurrency, so the dependency count should stay at
zero; adding one is a decision to argue for, not a convenience.

## Known gaps

Stated plainly so nobody rediscovers them the expensive way.

- **Adoption has a precondition this repository cannot discharge.** The service
  can dispatch an agent to another machine long before the surrounding system
  can safely let it edit anything there: repository state is a non-goal (§1),
  so nothing here prevents two machines editing one working tree, or two
  supervisors claiming one piece of work. Measured, not theorised. See
  [`docs/adoption.md`](docs/adoption.md) §2 — it is the one thing that must be
  answered before a supervisor's *write* path is cut over.
- **There are no metrics.** Subprocess spawn cost is known to degrade with host
  load — 8× idle on a machine at load average 63 (F19) — and nothing measures
  it in production.
- **Deadline composition across a hop is unspecified.** Bounded in practice by
  §13.1's one-hop rule, so it is a correctness gap rather than a live hazard.
  See spec §14 D7.
- **Capability declaration cannot say "I don't know yet."** `Capabilities()` is
  synchronous and infallible — fine for a driver describing itself, impossible
  for one describing a peer across a network. An unreached peer is
  indistinguishable from a peer that genuinely supports nothing. Both degrade
  safely, so the cost is diagnostic rather than correctness: a misconfigured
  peer looks like a minimal one forever. This is §5.7 a third time; see spec
  §14 D3.
- **A subscription's authority is the service's, not the subscriber's.** A
  multiplexed stream serves many callers at once and outlives any of them, so
  there is no single "original caller" whose credential it could present — the
  assumption §13's rule was written on. The service subscribes to peers as
  itself, over a read path only, which bounds the widening to reads but does
  not remove it: a subscriber sees a peer under the service's authority rather
  than its own. See spec §14 D9.
- **Auth has no rotation or expiry.** Credentials are per principal with
  per-verb grants and an audited outcome, and enrolment is now a command
  (`muster principal add`) that mints a token and validates grants before
  writing. What is still missing is the rest of a lifecycle: nothing expires, a
  compromised token is revoked by editing a file. Changing grants is a command
  (`muster principal grant|revoke`, #274), but it edits the file and does not
  reload a running service.
- **`SourceState` has no member for "reachable but unsupported"** — currently
  squeezed into `degraded`.
- **Enumeration cost is the real scaling risk, not the network** — and the fix
  is structural rather than incremental. Re-measured on a host running 22 live
  sessions:

  | approach | spawns | wall clock |
  |---|---|---|
  | per-session capture loop (what the incumbent does) | N+1 (23) | 119 ms |
  | one batched invocation | 1 | 18 ms |

  The multiplexer accepts a command sequence in a single invocation, so a full
  fleet view costs a constant number of spawns regardless of session count —
  ~5 ms per session becomes ~0.15 ms per session, and the curve stops being a
  curve. `List` returns everything in one call for this reason.

  **That "regardless of session count" needed a correction (muster#141).**
  The multiplexer's own client-to-server command channel refuses a chained
  invocation outright once it gets big enough — measured by bisection against
  a real server, ~995 args survives and ~1007 fails — and that refusal is
  total: the whole invocation comes back with no output at all, so every
  session in it misclassifies as a driver malfunction, not just the ones past
  the wall. A machine carrying 85 sessions (~1020 args) hit this; the number
  above (22 sessions) does not, which is why the constant-spawns property
  measured cleanly at that size and stayed wrong for any fleet crossing the
  wall. The capture step now chunks (`captureChunkMaxArgs`/
  `captureChunkMaxBytes` in `internal/drivers/tmux/tmux.go`), so a fleet under
  either cap still costs the one spawn this table describes, and a fleet
  above it costs one spawn per chunk rather than losing the whole batch.

