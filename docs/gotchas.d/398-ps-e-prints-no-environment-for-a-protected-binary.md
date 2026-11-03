# `ps -E` prints no environment for a protected system binary

A server the start-up sweep must stop has no record left to name it, so it is
recognised by its own environment: `TMPDIR` inside a scoped directory under the
session root (`internal/drivers/opencode/orphans.go`). On macOS that environment
is read with `ps -ww -E -p <pid> -o command=`.

Measured while testing it: for an ordinary executable `ps -E` appends
`HOME=… TMPDIR=… PATH=…` after the command line; for an Apple platform binary
(`/bin/sh`, anything under SIP) it prints **only the command line**, with no error
and no empty marker. A test that used a shell wearing a server's argv as its
stand-in found nothing, and said nothing was wrong.

What follows from it:

- A stand-in for a server in a process-table test must be a non-platform binary.
  The tests use the test binary acting as the fake runtime (`spawnStandIn` in
  `reap_unix_test.go`), the same one the driver tests start for real.
- "No matching environment" and "environment not shown" are different answers.
  A line with neither `PATH=` nor `HOME=` was printed without an environment
  (`envHidden`), and the scan counts it as *uninspected*, never as "not ours" —
  the doctor row says so instead of reporting a clean zero. The sweep leaves such a
  process alone.
- `ps -E` shows the environment only for the caller's own processes. A doctor run
  as another user than the service user sees the same command line and nothing
  else; the scan counts those as uninspected too.

Linux reads `/proc/<pid>/environ` instead (NUL-separated), which is readable for
the same user's processes and fails visibly when it is not.
