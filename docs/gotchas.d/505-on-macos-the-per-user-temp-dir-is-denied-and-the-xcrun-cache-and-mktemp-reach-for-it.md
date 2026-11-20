# On macOS the per-user temp dir is denied, and the xcrun cache and `mktemp` reach for it

**Issue:** #505

## What happened

Two things in a confined macOS session write the system's per-user temporary
directory (`confstr`'s `_CS_DARWIN_USER_TEMP_DIR`, `/private/var/folders/…/T/`)
and ignore the session's own `TMPDIR`. The profile denies that directory on
purpose: other processes keep their hand-off files in it.

1. **The toolchain shim's cache.** `/usr/bin/git` (and `cc`, `python3`, anything
   that is an `xcrun` shim) writes a lookup cache `xcrun_db-XXXXXXXX` there and,
   refused, prints `couldn't create cache file … (errno=Operation not permitted)`,
   usually twice, into every tool output the model reads. Git still succeeds.
   Measured: ~795 such lines across 16 confined sessions in three days on one Mac.
2. **`mktemp` with no template.** `mktemp -d` and `mktemp -t` are refused with
   `mkdtemp failed on …/T/tmp.XXXXXXXX: Operation not permitted`. A test that
   spawns it passes on a Linux runner and fails in the sandbox, so agents report a
   "sandbox-blocked check" and reviewers discount it.

## The rule it produced

- The shim reads the cache's path from an environment variable literally named
  `xcrun_db` (lower case; it must be absolute, a relative value is ignored and the
  default is used). A sandboxed session is given `xcrun_db=<session dir>/tmp/xcrun_db`,
  inside the directory the profile already lets it write. No grant. It is owned on
  macOS (a create naming it is refused `invalid`) and an unconfined session is given
  none, like `DEVELOPER_DIR` (#484).
- The per-user temporary directory is **not** opened, not even by name. The
  tempting fix — a write grant on `T/xcrun_db-` (safe, narrow) plus one on `T/tmp\.`
  for `mktemp` — fails on the second half: `mktemp -d` run by *any* process on the
  account makes a `T/tmp.XXXXXXXX` directory, so that grant would let a confined
  session write into another process's temp dir, and a profile cannot say "only one
  I created". The first half did not need a grant either, so neither was added.

## What is easy to get wrong

- **Handing the child `TMPDIR` does not make bare `mktemp -d` work.** On macOS
  `mktemp(1)` takes the per-user temporary directory first and consults `TMPDIR`
  only when the system gives none (measured unconfined too: `TMPDIR=/x mktemp -d`
  makes `…/T/tmp.…`). The fix for a script is a template it places itself:
  `mktemp -d "$TMPDIR/x.XXXXXXXX"`. A test in another repo that fails this way
  needs that change, not an environment one.
- `xcrun_db` is spelled lower case. `XCRUN_DB` is silently not read.

## How to test it so it fails for the right reason

`toolchain_darwin_test.go` runs the real shim from inside a real confined session
and asserts both halves (no complaint in the output, and the cache file exists at
the path the driver sent it to — silence alone could be a shim that wrote
nowhere). A second test is the guard on the decision: a write to a new name, and
to a file in a `tmp.*` directory made *outside* the sandbox, are still refused.
Whoever adds a grant on that directory breaks it and has to answer the reason
above.
