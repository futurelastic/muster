# ADR: an idle opencode server can be parked, and is started again by the next input

**Issues:** #421 (the remainder of #419); #423 (what a service restart does to a parked session)
**Status:** decided; off by default for the release that introduces it

## Context

#419 measured an idle opencode server and cut its CPU, and said what it left: an
idle server keeps several hundred megabytes resident (about 365 MB for a small
session, over a gigabyte reported on long-lived ones), and a fleet of idle
sessions holds gigabytes for as long as the sessions live. Nothing about reading
a session more cheaply gives that back; only stopping the process does.

The driver already had the two halves. A server stopped with the service keeps its
scoped directory and its record (`pauseForShutdown`), and `relaunch` starts a new
process on that directory and confirms the runtime still has the session. What it
did not have was a way to tell a server that is gone because somebody stopped it
for the session's sake from one that is gone because the session is dead.

## Decision

With `FLEET_OPENCODE_IDLE_STOP` set (a Go duration of at least `5m`; unset, `0` and
`off` mean never), a session's server is **parked** when it has had nothing to do
for that long: its process group is stopped the way the service stopping stops it,
and the session keeps its record and its directory. The next `input`, `interrupt`
or `respond` starts a server on the same directory again (`relaunch`) and does what
was asked. It applies only with a state directory, because a session is started
again from its record.

**One classifier.** `server.park` says a stop was on purpose, and `phaseOf`
(`internal/drivers/opencode/park.go`) is the only place that turns a server into
`none | running | parked | exited`. Every reader that used to read "the process has
exited" as "the session is dead" asks it instead: a state read, a listing,
Subscribe, the end of an event stream, the process-gone announcement, `turns`.
Parked is read first, because a parked server's process has exited too.

**What a parked session reports.** `idle`, observed, with evidence that begins
`parked:` and says it will start again on the next input; never `dead` or
`unknown`, and never a `session.closed` or a process-gone event, because the
session has not closed. It is answered from memory: **a read never starts a
process** (a state read, a listing, a subscription, the service's holder probe), or
a dashboard polling would undo the park. It still counts everywhere an idle session
counts, including toward any limit on sessions, because it owns a record, a
directory and a slot a send can fill at any time.

**Who is parked.** Only an isolated session with a record that can be started again
as it was created: one created with caller-supplied environment is never parked,
because its environment was never kept. It is parked when, for the bound, this
driver wrote nothing to it and its event stream said nothing about it (a heartbeat
does not count, or an open stream would keep every server awake for ever), and a
fresh read — not the #419 cache, which can be 30 s old — shows no turn in progress,
no permission ask and no question. A read that fails is not an answer: the session
stays up. A check that finds the session busy marks the server watched, and a
watched server is checked on every pass until it is first seen idle; that moment is
when its quiet time starts, so a turn nobody was subscribed to is still waited out
for a whole bound.

**Locking.** With the bound set, every write to a session and `close` run under one
lock per session, and the sweep takes it with `TryLock`. A send racing a park is
safe whichever wins: if the park holds the lock the send waits, finds the session
parked and starts it; if the send holds it, the sweep skips the session, which the
send has just made active. The lock is not taken at all with the bound unset, so a
driver without the knob behaves exactly as before. Lock order: the session lock,
then `d.mu`, `d.subMu`, and the leaf locks below them.

**Start and subscriptions.** The start puts the new server in the place of the
stopped one under `d.mu`, then connects every stream that follows the session to it
before anything is sent, so the first change of the turn is on the stream. A stream
sees nothing at the park and nothing from the start itself; what it sees afterwards
is the new process's own events. If a stream cannot attach, it ends with a gap,
which the service already answers by reading again from a fresh baseline.

**If the start fails** the session stays parked: it reads `idle`, its evidence says
the start failed and why, and the write that needed it fails with a retryable
`unreachable` error and nothing sent. The next write tries again. Becoming
`unknown` was rejected: an unknown session is only tried again at a service
restart, while every reason a start fails here is passing (a port taken, a slow
runtime) or fixable without one (an agents file, a sessionEnv file).

**`close` of a parked session** takes the path of a session found on disk and never
started: stop what it left, remove the directory and the record, publish one
`session.closed`. No process is started to be closed.

**`turns` of a parked session is a conflict (409)**, naming the park. The history is
in the runtime's store, which only its process reads, and starting one for a read is
the thing a polling client would keep doing. The cost is that 409 on `turns` already
meant "the cursor is not valid, read without `since`", and a client that does that
reads 409 again until it sends the session input.

**Default off.** Memory is what it saves, but the first input after a park pays a
process start (seconds), which is a change for callers. It is opt-in for one
release; a later release changes the default to 30 minutes once a park and start has
been seen working on a deployment.

## What it costs, and what it does not do

- Parking stops everything the session started, background servers a tool launched
  included, exactly as the service stopping does. The runtime keeps its own children
  in the group, so a "no other group members" guard would not work.
- The session's `tmp/` is kept, and still measured, so a parked session can still
  sit on gigabytes of it. #419 reports the size in `state.scratch`; removing it is
  not this change.
- A service restart leaves a parked session parked while the bound that parked it
  still holds (see "A restart leaves a parked session parked", below). Every other
  session is started again at a restart, as before, and holds its memory for one
  bound before the sweep considers it.
- A session left parked by a restart reports no `usage` until its first input: the
  figures are read from the runtime and kept in memory, and there is no process to
  read them from. Its sandbox and permission posture and its last failed turn are
  still reported; its conversation reads unconfirmed until that input (below).
- A runtime that publishes a change event while idle would keep stamping the
  activity clock and never be parked. If a deployment shows that, the list of events
  that count narrows.
- Shared mode (a server the driver did not start) is never parked.

## A restart leaves a parked session parked (#423)

Without this, every restart of the service — a deploy — started every record on
disk, parked ones included, because a record did not say it had been parked. After a
deploy every idle session held its memory again for one bound, on exactly the
machines that had turned the bound on to give it back.

**The record says so.** A park writes an optional `parked` object to the session's
record (when, how long it had been quiet, the bound in force, and how the newest turn
had ended) **before** the process is stopped. The write that records the process
started in its place clears it, so the fact is never present beside a running
process. The field is optional and the record's format version is unchanged: a record
written before it reads as not parked, and a build that does not know it ignores it
and starts the session, as every build before did. If the write fails the session is
still parked — freeing the memory is the point — and the log says a restart will
start it.

**What a restart does with it.** The stopped server is rebuilt from the record
without starting anything: the scoped directory, the sandbox and permission posture a
state read reports (the sandbox plan is made again from the record, which is a pure
computation when the record names the private cache copy it already made), and the
park note. The session is listed `idle`, observed, with `parked:` evidence that also
says the service has restarted since; the first `input`, `interrupt` or `respond`
starts it through the same path any parked session takes.

**When it does not.** The session is started the ordinary way, and so can never be
left stranded, when:

- **no bound is set.** Nothing would park the session again, and a parked session is
  only made with the bound set — the per-session lock that serialises a start
  against a park is only taken then. This is the case the issue called out.
- **the bound is lower than the one the park was decided under** (the question the
  issue left open: it starts). The fact belongs to a policy that has since been
  replaced; the new bound decides afresh from live evidence, one bound of memory
  later at most. The same or a higher bound keeps the session parked: it is a
  session the new bound would have parked too.
- **a start would fail before any process exists:** the posture can no longer be
  enforced, the directory is gone, a required environment file or the operator's
  agents file cannot be read, the sandbox cannot be planned. These are refused at a
  restart as they always were — listed unknown, with the reason — rather than left
  parked behind a first input that cannot start the session.

**What is not checked.** That the runtime still holds the session in its store: only
a process can say, and none is started. The start the first input causes reads the
session back by id, as every start does, and a failure there is the retryable one a
parked session already has (the session stays parked, with the failure in its
evidence).

**Conversation.** A restart confirms a session's conversation id by reading it back
from the relaunched runtime. A session left parked has no runtime to ask, so its
`conversation` reads unconfirmed — never the remembered id served on its own — until
the input that starts it, whose read-back confirms it.

**Considered and not done.** Writing the usage figures into the record: they are a
per-message memo, large and changing with every turn, and the field is optional and
cheap to add later. Starting each parked session briefly at a restart to read its
usage and last turn: that is the memory spike this change exists to avoid, multiplied
by every idle session at once.

