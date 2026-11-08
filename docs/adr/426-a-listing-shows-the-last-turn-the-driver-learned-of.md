# ADR: a listing shows the last turn the driver learned of, and reads a turn's end once

**Issue:** #426 (a follow-up to #363, which gave the single-session read `lastTurn`)
**Status:** decided

## Context

A session whose newest turn ended on a provider error leaves the runtime's status
map and reads idle. The failure is written down in one place, the session's own
message record, and asking for it costs a request per session. The single-session
read pays that (#363). A listing could not, and left `lastTurn` out. Measured on one
machine: a provider ran out of credit, and for about thirteen hours most of its
opencode sessions sat on a failed turn while every listing showed them idle. What
polled the listing kept starting sessions that failed on their first request, and
input sent to the existing ones was accepted and failed again.

The first idea was to keep the failure the event stream reports. That only helps
while something is subscribed: the service opens the driver's stream when a
subscriber connects and closes it a minute after the last one leaves, and a driver's
subscription tracks failures for itself and for the sessions its filter admits. The
case measured was a poller that lists and does not watch.

## Decision

**One record per session, on the driver's entry for it, refreshed lazily.**

- It holds the failure of the newest finished turn (or none), whether a turn may have
  run since that was read, whether the runtime has been seen working since, and a
  generation that advances when a turn starts. No new lock: it is written under the
  lock the entry already has.
- It is opened by a turn this driver starts (`Send`, `Create`) and by a status read
  that finds the session working. It is closed by a read of the runtime's own reply
  to the turn, or by the event stream or the park path, which already know the turn
  ended.
- **A listing reads the record.** For a session the record says may have finished a
  turn, found idle, it makes one message read (the request `State` makes), in the
  same goroutine as the status read of that server, and closes the record. An idle
  session nothing has happened to costs nothing, and a session that has finished a
  turn costs one read per turn, whether or not anything is subscribed.
- **The row is the failure kept, else the interruption a restart left (#282).** A
  parked session still reads from the note taken when it parked (#421).
- **The failure of the previous turn is not cleared at `Send`.** It goes when the
  session is seen working or the next read replaces it. Clearing at the `Send` would
  show a healthy idle row for the moment between the runtime accepting a prompt and
  reporting the session working, which is the reading this removes.
- A read that raced a newer turn is dropped (generation), a read that failed is never
  kept, and a session whose newest message is still the user's is read again at most
  every two seconds and given up on after thirty.

## Rejected

- **A message read per idle session on every listing.** It is what #363 left out, and
  #419 measured what a request per server per listing costs.
- **A driver-owned event connection to every server.** It would change what an idle
  server costs (#419) and what stops one (#421), to learn what a read at the turn's end
  learns.
- **A failure store fed only by subscriptions.** Nothing feeds it for a listing nobody
  streams to, which is the measured case.
- **A short-lived watcher per turn the driver starts.** Every way it can fail (a runtime
  with no event bus, a connection that does not open, a turn end lost to a gap, a bus
  that ends when the server is parked, a create whose prompt failed after it started)
  needs the read above as its fallback; with the fallback it adds a connection per running
  turn and a goroutine to stop, and only makes an unwatched row fresher by the time the
  read takes.

## Costs and limits

- A listing can gain one round trip on the call that follows a turn's end, for each
  session that ended one.
- **After a service restart the record is empty and a listing omits the failure** until
  the session runs a turn or is read by id. The read by id stays the authority; a
  listing without `lastTurn` on an idle session has not shown that its last turn
  succeeded.
- If the runtime has not yet recorded a turn's first message when a listing reads, the
  previous turn's reply is taken for the new turn's; the next sighting of the session
  working reopens the record.
- No `/v1/health` or `/v1/runtimes` aggregate. One outage already reads as the same
  `lastTurn.reason` on every session it hit; a count is a new public field and its own
  decision.
