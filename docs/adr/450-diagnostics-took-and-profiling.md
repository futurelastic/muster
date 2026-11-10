# ADR: a slow create is explained by its own log line, and a running release build can be profiled under a grant of its own

**Issue:** #450
**Status:** decided

## Context

Creates were slow on a loaded host (p90 9.2 s, max 23.3 s over thirty minutes)
and nothing the service wrote said where the time went. The create's audit line
recorded no duration, so the slow tail was estimated from the caller's side; one
caller's own split put 27.4 s of a 33 s create in the terminal. And a busy
service could not be sampled: the release binary has no profiling endpoint, and
the figure available from outside, process-table CPU, is a decaying average on
macOS that blamed this service for load that was elsewhere.

## Decision

1. **Every logged mutation ends with `took=<ms>`, and a create adds
   `phase.<name>=<ms>` for the steps measured.** The clock starts where the verb is
   admitted and the phases are noted through a recorder on the request context
   (`driver.NotePhase`), so a driver notes a step without importing the service,
   and a context with no recorder makes the note a no-op. A create names `resolve`,
   `create` (the driver's whole create; for a relayed one, the round trip), `state`
   and, from the multiplexer driver, `terminal`. `terminal` is *inside* `create`;
   the two overlap on purpose, so `create - terminal` is everything else the driver
   did. The fields are appended after `status=`, so a reader that stops there still
   reads the line. Every verb gets `took=`, not only create: a line that carried it
   for one verb would read as though the others had been measured and found instant.
   Phases are printed only where measured, never as a zero.

2. **The profiling endpoint is behind two independent locks: a new grant, `debug`,
   and a config switch, `profilingEndpoint`, off by default.** Neither implies the
   other. The grant says who may read the process's internals; the switch says whether
   this machine offers them at all, so an operator can grant ahead of time and turn
   the surface on only while a sample is needed. The grant is checked first, so a
   caller without it learns nothing about the machine's configuration; a holder on a
   machine that has not switched it on gets a `501` naming the switch.

3. **`debug` is its own grant and is implied by nothing.** Not by `read`: a supervisor
   that may watch sessions has not thereby been trusted to look inside the service that
   serves them. Not by the legacy flags: a machine with no principal table refuses every
   request. Not by `muster service install`'s set, which excludes it. And not by a
   signed machine list, which is refused whole if it names it: whether this machine can
   be profiled, and by whom, is a local decision, like the pinned signer.

4. **No symbol files are published.** The question was whether a captured sample could
   be resolved without rebuilding. A Go profile is symbolized inside the process from
   the runtime's own tables, which the release build's `-s -w` keeps (it drops the
   linker's symbol table and DWARF, not the runtime's function tables). Measured on a
   release-flag build: a 30 s CPU profile taken from the running binary names its
   functions with nothing shipped beside it. The release build and workflow are
   unchanged.

5. **The handlers are written over `runtime/pprof` and `runtime/trace`, not taken from
   `net/http/pprof`.** Importing that package registers unauthenticated routes on
   `http.DefaultServeMux` as a side effect of its init. This service never serves the
   default mux, but a surface that is safe only because nothing else in the process ever
   will is the shape this repository avoids; a test holds the line.

6. **Bounded and visible.** `profile` and `trace` take `seconds` from 1 to 120. The
   runtime itself is the lock: starting the CPU profiler or the tracer when one is
   running fails, and that is answered as a retryable `409` in the error envelope, not
   the standard handler's plain-text 500. A caller that disconnects ends the sampling.
   `cmdline` (argv) and `symbol` are not offered. Every request, served or refused, is
   in the audit trail as `verb=debug`.

## Consequences

- An operator reads a slow create off the log first, and takes a profile only when the
  phases do not explain it. The two are meant to be used in that order.
- The switch is read at start. A running service cannot be flipped on or off by editing
  the file: such an edit is refused whole, naming `profilingEndpoint`, like the other
  start-time settings.
- `debug` is not carried across a machine list the way `push-list` is for a listed
  machine of the same key: an operator who wants a *listed* machine to profile another
  grants it in that machine's own config, not in a list.
- `go tool pprof <url>` cannot send a bearer header, so the documented flow is fetch the
  file with `curl`, then open it.
- The opencode runtime reports no `terminal` phase: its slow create, where there is one,
  shows as `create` alone. Extending it is a one-line `driver.NotePhase` where wanted.
