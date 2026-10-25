# 257 — a live delivery lane is a session's only input path, and an absent `submit` means true

**Issue:** #257
**Status:** decided. Supersedes the entering-side rewrite in ADR 184 and the
routing table and `TerminalFromAuto` paragraph in ADR 185. Resolves ADR 185's
"needs ratification" items on the inbox order and on a human relay crossing a
peer.

> Amended by [368](368-the-launch-prompt-follows-a-live-lane.md): the create-time prompt below is still exempt from `LiveLaneOnly`, but its first attempt is now `auto` and goes through a live lane instead of the terminal.

## Context

A caller sent `POST …/input` with no `route` and no `submit` to a session whose
delivery lane was live. The receipt said `placed in the composer, not submitted`
and the text sat unsent: an absent `submit` decoded as `false`, and with
`submit: false` neither the lane nor the inbox is eligible (the composer is not
free), so `auto` landed on the terminal and did exactly what it was asked.
Forcing the lane by name was refused with a 400 asking for a `from` label that
`auto` would have supplied itself.

Measured the same day on one fleet: 94 of 100 live sessions held a live lane.
Since the last restart on one machine, agent `auto` sends went to the lane 55
times and to the terminal 63 times, and the inbox was tried 105 times and
delivered none. The reliable lane existed and covered almost every session, but
the default contract sent more than half of agent sends away from it.

## Decision

Five rulings, two from the maintainer (direction) and three technical.

1. **While this service runs, a live lane is the session's only input path.**
   For a caller's `/input` on a session whose lane is usable right now, the
   lane delivers or the send is `refused` with nothing written. It never goes to
   the terminal. The terminal path remains for a session with no live lane.
2. **`auto` tries the lane first for every sender, and the inbox only when a
   caller names it.** The inbox delivered nothing here, and agent messages would
   have silently become peer messages the day it started working.
3. **An absent `submit` means `true`.** An explicit `false` keeps its meaning.
4. **A human relay\'s `auto` crosses a peer as `auto`**, with the existing
   human-relay assertion; the machine that owns the session decides. The
   entering machine no longer rewrites it to `terminal` and the
   `TerminalFromAuto` special case is gone.
5. **A forced `terminal` or module route from a caller with no printable `from`
   and no human-relay grant is labelled with the authenticated principal**, as
   `auto` already was, instead of being refused with a 400. A human relay stays
   unlabelled. The #180 M8 guarantee (an agent\'s send is never recorded as
   human-typed input) holds because the label prints.

### Shape

- The lane can only be seen on the machine that owns the session, so the refusal
  lives in the driver. Which calls are a caller\'s `/input` is known only to the
  service, so it sets `driver.SendOptions.LiveLaneOnly` on every `/input`, never
  from a body and never forwarded to a peer. The create-time prompt, the title
  sync, `/discard`, `/keys` and `/respond` leave it false. Since #368 the
  create-time prompt is sent as `auto` and a live lane carries it; the others keep
  the terminal.
- With `LiveLaneOnly` and a live lane, `route: "terminal"` (from anyone),
  `submit: false`, `resumeIfStranded` and `replaceIfStranded` are refused before
  any write, with a reason naming the lane. Text this driver itself left stranded
  in the composer is refused too (sending it through the lane as well would
  deliver it twice), pointing at `discard`. A lane that read live at the check
  and declined before a byte was written is refused ("send again") rather than
  handed to the terminal; the failure that degraded it makes the next send see no
  live lane. Counters: `route.refused.lane_live` (+ per shape),
  `route.refused.lane_lost`.
- `route.auto_fallback` is retired: nothing falls back from the inbox any more.

## Consequences

- Two kinds of caller see a behaviour change (a minor bump while on 0.x): one
  that relied on an absent `submit` to stage text, and one that asks for the
  terminal, `submit: false`, or a resume or replace on a session with a live lane.
- **Deploy order.** A caller that answers a refusal by driving the multiplexer
  directly must be changed first, or each new refusal becomes a raw keystroke
  into the same session. That caller is outside this repository.
- **Mixed-version peers.** An entering machine built before this change still
  sends a human relay\'s `auto` as an explicit `terminal`; a new owner refuses it
  on a live lane. No shim: reinterpreting an explicit `terminal` would break the
  rule that an explicit request is never quietly changed. Upgrade peers together
  and watch `route.refused.lane_live.terminal`.
- On a live lane, `resumeIfStranded` and `replaceIfStranded` have no consumer.
  Recovering stranded composer text there is `discard` (with `expect`) followed
  by a fresh send. The stranded-record mechanism itself is untouched.

## Not in scope

Lane coverage (sessions not launched by this service, or whose agent process was
replaced); enforcement outside this service of agents typing into the
multiplexer; the stranded-record mechanism.
