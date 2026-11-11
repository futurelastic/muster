# ADR: the record carries tool calls, reasoning, sub-tasks, summaries and images by reference, and a call is updated by id

**Issue:** #442
**Status:** decided — within the ruling of [`441-a-sessions-full-record-is-read-under-its-own-grant.md`](441-a-sessions-full-record-is-read-under-its-own-grant.md); no new grant, route family or audit event
**Extends:** ADR 441's "what each runtime emits today", for the terminal runtime

## Context

ADR 441 gave a session's record a route, a grant (`record`), an audit event, an
operator switch and a message shape, and shipped the terminal runtime's `text`
entries in both directions. The other kinds were declared and left empty until a
reader could fill them. This change fills them for the terminal runtime: tool
calls with their results, reasoning, the entries of delegated sub-tasks,
compaction summaries and images.

Three questions had no answer in the shape and had to be settled before a reader
could be written:

1. A tool call and its result are two lines of the runtime's file, and the result
   can come minutes later. A client that follows the record by cursor has moved
   past the call by then. How does it get the result?
2. An image is a block of base64 inside a line. The message never carries it, so
   what is the reference, how long does it last, and what does "expired" mean?
3. The terminal runtime does not keep a sub-task's entries in the conversation's
   file. Where do they come from, and how does one know which call started a
   sub-task?

## Decision

### A call is sent when it is recorded, and sent again, complete, when its result is

A `tool` message is emitted where the call was recorded, with the call's own `at`.
It carries the result and a `status` of `completed` or `error` when the same page
reads the result as well (a result is attached only to a call with exactly its id,
in the same file, and only the first result for it). Otherwise it goes out with
`status: "running"` and no result. When a later page reads the result, the page
carries that **same message again, complete: the same `tool.id`, the call's own
`at`, in the position of the result line.** The rule for a client is one sentence:
**a `tool` message whose `tool.id` you already hold replaces it.** A client that
does not implement it shows the call twice, and nothing is lost.

The cursor carries nothing about pending calls. A result whose call is not in the
page's scan is resolved by reading the file before it: one search per source per
request, widening back from the result like the tail read (256 KiB, then four times
that, to the 32 MiB ceiling), requiring a call with exactly that id, recorded
before the result, with no other result for the id between them. If any of that
fails the result is **omitted**, never attached to some other call. A call older
than the ceiling whose result arrives later stays `running`: a loss, never a leak.

A page that stops between a call and its result sends the call running, and the
result's line is read again as the first line of the next page, so the update goes
out exactly once. A result that completes a call the same page holds takes no room
on the page: `limit` bounds what the page carries.

### An image is a reference, and the reference is a place in the runtime's file

An `image` message carries `media`, an opaque reference: where in which file the
image block sits (the line's offset and the block's path in it), bound to the
conversation it was read from. It holds no content and no path. It is read through
`GET /v1/machines/{machine}/sessions/{id}/record/media/{ref}`, which returns the
image's bytes with their `Content-Type`. The route is the record route's sibling and
nothing else: the same `record` grant, the same `refuseRecordReads` switch (`501`,
`record-off`), the same `startedAt` corroboration, the same relay, and the same
`audit: content-read` line, whose route is the route pattern and so never carries
the reference.

**Lifetime.** A reference is valid for as long as the runtime's file holds the
entry. The service stores nothing, so nothing expires on a clock. It stops working
when the conversation is replaced (a `/clear` or a relaunch points the session at
another file, and the reference names the first), or the file goes. **Expired** means
exactly that, and so does every other way a reference can fail: it is malformed, was
never issued, names another conversation, an offset that is not the start of a line,
a block that is not an image, or an image that is no longer admissible. They are one
answer — `404 not_found`, **not** retryable (`ErrNoSuchMedia`) — because telling a
caller which references exist would tell it about a record it has not read.

**Never another session's image.** A read resolves the session the caller named,
with `startedAt` corroborated before anything is opened; requires the reference's
conversation to be that session's conversation now; finds the file by the same rules
a sub-task file is found by; reads that one line; classifies it with **the same
allow-list that issued the reference** and requires the block at the path to be an
image that list admits; decodes it; and checks the bytes are the type the line
claims. A reference handed to a different session names a conversation that session
does not hold.

**What may be referenced.** An inline base64 image of type `image/png`,
`image/jpeg`, `image/gif` or `image/webp`, starting with that type's signature and no
larger than 16 MiB decoded (`MaxRecordMediaBytes`). Anything else (a link, a file id,
an SVG, a type this API does not name) is not referenced; the text beside it is
still sent. The service and the relay check the answer again before serving it: a
peer's response that is not such an image is never passed on, and the bytes go out
with `Cache-Control: no-store`, `X-Content-Type-Options: nosniff` and a
`Content-Security-Policy` that allows nothing.

**The line limit.** An image is stored on the line that carries it, so the record
route now reads lines up to 16 MiB (`recordEntryLineLimit`), where the other readers
keep 1 MiB. At 1 MiB every image line, and the tool result on it, would be skipped.
The page scan and the media read share the one limit, so a page never hands out a
reference the media read cannot serve. A line over it is skipped whole, as before.
The base64 itself is never copied while a page is built: the scan keeps its length
and its first characters, which is what the type check needs.

### Sub-tasks come from the files beside the conversation, and `parent` from their sidecar

The terminal runtime writes each delegated sub-task's entries to a file of their own
in a directory beside the conversation file, and a small sidecar next to each file
naming the id of the call that started it. The record reads those files as further
sources of the same conversation, and every entry from one carries `parent`: the id
from the sidecar, on text, reasoning, tool and image entries alike (a sub-task started
from inside a sub-task names a call in the other sub-task's file). The sub-task's
first message, the prompt it was given, is a `text` entry with role `user` and the
same `parent`; its role is relative to the sub-task, whose "user" is the agent that
delegated.

A file is a source only when everything about it is certain: its name is the
runtime's (`agent-<id>.jsonl`, the id plain characters), it is a regular file and not
a link, the sidecar parses and names a call with a plain id, and each line it yields
says it is a sub-task line of that id (and, when it names a conversation, this one).
A file that fails any of that is not read. A sub-task line in the **conversation's**
file (an older runtime wrote them there) cannot be tied to a call and stays out.

### The cursor, version 2

The cursor is now a position in each source: for the conversation file and for each
sub-task file that still needs one. A position is a byte offset and the number of
that line's entries already sent, so a page may end inside a line (one line can yield
several entries, an image-bearing message or a tool result with images). The cursor
also carries a watermark, two minutes before the moment it was issued, which is what
lets it name only the sub-tasks that need naming:

- a sub-task the cursor names resumes where it says;
- a sub-task it does not name and whose file was last written before the watermark is
  done, and is not read;
- one it does not name and that began (by its first line, which never changes) after
  the watermark is new, and is read from its start;
- one it does not name that began before the watermark but has been written to since
  (a sub-task resumed after it finished) is followed from its end as it is now: what
  it wrote in between is lost, never repeated.

A cursor names at most 64 sub-tasks, the most recently written first. A cursor of
version 1 (a position in the conversation file only) is still accepted: sub-tasks are
followed from now. A `turns` cursor is still refused here. Without `since`, the most
recent `limit` entries are returned across the conversation and its sub-tasks, merged
by time; there is still no backward paging.

A page is the next `limit` entries across the sources in the order of their times
(ties: the conversation first, then the sub-task's id), so a sub-task's entries sit
among the conversation's where they happened.

### What the terminal runtime emits now

| Kind | Role | Source in the runtime's file |
|---|---|---|
| `text` | `assistant` | the agent's text blocks of a line, joined — the `turns` predicate, unchanged |
| `text` | `user` | what a person typed; now also when the message carries images |
| `reasoning` | `assistant` | a reasoning block with text (a redacted or empty one is out) |
| `tool` | `assistant` | a tool call, any tool; its result paired by id |
| `summary` | `assistant` | a compaction summary (the runtime stores it as a user line; its author is the model) |
| `image` | `user` / `assistant` | an image the person attached / one a tool returned (after the tool message) |

A tool's `input` is the raw JSON the runtime recorded. Over 64 KiB it cannot be cut
as JSON, so it becomes a JSON string holding the leading part of the input's text,
and the message says `truncated`. A `result` is the tool's text, cut at 64 KiB the
same way as any text; a result's non-text parts other than images are not carried.
`status` is `running`, `completed` or `error`: `error` when the runtime marked the
result an error, which includes a call that was interrupted.

Still out, always: meta entries and hook output, attachments, queued placeholders,
the runtime's own markup, a message from another session, a user message that mixes
text with a block that is not text or an image, reasoning that is not written out,
and any entry the reader cannot classify with certainty. They are absent, not empty.
The `system` kind is still emitted by no runtime.

## Alternatives considered

**Hold the cursor back at a running call.** The reader stops at the first unresolved
call and re-reads from there. Rejected: one long call (a build, a sub-task that runs
for an hour) would freeze the whole record behind it, and everything after it would
be sent again on every poll.

**Carry the pending calls in the cursor.** Rejected: the set is unbounded (a call
that never gets a result stays in it for ever), it makes every cursor a function of
what happened to be running, and it breaks on a `/clear`. Resolving a late result
against the file costs one bounded read only when a result actually arrives late.

**A `tool_result` kind.** Rejected by the issue: a call and its result are one
message, and a client then never has to join two.

**Send a call only once its result exists.** Rejected: the oracle requires an
unpaired call to read `running`, and a viewer of a live session needs to see what the
agent is doing now.

**A signed or time-limited media reference, or a store of issued references.**
Rejected. A signature would add nothing: every read applies the allow-list again,
bound to the session the caller named, and a reference cannot name anything the
caller could not have been sent. A store would be state to bound, expire and lose on
a restart, for a lifetime the file already defines.

**Image bytes inline, within the 64 KiB cut.** Rejected by the issue: the cut would
break an image, and a page would carry megabytes.

**A separate grant, switch or audit event for images.** Rejected: reading an image is
reading the record, and ADR 441's four constraints already say how that is guarded.
A second lock on a thing the first lock guards only makes a fleet that granted
`record` on purpose discover the second.

**The K most recently written sub-tasks, and "not named means new".** The first
design for the cursor. Rejected: a sub-task that fell out of the window and came
back, or one resumed after it finished, would be read again from its start and its
whole history repeated, and one that is still running but older than the window
would be lost. The watermark names exactly the files that need it, and errs toward a
bounded loss and never toward a repeat.

**Merging the sub-task entries into the conversation at the call's result.** Rejected:
a sub-task started in the background returns at once, long before it finishes, and a
viewer of a live session would see the sub-task only when it was over.

## Consequences

- **A client applies one rule to `tool`: replace by id.** Pages are in the order the
  reader emitted them, and `at` is not monotonic within a page: an update carries the
  call's time and sits where its result was read.
- **Holding `record` now means holding tool output and images**, which is what ADR
  441 said it would mean. Nothing in this change widens who can read; it fills what
  the grant was for.
- **Memory.** One line of up to 16 MiB is held per source per request, and a request
  reads each source it follows. The base64 is not copied and a tool's structured
  result is not decoded, but a deployment that serves many concurrent readers of
  sessions with large images pays for the peak. The switch and the grant are the
  levers.
- **Limits that are invisible to a client and recorded here:** a result on a line over
  16 MiB is skipped, so its call reads `running` for ever; a call older than 32 MiB of
  the record whose result arrives later stays `running`; a sub-task resumed after it
  finished loses what it wrote before the next read; a sub-task file that appears
  more than two minutes after its first line was written is not read as new; a cursor
  names at most 64 sub-tasks; and a sub-task whose sidecar the runtime has not written
  yet is not read until it is.
- **The record is the runtime's, not this service's.** Its entry shape can change in a
  release. The reader fails closed — an entry it does not recognise is omitted, never
  guessed at — so drift costs a missing entry, not a leaked category. The same holds
  for the sub-task layout: if the runtime moves it, sub-task entries disappear and
  nothing else does.
- **`opencode` was unchanged by this change** and answered `501` for both routes; its
  reader is [`443-the-opencode-record-is-read-by-part-and-a-running-call-is-sent-once-and-updated-by-id.md`](443-the-opencode-record-is-read-by-part-and-a-running-call-is-sent-once-and-updated-by-id.md),
  which fills the same kinds under the same rules.
