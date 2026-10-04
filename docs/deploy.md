# Deploy — from a merged commit to a running service

This is the document `.github/project.yml` refers to. Its `exposure: self` is
justified by one sentence: *merging to trunk reaches nothing by itself; the
service changes only when a human runs the documented build-install-restart
procedure on each machine.* Until now that procedure was not written down —
only [`scripts/deploy.sh`](../scripts/deploy.sh) existed, undiscoverable from
the descriptor and unable to target the machine most likely to need it. This
page is the missing pointer, and the missing case.

If you take one thing from this page: **`scripts/deploy.sh` is the procedure.**
Read its header before reading further — it explains, in the same order as
below, why each step exists and what it refuses to do silently.

**This page assumes a service that already runs.** Every step below backs up,
replaces, restarts or asks something that has to exist first. For a machine
that has never run this, start with [`install.md`](install.md) — from nothing
to a running service — and come back here for every change after that.

## The procedure

0. **Back up what you are about to replace, before anything else.**
   `scripts/fleet-backup.sh` captures the installed binary (checksum-verified
   against its source right after copying), the state directory, and the
   revision the running service currently reports — refusing rather than
   backing up a service it cannot identify if health does not answer with a
   build. This is not optional housekeeping: a build stamped `modified: true`
   has no commit that reproduces it, and the only way back from a bad deploy
   of one is a copy of the binary itself. See `docs/adr/123-backup-stays-separate-from-deploy.md`
   for why this is a separate command rather than something `deploy.sh`
   refuses to run without — the short version is that "no backup" is a policy
   this script would have to invent a default for, not a fact it can check the
   way a dirty tree is a fact.
1. **Build with the version-control stamp intact.** A binary built from a
   modified tree has no identity and can never be compared against a peer or
   against itself. The script refuses a dirty tree by default; do not override
   that on a deploy you intend to keep. It also stamps the release version
   (`git describe --tags`) at link time — the toolchain records the revision
   on its own but never the tag — so `/v1/health` can report `build.version`
   for clients enforcing a minimum version (#161).
2. **Install atomically** — write beside the target and rename into place.
   Writing over a running binary is how you get a half-written executable and
   a service that will not start.
3. **Restart through the machine's own service manager.** The script never
   invents one; it runs whatever command you give it and stops there.
4. **Verify by asking the running service what it is**, and compare that
   answer to the commit just built — the revision, and then the version stamp. A deploy that does not verify is a deploy
   that can silently not have happened — this is the step that makes it a
   deploy rather than a copy.
5. **Then the peer, and only then.** One machine deployed is a fleet at two
   revisions, not a finished deploy.

**If a deploy needs undoing, `scripts/fleet-revert.sh` is the other half of
step 0.** It checks the backup against its own manifest before touching
anything — a corrupt backup discovered during a rollback is the worst possible
moment to discover it — installs atomically, re-checksums, restarts, and polls
health the same way step 4 does, refusing to report success until the running
service reports the revision the backup recorded. It restores the **binary**
by default; the **state directory** only behind an explicit `--with-state`,
because state is forward-compatible far more often than not and rolling it
back can discard real session records the new binary wrote correctly. Rolling
back code and rolling back data are different decisions and are not spelled
the same way here.

**A verified deploy is not yet a usable one, for `keys` specifically
(muster #68).** `deliversRawKeys: true` on a runtime is a statement about
what the driver can do, not about who may ask it to — the `keys` grant is
separate, denied by default, and nothing above grants it. A fresh deployment
that never ran `muster principal add ... --grants=...,keys` will verify
clean and then refuse every keypress with `401 ... does not hold the keys
grant`, which reads like a permissions bug rather than the setup step it
actually is. Across a peer relay it is two grants, on two machines: `keys` at
the far end that runs the key, `relay` at the near end that forwards the
request there — see api-http.md §3 for why fixing the first refusal does not
fix the call. `muster doctor --principal=<name>` names the near half as
rows `principals.supervisor` and `principals.relay`; the far half is that
peer's own `doctor` run.

**A verified deploy is not yet a usable inbox delivery path either
(muster #122).** `deliversToInbox: true` on `GET /v1/runtimes` is
contingent on `FLEET_INBOX_INDEX` being set on that machine — a deploy that
verifies clean but never sets that variable runs with #119's resolver nil,
and every delivery keeps falling through to the pane path exactly as #122
found it doing in production. Setting it is an operator step this script
does not perform and cannot: the directory it names, and what populates it,
are machine-local facts this repository never commits (`cmd/muster`'s
own doc comment names the variable; it does not name a value). Check
`deliversToInbox` after any deploy you expect this path to be live on,
rather than assuming a clean verify implies it — `muster doctor` reports
it as row `inbox.index`, run under the service's own environment.

**And `deliversToInbox: true` is still not a usable path unless the index
carries a permission-mode class (muster #148).** Each index entry now has
an optional `mode_class` naming the class its target session RUNS IN. Without
it a send cannot be attested and falls back to the terminal path — silently,
and while the flag still reads true. Until whatever populates that directory
emits the field, expect a correctly-deployed machine to use the inbox for
nothing at all. That is the intended state, not a fault: sending unattested is
what caused #148, where 206 sends in three days were reported delivered and
were in fact held by the receiver and dropped.

Two things to check on a deploy you expect this path to be live on:

- the index writer emits `mode_class`, and emits the class the session is
  actually running in — a *wrong* class is held exactly as firmly as a missing
  one, so a writer that guesses is worse than one that omits the field;
- the resolver's `inbox_index.unattestable_entry` counter stops growing as the
  writer rolls out. It exists precisely because "the field is not being
  written" and "the fix did not take" are otherwise indistinguishable from
  outside.

`muster doctor` counts both from the index itself, as row
`inbox.mode-class`: entries attestable, entries without a class, entries with
an unrecognised one. That is one reading of the index on disk; the counters
are what the running service actually met.

**The resolver's index counters are read from `GET /v1/health`
(muster #163),** under the terminal runtime's entry in `counters`, and
only on a machine with `FLEET_INBOX_INDEX` set — absent there means the
resolver is not wired, `0` means wired and not yet hit:

- `inbox_index.unattestable_entry` — resolves that found a live, matching
  entry with no usable `mode_class` (#148);
- `inbox_index.start_time_mismatch` — resolves that found an entry for the pid
  whose `started_at` names a different process run (#147). A value that grows
  with every send means the writer's start time never matches, which otherwise
  fails silently: a resolver error reads as "no inbox", and the send goes to the
  pane.

They are in memory like every other counter here, so "stops growing" means
between two readings with the same `startedAt`. They count lookups against the
index, not exits of the send path, so they are named apart from `inbox.*` and
are not part of the ADR-150 sum below.

**Whether the body rule is worth widening is read from `GET /v1/health`
(muster #150).** The terminal runtime's entry under `counters` carries one
`inbox.*` counter per exit of the inbox send path; the full list, and how they
sum, is in `docs/adr/150-count-inbox-fallbacks-before-widening.md`. The number
that decision needs is `inbox.attest_body_lookalike / inbox.attest_checked`,
summed across machines — independent of whether the index carries classes yet.
The counters are in memory: a reading covers only the window since that
machine's `startedAt`. `inbox.fallback_no_mode_class` is the same class rollout
seen per send rather than per index entry.

**Rolling out the live-lane input contract (muster #257) — read this before
the inbox section below.** Since #257 a session's live delivery lane is its only
input path for `/input`, and `auto` never tries the inbox: the inbox is used only
when a caller names it (`route: "inbox"`). Where the section below says an `auto`
send goes through the inbox or falls back from it, read it as superseded by this
paragraph; the rest of the inbox mechanics (the gate, the counters, the ledger)
apply to a **named** inbox send.

- **Before this reaches a machine:** change any caller that falls back to
  driving the multiplexer **directly** whenever this service answers with a
  refusal. Each new refusal (a terminal, `submit: false`, resume or replace
  request on a session with a live lane) would otherwise turn into a raw
  keystroke into the same session, the opposite of the contract. That caller
  lives outside this repository; its change must be running on every machine
  first. Upgrade peers together too: an entering machine built before #257 still
  sends a human relay's `auto` as `terminal`, which a new owner refuses on a live
  lane.
- **Release notes carry two behaviour changes:** an absent `submit` now means
  `true`, and a caller that asks for the terminal, `submit: false`, or a resume
  or replace on a session with a live lane is refused.
- **What to watch on `GET /v1/health`** (terminal runtime's `counters`, in
  memory since `startedAt`): `route.refused.lane_live` and its split
  `route.refused.lane_live.{terminal,submit_false,resume,replace,stranded_text}`
  — a total that keeps growing means callers still ask for the terminal;
  `route.refused.lane_lost` — sends whose lane stopped being usable in flight;
  `route.decided.auto.module` against `route.decided.auto.terminal` — the share
  of `auto` sends the lane carries (it should approach the share of sessions
  holding a live lane); and `route.auto_fallback`, which no longer exists.

**Turning the inbox route on (muster #184) — what has to be true first, in
this order.** Since #184 a send with no `route` is `auto`: anyone who does not
hold the `human-relay` grant goes through the inbox whenever the session can take
it. On a machine whose index emits no class that is nobody, so shipping this
changes nothing there — the terminal carries every message, now with the
`delivery.route` on the receipt and a label on every non-human message. Enabling
the inbox is the operator step that follows, and it has an order:

0. **A machine with no principal table needs one before this build (#196).**
   The inbox route is not available without a table: with `FLEET_INBOX_INDEX`
   set and `FLEET_CONFIG` unset, `muster` **refuses to start**, naming the
   table. The reason is the one step 1 turns on — who relays a person's messages
   has to be a grant the table holds, and a single-token machine has nothing
   but a header any bearer of the token can set to say so. **Before you install
   this build on a machine that sets `FLEET_INBOX_INDEX`, run `muster
   doctor` there, under the service's own environment: row
   `principals.human-relay` reads `fail` on exactly the machines that would
   refuse to start.** Then either write a table
   (`docs/install.md` step 5) and continue with step 1, or unset
   `FLEET_INBOX_INDEX` to keep every delivery on the terminal path. A machine with
   no table and no index is unaffected — it has no inbox route, so nothing is
   diverted into a peer message — and its single-token relay keeps working as it
   did.
1. **Give the `human-relay` grant to the principal that relays a person's
   messages, before any class is emitted.** Without it that principal is an
   ordinary sender: once the inbox is live its messages arrive as **peer
   messages**, which the receiving runtime treats as not coming from the user and
   which cannot grant escalation — measured, a receiver refused an operator's
   approval for exactly this reason. `muster doctor` runs offline and does
   not read the running service's counters or its callers, so it cannot see who
   actually relays a person's messages — but it can see the configuration that
   makes the failure possible: row `principals.human-relay` warns when
   `FLEET_INBOX_INDEX` is set, a principal table is configured and no principal
   holds the grant (`--skip=principals.human-relay` where every caller is an
   agent). It is a warning, never a failure — nothing is broken until an index
   also emits a class. Two things it does not cover: a pass means *someone*
   holds the grant, not that the holder is the principal that relays (a
   `--principal` does not narrow it — that flag names the supervising client, and
   the grant is deliberately outside the supervisor set); and in single-token mode
   there is no grant to hold, so with an index set the row **fails** instead —
   see step 0.
2. **Have the index writer emit `mode_class`** (above). Until it does, a named
   `route: "inbox"` send is refused and this is a no-op.
3. **Take one live look** on throwaway sessions, from the receiver's side, before
   trusting the counters — the things below are precisely what no offline test can
   establish:
   - an agent's message sent with `route: "inbox"` arrives as a peer message
     carrying the sender's name and the runtime's own warning, and the receipt
     says `delivered` on `delivery.route: "inbox"`;
   - a human relay's `auto` message arrives as a plain, unlabelled user turn,
     receipt `delivery.route: "module"` on a session with a live lane and
     `"terminal"` otherwise;
   - an agent's `auto` message to a session with no live lane arrives through the
     terminal with its `[from: …]` line, receipt `terminal`;
   - which of `inbox.confirmed_by_envelope` and `inbox.confirmed_by_origin_body`
     moved — that is the transcript shape the runtime actually writes for a peer
     message, which this repository has never seen;
   - a receiver in the middle of a turn: how long its transcript takes to record
     the message, against the 4-second confirmation window.
4. **Then read the counters** on `GET /v1/health`, under the terminal runtime's
   entry, summed across machines and remembering they are in memory since that
   machine's `startedAt`:
   - the **decline rate** — `inbox.attempted` minus the sends that wrote. Every
     named inbox send the inbox declined before writing a byte (since #257 `auto`
     does not try the inbox, so `route.auto_fallback` is gone). Read the reasons
     in the `inbox.*` exits
     (`fallback_no_mode_class`, `fallback_no_transcript`, `fallback_dial_failed`,
     …); they sum to `inbox.attempted`, per ADR 150, with two more exits since
     #184 (`fallback_no_transcript`, `unknown_partial_write`);
   - the **unconfirmed rate** — `inbox.unconfirmed / inbox.written`. Written is
     the sum of confirmed and unconfirmed. Near zero is healthy. Near one means the
     matcher does not recognise what the runtime writes — every message is safe
     (`unknown`, never resent) but the path is not doing its job — or receivers are
     holding messages; step 3's transcript look tells which;
   - `route.guard.*` — follow-ups the ledger answered instead of sending again
     (`inbox_unconfirmed`), text kept out of the inbox because it was already
     stranded in the composer (`terminal_unconfirmed`), and earlier writes found
     recorded after all (`late_confirmed`).

The permission-mode class gate itself (#148) is **unchanged** by #184. Its
re-check, the evidence it rested on and what would reopen it are recorded in
`docs/adr/184-route-by-sender.md`: the short version is that the measurement that
prompted it covered no receiver running with permission prompts bypassed, which
is the case the gate exists for.

**One check this script — and `doctor` — cannot perform: whether the sender
label actually renders (muster #158).** `/input`'s optional `from` object
reaches the receiving side either as the envelope's sender-name attribute
(inbox path) or as a first line on the pane (terminal path) — proven
byte-identical against a transcription of the receiver's grammar, never
against the real receiving runtime, because no index and no real receiver
exist in this repo's own test environment. After a deploy that is expected to
carry this, look once at an actual received message on the other side: the
label should read `agent · session · machine` (parts empty get skipped) ahead
of the runtime's own advisory paragraph, and a `relayOfHuman: true` send
should add exactly one declaration line and change nothing about what the
receiver allows. No counter exists for this the way #122's unattestable-entry
counter does — it is a one-time look, not an ongoing signal, because the
underlying risk is a runtime grammar that is a different version than the one
this repo's tests were written against
(`docs/gotchas.d/148-attested-envelope-round-trip.md`).

`FLEET_CAPTURE_LINES` is the other operator lever this deploy gains. It widens
how much of each pane the driver captures to classify it. The default is
unchanged if it is unset or non-positive; raise it only if you have hit the
clipped-composer state, where accumulated notices push a composer out of the
capture window and the driver correctly refuses to act on what it cannot read.
It does not get a session out of that state. A composer already past the window
stays refused until a person reads or clears it at the pane, because nothing the
driver reads can prove the unseen rows are disposable
(`docs/adr/149-a-clipped-composer-has-no-in-driver-proof.md`). Since #169 it
also no longer makes a tall composer readable: a composer is read only from the
visible pane, and one whose opening fence sits in the margin above it is clipped
too, because those margin rows are scrollback
(`docs/adr/169-a-composer-is-read-from-the-visible-pane.md`). What a wider
margin still buys is transcript context above the pane for the rest of the
classification. The `composer_clipped.refused_{discard,keys,send}` counters say
how often the state is actually reached, and
`composer_clipped.fence_above_visible_pane` says how many of those refusals the
visible-pane rule alone caused, and `composer_clipped.bottom_cut` how many came
from a composer whose closing rule the pane's bottom edge cuts off (#216).

This deploy also changes the capture's argv: each batched capture's marker now
targets its pane and expands `#{pane_height}`, and the single-pane capture
chains a `display-message` after `capture-pane`. After deploying, a state read
of an idle session should still classify exactly as before. A driver that
cannot parse the height falls back to treating every row as visible, which is
the pre-#169 behaviour, not a failure.

## Running it

```sh
scripts/deploy.sh HOST REMOTE_PATH     # a peer, over ssh
scripts/deploy.sh local REMOTE_PATH    # this machine, no ssh
```

The local form exists because the ordinary case is deploying to the machine a
session is already running on, and until now the script could not do that —
its only argument was an ssh destination. Every step is identical either way,
including the read-back at the end; only how a command reaches its target
changes.

`REMOTE_PATH`, `FLEET_RESTART` and `FLEET_HEALTH_URL` are not defaulted, on
purpose — see the script header for why. Set all three, or the script tells
you loudly what it could not do on your behalf. `REMOTE_PATH` joined the other
two after muster issue #66: a default here is exactly the operational
fact this script otherwise refuses to guess, and guessing it wrong produced a
deploy that looked FAILED while every step had actually succeeded — see the
trap below.

Four more variables tune verification itself, all optional and all defaulted
to the prior behaviour (muster #93 — see the trap below for why they
exist):

- `FLEET_VERIFY_TIMEOUT` — seconds to poll the health URL before giving up.
  Default **180**.
- `FLEET_VERIFY_INTERVAL` — seconds between polls. Default **2**.
- `FLEET_HEALTH_TOKEN` — the literal bearer token to verify with. Takes
  precedence when set.
- `FLEET_HEALTH_TOKEN_FILE` — a path, read on the host via `cat`.

One of the last two is **required** whenever `FLEET_HEALTH_URL` is set
(muster #108) — see the trap below for why there is no longer a
hardcoded fallback. If your convention is a token file at
`~/.config/muster/token`, set `FLEET_HEALTH_TOKEN_FILE` to that path
explicitly; it is no longer assumed on your behalf.

### Optional delivery modules (#185)

Two more variables, both optional; when `FLEET_MODULE_SOURCES` is unset or empty
this step does not run at all and a deploy is what it always was.

- `FLEET_MODULE_SOURCES` — space-separated `name=source` entries, one per
  optional delivery module to install beside the daemon. **Install it through
  npx** — one install surface, the same as every other tool: the source is
  `npx:<spec>`, where `<spec>` is anything `npx` accepts (`@scope/pkg@version`, or
  `github:<owner>/<repo>#<tag>` for a private package). The fetch runs
  `npx --yes <spec> install-module --dir <stage>/<name>` with **this user's own
  access** (npm and git credentials, netrc, ssh config); the module's launcher owns
  fetching and verifying its binary, and muster knows nothing about any particular
  module. Two older forms still work for one more release, as the fallback: a Go
  package path with a version (`<package>@<version>`), or an absolute directory
  holding a `main` package, each built here with the Go toolchain. A source this
  user cannot fetch installs **nothing and says nothing**: a machine with only the
  built-in module is a supported state, not a deploy problem, and a failed fetch
  never removes a module an earlier deploy installed. A malformed entry gets one
  warning line naming its position and never its source.
- `FLEET_MODULES_DIR` — where the modules are installed on the host. Default: the
  parent of the directory `REMOTE_PATH` lives in, plus
  `libexec/muster/modules` — the place the daemon looks. If you set it here,
  set the same value in the service's own environment.

A launcher is called as `install-module --dir <dir>` where `<dir>` is an empty
private directory whose last element is the module's name; it must leave one
executable file of that name in it, built for the target (`GOOS`/`GOARCH` are in
its environment, set to the target's) and exit non-zero on any failure. Anything
else it writes there is ignored.

An npx module is also asked to prove it runs before it is enabled. After the
upload, **on the host**, muster starts `<module> serve`, sends one `health`
request and requires the hello line and an `ok` answer; a module that fails is
removed from the host again, one warning line names it (never its source), the
module an earlier deploy installed stays as it was, and the deploy carries on. The
request is bounded to a few seconds (`FLEET_MODULE_HEALTH_GRACE`, default 5) plus
a 30 s kill when `timeout` exists on the host.

The module is built (or fetched) for the target's `GOOS`/`GOARCH`, uploaded beside
its final name and renamed into place, as the daemon binary is. The daemon lists its
modules directory at startup, so the restart step is what makes a newly installed
module visible; it also needs `FLEET_DELIVERY_MODULES` naming the module in the
service's environment (`install.md`). Nothing here enables a module on any
machine by itself.

## Running the backup and the revert

```sh
scripts/fleet-backup.sh HOST     # captures a peer's binary + state, over ssh
scripts/fleet-backup.sh local    # captures this machine's own
```

Four variables are required, no defaults (muster #123 — the same
discipline as `deploy.sh`, for the same reason): `FLEET_BIN` (the installed
binary path on the target), `FLEET_STATE_DIR` (the state directory on the
target), `FLEET_HEALTH_URL` (curled ON THE TARGET), and one of
`FLEET_HEALTH_TOKEN` / `FLEET_HEALTH_TOKEN_FILE` — the same credential pair
`deploy.sh` uses for verification (#93, #108), for the same federation reason:
the credential that answers for a peer is not guaranteed to be one the peer's
own on-host token file holds. The backup refuses rather than proceeds if
health does not answer with a build, and refuses rather than trusts itself if
the copy it just made does not checksum-match the source. On success it prints
the exact `fleet-revert.sh` command that undoes it.

```sh
scripts/fleet-revert.sh HOST <backup-dir>                # binary only
scripts/fleet-revert.sh HOST <backup-dir> --with-state    # binary + state, explicit
```

Required: `FLEET_BIN`, `FLEET_RESTART`, `FLEET_HEALTH_URL`, and one of
`FLEET_HEALTH_TOKEN` / `FLEET_HEALTH_TOKEN_FILE` — `FLEET_STATE_DIR` joins that
list only when `--with-state` is passed. It re-verifies the backup's own
checksum against its own manifest before touching anything, installs
atomically, and polls health afterward exactly like `deploy.sh` step 4 does,
refusing to call it done until the reported revision matches what the backup
recorded. `--with-state` is never implied — see the design note under "The
procedure" above for why.

## Traps, measured running this

Each of these cost real time before it earned a place here.

**A deploy can succeed at every step and still verify as FAILED — for two
different reasons that read almost identically (muster #66).**

The first is an install path the service manager does not exec. `REMOTE_PATH`
used to default to `~/bin/muster`; on both machines here the service
definition execs `~/.local/bin/muster`. The script wrote a correct,
freshly-stamped binary to a path nothing runs, restarted the service, and the
service dutifully kept running what it has always run. Verification then
correctly reported a mismatch — but its wording named `FLEET_RESTART`, the one
thing that was actually right, because "the running revision does not match"
looks exactly like a bad restart command from the outside. `REMOTE_PATH` is
now required, the same way `FLEET_RESTART` and `FLEET_HEALTH_URL` already
were: the fix is not a smarter guess, it is refusing to guess at all.

The second is a health URL that reaches something, just not this service. A
stray port or an unrelated server on the same host answers with a real HTTP
status — `403` was the one measured — and the script's old `curl -f` discarded
the body on any non-2xx response, so "reached the wrong thing" and "reached
nothing" produced the same empty `RUNNING` and the same generic FAILED. The
script now keeps the status and the first line of the body specifically for
this case, so a wrong URL says *what answered* instead of pointing at whichever
step ran most recently.

**Two more deploys succeeded at every step and still verified as FAILED, for
two more reasons neither of the above covers (muster #93).**

The first is startup that is not instant. Verification used to probe once and
declare the service dead if that single probe found nothing. Startup does real
work after the process is back — a trust-seed pass and a session
reconciliation over everything the machine was carrying — and that work scales
with how much the machine is carrying, so the busiest machine is the one most
likely to be declared dead. One measured case came back **98 seconds** after
the restart and was healthy from then on; the single-probe script had already
printed the most alarming message it has ("the service did not come back up")
and exited 1, inviting a rollback of a deploy that was already fine. `scripts/deploy.sh`
now polls to a deadline (`FLEET_VERIFY_TIMEOUT`, default 180s — chosen with
slack above that 98s measurement) instead of probing once, and distinguishes
"not up yet" (an interim notice while still inside the deadline — expected,
not alarming) from "did not come up" (the deadline was reached with nothing
ever answering — a real failure). A probe that reaches something concrete — a
real HTTP status, even a bad one — still fails fast rather than waiting out
the whole deadline, because retrying will not change a stable answer like that.

The second is a credential mismatch across a federation. Verification curls the
health URL **on the host**, with a token file read on that same host. On a
federated fleet that file is not guaranteed to hold a credential the far
machine's own service accepts — the credential that answers for a peer can
instead be one only the machine driving the deploy holds. The deploy itself was
completely fine; verification simply asked with the wrong credential and got a
`401` with no build identity in the body — indistinguishable, to the operator,
from the deploy having actually failed, and landing on exactly the step that
talks you out of finishing a two-machine deploy. `FLEET_HEALTH_TOKEN` (a
literal token) and `FLEET_HEALTH_TOKEN_FILE` (an alternate path, still read on
the host) make the credential configuration instead of an assumption. At the
time, neither was required — a caller who set neither still fell back to a
hardcoded path. Colab-fleet #108, below, is what closed that gap.

**A deploy can succeed at every step, verification can reach the service and
authenticate cleanly, and the deploy can STILL report FAILED — because the
credential that authenticated was never going to be accepted (muster
#108).** The hardcoded fallback the previous paragraph describes
(`~/.config/muster/token`, read on the host) is correct for a
single-token deployment, where that file conventionally holds the same value
as the service's own token. It is silently wrong for a deployment configured
with a principal table: that value is never one of the table's principals,
every principal in the table authenticates fine, and the one credential a
table-mode deployment cannot accept is exactly the one nobody told this
script to use anything else. `FLEET_HEALTH_TOKEN` and `FLEET_HEALTH_TOKEN_FILE`
are now **required** whenever `FLEET_HEALTH_URL` is set — the script refuses
before making a network call rather than guess, the same call already made
for `REMOTE_PATH` (#66). This is the second instance of one pattern: the
service's own peer credential was empty in table-only mode until the table
was given a way to name the service's own identity (#98); here the default
credential a *caller* presents needed the identical fix — stop assuming a
value that only ever meant something in single-token mode.

**An untracked file the committed ignore rules do not cover makes the build
report itself modified, and a modified build compares equal to nothing.**
This happened: an agent-settings file was ignored on one machine by that
machine's own *global*, private ignore rule — nothing in this repository's own
`.gitignore` knew about it. On a second machine the same path was untracked
and unignored. At the time the script's gate was `git diff --quiet HEAD`,
which only inspects tracked files, so it still passed — but the build it let
through was already stamped `modified: true`, because Go's own VCS stamp
(`cmd/go/internal/vcs`) computes dirtiness from plain `git status --porcelain`,
which flags untracked files too. The two checks disagreed, and the gate's
answer was the wrong one to trust (muster #139). Two per-issue worktree
checkouts and this repo's own `.claude/plans/`, `.claude/briefs/` reproduced
the same gap later, at repo root.

The fix is two parts, not one: the gate itself now runs `git status
--porcelain` (matching what Go actually checks) instead of `git diff --quiet
HEAD`, so it agrees with the build stamp it is supposed to be a proxy for; and
the paths this repo is expected to always carry untracked — `.worktrees/`,
`.claude/plans/`, `.claude/briefs/` — are in this repository's own
`.gitignore`, not any one machine's private configuration, so a normal working
session does not trip the now-stricter gate. A rule only one machine holds
does not travel to its peer, to a fresh clone, or to CI.

**A machine's git metadata can be far behind while its working tree is
current.** A file-sync tool used elsewhere in this fleet mirrors a working
tree between machines but deliberately excludes the git directory — syncing
history has corrupted a repository before, so excluding it was the right call
for that failure. The consequence for deploy: the tree looks right and the
build behaves right, but the revision stamped comes from `.git` on the machine
doing the build, and that can lag behind what the tree already reflects.
Detect it before trusting a deploy from a machine you have not driven in a
while:

```sh
git fetch
git rev-parse HEAD                # what this machine's .git actually has
git rev-parse '@{u}'              # what trunk is, on the remote it tracks
```

If they differ, do not force anything into place — fast-forward only:

```sh
git merge --ff-only '@{u}'
```

A fast-forward merge either lands cleanly or refuses outright; it cannot
silently overwrite a real divergence, which is the property that matters here.
If it refuses, something other than a stale fetch is going on and is worth
looking at before building anything, not worth forcing past.

**A fix on the federated path only helps once BOTH ends run it.** A defect
that lives in how one machine talks to a peer is not fixed by deploying the
patched build to one side — the request still crosses to a peer running the
old code, and the old failure still reproduces, indistinguishable from the fix
having done nothing. Step 5 above is not a formality for this class of change;
verify the peer's own reported revision after its deploy, not only this
machine's.

**Run this script from the primary checkout, never a linked worktree.** Go's
own VCS build stamp was measured embedding the *primary checkout's* HEAD, not
a linked worktree's own, when built from inside one — reproduced twice,
including after `go clean -cache` (muster #140). Plain `git rev-parse
HEAD`, which the script uses for the revision it compares against, gets the
right answer from a worktree; Go's detector does not. The script now refuses
to build from a linked worktree by default (`ALLOW_WORKTREE_BUILD=1`
overrides it) — this note exists so the refusal is not the first time you
learn why.

**Restarting is safe for running sessions.** They live in the multiplexer, not
in this service, so a restart costs a moment of unavailability against this
service's own API — not lost work. Nothing about a session's own state needs
attention before you restart.

## Publishing to npm

Every publish goes through `release-auto.yml`, and only that. npm trusted
publishing matches the workflow that **starts** a run, each of the five packages
holds exactly one trusted publisher, and it is `release-auto.yml` — permanently.
Nobody repoints it to publish something by hand.

| You want | Do this |
|---|---|
| A candidate / final cut by the pipeline | Nothing — the run that cuts the tag publishes it (`next` for an rc, `latest` for a final). |
| A tag that already exists reaches npm (for example a final tagged by hand) | Actions → **Release (auto)** → Run workflow → `publish-tag` = the tag, or `gh workflow run release-auto.yml -f publish-tag=v0.3.0`. |
| A final tagged outside the workflow, and nobody remembers | Wait for the daily run: it publishes the **newest** final tag whose version the launcher package lacks on npm. |
| To prove a packaging change builds | Dispatch `release-npm.yml` with `dry_run: true`. It publishes nothing and needs no npm identity. |

`publish-tag` refuses, before building anything, a tag that is not
`vX.Y.Z` or `vX.Y.Z-rc.N`, does not exist on origin, or is not reachable from
`main`. An rc goes to `next`, a final to `latest`. A version already on npm is
skipped per package, so naming an already-published tag ends green with every
package reported as skipped, and re-running a half-finished publish completes it.

A `release-npm.yml` dispatch with `dry_run: false` fails at once with a message
naming the route above — it could only ever fail to authenticate (`ENEEDAUTH`).
The reconcile only ever considers the newest final, never an older one: publishing
an older version to `latest` after a newer one would move `latest` backwards.

## Why this is the procedure the descriptor means

`.github/project.yml`'s `exposure: self` comment and this repository's
`CLAUDE.md` both cite "the documented build-install-restart procedure" as the
reason merging to trunk is not itself a ship. This page, plus
`scripts/deploy.sh`, is that procedure. If either drifts from the other,
trust the script — this page describes it, not the other way round.
