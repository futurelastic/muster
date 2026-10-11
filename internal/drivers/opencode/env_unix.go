//go:build !windows

package opencode

import (
	"path/filepath"
	"runtime"
)

// envKey is the identity here: environment variable names are case-sensitive.
func envKey(name string) string { return name }

// platformOwnedEnv is the set of variables only Windows sets itself.
var platformOwnedEnv []string

// systemPath is the base PATH: system directories only.
func systemPath() []string {
	dirs := []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if runtime.GOOS == "darwin" {
		dirs = append([]string{"/opt/homebrew/bin"}, dirs...)
	}
	return dirs
}

// sessionSubdirs are the directories created under a session's own directory.
func sessionSubdirs() []string {
	return []string{"home", "tmp", "xdg/config", "xdg/data", "xdg/state", "xdg/cache"}
}

// scopedEnv is the part of a session's environment that points inside its own
// directory dir.
func scopedEnv(dir string) map[string]string {
	return map[string]string{
		"HOME":            filepath.Join(dir, "home"),
		"TMPDIR":          filepath.Join(dir, "tmp"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "xdg", "config"),
		"XDG_DATA_HOME":   filepath.Join(dir, "xdg", "data"),
		"XDG_STATE_HOME":  filepath.Join(dir, "xdg", "state"),
		"XDG_CACHE_HOME":  filepath.Join(dir, "xdg", "cache"),
	}
}
