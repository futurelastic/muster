# 449 — a listing reads a pane only when the multiplexer says it moved

## Context

Every listing read every pane's screen: one invocation carrying a capture per
pane, then a classification of each. On a host holding about seventy sessions,
most idle for hours and some for days, that kept the service at between a third
and a half of a core, and under load one listing took 16.6 s. The cost of a pass
grew with the size of the fleet, not with what it was doing.

The ask was a back-off ladder: read a session less often the longer it stays
quiet (1 s, 5 s, 30 s), back to 1 s on the first change or on input, with the
steps configurable and a default that keeps an attended session's
waiting-for-input detection as fast as before. It also named sessions left
`unknown` ("needs re-creation") as ones to stop reading.

Two things in the code decided the shape of the answer.

- There is no per-session capture timer to back off. A listing is one pass over
  the whole fleet by design: observation memory, stall verdicts and identity
  reconciliation all read the whole set, and the event stream's pacing already
  rejects "re-read only some panes" as a second path through them, free to drift.
  The ~1 s cadence is the stream's relisting plus the HTTP callers.
- The multiplexer already reports when a pane produced output, in the listing
  every pass makes anyway: the window's last-activity second, and the pane's size,
  cursor and scrollback depth. A ladder has to guess when a quiet pane woke up.
  This does not.

## Decision

A listing stays whole — the same rows, the same classification, the same
observation memory — and changes only where one input comes from. For each pane,
the screen is **served from the last capture** when all of these hold, and read
otherwise:

- the activity fingerprint is the one the last capture was taken under;
- the pane's last output is older than the idle bound, and that capture is
  younger than the ceiling;
- the pane is alive and past its start-up window, and nothing was sent to it since.

The fingerprint's activity field has one-second granularity, so a capture is
trusted only when the pane's last output was strictly earlier than the second of
the listing that took it: output after that listing has a later second, so it
moves the fingerprint. Detection of a quiet pane waking is therefore the next
listing — the same speed as before, which no ladder step could promise.

Bounds, because the signal is the multiplexer's and not this service's:

- **A ceiling** (default 30 s) on how old a reused screen may be, spread per pane
  between half of it and all of it so a fleet cached in one pass is not read again
  in one pass.
- **A self-check.** A read forced by the ceiling that finds the screen different
  although the fingerprint held means the signal missed output on this host. It is
  counted (`capture.reuse_missed`), logged once, and reuse is off until restart.
  Amended by [ADR 472](472-a-suspected-capture-miss-is-judged-by-the-next-listing.md): the
  verdict waits for the next listing, and one pane costs one pane, not the host.
- **No reuse** for a dead or starting pane, one listed without the fingerprint
  fields, or after any input this driver sent to it (one wrapper around the
  runner, because input goes out from more than twenty places).

Only `List` reads through the cache. Every verb that acts on a screen — a send, a
response, a discard, a key — captures fresh.

Configuration is `FLEET_CAPTURE_IDLE_AFTER` (default 2 m) and
`FLEET_CAPTURE_IDLE_MAX` (default 30 s); `0` or `off` on either reads every pane
every pass. The library default is off, so a driver built for a test behaves as it
always did; the composition root turns it on.

## What the ask asked for that this does not do

- **A ladder.** Rejected for the reason above: it adds up to its top step to the
  time a woken session is noticed, for no saving the exact signal does not give.
- **Stopping reads of `unknown` / needs-re-creation sessions.** That class belongs
  to the opencode runtime, which has no pane and never captured a screen. Its
  listing already says "listed, not running: nothing to read" for such a session
  and makes no request for it; a test now pins that. The multiplexer driver has no
  such class — a pane whose read failed is read again on the next pass, which is
  right, since an absent screen is not a session that needs re-creating.

## Consequences

- An idle session costs no read between two passes, so the service's cost follows
  the active share of the fleet. The pass's other work (the pane listing, the
  process table, conversation records) is unchanged and is the next cost to look
  at.
- A host whose multiplexer does not report the fingerprint reads every pane every
  pass, as before.
- Callers that want only the rows (`enumerate`) still read and discard every
  screen. They run at startup, on reattach, and per request, not on a cycle, so
  they were left alone.
- Proven against a real multiplexer (`TestLiveActivityFingerprintMatchesOutput`,
  opt-in with `FLEET_TMUX_INTEGRATION=1`): a quiet pane's fingerprint holds and its
  listings read nothing; the first listing after it prints reads it once and
  reports what it printed.
