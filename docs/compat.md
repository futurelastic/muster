# `muster compat` — does this runtime build still behave the way this service assumes?

`muster compat` runs this service's assumptions about the agent runtime
against **one candidate binary** and prints a versioned report. It exists to be
run *before* a fleet takes a new runtime build, not after something has already
quietly stopped working.

It only reports. Installing, pinning, switching and promoting a runtime build are
the caller's decision; nothing here changes any of them.

## Why it exists

This service does not call an API. It drives the runtime through a terminal and
reads a small set of things the runtime writes: the screen grammar its
classifiers read, the composer's geometry and wrap width, how a bracketed paste
is handled, the shape of the transcript entries, and the fields of the runtime's
per-process session record. **No public contract guarantees any of them**, and a
new release can change any of them without raising an error — a changed glyph
silently reclassifies, a moved field silently stops joining. The first sign is a
fleet that has quietly stopped delivering or classifying.

The remedy is to pin one supported build and to test every candidate before the
fleet takes it. This command is the part of that gate that belongs here: the
part that knows what *this service* relies on. What to do with the answer is not
its business.

## Usage

```
muster compat --claude <absolute path to a claude binary>
                    [--json] [--pack <dir>] [--only <id[,id…]>] [--timeout <duration>]
```

| Flag | Meaning |
|---|---|
| `--claude PATH` | The candidate. Required, and it must be an **absolute** path to an executable file. It is resolved through symlinks once and launched by that resolved path — never looked up on `PATH`, because a login shell's `PATH` would silently put the installed build back in front of the candidate. |
| `--json` | Print the schema 1 report as JSON on stdout (and nothing else on stdout). Without it, a table. |
| `--pack DIR` | Save the raw evidence each check produced (see [Pack layout](#pack-layout)). The directory must be new or empty. |
| `--only IDS` | Run only these check IDs, comma-separated. The probes they need are pulled in automatically. The report then carries an `only` field: a partial run is not a certification. An ID that is unknown, or known but not implemented in this build, is a usage error. |
| `--timeout D` | Overall limit for the whole run. Default `20m`. |

Both `--flag=value` and `--flag value` are accepted. Progress goes to stderr, so
`--json` output can be piped.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Every `must` check passed. |
| `1` | A `must` check **failed**: the candidate does something this service does not expect. |
| `2` | Could not certify: a `must` check could not run (see `error` below), or the invocation was bad. A bad invocation prints no report. |

When a `must` check has failed *and* another `must` check could not run, the exit
code is `1`: a proven failure outranks "could not certify", because retrying
cannot clear it.

## Isolation — what a run touches, and what it never does

A check that needs a session runs it in a **private multiplexer server** started
for that run alone:

- **One door.** The driver is built with a small wrapper script as its multiplexer
  binary, and that script is the only thing that names a socket:
  `env -i <minimal environment> tmux -L <label> -f /dev/null "$@"`. `env -i` matters
  as much as `-L`: it drops `$TMUX`, so no command can fall back to an implicit
  "current server", and it makes the server's own environment — which every
  session inherits — exactly the minimal set below. The harness never runs the
  multiplexer except through that script.
- **Proven before it is trusted.** The server must report the expected socket path
  and list only its own keeper session, or the run stops and every check that
  needs a session is `error`.
- **A minimal environment.** `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `TMPDIR`,
  `TERM`, a bare `PATH`, and `DISABLE_UPDATES=1` and `DISABLE_AUTOUPDATER=1`, so a
  check can never update the binary it is checking. Nothing else is inherited: not
  the caller's variables, and no `CLAUDE_*` names.
- **Throwaway working directories** under a scratch directory (`/tmp/cfc-<nonce>`):
  a trusted root, with folder trust seeded through the driver's own path so that
  path is exercised too, and two directories outside it — one that is *not* trusted,
  and one the harness trusts itself while leaving the external-imports question
  standing (see [The external-imports question](#the-external-imports-question)).
  Every one carries a project-local setting that turns remote control off — a
  project-local `false` wins over the user's own setting — so no check registers
  anything outside the machine.
- **The candidate by its resolved path**, as the first word of the command line —
  never `claude` looked up on `PATH`.
- **Cheap model settings** for the few checks that need a turn: the smallest model at
  low effort, synthetic nonce-tagged prompts only. The report's `turns` counts them.
- **The service's own sessions are never addressed.** The harness only ever talks
  to the server it started, and finds its own processes by asking that server, not
  the process table. A test runs the whole thing with `$TMUX` pointing at a
  stand-in server that has a session of its own, and requires that server's session
  ids and panes to be identical afterwards.

### Teardown always runs

On failure, on a panic, and on `SIGINT`, `SIGTERM` or `SIGHUP`, on its own deadline
(so a cancelled run still cleans up): the private server is killed, the socket
removed, the throwaway transcript directories and per-process records deleted, any
straggling process ended (only after its pid *and* start time are verified
unchanged, so a recycled pid is never touched), and the scratch directory removed.
Before it stops a session, teardown lets the youngest one reach an age of fifteen
seconds — see [A session must not be killed young](#a-session-must-not-be-killed-young).
Every step is attempted, and every failure is reported: the report is still printed,
the problems go to stderr, and the exit code is `2`. Every deletion is guarded by
the run's own nonce, so a path that does not carry it is never removed. For the one
case teardown cannot cover — the process being killed outright with `SIGKILL` — the
keeper session runs a small watchdog that waits for the process to disappear and then
removes what it left.

### A session must not be killed young

The runtime records every launch in a machine-wide state file and clears the record
once the launch has been alive for about ten seconds, which also clears the count of
failed launches. A launch that dies sooner is counted as a **failed start** by the
next launch, and enough of those switch the runtime's fullscreen renderer off for that
version on the whole machine. That was measured, not inferred: the record appeared one
second after a session started and was cleared, together with the count, at eleven.

A compat run that stopped short-lived sessions could therefore change how every real
session on the machine renders, silently and durably. So teardown lets every session
it started reach fifteen seconds of age before stopping it. It judges by age, not by
reading the runtime's private state, so it does not depend on a key name that may
change. A full run lasts minutes and never waits; only a run that ends within seconds
of a boot does, and only for as long as it has to. The one case this cannot cover is
the process being killed outright (`SIGKILL`), which the watchdog handles without the
courtesy of waiting — at worst one failed start is left behind, which the next
healthy launch clears.

### What a run does leave

- **One folder-trust entry for each trusted throwaway directory a run launched a session
  in** in the runtime's own state file — three in a full run (measured: the file went from
  876 project entries to 879). There is no API to remove one, and editing that file while
  live sessions rewrite it is the race the trust seeder exists to survive. The third entry
  is the imports directory's, and it carries the trust answer only, on purpose.
- **The model turns it spent**, against whatever account the runtime is signed in to.
- While it runs, the throwaway sessions are visible to anything that lists the
  runtime's per-process records, as any session is. They are removed afterwards.

Two lines on stderr are expected in every full run and are not a problem:
`tmux: trust-seed: trustseed: refusing …/u: outside every configured root`, and the same for
`…/i`. That is the driver's trust seeder declining the two directories that sit outside its
root on purpose: `u`, which lets the folder-trust dialog appear, and `i`, which the harness
trusts itself so that only the external-imports dialog can appear.

A run restricted with `--only` to checks that need no session (the static ones) never
starts a server and creates no scratch directory.

## The report

The report is a **public, versioned contract**. This is a complete example:

```json
{
  "schema": 1,
  "claude": {
    "path": "/path/to/claude",
    "version": "9.9.9",
    "resolved": "/path/to/versions/9.9.9",
    "sha256": "0000000000000000000000000000000000000000000000000000000000000000",
    "arch": "x86_64"
  },
  "colabFleet": {
    "version": "v0.0.0",
    "commit": "abc1234"
  },
  "checks": [
    {
      "id": "F-LIMIT",
      "gate": "warn",
      "pass": true,
      "error": false,
      "detail": "all 4 marker strings are present · relied on by: internal/drivers/tmux/classify.go#usageLimit",
      "reliedOn": ["internal/drivers/tmux/classify.go#usageLimit"]
    }
  ],
  "turns": 0,
  "pass": true,
  "durationMs": 1075
}
```

<!-- compat:fields:begin -->
| Field | Type | Required |
|---|---|---|
| `schema` | integer | required |
| `claude` | object | required |
| `claude.path` | string | required |
| `claude.version` | string | required |
| `claude.resolved` | string | additive |
| `claude.sha256` | string | additive |
| `claude.arch` | string | additive |
| `colabFleet` | object | additive |
| `colabFleet.version` | string | additive |
| `colabFleet.commit` | string | additive |
| `colabFleet.modified` | boolean | additive |
| `checks` | array | required |
| `checks[].id` | string | required |
| `checks[].gate` | string | additive |
| `checks[].pass` | boolean | required |
| `checks[].error` | boolean | additive |
| `checks[].detail` | string | required |
| `checks[].reliedOn` | array | additive |
| `turns` | integer | additive |
| `pass` | boolean | required |
| `only` | array | additive |
| `durationMs` | integer | additive |
<!-- compat:fields:end -->

The **required core** — the fields a caller may rely on — is `schema`,
`claude.path`, `claude.version`, `checks[].id`, `checks[].pass`, `checks[].detail`
and the top-level `pass`. Everything else is additive: a caller must ignore
fields it does not know.

### `error` is not a failure

`pass: false, error: false` means the candidate **behaved differently** from what
this service expects. `error: true` means the check **could not run** — the
environment failed, a timeout left no evidence either way, or a probe it depends
on did not complete. The two are reported apart on purpose so a caller can retry
an error and reject a failure. `error: true` always comes with `pass: false`.

### Gates

Each check has a gate, taken from the catalogue and never chosen by the check:

- **`must`** — anything the driver needs in order to deliver, confirm, classify a
  dialog or identify a session. A `must` check that fails or cannot run keeps
  the top-level `pass` `false`.
- **`warn`** — cosmetic, or a path that is not live yet. Reported; never changes
  the verdict or the exit code.

The top-level `pass` is true when every `must` check passed with no `must` error.
A report that judged no check at all does not pass.

### Partial runs

A run restricted with `--only` carries an `only` array. Its `pass` says the checks
it ran passed; it says nothing about the rest, and is not a certification.

## Versioning

- **Any change to the shape bumps `schema`.** That means removing, renaming or
  retyping a field, or changing what a field means.
- **Adding a field does not.** Callers ignore what they do not know.
- **A check ID is stable once shipped.** Renaming or removing one is a schema bump.
  The list of shipped IDs is `internal/compat/testdata/shipped-ids.txt`, and a test
  fails when a listed ID is missing from the catalogue, when a catalogue ID is not
  listed, or when the file's `schema` header does not match the code — so a rename
  cannot happen by accident.
- A check's `gate` may change without a bump; the gate is data, not shape.

| Schema | What changed |
|---|---|
| 1 | The first version. |

## The check catalogue

Every check names the driver code that depends on the behaviour it asserts. That
text is written once, in the catalogue in `internal/compat/catalogue.go`, and
appears in three places that tests hold together: the ` · relied on by: …` suffix
of every report `detail`, the additive `reliedOn` array, and this table. A
rename in the driver that leaves a `relied on` entry pointing at nothing fails the
build.

<!-- compat:catalogue:begin -->
| ID | Gate | What it asserts | Relied on by |
|---|---|---|---|
| `F-LIMIT` | warn | The candidate still contains the usage-limit notice wording the screen classifier recognises. Static text only: the screen cannot be produced on demand. | `internal/drivers/tmux/classify.go#usageLimit` |
| `F-APIERR` | warn | The candidate still contains the API-error wording the classifier reads to tell a failed turn from a finished one. Static text only: the screen cannot be produced on demand. | `internal/drivers/tmux/classify.go#lastTurnFailed` |
| `H-RC` | warn | The candidate still contains the four remote-control footer labels the control-channel reader maps. Static text only: a check never attaches a bridge, and the wording being present does not prove where the runtime draws it. | `internal/drivers/tmux/controlchannel.go#controlStates` |
| `H-RCREC` | warn | The candidate still contains the wording of the entries the control-channel reader takes from the runtime's own record: the one written when the channel comes up, the one written when it is disconnected, and the record's subtype for the first. Static text only: a check never attaches a bridge, and the wording being present does not prove where the runtime draws a label. | `internal/drivers/tmux/controlchannelrecord.go#classifyControlRecordLine` |
| `F-FEEDBACK` | warn | The candidate still contains the wording of the feedback-draft card's states the driver recognises: the send confirmation, the send error, the question about turning drafts off, the queued count and the /feedback panel's title. Static text only: no probe can make an agent draft feedback. | `internal/drivers/tmux/feedbackcard.go#parseFeedbackStatus` |
| `F-MODE` | warn | The driver reads the permission mode a live session shows: a session started in the default mode reads as default and one started in bypass-permissions mode reads as bypass, and the candidate still contains the wording of the accept-edits, plan and auto indicator rows. The other three modes cannot be entered without pressing keys in a session other checks share, so their wording is checked statically. | `internal/drivers/tmux/permissionmode.go#permissionModeOf` |
| `C1` | must | A working directory the driver seeds as trusted starts without the folder-trust dialog, and without the external-imports dialog although its instruction file imports a file from outside it, so a session created there reaches its composer on its own. | `internal/trustseed/trustseed.go#Seeder`, `internal/trustseed/trustseed.go#seededKeys` |
| `F-TRUST` | must | A directory outside the trust root shows the folder-trust dialog, which the driver classifies as such, reads as an unnumbered menu, and can find exactly one affirmative option in. Observed only: the dialog is never answered. | `internal/drivers/tmux/classify.go#classifyPromptKind`, `internal/drivers/tmux/tmux.go#affirmativeOption` |
| `F-IMPORTS` | must | A directory that is trusted but was never approved to import from outside itself, and whose instruction file does, shows the external-imports dialog, which the driver classifies as such, reads as an unnumbered menu with the decline highlighted, and can find exactly one affirmative option in. Observed only: the dialog is never answered. | `internal/drivers/tmux/classify.go#classifyPromptKind`, `internal/drivers/tmux/tmux.go#consentableKinds`, `internal/drivers/tmux/tmux.go#affirmativeOption` |
| `B5` | must | A session started in bypass-permissions mode reaches its composer with no acceptance screen in the way, given the user setting that suppresses it. | `internal/drivers/tmux/tmux.go#claudeCodeCommand` |
| `F-BYPASS` | warn | The bypass-acceptance screen, when it can be produced, is classified as such; otherwise the wording of its two options is still present in the candidate. Observed only: it is never answered. | `internal/drivers/tmux/tmux.go#acceptanceScreen` |
| `D1` | must | The runtime's per-process session record appears within fifteen seconds of launch carrying the fields this service reads, with the expected types and values. | `internal/drivers/tmux/terminalpath2_transcript.go#processSessionRecord` |
| `D3` | must | The record's process start time (UTC text, or on Linux clock ticks since boot) corroborates the running process, so the record can be trusted to belong to that process and not to a recycled pid. | `internal/drivers/tmux/terminalpath2_transcript.go#parseProcessSessionRecordStartTime` |
| `D4` | must | With remote control off, the record carries no bridge id and the screen shows no control-channel label. The negative half only: the positive half needs a bridge, which a check never creates. | `internal/drivers/tmux/controlchannel.go#controlChannelOf` |
| `F-COMPOSER` | must | The composer is the prompt glyph between two rules, an empty composer reads as empty even when a dim placeholder is painted in it, and a typed draft reads back exactly. | `internal/drivers/tmux/classify.go#composerText` |
| `F-MLDRAFT` | must | A multi-line draft pasted into the composer reads back as the text that was pasted. | `internal/drivers/tmux/composertext.go#composerMatchesText` |
| `F-PASTEMARK` | must | A long multi-line paste collapses to a [Pasted text #N +M lines] marker and one long line to a bare [Pasted text #N] marker, and both are counted the way delivery confirmation counts them. | `internal/drivers/tmux/tmux.go#markerCounts`, `internal/drivers/tmux/tmux.go#composerHoldsCollapsedPaste` |
| `F-WRAP` | must | A draft longer than a row wraps onto rows of one width, and the wrapped rows read back as the text that was pasted. | `internal/drivers/tmux/composertext.go#composerRegion` |
| `G6` | must | The prompt-mode characters behave as the input guard assumes: a leading ! in an empty composer enters shell mode, while the same text after a space, and a slash after a space, stay plain prompt text. | `internal/drivers/tmux/inputguard.go#refuseAsRuntimeSyntax` |
| `G1` | must | A send lands in an idle session and is confirmed as its own user turn by the runtime's transcript, exactly once, for short, long, multi-line and control-byte text, and the session comes back to an empty composer. | `internal/drivers/tmux/terminalpath2_transcript.go#transcriptTailScan` |
| `G2` | must | Bracketed paste is on at the composer, and a long single line, a long multi-line paste and text with control bytes each arrive whole and exactly once, with the control bytes removed as the sanitiser assumes. | `internal/drivers/tmux/terminalpath2.go#sanitizeForBracketedPaste` |
| `E-USER` | must | The user turn the runtime records for a send carries the fields delivery confirmation reads: message content, a human origin, a typed or queued prompt source, and no meta, sidechain or summary marking. | `internal/drivers/tmux/terminalpath2_transcript.go#extractTranscriptCandidate` |
| `E-PASTE` | must | A long or collapsed paste is recorded as the real text, wrapped or not, and unwraps to exactly the text that was sent; a marker alone is not enough. | `internal/drivers/tmux/terminalpath2_transcript.go#normalizeTranscriptText` |
| `E-NAME` | must | The transcript is a file named for the session id whose custom-title is the session's name, within the lines the driver reads. | `internal/drivers/tmux/conversation.go#readRecordEntry` |
| `E-SLUG` | must | A working directory containing a dot, an underscore, a space and a non-ASCII letter is recorded in the directory the driver derives by replacing every non-alphanumeric character. | `internal/drivers/tmux/conversation.go#recordDirFor` |
| `B2` | must | The -n value names the per-process record and the transcript's title, and the driver joins the session to its conversation by that name, with remote control off. | `internal/drivers/tmux/conversation.go#conversationStore` |
| `B7a` | must | A system-prompt file passed with --append-system-prompt-file is honoured: an instruction in it shows in the reply. | `internal/drivers/tmux/tmux.go#claudeCodeCommand` |
| `D2` | warn | The record's status moves off idle while a turn runs and back, with statusUpdatedAt advancing at each change. Nothing in this driver reads it yet, so this is warn-only: it is the record's own liveness signal. | `internal/drivers/tmux/terminalpath2_transcript.go#processSessionRecord` |
| `H-RENAME` | warn | Whether, and after how long, a programmatic rename — the driver's own Rename followed by its own SyncTitle, delivered via the composer exactly as the service delivers it — is followed by a custom-title transcript entry naming the session's new id. SyncTitle already degrades honestly to pending when this has not happened within its own four-second window; this is warn-only because that degradation, not this finding, is what the driver relies on. It reads for far longer than that window so a runtime that is merely slow to write the entry is not reported the same as one that never does. | `internal/drivers/tmux/titlesync.go#transcriptTitleScan` |
<!-- compat:catalogue:end -->

## The external-imports question

A directory whose instruction file imports a file from outside it raises a second boot
question after the trust one: whether the runtime may follow the import. The service
classifies it (`external-imports`), seeds its answer ahead of time under the configured
trust roots, and answers it on consent. All of that rests on three facts about one build —
the two option labels and the shape of the menu, and the two per-project state keys the
seeder writes — and two checks pin them from opposite sides:

- **`C1`** — the seeded directory's instruction file imports a file from outside it, so the
  question would appear if the seeder's answer did not hold. It reaches its composer with no
  dialog only while the runtime still honours the keys the seeder writes. When it does not, the
  report says the directory *still asks about that import* and names the two keys.
- **`F-IMPORTS`** — the other half: a directory that is trusted but was never approved to
  import, and does, shows the dialog, classified as such, as an unnumbered menu with the
  decline highlighted and exactly one affirmative option. Observed only; never answered.

A directory the harness seeds through the driver can never show this dialog, because the seeder
writes both answers. So the imports directory sits outside the trust root, where the driver's
seeder refuses it, and the harness writes its trust answer itself through the same writer the
seeder uses, with the keys narrowed to that one. That is the extra entry described under
[What a run does leave](#what-a-run-does-leave).

`F-IMPORTS` is a `must` for the same reason `F-TRUST` is: classifying a dialog is something the
driver needs in order to answer it. A build that simply stops asking also fails it, with the
words "no dialog appeared" — the seeder and the consent are then harmless dead weight, and a
maintainer who decides that is fine can lower the gate, which is data and not shape.

## Probes, stages and cost

A check never drives anything itself. A **probe** drives one shared session into a
state and records what it saw; a **check** judges the record. One booted session
therefore serves every check that reads it, and the expensive parts are paid once.
A probe fails only when the *environment* did — a multiplexer error, a timeout. What
the candidate did is evidence however surprising, so the check that reads it can say
"behaves differently" (a failure) instead of "could not run" (an `error`). Two cases
follow from that and are worth knowing: a candidate that never shows a composer, and
one whose composer cannot be cleared, **fail** the checks that depend on them, with the
reason; they do not report them as unable to run.

| Stage | What runs | Model turns |
|---|---|---|
| 0 | One scan of the candidate for the wording the screen classifiers read (F-LIMIT, F-APIERR, H-RC, H-RCREC, F-FEEDBACK, F-MODE, and F-BYPASS as a fallback). | 0 |
| 1 | The isolated multiplexer server. | 0 |
| 2 | Five sessions: a trusted directory whose instruction file imports a file from outside it (C1, D1, D3, D4, B2, E-*…), an untrusted one (F-TRUST), a trusted one never approved to import (F-IMPORTS), and two in bypass mode (B5, F-BYPASS; and F-MODE reads the default-mode and bypass sessions' indicator rows, after giving each up to ten seconds to paint it — measured: the composer is up about a second before the row under it). | 0 |
| 3 | Drafts pasted into the composer and cleared again (F-COMPOSER, F-MLDRAFT, F-PASTEMARK, F-WRAP), and a session used once to enter shell mode (G6). | 0 |
| 4 | Five sends on the trusted session: a first one that creates the transcript, then a short one, a single line over 800 bytes, a 40-line paste, and text with control bytes and a tab (G1, G2, E-USER, E-PASTE, E-NAME, E-SLUG, B2, B7a, D2). | 5 |
| 5 | One programmatic rename of the trusted session — the driver's own Rename then its own SyncTitle, the same two calls and order the service uses (H-RENAME). No model turn: a rename is a driver-level call, not a prompt. | 0 |

Measured on one full run against a current supported build: **27 checks, 5 model turns,
about 46 seconds** when the rename resolves quickly, **up to `compatRenameWait` (30s) longer**
when it does not — H-RENAME keeps reading the transcript for that long before it reports
the runtime never wrote a custom-title, so a slow "no" costs wall time even though it spends
no model turn. Every turn is a synthetic, nonce-tagged prompt at the smallest model
and lowest effort. The first send is made only to create the transcript, because the
runtime writes it at the first turn and the driver confirms a send by the screen until it
exists; the confirmation checks are asserted on the sends after it.

## Pack layout

`--pack DIR` saves what the checks were judged on, so a tool outside this repository can
replay its own classifiers over the same states without driving the candidate again. What
those tools are is none of this repository's business. The directory must be new or empty.

```
manifest.json                     what the pack holds, file by file
report.json                       the report this run printed
states/<name>/pane.txt            the pane as the driver captured it, redacted
states/<name>/pane.ansi.txt       the same, with escape sequences, redacted
states/<name>/pane.json           width, height, bracketed-paste flag, driver status
sessions/a/record.json            the per-process record, home directory rewritten
sessions/a/transcript.jsonl       the transcript entries the checks read
```

States are `boot`, `trust-dialog`, `imports-dialog`, `boot-bypass`, `bypass-attempt`, `shell-mode`, and one
`draft-<name>` per draft. Pane captures pass through `RedactCapture`, which keeps only fixed
runtime vocabulary and layout and discards everything else, so a pane's prose — a path, a
name, a reply — never reaches the pack. The record and transcript have the home directory
and the scratch path rewritten, and only `user`, `assistant`, `queue-operation`,
`custom-title` and `system` entries are kept: the runtime's other entries (attachments,
snapshots) can carry environment content. The pack is local data, created `0700`. `report.json`
records the `--claude` path exactly as it was given.

## Proving the command can fail

A check is only worth having if it can fail, and fail with the right ID. The test suite
does this against a synthetic runtime (`internal/drivers/tmux/testdata/faketui.py`) that
takes opt-in knobs, all defaulting to ordinary behaviour:

| Knob | What it breaks |
|---|---|
| `FAKE_GLYPH=<char>` | The prompt glyph the driver looks for. |
| `FAKE_NO_BRACKET=1` | Bracketed paste is never switched on. |
| `FAKE_PLACEHOLDER=1` / `plain` | A placeholder painted dim (read as empty), or not dim (read as a draft). |
| `FAKE_IMPORTS=1` / `reworded` / `allow-highlighted` | Asks the external-imports question in a directory that imports from outside it (unless the state file's answer says otherwise): as measured, with options nobody has seen, or with the allow row highlighted. Unset, the candidate never asks — a build that stopped. |
| `FAKE_IMPORTS_KEY=<name>` | The key the synthetic runtime reads the answer from, so a candidate that records it under another name can be shown to fail C1. |

With them, a healthy synthetic candidate passes the composer checks and each broken one fails
exactly the checks that depend on the broken behaviour, with exit `1` and a reason. To try it
against a real build, wrap the binary in a small script that changes one behaviour and pass the
script as `--claude`. These tests need a multiplexer and are gated behind
`FLEET_TMUX_INTEGRATION=1`, like the driver's other multiplexer tests.

## Not checked

The checks above are the ones that were built. Everything else the runtime does is out of
scope **by the maintainers' ruling**, not deferred — delivery is now confirmed from the
transcript rather than the screen, so the checks that existed to guard the screen path are
not needed. If one is ever needed it gets a new issue. Not built:

- launch and resume: `--resume` continuing the same conversation (**B1**);
- busy-session delivery and its queue entries (**G4**, **E-QUEUE**), interrupt (**G7**), and
  pasted text not answering an open dialog (**G5**);
- the spinner and the permission, question and multi-select dialogs (**F-SPIN**, **F-PERM**,
  **F-ASKQ**, **F-MULTI**), and the respond and multi-select key paths (**G3**);
- local-command entries in general (**E-CMD**) — `/rename` itself is now measured (**H-RENAME**);
- the umbrella comparison of every transcript entry type's key paths (**E-SCHEMA**), and replay of
  the committed classifier corpus against a candidate (**F-CORPUS**).

Also outside every version of this command, on purpose: paths that register a bridge with an outside
service (a check never attaches one, so the remote-control checks are static or negative-only), the
inbox path, and quota and API-error screens, which cannot be produced on demand and are checked as
wording only.
