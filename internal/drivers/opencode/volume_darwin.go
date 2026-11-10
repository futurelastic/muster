//go:build darwin

package opencode

import (
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
)

// volumeOfPath reports the volume path is on, reading the nearest parent that
// exists when path itself does not yet (the session's private cache is made by
// the copy this answer comes before). A link is followed: it is the volume of
// what the path reaches that decides whether a clone can cross.
func volumeOfPath(path string) (volume, error) {
	p := path
	for {
		var st syscall.Stat_t
		err := syscall.Stat(p, &st)
		if err == nil {
			v := volume{id: uint64(st.Dev), mount: p}
			var sf syscall.Statfs_t
			if syscall.Statfs(p, &sf) == nil {
				b := make([]byte, 0, len(sf.Mntonname))
				for _, c := range sf.Mntonname {
					if c == 0 {
						break
					}
					b = append(b, byte(c))
				}
				if len(b) > 0 {
					v.mount = string(b)
				}
			}
			return v, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return volume{}, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return volume{}, err
		}
		p = parent
	}
}
