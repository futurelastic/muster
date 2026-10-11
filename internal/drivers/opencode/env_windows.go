//go:build windows

package opencode

import (
	"os"
	"path/filepath"
	"strings"
)

// envKey folds a variable name to upper case: Windows environment names are
// case-insensitive, and `os/exec` keeps only the last of two case-variants of
// one name. Every name this driver handles is folded first so a comparison is
// an exact one.
func envKey(name string) string { return strings.ToUpper(name) }

// platformOwnedEnv are the variables only Windows scopes: where a process looks
// for its profile, application data and temporary files.
var platformOwnedEnv = []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP"}

// systemRoot is the Windows directory, from the service's own environment when
// it is an absolute path and from the conventional location otherwise.
func systemRoot() string {
	if r := os.Getenv("SystemRoot"); filepath.IsAbs(r) {
		return filepath.Clean(r)
	}
	return `C:\Windows`
}

// systemPath is the base PATH: system directories only. Never a directory
// under the service user's profile, for the reason stated in env.go.
func systemPath() []string {
	root := systemRoot()
	return []string{
		filepath.Join(root, "System32"),
		root,
		filepath.Join(root, "System32", "Wbem"),
		filepath.Join(root, "System32", "WindowsPowerShell", "v1.0"),
	}
}

// sessionSubdirs are the directories created under a session's own directory.
func sessionSubdirs() []string {
	return []string{
		"home", "tmp",
		"xdg/config", "xdg/data", "xdg/state", "xdg/cache",
		"appdata/roaming", "appdata/local",
	}
}

// scopedEnv is the part of a session's environment that points inside its own
// directory dir, plus the few system variables a Windows process cannot start
// without. Those are copied from the service because they name system
// locations, never a user's.
func scopedEnv(dir string) map[string]string {
	home := filepath.Join(dir, "home")
	tmp := filepath.Join(dir, "tmp")
	m := map[string]string{
		"HOME":            home,
		"USERPROFILE":     home,
		"APPDATA":         filepath.Join(dir, "appdata", "roaming"),
		"LOCALAPPDATA":    filepath.Join(dir, "appdata", "local"),
		"TEMP":            tmp,
		"TMP":             tmp,
		"TMPDIR":          tmp,
		"XDG_CONFIG_HOME": filepath.Join(dir, "xdg", "config"),
		"XDG_DATA_HOME":   filepath.Join(dir, "xdg", "data"),
		"XDG_STATE_HOME":  filepath.Join(dir, "xdg", "state"),
		"XDG_CACHE_HOME":  filepath.Join(dir, "xdg", "cache"),
		"SYSTEMROOT":      systemRoot(),
		"WINDIR":          systemRoot(),
	}
	for _, name := range []string{"SYSTEMDRIVE", "COMSPEC"} {
		if v := os.Getenv(name); v != "" {
			m[name] = v
		}
	}
	m["PATHEXT"] = ".COM;.EXE;.BAT;.CMD"
	if v := os.Getenv("PATHEXT"); v != "" {
		m["PATHEXT"] = v
	}
	return m
}
