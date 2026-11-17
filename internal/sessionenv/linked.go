package sessionenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
)

// Linked is the set of session variables a CLIENT put here over the credentials
// routes, as opposed to the ones the operator declared in the config file (muster
// #498).
//
// # Why it exists, and why it is not a config rewrite
//
// An Entry in the config file is read at start and changing one needs a restart
// (config.go). A client that links an API key at runtime cannot wait for that, and
// rewriting a file the operator owns — losing their comments, racing their editor,
// with no reload to pick it up — would make the service an author of the operator's
// own configuration. So the two stay separate: this is a directory under the
// service's state, written by the service, and the driver reads it next to the
// declared entries.
//
// A linked variable is delivered by the very same path as a declared one. Entries
// returns ordinary Entry values whose FromFile points at the stored file, so
// Provision reads it fresh at every create and a rotation (a second Put) is in the
// next session with nothing to restart or reload, and CheckFile judges it the way
// it judges a declared one.
//
// # What it never does
//
//   - It never returns a value. Put takes one and Entries names a file; nothing here
//     reads the content back out for a caller.
//   - It never lets a declared entry and a linked one compete: the driver drops a
//     linked entry whose variable the operator declares, so the operator's file
//     wins (see the opencode driver's effective environment). That rule is the
//     driver's, not this store's, because only it knows the configured set.
//
// # Layout
//
//	<state>/linked/<runtime>/<VARIABLE>     owner-only file, the value and no newline
//
// A directory per runtime because a credential belongs to one runtime and must not
// reach another (the Scope.Runtimes axis, which Entries sets).
type Linked struct {
	dir string
}

// NewLinked returns the store for a service whose state directory is stateDir.
func NewLinked(stateDir string) *Linked {
	return &Linked{dir: filepath.Join(stateDir, "linked")}
}

// ErrLinkedInvalid is what Put and Remove answer for a name or runtime that cannot
// be a file name here. It carries no value.
var ErrLinkedInvalid = errors.New("sessionenv: not a usable runtime id or variable name")

func (l *Linked) runtimeDir(runtime fleet.RuntimeId) (string, error) {
	// A runtime id is a directory name here: letters, digits, '-', '_' and an inner
	// '.', so one with a separator or a dot-segment cannot walk out of the store.
	r := string(runtime)
	if r == "" || r == "." || r == ".." {
		return "", ErrLinkedInvalid
	}
	for _, c := range r {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return "", ErrLinkedInvalid
		}
	}
	return filepath.Join(l.dir, r), nil
}

func (l *Linked) file(runtime fleet.RuntimeId, name string) (string, error) {
	dir, err := l.runtimeDir(runtime)
	if err != nil || !ValidName(name) {
		return "", ErrLinkedInvalid
	}
	return filepath.Join(dir, name), nil
}

// Put stores value as the runtime's variable name, replacing what was there. The
// file is written whole beside the target and renamed over it, owner-only, so a
// create that reads it at that instant sees the old value or the new one and never
// half of one. The value is not stored with a newline. An error never holds it.
func (l *Linked) Put(runtime fleet.RuntimeId, name string, value []byte) error {
	path, err := l.file(runtime, name)
	if err != nil {
		return err
	}
	if err := privfs.MkdirAll(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sessionenv: creating the linked-key directory: %w", err)
	}
	tmp, err := privfs.CreateTemp(filepath.Dir(path), "."+name+"-*.tmp")
	if err != nil {
		return fmt.Errorf("sessionenv: creating a temporary file: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(value)
	serr := tmp.Sync()
	cerr := tmp.Close()
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			_ = os.Remove(tmpName)
			// e is an *fs.PathError for the temporary file, never the value.
			return fmt.Errorf("sessionenv: writing the linked key: %w", e)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("sessionenv: replacing the linked key: %w", err)
	}
	return nil
}

// Remove deletes the runtime's variable and reports whether there was one. Nothing
// there is not an error: unlinking is idempotent.
func (l *Linked) Remove(runtime fleet.RuntimeId, name string) (bool, error) {
	path, err := l.file(runtime, name)
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("sessionenv: removing the linked key: %w", err)
	}
	return true, nil
}

// Has reports whether the runtime has a linked value for name, whatever the file
// holds.
func (l *Linked) Has(runtime fleet.RuntimeId, name string) bool {
	path, err := l.file(runtime, name)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Entries is the runtime's linked variables as ordinary entries, in name order,
// read from the directory as it is now. A name that is not a valid variable, a
// directory and a leftover temporary file are skipped. Not required: a file that
// went missing or unusable leaves the session without the variable, as an optional
// declared entry does, and the credentials view says why.
func (l *Linked) Entries(runtime fleet.RuntimeId) []Entry {
	dir, err := l.runtimeDir(runtime)
	if err != nil {
		return nil
	}
	infos, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, info := range infos {
		name := info.Name()
		if info.IsDir() || !ValidName(name) {
			continue
		}
		out = append(out, Entry{
			Name:      name,
			FromFile:  filepath.Join(dir, name),
			AppliesTo: Scope{Runtimes: []string{string(runtime)}},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
