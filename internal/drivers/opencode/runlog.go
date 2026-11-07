package opencode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/futurelastic/muster/internal/privfs"
)

// runtimeLogName is the file the runtime writes its own log to (v1.18.3). The
// name is only a convenience: the directory around it is what keeps the log
// private whatever a later version calls the file.
const runtimeLogName = "opencode.log"

// runtimeLogDir is where the runtime keeps its own log inside a session's
// directory: <XDG_DATA_HOME>/opencode/log. It is derived from the same
// environment the runtime is started with, so the two cannot disagree.
func runtimeLogDir(dir string) string {
	return filepath.Join(scopedEnv(dir)["XDG_DATA_HOME"], "opencode", "log")
}

// privateRuntimeLog makes the runtime's log owner-only before the runtime runs
// (muster #420): its directory is created 0700 and the file it will write is
// created 0600, so the log is never readable by another account, not even for
// the moment between the runtime creating it and anyone tightening it.
//
// What keeps a secret out of the log is the level the runtime is started with
// (runtimeLogLevel); this is the second layer, for whatever the level lets
// through. The runtime opens a log that already exists and keeps its mode, and
// it keeps a directory it finds — measured on v1.18.3 — so the modes set here
// are the ones it writes under. It runs on every start, a relaunch included:
// a session directory left by an earlier start holds a log written 0644, and
// that one is tightened in place without losing what it holds.
func privateRuntimeLog(dir string) error {
	logDir := runtimeLogDir(dir)
	if err := privfs.MkdirAll(logDir); err != nil {
		return fmt.Errorf("opencode: creating the runtime log directory: %w", err)
	}
	// MkdirAll leaves a directory that exists as it is; the runtime may have made
	// this one 0755 on an earlier start.
	if err := os.Chmod(logDir, 0o700); err != nil {
		return fmt.Errorf("opencode: restricting the runtime log directory: %w", err)
	}
	f, err := privfs.OpenExcl(filepath.Join(logDir, runtimeLogName))
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return fmt.Errorf("opencode: creating the runtime log: %w", err)
		}
	case errors.Is(err, fs.ErrExist):
	default:
		return fmt.Errorf("opencode: creating the runtime log: %w", err)
	}
	logs, err := filepath.Glob(filepath.Join(logDir, "*.log"))
	if err != nil {
		return fmt.Errorf("opencode: listing the runtime log directory: %w", err)
	}
	for _, p := range logs {
		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("opencode: restricting the runtime log: %w", err)
		}
	}
	return nil
}
