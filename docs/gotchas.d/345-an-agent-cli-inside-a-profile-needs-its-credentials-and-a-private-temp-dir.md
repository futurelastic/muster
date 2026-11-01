# The agent CLI starts inside the sandbox profile and then fails twice — credentials and a shared temp directory

**Issue:** #345 (confining the default terminal runtime's agent). Measured
2026-11-01 on one macOS host (13.6, Intel) with agent CLI 2.1.316; re-measure
on a new CLI major or a new host class before trusting the lists below.

## What was measured

The profile the other runtime ships (the working directory, a private
directory, the program's own install, the system; everything else denied; unix
sockets and system services denied) was applied to the agent CLI unchanged.

| Under that profile | Result |
|---|---|
| `--version` | runs |
| `auth status` | **logged out** — its credential file is not readable |
| a one-shot prompt | **EPERM opening `/tmp/claude-<uid>`** — the CLI hard-codes a temp directory shared between users unless `CLAUDE_CODE_TMPDIR` (or `TMPDIR`) names another |
| + the credential file readable, a private temp directory | answers |
| interactive, + its configuration read-only, its own transcript directory writable | answers, writes `projects/<cwd-encoded>/<id>.jsonl`; the service reads that for usage, context and conversation |
| through the service, remote control on (the default) | control channel `active`; the conversation is found **by the session's name** |

A confined, prompt-free (`bypass`) session asked to `touch` a file in the
home directory was refused by the host (`Operation not permitted`); the file
does not exist.

"Starts" and "logged in" are different measurements. The first passes on every
host; the second is the capability, and the driver takes it on each host
(`auth status`, through the same launch builder a create uses) instead of
asserting it.

## What the profile allows for this agent, and why the list is short

- **read** its install, `.credentials.json`, `settings.json`,
  `settings.local.json`, `CLAUDE.md`, `agents/`, `skills/`, `commands/`,
  `rules/`, `plugins/`, `cache/`, and the config file beside the config
  directory — read-only, because a setting, a hook, a skill, an MCP server or a
  trusted-folder entry is code or authority a *later* unconfined session would
  run;
- **write** the working directory, the session's own space (its `TMPDIR`), and
  `projects/<its own cwd>` — nothing else of the agent's.

The directory holding the profile and the report of what is in force is *not*
granted: a confined session cannot rewrite what it is confined by.

## What stops working, and is not fixed here

These are consequences of denying; none stops the session.

- **User-level hooks do not run** (`~/.claude/hooks/…` is not readable). They
  fail non-blocking, so a hook that *guards* something fails open. The profile
  is the control for a confined session, not the hooks; a caller that wants a
  hook names its script and interpreter in `posture.files.readPaths`.
- **Agent and skill definitions that are symlinks out of the config directory**
  are unreadable until the caller names their targets in `readPaths`.
- **A linked worktree** needs its git common directory in `writePaths` (git
  cannot read `.git/worktrees/<name>` otherwise).
- **The process registry** (`~/.claude/sessions/<pid>.json`) is not written, so
  a session created *without* remote control (and so without `-n <name>`) has a
  transcript the service cannot attach to a session. With the default
  (remote control on) the name finds it.
- Denied and harmless: writes to `~/.claude.json`, `session-env/`, `cache/`,
  `plugins/`, the CLI's own cache and update locks, browser native-messaging
  manifests; mach lookups for the keychain and a preferences agent. One visible
  cost: a `SessionStart` hook line `EPERM mkdir ~/.claude/session-env/<id>`.
- **A host whose credentials are in the keychain fails `auth status` and keeps
  refusing.** Allowing the keychain's services would open every item the user
  has to the session; the driver does not.

## The rule going forward

A capability about a *runtime* is measured on the *host*, by running that
runtime the way a create runs it; "the host has a sandbox" is a statement about
the host. And a grant for a confined session is read-only unless the service
itself reads what is written.
