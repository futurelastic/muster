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

## The short way: `muster service install`

On one macOS or Linux machine, for the current user, one command does the
procedure below ([the manual procedure](#the-procedure) stays as the reference
for what each step is for, and for everything the command leaves out):

```sh
npm i -g @futurelastic/muster
muster service install
```

It copies the running binary to `~/.local/bin/muster` (so an npm upgrade never
changes what the unit executes), creates the state directory
`~/.local/state/muster` (`0700`) and a principal table in
`~/.config/muster/config.json`, writes the unit, runs `muster doctor` under the
unit's own environment and prints it, loads the unit and checks `/v1/health`.
The unit is a launchd job in the user's **Background** domain on macOS and a
`systemd --user` service on Linux; it sets `FLEET_MACHINE` (the short
hostname), `FLEET_ADDR` (`127.0.0.1:9470`), `FLEET_STATE_DIR`, `FLEET_CONFIG`
and `FLEET_TMUX_BIN` (the tmux found on `PATH`, recorded as an absolute path),
and restarts the service when it exits.

- **A new table is not empty.** The procedure writes `{}` and enrols the
  supervisor next, because a table with nobody in it fails `config.load` and the
  service refuses to start. A new table therefore gets one principal,
  `supervisor`, with every grant a supervising client uses (not `relay`: that is
  for peers), and its token in `supervisor.token` beside it. A table that exists
  is never changed.
- **A restart does not end the sessions.** The units ask the manager to leave
  the multiplexer server the service started alone when the service stops
  (`KillMode=process`, `AbandonProcessGroup`).
- **Idempotent.** A second run changes nothing and says `Nothing to change`. After
  an npm upgrade it replaces the installed binary and restarts the service. A rerun
  without flags keeps the machine id, address and tmux path the unit already has;
  `--machine`, `--addr` and `--tmux` change them.
- **A variable you added to the unit stays.** Some runtimes are switched on only
  by the environment (`FLEET_RUNTIME_OPENCODE=1` with `FLEET_OPENCODE_BIN` and the
  rest of the opencode settings, `FLEET_INBOX_INDEX`), and the locale (`LANG`,
  `LC_*`) decides what a session's tools can print. Add them to the unit by hand
  and every later run, an update included, writes them back; without this an update
  would turn the runtime off. Every `FLEET_*` name and `LANG` / `LC_*` is kept, and
  nothing else is: a variable of any other name is not carried. `FLEET_STATE_DIR`
  and `FLEET_CONFIG` are the layout's: a different value in the unit is reset, and
  the run says so. The run prints the names it kept (never the values), and
  `muster service status` lists the same names in an `env` row.
- **Loopback only.** `--addr` takes a loopback `HOST:PORT` with a fixed port; any
  other address is refused. Putting the service on a network interface is step 3
  of the manual procedure.
- **`doctor` fails, the service is not started.** The unit and files stay;
  fix the failing row and run it again. `--no-start` writes everything and runs
  `doctor` without loading or starting the unit.
- **Linux, no one logged in.** A user service runs while its user has a session.
  To have it run at boot, `loginctl enable-linger` (the command prints this; it
  does not change the account's setting).

```sh
muster service status        # unit present, loaded, running, healthy, which binary; exit 1 unless all hold
muster service uninstall     # unload and remove the unit; configuration, tokens, state and binary stay
muster service uninstall --purge   # ...and delete them (the tokens go too)
```

Not covered, and continuing in the manual procedure: peers (steps 5-6 of it, the
two-sided enrolment), inbox delivery (step 7), tmux 3.7 or newer (step 8), and
Windows ([its own section](#windows)). The command prints these when it finishes.

## From the release host

On a machine that installs and updates its apps from the company release host, muster comes from there too, as a
final release only. Needs `curl` or `wget`, `sha256sum` or `shasum`, and tmux (the service needs it):

```sh
curl -fsSL <host>/muster/install.sh | sh                  # the newest final
curl -fsSL <host>/muster/install.sh | sh -s -- 0.4.0      # an exact version
```

It checks the download against the version's `SHA256SUMS`, then runs that binary's
[`muster service install`](#the-short-way-muster-service-install), so what is above applies unchanged: a new machine
gets a service, an existing one keeps its configuration and gets the new binary and a restart, and the command ends
by asking the service whether it is healthy. If the new version does not come up, the previous one is put back and
the installer exits 1. npm remains the other way in; [`release-host.md`](release-host.md) says how a final reaches
the host.

A host that asks for a sign-in answers 401 to an unsigned read, the installer's own download included. Give the
installer the read token in `DL_TOKEN` and it sends it as a bearer header on every read it makes (the newest-version
file, the sums, the build). Fetch the installer the same way, through curl's standard input, so the token is never an
argument that another account on the machine can read in the process list:

```sh
export DL_TOKEN=<read token>
printf 'header = "Authorization: Bearer %s"\n' "$DL_TOKEN" | curl -fsSL -K - <host>/muster/install.sh | sh
```

`DL_TOKEN` needs `curl`: with only `wget` the installer refuses, because wget has no way to take a header off the
command line.

### Kept current by the host's update client

A machine that installed muster from the host is also kept on the newest final by the host's update client (its
periodic sync, or its update command for one app). The client updates an app on its own only when the app's manifest
says how to do that safely, and `.github/downloads/manifest.json` does, in an `update` block of three shell commands.
The client runs each with `sh -c`, as the user. The update itself is the installer above: it runs the new binary's
`muster service install`, which restarts the service and waits for health, and puts the previous version back when
the new one does not come up. The block only tells the client what is installed, when it may start and whether the
result works.

| Command | Runs | The client reads |
|---|---|---|
| `version` | `~/.local/bin/muster --version`, cut down to `X.Y.Z` | The release the service runs: the copy `muster service install` puts at that path, never whichever `muster` comes first on `PATH` (an npm launcher, a build in a checkout). It exits 1 and prints nothing when that file is missing, does not run, or names no release tag (an unstamped build, a candidate), and the client then falls back to what it recorded. |
| `busy` | Looks in the process table for a `muster ... service install` | Exit 75: an install or update is running, try later. Exit 0: free. Any other exit: the client cannot tell and does not update on that run. |
| `health` | `~/.local/bin/muster service status` | Exit 0: unit present, loaded, running, and `/v1/health` answering. Run after the installer; a failure blocks that version on this machine (never retried by the client) and shows the status rows. |

- **`busy` is not "muster is running".** `muster serve` is the normal state of every machine; if it counted, the
  client would report "waiting" on every machine for ever. Only a command line that *starts* with the binary and then
  says `service install` counts, which is what the installer's step 4 and a hand-run `muster service install` look
  like. The download and checksum steps before it are not visible in the process table, so `busy` does not hold back
  an update while another is still downloading.
- **A failed update leaves the previous version running.** That is the installer's put-back (above), not the block's.
  A `health` failure after an installer that exited 0 blocks the version but cannot put anything back; the installer
  has already run the same `muster service status` and would have exited 1 first.
- **Changing a command** means changing it where the client reads it: the manifest is published with each final, so a
  fix reaches machines with the next final. `release_downloads_test.go` runs the three commands as written
  (`TestUpdateVersionIsTheBinaryTheServiceRuns`, `TestUpdateBusyIsAnInstallInProgressAndNeverTheRunningService`).

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
   resulting set is printed as `{principal, grants}`. It edits the file only, and
   a running service applies the change within seconds with no restart (see
   *Changing principals and peers while it runs*, below); confirm with `whoami`
   or the `config.reload` row. It is local to the machine and has no HTTP route. `human-relay` is refused unless
   `--allow-human-relay` is given, which prints the shared-principal warning.
   Rows: `config.load`, `config.reload`, `token.source`, `principals.supervisor`,
   `principals.relay`, `local.mutations`.

   The empty document is the whole starting config: a setting that is absent
   takes its default, and the install writes none. One default is worth knowing
   before the first client connects: `maxInputBytes`, the limit on a create
   `prompt` and an `input` `text`, is **32768** bytes (1024 before muster #412),
   enough for a real briefing. Set it in this file to move it either way — a
   smaller value refuses more — and restart, because it is read at start;
   `muster doctor` validates it (row `config.load`).

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

   **A client that should see only this machine** — a second UI beside the main
   one, say — gets a principal with `"scope": "local"`:

   ```json
   { "name": "second-ui", "token": "…", "grants": ["read", "create", "send"], "scope": "local" }
   ```

   Its fleet-scoped reads are answered for this machine alone, a request naming
   another machine is refused (`reason: "local-only"`), `GET /v1/machines` lists
   only this machine, and `GET /v1/whoami` says `scope: "local"`. No other
   principal changes, and `muster principal list` marks the pinned one. It is
   applied within seconds like any principal edit; `"fleet"` or no `scope` is
   the default. See *Scope* in [`api.md`](api.md).

   The address is one **you** confirmed reachable from a, never b's own idea of
   its name. A peer known by several addresses takes `"urls": [...]` in place of
   `"url"`, tried in order. Rows: `peer.self-credential`, `peer.<m>.url`,
   `peer.<m>.credential`, `peer.<m>.reachable`, `peer.<m>.grants`.

   With more than a few machines, do not hand-write peers: have the owner sign
   one machine list and point each machine's `machineList` block at it
   (`shadow` first, read the `machine-list` row, then `prefer` or `only`). A
   newer list can also be handed to a running machine by a peer you grant
   `push-list` (`muster machine-list push`), and applies at once. See
   [`machine-list.md`](machine-list.md).

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
   when the binary is reached through a symlink; **under an npm install,
   `<prefix>` is the npm prefix** — the `--prefix` of `npm install -g`, the one
   whose `bin/muster` is the launcher — not the platform package the binary runs
   from, so both install shapes share `<prefix>/libexec/muster/modules`. A
   package manager that keeps its packages in a store of its own (pnpm) has no
   such prefix: set `FLEET_MODULES_DIR`), one file per module named by
   the module, not writable by anyone but the service's account (on Unix: mode not
   group- or world-writable; on Windows: an access list that grants no other
   account, apart from system and administrators, a write, delete or
   change-permissions right), and list the name. A name with
   no executable is logged once and skipped: a machine that could not fetch the
   module is a supported state. It is also reported by `GET /v1/health` under
   `modules` (`enabled`, `loaded`, `skipped` with a reason — absent when no module
   is configured), so an updater or a monitor can fail on a skipped module without
   reading the journal. `scripts/deploy.sh` can install one for you
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
   - prefer **tmux 3.7 or newer**. The terminal path pastes with bracketed paste,
     and from 3.7 tmux can report that the pane asked for it
     (`#{bracket_paste_flag}`), so the driver checks before it pastes. Older
     tmux (measured on 3.4, 3.5a, 3.6 and 3.6a; most distribution packages) reads
     that variable empty. There the driver pastes with `paste-buffer -p` and no
     pre-check — tmux brackets exactly when the pane asked — and relies on the
     composer read-back before it presses Enter. Sends work, proven for one
     runtime build by `muster compat` run against that tmux (check `G2`); the
     row `runtime.tmux-version` is a `warn` naming what is not being checked.
     To get the check back, install tmux 3.7 or newer and point `FLEET_TMUX_BIN`
     at it. A development build whose version cannot be read, or a 3.7+ tmux that
     prints nothing, still refuses every send;
   - on macOS, load the service into the user's **Background** launchd domain
     (`launchctl bootstrap user/$(id -u) …`, with `LimitLoadToSessionType=Background`
     in the plist), not `gui/<uid>` and not from a Terminal window. Every session
     inherits the security session of the process that started the session server;
     one started from the desktop (Aqua) context can move an agent runtime's saved
     login into the login Keychain, which a Background session cannot read. The
     service refuses to start a server from any other context (`reason:
     "unsafe_server_context"`) and `GET /v1/health` shows `serverContext` for both
     the service and the running server; `FLEET_TMUX_ALLOW_DESKTOP_SERVER=1` is the
     explicit override;
   - the default runtime offers a sandbox for the agent it starts (muster #345) on a
     host where the agent CLI was measured to start and authenticate inside the
     profile — `GET /v1/runtimes` shows `sandbox` there, or `sandboxUnavailable`
     with the reason where it does not (most often: the agent's login is in the
     login keychain, which the profile does not open). On Linux (muster #439) it
     needs kernel 6.12 or newer with `landlock` in `/sys/kernel/security/lsm`, and
     an agent whose install and interpreter the policy can read (see "What a Linux
     machine reports about the sandbox", below; row `sandbox.host` says what the host
     itself can offer, whether or not the runtime is enabled). `FLEET_TMUX_SANDBOX=off`
     stops the runtime offering it;
   - when `FLEET_RUNTIME_OPENCODE=1`, set `FLEET_OPENCODE_BIN` to the opencode
     binary's absolute path, for the same reason (row: `runtime.opencode`,
     which resolves the binary and never runs it; row `runtime.opencode.servers`
     counts server processes running with no session record, a `warn` that a
     restart of the service clears; row `runtime.opencode.dirs` counts session
     directories no record names, a `warn` whose leftovers the service removes by
     itself (at its next start, and hourly while it runs) and whose directories that
     may hold a conversation it never does; row `runtime.opencode.sessions` lists the
     sessions that run no server and that a restart did not or cannot relaunch (created with
     caller-supplied environment, or marked `relaunch failed` because the last start of the service
     could not relaunch them — the next start that can takes the mark off), each
     with its age and the size of the directory it holds, a `warn` that removes nothing:
     closing the session is what removes its record and directory, and that stays with
     the caller that created it; row
     `runtime.opencode.providers` shows which providers, and which models of them,
     `FLEET_OPENCODE_PROVIDERS` lets a session use, and fails on an entry the service
     will refuse to start on; row `runtime.opencode.grant` reports a ChatGPT sign-in
     made with `muster grant login`, below). A runtime that is enabled but
     not usable does not stop the service: it is listed on `GET /v1/runtimes`
     as `unavailable` with a `cause`, and a version check that merely timed out
     under load is retried until it answers;
   - install the agent program the terminal runtime starts (`claude`) where the
     login shell finds it. Sessions are started through `$SHELL -lic`, so that
     shell's `PATH` decides, not the unit's. Without it the runtime is listed
     `unavailable` / `not-installed` on `GET /v1/runtimes` and a create is refused
     with `runtime-unavailable`; installing it needs no restart (row:
     `runtime.agent-program`);
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

### Changing principals and peers while it runs

Enrolling a client, changing or revoking its grants, and adding, changing or
removing a peer take effect on the running service **without a restart**, and
without dropping anyone else's connections. The service watches the config file
it was started from (a content check every two seconds); `kill -HUP` applies it
at once. `muster principal add|grant|revoke` and a hand edit are the same thing
to it.

- A new or changed principal works from its next request. A **revoked** one gets
  a `401` from its next request on, and the event streams and parked long polls
  it had open are closed; every other principal's streams are untouched. A
  principal whose token or key is replaced loses its open streams the same way;
  one whose grants change but still includes `read` keeps them.
- The file is applied **as a whole or not at all.** If it does not parse, names
  an unknown grant, lists this machine as its own peer, or fails any other check
  the start applies, the service keeps the last good table, logs the reason, and
  records it for `muster doctor` (row `config.reload`: `warn`, with the reason).
  Fix the file and the next check applies it.
- Only the two tables move. `trustRoots`, `defaultRuntime`, `sessionEnv`, `providerCredentials`,
  `opencode` (which file holds [the machine's opencode agents](#agents-every-opencode-session-on-this-machine-receives)),
  `models` ([the model lists a client may offer](#models-a-client-may-offer-for-a-create)),
  `maxInputBytes`, `gateRemoteControlInput`, `closedRetentionDays`,
  `inputAuditRetentionDays`, `profilingEndpoint`, `refuseRecordReads` and `machineList` are read at start, so an edit that
  changes one of them is **refused whole**, naming it, and none of that edit —
  principals and peers included — is applied until the service restarts. The
  refusal also says whether the restart it points to would work: it runs the same
  file checks as `muster doctor`'s `config.load` row and as the start itself (the
  principal table, trust roots, `sessionEnv`, `providerCredentials`, `opencode.agents`, `models`, a `defaultRuntime` the machine does
  not run, `maxInputBytes`, the retention days, a `machineList` in mode `only`
  that does not verify, the peer list), and ends either "the file passes start-up
  validation" or "a restart would fail: <the start-up error>". Read that before
  restarting a service that a process manager keeps alive: a file that does not
  start is restarted again every few seconds. The three read one function, so
  they cannot disagree about a setting they all check (#374). It is the file's
  answer (and the environment's) only: whether the default runtime's driver is
  up at that moment (opencode enabled but its binary missing, say) only the
  running process can say, and is still found out at the start. The bind
  addresses and the state directory come from the environment, so no edit to
  this file can change them.
- The owner-signed machine list is re-read too (#354). The service watches the
  list file as well as the config, and applies a new list the same way it applies
  a peer edit: build every new peer, then swap. A list replaces the one in force
  only if it verifies against the pinned signer **and** carries a higher sequence
  than the list in force; the peers and the machine principals it implies follow
  it, and only a revoked or replaced machine credential loses its open streams.
  A list that does not verify, an older genuine one replayed, or one re-signed at
  the same sequence is **refused as a whole**: the last good list stays, the
  reason is logged, and `config.reload` goes to `warn` with it (the config itself
  may have been applied in the same pass — a bad list never holds back an
  unrelated principal edit). An unchanged list is not re-applied. Signing the next
  list clears the warning. A list a peer pushes (`PUT /v1/machine-list`, #352)
  goes through the same code in the request itself, so the pusher gets the real
  outcome; the poll that follows finds nothing new.
- The `machineList` block itself — its `mode`, `path`, `fleet` and pinned
  `signer` — stays **restart-only**. The path and the pin say what the service
  trusts, and an edit to a trust root takes effect only when a person restarts the
  service, like `trustRoots`. What a reload picks up is a new list *at the same
  path*, so the usual routine is: sign the new list, write it over the old one
  atomically (write beside it, then rename), and within a poll interval the
  running service has it.
- `config.reload` reads a status file the service writes in `FLEET_STATE_DIR`.
  With no state directory the reload still works; the row says it cannot compare.
  The service rewrites that file whole when it starts (`appliedReason: "start"`),
  naming the file it started with and the machine list it loaded, so a restart
  clears a refusal the restart resolved; a reload after that moves it on.

### Agents every opencode session on this machine receives

An `opencode` session runs with a home and configuration directories of its own,
so it never reads the service user's opencode configuration, and an `agent` a
create names must be one that session's runtime lists: its built-in agents plus
the project agents under the working directory. To make one agent available on
every session of this machine without asking each repo to commit it, point
`opencode.agents` at a JSON file in the file `FLEET_CONFIG` names:

```json
{ "opencode": { "agents": "/etc/muster/opencode-agents.json" } }
```

The file holds one top-level key, `agent`, whose value is the object opencode's
own configuration carries under that name: one definition per agent.

```json
{
  "agent": {
    "reviewer": {
      "description": "Reviews a change and reports findings, never edits",
      "mode": "primary",
      "model": "provider/model",
      "prompt": "You review. You do not edit files."
    }
  }
}
```

- **The path is declared at start; the content is read on every create and every
  relaunch.** Editing an agent, adding one, or creating the file after the service
  started needs no restart. A session that is already running keeps the
  definition it started with, because its runtime read it at its own start.
  Changing *which file* is named is a restart-only edit, like `sessionEnv`.
  The path must be absolute; whether the file exists is not checked at start.
- **A file that cannot be used refuses the create with the reason** (`unsupported`:
  an operator must repair it). That covers a missing or unreadable file, anything
  that is not a JSON object, an agent defined twice, a definition that is not an
  object, a file over 1 MiB, and any top-level key other than `agent`. The last is
  deliberate: providers, permissions and tool servers are set by the service, and a
  file that could set them would be a way around that policy. A definition the
  runtime itself rejects (a `mode` that is not `subagent`, `primary` or `all`, say)
  stops that session's runtime from starting, and the create is refused naming the error
  the runtime logged. A create is never run as the default agent instead. A session
  that cannot be relaunched after a restart for this reason is listed `unknown`
  with the reason.
- **A project agent of the same name wins, field by field.** The operator's
  definition is the session's global one. A project's `.opencode/agents/<name>.md`
  file, or an `agent` entry in the project's `opencode.json`, overrides each field
  it sets, and leaves the operator's `model`, `temperature` and the rest in force
  where it sets nothing. An inline `OPENCODE_CONFIG_CONTENT` (a `sessionEnv` entry
  can still supply one) ranks above both, also field by field; one that carries no
  `agent` entry leaves these agents alone. This is how opencode 1.18 merges, measured
  with an isolated home; the driver's gated integration test
  (`FLEET_OPENCODE_INTEGRATION=1`) pins it. Naming a built-in agent (`build`, `plan`)
  overrides that agent for every session.
- **It is not a secret channel.** The definition is written into the session's own
  configuration and is as visible to the session as any agent prompt; its own
  `permission` and `tools` entries apply as they do for any agent. In a `bypass`
  session the sandbox, not the agent file, is the boundary.
- It has no effect on a shared-mode driver (a test seam that talks to a server it
  did not start), which has no configuration of its own to write them into.

### A ChatGPT subscription for every opencode session

An operator can sign this machine in to a ChatGPT subscription (Plus or Pro) **once**,
and every `opencode` session the service starts can then use the `openai` provider
under that sign-in, with no sign-in per session. It is optional and off until someone
runs the command: a machine that never does behaves exactly as before.

```sh
muster grant login          # prints a web address and a code; open it on any device, approve
muster grant import PATH    # or take the `openai` sign-in out of an opencode auth.json
muster grant status         # ok · revoked · signed out; exits 1 unless ok
muster grant logout         # delete the stored sign-in
```

What it does, and why it is shaped this way (`docs/adr/451-*`):

- The sign-in is stored owner-only in the `grants` directory of `FLEET_STATE_DIR`, so
  the service needs a state directory for it. The running service **alone** renews it,
  before it lapses: the refresh token rotates on every use, so two holders of it cannot
  both be right. It notices a `login`, `import` or `logout` within half a minute.
- A session's own `auth.json` holds only the current **access token**, with a
  placeholder where the refresh token goes, and it is rewritten in place whenever
  the service renews. The runtime reads that file again before every provider request, so
  a renewal reaches a running session on its next turn: nothing is relaunched.
  The access token is in the session's reach, as a model-provider key always is; the
  sandbox is what confines the filesystem. A session never holds the refresh token, so
  it cannot keep the sign-in alive after it ends.
- **A sign-in makes `openai` usable; it does not allow it.** A session gets the token
  only where `FLEET_OPENCODE_PROVIDERS` allows `openai` (or names no provider at all,
  the default that leaves every provider but the built-in hosted one open). A machine
  that names only other providers hands the credential to nobody.
- **After `import`, sign the source file out.** The service rotates the refresh token
  from then on, so the source copy stops working the first time it renews.
- A sign-in the provider has revoked, or that cannot be renewed, is visible rather than
  a run of failed turns: `muster doctor` row `runtime.opencode.grant` (a `warn` that
  says to sign in again; a `fail` if the stored file is readable by others), the
  `grants` field of `GET /v1/runtimes`, and the reason on a create the missing
  provider refuses. A renewal that fails for a reason that is not the sign-in's fault
  (the provider unreachable) is `degraded` and retried with a backoff; it never revokes.
- All sessions on a machine share one plan's usage windows. When the window runs out,
  the runtime does not fail a turn: it keeps retrying the provider's refusal (watched
  for over three minutes: attempt 7, the delay doubling, no failed turn ever recorded), so
  every session on the subscription just waits. That is one state of the sign-in, and
  it is reported as one: the `quota` of the grant on `GET /v1/runtimes` and a `warn` on
  `runtime.opencode.grant` while the provider keeps refusing the sign-in, gone when it
  stops. The cause is not parsed (an exhausted window reads like any other persistent
  refusal; the provider's own message says which); what a real exhausted window looks like
  on a live subscription is not yet measured.
- **Terms.** Whether one subscription may be shared by automated sessions is a
  question of the provider's terms, and for the operator to settle; muster does not
  decide it. Nothing is signed in until someone chooses to.

### Which credential an openai session runs on

A machine can hold **two** credentials for the `openai` provider: the ChatGPT sign-in
above, and an API key (a `sessionEnv` entry supplying `OPENAI_API_KEY`). They are
separate accounts with separate metering. The subscription has plan usage windows shared
by every session on it; the key is billed per token. The runtime, left to itself, takes
the sign-in and ignores the key (measured on 1.18.3; `docs/adr/458-*`), and nobody chose
that. So the operator chooses, in the file `FLEET_CONFIG` names:

```json
{
  "providerCredentials": [
    { "provider": "openai", "use": "api-key",      "appliesTo": { "markers": ["metered"] } },
    { "provider": "openai", "use": "subscription" }
  ]
}
```

- **An entry names the provider (`openai`), the credential (`"subscription"` or
  `"api-key"`) and an optional `appliesTo`** with `runtimes`, `agents` and `markers`,
  the same shape and the same matching as a `sessionEnv` entry's. **The first entry
  whose scope matches a session decides**, in file order. An entry written after one
  that already matches everything it could is refused at start, naming it: it would do
  nothing. A misspelt runtime id is refused the same way.
- **It is enforced at every create and every relaunch, by what is absent.** On
  `subscription` the session gets the sign-in's access token and the key is removed
  from its environment. On `api-key` the session gets no sign-in (one an earlier launch
  wrote is taken out of its `auth.json`) and keeps the key. The other credential never
  reaches the session, so nothing falls back to whichever the runtime would have found.
- **A named credential that is missing refuses the create, with the reason**, when the
  session is certain to need the provider: it pins a `provider/model` on `openai`, or
  `FLEET_OPENCODE_PROVIDERS` names `openai` alone. A session that can run on another
  provider is not refused for a credential it may never use; it simply gets neither.
  The key is not used in place of a missing sign-in and the sign-in is not used in
  place of a missing key.
- **A create does not choose.** Under a `subscription` rule, a create whose own `env`
  carries `OPENAI_API_KEY` is refused by name. Under an `api-key` rule there is
  nothing for it to override. **Supply the key with a `sessionEnv` entry**: a create's
  own `env` values are not kept, so a key sent that way does not come back at a
  relaunch, which an `api-key` rule then refuses for a session bound for `openai`,
  naming the missing variable.
- **With no rule that matches:** a usable sign-in is used and the key is withheld; with
  no usable sign-in, the key is used. That is what the runtime did before, now decided
  by the service and so unable to change with a runtime release, and it adds no
  per-token spend. `muster doctor` row `runtime.opencode.credentials` warns when both
  credentials reach sessions and no rule chooses between them, and when a rule names a
  credential this machine does not hold.
- **A rule only chooses between credentials.** Whether `openai` may be used at all is
  `FLEET_OPENCODE_PROVIDERS`'s decision, unchanged: where it does not permit `openai`,
  the rule applies to no session and nothing is stripped or written.
- **Usage names the kind.** A session's `usage.credential` is `subscription` or
  `api-key` (`docs/api.md`), so spend on the two accounts is never added together.
- The declaration is read at start and changing it needs a restart, like `sessionEnv`;
  a reload that changes it is refused whole.

### Models a client may offer for a create

`GET /v1/runtimes` carries a `models` list on every row, so a client can offer a
picker instead of a free-text box (`docs/api.md`, `GET /v1/runtimes`). The
`opencode` runtime reports what its own runtime lists, filtered by the provider
policy, and needs no configuration. The Claude Code runtime has no catalogue to
ask, so it reports the aliases its CLI documents (`opus`, `sonnet`, `haiku`). To
offer other ids, declare them in the file `FLEET_CONFIG` names, keyed by runtime
id:

```json
{ "models": { "claude-code-tmux": ["opus", "sonnet", "haiku", "claude-opus-4-1"] } }
```

- The list is `source: "declared"`: a client still accepts a value outside it,
  because nothing checks a typed model against the CLI before the first turn.
- The `opencode` runtime takes no declared list (a second source for an answer it
  already has, which would drift from what a create accepts), and no other
  runtime id is accepted. An id is non-empty, does not start with `-`, has no
  whitespace and is listed once; a list that breaks this stops the start and shows
  as a `config.load` failure in `muster doctor`.
- It is read at start. A reload that changes it is refused whole, like the other
  start-only settings above.

### Sampling a slow service: the profiling endpoint

Off by default. To take a CPU profile of a running release build, without
rebuilding it and without any symbol file:

1. Give one principal the **`debug`** grant (`muster principal grant <name> debug`).
   It is not in the set `muster service install` enrols, and no other grant
   implies it.
2. Add `"profilingEndpoint": true` to the config file and **restart** the
   service. It is read at start; an edit made while the service runs is refused
   whole, naming it, like the other start-time settings above. The start logs a
   line saying the endpoint is on.
3. Fetch a profile with the principal's token (a bare `go tool pprof <url>`
   cannot send the header), then read it:

   ```sh
   curl -H "Authorization: Bearer $(cat TOKEN_FILE)" -o cpu.pb.gz \
     "http://127.0.0.1:PORT/v1/debug/pprof/profile?seconds=30"
   go tool pprof -top cpu.pb.gz
   ```

4. When done, remove `profilingEndpoint` (or set it `false`) and restart. The
   grant may stay: with the switch off it is refused with a `501` that names
   the switch.

Every request is in the service log as `audit: … verb=debug …`, with `took=`.
The same log's `create` lines now end with `took=<ms>` and `phase.*` fields, so a
slow create can be read from the log before any profile is needed. Contract:
[`spec/api-http.md`](spec/api-http.md) §3.1.2 and §5.

### Refusing record reads on this machine

`GET …/sessions/{id}/record` returns a session's whole record, and it is guarded by
the **`record`** grant, which nothing implies and nobody holds by default
(`muster principal grant <name> record`; it is not in the set `muster service
install` enrols). To refuse the route outright on this machine, whatever grants are
held, add `"refuseRecordReads": true` to the config file and **restart** the service
(it is read at start; an edit made while the service runs is refused whole, naming
it). The start logs a line saying the route is refused. Every call then answers
`501 unsupported` with the reason `record-off`, for a local session, a peer target
and a call relayed in alike; the grant is checked first, so a caller without it
learns nothing about how the machine is set up.

This is the only lever on a machine with **no principal table**, where a valid
token is the credential and reads the record as it reads everything else. Every
read the route serves is in the service log as `audit: content-read …` — who, which
session, how many entries, never the content. Contract:
[`spec/api-http.md`](spec/api-http.md) §3.2 and §5, and
[the decision](adr/441-a-sessions-full-record-is-read-under-its-own-grant.md).

## Windows

muster runs on Windows as a per-user service, started by a scheduled task. Read
[the decision](adr/303-windows-ships-without-an-interactive-terminal-driver.md)
before relying on it for anything but the subprocess runtime: **a Windows
machine has no interactive-terminal driver.** What it does have is the
`opencode` runtime, driven as a child process, and the whole HTTP API, the
principal table and federation. The procedure above applies; this section is
what differs.

### What a Linux machine reports about the sandbox

A Linux host is offered the sandbox (muster #439, mechanism `linux-landlock-seccomp`) only
when all of this is true, and `GET /v1/runtimes` shows `sandboxUnavailable` with the first
thing that is not:

- the machine is amd64 or arm64;
- the kernel is **6.12 or newer** (Landlock ABI 6) and `landlock` is among the active security
  modules: `cat /sys/kernel/security/lsm` lists it. A kernel built without it, or booted with an
  `lsm=` list that leaves it out, or a container whose seccomp profile blocks the system call,
  each get their own reason;
- a child process confined by the same mechanism was seen unable to read outside its grants,
  create a unix or UDP socket, connect to loopback under a closed network, touch shared memory,
  read another process's environment or signal the service. This is run once when the service
  starts, through the service's own executable, so that executable must be the file the service
  was started from;
- for the terminal runtime, the agent CLI, run through the same launch builder a create uses,
  reports a logged-in account under the policy. An agent installed under a version manager's
  directory in the home (the policy does not open the home), or as a confined package, cannot be
  read by its own session and is refused with that reason; install it where the system's
  directories are;
- for the opencode runtime (muster #456), its own `--version`, started through the launcher under
  the strictest plan a create can ask for (a closed network), answers. A runtime whose interpreter
  lives outside the directories the policy opens (a version manager's shim, an interpreter in the
  home), or that reads a file the policy denies, is refused with the launcher's reason or the
  runtime's last line in `sandboxUnavailable`, before any create; grant the directory it needs with
  `FLEET_OPENCODE_SANDBOX_READ`, or install it where the system's directories are. The answer is
  kept five minutes when it was good and thirty seconds when it was not (longer, in the
  background, while it keeps failing), so a runtime installed or fixed while the service runs is
  offered again without a restart.

`muster doctor` shows the same facts without a running service (row `sandbox.host`, muster #457),
so an operator does not have to read the capability's reason or `cat /sys/kernel/security/lsm`.
It reads the host check the capability reads, so the two cannot disagree, and it answers offline:

- `pass`: the host can enforce the mechanism. The detail lists what was seen: the architecture,
  whether the service has named its launcher, the Landlock ABI against the floor (6, Linux 6.12),
  the active security modules, and that the confined self-test passed. This is the host's half
  only: the runtime measures its own agent on top of it, so `GET /v1/runtimes` can still say
  `sandboxUnavailable` for a reason about the agent.
- `warn`: the platform has a mechanism and this host refuses it. The summary is the reason the
  capability carries, word for word; the detail is every fact, including the ones read after the
  first refusal (a kernel below the floor also shows whether `landlock` is among the modules). The
  self-test is shown as not run when an earlier check refused, because it needs a launcher and a
  kernel with Landlock to run on. A session that asks for no sandbox is unaffected; this row never
  fails the doctor.
- `skip`, reading `n/a`: the platform has no mechanism (Windows, or any system that is neither
  macOS nor Linux).

On macOS the facts are whether `/usr/bin/sandbox-exec` is present and whether a profile was applied
to a throwaway process, which is what finds a service that itself runs inside a sandbox. On Linux the
self-test starts a child, so `--skip=sandbox.host` skips the row and does not run it.

What a session on Linux gets that a macOS one does not, and the reverse: it cannot create a unix
socket at all (a pre-connected pair still works), `closed` means no outbound connection even to
loopback, there is no shared memory directory, and it can read the system's `/etc`, `/usr`,
`/proc` and `/sys` but nothing of the home. Keep a token or a key out of `/etc`, which sessions
can read, as on macOS. The reasoning is in
[ADR 439](adr/439-the-linux-sandbox-is-landlock-and-seccomp-launched-by-the-service-binary.md).

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

`npm i -g @futurelastic/muster` installs on Windows as it does anywhere else:
the launcher pulls `@futurelastic/muster-win32-x64` or `-win32-arm64` to match
Node's architecture, and `muster --version` names the release. Every release
proves that twice (`release-npm.yml`): before anything is published, each
Windows binary runs on a runner of its own architecture, and after the publish
the launcher is installed from the registry on both and run. A release cut from
a source older than the Windows work has no Windows build, so it publishes no
Windows package and its launcher lists none. Two more routes work without the
registry:

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

- **The registry install is proved only as far as `--version`.** The release
  installs the published launcher on a bare runner and runs `muster --version`.
  The service, the scheduled task, enrolment and sessions are exercised by CI's
  end-to-end job, from locally assembled packages rather than the registry.
- **arm64 is run only that far.** The release runs the arm64 binary, and installs
  from the registry, on an arm64 Windows runner; the end-to-end job (service,
  task, sessions) runs on `amd64` only.
- **The WSL peer route has not been run.** The Windows-to-WSL direction relies on
  WSL's localhost forwarding; the reverse direction depends on the WSL network
  mode. Treat both as configuration to prove on the machine.
- **The scheduled task is exercised on a CI runner,** which may not give it an
  interactive desktop; CI says which start path it took. A task on a signed-in
  desktop has not been observed there.
- **No Windows service (SCM) form exists.** A task runs only while the user is
  signed in.
