# 480 — a login item puts the Background service back after a reboot

## Context

On macOS `muster service install` loads the unit into the user's Background
launchd domain (`user/<uid>`), and the plist carries
`LimitLoadToSessionType=Background`. That is deliberate: every session inherits the
security session of the process that started the session server, and one started
from the desktop context can move an agent runtime's saved login into the login
Keychain, which a Background session then cannot read.

On a Mac with a GUI login, launchd restores only the `gui/<uid>` domain at login.
A Background job loaded into `user/<uid>` by hand is not restored: after a reboot
the service was loaded nowhere. The usual recovery, a `launchctl load` from a
desktop Terminal, put it where it must not be, and that broke two things:

- **The next update failed.** The installer's `muster service install` bootstraps
  into `user/<uid>`; launchd refuses that while a copy of the same label is loaded
  in `gui/<uid>`, so the install ended in "did not come up healthy" and rolled back.
- **The session server it started was in the desktop context**, and sessions
  created through it moved logins into the Keychain.

## Measured

On a Mac with a GUI login, with throwaway labels beside the real one, before any
code:

| Done | Result |
|---|---|
| a job in `gui/<uid>` (no session-type limit, context `Aqua`) ran `launchctl bootstrap user/<uid>` on a Background plist | accepted; the job it started reports the `Background` context |
| the same label loaded in `gui/<uid>`, then `bootstrap user/<uid>` | **refused**, I/O error (5); nothing loaded in `user/` |
| `bootout gui/<uid>/<label>`, then `bootstrap user/<uid>` | works at once, no retry needed |
| the same script a second time, with the job already loaded | a no-op |
| `bootstrap gui/<uid>` and legacy `load` of a plist that has the Background limit | both **refused** (5): a copy in `gui/<uid>` can only come from a unit without that key |
| `launchctl print` of a loaded job | its `properties` line says `abandon process group` when the plist asked for it |

The reboot itself was not run; it is a `Human verify:` row.

## Decision

- **`install` writes a second launchd job, `com.futurelastic.muster.login`**, once
  `doctor` has passed. It has no `LimitLoadToSessionType`, so launchd loads it into
  `gui/<uid>` at a GUI login. It runs one `sh` line that bootstraps the unit into
  `user/<uid>` unless it is loaded there, and exits (`RunAtLoad`, no `KeepAlive`).
  It is written and never loaded by install: loading it would run beside the
  install. On a Mac with no GUI login there is no `gui/<uid>` and it never runs.
- **It calls `launchctl` itself, not `muster service restore`.** After a rollback
  the binary at the installed path is the previous release's and has no such
  command; a login item that depended on the binary's version would stop with it.
- **`muster service restore` is the same load by hand**, for a person: it loads the
  unit into `user/<uid>` if it is not loaded, starts it if it is stopped, checks
  `/v1/health`, and rewrites no unit or configuration. It shares the load step with
  `install`, so an update and a recovery take one path.
- **A copy in `gui/<uid>` is seen and moved.** `install` and `restore` boot it out
  before they bootstrap; `status` has a `domain` row (exit 1) and `doctor` a
  `service.domain` row (a `warn`, never a `fail`: install runs doctor before it
  moves the copy, and a failing row would stop it). The row exists on every macOS
  host, as a skip when the unit is loaded nowhere, so `--skip=service.domain` means
  the same on all of them.
- **The move is refused when it would end the sessions.** Booting out a job that
  does not abandon its process group takes the session server with it; this service
  asks launchd not to do that to its own unit, and a copy in `gui/<uid>` comes from a
  unit this release did not write. The loaded job's `properties` are read; a job that
  does not abandon its group, or whose properties cannot be read, is left alone, and
  the error names the `launchctl bootout` command for whoever accepts what it ends.
  `uninstall` takes the same refusal before it removes anything.
- **A bootstrap refused because the unit is by then loaded is met, not failed.** The
  login item bootstraps the file install has already rewritten; if it gets there
  between an update's unload and load, the update's bootstrap is refused and the unit
  is running from the right file.

## Consequences

- The service returns at the **first GUI login** after a reboot. A Mac that boots to
  a login window (disk encryption) stays down until someone logs in, and a Mac nobody
  ever logs into at the screen is not covered by this job.
- macOS shows a "Background Items Added" notice for a new launch agent, and
  switching it off in the system's Login Items settings turns the survival off
  without `status` knowing.
- `status` exits 1 when the login item file is missing. An install of this release
  always writes it, so the fleet's post-update health gate (which runs `status`) sees
  it present; an install made before it reports the row until it is run again.
- The session server a moved copy started stays in the desktop context: restarting
  it would end its sessions. `GET /v1/health` shows its `serverContext`.
- Not decided here: a lock that serialises `install`, `restore` and `uninstall`. The
  login item cannot take one (it does not run this binary), so the bootstrap rule
  above is what covers the one race that matters.
