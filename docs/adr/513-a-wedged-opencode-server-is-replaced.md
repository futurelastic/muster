# 513 — a wedged opencode server is replaced, a bounded number of times

## Context

With the opencode driver in isolated mode a session is a process of its own, and
that process keeps one database connection for as long as it lives. Under a load
spike a runtime's connection to its own store can fail with a storage I/O error and
never recover: several sessions on one machine did within seconds of each other, and
from then on every read of the session and every prompt answered `500` for hours.
The files under the connection were healthy (a consistency check passed, the same
inodes were still open, there was free space), and a new process started on the same
directory worked at once. The driver reported each session red and left it, so the
work stopped until a person killed and re-created every one by hand.

## Decision

The driver notices, stops that server, starts another on the same directory and
reattaches the session — at most three times within an hour per session — and says so.

- **What counts.** Every answer a server gives passes through one function of the
  client, so a run of failures is counted there, per server, whoever asked: a state
  read, a send, a turns read. Only a path that reads or writes the session's **store**
  is evidence. A `5xx` on one adds to the run; a healthy answer from one ends it. An
  answer from any other path (the status map, the permission and question lists, the
  provider catalog: all in memory or in configuration) neither adds nor ends. That
  distinction matters both ways. The measured fault failed every read of the session
  and every prompt while those endpoints went on answering, so a poller reading the
  status map between two failed reads would otherwise reset the run for ever. And a
  runtime whose permission list alone fails has a working store; replacing it would
  trade a failure it can live with for a restart it cannot.
- **When it decides.** A run of three that has lasted twenty seconds on the driver's
  clock. Three reads failing together under one load blip are not a wedge. If the
  runtime's own log has named a storage I/O failure since the store last answered, two
  failures are enough and the span is not waited for. The log is read only for whether
  such a line exists, never for what it says. One failed answer decides nothing.
- **How it replaces.** In its own goroutine, under the session's lifecycle lock, never
  inside the request that tripped it. The server carries a note saying why before its
  process is stopped, and the phase classifier reads it as a fourth phase, *wedged*, so
  no reader reports the session dead for a stop this driver made and no event reports it
  closed. The record names the restart before it happens, so the budget survives a
  service restart.
- **A turn in flight.** The request that met the `500` already returned a retryable
  error and is never sent again: the runtime may have stored it, and a second delivery
  would be a duplicate. A write during the replacement waits for the lock when an idle
  bound is set and is refused, retryable and nothing sent, when it is not. A turn the
  runtime had not finished is reported interrupted, as after a service restart, and
  says the runtime was restarted. Nothing is queued by the driver: what the runtime
  accepted is in its store, which the new process reads.
- **A caller-supplied environment.** The driver never keeps those values, so it cannot
  start such a session again. Its server is stopped and it is listed `unknown` with the
  cause and what to do; its caller revives it by handing the environment over again
  (muster #427), which now also accepts this cause. No budget is spent: nothing was
  restarted. It is not counted in the figure the restart reports, which is about the
  start that built the driver.
- **The budget.** A session that wedges again after three restarts in an hour is
  stopped and listed `unknown` with the cause and the budget, rather than restarted for
  ever on a fault the new process may share. A restart of the service tries it once
  more. A restart that fails is listed the same way, with the failure, and counted by
  `muster doctor` as any failed relaunch is.
- **What it says.** One line in the driver's log per replacement or give-up, with the
  session id, the cause and how much of the budget is used; a `session.state` event;
  the same text in the listing; the record's `wedge` field. The cause reads
  `runtime 500: sqlite disk I/O error` when the log named it, and otherwise states the
  failures that were counted.

## What this does not do

- **Restart on the moved-directory cause** (ADR 473, #474). That is named already and a
  new process would fail the same way.
- **Act without a state directory.** A driver with none keeps no record to start a
  session from, so it behaves as it did: the reads keep failing and say so.
- **Add a service-level audit line.** The service's audit lines are written for requests
  and carry an actor; a replacement the driver decides has none. The driver's own log
  line, the event, the listing and the record carry it instead. A line with a system
  actor would be a new, non-request audit path in the service, which was left out of
  this change.
- **Know the right numbers.** Three failures, twenty seconds and three restarts an hour
  are a first measurement from one incident, kept as constants in one place.

## Consequences

- A session on a server whose store connection broke comes back by itself within the
  span, with its conversation, on a new process, and a subscriber sees a state event
  and no close.
- A session that cannot be brought back says why in its listing instead of reading as a
  bare failed read, and a caller can close it or, with its environment, revive it.
- A server that fails only some store paths is replaced too; one whose store keeps
  answering is not.
