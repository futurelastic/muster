# ADR: a record that grew is said with a cursor and never content

**Issue:** #444
**Status:** decided — within the rulings of [`441-a-sessions-full-record-is-read-under-its-own-grant.md`](441-a-sessions-full-record-is-read-under-its-own-grant.md): its four constraints stand, and the event adds none of its own and relaxes none of theirs; no new grant, route, audit event or switch
**Extends:** ADR 441 (a way to follow the record without polling it) and the `readsRecord` declaration of ADR 441 (a format beside it)

## Context

ADR 441 gave a session's record a route and a cursor, and ADR 442 and 443 filled it for
both runtimes. A client that wants to show a live session has one way to follow it: call
the route again and again from its last cursor. That is polling, it costs a read per
session per interval whether anything happened or not, and it is the one thing the event
feed exists to spare a client.

The feed already says when a session's state changed, its labels changed or a machine's
account did. What it cannot do is say a record grew, and it must not say it by carrying
the record: the feed is read under `read`, the record under `record`, and ADR 441's whole
point is that nothing implies the second.

Five questions followed:

1. Where is growth noticed: in each driver's stream, or in the service?
2. What does "coalesced" mean, and who owns the window?
3. What is in the event, and what is its cursor?
4. Who is told, given that the record route can be switched off?
5. The issue asks for the capability "on the session object where it differs". When does
   it differ?

## Decision

### Growth is noticed in each driver's own stream

A driver already owns its own change trigger, and what it adds is a comparison, not a
trigger.

- **The terminal runtime** compares, on the pass its stream already makes when the
  multiplexer's notifications wake it, a mark of each watched session's record: the
  conversation's id, the size of its file, and the sizes of its sub-task files. One stat
  and one directory listing per watched session per pass; no timer, no wake-up of its own,
  so an idle fleet still costs nothing (§5.5).
- **`opencode`** reads the runtime's own bus, which pushes the change. Five events mean the
  record moved (`message.updated`, `message.removed`, `message.part.updated`,
  `message.part.removed`, `message.part.delta`); each carries the session under
  `properties.sessionID`, which was read off a real server's OpenAPI document. They are
  hints: nothing about a message is decoded for them.

The service stays what it is for every other kind. It stamps a cursor and an epoch,
applies the subscriber's `session` and `cwdPrefix`, and relays a peer's frame. It does not
read a record, and it does not poll one: a service that called `Record` on a timer would
read content in the layer that must never hold it, and would charge an idle machine for
the question.

### The window is the driver's, and the promise is "at most one per session per window"

A client gets no number. The terminal runtime's window is its pass (the coalescing delay
of the stream, then the duty-paced gap that bounds what a listing may cost); a burst of
appends between two passes is one event. `opencode`'s is a per-session window of 250 ms
opened by the first hint and closed by one event, so a model streaming tokens is one event
per window: never one per token, and never starved by a debounce that restarts on each
hint.

The terminal runtime's cursor carries a clock (the watermark of its cursor), so two of
them cannot be compared and it emits on a changed mark. `opencode`'s cursor is a position
in the message list with no clock, so it can be, and an event whose cursor equals the last
one that subscription sent for the session is dropped: a token streaming into a part that
has not settled changes nothing a client can read, and a client told to pull would pull an
empty page. The first window of a session in a subscription has nothing to compare with and
is sent.

An event is a hint, not a promise that something visible was added. A record can grow by a
line the reader leaves out, and the pull is then an empty page, which ADR 441 already makes
a normal answer.

### The event is a reference and a cursor, and its cursor is the route's own

`{ref, startedAt?, cursor}`, and nothing else: a test holds the keys, on the Go type, on the
marshalled payload and on the frame the real SSE handler writes. `startedAt` is there for
the reason `session.labels` carries it, a recycled id.

The cursor is what `Record(since="")` would answer as `next` at that moment, minted by the
driver's own reader (`readRecordAt` for the terminal runtime, a `recordHead` factored out of
`Record` for `opencode`). So "pass it as `since` and read what is recorded after this event"
holds by construction and cannot drift from the route: one code path decides what a cursor
means. The cost is one bounded tail read of the record per growing session per window; the
page is read in the driver and discarded, and only `next` leaves it. If that is ever
measured as heavy, detection and minting can be split without changing what a cursor means.

A client reads what the event announced from the cursor of **its own** last read. The
event's cursor is where the record had reached; it is opaque and not comparable with the one
the client holds.

A replaced conversation (a `/clear`) is an event with a cursor of the new one. A client's old
cursor is a conflict against it, and the event is how it learns to start again without
`since`.

### Who is told

The event travels under `read`, like every event. The cursor it carries is an identifier of
a place in a record (a conversation id and offsets, or a message id), the same kind of thing
`Session.conversation` already shows a `read` holder; none of it is content. While the
operator's `refuseRecordReads` switch is on, the service publishes none of its own
`session.record` events: the route answers `501 record-off`, and a client told to pull it
would be misled rather than informed. The frame is dropped before it takes a cursor, so the
sequence has no hole. A frame relayed from a peer is that peer's statement about its own
route and passes unchanged.

### `recordFormat`, and no per-session value

`DriverCapabilities.recordFormat` is a number, declared by a runtime that reads a record, and
present exactly when `readsRecord` is true. It is the page's own `RecordFormat`, so the
capability and the route cannot disagree. Absent means not declared, and a peer built before
the field decodes to absent with no code of its own, as `readsRecord` already does; a number a
newer peer declares passes through the relay unchanged.

The capability is per **runtime**, and the session object carries none. The issue says "where
it differs", and nothing does: both drivers read a record for every session they hold, and a
session whose record is not identified yet, is parked or has been replaced already says so on
the route. Spec §4.3 states the rule (a per-session declaration arrives the day something
genuinely differs) and F52 states the cost of getting it wrong (a closed set nothing produces
is in the contract all the same, and clients are entitled to it). It arrives with the first
session whose runtime process is not the one its runtime row describes, as an optional
`readsRecord?` on the session with the same tri-state, **absent meaning "as its runtime
says"**, which makes it additive.

## Alternatives considered

- **The service polls `Record` and diffs.** Rejected. It reads content in the wrong layer,
  needs the record grant it must not hold, and charges an idle machine for the question.
- **A timer in the terminal runtime to catch a record that grows behind a screen that never
  repaints.** Rejected. It is a poll, and the stream is deliberately edge-triggered. The
  limit is stated instead: such growth is reported on the next wake.
- **A `kinds` selector on the feed**, so a client that does not follow records is not sent
  them. Declined here, recorded as the cost below. It is a change to every client of the feed
  and its own issue.
- **Gating the event by the `record` grant.** Rejected for now. The payload is a reference and
  a cursor, and the hub would need a per-subscriber grant test it does not have. If a driver's
  cursor were ever found to embed anything derived from content, this decision would need a
  ruling instead.
- **Compare cursors for the terminal runtime too.** Not possible: its cursor holds a clock.
  Comparing the underlying marks is what it does.
- **Putting the content, or an entry count, in the event.** Rejected without cost-benefit:
  it is the thing the event is for not doing.
- **A per-session `readsRecord` now.** Declined above.

## Consequences

- A client follows a live record with one request per announced window instead of a request
  per interval. A session nobody is following costs the terminal runtime a stat and a listing
  per pass, and `opencode` a timer per growing session.
- A subscriber that names no session now receives `session.record` for every session that
  grows on the machine, and each event takes a slot in the feed's retention beside the state
  events a slow client needs to resume. Coalescing bounds it to one per session per window; a
  selector that lets a client opt out is the follow-up this ADR names.
- The terminal runtime reports growth on the next wake, not on a timer.
- `docs/api.md`, `docs/spec/api-http.md` §4 and `docs/spec/session-abstraction.md` (§2.16,
  §4.3) carry the contract; `internal/drivers/tmux/recordevent.go` and
  `internal/drivers/opencode/recordevent.go` carry the reasoning at the code.
