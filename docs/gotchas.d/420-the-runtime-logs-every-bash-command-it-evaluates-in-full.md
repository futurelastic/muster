# The opencode runtime logs every bash command it evaluates, in full, at its default level

**Issue:** #420

## What happened

A client of the service passed a per-session API token inline on a command line
(an `Authorization: Bearer …` header on a `curl`). The session's own log, under
the state directory the driver gives it, then held that token in plaintext:

```
level=INFO message=evaluated permission=bash pattern="curl -s -H 'Authorization: Bearer …' http://…"
```

The line is written by the runtime's permission check, once per evaluated
command, whatever the answer. It went to the runtime's stderr and to its log
file, readable by any process of the same account for as long as the session
directory lives.

## Measured (runtime v1.18.3, an isolated per-session directory)

A scripted provider asked for one bash call carrying an auth header; the
session's permission for bash was `allow`, `deny` or `ask` in turn.

| `--log-level` | lines in the log file | the header value in it |
|---|---|---|
| (none) / `INFO` / `DEBUG` | 30 or so | **yes** when the command was allowed |
| `WARN` | 0 | no, in all three permission answers |
| `ERROR` | 0 | no |

- The level applies to the log **file** as well as to `--print-logs` on stderr.
  One flag covers both.
- `ERROR` lines survive a `WARN` cap. A model the runtime does not know is
  still logged by name (`ProviderModelNotFoundError`), which is what the driver's
  error-name capture reads (#284, #288). Pinned against the real binary by
  `TestIntegration_ACappedRuntimeLogStillNamesAnError`.
- The runtime has **no redaction hook**. Choosing "redact secret shapes" over
  "log less" would have meant the driver reading the log and rewriting it, for a
  pattern list that can never be complete (a token in a URL, a password in a
  `-u user:pass`, a value in an environment assignment).
- The runtime opens a log file and a log directory that already exist and keeps
  their modes. Creating them first is how they are owner-only from the start;
  left to the runtime the file is `0644`.

## The rule going forward

- The driver starts the runtime with `--log-level WARN` (`runtimeLogLevel`,
  `process.go`), next to `--print-logs`. WARN rather than ERROR: everything the
  driver reads is ERROR, and WARN is the quietest level that still shows an
  operator the runtime's own warnings. No WARN line carried command text in the
  runs above; if one is ever found that does, raise the cap to ERROR.
- Before every start, a relaunch included, the driver creates the runtime's log
  directory `0700` and its log file `0600`, and tightens a log an earlier start
  left (`privateRuntimeLog`, `runlog.go`). This is the second layer, for whatever
  the level lets through; the level is what keeps the secret out.
- The client that put the secret on a command line is the real fix and belongs to
  the client: read it from a file, never inline. Both layers here are defence in
  depth.
- `TestIntegration_ARuntimeLogHoldsNoCommandText` is the check that a command
  with an auth header leaves nothing in the log. It runs against the real binary
  (`FLEET_OPENCODE_INTEGRATION=1`) and fails when the level is raised back to
  `INFO`. A runtime upgrade is the moment to run it: the cap is the runtime's
  behaviour, not this repo's.
- The runtime writes its log behind the request that caused the line, so a log
  read straight after a turn stops short of it. A negative check on such a read
  passes for the wrong reason; the test waits out a settle time and its mutation
  check is what shows that the wait is long enough.
