# ADR: the opencode record is read by part, and a running call is sent once and updated by id

**Issue:** #443
**Status:** decided — within the rulings of [`441-a-sessions-full-record-is-read-under-its-own-grant.md`](441-a-sessions-full-record-is-read-under-its-own-grant.md) and [`442-the-record-carries-tool-calls-sub-tasks-summaries-and-images-by-reference.md`](442-the-record-carries-tool-calls-sub-tasks-summaries-and-images-by-reference.md); no new grant, route, audit event or switch
**Extends:** ADR 442's "what the terminal runtime emits now", for the `opencode` runtime

## Context

ADR 441 gave a session's record a route, a grant, an audit event, an operator switch and
a message shape, and ADR 442 filled the kinds for the terminal runtime and added the
media route. The `opencode` runtime answered `501` for both. This change reads its
record into the same shape, under the same rules.

The two runtimes keep a record differently, and three questions followed from it:

1. The terminal runtime appends a line to a file when something is done, and a line is
   never rewritten. `opencode` keeps a **message** with **parts**, and a part is
   rewritten in place while it runs: a text part grows while it streams, and a tool part
   goes from `pending` to `running` to `completed` or `error` inside the same message.
   What is an entry, then, and when is it final?
2. A model round is one assistant message in `opencode`, and a round that calls a tool
   does not finish until the tool does. A build that runs for ten minutes keeps its
   message in flight for ten minutes. `turns` holds an in-flight message back whole. Does
   the record?
3. The runtime has no "after": it pages a message list from the tail with `limit` and
   backwards with `before`. What is a cursor?

## Decision

### An entry is a part

A part is the runtime's unit, and the only one that is stable while a message is being
written. So the record has **one entry per part, in the order the runtime wrote them**:
a text part is a `text`, a reasoning part a `reasoning`, a tool part a `tool`, an image
an `image`. This is not what `turns` does, which joins an assistant message's text parts
into one turn because a turn is the unit ADR 258 names. A joined entry would change
under a client that already holds it. The two routes agree on **which text** is the
agent's: the same predicate (a message with no error and a model; a part the runtime
flags `synthetic` or `ignored`, or that is blank, is out). They differ in how it is
grouped, and a round that wrote one text part, which is nearly every round, reads alike
on both. Two model rounds in one prompt read as two assistant entries.

### What the allow-list admits

| Kind | Role | Source |
|---|---|---|
| `text` | `user` | a text part of a user message, not `synthetic`, not `ignored`, not blank |
| `text` | `assistant` | a text part of an error-free message from a model, same flags |
| `reasoning` | `assistant` | a reasoning part with text (an empty or encrypted one is out) |
| `tool` | `assistant` | a tool part: its id (`callID`), `tool`, `state.input`, and the state |
| `summary` | `assistant` | the text of a message flagged `summary` (a compaction); nothing else of that message |
| `image` | `user` | a file part of a user message that is an admissible inline image |
| `image` | `assistant` | an attachment of a **completed** tool that is an admissible inline image |

A tool's `status` is the runtime's own: `running`, `completed`, or `error` (the
runtime's error text is the `result`). A `pending` call is one whose input is still
arriving and is not a call yet. `at` is the runtime's time for the part: the end of a
text or reasoning part, else its start, else the message's; for a call, when it began,
which is the same in every state. A tool's metadata, title and compaction mark are
never carried, and neither is any part this reader does not name: step, snapshot, patch,
agent, retry, compaction and subtask parts, or a part that does not decode. A part that
does not decode costs that part, not its neighbours.

**What may be referenced as an image** is as in ADR 442: an inline `data:` URL of one of
the four named types, the type the file says it is, starting with that type's signature,
at most 16 MiB decoded. A link, a path, an SVG, a mismatched or short payload is not
referenced; the text beside it is still sent. An image on an assistant message itself is
not a shape the runtime writes for the agent, so it is not read.

**No sub-tasks.** A delegated sub-task is a session of its own in this runtime, so no
entry carries `parent`. What the sub-task did is read from that session.

### A message in flight returns its settled parts, and a running call is sent once and updated by id

Holding a message back whole, as `turns` does, would show a viewer of a live session
nothing from the moment a long tool starts, which is when it most wants to see the
conversation. So the record returns the **settled parts** of the message in flight and
stops at the first one that is not:

- a text or reasoning part is settled once the runtime has written its end, or has moved
  on to a part after it, or has finished the message;
- a tool part is settled once it is `running` or finished (not `pending`).

A call that is `running` goes out once, `status: "running"`, with the call's own `at`.
When a later read finds it finished, the page carries the **same message again,
complete: the same `tool.id`, the same `at`**, in front of what is new. The rule for a
client is ADR 442's, unchanged: a `tool` message whose `tool.id` you hold replaces it.
The images a completed call returned follow it, once.

The cursor keeps the ids of the calls that went out running, and **never leaves a
message while one of them is pending**, so the set is the running calls of one model
round: it is bounded by what the runtime runs at once, and a cursor refuses more than 64.
This differs from ADR 442, which resolves a late result against the file because a line
there is never rewritten; here the call is the same part and its id is in the message.

**A remnant.** An assistant message with no completion time is in flight only while it
is the last assistant message. When a LATER assistant message exists the runtime cannot
still be writing it (it is sequential within a session), so it is a remnant of a cut-off
turn, as `turns` already reads it. Its calls that have no outcome go out `error` with no
result, and replace a `running` one the reader holds, so that nothing shows as running
for ever. A message that ends in an error after a call of it went out running is the
same: that call goes out `error`, with no result, and nothing else of the message is
read, since an errored message is left out as in `turns`. A trailing unfinished message
stays held, which fails closed and needs no status read.

### The cursor is a position in a message

`o1`: a JSON object, base64url, binding the session id and its start time (so another
conversation, or a recycled id, is `ErrAmbiguousTarget` and not a stale read) and holding
the message the position is in, how many of its parts are consumed (`-1`: the message is
done), how many entries of the next part were sent (a completed call that returned
images is one part and several entries, and a page may end between them, so `limit`
bounds the page exactly) and the calls still owed. A read finds the message again by
widening a tail window from 32 messages, as `turns` does, and a record that does not
hold it even when read whole is the caller's stale belief. Without `since` the most
recent `limit` entries are returned, as in `turns`.

It is not a cursor of `turns` (`1|…`) and neither route accepts the other's. Whatever is
not a record cursor, or whose position the record cannot hold (a part beyond the
message's parts), is refused: `invalid` for the first, `ErrAmbiguousTarget` for the
second.

### The media read is one message

A reference is `om1`: the session id, its start, the message id, the part and, for a
tool's attachment, its index. It holds no content and no path. A read resolves the
session as a record read does (a parked one is the same `409`), requires the reference's
conversation to be the session's now, reads **that one message** through the runtime's
`GET /session/{id}/message/{messageID}`, classifies it with the same allow-list and
requires that list to emit this very reference, decodes the image and checks the bytes
are the type claimed. Every way that fails is `ErrNoSuchMedia`, as in ADR 442; a failure
to reach the runtime is not one of them, it is the error it is.

### A parked session

A session whose runtime process was parked to free memory (ADR 421) answers a record
read and a media read with the conflict `turns` gives, which says it is parked. A read
does not start it.

## Alternatives considered

**One text entry per message, joined, as `turns`.** Rejected: it changes while the
message is in flight, which breaks both the cursor and a client that holds it. It also
puts text before and after a tool in one entry, in a place the tool is not.

**Hold the in-flight message back whole.** Rejected, above: a viewer sees nothing for
the length of a long tool.

**Re-send a running call on every poll, with a cursor that carries nothing.** Rejected:
a client that does not replace by id would show the call once for every poll, where the
rule promises twice at worst. Keeping the id in the cursor sends each state once.

**Resolve the late result by reading the message before the cursor, as ADR 442.**
Unneeded: the call is a part of the message the cursor is already in.

**Carry the messages' parts in the cursor.** Rejected: the cursor names a position and
the record is read again; it is a few dozen bytes plus the ids of calls in flight.

**Emit an errored message's completed calls.** Rejected: an errored message is out for
`turns`, and the record's predicate is the same so the routes cannot disagree about
what the agent said. A call that already went out running is closed, which is a
correction of what the reader was sent, not new content.

**Read the whole record for a media reference.** Rejected: the runtime has a route for
one message, and the reference names it.

## Consequences

- **A client applies one rule to `tool`: replace by id.** Pages are in the order the
  walk emitted them, and `at` is not monotonic within a page: an update carries the
  call's time and sits in front of what is new.
- **Holding `record` means holding tool output and images** on this runtime too, which
  ADR 441 said it would. Nothing here widens who can read.
- **Limits invisible to a client and recorded here:** a message edited in place after it
  was read is read from the position the cursor holds, so an inserted part before it is
  not seen; an image on a message the runtime has since reverted is gone with it; a
  message of up to the runtime's own size is held in memory per read, base64 images
  included, but the payload is neither copied while a page is built nor sent in it; a
  cursor carries at most 64 calls, and a message with more calls at once than that waits
  for them.
- **The record is the runtime's, not this service's.** Its part shapes can change in a
  release. The reader fails closed, so drift costs a missing entry, not a leaked
  category. The shapes read here were checked against the runtime's own OpenAPI document
  (version 1.18).
