package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
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

// ownedEnvNames are the variables this driver sets itself, keyed by envKey so
// that a lookup is case-insensitive where the platform's environment is (a
// variable is the same variable as "home" on Windows). A caller or a machine's
// sessionEnv entry naming one would silently compete with the session scoping,
// so both are refused. platformOwnedEnv adds the names only one platform sets.
var ownedEnvNames = func() map[string]bool {
	names := []string{
		"HOME",
		"TMPDIR",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_STATE_HOME",
		"XDG_CACHE_HOME",
		"OPENCODE_SERVER_USERNAME",
		"OPENCODE_SERVER_PASSWORD",
		// muster #281: where a sandboxed session's package cache is.
		packageCacheEnv,
	}
	m := make(map[string]bool, len(names)+len(platformOwnedEnv))
	for _, n := range append(names, platformOwnedEnv...) {
		m[envKey(n)] = true
	}
	return m
}()

// ownedName reports whether name is one this driver owns.
func ownedName(name string) bool { return ownedEnvNames[envKey(name)] }

// checkOwnedEnv refuses an env map naming a variable this driver owns.
func checkOwnedEnv(env map[string]string) error {
	var hit []string
	for name := range env {
		if ownedName(name) {
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

// normalizeEnvNames returns env with every name passed through key, refusing
// two names that become the same one.
//
// Where the platform's environment is case-insensitive, `os/exec` keeps only
// the last of two case-variants of a name, so a differently-cased copy of a
// name would silently shadow another — an owned variable included. Folding the
// names first makes every later comparison (owned names, a required sessionEnv
// entry, the merge itself) an exact one. With the identity key it changes
// nothing.
func normalizeEnvNames(env map[string]string, key func(string) string) (map[string]string, error) {
	if env == nil {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	from := make(map[string]string, len(env))
	var clash []string
	for name, v := range env {
		k := key(name)
		if prev, dup := from[k]; dup {
			a, b := prev, name
			if b < a {
				a, b = b, a
			}
			clash = append(clash, fmt.Sprintf("%s and %s", a, b))
			continue
		}
		from[k] = name
		out[k] = v
	}
	if len(clash) > 0 {
		sort.Strings(clash)
		return nil, fmt.Errorf("env: %s name the same variable on this platform; supply each once", strings.Join(clash, "; "))
	}
	return out, nil
}

// systemPath, sessionSubdirs, scopedEnv, envKey and platformOwnedEnv are
// per-platform: env_unix.go and env_windows.go.

// checkRuntimePath validates the operator's extra PATH directories: absolute,
// and not under the service user's home.
func checkRuntimePath(dirs []string) error {
	home, _ := os.UserHomeDir()
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("opencode: runtime path entry %q must be an absolute path", d)
		}
		if home != "" {
			clean, h := envKey(filepath.Clean(d)), envKey(filepath.Clean(home))
			if clean == h || strings.HasPrefix(clean, h+string(filepath.Separator)) {
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
		if ownedName(e.Name) {
			return fmt.Errorf("opencode: sessionEnv %q names a variable this driver sets for every session", e.Name)
		}
	}
	return nil
}

// foldEntryNames returns entries with each Name passed through envKey. The
// slice the caller supplied is not modified.
func foldEntryNames(entries []sessionenv.Entry) []sessionenv.Entry {
	if len(entries) == 0 {
		return entries
	}
	out := make([]sessionenv.Entry, len(entries))
	for i, e := range entries {
		e.Name = envKey(e.Name)
		out[i] = e
	}
	return out
}

// sessionDirs creates the session-scoped directory tree under root.
func sessionDirs(root string) (dir string, err error) {
	dir, err = privfs.MkdirTemp(root, "muster-opencode-")
	if err != nil {
		return "", fmt.Errorf("opencode: creating the session directory: %w", err)
	}
	for _, sub := range sessionSubdirs() {
		if err := privfs.MkdirAll(filepath.Join(dir, sub)); err != nil {
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
	m := scopedEnv(dir)
	m["PATH"] = strings.Join(path, string(os.PathListSeparator))
	m["OPENCODE_DISABLE_CLAUDE_CODE"] = "1"
	if lang := os.Getenv("LANG"); lang != "" {
		m["LANG"] = lang
	}
	for k, v := range extra {
		m[envKey(k)] = v
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
	// Names are folded first (a no-op where the environment is case-sensitive)
	// so that the owned-name check and a required sessionEnv entry compare
	// exactly, and two spellings of one name cannot reach the merge.
	folded, err := normalizeEnvNames(spec.Env, envKey)
	if err != nil {
		return nil, invalid(err)
	}
	spec.Env = folded
	if err := checkOwnedEnv(spec.Env); err != nil {
		return nil, invalid(err)
	}
	merged, err := sessionenv.Provision(d.sessionEnv, spec, d.machine, d.runtime, nil)
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
