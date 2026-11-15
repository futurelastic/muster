//go:build !windows

package opencode

import (
	"path/filepath"
	"runtime"
)

// envKey is the identity here: environment variable names are case-sensitive.
func envKey(name string) string { return name }

// platformOwnedEnv is the set of variables only some platforms set themselves.
// Here: TMPPREFIX, which zsh writes here-document temp files under, and — on
// macOS only, where the toolchain shim reads it — DEVELOPER_DIR (muster #484).
var platformOwnedEnv = func() []string {
	names := []string{"TMPPREFIX"}
	if runtime.GOOS == "darwin" {
		names = append(names, developerDirEnv)
	}
	return names
}()

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
//
// TMPPREFIX is there because zsh puts a here-document's temp file under it, not
// under TMPDIR, and its default (/tmp/zsh) is outside every confined session's
// writes, so `cmd <<EOF` fails (muster #484). It is a prefix, not a directory:
// the file is named by appending random characters to it, so it lands in tmp.
func scopedEnv(dir string) map[string]string {
	return map[string]string{
		"HOME":            filepath.Join(dir, "home"),
		"TMPDIR":          filepath.Join(dir, "tmp"),
		"TMPPREFIX":       filepath.Join(dir, "tmp", "zsh"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "xdg", "config"),
		"XDG_DATA_HOME":   filepath.Join(dir, "xdg", "data"),
		"XDG_STATE_HOME":  filepath.Join(dir, "xdg", "state"),
		"XDG_CACHE_HOME":  filepath.Join(dir, "xdg", "cache"),
	}
}
