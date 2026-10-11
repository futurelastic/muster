//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// W_OK for access(2). Spelled out because the syscall package does not export
// it on every platform this builds for.
const accessWriteOK = 0x2

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
		if syscall.Access(dir, accessWriteOK) != nil {
			row.Status = statusFail
			row.Summary = "FLEET_STATE_DIR is not writable by this user"
			return row
		}
		if info.Mode().Perm()&0o077 != 0 {
			row.Status = statusWarn
			row.Summary = fmt.Sprintf("FLEET_STATE_DIR is writable but mode %#o is looser than 0700", info.Mode().Perm())
			row.Detail = "it holds idempotency keys naming working directories and records of what runs here"
			return row
		}
		row.Status = statusPass
		row.Summary = "FLEET_STATE_DIR is a writable directory, mode 0700"
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(filepath.Clean(dir))
		for {
			pi, perr := os.Stat(parent)
			if perr == nil {
				if pi.IsDir() && syscall.Access(parent, accessWriteOK) == nil {
					row.Status = statusPass
					row.Summary = "FLEET_STATE_DIR does not exist yet; the service creates it (mode 0700) on start"
				} else {
					row.Status = statusFail
					row.Summary = "FLEET_STATE_DIR does not exist and cannot be created by this user"
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
