# ADR: a session's full record is read through its own route, under a `record` grant nobody holds by default

**Issue:** #441
**Status:** decided — the owner's ruling, 2026-11-11 (option B of the three below)
**Supersedes, for this route only:** the content boundary of [`258-assistant-turns-read.md`](258-assistant-turns-read.md)
**Further narrows:** [`82-session-result-belongs-above-this-layer.md`](82-session-result-belongs-above-this-layer.md)
**Extended by:** [`442-the-record-carries-tool-calls-sub-tasks-summaries-and-images-by-reference.md`](442-the-record-carries-tool-calls-sub-tasks-summaries-and-images-by-reference.md) — what the terminal runtime emits (the paragraph "What each runtime emits today" below is the state at #441; ADR 442 supersedes it), the media route, and cursor version 2. The route, the grant, the audit event and the switch are unchanged.
**Extended again by:** [`443-the-opencode-record-is-read-by-part-and-a-running-call-is-sent-once-and-updated-by-id.md`](443-the-opencode-record-is-read-by-part-and-a-running-call-is-sent-once-and-updated-by-id.md) — the `opencode` runtime reads a record too (the sentence below that says it answers `501` is the state at #441).

## Context

[ADR 258](258-assistant-turns-read.md) let a caller read what a session's agent
*said*: its assistant turns, as bare `{at, text}`, on the credential that can
already `send`. It left out, on purpose, everything else a record holds — tool
calls and their results, "because tool results are where file contents and
credentials live", and the messages people and other sessions sent in.

A client that shows a session as a conversation needs more than that. It needs
both voices, in order, and then the tool calls, the reasoning, the sub-task
entries, the summaries and the images. That is the whole record, and it is a
different thing to expose from a list of what the agent wrote: the exposure this
adds over `turns` is exactly the one ADR 258 named as its reason for stopping.

The owner filed the epic that widens the content himself, so the direction is
his. What was left open was *who may read it*. Choosing a grant decides who may
read a record that will hold tool output and messages people sent in, which is an
access decision and so the owner's. Three options were put to him:

- **A — `send`, as `turns` does.** No new grant. Anyone who can drive a session
  reads all of its record, tool output included.
- **B — a new `record` grant.** Separate from `send`. The full record needs an
  explicit grant nobody holds until it is configured.
- **C — split by kind.** `send` for the text entries, a new grant for tool,
  image and user entries; every page filtered by what the caller holds.

He ruled **B**, on 2026-11-11.

## Decision

`GET /v1/machines/{machine}/sessions/{id}/record?since=<cursor>&limit=<1..100>&startedAt=<t>`
returns `{messages: [...], next, format}`, oldest first, read from the runtime's
own record of the conversation on the machine that owns the session, and relayed
to a peer like every other session route. It is a route of its own beside `turns`,
not a wider `turns`: `turns` keeps its shape, its boundary and its grant.

- **The grant is `record`.** Under a principal table the caller needs it and
  nothing stands in for it: not `read`, not `send`, not `relay`. It is in no
  default set. `muster service install` does not enrol the local service
  principal with it, the legacy single-token flags do not imply it, and no
  existing principal gains it by upgrading. A signed machine list may name it for
  a peer — an owner-signed, explicit act, and the way one machine of a fleet is
  allowed to read another's records. For a peer target it does not also require
  `relay` (the ruling of [#81](81-relay-grant-not-required-for-reads.md): reaching
  is not changing); the machine that owns the session applies its own table to the
  asserted caller. A read relayed through machine A therefore needs `record` on
  A's principal and, on the owner, on the principal A relays as.
- **The shape is versioned by `format`** (1). One message is
  `{at, kind, role, text?, tool?, parent?, model?, media?, truncated?}`:
  `kind` is one of `text`, `reasoning`, `tool`, `image`, `summary`, `system`;
  `role` is `user` or `assistant`. `tool` is a call and its result paired by the
  service, `parent` names the tool call an entry was made inside, `media` is a
  reference read separately and never inline. Later work adds *kinds* under the
  same format and never renames a key, so a client skips a `kind` it does not
  know. `next` is returned even when `messages` is empty.
- **What each runtime emits today** is the allow-list of that runtime's reader,
  held where the record is read and nowhere downstream, exactly as for `turns`.
  This change ships the terminal runtime's `text` entries in both directions:
  - the assistant's prose, by the **same predicate `turns` uses**, so the two
    routes cannot disagree about what the agent said (with `model` set);
  - a message a person typed: a top-level user entry, from a human origin, prompt
    source typed or queued, whose content is text and only text, and which is not
    the runtime's own markup (a slash command or its output, a bash-mode line, an
    injected reminder or hook output, an interruption note).

  Out, always, for now: tool calls and results, reasoning, sub-agent entries,
  summaries, system and hook output, attachments, images, a user message that
  mixes text with any other block, a message from another session, and any entry
  the reader cannot classify with certainty. They are absent from the page, not
  empty. The `opencode` runtime and the test stub emit nothing yet and answer
  `501`; a later change adds a reader for the first and flips
  `capabilities.readsRecord`.
- **The #14 constraints**, all four:
  - *The route serves content and nothing else.* It carries no `status`, no
    `result`, nothing a caller could read as the service vouching for it.
  - *Every read writes a content-read audit event* — `audit: content-read` — with
    who, which session, how many entries and how it ended, and never the content,
    a cursor or a model name. It is a different event from the state reads' lines
    and from `turns`' `read-turns`, so the trail tells a content read from a state
    read by its name. A refused caller leaves a `DENIED` line, as for every grant.
  - *One configuration switch refuses the route wholesale.* `refuseRecordReads`
    in the config file, per machine and read at start. Every call then answers
    `501 unsupported` with the reason `record-off` — a local session, a peer
    target and a call relayed in alike, whatever grants the caller holds. The
    grant is checked first, so a caller without it learns nothing about how the
    machine is set up.
  - *The grant is `record`*, as ruled.
- **The rules are `turns`'.** `?startedAt=` is checked against the live session
  before anything is read (a disagreement, `409`; one that does not parse, `400`).
  The cursor is opaque, bound to the conversation it was issued against, and
  `409` against another conversation or past the end of a shrunken record. It
  reuses the offset machinery `turns` has — one place for the partial-last-line
  and tail-window logic — under its own version tag, so a `turns` cursor handed to
  `record`, or the reverse, is `400`: the two routes' page sequences differ and a
  position in one is not a position in the other. `limit` defaults to 20 and over
  100 is refused, never clamped. A text field over 64 KiB is cut on a rune boundary
  and marked `truncated`. A record that cannot be identified yet is `404`
  `retryable`, a runtime that cannot be read is `501`, and a conversation with
  nothing admitted is `200` with `messages: []`.

## What it supersedes, and what it does not

ADR 258's "never tool calls, tool results, or the messages a human or another
session sent in" no longer binds the record route: it may carry them as the kinds
that hold them are added, and this change already carries the messages a person
typed. It still binds `turns`, whose type, route, grant and tests are unchanged.
Reading a session's agent's words through `turns` still costs `send` and nothing
more.

The premises ADR 258 kept from [ADR 82](82-session-result-belongs-above-this-layer.md)
are not weakened. A record is still an exfiltration surface, which is why it is
behind a grant that exists to be withheld, an audit trail that cannot carry the
content, and a switch the operator can use without touching a grant. And a
session-authored value is still forgeable as a statement of fact, which is why a
message is a bare statement of who said what and when, and the service still
stores no result and has no opinion on whether a message answers anything.

## Alternatives considered

**A — `send`, as `turns` does.** Rejected by the ruling. It would hand every
holder of `send` the tool output a record carries, which is the exposure ADR 258
stopped at. A deployment that hands out `send` today has not agreed to that, and
a changelog is a weak way to tell it; a content read is a deliberate, attributable
act, and a grant keeps it one.

**C — split by kind.** Rejected. It is the most precise option, and it means
filtering every page against the caller's grants to save one entry in a table.
It also puts the access decision inside the page, where a missing entry reads as
an empty conversation.

**`read`.** Rejected: `read` covers listing, state and the environment, which are
facts about sessions. Content is a different class (the reason ADR 258 refused it
too).

**Widening `turns` instead of adding a route.** Rejected. `turns` is a wire type
with deliberately no kind, role or raw field, each of which would be a place for
the next category to slip in unreviewed, and it is on a grant the owner chose for
what it carries. A wider shape on the same route would have widened that grant
with it.

**A persisted audit store, like the input audit's.** Rejected for now. That store
exists so a verifier can read an absence back over a route with retention; nothing
reads content-read events back yet, and a store brings a file, retention, a read
route and a grant decision with it. The log line is what every read audit (`turns`,
the composer) is today, and a store can be added behind the same event.

**A switch that takes effect live.** Rejected. It needs an atomic flag on the
service and apply logic in the reload, while `profilingEndpoint`, the switch this
one follows, is read at start. An operator who needs the route off in an emergency
restarts the service; the grant is the lever that needs no restart.

**A switch that is on by default** (the route off until the operator turns it on).
Rejected: the grant is already default-closed, and a second default-closed lock
would make the route unusable on a fleet that has granted `record` on purpose.

**Masking `capabilities.readsRecord` when the switch is on.** Rejected. The flag
is what the *runtime* can do; the switch is a policy, and it reports itself in the
refusal's `reason`. A client that offers the control must be ready for `record-off`.

## Consequences

- **Holding `record` will mean holding tool output** once the kinds that carry it
  land, and the messages other sessions sent in. Operators should grant it the way
  they grant `keys`: to a principal that is meant to see everything a session did.
- **On a machine with no principal table a valid token reads the record**, exactly
  as it reads `turns` and every other read: the legacy single-token model has no
  per-verb grants to consult. There the switch is the only lever, and it lives in
  the config file, so a machine that wants the route closed must have one.
  `GET /v1/whoami` reports `record` for such a credential unless the switch is on,
  so a client is not told it cannot do what it can.
- **A relayed read needs `record` twice**: on the principal the entry machine
  authenticates, and on the owning machine for the principal it relays as.
- **The record is the runtime's, not this service's.** Its entry shape can change
  in a release. The reader fails closed — an entry it does not recognise is
  omitted, never guessed at — so drift costs a missing entry, not a leaked
  category.
- **Three limits of this change are invisible to a client** and recorded here: a
  prompt that mixes text with an image or a document is dropped whole until the
  kinds that carry them are read; a message over 1 MiB on one line is skipped
  rather than truncated (as in `turns`); and a message another session sent in is
  excluded because format 1 has no field to attribute it.
- There is no backward paging, as in `turns`: without `since` the most recent
  `limit` entries are returned, and the cursor only moves forward.
- The normative layer is amended to match: `session-abstraction.md` §2.16 (the
  shape), §4 (`readsRecord`), §5.8 and §7.6; `api-http.md` §2, §3.1, §3.3, §5 and
  §6; and `docs/api.md`.
