package opencode

import (
	"errors"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// Servers nobody holds (muster #398).
//
// A server process can outlive the session it served: the units abandon the
// process group when the service stops, and a record that is closed after that
// has no process attached any more. A record names the process it last started
// (Pid, Port, Bin), which is what reapStale uses; but the processes this file is
// for have NO record left to name them. What identifies one anyway is its own
// environment: every server this driver starts is given HOME, TMPDIR and the
// XDG_* variables inside a scoped directory under the session root
// (env.go), and a caller cannot override them. A process whose TMPDIR is
// <session root>/muster-opencode-<digits>/tmp, running `serve --port N
// --hostname 127.0.0.1` as the leader of its own process group, is a server of
// this driver's root — whether or not any record still says so.
//
// The environment of a server holds its credential (OPENCODE_SERVER_PASSWORD).
// It is read only to find the scoped directory's name; the text is dropped at
// once, never returned, wrapped into an error or logged.

// ErrScanUnsupported is the answer where the platform cannot list the machine's
// processes with their environment. Nothing is swept there and the doctor row is
// skipped. Off unix there is nothing to sweep anyway: a server cannot outlive the
// service on Windows (reap_windows.go).
var ErrScanUnsupported = errors.New("opencode: this platform cannot list running processes with their environment")

// serverProc is one running server found under a session root.
type serverProc struct {
	pid, pgid, port int
	// dir is the scoped directory's name (muster-opencode-<digits>), never its path.
	dir string
}

// serveArgs matches the arguments buildServeCmd gives every server. The port is
// the one the server was started on.
var serveArgs = regexp.MustCompile(`\sserve --port ([0-9]+) --hostname 127\.0\.0\.1(?:\s|$)`)

// servePort reports the port in line when line is an opencode server's command
// line, or any text that begins with one (a `ps -E` line carries the environment
// after the arguments).
func servePort(line string) (int, bool) {
	m := serveArgs.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	port, err := strconv.Atoi(m[1])
	return port, err == nil && port > 0
}

// scopedDirIn returns the name of the scoped directory line's TMPDIR points at,
// when it is one directly under root. The match is on the whole line, not on
// fields: an environment value may contain spaces (a root under "Application
// Support"), so the line cannot be tokenised, and the pattern is anchored at
// both ends instead — a variable of another name ending in TMPDIR, a sibling
// directory of root, or a value that merely starts with it does not match.
func scopedDirIn(line, root string) (string, bool) {
	sep := regexp.QuoteMeta(string(filepath.Separator))
	prefix := regexp.QuoteMeta(filepath.Clean(root)) + sep
	re := regexp.MustCompile(`(?:^|\s)TMPDIR=` + prefix + `(muster-opencode-[0-9]+)` + sep + `tmp(?:\s|$)`)
	m := re.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// say writes one notice, when the driver was given somewhere to write it.
func (d *Driver) say(format string, args ...any) {
	if d.logf != nil {
		d.logf(format, args...)
	}
}

// sweepOrphans stops every server under the session root before anything is
// resumed. At that point this driver holds no server at all, so every server
// found is one an earlier service left behind: the records that name a
// still-running server are resumed (or listed unknown) after, and the processes
// whose record is gone are exactly the ones nothing else would ever stop.
//
// It runs only with a state directory. Without one the root is the OS temp
// directory, which is not this driver's alone, and nothing outlives the service
// by design. A process it cannot read, one that is not its own group's leader,
// or any error leaves the process alone: the sweep stops a server only when
// every part of its identity was seen.
func (d *Driver) sweepOrphans() {
	if d.noStartSweep || d.store == nil {
		return
	}
	found, _, err := scanServers(d.sessionRoot)
	if err != nil {
		if !errors.Is(err, ErrScanUnsupported) {
			d.say("opencode: the start-up sweep could not list running servers: %v", err)
		}
		return
	}
	var stop []serverProc
	for _, p := range found {
		// Looked at again right before the signal, so a pid that was reused since the
		// scan is not signalled.
		if again, ok := inspectServer(p.pid, d.sessionRoot); ok && again.dir == p.dir && again.port == p.port {
			stop = append(stop, p)
		}
	}
	if len(stop) == 0 {
		return
	}
	pids := make([]int, len(stop))
	for i, p := range stop {
		pids[i] = p.pid
	}
	left := map[int]bool{}
	for _, pid := range stopGroups(pids) {
		left[pid] = true
	}
	for _, p := range stop {
		if left[p.pid] {
			d.say("opencode: a server left running by an earlier service did not stop (pid %d, port %d, dir %s)", p.pid, p.port, p.dir)
			continue
		}
		d.say("opencode: stopped a server left running by an earlier service (pid %d, port %d, dir %s)", p.pid, p.port, p.dir)
	}
}

// OrphanServers lists the servers running under stateDir's session root that no
// session record names: processes with no session (muster #398). The doctor runs
// it from another process, so it cannot see which servers the running service
// holds; a record is what a session leaves, and a server whose pid no record
// names has none. The one false positive is a create in flight — its server is
// up a moment before its record is written.
//
// uninspected counts servers that look like ours but could not be inspected
// (another user's, or unreadable): a count of zero from a doctor run as someone
// else is not an answer. err is ErrScanUnsupported where the platform cannot
// list processes.
func OrphanServers(stateDir string) (pids []int, uninspected int, err error) {
	storeDir, root := stateLayout(stateDir)
	found, uninspected, err := scanServers(root)
	if err != nil {
		return nil, 0, err
	}
	recorded := map[int]bool{}
	for _, it := range (&recordStore{dir: storeDir}).loadAll() {
		if it.rec != nil && it.rec.Pid > 0 {
			recorded[it.rec.Pid] = true
		}
	}
	for _, p := range found {
		if !recorded[p.pid] {
			pids = append(pids, p.pid)
		}
	}
	sort.Ints(pids)
	return pids, uninspected, nil
}

// stateLayout is where a state directory keeps the driver's records and the
// session-scoped directories its servers run in.
func stateLayout(stateDir string) (storeDir, sessionRoot string) {
	base := filepath.Join(stateDir, "opencode")
	return filepath.Join(base, "sessions"), filepath.Join(base, "dirs")
}
