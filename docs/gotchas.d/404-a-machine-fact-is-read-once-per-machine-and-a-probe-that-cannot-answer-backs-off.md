# A fact about the machine is read once for the machine, and a probe that cannot answer backs off

**Issue:** #404

## What happened

On a busy host (about a thousand processes, about ninety multiplexer sessions,
a dozen event-stream attachments) the service held roughly one core all the
time, and it held it because of how it watches, not because of any session's
work. Sampled once a second for ten seconds, a whole-table `ps` child was alive
in eight samples, and a login-shell probe appeared beside it. A three-second
stack sample was almost entirely `fork` and `wait4`: the service was starting
children and waiting on them.

Two things were doing it:

- **The process table was read once per `List`.** `List` is not once per
  request: the event stream calls it once per notification burst for every
  subscriber. The table is a fact about the machine, so the cost multiplied by
  subscribers and pane activity, and it grew with the process count, so the
  service got slower exactly when the host was busiest.
- **Probes that could not answer were retried on a short fixed clock.** A login
  shell sources the user's whole startup profile. On a loaded host it can
  outlast its timeout, the answer is then "not known", and "not known" was asked
  again ten seconds later, forever. The same shape on the sandbox measurement: a
  host that cannot be confined (credentials in a store the profile denies) was
  re-measured every thirty seconds, each time starting two login shells and the
  agent CLI.

## The rule

- A fact about the **machine** (the process table) is taken once and shared by
  every listing inside a short lifetime, whoever asks: `processtable.go`. Callers
  that arrive while a read is running wait for that read (and only as long as
  their own context allows); a read that failed because its caller gave up is
  never remembered. Complete (§5.7) still means "no source failed"; a snapshot
  that was read is read.
- A shared snapshot is not trusted for a pane it predates. A live pane whose root
  pid the snapshot does not contain retakes it, but only once the snapshot is a
  second old, so the worst case is one read a second, not one per call.
- A probe whose answer is "not known" waits twice as long after each consecutive
  one, up to five minutes, and a known answer starts it over (`backoffTTL`). The
  caller already reads "not known" as available, so the longer wait costs nothing
  anyone could see.
- A caller that is about to **act** on a probe (a sandboxed create) is not made
  to wait out the background backoff: it gets the short lifetime, because the
  reason for a short negative lifetime is a user who has just logged in.
- A cost that has to be bounded is bounded by **time**, not by a count of
  subscribers or sessions: the number of readers is exactly what the user does
  not control.

## What this does not measure

The acceptance for the issue was a live number (the service under 15 % of one
core over five minutes at that load), not a test. The unit tests prove the reads
are shared and the backoff holds; the CPU figure is a human run on a host under
that load.
