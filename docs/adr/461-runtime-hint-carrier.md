# ADR: the caller's `?runtime=` reaches a peer as a field of `Request`

**Issue:** #461
**Status:** decided

## Context

`resolveSessionDriver` returns for a peer before it reads the `runtime` hint,
and the remote driver had nothing to forward it with: a `SessionRef` is
`{machine, id, name}` and none of the fourteen session-addressed calls the
driver builds set a `runtime` query key. A session id is only unique within a
runtime, so a peer holding one id under two runtimes answered `invalid` ("pass
`?runtime=` to disambiguate") to a caller addressing it through this machine,
who had no way to.

The hint has to cross from the route handler, which reads the query, to the
remote driver's request builders, which only receive a `fleet.Request`.

## Decision

`Request.RuntimeHint` (`session-abstraction.md` §2.6), set once in
`requestFrom` from `?runtime=` and appended by the remote driver to every
session-addressed path through one helper, `withRuntimeHint`. No hint ⇒ the path
is returned untouched, so a call that named none is byte-for-byte what it was.
The value is forwarded verbatim; the peer's resolution stays the peer's (§13.1).

On `GET …/sessions/{id}` the entry machine answers from the peer's *listing*,
which spans all of the peer's runtimes, so forwarding alone would not have
fixed that route: with a hint, the row is the one whose `runtime` is the one
named (`listedSession`).

## Alternatives rejected

- **A `Runtime` field on `SessionRef`.** The ref is an address and is
  serialised into events, labels and records; a hint in it would travel into
  every one of them, and every constructor of a ref would have to decide what
  to put there. The hint is something the caller *said*, not a fact about the
  target.
- **A context value.** The thing `Request` exists to stop: caller-side context
  moved out of band, where the next driver forgets it (`request.go`'s own
  history with authority and corroboration).
- **Appending at each of the fourteen builders by hand with no guard.** It is
  how the hint was dropped in the first place. A test now scans the package for
  a function that builds a `/sessions/%s` path and never calls the helper, and
  fails when the count of such builders and the table of operations disagree.

## Consequences

- A new session-addressed operation in the remote driver must call
  `withRuntimeHint`, last, after its own query; the scan test fails if it does
  not.
- `create` (runtime in the body) and listings (not session-addressed) ignore
  the hint even though the `Request` they receive carries it.
- A peer row that states no runtime stands for a hint only when it is the single
  row with that id; otherwise the call falls through to the peer's own answer.
