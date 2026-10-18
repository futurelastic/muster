# ADR: every input is recorded with a hash of its text, and the record can be read back

**Issue:** #330
**Status:** decided — on the issue's plan; the open choices below were not ruled by the owner and are recorded for review

## Context

A client wants to prove later that a prompt in a session was typed by a person
and not sent by another agent through `POST …/input` — for example, a tool that
accepts a typed approval as consent for an irreversible act. Three things were
true: the engine's transcript marks every user turn the same way; the label
convention in the text is controlled by the sender; and the service's mutation
audit line (actor, route, target, outcome) is a log line — no text hash, nothing
durable, nothing a client can query.

Two earlier rulings already did half the work. The service stamps and labels
senders itself (#158, #257), and whether a call is "a person's own message" is a
grant (`human-relay`), never a header. What was missing is a durable, queryable
record of that finding.

## Decision

1. **One record per input that reaches a driver**: principal, machine, session,
   time, sha256 and length of the text as sent, the `humanRelay` finding, the
   requested route, direction (`local`, `relayed-out`, `relayed-in`) and outcome.
   The text is never stored. A request refused for its own shape reached nobody
   and leaves nothing; a driver failure after the attempt is recorded as `error`,
   because the text may have arrived.
2. **Append-only JSON lines, not history.go's document.** `/input` is hot and a
   record is immutable once final, so rewriting a whole document per call would
   be the wrong shape. The log is `input-audit.jsonl` in the state directory,
   owner-only, indexed in memory, compacted by atomic rewrite at most hourly and
   whenever retention drops something. No state directory: in memory, and the
   read says `durable: false`.
3. **Two phases.** A `pending` line is synced before the driver is asked to
   deliver; a final line follows. A proof that nobody sent a text must not have a
   hole exactly at the delivery, which a record written only afterwards would
   have if the process died in between. A pending record found after a restart
   reads `unknown`.
4. **The read says what it covers.** `coveredFrom` is the later of when
   recording began, the retention cut, the record cap's cut and any failed write.
   A window starting earlier than it cannot prove absence. A failed write never
   fails the input (the same stance as the history store).
5. **Both ends of a peer relay record.** The machine the call entered knows the
   authenticated principal; the owner knows the peer and its assertion, which it
   repeats only from a configured peer. The read is machine-local, with no
   fan-out: a verifier asks the machine the call entered.
6. **Retention** is 14 days by default (`inputAuditRetentionDays`), capped at
   100000 records.

## Choices the issue left open, taken here

- **The read grant is `read`.** The issue asks for it and §5 says reads are one
  grant. A dedicated `audit` grant is the narrower option and is cheap to add
  later (it would need a config migration for every reader); not taken because
  nothing yet shows a reader that should see sessions but not who messaged them.
- **A relayed call is recorded on the relaying machine as well**, not only on
  the owner — otherwise the original principal is held only by a machine the
  verifier may not be able to reach.
- **The session is recorded under the id the caller addressed.** A name or
  conversation id that resolves to a session is not rewritten, so a query uses
  the id the sender used.

## Not covered

The log is as trustworthy as the machine's filesystem: a principal with write
access to the state directory can edit it. It proves what this service accepted,
not what the engine did with it, and a person who relays another agent's text
through a `human-relay` principal is, by that grant's own definition, a person.
