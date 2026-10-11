//go:build unix

package main

import "os"

// isExecutableFile reports whether path names a file this process could run: a
// regular (non-directory) file with an execute bit set. The doctor's runtime
// rows ask it so that "executable" is judged in one place per platform (#312);
// the Windows answer is in execbit_windows.go.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
