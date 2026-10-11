//go:build !unix

package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/futurelastic/muster/internal/privfs"
)

// checkStateDir answers the state.dir row where POSIX modes and access(2) do
// not exist. It reads the directory's access list (privfs.Check) and says what
// it finds: owner-only passes, a list that also grants someone else warns, and
// a list that could not be read says so rather than guessing — "unknown" is a
// real answer, and it is never reported as a pass.
//
// Whether this account can write the directory is not tested by a probe write
// (the doctor does not modify the state directory); an unwritable directory
// shows up as the service failing to start, not as a silent pass here.
func checkStateDir(dir string) doctorRow {
	row := doctorRow{ID: "state.dir"}
	if dir == "" {
		row.Status = statusWarn
		row.Summary = "FLEET_STATE_DIR unset — in-memory only"
		row.Detail = "idempotency keys and the event sequence are lost on every restart (D5)"
		return row
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && !info.IsDir():
		row.Status = statusFail
		row.Summary = "FLEET_STATE_DIR names something that is not a directory"
	case err == nil:
		rep := privfs.Check(dir)
		switch rep.Verdict {
		case privfs.OwnerOnly:
			row.Status = statusPass
			row.Summary = "FLEET_STATE_DIR is a directory only its owner can use"
		case privfs.Loose:
			row.Status = statusWarn
			row.Summary = "FLEET_STATE_DIR is accessible to more than its owner"
			row.Detail = rep.Detail + "; it holds idempotency keys naming working directories and records of what runs here"
		default:
			row.Status = statusUnknown
			row.Summary = "FLEET_STATE_DIR's access list could not be read"
			row.Detail = rep.Detail
		}
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(filepath.Clean(dir))
		for {
			pi, perr := os.Stat(parent)
			if perr == nil {
				if pi.IsDir() {
					row.Status = statusPass
					row.Summary = "FLEET_STATE_DIR does not exist yet; the service creates it, owner-only, on start"
				} else {
					row.Status = statusFail
					row.Summary = "FLEET_STATE_DIR does not exist and cannot be created: an ancestor is not a directory"
				}
				return row
			}
			next := filepath.Dir(parent)
			if next == parent {
				row.Status = statusFail
				row.Summary = "FLEET_STATE_DIR does not exist and has no existing ancestor"
				return row
			}
			parent = next
		}
	default:
		row.Status = statusFail
		row.Summary = "FLEET_STATE_DIR cannot be inspected by this user"
	}
	return row
}
