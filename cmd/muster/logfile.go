package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/futurelastic/muster/internal/privfs"
)

// FLEET_LOG_FILE: an opt-in file the service's log is also written to.
//
// A service a scheduler starts has no terminal and no stderr anyone reads, so
// on Windows a failed start leaves nothing to look at. The variable is honoured
// on every OS, so one service log means one thing everywhere. It is off by
// default: with it unset nothing here runs and the log goes to stderr exactly as
// before.
//
// The file is created owner-only (privfs), because the log names working
// directories and principals, and it is opened for append, so a restart adds to
// it rather than replacing what the last run said. Rotation is the operator's.

// openLogFile opens path for appending, creating it — and its directory —
// owner-only when it is not there yet.
func openLogFile(path string) (*os.File, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := privfs.MkdirAll(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("creating the directory for FLEET_LOG_FILE: %w", err)
		}
		if err := privfs.WriteFile(path, nil); err != nil {
			return nil, fmt.Errorf("creating FLEET_LOG_FILE: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, fmt.Errorf("opening FLEET_LOG_FILE: %w", err)
	}
	return f, nil
}

// teeLog writes every line to the file first and to the original destination
// second. The second write is best effort: a console that has been freed, or a
// stderr that points nowhere, must not take the file's copy down with it, which
// io.MultiWriter would (it stops at the first error).
type teeLog struct {
	mu    sync.Mutex
	file  io.Writer
	other io.Writer
}

func (t *teeLog) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, err := t.file.Write(p)
	if t.other != nil {
		_, _ = t.other.Write(p)
	}
	return n, err
}

// newTeeLog is the writer the standard logger is switched to.
func newTeeLog(file, other io.Writer) io.Writer { return &teeLog{file: file, other: other} }
