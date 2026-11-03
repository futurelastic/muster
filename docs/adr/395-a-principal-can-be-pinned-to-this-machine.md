# ADR: a principal can be pinned to this machine, by a field that narrows

**Issue:** #395
**Status:** decided

## Context

A principal holding `read` sees the whole fleet. Every plural read defaults to
`scope=fleet`, and a path naming a peer is answered with `read` alone (ruled in
#81). So there was no way to give a client a credential that sees **only this
machine's sessions**. Measured: a session UI pointed at one machine's service,
with a read/create/send principal, listed the sessions of a second, peered
machine too — the client never set `scope`, and the default is `fleet`.

#81 left a narrower cross-machine-reach control open "against a distinction
nothing has yet been harmed by". This is the case that was harmed: an operator
wants a local-only view for one client without changing what any other principal
sees.

## Decision

**A principal field, `"scope": "local"`.** Absent, or `"fleet"`, is today's
behaviour. For a pinned principal, whatever grants it holds:

- a fleet-scoped read is **answered as `scope=local`**;
- a request naming another machine is **refused**, reads and mutations alike
  (`401`, `reason: "local-only"`);
- `GET /v1/machines` (and `/v1/runtimes`) list this machine alone;
- `GET /v1/whoami` reports `scope`.

### Field, not grant

#81 priced a new grant at "a migration for every existing principal". That is
the cost of a grant that **adds** reach: every grant defaults to denied, so the
principals that already have fleet reach would have to be given the new grant to
keep it. A restriction inverts the arithmetic. As a field that only narrows,
absent means unchanged, so no principal already in a table moves — no migration,
and nothing to remember to add to the supervisor set or a machine list. A grant
spelled "local-only" would also read backwards: holding it would mean being able
to do *less*, in a vocabulary where holding a grant means being able to do more.

### Clamp the scope, refuse the path

The two asks are different acts. A fleet-scoped read is the **default**: the
client in the measured case never asked for the fleet, it got it by not saying.
Refusing that request would break exactly the client the operator is trying to
keep working, so it is answered for this machine — the response carries this
machine's one source, which is what the same request with `scope=local` returns.
An explicit `scope=fleet` is clamped the same way: one rule, and a client has no
way to tell its configuration from its request. A malformed `scope` stays `400`.

A path naming another machine is a **deliberate** act: the client chose the peer.
There is no local answer to give instead, so it is refused. The `reason` is
`local-only`, so a client can tell "this credential is pinned" from "this
credential lacks a grant" and does not go asking for a grant that would not help.

### One gate, in front of every route

The refusal is in `authenticated()`, not in `reading()` and `mutating()`. Those
wrappers do not cover every route that can name a machine: the turns read has its
own gate (`revealing`), the machine-list push decides its grant before reading a
body, and whoami names its machine in the query. A check in the wrappers is a
check on the routes someone remembered to wrap; this one is on all of them,
including one added later. The answer is identical for a peer and for a machine
id that exists nowhere, so the refusal cannot be used to enumerate peers.

The machine list (`GET`/`PUT /v1/machine-list`) names no machine in its path but
is the fleet's roster and the peers' addresses, and a push replaces it; a pinned
principal is refused there too.

### A reload that pins a principal cuts its fleet streams

`stillValid` takes whether the stream reaches the fleet. A reload that pins the
principal closes the streams that do and leaves the ones that already read
locally, so the client reconnects into the local view instead of holding the wide
one open until it happens to drop. A request that authenticated just before the
reload and binds just after is closed at once, as for a revoked credential.

## Alternatives considered

**A grant (`local-only` or its inverse, a `fleet` grant).** Rejected for the
reasons above: it is either a migration for every principal (the inverse) or a
grant that subtracts (the direct form).

**Refuse a fleet-scoped read from a pinned principal.** Rejected: it breaks the
very client the pin exists for, which sends the default.

**Put the refusal in `reading()` and `mutating()`.** Rejected: misses the routes
that bypass them (above).

**A per-principal list of machines it may reach.** More general, and what an
operator might eventually want, but nothing asked for it: the harmed case is one
machine. A field that takes `"local"` leaves room to take a list later without
breaking a config that says `"local"`.

## Consequences

- No behaviour changed for any principal that does not set `scope`.
- The pin governs the principal as a caller of this service. It does not stop the
  service from talking to its peers for everyone else, and a principal this
  machine presents *to* a peer is that peer's to pin.
- A pinned principal cannot be the machine's own supervising credential if that
  credential needs the fleet view; `muster doctor`'s grants rows are unchanged.
- There is no `muster principal` flag for it yet; an operator writes the field.
  `muster principal list` marks a pinned principal.
