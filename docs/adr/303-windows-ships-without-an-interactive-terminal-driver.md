# ADR 303 — Windows ships without a native interactive-terminal driver

**Status:** decided by the implementing session on the plan for #303; reversible
(see *Consequences*).
**Issue:** #303 (the Windows port), recorded in #304.

## Context

Running muster on Windows is decided (#303). Most of what that takes is
mechanical: a Windows build, a platform layer, a service. One part is a design
question, and the issue says to settle it before building: the **interactive-
terminal driver** (the `claude-code-tmux` runtime) is built on the tmux binary,
which has no native Windows build.

Three candidates were on the table:

- **A ConPTY-based driver** that keeps the tmux driver's contract.
- **tmux inside WSL, driven from the Windows service.**
- **No interactive-terminal driver on Windows** at first, with
  `GET /v1/runtimes` saying so.

Done-when for #303 needs only the subprocess driver (the opencode runtime) on
Windows. So the question is not "what is the best terminal driver" but "what
may a Windows machine honestly claim, and what must be true before it claims
more".

### What a terminal driver has to keep

The tmux driver gives guarantees that clients rely on and that the spec and the
ADRs state. Any other terminal driver is judged against this table; a row it
cannot keep it must declare, not silently weaken (§5.6 *Degrade, never
emulate*).

| # | Guarantee | Where it is stated |
|---|---|---|
| G1 | Sessions outlive the service; a restart adopts them | spec §12, §7.5; ADR 182 |
| G2 | The composer is read from the visible pane with its height; clipped is not absent; capture is time-sliced inside the call's deadline | ADR 169, 134, 149, 216, 156 |
| G3 | The driver never clears or submits text it did not place (the draft rule) | spec §3; ADR 132, 133, 135, 138 |
| G4 | A delivery is confirmed against the runtime's own transcript; queued is not gone | ADR 187, 240 |
| G5 | A held prompt is bounded by the session's life, and "waiting" has a machine-readable class | ADR 125, 126 |
| G6 | A watching client re-attaches when its control client dies, and there is an event stream | ADR 172 |
| G7 | Raw keys, including back-tab, ride a grant of their own | ADR 188 |
| G8 | A live delivery lane is the only input path, and a lane that enqueues and never starts a turn is degraded | ADR 180, 257, 264 |
| G9 | Inbox delivery, with an attested class | ADR 119, 146, 148 |
| G10 | An attach hint a person can use | spec §2.8 |

### What each candidate costs in this codebase

**ConPTY.** A pseudoconsole dies with the process that owns it, so G1 needs a
separate long-lived host process: a new multiplexer. G2 and G3 need a terminal
screen model written in the standard library (the repository has no third-party
dependencies and does not take one for this). G6 needs an event protocol from
that host, G10 needs an attach client, and the classifier corpus has to be
re-measured against how the runtime renders under ConPTY. The tmux driver is
large, with direct tmux command sites throughout and no multiplexer interface to
slot a second implementation behind. This is several xl pieces of work, and
none of them is required for #303's Done-when.

**tmux in WSL, driven from the Windows service.** Every multiplexer call would
spawn a WSL process, which works against the capture budget of ADR 156. The
runtime's record roots, process identity and working directories all live on the
Linux side. The inbox's unix sockets (G9) do not cross the WSL2 virtual-machine
boundary. Doing it properly is a remote transport inside the driver — and
muster already has one: federation.

**None.** Honest under §5.6 and under #297's rule that an absent runtime means
"not enabled", not "broken". It adds no new API vocabulary.

## Decision

1. **Native Windows has no interactive-terminal driver in this release.**
   `FLEET_RUNTIME` defaults to `none` on Windows; `tmux` there is a startup
   refusal that names this ADR; `muster compat` refuses.
2. **`GET /v1/runtimes` on Windows lists only drivers registered there.** The
   terminal runtime is **absent**, not `unavailable`: no cause is invented for a
   runtime that cannot be enabled. The one local row is the subprocess runtime
   — `available`, or `unavailable` with a cause when it is enabled but not
   usable — and it carries no `sandbox` entry, because the sandbox is not
   enforceable there.
3. **Terminal sessions on a Windows machine are served by the Linux build
   inside WSL**, under its own machine id, peered with the Windows service
   through the existing static peer configuration (spec §7.2, §13). The terminal
   driver runs where tmux runs and keeps G1–G10; the Windows service relays. This
   is the same federation every pair of machines already uses; it needs no new
   code, only documentation (`docs/install.md`, in the Windows install work).
4. **A native terminal driver stays open**, as a **new runtime id** (not a
   change to `claude-code-tmux`). Before it is registered it must provide G1–G10
   or declare each one it lacks through the driver capabilities (§4.3), so a
   client can tell. If demand for native terminal sessions on Windows is
   measured, this ADR is reopened, not worked around.

## Consequences

- Clients that key capability tables by (machine, runtime) need nothing new:
  the Windows machine simply has no terminal-runtime row of its own, and a
  configured WSL peer shows its row under **the peer's** machine id.
- Verbs that need a terminal — `keys`, the composer reads, attach — are
  `unsupported` for a Windows machine's *own* sessions, as the subprocess
  runtime's capabilities already say. Nothing about them changes on Unix.
- Adding a native terminal driver later is additive: a new runtime id, no wire
  change, no migration.
- Running two muster processes per machine (the Windows one and the one inside
  WSL) is an operator cost. It is stated in the install documentation rather
  than hidden; a user who wants no terminal sessions on Windows runs one.
- A Windows machine without WSL has no terminal sessions at all, and says so
  through an absent row. That is the honest first release; it is not a fault
  `muster doctor` reports.
- The decision constrains the work after it: the runtime/service child sets the
  Windows default and the refusal, and the platform-layer child does not try to
  make the terminal driver compile-and-run.
