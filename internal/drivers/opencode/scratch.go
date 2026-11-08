package opencode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// What a session's scratch directory holds (muster #419, #431).
//
// One idle session was found sitting on 3 GB in its temporary directory — test
// fixtures — while it waited for review, and nothing said so. The directory goes
// when the session is closed (Close tears the server down and removes the whole
// scoped directory; isolation_test and readonly_close_test pin it). Until then the
// size is what is worth knowing, so State and List report it (fleet.ScratchUsage).
// Nothing is removed while the session lives: a running tool may be using the files,
// and whether a fixture is garbage is not this driver's call.
//
// # The whole directory, not one part of it
//
// The first version walked tmp/ alone, and on one machine reported 8.5 GiB for 45
// sessions whose directories held 49.2 GiB (#431). The rest was home/ and xdg/ — the
// Go, Cargo and npm caches and the toolchains a session's tools fetched — and the tmp/
// of sessions that had died. Closing a session removes all of it, so all of it counts:
// the walk covers the session's whole directory, and the figure says how it splits
// between tmp/, home/ and xdg/ so a reader can tell build output from caches.
//
// # A dead session still has a directory
//
// A session whose process exited keeps its directory until it is closed (the record
// still names it, so the sweep leaves it alone), and the largest directory on that
// machine, 11.5 GiB, belonged to one. So the figure does not depend on the session's
// status: it depends on the directory existing.
//
// # Measured off the request path, and bounded
//
// A walk costs one lstat per file — about 19 µs each on the machine it was measured
// on — and a cache can hold millions. So a read never waits for one: it returns the
// figure the last walk produced (nil until there is one) and, when that is older than
// scratchRefresh, starts at most one walk for that session in the background. Because
// nothing waits for a walk, its bound is not there to protect a caller's latency; it is
// there to protect the machine, and does so three ways: a walk stops at
// scratchMaxEntries entries or scratchMaxWalk, whichever comes first, and then says so
// (Partial: the figure is a lower bound); at most scratchMaxWalks walks run at once
// across the driver, the rest queueing (without that a machine with many sessions
// would run one walker each, all at the same moment); and a session is not walked
// again until scratchRefresh after its last walk ended. Only the isolated driver
// reports it; in shared mode there is no directory of the driver's own to measure.
const (
	// scratchRefresh is how old a figure may get before a read starts a new walk.
	scratchRefresh = 10 * time.Minute
	// scratchMaxEntries and scratchMaxWalk bound one walk. They are far above what
	// a session's directory held when measured, so that a large tree finishes (#431:
	// the previous 200,000 entries and 3 s stopped one session at 0.88 GiB of 6.12).
	scratchMaxEntries = 5_000_000
	scratchMaxWalk    = 3 * time.Minute
	// scratchMaxWalks is how many walks may run at once across the driver.
	scratchMaxWalks = 2
)

// The areas of a session's directory a figure is split into. They are the
// directories the driver itself gives the session (env_unix.go: HOME, TMPDIR and the
// XDG_* variables). Anything else at the top of the directory is "other".
const (
	scratchAreaTmp   = "tmp"
	scratchAreaHome  = "home"
	scratchAreaXDG   = "xdg"
	scratchAreaOther = "other"
)

// scratchLimits are the bounds of a walk and whether it runs inline. Fields of the
// driver so a test can set small ones; the zero value means the constants above.
type scratchLimits struct {
	refresh    time.Duration
	maxEntries int
	maxWalk    time.Duration
	// maxWalks is how many walks may run at once across the driver.
	maxWalks int
	// inline makes a read wait for its walk instead of starting it in the
	// background, so a test sees the figure on the read that asked for it.
	inline bool
	// walkDir walks one tree the way filepath.WalkDir does. Nil means the file
	// system; a test sets it to present a tree too large to build on disk.
	walkDir func(root string, fn fs.WalkDirFunc) error
}

// withScratchLimits sets the bounds. Unexported: a test seam.
func withScratchLimits(l scratchLimits) Option { return func(d *Driver) { d.scratchLimits = l } }

func (l scratchLimits) normalized() scratchLimits {
	if l.refresh <= 0 {
		l.refresh = scratchRefresh
	}
	if l.maxEntries <= 0 {
		l.maxEntries = scratchMaxEntries
	}
	if l.maxWalk <= 0 {
		l.maxWalk = scratchMaxWalk
	}
	if l.maxWalks <= 0 {
		l.maxWalks = scratchMaxWalks
	}
	if l.walkDir == nil {
		l.walkDir = filepath.WalkDir
	}
	return l
}

// scratchMemo is what one server remembers of its scratch directory.
type scratchMemo struct {
	mu      sync.Mutex
	have    bool
	at      time.Time // when the last walk finished
	areas   map[string]int64
	partial bool
	running bool
}

// scratchFigure is the result of one walk: the bytes found in each area, and whether
// the walk was cut short or could not read part of the tree (a lower bound).
type scratchFigure struct {
	areas   map[string]int64
	partial bool
}

// total is the size of the whole directory: the areas added up, so the two can never
// disagree.
func (f scratchFigure) total() int64 {
	var n int64
	for _, b := range f.areas {
		n += b
	}
	return n
}

// walkGate is the channel whose capacity is the number of walks that may run at once
// across this driver. Made on first use from the limits then in force: a Driver built
// as a literal in a test has none until something measures.
func (d *Driver) walkGate() chan struct{} {
	d.scratchGateOnce.Do(func() {
		d.scratchGate = make(chan struct{}, d.scratchLimits.normalized().maxWalks)
	})
	return d.scratchGate
}

// measureQueued is measureScratch, after waiting for one of the driver's walk places.
// The walk's own time bound starts when it does, not when it was asked for.
func (d *Driver) measureQueued(dir string, lim scratchLimits) (scratchFigure, bool) {
	gate := d.walkGate()
	gate <- struct{}{}
	defer func() { <-gate }()
	return measureScratch(dir, lim)
}

// scratchOf is the figure State and List attach for srv: the last walk's, with a
// new walk started if it is stale. nil when there is nothing to report yet or no
// directory to measure. It reads the directory and never the server, so it does not
// matter whether the session is alive.
func (d *Driver) scratchOf(srv *server) *fleet.ScratchUsage {
	if srv == nil || srv.dir == "" {
		return nil
	}
	lim := d.scratchLimits.normalized()
	m := &srv.scratch
	now := d.now()

	m.mu.Lock()
	stale := !m.have || now.Sub(m.at) >= lim.refresh
	start := stale && !m.running
	if start {
		m.running = true
	}
	m.mu.Unlock()

	if start {
		walk := func() {
			fig, ok := d.measureQueued(srv.dir, lim)
			m.mu.Lock()
			if ok {
				m.have, m.at, m.areas, m.partial = true, d.now(), fig.areas, fig.partial
			} else {
				// The directory is gone (closed, or swept): an earlier figure is no
				// longer a fact about anything, and absent is what that means.
				m.have, m.areas, m.partial = false, nil, false
			}
			m.running = false
			m.mu.Unlock()
		}
		if lim.inline {
			walk()
		} else {
			go walk()
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.have {
		return nil
	}
	// A copy: the memo's map is replaced by the next walk, never edited, but what a
	// reader is handed should not alias it.
	fig := scratchFigure{areas: make(map[string]int64, len(m.areas)), partial: m.partial}
	for k, v := range m.areas {
		fig.areas[k] = v
	}
	return &fleet.ScratchUsage{Bytes: fig.total(), Areas: fig.areas, AsOf: m.at.UTC(), Partial: m.partial}
}

// scratchAreaOf names the area a top-level entry of a session's directory belongs to.
func scratchAreaOf(name string) string {
	switch name {
	case scratchAreaTmp, scratchAreaHome, scratchAreaXDG:
		return name
	}
	return scratchAreaOther
}

// measureScratch sums the sizes of the regular files under dir, the session's whole
// directory, without following a link, and splits the sum by area. ok is false when
// there is no directory to measure (it does not exist), which is not a size of zero.
// partial is true when the walk stopped at its bound or could not read part of the
// tree: the figure is then a lower bound. The entry and time bounds are shared by the
// whole directory, not given to each area.
func measureScratch(dir string, lim scratchLimits) (fig scratchFigure, ok bool) {
	top, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return scratchFigure{}, false
	}
	// tmp, home and xdg are reported even when empty: they are the areas the driver
	// made, and a reader looking for one should find a number. "other" is only
	// there when it holds something.
	fig.areas = map[string]int64{scratchAreaTmp: 0, scratchAreaHome: 0, scratchAreaXDG: 0}
	if err != nil {
		fig.partial = true // some of the top level could not be listed
	}

	deadline := time.Now().Add(lim.maxWalk)
	entries := 0
	errStop := errors.New("bound reached")
walking:
	for _, t := range top {
		area := scratchAreaOf(t.Name())
		err := lim.walkDir(filepath.Join(dir, t.Name()), func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				fig.partial = true // a part of the tree could not be read
				if e != nil && e.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			entries++
			if entries > lim.maxEntries || (entries%256 == 0 && time.Now().After(deadline)) {
				fig.partial = true
				return errStop
			}
			if e.Type().IsRegular() {
				if info, ierr := e.Info(); ierr == nil {
					fig.areas[area] += info.Size()
				} else {
					fig.partial = true
				}
			}
			return nil
		})
		switch {
		case err == nil:
		case errors.Is(err, errStop):
			break walking
		default:
			fig.partial = true
		}
	}
	if fig.areas[scratchAreaOther] == 0 {
		delete(fig.areas, scratchAreaOther)
	}
	return fig, true
}
