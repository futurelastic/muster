# ADR: a session's result is delivered by the session, not returned by this API

**Issue:** #82
**Status:** decided — narrowly superseded by #258, which permits reading a session's assistant turns (and nothing else); every other part of this decision stands. See [`258-assistant-turns-read.md`](258-assistant-turns-read.md). Narrowed further by #441, which permits reading a session's full record through its own route under its own `record` grant; see [`441-a-sessions-full-record-is-read-under-its-own-grant.md`](441-a-sessions-full-record-is-read-under-its-own-grant.md).

## Context

Dispatching a named agent to a peer machine through this API is a complete
round trip for everything except the answer. Create it there with `agent`,
`prompt`, `cwd`; poll or subscribe for `idle`/`waiting_input`; `respond` to a
permission dialog; `input` a follow-up so the loop is not one-shot;
`DELETE` with `startedAt` for a corroborated teardown. None of that needed
anything outside this service — and none of it gets a caller back what the
dispatched agent produced. `state` reports `screenDigest`, a fingerprint of
the screen, never its content, so a caller learns *that* the work finished
and never *what it produced*.

Three workarounds are already in use to close that gap, and each leaves this
API entirely:

- a directory both machines synchronise — no completion signal, and lags, so
  a reader acting the moment `state` reports `idle` is racing the sync, not
  verifying it;
- a local HTTP server on the worker's machine, returning a URL — faster and
  race-free, but assumes that server exists there;
- a launcher script over a shell connection, capturing standard output —
  bypasses this service entirely, so nothing it produces is a session: no
  state, no dialog answering, no teardown, nothing visible to any other
  client.

The Issue argues this belongs inside the service rather than above it,
because §1's non-goals list version control, issue trackers, planning, and
completeness judgements, and a session's own result is on none of those
lists. That test answers the wrong question: §1 lists *domains* this
service does not understand. A session's result is not a domain — it is a
*data class*, and the constraint that actually governs data classes is
written elsewhere, independently, three times: `screenDigest` is a
fingerprint and never the text it was taken from; `environment.names`
carries variable names and never values; §2.9's `ConversationRef` names a
transcript record without ever opening it. All three draw the same line —
this service reports content the *runtime* produced about itself, never
content the *session* produced — and a result is definitionally the latter.

## Decision

**No result endpoint, no result-carrying wire field, no service-held result
slot.** The reply address belongs in the dispatch brief the caller hands the
worker, and the worker delivers its answer by calling `input` on the
requesting session — the endorsed shape of the convention that was already
in informal use, written down rather than left for the next caller to
reinvent as a fourth workaround.

This is written into the normative layer, not left as this ADR's own
opinion: session-abstraction.md gains §5.8 ("Report facts about content,
never content the session produced") stating the rule and its derivation,
and §7.6 recording the decision; api-http.md §6 and api.md's "what this API
deliberately lacks" both name the data class explicitly; client-guide.md
gains a full section on how to use the convention and what it costs.

A large answer that does not fit in a prompt has no answer at this layer.
That is a transport choice made above it — the identical shape
`docs/adoption.md` §2 already uses for the cross-machine write race: neither
is a defect in this service, and neither is fixed here. Pick a transport
deliberately; the failure mode of not deciding is silent.

## Alternatives considered

**A session-scoped result slot (the Issue's Option 2).** The worker writes
once, the caller reads once, the service stores bytes it never interprets.
This looks smaller than exposing a transcript and is not: on the *read*
path it is the same endpoint. A worker that writes its own transcript into
the slot has built the exact leak `screenDigest`'s fingerprint-only design
was written to prevent, and this service — by the design's own premise that
it never interprets the bytes — cannot tell that it did. The slot does not
remove Option 3's exfiltration surface; it moves the decision about that
surface from the service, which has a documented policy, to the dispatched
agent, which has none and is the component under the least control.

Two further objections, independent of the one above:

- **The field would publish a value this service was handed as a fact.**
  This service has already ruled against that shape three times — a pin
  reported as applied, an assertion reported as agreement, a dictated
  identifier reported as an address (§2.13). A `result` field reads to a
  caller as "the work finished, and here is what it produced," a claim the
  service is in no position to make and would be making anyway, by the
  field's mere presence.
- **§5.7's absence/failure distinction is unbuildable at this layer.**
  Every existing optional-and-graduated field (`ConversationRef`,
  `RuntimeSurfaceRef`, `IdentityAssertion`) resolves its states from
  something the *driver* can observe. A result slot's central distinction —
  "no result yet" versus "no result is ever coming" — is decided by the
  dispatch brief, which lives above this layer entirely. There is no
  observer here to resolve it, so shipping the field means shipping a
  permanently-unresolvable state at its centre.

**Expose captured output directly (the Issue's Option 3).** Rejected on the
same grounds `screenDigest` was already decided against: a transcript is
unbounded, carries whatever the agent chose to print, and turns the read
path into a data-exfiltration surface the fingerprint-only design
deliberately avoids. Restated here only because #82 asked the question
again; the answer is the one already given.

**A `ConversationRef`-shaped `ResultRef`.** The most seductive alternative,
and the one worth spelling out carefully because the difference from
`ConversationRef` is not structural. `ConversationRef` names a transcript
record the *runtime* wrote **unasked** — §2.9 calls it "an independent
witness... the first source in this model that is not an echo," and that
unprompted provenance is exactly what makes naming it, without opening it,
a safe thing for this service to do. A session's result is written by the
agent **on purpose, for whoever reads it back** — the same forgeable class
§2.3's `ControlChannel.reason` note (muster #69) already warned about,
twice measured: a supervisor grepping panes for a disconnection notice
classified *itself* as disconnected, and a prompt classifier was fooled by
an agent that had typed "No auth bypass" into its own prompt. Same
reference-not-content structure as `ConversationRef`; opposite provenance.
Provenance is the property that matters, not shape, so this does not
transfer.

## Consequences

- The three existing workarounds keep working exactly as before; nothing
  here removes a capability anyone has today. What changes is that the
  convention that already covers the common case — an answer that fits in a
  prompt — is now written down, with its grants and its authority cost
  stated, instead of being folklore a caller has to rediscover.
- A large answer remains entirely a supervisor's problem, same as the
  cross-machine write race. `docs/adoption.md` §2 now states both in the
  same place, as one recurring pattern rather than two unrelated notes.
- **A delivered reply is untrusted text landing in the requester's own
  composer, indistinguishable from its own operator's instruction.** This
  property existed the moment any caller adopted the informal convention;
  it was simply never written down. `docs/client-guide.md`'s new section
  states it plainly so a future incident is legible against something,
  rather than being the first time anyone said it out loud.
- The audit trail (§6 requirement 4) already names both parties on a
  relayed reply — `actor="<worker> via <worker machine>"` — but records the
  *grant*, not the route: `input` and `respond` both log `verb=send`, so a
  delivered result and an ordinary follow-up are indistinguishable in the
  log. Filed separately as muster #105 rather than fixed here,
  because this decision's content is "no new surface," and a wire-visible
  marker distinguishing "instruction" from "answer" would itself be exactly
  that kind of surface, proposed against a harm nothing has yet caused.
- The Issue's own "Related" remark — a peer's capability report comes back
  `source: "assumed"` with every field false when unreached, so a caller
  cannot check the agent pin will be honoured before dispatching — is a
  real, separate defect that this decision does not depend on and does not
  fix. Filed separately as muster #106.
- **Left genuinely open, for a human rather than this ADR:** whether "this
  service never returns session-produced content" is a permanent invariant
  — in which case it belongs in §1's non-goals, alongside version control
  and work planning — or the current best answer, kept in §5 as a rule
  derived from precedent and open to being outweighed by a precedent nobody
  has found yet. This ADR and §5.8 take the second shape. Promoting it to
  §1 later is a one-line move, not a rewrite, if the repo's owner judges
  otherwise.

## Follow-ups filed against this Issue, not bundled into it

- **A.** `internal/service/audit.go`'s `logMutation` records the grant
  (`verb=send`), not the route, so `input` and `respond` — and now a
  delivered reply — are indistinguishable in the audit trail. §6
  requirement 4 asks for "verb"; what is recorded is coarser.
- **B.** A caller cannot read its own grants, so every precondition this
  convention needs (`send` here, `relay` there) is discovered by trying and
  reading the refusal — the grant-plane sibling of this Issue's own
  capability-report gap (`source: "assumed"`, every field false, when a peer
  is unreached).
