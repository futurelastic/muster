//go:build !darwin

package opencode

import "errors"

// volumeOfPath: only a host that clones (hostClones) asks which volume a path is
// on, and none but darwin does. `syscall.Stat_t.Dev` is unix-only, so the one
// reader lives in volume_darwin.go and this package still builds for Windows.
func volumeOfPath(string) (volume, error) {
	return volume{}, errors.New("no volume reader on this platform")
}
