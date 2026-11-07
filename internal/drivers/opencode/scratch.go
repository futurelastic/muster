package opencode

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// What a session's scratch directory holds (muster #419).
//
// One idle session was found sitting on 3 GB in its temporary directory — test
// fixtures — while it waited for review, and nothing said so. The directory goes
// when the session is closed (Close tears the server down and removes the whole
// scoped directory, tmp/ included; isolation_test and readonly_close_test pin
// it). Until then the size is what is worth knowing, so State and List report it
// (fleet.ScratchUsage). Nothing is removed while the session lives: a running tool
// may be using the files, and whether a fixture is garbage is not this driver's
// call.
//
// # Measured off the request path, and bounded
//
// A walk costs one lstat per file, and a tree of fixtures can have hundreds of
// thousands of them. So a read never waits for one: it returns the figure the
// last walk produced (nil until there is one) and, when that is older than
// scratchRefresh, starts at most one walk in the background. A walk stops at
// scratchMaxEntries entries or scratchMaxWalk, whichever comes first, and then
// says so (Partial: the figure is a lower bound). Only the isolated driver reports
// it; in shared mode there is no directory of the driver's own to measure.
const (
	// scratchRefresh is how old a figure may get before a read starts a new walk.
	scratchRefresh = 10 * time.Minute
	// scratchMaxEntries and scratchMaxWalk bound one walk.
	scratchMaxEntries = 200_000
	scratchMaxWalk    = 3 * time.Second
)

// scratchLimits are the bounds of a walk and whether it runs inline. Fields of the
// driver so a test can set small ones; the zero value means the constants above.
type scratchLimits struct {
	refresh    time.Duration
	maxEntries int
	maxWalk    time.Duration
	// inline makes a read wait for its walk instead of starting it in the
	// background, so a test sees the figure on the read that asked for it.
	inline bool
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
	return l
}

// scratchMemo is what one server remembers of its scratch directory.
type scratchMemo struct {
	mu      sync.Mutex
	have    bool
	at      time.Time // when the last walk finished
	bytes   int64
	partial bool
	running bool
}

// scratchOf is the figure State and List attach for srv: the last walk's, with a
// new walk started if it is stale. nil when there is nothing to report yet or no
// directory to measure.
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
			bytes, partial, ok := measureScratch(filepath.Join(srv.dir, "tmp"), lim)
			m.mu.Lock()
			if ok {
				m.have, m.at, m.bytes, m.partial = true, d.now(), bytes, partial
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
	return &fleet.ScratchUsage{Bytes: m.bytes, AsOf: m.at.UTC(), Partial: m.partial}
}

// measureScratch sums the sizes of the regular files under dir without following a
// link. ok is false when there is no directory to measure (it does not exist), which
// is not a size of zero. partial is true when the walk stopped at its bound or could
// not read part of the tree: the figure is then a lower bound.
func measureScratch(dir string, lim scratchLimits) (bytes int64, partial, ok bool) {
	deadline := time.Now().Add(lim.maxWalk)
	entries := 0
	errStop := errors.New("bound reached")
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			if path == dir && errors.Is(err, fs.ErrNotExist) {
				return err
			}
			partial = true // a part of the tree could not be read
			if e != nil && e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entries++
		if entries > lim.maxEntries || (entries%256 == 0 && time.Now().After(deadline)) {
			partial = true
			return errStop
		}
		if e.Type().IsRegular() {
			if info, ierr := e.Info(); ierr == nil {
				bytes += info.Size()
			} else {
				partial = true
			}
		}
		return nil
	})
	switch {
	case err == nil, errors.Is(err, errStop):
		return bytes, partial, true
	case errors.Is(err, fs.ErrNotExist):
		return 0, false, false
	default:
		return bytes, true, true
	}
}
