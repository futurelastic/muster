# A wedged runtime store is not a release regression until the rollback's own servers say so

**Issue:** #515 (cause still open — this records what was measured, so the next
session starts from it and does not bisect the same six commits again)

## What happened

On one macOS (Intel) host, runtime servers started answering 500 on every
store-backed route with `SQLiteError: disk I/O error`, and never recovered. The
first error came about 8.5 h after a release was installed, so the release's
commits were the suspects and the service was rolled back to the previous version
as the experiment ("no error in about 24 h means the cause is in the range").

The experiment was already answered within minutes of the rollback, and the
service log did not show it: it recorded no "runtime answered 500" line in the
first fifty minutes after the restart, while the runtime's own log in each
session directory held dozens per server.

## What was measured

- **The rollback is red.** The service was rolled back at 10:12. The servers it
  started afterwards, in session directories it created itself, logged the error
  from 10:16:57 on: every one of the 14 it started between 10:13 and 10:20, in
  two bursts (10:16:57 and 10:23:05), each server's first error within about a
  minute of its burst. The servers started after the second burst logged nothing
  (eight of them by 11:05; the oldest had run 42 minutes). So the cause is not in
  the range between the two releases.
- **A wedge is per server and sticky.** The service polls a wedged server about
  every 30 s and logs one failed read each time; none recovered.
- **The data is intact.** The wedged server still holds the same inodes (store,
  write-ahead log, shared-memory file), and a separate read-only reader gets the
  rows back and `quick_check` says ok. The fault is in the wedged process's own
  access to files it already has open.
- **The last good write is often a tool starting.** Of five servers whose stores
  were read, three ended on a `tool … pending` part and their first failed read
  came 47 to 60 ms later; one ended on a step-finish and one on a text part.
  Servers that were idle at a burst had no write to line up with. The bursts are
  not one instant: two servers in the first burst wrote successfully 0.3 s apart
  on either side of another server's first failure.
- **Only the muster-started servers showed it.** The machine's own default runtime
  instance (no isolation, default directories) and another SQLite user on the host
  have no such error in their logs.

What did **not** reproduce it, run on the same host:

| trial | result |
|---|---|
| 30 plain SQLite WAL writers, 90 s | 0 errors |
| 20 writers inside a profile shaped like the isolation profile plus 20 outside it, 120 s, with six tree walkers running | 0 errors in both |
| a full-speed delete of a 150 000-file tree beside them | 0 errors |
| a real runtime server, started by hand inside the same kind of profile, and one outside it, about 450 and 500 `session/:id/shell` spawns each | 0 errors |
| closes | not it: the second burst has no close in flight, and closes had run dozens of times an hour for ten hours before the first error |

Ruled out on the host: disk space, SSD health (SMART verified), memory (84 % free),
file-descriptor and process limits. The vnode table is saturated
(`kern.num_vnodes` equals `kern.maxvnodes`) and the load average is about 13; nothing
yet ties either to the error.

## The rule it produced

1. **Read a rollback from the servers the rolled-back service started, in the
   runtime's own log**, not from the service log. For a session directory:
   the first `disk I/O error` line in `xdg/data/opencode/log/opencode.log` against
   that server's start time.
2. **Tell a wedge from a regression by the store, not the story.** The last write
   in a wedged store (`select max(time_updated) from part`, opened `mode=ro`)
   against the first error in the same server's log says what the server was
   doing when it broke.
3. **The missing fact is the errno at the instant.** Arm, before the next burst,
   a plain and an isolated canary that log SQLite and raw file operations with
   their errno, and `fs_usage -w -f filesys` on the runtime processes keeping only
   failing calls other than `ENOENT` (it needs root). The next burst then names the
   failing call; guessing from the symptom did not.

## Not done, on purpose

No code changed. Nothing measured implicates a line, and a patch aimed at the
isolation profile, the deleter or the walker would be a guess the trials above do
not support. The auto-replace of a wedged server (#513) is the mitigation that
exists; it does not explain the wedge.
