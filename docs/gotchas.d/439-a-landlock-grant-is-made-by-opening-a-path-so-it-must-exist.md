# A Landlock grant is made by opening a path, so the path must exist when the policy is applied

**Issue:** #439

## What would have happened

The macOS profile names paths in text, and a path that is not there yet costs
nothing: the agent's transcript directory for a working directory it has not run in
before is granted, then created by the agent on first use. A Landlock rule is
attached to an open descriptor, so the same plan on Linux names a path the
launcher cannot open, and the launcher refuses (exit 125, the program never
runs) — correctly, because a grant that silently did not take is a weaker profile
than the one reported.

## The rule it produced

On Linux the driver settles the plan before it writes the policy:

- directories the agent writes as it works (its transcripts) are **made**, owner
  only, before the launch — but not by the capability measurement, which must not
  leave a directory in the user's tree for every probe;
- a driver-carried read the host lacks (an agent with no skills directory) is
  **dropped**, so the state report names exactly what the policy grants;
- a path a **caller** names that does not exist is **refused** as invalid: starting
  without it would be a weaker profile than the one asked for.

## Neighbours found on the way

- A rule on something that is not a directory may carry only file rights, or the
  kernel answers `EINVAL`; and the path-rule attribute is a *packed* twelve bytes,
  where a Go struct of the same fields is sixteen. Both are written as bytes.
- `/etc/resolv.conf` and `/etc/mtab` are links out of `/etc` on some distributions,
  so granting `/etc` does not reach what they point at; their targets are granted.
- Opening a new pseudo-terminal opens its peer by number, so granting
  `/dev/ptmx` is not enough: the `/dev/pts` directory is granted too.
- `/proc/<pid>/environ` of another same-user process stays closed with `/proc`
  granted, by Landlock's scoping of process access — but a test that sets a
  variable with `Setenv` and then looks for it there finds nothing even unconfined,
  because that file holds the environment the process was *started* with. A test
  that proves a refusal runs the same read unconfined first.
- GNU `cp -c` is `--preserve=context` and succeeds on a plain copy, so passing it
  where macOS means "clone" reports every private cache as a clone.
- `socket(AF_UNIX)` is refused for every type, but `socketpair` stream and
  seqpacket are not: a child-process library cannot spawn a program without one.
  A datagram pair is refused, because it can be connected again to any address.
