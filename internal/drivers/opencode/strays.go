package opencode

import (
	"errors"
	"path/filepath"
	"regexp"
	"sort"
)

// Processes a session started that outlived it (muster #405).
//
// Stopping a server stops its process group (process.go), and #398 finds a server
// the group stop did not reach. Neither reaches a process that LEFT the group: a
// tool the session ran can start a browser, a language server or a watcher in a
// session of its own (setsid), and that process is then reparented to init, keeps
// running, and has nothing left to name it — measured as headless browsers that
// burned CPU for over an hour after the session that started them was gone.
//
// What names such a process anyway is where it lives. Every session runs inside
// one scoped directory (env.go) that nothing else uses, and a process the session
// started is tied to it by one of three facts a caller cannot change after the
// fact:
//
//   - executable: its command line begins with a path inside the directory (a
//     browser a tool installed under the session's own temp area);
//   - profile: its command line carries --user-data-dir= with a path inside the
//     directory (a browser started with a profile there);
//   - environment: its TMPDIR is the directory's tmp — the variable the server was
//     started with, which every process the server's tools start inherits.
//
// The rule decides what gets killed, so it is deliberately narrow: each fact is
// matched on the WHOLE path (the session root, the scoped name's pattern and a
// separator), so a sibling directory, another root, or a directory whose name
// merely begins with the same digits does not match. It is only ever applied at a
// moment the directory's session is already over (a close, a shutdown, the
// start-up before anything is resumed), never to a live session.
//
// The environment of a process can hold its credentials. It is matched in memory
// and dropped at once, never returned, wrapped into an error or logged; a log line
// carries a count and the directory's NAME.

// strayBy names which fact tied a process to its directory, for the log.
type strayBy string

const (
	strayByExecutable  strayBy = "executable"
	strayByProfile     strayBy = "profile"
	strayByEnvironment strayBy = "environment"
)

// strayProc is one process, in any process group, that belongs to a scoped
// directory.
type strayProc struct {
	pid int
	// dir is the scoped directory's name (muster-opencode-<digits>), never its path.
	dir string
	by  strayBy
}

// strayMatcher decides, for one command line (followed by the environment where it
// could be read), which scoped directory under one root the process belongs to.
// It is built once per scan: compiling the patterns for every line of a machine's
// process table would be most of the scan's cost.
type strayMatcher struct {
	exe, profile, tmp *regexp.Regexp
}

func newStrayMatcher(root string) *strayMatcher {
	sep := regexp.QuoteMeta(string(filepath.Separator))
	// The full path of a scoped directory, captured by its name and ended by a
	// separator: the same path scopedDirIn anchors the TMPDIR rule on.
	dir := regexp.QuoteMeta(filepath.Clean(root)) + sep + `(muster-opencode-[0-9]+)` + sep
	return &strayMatcher{
		exe:     regexp.MustCompile(`^` + dir),
		profile: regexp.MustCompile(`(?:^|\s)--user-data-dir=` + dir),
		tmp:     regexp.MustCompile(`(?:^|\s)TMPDIR=` + dir + `tmp(?:\s|$)`),
	}
}

// match reports the scoped directory line belongs to and the fact that says so.
// Command-line facts are tried first: they hold where the environment could not be
// read.
func (m *strayMatcher) match(line string) (dir string, by strayBy, ok bool) {
	if g := m.exe.FindStringSubmatch(line); g != nil {
		return g[1], strayByExecutable, true
	}
	if g := m.profile.FindStringSubmatch(line); g != nil {
		return g[1], strayByProfile, true
	}
	if g := m.tmp.FindStringSubmatch(line); g != nil {
		return g[1], strayByEnvironment, true
	}
	return "", "", false
}

// strayFromLine builds the result for one process from its command line, followed
// by its environment where that could be read. only, when non-empty, narrows the
// answer to one scoped directory's name.
func (m *strayMatcher) strayFromLine(line string, pid int, only string) (strayProc, bool) {
	dir, by, ok := m.match(line)
	if !ok || (only != "" && dir != only) {
		return strayProc{}, false
	}
	return strayProc{pid: pid, dir: dir, by: by}, true
}

// stopStraysIn stops the processes that belong to one session's scoped directory,
// given its path. A path that is not directly a scoped directory by name is not
// one this driver made, and nothing is looked for under it: the rule's anchors
// are built from the root and the name, and a name that does not fit them would
// not be a directory the rule was written for.
func stopStraysIn(dir string) (stopped, left []strayProc) {
	if dir == "" {
		return nil, nil
	}
	name := filepath.Base(dir)
	if !scopedDirName.MatchString(name) {
		return nil, nil
	}
	stopped, left, _ = stopStrays(filepath.Dir(dir), name)
	return stopped, left
}

// sweepOrphanStrays stops, before anything is resumed, every process that belongs
// to a scoped directory under the session root and is not a server — the
// processes an earlier service's sessions started and left behind. At that point
// this driver holds no session, so every one found is one nobody owns; the
// servers themselves were stopped by sweepOrphans just before, and the same rule
// would stop one that survived it.
//
// It runs only with a state directory, for the reason sweepOrphans does: without
// one the root is the OS temp directory, which is not this driver's alone.
func (d *Driver) sweepOrphanStrays() {
	if d.noStartSweep || d.noStraySweep || d.store == nil {
		return
	}
	stopped, left, err := stopStrays(d.sessionRoot, "")
	if err != nil {
		if !errors.Is(err, ErrScanUnsupported) {
			d.say("opencode: the start-up sweep could not list running processes: %v", err)
		}
		return
	}
	d.sayStrays("left running by an earlier service", stopped, left)
}

// sayStrays writes one line per scoped directory for what was stopped and one for
// what would not stop: counts and the directory's name, never a command line.
func (d *Driver) sayStrays(what string, stopped, left []strayProc) {
	for _, g := range groupByDir(stopped) {
		d.say("opencode: stopped %d process(es) a session started that were %s (dir %s)", g.n, what, g.dir)
	}
	for _, g := range groupByDir(left) {
		d.say("opencode: %d process(es) a session started did not stop (dir %s)", g.n, g.dir)
	}
}

type dirCount struct {
	dir string
	n   int
}

// groupByDir counts procs per scoped directory, in name order.
func groupByDir(procs []strayProc) []dirCount {
	counts := map[string]int{}
	for _, p := range procs {
		counts[p.dir]++
	}
	out := make([]dirCount, 0, len(counts))
	for dir, n := range counts {
		out = append(out, dirCount{dir: dir, n: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}
