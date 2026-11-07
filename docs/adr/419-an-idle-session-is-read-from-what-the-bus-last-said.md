# ADR: an idle opencode session is read from what was last read, not asked again on every listing

**Issues:** #419
**Status:** decided

## Context

Seven idle opencode sessions on one machine, every one reporting `idle`, cost
most of a core and several gigabytes between them. The first question the issue
asked was whose cost it was: the runtime's own, or this driver's traffic to it.

What was measured (one machine, one runtime release, a local stand-in for the
model so real turns ran):

| a server that... | CPU |
|---|---|
| has no session and no client | about 1 % |
| has this driver's event stream attached and a finished turn, text or tool calls | about 1.5–2.5 % |
| is also asked `GET /session/status` twice a second | 5–6 % |
| is also asked it ten times a second | 10–12 % |

So an idle server costs little, and each request per second costs about 2 % of
a core. The live servers were each being asked for their status about 1.5 times
a second, steadily, with no gaps: every listing the service makes (a dashboard
refresh, a supervisor's poll) is one `GET /session/status` per live server,
because in isolated mode every session has a server of its own. That accounts
for roughly two thirds of the 8–10 % the live servers showed; the rest was not
reproduced, which this record does not hide.

## Decision

A server's status map, the list of permission asks it holds, and — per idle
session — the confirmation State makes of it (the session's record, its newest
message's verdict, its usage figures) are **read once per window and shared**
(`internal/drivers/opencode/statuscache.go`):

- Callers that arrive while a read is in flight wait for it. N concurrent
  listings make one request per server.
- A read that failed is never kept, and every caller waiting on it is told it
  failed (§5.7 holds: a failed read is an error, never the last good answer).
- The window is **30 s while an event stream is attached to the server** and
  **2 s while none is**. With a stream, every change of status, every
  permission ask and every message record arrives as an event, and the event
  drops what was read; the 30 s is a ceiling for a stream that went quiet while
  staying open, a risk `subscribe.go` already accepts and which this cannot make
  worse than "the feed is stale with no limit". With none, nothing says anything
  changed, so the window is short.
- A write this driver makes (send, interrupt, answer, create, close) drops what
  was read **and keeps nothing for 2 s**, because the runtime accepts a prompt
  before it reports the session working and a read in that gap would otherwise be
  kept for the whole window.
- A read stores its result only if nothing dropped the cache while it ran.
- The read that answers a permission ask is not shared: an answer is made to the
  ask that exists now.

What this changes for a caller: a listing or a state read can show a change the
runtime made on its own up to the window late (2 s, or 30 s on a silent stream),
and never a change of this driver's making. The spec promises that state is
readable on demand and that changes are pushed (§5.5); it promises no freshness
per read, so no spec text changes.

## Not decided here

Stopping a server that has been idle for a while and starting it again on the next
input. It is the only thing that gives the memory back (an idle server holds
several hundred megabytes), and it is a lifecycle of its own: a stopped session
must read as idle and not dead or unknown, a send racing the stop must be safe,
every open subscription must re-attach. It is filed as its own issue.

## Consequences

- An idle server with a subscriber is asked for its status about twice a minute
  instead of 90 times.
- Tests that flip the fake runtime's status without any write or event construct
  their driver with the windows set to zero (`withStatusTTL(0, 0)`); a changed
  assertion in an existing test would have meant the observable behaviour moved.
