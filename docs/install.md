# Install — from nothing to a running service

[`deploy.md`](deploy.md) starts from a service that already runs: it backs up
the installed binary, restarts through the machine's service manager, and
verifies by asking the running service what it is. On a machine that has never
run this, there is nothing to back up, no path anything executes, and nothing
to restart or ask. This page is the part before that.

The [Quickstart](../README.md#quickstart) is a demo: a foreground process with
a token that lives as long as the shell. This page ends with a service that
survives a reboot and a deploy procedure that applies to it.

It does not install or configure a service manager, for the same reason
`deploy.md` step 3 does not: which one a machine runs is that machine's fact.
It states what the unit must execute, what its environment must contain, and
in what order — and it ends with a command that checks all of it.

## What a working installation is

Every row below is something an installation can be missing and still start.
Several of them are missing silently, and each has cost somebody a day: a
supervising principal without the `keys` grant verifies clean and then refuses
every keypress (#68); an unset inbox index leaves every delivery on the pane
path (#122); an index without a permission-mode class uses the inbox for
nothing while reporting that it delivers to it (#148).

`muster doctor` reports each of these as a row — step 9 below — so the
order of this page and the order of its output are the same.

## The procedure

0. **Or skip the build.** `npm i -g @futurelastic/muster` installs a prebuilt,
   already-stamped binary for this platform (the README's *Install* section has
   the details). `command -v muster` is the *launcher*, a Node script — not
   something to copy or to point a service unit at. The binary itself is in the
   platform package beside it; copy that to a fixed path, so an npm upgrade can
   never change what the unit executes:

   ```sh
   BIN=$(node -p "require.resolve('@futurelastic/muster-' + process.platform + '-' + process.arch + '/package.json', { paths: [require('fs').realpathSync('$(command -v muster)')] }).replace('package.json', 'bin/muster')")
   install -m 0755 "$BIN" /usr/local/bin/muster
   muster --version      # prints the release
   ```

   Then continue at step 2 with `/usr/local/bin/muster` as the binary.

1. **Build with the version-control stamp, and choose where the binary
   lives.** Build from a clean checkout — a binary built from a modified tree
   has no identity (`deploy.md` step 1). Pick an absolute install path and
   keep it: the service unit will execute that path, and it is the value
   `scripts/deploy.sh` later needs as `REMOTE_PATH` (#66). A path nothing
   executes is not an install path.

   ```sh
   go build -ldflags "-X github.com/futurelastic/muster.version=$(git describe --tags --match 'v[0-9]*')" \
     -o ./muster ./cmd/muster
   install -m 0755 ./muster /usr/local/bin/muster
   ```

   The `-ldflags` stamp is what `/v1/health` reports as `build.version` — the
   release a client checks a minimum supported version against (#161). A plain
   `go build` still runs, and reports `version: null`; `scripts/deploy.sh`
   stamps it for you on every later deploy.

2. **Pick this machine's id** — `FLEET_MACHINE`. It is the name every peer
   uses for this machine in its own peer list, so choose it once. Unset, it
   defaults to `local`, which no peer can name. Row: `machine.id`.

3. **Pick the listen address** — `FLEET_ADDR`. A specific interface and a
   fixed port. The default is loopback on an ephemeral port, deliberately: an
   unconfigured service is reachable only from its own machine, and it has no
   stable health URL either. Never `0.0.0.0` — the service reads paths and, when
   permitted, starts processes. Loopback is added on the same port
   automatically. A machine known by several addresses lists them all,
   comma-separated; the ones not present yet are waited for rather than
   refused (see [Multi-machine](multi-machine.md#a-machine-that-is-known-by-more-than-one-address)).
   Row: `bind.addr`.

4. **Create the state directory**, mode `0700` — `FLEET_STATE_DIR`. Without it,
   idempotency keys and the event sequence live in memory and are lost on
   every restart (defect D5). The service creates it if it is missing; creating
   it yourself lets you choose the owner. Row: `state.dir`.

5. **Write a principal table** — `FLEET_CONFIG`. A fleet runs in table mode:
   one identity per caller, per-verb grants, per-peer credentials. Start from
   an empty document and enrol with the command that validates grants before
   it writes:

   ```sh
   mkdir -p -m 0700 ~/.config/muster
   printf '{}\n' > ~/.config/muster/config.json
   export FLEET_CONFIG=~/.config/muster/config.json

   muster principal add supervisor \
     --grants=read,create,send,interrupt,close,rename,discard,keys,relay
   ```

   The token lands in a `0600` file beside the config; clients read it from
   there, never from an environment variable. **Give the supervising client
   the same principal name, with the same grants, on every machine** — a
   machine added later with a differently named principal lacking one grant
   is exactly the drift nothing else surfaces. `keys` is its own grant and is
   denied by default (#68), and it is the grant that can **escalate** a session —
   it delivers `BTab`, which cycles the permission mode (#188) — so give it to a
   principal you would trust to loosen any session it can reach; `relay` is only
   needed where peers exist.

   To change a principal that already exists — a later release adds a verb with
   its own grant — do not hand-edit the file:

   ```sh
   muster principal grant supervisor remote-control
   muster principal revoke supervisor remote-control
   ```

   Grant names are checked against the build, the config is replaced atomically
   after a dated backup (`config.json.<YYYYMMDD-HHMMSS>.bak`, `0600`), and the
   resulting set is printed as `{principal, grants}`. It edits the file only:
   restart the service to apply it, then confirm with `whoami`. It is local to
   the machine and has no HTTP route. `human-relay` is refused unless
   `--allow-human-relay` is given, which prints the shared-principal warning.
   Rows: `config.load`, `token.source`, `principals.supervisor`,
   `principals.relay`, `local.mutations`.

6. **Peers, both halves** — only for a fleet. Each direction needs one secret
   in two places, and the far half is invisible from the near machine:

   - On the machine that **owns the sessions** (call it b), enrol the calling
     machine a as a principal holding the verbs a will relay — including `keys`
     if a will relay keys:

     ```sh
     muster principal add machine-a --grants=read,send,keys \
       --token-file=./machine-a-on-b.token
     ```

   - On the **calling** machine a, put that token in the peer entry — it is a's
     identity on b, not b's on a — and name a principal for a's own system
     identity holding the **same** token, so a has something to present for
     its long-lived peer reads (#98). The peer compares the token value, never
     the name; a name mismatch is harmless and a token mismatch is fatal:

     ```json
     {
       "principals": [
         { "name": "supervisor", "token": "…", "grants": ["read", "…", "relay"] },
         { "name": "system:machine-a", "token": "<machine-a-on-b token>", "grants": ["read"] }
       ],
       "peers": [
         { "machine": "machine-b", "url": "http://<b's address>:<port>", "token": "<machine-a-on-b token>" }
       ]
     }
     ```

   The address is one **you** confirmed reachable from a, never b's own idea of
   its name. A peer known by several addresses takes `"urls": [...]` in place of
   `"url"`, tried in order. Rows: `peer.self-credential`, `peer.<m>.url`,
   `peer.<m>.credential`, `peer.<m>.reachable`, `peer.<m>.grants`.

7. **Inbox delivery, if this machine has an index writer** —
   `FLEET_INBOX_INDEX`. Absent means every delivery uses the pane path, which is
   correct on a machine with nothing populating an index and is #122 on a
   machine you expected to deliver to an inbox. Where it is set, the writer
   must emit `mode_class` for each entry, and emit the class the session is
   actually running in — an entry without one cannot be attested and is sent
   through the pane path (#148). **Setting it requires a principal table
   (step 5): with none, the service refuses to start (#196)** — who relays a
   person's messages has to be a grant the table holds, not a header a caller
   sets. The principal that relays a person's messages must also hold
   `human-relay` **before** the writer emits a class — otherwise its messages
   arrive through the inbox as peer messages (#184; `docs/deploy.md`, "Turning
   the inbox route on"). Rows: `inbox.index`, `inbox.mode-class`,
   `principals.human-relay` (fails when an index is set and there is no table).

   **Optional external delivery modules** — `FLEET_DELIVERY_MODULES` (#185). A
   module is a separate helper program that delivers a message by a channel other
   than the terminal; this service uses it for sessions it launches itself, and
   falls back to the built-in terminal path for everything else. Unset or empty
   means none — the ordinary state, and byte-for-byte what a build without the
   feature does. To enable one, put its executable in the modules directory
   (`FLEET_MODULES_DIR`, default `<prefix>/libexec/muster/modules` where
   `<prefix>` is the parent of the daemon binary's directory — set it explicitly
   when the binary is reached through a symlink), one file per module named by
   the module, mode not group- or world-writable, and list the name. A name with
   no executable is logged once and skipped: a machine that could not fetch the
   module is a supported state. `scripts/deploy.sh` can install one for you
   (`deploy.md`, "Optional delivery modules"). Row: `delivery.modules`, which
   only ever warns.

8. **Write the service unit.** Whatever the machine's service manager is, the
   unit must:

   - execute the absolute path from step 1 with the single argument `serve` (a unit still pointing at the former `colab-fleetd` name keeps starting bare, which is the same thing for one release);
   - set `FLEET_MACHINE`, `FLEET_ADDR`, `FLEET_STATE_DIR`, `FLEET_CONFIG`, and
     where they apply `FLEET_INBOX_INDEX`, `FLEET_PEERS` and
     `FLEET_DELIVERY_MODULES` (with `FLEET_MODULES_DIR` and
     `FLEET_DELIVERY_MODULE_ENV` if you use them);
   - set `FLEET_TMUX_BIN` to the multiplexer's absolute path — a service
     manager starts processes with a bare `PATH` that usually lacks it, and
     the failure appears only under the manager, never in your shell
     (row: `runtime`);
   - use **tmux 3.7 or newer**. The terminal path pastes with bracketed paste
     only when tmux can report that the pane asked for it
     (`#{bracket_paste_flag}`), and that variable first exists in 3.7. On an
     older tmux, measured on 3.4, 3.5a, 3.6 and 3.6a, it reads empty: sessions
     are created and read but every send on the terminal path is refused. Most
     distribution packages are older, so build or install a current tmux and
     point `FLEET_TMUX_BIN` at it (row: `runtime.tmux-version`, which runs
     `tmux -V` and fails on an older release);
   - when `FLEET_RUNTIME_OPENCODE=1`, set `FLEET_OPENCODE_BIN` to the opencode
     binary's absolute path, for the same reason (row: `runtime.opencode`,
     which resolves the binary and never runs it). A runtime that is enabled but
     not usable does not stop the service: it is listed on `GET /v1/runtimes`
     as `unavailable` with a `cause`, and a version check that merely timed out
     under load is retried until it answers;
   - run as the user who owns the sessions it will manage;
   - restart the process when it exits, and start it at boot;
   - put **no secret** in the unit's environment. With a principal table,
     `FLEET_TOKEN` and `FLEET_ALLOW_*` are ignored; leaving them set only
     misleads the next reader (row: `local.mutations`).

9. **Run `doctor` under the unit's environment, before starting it.**

   ```sh
   env -i FLEET_MACHINE=… FLEET_ADDR=… FLEET_STATE_DIR=… FLEET_CONFIG=… \
     FLEET_TMUX_BIN=… /usr/local/bin/muster doctor --principal=supervisor
   ```

   Your login shell's environment is not the unit's. A doctor run from the
   shell answers for a service that does not exist; `env -i` with exactly the
   unit's variables answers for the one that will. It exits `1` if any row
   fails, `0` otherwise; `warn`, `unknown` and `skip` never change that. It
   never prints a token, a path or a peer address, so its output can be pasted
   into an issue as it is. `--json` is for scripts; `--offline` skips the peer
   probe; `--skip=<row>` marks a row deliberate (for example
   `--skip=inbox.index` on a machine without an index writer).

   One row is always `unknown` for now: `peer.<m>.grants`. Whether a peer
   grants this machine `keys` is not readable from this machine (#106) — run
   `doctor` on that peer too.

10. **Start the service and verify it** with a principal's token file:

    ```sh
    curl -s -H "Authorization: Bearer $(cat ~/.config/muster/supervisor.token)" \
      http://127.0.0.1:<port>/v1/health
    ```

    A `build` in the answer is the service telling you what it is. That URL
    and that token file are what `deploy.md` uses as `FLEET_HEALTH_URL` and
    `FLEET_HEALTH_TOKEN_FILE` from now on.

11. **Then the peer, and only then** — the same procedure on each machine, and
    a `doctor` run on each: a peer's grants to this machine are that peer's row
    to report, not this machine's.

From here on, every change is a deploy: [`deploy.md`](deploy.md).

## Windows

muster runs on Windows as a per-user service, started by a scheduled task. Read
[the decision](adr/303-windows-ships-without-an-interactive-terminal-driver.md)
before relying on it for anything but the subprocess runtime: **a Windows
machine has no interactive-terminal driver.** What it does have is the
`opencode` runtime, driven as a child process, and the whole HTTP API, the
principal table and federation. The procedure above applies; this section is
what differs.

### What a Windows machine reports

- `FLEET_RUNTIME` defaults to `none`: no primary local driver. `tmux` is refused
  at startup, naming the decision. `muster compat` refuses too, because there is
  no terminal runtime for it to check.
- Set `FLEET_RUNTIME_OPENCODE=1` and `FLEET_OPENCODE_BIN` (an absolute path) to
  enable the one local runtime. `GET /v1/runtimes` then lists exactly one local
  row, `opencode`, with no `sandbox` entry: the sandbox is not enforceable on
  Windows, so a create that asks for one is refused `unsupported`, never started
  unconfined. With the runtime not enabled the service has no local sessions and
  fronts its peers.
- The terminal runtime is **absent** from the list, not `unavailable`: nothing
  enabled it and nothing could. A client that keys capabilities by
  `(machine, runtime)` needs no change.

### Where things live

| What | Where | Notes |
|---|---|---|
| the binary | `%LOCALAPPDATA%\Programs\muster\muster.exe` | a fixed path the task executes; the launcher is not what a task runs |
| config and tokens | `%APPDATA%\muster` | `config.json` and one `<principal>.token` per principal |
| state | `%LOCALAPPDATA%\muster\state` | `FLEET_STATE_DIR`; created owner-only |
| service log | `%LOCALAPPDATA%\muster\muster.log` | `FLEET_LOG_FILE`; created owner-only, appended to |

Owner-only means a protected access list naming only the current user, applied
when the object is created. `muster doctor` reports the state directory's list
(`state.dir`) rather than guessing from a mode that does not exist here.

**Write the config as UTF-8 without a byte-order mark.** Windows PowerShell 5's
`Set-Content -Encoding UTF8` and `Out-File` add one, and the JSON reader does not
accept it. From any PowerShell:

```powershell
$dir = Join-Path $env:APPDATA 'muster'
New-Item -ItemType Directory -Force $dir | Out-Null
[IO.File]::WriteAllText((Join-Path $dir 'config.json'), '{}', [Text.UTF8Encoding]::new($false))
```

### Install

The Windows packages are built and tested by every release but are **not
published to npm yet**, so `npm i -g @futurelastic/muster` does not give a
Windows machine a binary. Two routes work today:

1. **Build from source** on any machine with Go, stamping it as step 1 does:

   ```powershell
   $v = git describe --tags --match 'v[0-9]*'
   $env:GOOS = 'windows'; $env:GOARCH = 'amd64'   # or arm64; unneeded on Windows itself
   go build -ldflags "-X github.com/futurelastic/muster.version=$v" -o muster.exe .\cmd\muster
   ```

   then copy `muster.exe` to the path in the table above.
2. **Assemble the npm packages locally** from a release build, as CI's
   end-to-end job does (`scripts/e2e-windows.ps1`): `scripts/build-release.sh`,
   then `node scripts/assemble-npm.mjs --all`, then `npm pack` and
   `npm install -g` the launcher and the `win32-x64` tarball. The platform
   package is `@futurelastic/muster-win32-x64` (or `-win32-arm64`); npm names
   follow `process.platform`.

### Enrol a client

Steps 5 and 6 above, unchanged — `muster principal add` writes the token file
beside the config, owner-only:

```powershell
$cfg = Join-Path $env:APPDATA 'muster\config.json'
muster principal add supervisor --grants=read,create,send,interrupt,close,rename,discard,keys --config=$cfg
```

A client reads the token from that file; it is never put in an environment
variable.

### The service: a scheduled task

muster installs no service manager here either. The supported shape is a task
that runs `muster.exe serve` when the user signs in. Each setting is there
because the default is wrong for a service:

```powershell
$exe    = Join-Path $env:LOCALAPPDATA 'Programs\muster\muster.exe'
$who    = "$env:USERDOMAIN\$env:USERNAME"
$action = New-ScheduledTaskAction -Execute $exe -Argument 'serve'
$trigger  = New-ScheduledTaskTrigger -AtLogOn -User $who
$settings = New-ScheduledTaskSettingsSet `
  -ExecutionTimeLimit ([TimeSpan]::Zero) `
  -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -MultipleInstances IgnoreNew `
  -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
$principal = New-ScheduledTaskPrincipal -UserId $who -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -TaskName muster -Action $action -Trigger $trigger `
  -Settings $settings -Principal $principal
```

- **Execution time limit 0 (`PT0S`).** Task Scheduler's default is 72 hours, after
  which it ends the task. The service then stops with no error anywhere.
- **Battery settings off.** The defaults refuse to start on battery and stop
  when the machine goes onto it.
- **Restart on failure, and ignore new instances.** A crash is retried; a second
  start while one is running does nothing.
- **Interactive token, limited.** It runs as the user who owns the sessions,
  without elevation, and only while they are signed in.

A task cannot carry its own environment. `FLEET_*` come from the **user's**
environment, which a task started as that user is given:

```powershell
[Environment]::SetEnvironmentVariable('FLEET_MACHINE', 'machine-w', 'User')
[Environment]::SetEnvironmentVariable('FLEET_CONFIG', "$env:APPDATA\muster\config.json", 'User')
[Environment]::SetEnvironmentVariable('FLEET_STATE_DIR', "$env:LOCALAPPDATA\muster\state", 'User')
[Environment]::SetEnvironmentVariable('FLEET_ADDR', '127.0.0.1:9470', 'User')   # a fixed port of your choice
[Environment]::SetEnvironmentVariable('FLEET_LOG_FILE', "$env:LOCALAPPDATA\muster\muster.log", 'User')
[Environment]::SetEnvironmentVariable('FLEET_RUNTIME_OPENCODE', '1', 'User')
[Environment]::SetEnvironmentVariable('FLEET_OPENCODE_BIN', 'C:\path\to\opencode.exe', 'User')
```

Put no secret there, for the reason in step 8. Variables set this way are read
when a process starts, so sign out and in, or start the task again, after
changing them.

A scheduled task gives the process a console window; muster releases it when it
is the only process on that console, so no window stays on the desktop. Started
from a shell, it keeps the shell's console.

### Check it

`muster doctor` runs as on any machine (step 9) and adds one row on Windows,
`service.task`, which reads the task with `schtasks /Query /XML`:

| Finding | Status |
|---|---|
| no task of that name | fail |
| execution time limit absent (the 72 h default) or anything but `PT0S` | fail |
| battery settings on; the action runs a different executable than this one; no at-log-on trigger | warn |
| the definition could not be read | unknown |

The task is looked up as `muster`; set `FLEET_TASK_NAME` for doctor to look for
another name. Run doctor from a shell that has the same `FLEET_*` as the task.

Stopping the service ends it without a graceful shutdown: there is no
console-signal route to a service the scheduler started. Everything it started
is in a Job Object that ends with it ([ADR 306](adr/306-windows-process-tree-and-owner-only-files.md)),
so a stop does not leave runtime processes behind.

### Terminal sessions: a peer in WSL

A Windows machine serves terminal sessions by federation, not natively: run the
Linux build inside WSL with its **own** machine id, and list it as a peer of the
Windows service (step 6, both halves). The Windows service relays; the terminal
driver runs where tmux runs and keeps its guarantees. Two muster processes per
machine is the cost, and it is the operator's to bear.

### What is not verified

Said here so nobody has to find it out:

- **The Windows packages are unpublished.** They are assembled and installed in
  CI, never from the registry.
- **arm64 is cross-built and never run.** Only `amd64` is exercised.
- **The WSL peer route has not been run.** The Windows-to-WSL direction relies on
  WSL's localhost forwarding; the reverse direction depends on the WSL network
  mode. Treat both as configuration to prove on the machine.
- **The scheduled task is exercised on a CI runner,** which may not give it an
  interactive desktop; CI says which start path it took. A task on a signed-in
  desktop has not been observed there.
- **No Windows service (SCM) form exists.** A task runs only while the user is
  signed in.
