# 472 — a differing read is a suspect until the next listing agrees

## Context

[ADR 449](449-an-idle-pane-is-served-from-its-last-capture.md) lets a listing
serve a quiet pane from its last capture, and bounds the harm of a wrong signal
with a self-check: a read forced by the age ceiling that finds the screen
different although the fingerprint held means the multiplexer missed output, so
reuse is switched off until restart.

On a host holding well over a hundred sessions that check fired three times in
two and a half days, once on each of three process lifetimes. After the last one
every pane was read on every pass again (a pass was two or three chunks of 64
panes), and when the host's load rose the capture budget ran out: hundreds of
listings an hour came back `complete: false` with the multiplexer source
`degraded`. The reported symptom was a budget that is too small; the cause was a
saving that had been switched off by a false alarm.

The alarm was false because of when the two sides of the comparison were taken.
The fingerprint comes from the listing; the screen it is compared with comes from
a read that follows the listing. They are two moments, and on a loaded host the
listing alone has been seen to take 2 to 3.7 s. Output that arrives between them
changes the screen under a fingerprint that has not moved yet. That is a race
inside the check, not a signal that missed output, and the next listing would
have shown the fingerprint moved.

## Decision

A differing read is only a **suspect**. It is recorded against the fingerprint it
was compared with, and the next listing settles it:

- the pane's fingerprint has moved: the output is explained, nothing was missed.
  Counted as `capture.reuse_raced`, and nothing else changes;
- the pane's fingerprint is still the cached one: nothing explains the change, so
  the signal missed output on this pane. Counted as `capture.reuse_missed`,
  logged once by pane id, and **that pane** is read on every pass for as long as
  it lives. Every other pane keeps its saving;
- **three** distinct panes shown to miss switch reuse off for the host until
  restart, as ADR 449 did for one. Several panes disagreeing with their
  fingerprints is what a multiplexer that does not report output looks like; one
  pane is a pane.

A suspect whose pane has gone, or whose listing carried no fingerprint, is
dropped unjudged. The read that found the difference is cached as usual: it is the
screen as it was read, and the fingerprint comparison on the next pass decides
whether it is still current.

## What this does not do

- **Scale the capture budget with the number of panes.** Contradicted by the
  measurement behind the per-invocation slices (#156): failures were seen on
  machines with three or four panes, so pane count is not the lever.
- **Serve a stale capture for a pane the pass could not read, and mark it.** It
  would change what a session state means (a screen of unknown age behind a
  state), and it is not needed once the saving stays on: a pass then reads only
  the panes that moved.
- **Report identity and source health apart from capture completeness.** Already
  so: `/v1/machines`, `/v1/health` and the `sources[]` of every listing answer it
  without reading a pane.

## Consequences

- One race no longer costs the whole saving. The check still catches a
  multiplexer whose activity signal is wrong, one pane at a time, and the host
  whose signal is wrong everywhere switches itself off after three.
- A real miss is noticed one listing later than before (the verdict waits for the
  next listing), during which the pane's screen is at most one ceiling old, the
  same bound ADR 449 already gives.
- `capture.reuse_raced` is on record, so a host where the race is common can be
  told from one where it never happens.
