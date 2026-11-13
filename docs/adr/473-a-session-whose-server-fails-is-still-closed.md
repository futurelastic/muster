# 473 — a session whose server fails is still closed

## Context

With the opencode driver in isolated mode a session is a process of its own: one
server per session, started by this service, whose scoped directory this service
removes. A close first read `GET /session/{id}` from that server, to corroborate
what it was about to remove, and refused on any failure of that read but "no such
session" and "nothing answered".

A server can be up and answer `500` to every request, for instance because the
directory its sandbox was given was moved from under it. Such a session could not
be closed at all: with `?runtime=` the close answered `400 invalid` carrying the
server's text, and without it the service could not tell which runtime held the id
(its existence probe is the same read, which also failed) and answered that
existence could not be confirmed. The session whose holder most needs to remove it
was the one it could not.

## Decision

- **A close in isolated mode goes ahead on the driver's own record when the
  runtime's read fails** — any failure of the read except a real "no such session"
  (still `404`). The server process is stopped, its directory removed, the record
  dropped. The caller's expectation (`startedAt`) is still checked, against the
  start time the driver recorded. Shared mode keeps its refusal: its one server is
  every session's, so a failed read of it says nothing about one session's fate.
- **The answer says it went ahead without the runtime.** `Ack` gains an optional
  `note`, prose for a person, never parsed, absent on an ordinary close. It is not a
  status: it states what the driver could not do, not a fact about the session. A
  peer relays it unchanged and tolerates a peer that sends none.
- **A bare id is resolved to the driver whose own record holds it**, even when
  that driver's read of the session fails. A driver may implement the optional
  `driver.RecordHolder` (`Holds(id)`, memory only, no round trip); the existence
  probe then counts a driver that holds the id as the holder rather than as
  "could not be asked". A driver that does not hold the id, or cannot say, stays
  inconclusive as before.

## What this does not do

- **Change what a listing says when one isolated server's read fails.** That
  session is `unknown` with the error, and the runtime's source is `degraded`, so
  `complete` is `false`, as §5.7 and §9 of the session-abstraction spec define it
  and both drivers implement it. Whether `complete` should stop covering a
  per-session state read is a change to the normative contract and was not made
  here; a client that needs the machine's identity reads it from `/v1/machines` and
  `/v1/health`, not from the completeness of a listing (ADR 472).
- **Delete a session on the runtime's say-so alone.** The service closes only a
  session it holds the record of; an id nobody holds is still `404`.

## Consequences

- A session on a failed server can be removed through the API, with or without
  `?runtime=`, and the answer tells the caller the runtime did not confirm it.
- Operations other than a close that are routed by bare id to a driver that holds
  a session it cannot read now reach that driver and fail with its own error,
  instead of the generic "could not be confirmed" refusal.
