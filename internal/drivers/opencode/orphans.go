package opencode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/futurelastic/muster/internal/privfs"
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

// Directories nobody holds (muster #399).
//
// #398 stops the servers nobody holds and leaves their scoped directories alone,
// on purpose: a directory holds the runtime's conversation database, and removing
// one is destructive. A directory whose record is gone was still never removed by
// any path, so they accumulate.
//
// What was measured on a machine that had them is not what the title suggests:
// none of the record-less directories was an intact tree whose record had gone.
// Every one was the remnant of a removal that did not finish — only the runtime's
// cache and lock files under xdg/, or only scratch under tmp/ — and none held the
// conversation database (xdg/data/opencode/opencode.db). So the rule below is not
// about age alone. A record-less directory is removed only when it provably holds
// nothing that is a session's own: every file in it lies in an area that is
// disposable by construction. One that does not is reported and never removed.

// orphanDirGrace is how long a record-less directory must have gone unwritten
// before it is removed. It is not what makes removal safe (the checks below are);
// it keeps the sweep off a create that is still being set up and off a remnant a
// person is looking at.
const orphanDirGrace = time.Hour

// scopedDirName is how sessionDirs names every directory it makes.
var scopedDirName = regexp.MustCompile(`^muster-opencode-[0-9]+$`)

// disposableAreas are the places in a scoped directory that hold nothing a
// session owns: scratch, caches, lock files and the configuration this driver
// writes itself. The conversation database lives in xdg/data and a session's
// working files in home, and neither is listed — a file anywhere else, or in a
// place this list does not know, means "may be a session's own".
var disposableAreas = []string{"tmp", "xdg/cache", "xdg/state", "xdg/config"}

// orphanDir is one scoped directory that no record names and no live server holds.
type orphanDir struct {
	// name is the directory's name under the session root, never its path.
	name string
	// bytes is the size of the regular files in it.
	bytes int64
	// newest is the latest modification time of anything in it, the directory
	// itself included: a directory's own time moves only when an entry is added
	// or removed directly in it, so a file written deeper does not show there.
	newest time.Time
	// holdsStore is true when anything in it lies outside disposableAreas — or
	// when it could not be read to the end, which cannot be told apart from a
	// store.
	holdsStore bool
}

// inspectDir reads one scoped directory without following a link.
func inspectDir(root, name string) orphanDir {
	dir := filepath.Join(root, name)
	o := orphanDir{name: name}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			o.holdsStore = true
			return nil
		}
		info, ierr := e.Info()
		if ierr != nil {
			o.holdsStore = true
			return nil
		}
		if t := info.ModTime(); t.After(o.newest) {
			o.newest = t
		}
		if e.IsDir() {
			return nil
		}
		if info.Mode().IsRegular() {
			o.bytes += info.Size()
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil || !inDisposableArea(filepath.ToSlash(rel)) {
			o.holdsStore = true
		}
		return nil
	})
	if err != nil {
		o.holdsStore = true
	}
	return o
}

// inDisposableArea reports whether rel (slash-separated, relative to a scoped
// directory) is a file inside one of disposableAreas.
func inDisposableArea(rel string) bool {
	for _, area := range disposableAreas {
		if strings.HasPrefix(rel, area+"/") {
			return true
		}
	}
	return false
}

// recordedDirs returns the names of the scoped directories the records in
// storeDir name, and how many record files could not be read. The comparison is on
// the last path element, so a record naming a directory under another root still
// holds the same-named one here: the wrong answer costs a directory kept, never
// one removed. A record that cannot be read could name any directory, which is why
// callers that remove treat a non-zero count as "remove nothing".
func recordedDirs(storeDir string) (names map[string]bool, unreadable int) {
	names = map[string]bool{}
	for _, it := range (&recordStore{dir: storeDir}).loadAll() {
		if it.rec == nil {
			unreadable++
			continue
		}
		if it.rec.Dir != "" {
			names[filepath.Base(filepath.Clean(it.rec.Dir))] = true
		}
	}
	return names, unreadable
}

// orphanDirs lists the scoped directories directly under root that no record in
// storeDir names and no running server holds, in name order. A link, or a file,
// that merely bears the name is not a scoped directory and is not listed.
//
// unreadable is the number of record files that could not be read and uninspected
// the number of processes that look like servers but could not be inspected: while
// either is non-zero the list may be missing a holder, and the caller must not
// treat a directory in it as one nobody holds. err is the scan's, including
// ErrScanUnsupported; the list is then nil.
func orphanDirs(storeDir, root string, scan func(string) ([]serverProc, int, error)) (dirs []orphanDir, unreadable, uninspected int, err error) {
	servers, uninspected, err := scan(root)
	if err != nil {
		return nil, 0, 0, err
	}
	held := map[string]bool{}
	for _, p := range servers {
		held[p.dir] = true
	}
	named, unreadable := recordedDirs(storeDir)
	entries, rerr := os.ReadDir(root)
	if rerr != nil {
		// A root that is not there has no directories in it.
		if errors.Is(rerr, fs.ErrNotExist) {
			return nil, unreadable, uninspected, nil
		}
		return nil, unreadable, uninspected, rerr
	}
	for _, e := range entries {
		// DirEntry.Type does not follow a link, so a link named like a scoped
		// directory is not a directory here.
		if !e.Type().IsDir() || !scopedDirName.MatchString(e.Name()) || named[e.Name()] || held[e.Name()] {
			continue
		}
		dirs = append(dirs, inspectDir(root, e.Name()))
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].name < dirs[j].name })
	return dirs, unreadable, uninspected, nil
}

// removable reports whether o may be removed at now: it holds nothing that is a
// session's own, and nothing in it has been written within the grace period.
func (o orphanDir) removable(now time.Time) bool {
	return !o.holdsStore && now.Sub(o.newest) >= orphanDirGrace
}

// sweepOrphanDirs removes the record-less scoped directories that hold nothing of
// a session's, once, at start-up. It runs after sweepOrphans (so a server that
// sweep stopped no longer holds its directory) and before anything is resumed.
//
// It removes nothing when a record could not be read, when a process that looks
// like a server could not be inspected, or when the platform cannot list
// processes: in each the list of directories nobody holds cannot be trusted.
// A directory that fails any other check stays, and the doctor row counts it.
func (d *Driver) sweepOrphanDirs() { d.sweepOrphanDirsWith(scanServers) }

// sweepOrphanDirsWith is sweepOrphanDirs with the process listing given, so a
// test on any platform can say which servers are running.
func (d *Driver) sweepOrphanDirsWith(scan func(string) ([]serverProc, int, error)) {
	if d.noStartSweep || d.store == nil {
		return
	}
	dirs, unreadable, uninspected, err := orphanDirs(d.store.dir, d.sessionRoot, scan)
	if err != nil {
		if !errors.Is(err, ErrScanUnsupported) {
			d.say("opencode: the start-up sweep could not list session directories: %v", err)
		}
		return
	}
	if unreadable > 0 || uninspected > 0 {
		if len(dirs) > 0 {
			d.say("opencode: session directories no record names were left in place: %d record(s) and %d process(es) could not be read, "+
				"so a directory could still be in use", unreadable, uninspected)
		}
		return
	}
	now := time.Now()
	for _, o := range dirs {
		if !o.removable(now) {
			continue
		}
		if err := privfs.RemoveAll(filepath.Join(d.sessionRoot, o.name)); err != nil {
			d.say("opencode: a session directory no record names could not be removed (%s): %v", o.name, err)
			continue
		}
		d.say("opencode: removed a session directory no record names, no server holds and nothing has written to for %s (%s, %d KiB)",
			now.Sub(o.newest).Round(time.Minute), o.name, (o.bytes+1023)/1024)
	}
}

// OrphanDirReport is what the doctor says about the scoped directories no record
// names.
type OrphanDirReport struct {
	// Removable counts directories holding nothing a session owns; the start-up
	// sweep removes those that have also gone unwritten for the grace period.
	// Bytes is their size.
	Removable      int
	RemovableBytes int64
	// HoldStore counts directories that may hold a session's own data — its
	// conversation database among them. They are never removed automatically.
	HoldStore      int
	HoldStoreBytes int64
	// Unreadable is the number of record files that could not be read, and
	// Uninspected the number of processes that look like servers but could not
	// be: with either non-zero the counts may include a directory that is in use.
	Unreadable  int
	Uninspected int
}

// Total is the number of directories counted.
func (r OrphanDirReport) Total() int { return r.Removable + r.HoldStore }

// OrphanDirs counts the scoped directories under stateDir's session root that no
// session record names and no running server holds (muster #399). Like
// OrphanServers it runs from the doctor, another process, so a directory a create
// in flight has just made is counted for a moment before its record is written.
// err is ErrScanUnsupported where the platform cannot list processes.
func OrphanDirs(stateDir string) (OrphanDirReport, error) {
	storeDir, root := stateLayout(stateDir)
	return orphanDirReport(storeDir, root, scanServers)
}

func orphanDirReport(storeDir, root string, scan func(string) ([]serverProc, int, error)) (OrphanDirReport, error) {
	dirs, unreadable, uninspected, err := orphanDirs(storeDir, root, scan)
	if err != nil {
		return OrphanDirReport{}, err
	}
	r := OrphanDirReport{Unreadable: unreadable, Uninspected: uninspected}
	for _, o := range dirs {
		if o.holdsStore {
			r.HoldStore++
			r.HoldStoreBytes += o.bytes
		} else {
			r.Removable++
			r.RemovableBytes += o.bytes
		}
	}
	return r, nil
}
