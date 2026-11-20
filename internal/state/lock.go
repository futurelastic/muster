package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/futurelastic/muster/internal/privfs"
)

// One service per state directory (muster #507).
//
// Everything this package stores is written on the premise that one process owns
// the directory: the event cursor, the idempotency keys and the session records
// are read once at start and rewritten whole. A runtime driver goes further. The
// opencode driver's start-up sweep stops every runtime server it finds under the
// directory, on the premise that this process holds none, so every one it finds is
// a leftover of an earlier service. That is true after a crash and false when the
// earlier service is alive, and the sweep runs while the driver is being built,
// before the service has bound a listener — so a second start that was going to
// fail anyway (the address is taken), or was started by mistake, or by a
// supervisor while the old process was still shutting down, had already stopped
// the first one's servers by the time it was refused.
//
// So a service takes the directory before it reads or changes anything in it, and
// a second one stops there, naming the first. The lock is a byte range of a file
// in the directory that the operating system holds for as long as the process
// lives and drops when it ends, however it ends: nothing a crash leaves behind
// needs to be cleaned up, and a stale file proves nothing. The file's content is
// only the owner saying who it is, written after the lock is held, so that the
// refusal can say.

// lockName is the file the lock is held on. A document of the store is
// <name>.json; this has no such shape, so no document can collide with it.
const lockName = "muster.lock"

// recordAt is where the owner's record starts. Byte 0 is the byte Windows locks, and
// a byte-range lock there stops another handle reading it, so the record that a
// refused start has to read is kept clear of it.
const recordAt = 1

// Holder is what an owner records about itself in the lock file.
type Holder struct {
	Pid int `json:"pid"`
	// Started is when the owning process took the directory.
	Started time.Time `json:"started"`
	// Exe is the owner's executable, when the platform said.
	Exe string `json:"exe,omitempty"`
}

// HeldError is the refusal to take a state directory another process owns.
type HeldError struct {
	Dir string
	// Holder is who the owner recorded, nil when it recorded nothing readable: it
	// took the lock a moment ago and has not written yet, or something else wrote
	// the file.
	Holder *Holder
}

func (e *HeldError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "state directory %s is in use by a running muster service", e.Dir)
	if h := e.Holder; h != nil && h.Pid > 0 {
		fmt.Fprintf(&b, " (pid %d", h.Pid)
		if !h.Started.IsZero() {
			fmt.Fprintf(&b, ", up since %s", h.Started.UTC().Format(time.RFC3339))
		}
		if h.Exe != "" {
			fmt.Fprintf(&b, ", %s", h.Exe)
		}
		b.WriteString(")")
	}
	b.WriteString("; nothing in it was touched — stop that service first, or give this one its own FLEET_STATE_DIR")
	return b.String()
}

// errHeld is what a platform's lockFile answers when another holder has the lock.
var errHeld = errors.New("state: directory lock held")

// Lock is this process's ownership of a state directory. Hold it for the life of
// the service and keep it reachable: the lock lives as long as the open file under
// it, and the garbage collector closes a file nothing refers to any more.
type Lock struct {
	f   *os.File
	dir string
}

// Acquire takes dir for this process, creating it if needed (owner-only, as Open
// does), and fails with a *HeldError when another process owns it. An empty dir
// has nothing to own and returns a nil Lock, which Release accepts.
//
// It never waits: a service that cannot have its directory now is told so, and
// the supervisor that started it decides when to try again.
func Acquire(dir string) (*Lock, error) {
	if dir == "" {
		return nil, nil
	}
	if err := privfs.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	// 0600 where modes exist; where they do not, the file takes the directory's own
	// owner-only list, which is why the directory is made first.
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: opening the directory lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		holder := readHolder(f)
		_ = f.Close()
		if errors.Is(err, errHeld) {
			return nil, &HeldError{Dir: dir, Holder: holder}
		}
		return nil, fmt.Errorf("state: locking %s: %w", dir, err)
	}
	l := &Lock{f: f, dir: dir}
	// Failing to say who owns it does not mean this process does not: the lock is
	// held, and only the next refusal reads less. It is reported by the caller's
	// log line about the directory, not by refusing a start that is safe.
	_ = l.record()
	return l, nil
}

// record writes who holds the lock. The new content goes over the old and the file
// is cut to its length after, so a reader never finds it empty.
func (l *Lock) record() error {
	h := Holder{Pid: os.Getpid(), Started: time.Now().UTC().Truncate(time.Second)}
	if exe, err := os.Executable(); err == nil {
		h.Exe = exe
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := l.f.WriteAt(raw, recordAt); err != nil {
		return err
	}
	return l.f.Truncate(recordAt + int64(len(raw)))
}

// readHolder reads what the owner recorded, or nil when nothing readable is there.
// Only the first JSON value counts: a shorter record written over a longer one is
// cut to length a moment later, and the reader may arrive in between.
func readHolder(f *os.File) *Holder {
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, recordAt)
	var h Holder
	if err := json.NewDecoder(bytes.NewReader(buf[:n])).Decode(&h); err != nil {
		return nil
	}
	return &h
}

// Dir is the directory this lock covers.
func (l *Lock) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Release gives the directory up. A service that is stopping calls it last, once
// nothing is left to write; one that is killed needs no call — the operating
// system drops the lock with the process. Safe on a nil or released Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	// The file stays: removing it would let a second service create a new one and
	// lock that while the first still holds the old, which is two owners.
	uerr := unlockFile(f)
	cerr := f.Close()
	if uerr != nil {
		return uerr
	}
	return cerr
}
