# A process that left the server's group is found by where it lives

A tool a session runs can start a browser with `setsid`. It then leads a session of
its own, so killing the server's process group does not reach it; when its launcher
exits it is reparented to init, and nothing — no record, no pid, no parent — names
it any more (`internal/drivers/opencode/strays.go`).

What is left to go on is the scoped directory every session runs in. Three facts tie a
process to it, matched on the whole path: its command line begins with a path inside
it, it carries `--user-data-dir=` with a path inside it, or its `TMPDIR` is its `tmp`.
A browser started by a Playwright-style tool satisfies the second and usually the
third; the first needs the browser to be installed inside the directory.

Measured while writing it:

- **One `ps -axwwE` is enough.** On macOS it prints every process of the caller with
  its environment after the command line: 0.24 s and about 890 KB for ~1000 processes,
  against one `ps -E` per candidate. The text is matched in memory and dropped. It still
  prints no environment for a protected system binary (see the #398 note), which only
  means the environment fact is unavailable for that process — the command-line facts
  are not.
- **Signal 0 is not "gone".** `kill(pid, 0)` succeeds on a zombie until its parent
  reaps it, and the processes this finds are orphans of init, which reaps on its own
  schedule. Whether a stray is gone is read from the process table again: a zombie's
  command line no longer matches.
- **Signal the pid, not the group.** The leader of a stray's group is a main browser
  process whose helpers are in the same group, but a group is not a proof that every
  member is the session's. Every helper carries the same executable and the same
  inherited `TMPDIR`, so each matches on its own.
- **A process killed with SIGKILL can stay for seconds** when it sleeps uninterruptibly
  (observed: three of four browsers). The wait is a bound; what remains is reported.
- **The service must not stop itself.** A service started from a session's own tool
  shell inherits that session's `TMPDIR`. The sweep skips its own process and every
  ancestor, tested with a process that carries the very fact that matches.
- **Each scan costs about a quarter of a second on macOS**, enough to double the driver
  tests' run time if every close paid it. The driver has an unexported switch for it
  (`noStraySweep`); the shared test helpers turn it off and the tests about the sweep
  turn it back on.
