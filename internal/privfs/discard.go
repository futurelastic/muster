package privfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/futurelastic/muster/internal/lowprio"
)

// DiscardPrefix starts the name of a directory that Discard set aside and has not
// finished deleting. It is distinctive on purpose: ResumeDiscards deletes every
// directory carrying it, and the parent it is pointed at may be a shared one.
const DiscardPrefix = ".muster-removing-"

// Discard takes the directory at path out of the way now and deletes it afterwards,
// slowly (muster #436). It is RemoveAll for a tree that is expensive to delete.
//
// # Why not just RemoveAll
//
// A session's private home holds whatever its tools downloaded, and a language's
// package cache is hundreds of thousands of small, read-only files. Removing one
// takes a chmod and an unlink per entry, which on a measured machine meant about
// 366,000 file system calls in nine seconds from one thread. Every one of them is an
// event for the system's file watchers and a scan for its malware checks, and a
// machine closing thirty sessions an hour spent its time on that rather than on the
// sessions. The cost is not in the deleting; it is in doing it all at once, in the
// caller's thread, at full priority.
//
// # What it does
//
// The directory is renamed to a hidden sibling, DiscardPrefix + its own name + a
// random suffix. A rename within one directory is atomic and never crosses a
// volume, so it is the same cost for a tree of ten files and ten million, and the
// original name is free the moment it returns. The sibling goes on a queue served
// by one goroutine for the whole process: it runs at background priority (package
// lowprio), deletes at most discardRate entries a second, and makes a directory
// writable before emptying it, as RemoveAll does for a tree a package cache locked.
//
// # What it promises and what it does not
//
// The path is gone when Discard returns nil; the disk space is not back yet. A
// process that is stopped while the queue is not empty leaves directories carrying
// DiscardPrefix, and ResumeDiscards on their parent finishes them.
//
// Discard falls back to RemoveAll — synchronous, with its error — wherever it cannot
// set the directory aside: the path is not a directory (a file or a link is one
// unlink, nothing to spread out), the parent is not writable, or the platform refuses
// a rename of a directory something has open. So a caller that reported RemoveAll's
// failure to a user reports the same failure here, and one that did not loses nothing.
//
// A failure of the deletion itself happens later and is not returned. It is kept,
// the entry stays where it is, and the next ResumeDiscards on its parent reports it
// and tries again.
func Discard(path string) error { return discards.discard(path) }

// DiscardFailure is a directory that Discard set aside and could not finish deleting.
type DiscardFailure struct {
	Path string
	Err  error
}

// ResumeDiscards queues every directory in dir that carries DiscardPrefix, so that a
// restart finishes what the process before it was in the middle of. It also returns
// the deletions that failed since the last call for this dir — the entries are queued
// again, so a second failure is reported again next time and a success is not. queued
// counts directories handed to the deleter, not those it was already working on.
//
// It returns without waiting for any deletion. A dir that does not exist has nothing
// to resume and is not an error.
func ResumeDiscards(dir string) (queued int, failed []DiscardFailure, err error) {
	return discards.resume(dir)
}

// WaitDiscards waits until nothing is queued or being deleted, up to timeout, and
// reports whether it got there. It is for a test that needs the disk to be as the
// deleter will leave it, and for a tool that wants a clean directory before it looks.
func WaitDiscards(timeout time.Duration) bool { return discards.wait(timeout) }

// The deleter's default pace. Unpaced, the deletion measured 13,500 entries a second
// on a tree of read-only Go module cache shape, against 2,400 for RemoveAll's chmod
// walk followed by its removal; 5,000 is the pace the host is asked to bear, about
// one file system call per entry. A small tree, which is nearly every tree a session
// leaves that was not a package cache, is under the burst and is not slowed at all.
//
// discardBacklog is how many set-aside directories may wait before a second deleter,
// at normal priority, helps the background one. Background priority is a promise to
// give way, and on a busy machine it is kept: the same tree took 120 s at background
// priority and 20 s at normal priority at the same pace. Left alone, a burst of
// closes would be deleted more slowly than it arrived and the disk would fill with
// directories nobody uses. The helper ends when the queue is down to half the limit.
const (
	defaultDiscardRate    = 5000.0
	defaultDiscardBurst   = 500.0
	defaultDiscardBacklog = 6
)

// discards is the process-wide deleter behind Discard and ResumeDiscards.
var discards = newDiscarder(pacing{rate: defaultDiscardRate, burst: defaultDiscardBurst, backlog: defaultDiscardBacklog})

// pacing is how fast a deleter may work. The clock and the sleep are fields so a
// test can run a minute's pacing without waiting for it.
type pacing struct {
	rate    float64 // entries per second; zero or less means unpaced
	burst   float64 // entries that may go before the rate applies
	backlog int     // queued directories that call a helper in; zero means never
	now     func() time.Time
	sleep   func(time.Duration)
}

type discarder struct {
	pace pacing

	mu      sync.Mutex
	queue   []string
	queued  map[string]struct{} // the queue, and the ones being deleted
	failed  map[string]error
	running bool // the background deleter
	helping bool // the helper that a backlog calls in
}

func newDiscarder(p pacing) *discarder {
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = time.Sleep
	}
	return &discarder{pace: p, queued: map[string]struct{}{}, failed: map[string]error{}}
}

func (d *discarder) discard(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !fi.IsDir() {
		return RemoveAll(path)
	}
	aside, err := setAside(path)
	if err != nil && errors.Is(err, fs.ErrPermission) {
		// A directory its owner cannot write to cannot be renamed on some systems
		// (macOS refuses with "permission denied"): the rename rewrites the entry the
		// directory keeps for its parent. A tool that locked the session's own top
		// directory is what this is for; every directory below it moves with it, as
		// they are.
		if chmodAdd(path, fi, 0o700) == nil {
			aside, err = setAside(path)
		}
	}
	if err != nil {
		return RemoveAll(path)
	}
	d.add(aside)
	return nil
}

// setAside renames the directory to a hidden sibling and returns the new path.
func setAside(path string) (string, error) {
	path = filepath.Clean(path)
	parent, base := filepath.Split(path)
	if parent == "" {
		parent = "."
	}
	if base == "" || base == "." || base == ".." || base == string(filepath.Separator) {
		return "", fmt.Errorf("privfs: %q is not a directory name", path)
	}
	if len(base) > 120 {
		base = base[:120]
	}
	var lastErr error
	for range 8 {
		aside := filepath.Join(parent, DiscardPrefix+base+"-"+strconv.FormatUint(rand.Uint64(), 36))
		// A rename onto an empty directory replaces it, so an existing one is
		// looked for rather than relied on to fail.
		if _, err := os.Lstat(aside); err == nil {
			continue
		}
		if lastErr = os.Rename(path, aside); lastErr == nil {
			return aside, nil
		}
		break
	}
	if lastErr == nil {
		lastErr = errors.New("privfs: no free name to set the directory aside under")
	}
	return "", lastErr
}

// add queues path unless it is already queued or being deleted, and starts the
// deleter if none is running.
func (d *discarder) add(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.queued[path]; ok {
		return
	}
	delete(d.failed, path)
	d.queued[path] = struct{}{}
	d.queue = append(d.queue, path)
	if !d.running {
		d.running = true
		lowprio.Go(d.run)
	}
	if d.pace.backlog > 0 && len(d.queue) >= d.pace.backlog && !d.helping {
		d.helping = true
		go d.help()
	}
}

// run is the background deleter. It ends when the queue is empty, so an idle process
// holds no goroutine and no thread for it; the next add starts another.
func (d *discarder) run() {
	d.drain(func() bool { return len(d.queue) == 0 }, func() { d.running = false })
}

// help is the second deleter a backlog calls in, at normal priority. It ends when the
// queue is down to half the limit, and leaves the rest to run.
func (d *discarder) help() {
	d.drain(func() bool { return len(d.queue) < (d.pace.backlog+1)/2 }, func() { d.helping = false })
}

// drain deletes queued directories until done, which is asked with the lock held, says
// to stop; finish then runs under the same hold. Each deleter paces itself.
func (d *discarder) drain(done func() bool, finish func()) {
	p := d.newPacer()
	for {
		d.mu.Lock()
		if done() {
			finish()
			d.mu.Unlock()
			return
		}
		path := d.queue[0]
		d.queue = d.queue[1:]
		d.mu.Unlock()

		err := removeSlowly(path, p)

		d.mu.Lock()
		delete(d.queued, path)
		if err != nil {
			d.failed[path] = err
		}
		d.mu.Unlock()
	}
}

func (d *discarder) resume(dir string) (int, []DiscardFailure, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	dir = filepath.Clean(dir)

	// The failures are taken before the entries are queued again, and only the
	// ones that were in this dir.
	var failed []DiscardFailure
	d.mu.Lock()
	for path, ferr := range d.failed {
		if filepath.Dir(path) == dir {
			failed = append(failed, DiscardFailure{Path: path, Err: ferr})
			delete(d.failed, path)
		}
	}
	d.mu.Unlock()

	queued := 0
	for _, e := range entries {
		// Type does not follow a link: a link named like one is not a directory.
		if !e.Type().IsDir() || !strings.HasPrefix(e.Name(), DiscardPrefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		d.mu.Lock()
		_, already := d.queued[path]
		d.mu.Unlock()
		if already {
			continue
		}
		d.add(path)
		queued++
	}
	slices.SortFunc(failed, func(a, b DiscardFailure) int { return strings.Compare(a.Path, b.Path) })
	return queued, failed, nil
}

func (d *discarder) wait(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		d.mu.Lock()
		idle := !d.running && !d.helping && len(d.queue) == 0
		d.mu.Unlock()
		if idle {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pacer spends one token per entry and sleeps when it has spent more than it was
// given. It sleeps only once the debt is worth a sleep (minSleep), so a fast rate
// does not turn into a system call per entry.
type pacer struct {
	pacing
	tokens float64
	last   time.Time
}

const minSleep = 5 * time.Millisecond

func (d *discarder) newPacer() *pacer {
	return &pacer{pacing: d.pace, tokens: d.pace.burst, last: d.pace.now()}
}

func (p *pacer) tick() {
	if p.rate <= 0 {
		return
	}
	now := p.now()
	p.tokens += now.Sub(p.last).Seconds() * p.rate
	if p.tokens > p.burst {
		p.tokens = p.burst
	}
	p.last = now
	p.tokens--
	if p.tokens >= 0 {
		return
	}
	if wait := time.Duration(-p.tokens / p.rate * float64(time.Second)); wait >= minSleep {
		p.sleep(wait)
		p.last = p.now()
		p.tokens = 0
	}
}

// removeSlowly deletes the tree at path, entry by entry at the pacer's rate. It is
// RemoveAll's contract with one difference, which is the point: it does not make the
// whole tree writable first (that is a walk of its own) but each directory as it
// reaches it. The walk runs inside an os.Root, so no link in the tree is followed out
// of it; a link is unlinked as a link.
//
// It keeps going past an entry it cannot remove, so one stubborn file does not leave
// the rest of a tree on disk, and returns the first error.
func removeSlowly(path string, p *pacer) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !fi.IsDir() {
		p.tick()
		return os.Remove(path)
	}
	// Opening a directory needs r and x, emptying it needs w.
	_ = chmodAdd(path, fi, 0o700)
	root, err := os.OpenRoot(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer root.Close()
	// A process the session started and nothing stopped can write into the tree
	// while it is deleted, which makes the last rmdir fail on an entry that was not
	// there a moment ago. Emptying again is the answer, a few times at most.
	for attempt := 1; ; attempt++ {
		if err := emptyRoot(root, p); err != nil {
			return err
		}
		p.tick()
		err := os.Remove(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if attempt >= 3 {
			return err
		}
	}
}

// emptyRoot removes everything inside root. A directory read while its entries are
// being removed may skip some on some file systems, so it reads again until a pass
// finds nothing, or finds only entries it cannot remove.
func emptyRoot(root *os.Root, p *pacer) error {
	var first error
	for {
		seen, removed, err := emptyPass(root, p)
		if first == nil {
			first = err
		}
		if seen == 0 || removed == 0 {
			return first
		}
	}
}

func emptyPass(root *os.Root, p *pacer) (seen, removed int, first error) {
	dir, err := root.Open(".")
	if err != nil {
		return 0, 0, err
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(256)
		for _, e := range entries {
			seen++
			p.tick()
			if rerr := removeEntry(root, e, p); rerr != nil {
				if first == nil {
					first = rerr
				}
				continue
			}
			removed++
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && first == nil {
				first = err
			}
			return seen, removed, first
		}
	}
}

func removeEntry(root *os.Root, e fs.DirEntry, p *pacer) error {
	name := e.Name()
	if !e.Type().IsDir() {
		return unlinkEntry(root, name, 0o600)
	}
	// A directory a package cache locked cannot be listed or emptied until it is
	// made owner-readable, -searchable and -writable. Chmod through the root, so a
	// link swapped in for it cannot send the change out of the tree.
	if info, err := root.Lstat(name); err == nil && info.Mode().Perm()&0o700 != 0o700 {
		_ = root.Chmod(name, info.Mode().Perm()|0o700)
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // another deleter got there first (a second driver, a person)
		}
		return err
	}
	// One pacer for the whole tree: an entry costs the same wherever it is, so a
	// deep tree and a wide one are paced alike.
	err = emptyRoot(sub, p)
	sub.Close()
	if err != nil {
		return err
	}
	return unlinkEntry(root, name, 0o700)
}

// unlinkEntry removes name from root. An entry that is already gone is a success.
//
// Where the platform refuses a delete for a read-only attribute (Windows), the
// entry's mode is loosened once and the delete tried again. Elsewhere that is never
// the obstacle — unlinking needs write access to the directory, which emptyRoot has
// already seen to — and a chmod would only change an inode that a hard link may
// share with something outside this tree (a shared package store, say).
func unlinkEntry(root *os.Root, name string, loosen fs.FileMode) error {
	err := root.Remove(name)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if runtime.GOOS != "windows" || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if info, lerr := root.Lstat(name); lerr == nil && info.Mode()&fs.ModeSymlink == 0 {
		_ = root.Chmod(name, info.Mode().Perm()|loosen)
	}
	return root.Remove(name)
}
