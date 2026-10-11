//go:build !unix

package main

// checkStateDir answers the state.dir row where POSIX modes and access(2) do
// not exist. Reading the directory's ACL is the platform layer's work (#303);
// until then the row says it did not check, which is honest, rather than
// reporting a mode Go makes up for a Windows file.
func checkStateDir(dir string) doctorRow {
	row := doctorRow{ID: "state.dir"}
	if dir == "" {
		row.Status = statusWarn
		row.Summary = "FLEET_STATE_DIR unset — in-memory only"
		row.Detail = "idempotency keys and the event sequence are lost on every restart (D5)"
		return row
	}
	row.Status = statusUnknown
	row.Summary = "FLEET_STATE_DIR is not checked on this platform yet"
	row.Detail = "who can write it and who can read it is not verified here; see #303"
	return row
}
