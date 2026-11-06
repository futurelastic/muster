# ADR: an over-long prompt/text is capped at the HTTP boundary, not inside a driver

**Issue:** #114
**Status:** decided

> Amended by the follow-up at the end of this page (#412): the 1024 default is
> superseded by the owner's decision of 2026-11-06. The cap, the refusal and
> the per-machine override stand; only the number moved.

## Context

A long `prompt` sent to session-create, or a long `text` sent to `input`,
reaches the target composer and then never submits. The session sits at
zero turns holding unsent text, and `resumeIfStranded` cannot recover it
unless the retry sends back the identical bytes. The result is a silent
failure: the API accepts the request, the session is created (or the input
call reports `queued`-shaped hope), and nothing runs.

Field data from three issues, none of them a clean single threshold:

- This issue's own table: a 1645-byte multi-line prompt stranded; a
  212-byte single-paragraph one landed.
- A companion issue's controlled one-line/no-newline sweep against
  `/input`: reliable through 1200 bytes, broke at 1600.
- Another companion issue's creation-path reproduction: ~900-byte prompts
  stranded twice the same day 3833- and 5139-byte prompts landed fine
  elsewhere — naming multi-line shape and host load as confounds, not raw
  size alone.

This issue's own framing settles what the fix should be, independent of
finding the exact boundary: "the ask is not 'make very long prompts submit
reliably' — it is 'stop accepting what cannot be delivered'."

## Decision

**Reject over-length `prompt`/`text` at the HTTP handler**, before any
driver is resolved: `internal/service/http.go` gained `maxInputBytes = 1024`
and `rejectOverLength(field, text, machine) *fleet.Error`, called from
`handleCreateSession` against `prompt` and `handleSendInput` against `text`.
Both return `invalid` (400), naming the field, the limit, and the caller's
actual size.

**1024 bytes is a conservative default, not the bisected true boundary** —
this issue explicitly leaves that bisect (including whether line count
matters independently of byte count) as open work. 1024 sits inside the one
controlled, single-variable measurement available (the `/input` sweep
above) with headroom below its own failure point, and is well under the
"detailed briefing material" case this issue itself says belongs in a
reference the agent reads deliberately, not a pasted composer.

## Alternatives considered

**A guard inside `internal/drivers/tmux/inputguard.go`.** This looked like
the natural home going in — it is exactly this shape, a fail-closed refusal
seam checked before delivery (see #53, the same file's existing bash-mode
refusal). Rejected on inspection, for two independent reasons:

- It is wired into `Send` only, i.e. the `/input` path. Session-create's
  prompt delivery is a separate code path inside the tmux driver's own
  `Create` and never calls `Send` — a guard there would silently cover only
  `text`, not `prompt`, defeating half of this issue's explicit ask ("apply
  it to both the session-create `prompt` field and the input route's `text`
  field").
- It is tmux-only. Other drivers registered in this service (opencode,
  remote) would ship with no protection at all, when the failure this issue
  describes ("reaches the composer and never submits") is a property of
  pasting into any interactive runtime's input surface, not specifically
  this driver's multiplexer.

**A per-driver capability/limit, negotiated through `driver.Capabilities()`.**
Not pursued: this issue's own proposed fix is stated as boundary enforcement
("Enforce a documented maximum prompt/text length at the API boundary"), and
a capability-negotiated limit would still let an over-length request reach
at least one driver's `Create`/`Send` before being refused, reintroducing
the timing/ordering questions #112 already had to solve for a different
guard. A flat, undifferentiated cap enforced once, for every driver, is
simpler and matches the issue text directly; if a future driver genuinely
tolerates longer input, that is a reason to widen `maxInputBytes` (or make
it driver-aware) with its own evidence, not a reason to default to
per-driver limits now on no evidence at all.

**Precisely bisecting the failure boundary before shipping any cap.** Not
done here — the confounds named in Context (multi-line shape, host load)
mean a clean bisect needs its own controlled measurement session, which
this issue defers explicitly. Shipping a conservative, documented, easily
adjusted constant now removes the far more common failure (someone pastes
kilobytes of context) without waiting on that measurement.

## Consequences

- A caller sending more than 1024 bytes in `prompt` or `text` gets an
  immediate, actionable 400 instead of a stranded session discoverable only
  by polling `promptDelivery`/`state.turns` and giving up.
- The limit is identical for `prompt` and `text` even though the field data
  suggests the creation path may be more fragile at a given byte count than
  `/input` is (#112's root-cause finding: composer readiness at create time,
  not size alone, is the dominant factor there). Splitting the limit by
  field would need its own evidence; today's single constant is the
  simplest correct response to "both fields exhibit this."
- `docs/spec/api-http.md` documents 1024 as the enforced number on both
  endpoints — a future session tightening or loosening it from real bisect
  data updates one constant and this doc, not a driver-specific pattern
  list.

## Follow-up (2026-11-06, #412) — the default is 32768

**The 1024 default above is superseded.** The owner decided on 2026-11-06 to
raise the default `maxInputBytes` to **32768**, on field evidence this ADR did
not have:

- On a fresh install, a client that opens sessions with a real briefing prompt
  (8–15 KB) had about 140 creates refused in 30 minutes with
  `400 … over the 1024-byte limit`. Sessions opened by hand with short prompts
  worked, so the client looked switched on and nothing happened.
- Machines whose operators had set `maxInputBytes: 32768` by hand had run that
  way for a long time without the stranded composer this ADR was written to
  prevent. That is the bisect this ADR left open, answered by operation rather
  than by a controlled sweep.

What does not change: the cap sits at the HTTP boundary and is checked before
any driver is resolved; the refusal is still `invalid` (400) naming the field,
the limit and the caller's size; `prompt` and `text` still share one number; the
setting is still a per-machine override read at start, validated by
`muster doctor`, and it still moves in both directions — a configured 1024
refuses a 15 KB prompt exactly as before. The install writes no value, so a
machine follows the default instead of a number it was handed once.

The "conservative default" reasoning in Decision and Consequences above is kept
as the record of what was known then. `docs/spec/api-http.md` and
`docs/client-guide.md` name 32768.
