package grant

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/futurelastic/muster/internal/privfs"
)

// ErrNoGrant is Load's answer when nothing is stored.
var ErrNoGrant = errors.New("grant: nothing is stored")

// Store is the owner-only file the grant lives in: <state dir>/grants/openai.json.
//
// Two processes touch it — the service's refresher and the operator's `muster
// grant` command — so a read-modify-write goes through a lock file (an exclusive
// create, which every platform has) and a write never leaves a half-written file
// behind: it is written whole beside the target and renamed over it.
type Store struct {
	dir string
	// lockWait and lockStale bound the lock: how long a caller waits for it, and
	// how old a lock file must be before it is taken to belong to a process that
	// died holding it. Fields so a test does not wait seconds.
	lockWait  time.Duration
	lockStale time.Duration
}

// NewStore returns the store for a service whose state directory is stateDir.
func NewStore(stateDir string) *Store {
	return &Store{
		dir:       filepath.Join(stateDir, "grants"),
		lockWait:  5 * time.Second,
		lockStale: 30 * time.Second,
	}
}

// Path is the grant file.
func (s *Store) Path() string { return filepath.Join(s.dir, ProviderOpenAI+".json") }

func (s *Store) lockPath() string { return filepath.Join(s.dir, ProviderOpenAI+".lock") }

// Load reads the grant. ErrNoGrant means none is stored; any other error means
// the file is there and unusable, which is never read as "signed out".
func (s *Store) Load() (Grant, error) {
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Grant{}, ErrNoGrant
		}
		return Grant{}, fmt.Errorf("grant: reading the store: %w", err)
	}
	var g Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return Grant{}, fmt.Errorf("grant: the store is not readable JSON (%v); sign in again to replace it", err)
	}
	if g.Version != 1 || g.Refresh == "" {
		return Grant{}, errors.New("grant: the store holds no usable grant; sign in again to replace it")
	}
	return g, nil
}

// Replace stores g as a new sign-in: its generation is one past whatever was
// stored, its revocation mark is cleared, and the write is atomic.
func (s *Store) Replace(g Grant) (Grant, error) {
	var out Grant
	err := s.locked(func() error {
		var prev int64
		if cur, err := s.Load(); err == nil {
			prev = cur.Generation
		}
		g.Version = 1
		g.Generation = prev + 1
		g.Revoked = nil
		g.Limit = nil // a new sign-in may be a different account
		out = g
		return s.write(g)
	})
	return out, err
}

// Update applies fn to the stored grant and saves it, but only if the stored
// generation is still gen. It reports false, saving nothing, when a sign-in
// landed in the meantime or the grant is gone: whoever asked is working from a
// grant that no longer exists.
func (s *Store) Update(gen int64, fn func(*Grant)) (Grant, bool, error) {
	var out Grant
	ok := false
	err := s.locked(func() error {
		cur, err := s.Load()
		if err != nil {
			if errors.Is(err, ErrNoGrant) {
				return nil
			}
			return err
		}
		if cur.Generation != gen {
			return nil
		}
		fn(&cur)
		if err := s.write(cur); err != nil {
			return err
		}
		out, ok = cur, true
		return nil
	})
	return out, ok, err
}

// Remove deletes the stored grant; none stored is not an error.
func (s *Store) Remove() error {
	return s.locked(func() error {
		if err := os.Remove(s.Path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("grant: removing the store: %w", err)
		}
		return nil
	})
}

// Check reports whether the stored file is readable by its owner alone. A store
// that is not there is not a finding.
func (s *Store) Check() (privfs.Report, bool) {
	if _, err := os.Stat(s.Path()); err != nil {
		return privfs.Report{}, false
	}
	return privfs.Check(s.Path()), true
}

// write puts g in the file whole: a temporary file in the same directory,
// flushed, then renamed over the target. Called with the lock held.
func (s *Store) write(g Grant) error {
	raw, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("grant: encoding the grant: %w", err)
	}
	tmp, err := privfs.CreateTemp(s.dir, ProviderOpenAI+"-*.tmp")
	if err != nil {
		return fmt.Errorf("grant: creating a temporary file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(raw, '\n'))
	serr := tmp.Sync()
	cerr := tmp.Close()
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			_ = os.Remove(name)
			return fmt.Errorf("grant: writing the store: %w", e)
		}
	}
	if err := os.Rename(name, s.Path()); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("grant: replacing the store: %w", err)
	}
	return nil
}

// locked runs fn holding the store's lock, creating the directory first.
func (s *Store) locked(fn func() error) error {
	if err := privfs.MkdirAll(s.dir); err != nil {
		return fmt.Errorf("grant: creating the store directory: %w", err)
	}
	deadline := time.Now().Add(s.lockWait)
	for {
		f, err := privfs.OpenExcl(s.lockPath())
		if err == nil {
			_ = f.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("grant: taking the store lock: %w", err)
		}
		if info, serr := os.Stat(s.lockPath()); serr == nil && time.Since(info.ModTime()) > s.lockStale {
			// A lock that old belongs to a process that died holding it.
			_ = os.Remove(s.lockPath())
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("grant: the store is locked by another process")
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() { _ = os.Remove(s.lockPath()) }()
	return fn()
}
