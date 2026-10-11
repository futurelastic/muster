# Three things only a real Windows run told us, each of which the cross-compile had passed

**Issue:** #306. Cross-compiling and cross-vetting for Windows passed every
time; each of these was found by the first run of the Windows CI job.

## A hard link to the test binary cannot be deleted while the test runs

A fake module was installed as a hard link to the running test binary. The
test's temporary directory then could not be removed (`Access is denied`) for
the whole life of the test process: Windows refuses to delete **any name** of a
file that has a running image, and the test process itself is that image. The
installer copies the binary instead.

## The system renders the user's own entry as an alias

On a runner whose user is the built-in administrator, the access list that was
applied correctly read back as `D:P(A;;FA;;;LA)` — never as the long SID. A
check that compares trustees as strings concluded "someone else". `LA` and `LG`
are domain-relative aliases (relative ids 500 and 501); the comparison now
expands them against the user's own SID. A service running as the local system
account shows up as `SY` in the same way.

## macOS answers a signal to a group of exiting processes with EPERM, not ESRCH

Found while moving the Unix half onto the shared package: a second `kill(-pgid)`
after the leader had been waited for returned `EPERM` on macOS. We created the
group, so a refusal to signal it can only mean its members are on their way out;
it is treated as "gone", as `ESRCH` is.
