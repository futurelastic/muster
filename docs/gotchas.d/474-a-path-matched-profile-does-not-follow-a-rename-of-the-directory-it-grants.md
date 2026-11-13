# A path-matched sandbox profile does not follow a rename of the directory it grants

**Issue:** #474

## What happened

A session in isolated mode gets its profile planned when it starts, from the
**real path** its working directory resolved to then. Later, the directory's parent
folder was renamed and a symbolic link was left at the old name. The session's
process kept using the old name; every access through it now resolved to the new
real path, which the profile does not name, so each one was refused. The runtime
answered 500 to every request from then on, and the session was listed `unknown`
with the runtime's bare text.

Measured on one machine, two sessions: about 4,300 refused `lstat` calls in the
first ten minutes, starting the minute the folder was renamed. Nothing said why.

## The rule it produced

- The service remembers, for each session it starts under a path-matched profile,
  the directory as the process was given it and the real path the plan granted
  (`server.cwdGiven`, `server.cwdPlanned`). Memory only: every server is built from
  a fresh plan, because a restarted service never takes over an old process.
- When a read of that session's runtime fails **with the runtime's own refusal**, the
  service resolves the directory again. A different real path is reported as *the
  working directory moved* with both paths; a path that no longer resolves is
  reported as gone. Nothing is checked while reads succeed, and an unreachable
  runtime or a credential problem is never blamed on a move.
- It compares the real path at plan time with the real path now, never the path as
  given with either, so a working directory that is a link on purpose, or a temp
  root behind a system link, is never reported.
- Every start already plans again from where the directory is now (a relaunch, a
  revive after a park, the service starting over its records), so a restart is the
  repair. The Linux mechanism makes each grant by opening the directory, so its grant
  follows the rename and there is nothing to report there.

## Not done, on purpose

The service does not restart such a session by itself, and has no restart route. A
session whose status cannot be read is not known to be idle, and stopping a process
mid-turn is what parking refuses to do; automatic recovery is a product decision for
the owner, not a side effect of this fix.
