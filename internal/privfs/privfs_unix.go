//go:build !windows

package privfs

import (
	"fmt"
	"os"
)

// Label is how a user-facing message names what these functions create.
const Label = "0600"

// MkdirAll is os.MkdirAll with mode 0700.
func MkdirAll(path string) error { return os.MkdirAll(path, 0o700) }

// MkdirTemp is os.MkdirTemp, which creates the directory with mode 0700.
func MkdirTemp(dir, pattern string) (string, error) { return os.MkdirTemp(dir, pattern) }

// WriteFile is os.WriteFile with mode 0600.
func WriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

// OpenExcl creates path for writing and fails if it exists, with mode 0600.
func OpenExcl(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// CreateTemp is os.CreateTemp, which creates the file with mode 0600.
func CreateTemp(dir, pattern string) (*os.File, error) { return os.CreateTemp(dir, pattern) }

// Check reports whether path is accessible to its owner alone, from its mode.
func Check(path string) Report {
	info, err := os.Stat(path)
	if err != nil {
		return Report{Verdict: Unknown, Detail: err.Error()}
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Report{Verdict: Loose, Detail: fmt.Sprintf("mode %#o is looser than owner-only", perm)}
	}
	return Report{Verdict: OwnerOnly}
}
