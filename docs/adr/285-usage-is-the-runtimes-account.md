# ADR: token usage is the runtime's account of itself, summed, and per-turn figures ride on the state

**Issue:** #285
**Status:** decided by the implementing session; reversible (see *Consequences*)

## Context

A consumer wanting activity and cost for every session, whatever its engine, had
to read each runtime's private records itself. Both drivers' runtimes already
write the numbers: the tmux driver's record carries per-request token counts, and
the opencode runtime reports tokens and cost on each step. None was exposed.

The issue asked for a cumulative block, "the same figures for the turn on the
turn-end event", and a capability field. There is no turn-end event: `TurnEnd` is
only `SessionState.lastTurn`, and an event kind is a closed set every client has
to handle.

## Decision

1. **`state.usage`** — input, output, cacheRead, cacheWrite, and `reasoning` /
   `cost` only when the runtime reports them, with `source` and `asOf`.
   `DriverCapabilities.reportsUsage` makes an absent block answerable. Absent is
   "not yet known", never zero; nothing is priced by this service (§5.8: only
   what the runtime reports).
2. **Always the sum.** Usage is per request and a turn is many; the last request
   understates a run 2–20×. The tmux reader dedupes by message id (one message is
   written as several entries, each repeating its usage: 219 entries for 82
   messages in a measured record). The opencode reader sums the per-step records,
   and uses a message's own figures only where it has no step records.
3. **Per-turn figures are `usage.lastTurn` on the state, not a new event.** A turn
   ending is a `session.state` event (material change) that carries them beside
   the cumulative block; a poller reads the same field. No new closed event kind,
   nothing for a relay to learn, nothing that can be missed between polls.
4. **Incremental.** tmux keeps a byte-offset memo per record; a first read of a
   very large record happens in the background and the block is absent until it
   lands. opencode splices a short tail onto a remembered history, widening the
   window until the two overlap.

## Consequences

- Usage is material: a consumer summing off the feed hears every completed
  request. `asOf` alone is not a change.
- `reasoning` is *additional* to `output`. The tmux runtime counts reasoning
  inside output, so it leaves `reasoning` absent rather than double-counting.
- If a dedicated turn-end event is wanted later, it is additive: `lastTurn` can
  keep riding the state.
- opencode `List` reports what a State read or a turn's end last learned and makes
  no request of its own (one status read per server stays the rule); its `asOf`
  says how old that is.
