# ADR 306 — Windows: a kill-on-close Job Object for the process tree, a protected access list for owner-only files

**Status:** decided and measured on a hosted Windows runner while implementing #306.
**Issue:** #306 (the platform layer of the Windows port, #303).
**Related:** ADR 303 (Windows ships without a native interactive-terminal driver).

## Context

Two guarantees that were free on Unix had to be rebuilt for Windows:

1. **Stop a child and everything it started.** The subprocess driver must not
   leave a session's tools running after the session closes (#280), and the
   module launcher must not leave a module's descendants running when it is
   killed. On Unix that is a process group and a signal.
2. **Owner-only files.** Tokens, the principal-table backup, the state directory
   and every session's own directory were created with POSIX modes. Go reports
   and honours no such mode on Windows, so the same calls silently produced
   objects that inherited their parent's access list.

## Decision

### Process tree — `internal/procgroup`

One package, used by both callers. Unix keeps `Setpgid` and signalling the
group, byte for byte. Windows:

- The child is created `CREATE_SUSPENDED | CREATE_NEW_PROCESS_GROUP`, assigned
  to an unnamed Job Object created with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`
  and **no breakaway**, and only then resumed. Nothing it starts can leave the
  job, because it could not run before the job held it.
- `os/exec` closes the primary-thread handle `CreateProcess` returns, so the
  suspended thread is found again with a Toolhelp32 thread snapshot,
  `OpenThread` and `ResumeThread`. The process handle comes from
  `os.Process.WithHandle`, which also means the pid cannot have been reused.
  Alternatives rejected: attaching after an unsuspended start leaves a window in
  which a grandchild escapes; the process-attribute route for assigning at
  creation is not reachable through `os/exec`; calling `CreateProcess` ourselves
  means re-implementing its pipes, `WaitDelay` and stderr copying for both
  callers.
- Any failed step terminates the job, closes it, waits for the process and
  fails the start, so a caller only returns the error.
- **There is no graceful stop.** Children share no console with a service, and a
  stop that depended on how the service was launched would be worse than none.
  `Graceful()` is false, so the driver's stop goes straight to terminating the
  job and waiting (bounded) until it is empty — a file a dying process still
  holds cannot be removed on Windows.
- The job handle belongs to the service. A service that dies for any reason
  takes its servers with it, so the Windows reap-on-start returns false **by
  invariant**, not by omission.
- Win32 is reached through `syscall.NewLazyDLL` on `kernel32.dll` and
  `advapi32.dll` only (both KnownDLLs). `JOBOBJECT_EXTENDED_LIMIT_INFORMATION`
  is pinned at 144 bytes by a test.

Nested jobs (Windows 8 and later) are assumed: a scheduled task and a CI runner
both start processes inside a job, and assigning the child to a new, empty job
forms a valid hierarchy. Measured: assignment succeeds on the hosted runner.

### Owner-only files — `internal/privfs`

`MkdirAll`, `MkdirTemp`, `WriteFile`, `OpenExcl`, `CreateTemp`, `Check`. Unix is
exactly the previous calls. On Windows a protected list naming only the current
user (`D:P(A;;FA;;;<sid>)`, with `OICI` on directories so what a stranger
process creates inside inherits it) is applied **at creation**, through the
security attributes of the create call — there is no moment at which the object
exists with the inherited list. An existing file is fixed in place with
`SetNamedSecurityInfoW` before it is truncated.

`Check` reads the list back, renders it as SDDL and judges it with a pure
parser that is tested on every platform. It decides by **trustees**, not flags:
an entry inherited from an owner-only parent is as private as one set directly.
The doctor's `state.dir` row uses it and says `unknown` when it cannot read the
list — never a pass it did not earn.

## Consequences

- Behaviour that differs from Unix, stated: no graceful stop; a service crash
  ends its sessions' servers instead of leaving them to be reaped; names in the
  session environment are case-folded and a collision is refused.
- Module discovery judges POSIX mode bits, which Windows does not have. There
  it asks only for a regular, non-symlink file and **does not check who can
  write it** — a follow-up (read the file's list, as `privfs.Check` does).
- The Windows CI job runs an allowlist (`scripts/gate-windows.sh`: procgroup,
  privfs, the subprocess driver, modclient, state, and `cmd/muster` since #311),
  prints every skip, and fails if a named must-run test did not PASS. Widening
  it further is a follow-up. In `cmd/muster` the tests that cover a Unix tool
  (the shell installer scripts, the git hook row, the inbox route the tmux
  runtime owns) skip with a message ending `(windows, #303)`; the runtime rows
  judge "executable" per platform (a mode bit on Unix, a `PATHEXT` extension on
  Windows, `cmd/muster/execbit_*.go`, #312) and run there; the state-directory
  row, the grant-edit backup and principal enrolment run there and assert
  through `privfs.Check`.
- The sandbox remains macOS-only; the opencode tests that cover its profile
  skip on Windows with a message ending `(windows, #303)`.
