# On macOS, `git` and a zsh here-document each reach outside a confined session

**Issue:** #484

## What happened

A session started under the sandbox profile could not run `git` or a
here-document on macOS, out of the box, for two unrelated reasons that read as one
symptom ("the tools don't work in the sandbox"):

1. `/usr/bin/git` is a shim. It asks the developer directory `xcode-select`
   chose for the real tool and loads that directory's own library first. The
   profile reads the system directories and the stock Xcode app and nothing else,
   so a host that selected a *versioned* Xcode app (`Xcode-<version>.app`) failed
   every call with `xcrun: error: unable to load libxcrun … Operation not
   permitted`.
2. zsh writes a here-document's temporary file under `TMPPREFIX`, not under
   `TMPDIR`. Its default is `/tmp/zsh`, which a confined session may not write, so
   `cmd <<EOF` failed with `can't create temp file for here document`.

Measured on 11 live sessions on one host: 119 tool outputs carried the `xcrun`
error and 22 the here-document one; about a fifth of all shell calls carried a
workaround (`DEVELOPER_DIR=…` on every command), which each session rediscovered.

## The rule it produced

- The driver sets `TMPPREFIX` inside the session's own `tmp` directory, beside
  `TMPDIR`.
- When the selected developer directory is not under a root the profile reads, the
  driver sets `DEVELOPER_DIR` to the Command Line Tools directory (already under
  `/Library`, so the session sees no more of the host); with no Command Line Tools
  it adds the selected directory to the session's read-only grants instead.
- Both names are the driver's own and a create naming one is refused `invalid`.
  `DEVELOPER_DIR` is owned on macOS whether or not a given session is given it, so
  what a session finds does not depend on which Xcode the host has selected.

## How to test it so it fails for the right reason

The defect shows only when the selected directory is *not* the stock app: a test
on a host that selected `/Applications/Xcode.app` passes before the fix too. The
decision is therefore a pure function tested with those paths stated outright, and
the end-to-end test (a real confined session running `git` and a zsh
here-document) logs which directory the host selected so a green run says whether
it could have caught the defect.

A session that needs the full Xcode toolchain (a simulator, an app build) is given
the Command Line Tools, which is the smaller one. That case is not served by this
rule.
