# ADR: a caller may read what a session's agent wrote — assistant turns only

**Issue:** #258
**Status:** decided — the owner's ruling, 2026-10-04
**Supersedes, narrowly:** [`82-session-result-belongs-above-this-layer.md`](82-session-result-belongs-above-this-layer.md)

## Context

#82 ruled that this API returns no content a session produced, and endorsed a
convention in its place: the dispatch brief names a reply address and the worker
writes its answer back with `input`. The convention works when the worker
remembers and the answer fits in a prompt. It has no answer for the case that
shows up in practice: a coordinator that wants to know what a peer session
*concluded* — to check, not to be told — has nothing to call. The deep-link
viewer that humans use is explicitly not an API.

The owner ruled, live, that this should be readable through the API, narrowly:

- **Who:** any caller holding a valid fleet credential for the target machine —
  the credential `input` and `respond` already require. No new grant.
- **What:** the session's **assistant turns only**: the text its own agent
  produced. Never tool calls or results, never file contents, never the messages
  a human or another session sent in, never system or hook output.

## Decision

`GET /v1/machines/{machine}/sessions/{id}/turns?since=<cursor>&limit=<n>&startedAt=<ts>`
returns `{turns: [{at, text}], next}`, oldest first, read from the runtime's own
record of the conversation on the machine that owns the session, and relayed to
a peer like every other session route.

- **The boundary is an allow-list, held where the record is read.** An entry is
  returned only when it decodes cleanly and the runtime's own fields say it is a
  top-level assistant message, from a real model, with text blocks. Reasoning
  blocks, tool calls, tool results, user entries of any origin, system and hook
  entries, attachments, sub-agent (sidechain) entries, meta entries and the
  runtime's synthetic notices (an API error is the runtime speaking, not the
  agent) are never returned. An entry the reader cannot classify with certainty
  is left out. Nothing downstream of the driver — the service, the relay, the
  wire type — filters or widens it, so there is one place to audit.
- **Authority.** With a principal table, the `send` grant: the grant that already
  governs speaking to a session also governs reading what it said back. `relay`
  is not additionally required for a peer target (the same ruling #81 made for
  reads — reaching is not changing); the owning machine applies its own table to
  the asserted caller (§13). Without a principal table a valid token is the
  credential, as for every other read.
- **Corroboration.** `?startedAt=` is checked against the live session before
  anything is read, and a disagreement is `conflict` — a recycled id must not
  hand one session's words to a caller who meant another's. Omitted, the read
  has the weaker id-only guarantee, as `labels` does. A `startedAt` that does not
  parse is refused rather than ignored: for a read of another session's words,
  silently dropping the check the caller asked for is worse than refusing.
- **The cursor** is opaque. It names the conversation it was issued against and
  a position in that conversation's append-only record. Presented against a
  different conversation (a `/clear`, a relaunch) or past the end of a shrunken
  record, it is `conflict`: the caller's belief is stale, and the same offset in
  another file is a different place. Without `since`, the most recent `limit`
  turns are returned and `next` is where to resume. `limit` defaults to 20; over
  100 is refused rather than clamped, so a short page is never mistaken for a
  short conversation. A turn longer than 64 KiB is cut and marked `truncated`.
- **A session whose record cannot be identified** (created a moment ago and not
  written yet, or resumed in a way the record cannot be tied back to) answers
  `not_found`, `retryable`. That is the absence of a source. A conversation in
  which the agent has said nothing is `200` with `turns: []` (§5.7); the two are
  not collapsed.
- **Audit.** Every read leaves a line — caller, session, number of turns,
  outcome — whatever the outcome, and never the text. A refused caller leaves a
  `DENIED` line.

It is unrelated to `SessionState.turns`, the liveness count of #111: that field
is a number the runtime wrote about itself, and this route is text the agent
wrote.

## Why this is acceptable now, when #82 declined

#82's case against exposing content rested on two premises, and the ruling does
not weaken either; it narrows the thing they were applied to.

1. **An unbounded read path is an exfiltration surface.** A raw transcript
   carries whatever the agent chose to print or read — file bodies, command
   output, credentials in a tool result. This route does not expose a transcript.
   It exposes one data class, selected by an allow-list over the runtime's own
   fields, size-bounded per turn and per page, behind the grant that already lets
   the caller act on the session.
2. **A session-authored value is forgeable as a statement of fact.** An agent
   that writes "done, all tests pass" has produced text, not evidence. That is
   still true, and it is why the wire type is a bare `{at, text}`: no `status`,
   no `result`, no field a caller could read as the service vouching for the
   content. Callers must treat a turn as what the agent said. Whether the session
   finished is `state`, which the runtime reports about itself.

What does not change: the service still stores no result, still carries no
result field, and still has no opinion on whether a turn is the answer to
anything. A caller decides what a turn means.

## Alternatives considered

**A `read`-grant route.** Rejected: `read` today covers listing, state and
environment, all of which are facts about sessions. Content is a different
class, and the owner ruled it rides on the credential that can already drive the
session. A principal that may list but not send should not be able to read.

**A new `content` grant.** Rejected by the ruling ("no new grant"). It would also
be a seventh grant nobody configures separately from `send`, which is the reason
`respond` shares `send` rather than inventing one.

**Include tool results, or the human side, for a "complete transcript".**
Rejected by the ruling and by premise 1: tool results are where file contents and
credentials live, and inbound messages belong to someone else's conversation.

**A last-message field on `state`.** Rejected: it would put session-authored text
in every listing, which is the leak `screenDigest`'s fingerprint-only design
exists to prevent, and a listing is read far more widely than a deliberate call.

**Screen capture.** Rejected: the screen is the runtime's rendering, mixes the
agent's words with the human's composer and tool output, and cannot be
cursored.

## Consequences

- A coordinator can check what a peer session concluded without relying on the
  worker to write back. The reply-address convention in the client guide remains
  the way to *push* an answer; this is the way to *pull* one. Neither replaces
  the other.
- **What an agent writes is now readable by anyone holding `send` for its
  machine.** An agent that echoes a secret into its own prose has put it where
  this route can return it. The boundary excludes tool output, not what the
  agent chooses to say; operators who hand out `send` should read it as also
  granting this.
- Where no principal table is configured, a valid token is enough, including on a
  machine that withholds mutation. That is the legacy single-token model's
  documented shape (reads are broad, mutations are opt-in) and is stated here
  because it is the one place the new route is not gated on a mutation grant.
- Two runtime drivers implement it: the terminal runtime's and, since #369, the
  `opencode` driver's (see the addendum). A runtime without one answers
  `unsupported`, and so does a peer on a build that predates the route.
- The record is the runtime's, not this service's: its entry shape can change in a
  runtime release. The reader fails closed — an entry it does not recognise is
  omitted, never guessed at — so drift costs a missing turn, not a leaked
  category. The fixtures pinning the boundary are synthetic and exercise every
  category listed above.
- The normative layer is amended to match: session-abstraction.md §5.8 and §7.6
  state the narrower boundary, and api-http.md §3.3 and §6 and `docs/api.md`
  stop promising that no endpoint returns session content.

## Addendum — the `opencode` runtime (muster #369)

The route, the grant, the wire type and the audit line are unchanged; what a
second runtime has to supply is the allow-list and a cursor over *its* record.

- **A turn is one assistant message of the runtime's record**, its text parts
  joined by a blank line in the order written. That is this ADR's "top-level
  assistant entry", read at the grain this runtime stores. It is not one turn per
  user prompt: measured against a live runtime, a prompt that uses a tool is
  answered by two assistant messages (one per model round), and each is a turn —
  the same two turns the terminal driver returns for the same interaction. The
  last turn is still the final report.
- **The boundary** is the same shape: finished (completion time set), error-free,
  top-level, from a real model, and not the runtime's own compaction summary;
  only parts the runtime typed `text` and did not mark `synthetic` or `ignored`
  contribute. Reasoning, tool calls and results, files, patches, snapshots, step
  records and any part type not known today contribute nothing, and a message with
  a part the reader cannot decode is left out whole. A message that ended in an
  error is left out even if it holds partial prose: the failure is
  `state.lastTurn`'s to report. The code that decodes a part's text lives in one
  file, and the types the state and usage reads use still cannot.
- **The cursor** is a position in the record, not an offset: the runtime pages
  from the tail and has no "after", so `since` names the last message already
  consumed, together with the session id and the session's own start time. A
  cursor presented against another session or another incarnation of the id, or
  whose message the record no longer holds (deleted, reverted), is `conflict`;
  one that is not a cursor is `invalid`.
- **A message still being written is never returned partial.** It is held: the
  page ends before it and `next` does not move past it, so a later read returns it
  once, whole. A remnant of a cut-off turn is passed over only when a later
  assistant message exists, which means a session whose last turn was cut off
  shows an empty page until its next turn — fail-closed, and `state.lastTurn`
  already says the turn did not finish.
- **No `pending`.** That field is text read off a screen (#266); this runtime has
  none.
- A session that is listed but not running answers `conflict` (the reason is the
  runtime's, as for every other verb); one the runtime no longer holds is
  `not_found`. There is no "record not identifiable yet" state here: the record is
  the server's answer, and a session with no messages is `200`, `turns: []`.
