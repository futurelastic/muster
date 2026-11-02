# A `run-shell` job that finishes after its client has gone crashes the multiplexer server, and every session in it dies at once

**Issue:** #389 (the server-context probe of #336)

## What happened

On one busy macOS machine the session server segfaulted five times in three
days. Each time every session it hosted vanished in the same tick, with no kill
recorded for any of them. Every crash report had the same stack on the crashed
thread:

```
proc_send < file_vprint < file_print < server_client_print < cmdq_print
  < cmd_run_shell_callback < job_check_died | job_error_callback
```

The server runs a `run-shell` job; when the job ends it prints the job's output
(or its exit status, when non-zero) to the command client that asked for it. If
that client has already gone, the multiplexer (3.7c) does not check, writes to
a closed peer, and dies.

The only `run-shell` this service sent was the #336 probe that asks the server
which launchd session it is in. It ran under a 3 s context and was re-sent
every 30 s from the health read. On a loaded machine the context expired while
the job was still running, Go killed the client, and the job's output was then
printed to nothing.

Reproduced on a private server, no load needed: `run-shell 'sleep 1; echo x'`
with its client killed after 300 ms took the server down with the identical
stack.

## The rule going forward

Never send the server a job whose client can be killed before the job ends, and
never give a job anything to print. `run-shell`, `if-shell` and the `#(…)` of a
format all start a job the server reports on.

The probe now sends `run-shell -b` with a job that writes nothing to stdout or
stderr, always exits 0, and leaves its answer in a file that the driver polls
for (`servercontext.go`, `probeServerContext`). Its client runs under
`context.WithoutCancel`, so a timeout abandons the wait, never the client. The
answer is kept per server run (pid and start time), so the question is put to a
server once, not every window.

`launchctl procinfo <pid>`, which would answer without any job in the server,
needs root, so it was not an option.

The regression guard is `TestLiveProbeWhoseJobOutlivesItsTimeoutDoesNotCrashTheServer`
(`FLEET_TMUX_INTEGRATION=1`, against a private server): it holds the job past the
probe's timeout and asserts the server is still the same process afterwards.
