package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// The environment a session's process receives (muster #280).
//
// It is BUILT, not inherited. The previous shape — `append(os.Environ(), …)` —
// handed every session the service's whole environment (every credential the
// service user holds) and the service's working directory, which is why a
// create carrying `env` had to be refused: a shared process has no honest way
// to give one session a variable another does not see.
//
// What a session gets, and nothing else:
//
//   - PATH: system directories only (/usr/local/bin, /usr/bin, /bin, /usr/sbin,
//     /sbin, plus /opt/homebrew/bin on darwin), then any directories the
//     operator allow-listed with WithRuntimePath. Never a directory under the
//     service user's home: a shim there is not readable once a sandbox (#281)
//     confines the home, and the tool silently falls back to a different one.
//   - LANG, copied from the service's own environment only when it is set.
//   - HOME, TMPDIR, XDG_CONFIG_HOME, XDG_DATA_HOME, XDG_STATE_HOME and
//     XDG_CACHE_HOME, all inside a directory owned by this one session. opencode
//     otherwise reads a shared user config (permissions, providers) and keeps a
//     session database shared across sessions; scoped, closing a session
//     destroys what it knew.
//   - OPENCODE_DISABLE_CLAUDE_CODE=1: by default opencode loads the user's
//     Claude Code instructions and skills, which a session that asked for a
//     clean environment did not ask for.
//   - this machine's sessionEnv entries that apply (#94), then the create's own
//     `env`, which wins over a non-required entry.
//   - OPENCODE_SERVER_USERNAME and OPENCODE_SERVER_PASSWORD, last: the session's
//     own server credential. It reaches only this session's own process.
//
// What isolation cannot do, stated plainly: the model-provider key a session
// needs is in its environment, and opencode exposes that environment to its
// tool shell (`env` lists it). Isolation scopes every OTHER credential; it
// cannot protect the model key itself. Give each lane its own spend-capped key.
// Nor does it confine the filesystem — a session can still read what the
// service user can; that is the sandbox's job (#281).

// ownedEnvNames are the variables this driver sets itself. A caller or a
// machine's sessionEnv entry naming one would silently compete with the
// session scoping, so both are refused.
var ownedEnvNames = map[string]bool{
	"HOME":                     true,
	"TMPDIR":                   true,
	"XDG_CONFIG_HOME":          true,
	"XDG_DATA_HOME":            true,
	"XDG_STATE_HOME":           true,
	"XDG_CACHE_HOME":           true,
	"OPENCODE_SERVER_USERNAME": true,
	"OPENCODE_SERVER_PASSWORD": true,
	// muster #281: where a sandboxed session's package cache is.
	packageCacheEnv: true,
}

// checkOwnedEnv refuses an env map naming a variable this driver owns.
func checkOwnedEnv(env map[string]string) error {
	var hit []string
	for name := range env {
		if ownedEnvNames[name] {
			hit = append(hit, name)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	sort.Strings(hit)
	return fmt.Errorf("env: %s is set by this driver for every session (it scopes the session's "+
		"home, temp, XDG and server-credential variables), so it may not be supplied", strings.Join(hit, ", "))
}

// systemPath is the base PATH: system directories only.
func systemPath() []string {
	dirs := []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if runtime.GOOS == "darwin" {
		dirs = append([]string{"/opt/homebrew/bin"}, dirs...)
	}
	return dirs
}

// checkRuntimePath validates the operator's extra PATH directories: absolute,
// and not under the service user's home.
func checkRuntimePath(dirs []string) error {
	home, _ := os.UserHomeDir()
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("opencode: runtime path entry %q must be an absolute path", d)
		}
		if home != "" {
			clean := filepath.Clean(d)
			if clean == home || strings.HasPrefix(clean, home+string(filepath.Separator)) {
				return fmt.Errorf("opencode: runtime path entry %q is under the service user's home; "+
					"an interpreter there breaks once a sandbox confines the home — install it system-wide", d)
			}
		}
	}
	return nil
}

// checkSessionEnvEntries refuses a configured sessionEnv entry naming a
// variable this driver owns. Done once at construction, like the tmux
// driver's equivalent, so an operator reads it at start rather than every
// caller meeting it on every create.
func checkSessionEnvEntries(entries []sessionenv.Entry) error {
	for _, e := range entries {
		if ownedEnvNames[e.Name] {
			return fmt.Errorf("opencode: sessionEnv %q names a variable this driver sets for every session", e.Name)
		}
	}
	return nil
}

// sessionDirs creates the session-scoped directory tree under root.
func sessionDirs(root string) (dir string, err error) {
	dir, err = os.MkdirTemp(root, "muster-opencode-")
	if err != nil {
		return "", fmt.Errorf("opencode: creating the session directory: %w", err)
	}
	for _, sub := range []string{"home", "tmp", "xdg/config", "xdg/data", "xdg/state", "xdg/cache"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("opencode: creating the session directory: %w", err)
		}
	}
	return dir, nil
}

// buildEnv assembles the environment (as KEY=VALUE, sorted) for a session
// whose scoped directory is dir and whose caller/sessionEnv-merged variables
// are extra. It is the only place that decides it. The server credential is
// appended by buildServeCmd.
func (d *Driver) buildEnv(dir string, extra map[string]string) []string {
	path := append(append([]string(nil), d.runtimePath...), systemPath()...)
	m := map[string]string{
		"PATH":                         strings.Join(path, string(os.PathListSeparator)),
		"HOME":                         filepath.Join(dir, "home"),
		"TMPDIR":                       filepath.Join(dir, "tmp"),
		"XDG_CONFIG_HOME":              filepath.Join(dir, "xdg", "config"),
		"XDG_DATA_HOME":                filepath.Join(dir, "xdg", "data"),
		"XDG_STATE_HOME":               filepath.Join(dir, "xdg", "state"),
		"XDG_CACHE_HOME":               filepath.Join(dir, "xdg", "cache"),
		"OPENCODE_DISABLE_CLAUDE_CODE": "1",
	}
	if lang := os.Getenv("LANG"); lang != "" {
		m["LANG"] = lang
	}
	for k, v := range extra {
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// sessionEnvFor merges this machine's sessionEnv with the create's env and
// validates the result, mapping every refusal onto the right error kind.
func (d *Driver) sessionEnvFor(spec fleet.SessionSpec) (map[string]string, error) {
	invalid := func(err error) error {
		return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + strings.TrimPrefix(err.Error(), "create: "), Machine: d.machine}
	}
	if err := checkOwnedEnv(spec.Env); err != nil {
		return nil, invalid(err)
	}
	merged, err := sessionenv.Provision(d.sessionEnv, spec, d.machine, nil)
	if err != nil {
		if _, typed := err.(*fleet.Error); typed {
			return nil, err
		}
		return nil, invalid(err)
	}
	if err := sessionenv.CheckEnv(merged); err != nil {
		return nil, invalid(err)
	}
	return merged, nil
}
