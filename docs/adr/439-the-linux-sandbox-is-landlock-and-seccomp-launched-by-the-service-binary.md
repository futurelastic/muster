# ADR: the Linux sandbox is Landlock and seccomp, applied by the service's own binary

**Issue:** #439 (follows #281, #345)
**Status:** decided

## Context

A sandbox here is a contract, not a feature: the session's process, and every
tool it starts, cannot read or write outside the paths it was given, cannot
connect to a unix socket, and cannot reach a system service — all three together,
because each is a way out of the other two (`sandbox.go`). Where the platform
cannot deny all three the capability is absent and a create that needs it is
refused. That is the whole of what made the macOS mechanism worth having, and it
is what Linux lacked: every runtime there reported `sandboxUnavailable`, so a
code worker on a Linux workstation could not be started with a confined posture at
all, and the only boundary was the Unix user — which also owns the supervisor's
token and every provider key file.

The facts that decide the shape, measured on the target and in a VM:

- A Linux workstation (kernel 7.0) had Landlock enabled, and bubblewrap ran
  unprivileged only because the distribution ships a profile for that one binary.
  The same distribution restricts unprivileged user namespaces for every other
  program.
- On a 6.8 kernel (Landlock ABI 4) a process confined by Landlock rules and a
  seccomp filter could not read or write outside its grants (nor could its
  children), could not read another same-user process's `/proc/<pid>/environ` even
  with `/proc` granted, could not create a unix socket, UDP socket or shared
  memory, and — under a closed network — could not connect anywhere, while binding
  one named port worked and the unconfined service could connect into it. glibc
  name lookup and Node's child-process, DNS and loopback paths worked with unix
  sockets denied. Opening a new pty needed the `/dev/pts` directory granted.

## Decision

1. **The mechanism is Landlock (files, and from 6.12 abstract unix sockets and
   signals) plus a seccomp filter (everything a file rule cannot name), applied by
   the service's own binary to itself before it becomes the agent.** It is called
   `linux-landlock-seccomp`. The launcher is a hidden subcommand,
   `muster __sandbox-exec <policy> -- <program>`, that locks itself to one OS
   thread, applies the rules, sets `no_new_privs`, installs the filter, marks
   every inherited descriptor close-on-exec and execs. It sits where `sandbox-exec`
   sits on macOS, so the program keeps the pid and command name the service
   already follows, and no rule it applies can be removed by what it runs. Any
   failure before the exec exits 125 and the program never runs.
2. **The floor is Landlock ABI 6 (Linux 6.12).** ABI 6 is the first that scopes
   abstract unix sockets and signals. Below it a confined session could still
   signal the supervisor and connect to a service listening in the abstract
   namespace, and "denies unixSockets and systemServices" would be a claim the
   kernel does not back. The policy is the same on every kernel that offers it: a
   newer ABI does not make it harder, because one name must not mean two profiles.
3. **A closed network is stricter than macOS's, not equal to it.** The kernel's
   network rules cannot tell loopback from anywhere else, so a closed network
   allows no outbound connection at all, loopback included, no UDP (so no name
   resolution), and the only port that may be bound is the one the policy names —
   the runtime's own server, which the service (not confined) connects into. The
   enum value stays `closed`; the mechanism name in the capability carries the
   difference, and the contract text says so.
4. **The capability is declared only after a confined child proved it.**
   `HostSupport` checks the architecture, the launcher, and the kernel's ABI, and
   then runs the launcher's self-test in a throwaway child under a real policy: it
   must fail to read outside its grants, create a unix or UDP socket, connect to a
   loopback listener, touch shared memory, read the supervisor's environment, or
   signal the supervisor. Every refusal names the mechanism and what it was
   looking for.
5. **Allow-lists stay in the driver.** Reads and writes are the driver's defaults
   plus what a caller names; the caller cannot widen into what the classes deny. A
   Linux rule is attached to a path that exists, so a path a caller names that does
   not is refused, the directory the agent writes its transcripts to is made before
   the launch, and a driver-carried read the host lacks is dropped before the
   report, so the report names exactly what the policy grants.

## Rejected

- **bubblewrap.** It needs user namespaces, which the distribution restricts by
  default; a network namespace would cut the service off from the runtime's
  server, which listens on TCP loopback only; a path that is not mounted is
  *absent* where macOS makes it unreadable, so the two would differ in kind; it is
  an external binary whose flags vary; and its seccomp option takes a filter we
  would write anyway. Two mechanisms would also be two policies and two test
  matrices. It stays a possible later mechanism, under its own name, if a kernel
  below the floor must be supported.
- **Landlock alone.** It does not cover unix socket creation on kernels below
  the newest, UDP, shared memory, queues, keyrings or io_uring.
- **seccomp alone.** It cannot see a path, so it cannot confine files.
- **A lower floor with a weaker profile under the same name.** The one thing the
  contract forbids.
- **A network namespace for `closed`.** Faithful to "loopback only", and
  unreachable from the service for the same reason as above.

## Consequences

- A Linux host on a kernel older than 6.12 reports `sandboxUnavailable` with the
  ABI it has and the one it needs. The hosted CI image measured on 2026-11-11 has
  kernel 7.0 and Landlock ABI 8, so the whole test set runs there; the
  `sandbox-linux` job runs it with `MUSTER_SANDBOX_REQUIRE=1`, so a hosted image
  that moves to a kernel without the feature turns that job red (with the kernel's
  facts in its log) instead of the tests skipping quietly in the ordinary job. The
  repository variable `MUSTER_LINUX_SANDBOX_RUNNER` points the job at another
  runner without a change to the workflow.
- A session cannot create a unix socket at all (a pre-connected stream pair, as a
  child-process library makes, still works), has no shared memory directory, and
  cannot use io_uring. An agent installed under a version manager's directory in
  the home, or as a confined package, cannot be read by its own session and is
  refused with the reason.
- Other processes' `cmdline` and `status` stay readable, and `/etc` is readable,
  as on macOS: keep a token or key out of both.
- A launcher refusal inside the opencode driver cost up to three start attempts,
  because the exit looked like losing a port race, and a runtime that cannot run
  under the policy failed every create loudly instead of the capability being
  withheld. Both were follow-ups and are done (muster #456): the launcher's
  refusal is recognised and returned once with its reason, and the opencode
  runtime is measured under the policy like the terminal runtime's agent.
