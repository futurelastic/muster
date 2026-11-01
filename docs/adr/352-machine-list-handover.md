# ADR: a machine list is handed over under its own grant, which a signed list cannot give

**Issue:** #352
**Status:** decided (the first decision below is the owner's ruling; the rest are
the technical choices it left open)

## Context

The owner-signed machine list (#351) is delivery-agnostic: the signature makes
the channel untrusted, so a file copied into place and a list handed over by a
peer are the same thing to a verifier. Only the file existed. A route that lets a
peer replace the machine list is a route that replaces the trust root's peer
table and the machine principals in it, so who may use it, in what order the
checks run, and what it may overwrite are the whole design.

## Decision

1. **Serving is `read`; accepting is a new grant, `push-list`.** Reading a signed
   list reveals nothing a stronger grant would protect. Accepting one needs a
   grant of its own that nothing implies: not `send` or `relay`, not the legacy
   single-token flags, and not what `muster service install` gives the local
   service (the supervisor set excludes it). A push from a principal without it
   is refused before its body is read. *(Owner ruling.)*

2. **A pushed list replaces the current one, hand-placed or not, only if it
   verifies against the signer pinned on this machine and its `seq` is strictly
   higher than the highest accepted.** Equal is refused. It is written atomically
   and put in force through the reload #354 added, with no restart. *(Owner
   ruling.)* "Highest accepted" is one number, `highestAccepted()`: the higher of
   the mark in the state directory and the sequence of the list in force. The
   file reload uses the same function, so a file and a push cannot disagree about
   what is newer.

3. **A signed list cannot give `push-list`.** A list naming it is refused whole,
   like an unknown grant. The ruling says the grant is "given per peer by the
   operator", and `docs/machine-list.md` already says the document cannot add to
   the trust root; who may hand a machine its next list is the same kind of local
   decision as the pinned signer. The cost is that in `prefer` and `only`, where a
   listed machine replaces a hand-written principal of the same name, an operator
   could never grant it to a listed machine; so that one grant is carried across
   from the hand-written principal **when the public key is the same**, and lapses
   (with a `muster doctor` warning) when the list gives the machine a different
   key. *Alternative not taken:* let a list name its pushers. It reopens a
   compatibility hazard (a build without the grant refuses any list that names it)
   and lets the owner's key widen the set of machines that may replace lists
   everywhere at once. If the owner wants it, only `machinePrincipals`,
   `principalsFor` and their tests change.

4. **A signed request is verified from its headers on this one route.** Today the
   authenticator reads a signed request's body (up to 8 MiB) to digest it before
   any handler runs, which would defeat "refused before the body is read" for the
   machines that always sign. `peerkey.VerifyHeaders` verifies the signature over
   the digest the sender *claims* (inside the signed text, so the claim is
   authenticated) and `peerkey.CheckBody` compares the real body afterwards,
   before any byte is acted on. Only `PUT /v1/machine-list` defers; `Verify` and
   every other route are unchanged, and a test pins that.

5. **The push route is not `mutating()`.** In the legacy model `mutating()` lets
   every caller in when `FLEET_ALLOW_MUTATIONS` is set; a push must never be
   allowed without a per-principal grant, so a machine with no principal table
   refuses every push.

6. **The answer is binary.** Every step that can refuse (verification, the
   sequence, building the new peers) happens before the file is touched; a failure
   to record the sequence restores the previous file. `200` means written and in
   force; anything else means nothing changed. One path, `swapIn` (the tail of the
   reload's `apply`, moved verbatim), puts a list in force for a file and a push.

7. **Only the configured path is written.** The path is restart-only; a file
   written anywhere else would never be read, so "accepted" would be false.

## Consequences

- A push can bring forward any genuine newer list, including one that no longer
  names the pusher or empties the peer set in `only`. It cannot change the pinned
  signers, the mode or the path, and never touches principals that are not
  machines.
- A list signed far ahead in `seq` raises the mark for good.
- A config that names `push-list` is refused at start by a build that does not
  know it: grant it only after the machine runs a build that does, and revoke it
  before rolling back.
- The service never pushes or pulls on its own; `muster machine-list push` and
  `fetch` are commands someone runs.
