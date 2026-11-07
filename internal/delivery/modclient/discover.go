package modclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/futurelastic/muster/internal/delivery"
)

// ParseModuleList splits a comma-separated module list (the value of the
// operator's enable switch) into valid module names and invalid entries. Blank
// entries are dropped, entries are trimmed, and duplicates are removed keeping
// the FIRST position — order is preference, so a repeated name must not move.
// Validity is delivery.ValidModuleName, the same grammar the route field and the
// receipt use. Either result may be nil.
func ParseModuleList(s string) (valid []string, invalid []string) {
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if delivery.ValidModuleName(name) {
			valid = append(valid, name)
		} else {
			invalid = append(invalid, name)
		}
	}
	return valid, invalid
}

// DefaultModulesDir is where modules live when the operator does not say:
// <prefix>/libexec/muster/modules, where prefix is the parent of the
// directory holding the (symlink-resolved) executable — an installation under
// <prefix>/bin finds its helpers under <prefix>/libexec, the usual layout.
//
// The symlink is resolved on purpose. A service launched through a link that
// lives elsewhere must still find the directory the installer wrote next to the
// real binary; when it does not (a link farm), the operator sets the directory
// explicitly. If the path cannot be resolved (it does not exist yet) the
// lexical path is used. An empty exePath yields "".
//
// An npm install is the one layout where the binary does not sit under the
// prefix the operator chose (#414). `npm install -g --prefix <P>` puts a link
// <P>/bin/muster to a launcher, and the launcher runs the binary out of a
// platform package, so the process's own path is
// <root>/node_modules/@futurelastic/muster-<platform>/bin/muster and the rule
// above would yield a directory inside that package — one that never exists and
// that the next upgrade would replace. That layout is recognised by its path
// and the default becomes the install prefix's, the same as for a standalone
// binary: <P>/libexec/muster/modules. See npmInstallPrefix for what the prefix
// is and which layouts are not recognised.
func DefaultModulesDir(exePath string) string {
	return defaultModulesDir(exePath, runtime.GOOS)
}

// defaultModulesDir is DefaultModulesDir with the host OS as a parameter, so a
// test can drive both of npm's global layouts from one machine.
func defaultModulesDir(exePath, goos string) string {
	if exePath == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		resolved = filepath.Clean(exePath)
	}
	prefix, ok := npmInstallPrefix(resolved, goos)
	if !ok {
		prefix = filepath.Dir(filepath.Dir(resolved))
	}
	return filepath.Join(prefix, "libexec", "muster", "modules")
}

// npmScope and npmPlatformPrefix name the platform packages the npm launcher
// runs: @futurelastic/muster-<os>-<cpu>. They restate what
// npm/launcher/bin/muster.js and scripts/assemble-npm.mjs name, and a test
// (TestNpmPackageNamesMatchTheLauncher) fails if either moves.
const (
	npmScope          = "@futurelastic"
	npmPlatformPrefix = "muster-"
)

// npmInstallPrefix reports the install prefix of a binary that lives in an npm
// platform package: the directory whose bin/ holds the link to the launcher.
// resolved is the symlink-resolved path of the executable.
//
// The layout is npm's: node_modules is <prefix>/lib/node_modules for a global
// install on Unix and <prefix>/node_modules on Windows (goos picks which), and
// <project>/node_modules for a local install, whose "prefix" is the project.
// ok is false when resolved is not <root>/node_modules/@futurelastic/muster-*/bin/<file>.
//
// Not recognised, deliberately, because there is no prefix to name: a package
// manager that keeps packages in its own store (pnpm's .pnpm directory is the
// usual one) puts the binary where this reads a store entry, not a prefix. An
// operator there sets FLEET_MODULES_DIR, as a standalone binary reached through
// a link farm already must.
func npmInstallPrefix(resolved, goos string) (prefix string, ok bool) {
	binDir := filepath.Dir(resolved)
	pkgDir := filepath.Dir(binDir)
	scopeDir := filepath.Dir(pkgDir)
	nodeModules := filepath.Dir(scopeDir)
	if filepath.Base(binDir) != "bin" ||
		!strings.HasPrefix(filepath.Base(pkgDir), npmPlatformPrefix) ||
		filepath.Base(scopeDir) != npmScope ||
		filepath.Base(nodeModules) != "node_modules" {
		return "", false
	}
	root := filepath.Dir(nodeModules)
	if goos != "windows" && filepath.Base(root) == "lib" {
		return filepath.Dir(root), true
	}
	return root, true
}

// Found is a module executable that passed discovery.
type Found struct{ Name, Path string }

// Problem is a configured module that discovery skipped, with a reason meant
// for one log line. A skipped module is never an error for the daemon: the
// built-in path simply carries everything.
type Problem struct{ Name, Reason string }

// Discover looks for each name as an executable file directly inside dir.
//
// A name is a Problem when dir is empty or absent, or when dir/name is missing,
// is not a regular file (a symbolic link is NOT followed: the file we execute
// must be the file we checked, not whatever a link points at when we get
// around to it), is not executable by its owner, or is group- or world-
// writable (anyone who could rewrite it could run code as the daemon's user).
// A name containing a path separator or a dot-segment is refused before any
// file is touched. The directory's own permissions are not judged here.
//
// The order of names is preserved in both results.
func Discover(dir string, names []string) (found []Found, problems []Problem) {
	dirOK := true
	dirWhy := ""
	if dir == "" {
		dirOK, dirWhy = false, "modules directory absent"
	} else if st, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			dirOK, dirWhy = false, "modules directory absent"
		} else {
			dirOK, dirWhy = false, "modules directory unreadable"
		}
	} else if !st.IsDir() {
		dirOK, dirWhy = false, "modules directory is not a directory"
	}
	for _, name := range names {
		if !dirOK {
			problems = append(problems, Problem{Name: name, Reason: dirWhy})
			continue
		}
		if reason := checkExecutable(dir, name); reason != "" {
			problems = append(problems, Problem{Name: name, Reason: reason})
			continue
		}
		found = append(found, Found{Name: name, Path: filepath.Join(dir, name)})
	}
	return found, problems
}

// checkExecutable returns "" when dir/name is a usable module file, else why not.
func checkExecutable(dir, name string) string {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "invalid module name"
	}
	path := filepath.Join(dir, name)
	st, err := os.Lstat(path)
	switch {
	case err != nil && os.IsNotExist(err):
		return "no executable installed"
	case err != nil:
		return "not readable"
	case !st.Mode().IsRegular():
		return "not a regular file (symbolic links are not followed)"
	}
	return modeProblem(path, st.Mode())
}

// baseEnv are the only names taken from the daemon's own environment, plus the
// state directory. Everything else a module needs it must be told about
// explicitly through the forward list.
var baseEnv = []string{"PATH", "HOME", "USER", "LANG", "TMPDIR"}

// ChildEnv builds the COMPLETE environment for the module process: exactly
// PATH, HOME, USER, LANG and TMPDIR (each only when non-empty), then
// FLEET_STATE_DIR=stateDir (when non-empty), then each name in forward that is
// set, in the order given. Nothing is inherited implicitly, so a token in the
// daemon's environment never reaches a module unless an operator named it.
//
// Names beginning FLEET_ (any case) are NEVER forwarded — that namespace is the
// service's own, and a module must not be able to receive, say, the daemon's
// listen address or a peer credential by being listed — and names that are not
// valid variable names cannot be expressed in an environment block; both are
// returned in dropped (once each) so the caller can log them. A forwarded name
// that is unset (or empty: getenv cannot tell them apart) is simply absent and
// is not reported as dropped. Duplicates, and names already carried by the base
// set, appear once.
func ChildEnv(getenv func(string) string, stateDir string, forward []string) (env []string, dropped []string) {
	seen := map[string]bool{}
	add := func(name string) {
		if v := getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	for _, name := range baseEnv {
		seen[name] = true
		add(name)
	}
	if stateDir != "" {
		env = append(env, "FLEET_STATE_DIR="+stateDir)
	}
	droppedSeen := map[string]bool{}
	for _, raw := range forward {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(name), "FLEET_") || !validEnvName(name) {
			if !droppedSeen[name] {
				droppedSeen[name] = true
				dropped = append(dropped, name)
			}
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		add(name)
	}
	return env, dropped
}
