# A hard link to a live runtime store wedges its writer for good on macOS, whoever makes the link and whichever muster release is running

**Issue:** #515 (the cause of the sticky `SQLiteError: disk I/O error`; the earlier
entry for this issue records how the release-regression theory was refuted
first)

## What happened

On one macOS host (Intel, 13.x) every runtime server muster started answered 500
on its store-backed routes with `disk I/O error` and never recovered. Rolling the
service back one release did not stop it, so no muster line was the cause.

The cause was a second program on the same machine. Every six minutes it
**hard-linked** each runtime store (`opencode.db`) and its `-wal` and `-shm` into
a folder of its own, opened the links read-only to read rows, and unlinked them
again. A link was chosen on purpose: it pins the inode, so a session that renames
the store cannot redirect the open. The design assumed a link leaves the writer
undisturbed. For this runtime it does not.

The runtime is a bun-compiled binary, and on macOS its SQLite is the system's
(Apple's `libsqlite3`, 3.39.5 here). Apple's build watches the vnodes of the files
a connection has open. When the link count of one changes, it logs

```
BUG IN CLIENT OF libsqlite3.dylib: database integrity compromised by API violation:
vnode linked while in use: <path>          (and "vnode unlinked while in use" when the link goes)
```

and from then on that connection fails every operation with the extended code
`SQLITE_IOERR_VNODE`, which the runtime prints as `disk I/O error`. The state is
**per connection and permanent**: the files are intact, a separate reader gets the
rows back, `quick_check` says ok, and a new process on the same files works.

## How it fitted the earlier measurements

- **Bursts across many servers, at once.** One pass of the linking program touches
  every store on the machine. The two bursts that were read (10:16:57 and 10:23:05
  local) are two of its passes in the system log, six minutes apart, and two more
  that afternoon (15:04 and 15:10) wedged twelve servers, each first error within
  ninety seconds of the pass that preceded it.
- **A wedge per server, sticky, never recovered.** A connection that has seen the
  link fails on its next operation, and nothing recovered.
- **"The last good write is often a tool starting."** A server writing during a pass
  is the one that notices at once; an idle server notices on its next poll.
- **Only muster-started servers.** The machine's default instance had no connection
  open when looked at, so there was nothing there to wedge; the plain SQLite
  writers used as controls never had a link made to them.
- **The rollback was red.** The program is independent of muster's version. The
  change that made it read these stores had landed twelve minutes before the first
  wedge on the host.
- **Nothing reproduced it off the host.** Every earlier trial left out the link.

## What was measured (same runtime, one machine)

One writer, one small insert every 20 ms, a second process linking beside it:

| what the second process does to the store | writer |
|---|---|
| nothing (control) | 400 writes, no error |
| hard-link the store, the `-wal` or the `-shm` (any one of the three) | fails on the first pass, `SQLITE_IOERR_VNODE`, 0 of 20 later writes succeed |
| copy the three files, `cp -c` (clone) them, open the store read-only by name, or symlink to it | no error |

The same writer in Python, on the same Apple library, with the same links, did not
fail and logged nothing: the failure depends on the client, which is why "plain
SQLite" was a bad control.

Reproduce it with no model credential, in about ten seconds. `opencode` is a bun
binary and `BUN_BE_BUN=1` makes it run a script:

```js
// writer.js
import { Database } from "bun:sqlite";            // the runtime's SQLite: Apple's libsqlite3 on macOS
const db = new Database(process.argv[2]);
db.exec("pragma journal_mode=wal; create table if not exists t(i integer primary key, v text)");
for (let n = 0; n < 400; n++) {                    // one small write every 20 ms, about 8 s
  try { db.run("insert into t(v) values('x')"); }
  catch (e) { console.log(`write ${n} failed: ${e.message} (${e.code})`); process.exit(1); }
  await Bun.sleep(20);
}
console.log("400 writes, no error");
```

```sh
mkdir -p links && rm -f store.db store.db-wal store.db-shm links/*
BUN_BE_BUN=1 opencode writer.js store.db &        # opencode is a bun binary; BUN_BE_BUN=1 makes it run a script
sleep 1.5
for i in 1 2 3; do
  ln store.db links/db; ln store.db-wal links/wal; ln store.db-shm links/shm   # what the other process does
  sleep 0.5; rm links/db links/wal links/shm; sleep 1
done
wait          # without the loop: "400 writes, no error"; with it: "write 71 failed: disk I/O error (SQLITE_IOERR_VNODE)"
```

## What to do

1. **Never hard-link a store a live runtime holds open**, from any program, for any
   reason. Of the ways to read one that were tried, read-only by name, a copy, a
   clone and a symlink leave the writer alone; a link of any of the three files
   does not.
2. **On the next sticky `disk I/O error`, read the host's own log before arming
   anything.** On macOS: `/usr/bin/log show --last 30m --predicate 'eventMessage
   CONTAINS "BUG IN CLIENT OF libsqlite3"'` (`/usr/bin/log`, because zsh has a
   builtin called `log`). One line per affected connection, with the runtime's
   process id, at the second a program touched the files. That line was already in
   the system log, once per pass, while the errno trace of the earlier entry was
   being armed.
3. **Muster cannot refuse a link.** A process of the same user may link any file it
   can reach, and a profile on the runtime constrains the runtime, not its
   neighbours. What muster has is recovery: the auto-replace of a wedged server
   (#513) replaces it and reattaches the session. While a program keeps linking, a
   replacement is exposed to the next pass in the same way, so expect that bound to
   be reached; the cause has to stop at the program that links.
4. **A control must share the failing client's code path.** A SQLite writer from
   another language, or another build of the same library, answered "no problem"
   for a day. The control that mattered was the runtime's own interpreter.
