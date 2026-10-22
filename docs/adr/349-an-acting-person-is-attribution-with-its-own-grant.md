# ADR: a request may name the person it acts for — attribution only, under its own grant

**Issue:** #349
**Status:** decided — on the issue's plan; the open choices below were not ruled by the owner and are recorded for review

## Context

The input audit records which principal sent a request and, for a relayed one,
`<principal> via <peer>`. A client that signs several people in authenticates as
one principal, so neither the audit nor the machine that owns the session can say
which person asked.

## Decision

1. **Two headers, `Fleet-Acting-Person` (opaque id) and `Fleet-Acting-Issuer`,
   both required.** An id means something only to whatever minted it. Headers, not
   a body field, because the verbs that need it have no common body and because a
   relay already forwards its other assertions as headers.
2. **A grant of its own, `acting-person`, not human-relay and not send.**
   human-relay changes how a message is delivered (unlabelled, slash commands);
   this changes nothing about delivery. They are different claims by different
   kinds of client, and an operator should choose each separately. It is left out
   of the legacy all-verbs flags and of the doctor's supervisor set.
3. **Decided once per request, in the mutating wrapper**, stored in the request
   context, carried in `fleet.Caller.Person`, and forwarded by the remote driver.
   Honoured when the authenticated principal holds the grant; across a relay, when
   the relaying machine is a configured peer *and* holds the grant on the receiver
   (the entering machine having checked its own caller). Otherwise dropped, the
   call proceeds, and the drop is recorded.
4. **Never authorization.** The principal's verb grants decide; a person can
   neither widen nor narrow them.
5. **No principal table ⇒ dropped.** A shared token names no one, so attribution
   anyone holding it could forge is not attribution. (human-relay is honoured
   there; that is safe only for a reason that does not carry over.)

## Alternatives not taken

- **Compose the person into `principal` in the input record**, as the issue's
  `<person> · <principal> via <peer>` reads. Rejected for the stored record: the
  `principal` query filter is an exact match, and clients filtering on a principal
  would stop finding calls that named a person. The record carries `person` as its
  own field; the composed form is the mutation audit line's actor.
- **Reuse human-relay.** Rejected, point 2.

## Consequences

- A dropped claim is visible (`personDropped`, an `audit:` log line), so a client
  can find out it attributed nothing.
- An acting person is as trustworthy as the principal that vouched for it. The
  audit says so by recording both.
