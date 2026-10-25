# 368 — the launch prompt follows a live delivery lane, and every send is logged with its route

**Issue:** #368
**Status:** decided. Supersedes the create-time-prompt pin in ADR 184 (the
paragraph "The create-time prompt is pinned to the terminal, on both attempts"
and ratification item 3) and the create-time prompt's exemption from the live
lane in ADR 257 (§Shape), for the **first attempt**. The retry stays pinned.

## Context

A session created with a `prompt` has the prompt typed into its composer by this
service once the runtime paints one. ADR 184 pinned that delivery to the terminal
on both attempts, for two reasons: the inbox would have made the creator's
instruction a peer message, with a peer's authority, and the retry
(`resumeIfStranded`, a terminal operation) could re-send text the inbox had
already put in front of the receiver.

Both reasons were about the inbox, and ADR 257 removed it from `auto`. What was
measured since, on one fleet: no running session ever left its delivery lane; the
only sends that did not use it were this service's own typing (the launch prompt,
the title sync, the remote-control command) and input in a session's first one to
three seconds. In one observed case the lane read live about a second after
create, the launch prompt was typed into the composer regardless, stranded there
("initial prompt still unsent after one retry"), and a person had to discard and
send it again about three minutes later. The terminal is the fragile surface, and
the lane was up.

The second problem was separate. The logs held only totals
(`route.decided.<requested>.<taken>`, `fallback_to_builtin`), so a send that fell
back from the lane to the terminal could not afterwards be traced to its session.

## Decision

### 1. The first attempt is `auto`

`deliverInitialPrompt` sends the launch prompt with `route: auto`. If the
session's lane is live at send time, the lane carries it and the receipt names the
module. If it is not — no lane was ever offered, the lane is degraded, or the
module declines before writing a byte — the built-in path carries it, exactly as
before. This is the whole of "fall back to the terminal only if the lane is not up
at send time".

`LiveLaneOnly` stays **false** for the launch prompt, as ADR 257 left it. That flag
makes a live lane the only path and refuses instead of falling back; a creator's
launch instruction must not be refused because a lane lost its liveness between the
check and the write. A module step that handed the send back wrote nothing, so the
terminal carrying it cannot deliver it twice.

### 2. What the pin protected is kept, in two places

- **The retry stays pinned to the terminal and stays a resume.** A resume is
  composer-shaped, so a lane is never offered it, even one that has come back
  since the first attempt. It can only finish what the terminal's own stranded
  record holds.
- **A launch prompt the lane carried is never retried.** If the lane wrote it and
  could not confirm it (`unknown`), the module ledger already holds that text for
  that sender. A second Send would write nothing on either path and answer what the
  first one did ("NOT been sent again"), and the composer holds nothing to resume.
  The prompt is resolved `unknown` with the lane's own reason, for a person to read
  the transcript. `initial_prompt.delivery_lane_unconfirmed` counts it; it is
  neither `delivery_retried` nor `delivery_stranded`.

Together these keep ADR 184's property: the launch prompt is delivered by at most
one path per attempt, and a retry cannot deliver text a lane has seen.

### 3. Every send is logged once, with its session

Each `Send` writes one line after it finishes:

```
tmux: send session=<id> machine=<m> requested=<route asked> route=<route taken> outcome=<o> fallback=<bool> reason="…" [detail="…"]
```

- `route` is `terminal`, `inbox`, `module/<name>` (a delivery lane), `none` (the
  send was refused before any path) or `error`.
- `fallback=true` means a lane was offered to this session and handed the send back
  with nothing written, so the built-in path carried it. A session that never had a
  lane, and a shape no lane can take (a forced terminal, `submit: false`, a resume),
  are `reason`s but not fallbacks.
- `reason` is why the send was on that path; for a fallback it is the module's own
  decline text. `detail` appears only for a refused or unknown send and carries the
  receipt's words.
- The message text and the sender are never logged. Reason and detail are
  sanitised and cut at 200 characters.

A log line, not a new field: the ask is "the logs", an API field would be a spec
change, and the counters already answer "how many". The line answers "whose".

## Consequences

- **A launch prompt now reads `route.decided.auto.module` on a machine with a live
  lane**, and `route.decided.terminal.terminal` only for the retry. A dashboard
  that counted launch prompts under `terminal.terminal` will see them move.
- **Sessions with no lane are unchanged.** `d.mods` nil, or a session that was
  never offered a lane, takes the terminal as before; the first attempt's counter
  key changes from `terminal.terminal` to `auto.terminal`.
- **One line per send.** Volume follows `/input` traffic plus this service's own
  typing, not the poll rate. If it turns out to be too much to keep, the line is
  the only thing to gate; the counters are untouched.
- **A launch prompt written to a lane and unconfirmed is reported `unknown` after
  one attempt**, where the terminal would have tried a second time. That is the
  honest answer: there is no stranded text to finish, and a second write is the
  double delivery the ledger exists to refuse.

## Alternatives rejected

- **Wait for the lane to come up before sending.** The lane can be absent for good
  (a session this service did not launch, a module that is down), and a bounded
  wait only moves the stranded case. The ask is to use the lane when it is up.
- **`LiveLaneOnly` for the launch prompt.** Refuses a creator's instruction on a
  race between the check and the write, where the terminal could have carried it
  safely.
- **Retry a lane `unknown` on the terminal.** Types text the lane may already have
  delivered. The ledger refuses it; the honest outcome is `unknown`.
- **A ring buffer or a `state` field for the per-send record.** Neither was asked
  for, and each is a surface to specify and keep.
