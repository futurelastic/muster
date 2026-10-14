# Adoption — turning a supervisor into a client

This document is about the item the README's roadmap once ended on, the one
that decided whether any of the rest was worth building (it is now done; the
plan below is how it was approached):

> **A supervisor as a client**, replacing direct terminal-multiplexer access.
> Until it happens, this is a second implementation of session management rather
> than a replacement for one.

It is written from an actual adoption, planned against a supervisor that has
been run in anger for months and has the bug tracker to prove it. Machine
names, ports and repository names are deliberately absent — this repository is
public, and every fact here survives being stated as a shape.

---

## 1. What adoption is, and what it is not

Adoption is **not** "replace the supervisor". A supervisor does at least two
jobs, and only one of them is ours:

| the supervisor's job | who owns it after adoption |
|---|---|
| deciding what work should happen — issues, claims, planning, sweeps | the supervisor, unchanged |
| version control state — branches, worktrees, merges | the supervisor, unchanged |
| knowing what a session is doing and being able to drive it | this service |

So the sentence to hold onto is narrower and much less alarming: **the
supervisor stops talking to the multiplexer and talks to a service instead.**
Everything else it does, it keeps.

This matters for scoping, because the surface being removed is large but very
uniform. In the case measured, roughly 1,800–2,000 lines of backend existed
solely to infer session state from a terminal screen — pane classifiers,
wrapped-line joining, stall detection, unsent-input detection, a liveness
probe's tail-block matcher — plus a comparable amount of frontend mirroring
those fields. None of it encodes domain judgement. All of it is a driver that
was never named as one.

## 2. The precondition nobody expects

*What the supervisor drives is not what this service owns.*

The service owns sessions. A supervisor drives sessions **in repositories**,
and repository state is explicitly outside this boundary (§1 non-goals). That
boundary is correct, and adoption is where its consequence arrives:

> **This service can dispatch an agent to another machine long before the
> surrounding system can safely let it edit anything there.**

The instance measured: a file-sync tool kept two machines' working trees
identical while their version-control histories stayed independent (syncing the
history directory had corrupted repositories before, so excluding it was
right). Within minutes of an edit on one machine, the other's checkout showed
the same file as locally modified — with no commit, no lock, and no way for
either side to know. Two agents editing one file across two machines is a
lost-update race that no amount of session-layer correctness prevents.

The same shape applies to work claims: if each machine's supervisor holds its
own claim database, both can claim the same work.

**Neither is a defect in this service, and neither can be fixed here.** They are
the price of the non-goals, and the adoption plan must answer them before any
cross-machine *write* is dispatched. The available answers are all outside this
repository — give each machine a checkout that is not file-synced and let
version control be the only transport; exclude the worked-on repositories from
the sync; or keep one machine per repository by discipline. Pick one
deliberately. The failure mode is silent.

**The same shape recurs on the read side, once dispatch is a real use of this
API rather than a demonstration of it: this service can hand you a session on
another machine long before it can hand you back what that session
produced.** Nothing here stores a session's result or returns its tool
output or screen (session-abstraction.md §5.8, muster #82) — again,
correctly, and again a consequence the adoption plan must answer rather than
a defect to file. One narrow read exists since #258: what the session's agent
itself wrote (`GET …/turns`, assistant turns only), for a coordinator that
needs to check rather than be told. For the rest the available answers are
the same shape as above and equally outside this repository: put a reply
address in the dispatch brief and have the worker
deliver its answer over `input`, for anything that fits in a prompt; build a
transport of your own — a synced directory, a small HTTP server on the
worker's machine — for anything that does not, knowing each carries the
failure modes `docs/client-guide.md`'s dispatch section records. Pick one
deliberately, per answer size. The failure mode is the same as above: silent.

### How it was answered here

Recorded because the shape of the answer generalises, even though the paths do
not.

**Only the machines that receive dispatched work move out of the synced tree.**
The race needs *two writers into the shared tree*, not two machines — so the
host a human works on directly keeps its existing paths, tooling and habits,
and the dispatch targets get their own checkouts cloned from the forge. The
synced copy stays where it is on those machines and simply has no writers. That
asymmetry turns a fleet-wide migration into a change on the hosts that were
going to be reconfigured anyway.

**Claims moved to the forge rather than to a shared database.** Each supervisor
had its own local claim store, and the tempting fix — one shared database — is
a distributed lock in disguise. The issue tracker is *already* shared state that
every machine talks to, so a claim became an assignment on the issue itself:
fleet-wide by construction, legible to a human, and no new source of truth. The
local database keeps caching it and stops being authority.

The general rule: **when two machines need to agree on something, prefer a
system they both already depend on over a new one built to make them agree.**

One trap worth repeating: the new root's name must not resemble an existing
synced root. On a case-insensitive filesystem, a path that differs by one
character from a synced directory will eventually be typed, and it puts a
working checkout back inside sync — silently, and precisely where the design
was supposed to have removed it.

## 3. Stage it so every step is reversible

Ordered so that the cheapest thing that could disprove the plan runs first.

**S — the smallest client first.** Adopt the thinnest caller you have; a
shell wrapper that starts a session on another machine over SSH is ideal. It
does exactly what this service does, has one human caller, and fails
obviously. Replacing it proves the API end to end at nearly no risk, and
retires whatever "is the far machine dispatchable" probe it was using — that
question is a health endpoint, not a login shell.

**C — confirm adoption, change nothing.** Whatever else creates sessions
locally may keep doing so. §12 reconciliation is what makes this safe: the
service adopts what it finds rather than owning only what it created. This
stage is a *test*, not a change, and it should be run rather than assumed.

**D1 — read path, shadow mode.** The supervisor keeps its own screen-scraping
and *additionally* reads the service, logging disagreements. No behaviour
change at all.

Expect disagreements, and treat the log as the most valuable artifact of the
whole migration. Two classifiers built from different evidence will differ, and
**both will be wrong somewhere** — every finding in Appendix A phases 2 and 5
was one of them being wrong. A disagreement log is a free, continuously-running
differential test on real sessions.

#### Log what you COMPARED, not only what disagreed

A log that records disagreements alone cannot distinguish *"the two agree"* from
*"the comparison never ran"*. Both are silence. That ambiguity is not
theoretical — it happened on the first real adoption: the shadow was written,
merged, and marked done with the switch off, and it collected nothing. The stage
reported complete because the *code* was complete.

So the shadow must also record, periodically, **how many sessions it compared**.
One line every few minutes is enough. With that line, silence has exactly one
meaning.

**D1 is done when you can state a number, not when the code merges.** Before
starting D2, answer: *how many sessions were compared, over how many hours, on
how many machines?* If that number is zero after a few hours of real traffic,
the comparison is not wired — it is **not** evidence that the classifiers agree.

Two traps worth knowing before you conclude "it is not wired":

- A flag can be set in a service definition and still not reach the process.
  Restarting a job is not the same as reloading its definition; on some
  supervisors an edit needs a full unload and reload, and a plain restart
  silently keeps the old environment. This cost an hour on the first adoption:
  a healthy supervisor, a correct flag, and a silent shadow.
- If two of your services run a file with the same name, identifying the
  process by name will read the wrong one's environment. Identify it by the
  port it is listening on.

*Why not have this service count the reads for you?* Because it would answer a
weaker question than it appears to. The service can see that something
authenticated and read a listing; it cannot see whether that read was fed into
a comparison. A non-zero counter would be compatible with a shadow that never
ran — assurance without evidence, which is worse than silence you know how to
read.

**D2 — read path, cutover.** State, unsent-input detection, liveness and the
*diagnosis* half of any repair ladder come from the service. Leave the old code
in the tree, unreferenced, for a release.

**D3 — write path.** Create, send, interrupt, close, respond. This is where §6
per-verb grants stop being theoretical: the supervisor becomes a principal with
`create`/`send`/`close` on its own machine, and whatever subset you are willing
to grant it on any other. Mutations on remote machines stay off until someone
decides otherwise — a machine's operator opting in per verb is the whole point
of the grant model.

**D4 — delete.** Remove the scraping. Keep the database, the issue board, the
claims, the planner.

**C2 — optional, last.** Have the local launcher create *through* the service
too, so spawn intent, idempotency (§10) and the audit trail cover
locally-started sessions as well. Only worth doing once the supervisor
migration has proven the API.

## 4. What must be true before the write cutover

These are not niceties. Each was found the hard way during the work that
produced Appendix A phases 4 and 5. **All four are now settled** — three by
changes in this repository, and the fourth by a decision outside it.

1. ~~**The repository/claim question of §2 must have an answer.**~~ ✅ Decided
   — see §2's "How it was answered here". The work it implies is entirely on
   the supervisor's side of the boundary; nothing in this service changes,
   which is the non-goals holding.
2. ~~**Bind loopback as well as any tunnel address.**~~ ✅ Done. Loopback is
   bound automatically on the configured port whenever the configured
   addresses do not already cover it. A service bound only to a tunnel becomes
   unreachable *from its own machine* when the tunnel half-fails, and the
   symptom is indistinguishable from a wedged process (F36).
3. ~~**Build identity must be visible in the health endpoint.**~~ ✅ Done.
   `GET /v1/health` reports a version-control stamp, peers learn each other's
   on the probe that learns their deadline, and a mismatch is logged with the
   reason it could not be verified. Unstamped and modified builds never
   compare equal — see F39 for why that asymmetry is the whole point.
4. ~~**A deployment path that is not hand-copying a binary.**~~ ✅ Done —
   `scripts/deploy.sh`, which refuses to build from a modified tree, and reads
   the running build back *after* restart to prove the deploy happened. That
   read-back is the part that matters: the skew in (3) was not caused by a
   failed copy but by a service that kept serving the old binary afterwards
   (F40). This is also the procedure `exposure: self` depends on; see
   [`docs/deploy.md`](deploy.md) for the write-up and the traps found running
   it.

## 5. What adoption does not fix

Stated so that nobody expects it to.

- **Anything above the session.** Sweeps, teardown policy, worktree records,
  ship intents, autopilot lifecycle. These get *better inputs* — a state they
  can trust, a prompt they can answer — and nothing more.
- **Deadline composition across a hop** (§14 D7), bounded today by §13.1's
  one-hop rule.
- **Per-subscriber authority on a shared stream** (§14 D9, residual). A
  subscription reads a peer under the service's authority, not the
  subscriber's.
- **Measurement.** There are no metrics. Subprocess spawn cost is known to
  degrade with host load — measured at 8× idle on a heavily loaded machine
  (F19) — and nothing reports it.

## 6. The general lesson

The service was ready for this well before the system around it was, and the
gap was not in the protocol, the transport, or the authorization model. It was
that **a session abstraction makes remote execution easy without making remote
execution safe** — safety lives in the layer that owns repositories, and that
layer is one this service is forbidden to know about.

Which is the boundary working exactly as specified, and also the reason
adoption has a precondition that no amount of work in this repository can
discharge.
