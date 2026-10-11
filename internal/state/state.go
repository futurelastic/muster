// Package state persists the small amount a muster service must remember
// across a restart.
//
// # Why any of this exists
//
// The service was entirely in-memory, and four separate-looking problems were
// the same problem:
//
//   - §10's idempotency keys died on restart, so a federated create retried
//     across an upgrade produced two agents in one working directory — the
//     exact disaster §10 exists to prevent, on a driver that correctly
//     declares supportsResume.
//   - §7.3's epoch changed on every start, so every subscriber resynced after
//     every upgrade. For a service meant to stay up, restart is routine, and
//     that made it maximally disruptive.
//   - A driver's own sightings vanished, so §5.4's weaker corroboration path
//     had nothing to compare against until something listed first.
//   - §12's reconciliation was structurally incapable of its own job. It
//     enumerates and classifies into adopted / orphaned / vanished — but with
//     no persisted records, EVERYTHING is orphaned by definition. It looked
//     implemented and could not make the distinction it exists to make.
//
// # Why files, and why this shape
//
// Zero dependencies is a standing decision, which rules out an embedded
// database. What is actually needed is small, rarely written, and read once at
// startup, so a JSON file per concern is not a compromise — it is the right
// size of mechanism, and an operator can read it during an incident.
//
// Writes are atomic: a temp file in the same directory, fsynced, then renamed
// over the target. A half-written state file is worse than a missing one,
// because a missing one is obviously absent while a truncated one parses as
// authoritative for whatever it happens to contain.
//
// A corrupt or unreadable file is reported, never silently replaced with a
// fresh empty one. Losing idempotency keys quietly is how the §10 disaster
// arrives dressed as a clean start.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/futurelastic/muster/internal/privfs"
)

// Store is a directory of small JSON documents.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open prepares a state directory, creating it if needed.
//
// An empty dir disables persistence and returns nil, which every caller must
// handle — running without durable state is a legitimate configuration (a
// throwaway instance, a test) and should not require a scratch directory.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, nil
	}
	if err := privfs.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	// Owner-only (mode 0700; a protected list for the current user on Windows)
	// because this directory holds idempotency keys that name working
	// directories, and session records that describe what is running on this
	// machine. Not secrets, but not everyone's business either.
	return &Store{dir: dir}, nil
}

// Dir reports where this store writes, for logs and diagnostics.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Load reads a document into v. A missing file is not an error: it means this
// is the first run, which callers distinguish by v being left at its zero
// value. Returns false when nothing was loaded.
func (s *Store) Load(name string, v any) (bool, error) {
	if s == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("state: reading %s: %w", name, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		// Deliberately not recovered from. A state file that will not parse
		// is a fact an operator needs to see, and the tempting fallback —
		// start fresh — silently discards exactly what this package exists
		// to keep.
		return false, fmt.Errorf("state: %s is corrupt (not overwriting): %w", name, err)
	}
	return true, nil
}

// Save writes a document atomically.
func (s *Store) Save(name string, v any) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encoding %s: %w", name, err)
	}
	raw = append(raw, '\n')

	tmp, err := privfs.CreateTemp(s.dir, "."+name+".tmp*")
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("state: writing %s: %w", name, err)
	}
	// fsync before rename: rename is atomic with respect to readers, but
	// without the sync the rename can land while the contents have not,
	// leaving a file that is atomically empty.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("state: syncing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: closing %s: %w", name, err)
	}
	if err := os.Rename(tmpName, s.path(name)); err != nil {
		return fmt.Errorf("state: replacing %s: %w", name, err)
	}
	return nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".json") }
