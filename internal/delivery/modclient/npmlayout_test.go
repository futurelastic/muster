package modclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #414: under an npm install the daemon's own path is inside a platform
// package, so the default modules directory has to be recognised from that
// layout and answered as the install prefix's, the same as for a standalone
// binary at <prefix>/bin/muster.

// layoutExe creates dir/<elems...> as an empty executable, creating the
// directories, and returns its symlink-resolved path (t.TempDir is a link on
// macOS, and the daemon resolves its own path).
func layoutExe(t *testing.T, root string, elems ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{root}, elems...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDefaultModulesDir_NpmLayouts(t *testing.T) {
	root := resolvedTemp(t)
	modules := func(prefix string) string {
		return filepath.Join(prefix, "libexec", "muster", "modules")
	}

	cases := []struct {
		name string
		goos string
		exe  []string // under a per-case prefix
	}{
		{
			name: "global install on Unix: <prefix>/lib/node_modules",
			goos: "linux",
			exe:  []string{"lib", "node_modules", "@futurelastic", "muster-linux-x64", "bin", "muster"},
		},
		{
			name: "global install on macOS",
			goos: "darwin",
			exe:  []string{"lib", "node_modules", "@futurelastic", "muster-darwin-arm64", "bin", "muster"},
		},
		{
			name: "global install on Windows: <prefix>/node_modules",
			goos: "windows",
			exe:  []string{"node_modules", "@futurelastic", "muster-win32-x64", "bin", "muster.exe"},
		},
		{
			name: "local install: the project is the prefix",
			goos: "linux",
			exe:  []string{"node_modules", "@futurelastic", "muster-linux-x64", "bin", "muster"},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefix := filepath.Join(root, "case"+string(rune('a'+i)))
			exe := layoutExe(t, prefix, tc.exe...)
			if got, want := defaultModulesDir(exe, tc.goos), modules(prefix); got != want {
				t.Errorf("defaultModulesDir(%s, %s) = %s, want %s", exe, tc.goos, got, want)
			}
		})
	}

	// The `lib` step is npm's Unix layout and not a rule about the name: on
	// Windows a prefix that happens to be called lib is the prefix.
	t.Run("Windows prefix named lib", func(t *testing.T) {
		prefix := filepath.Join(root, "onwindows", "lib")
		exe := layoutExe(t, prefix, "node_modules", "@futurelastic", "muster-win32-x64", "bin", "muster.exe")
		if got, want := defaultModulesDir(exe, "windows"), modules(prefix); got != want {
			t.Errorf("defaultModulesDir(%s, windows) = %s, want %s", exe, got, want)
		}
	})

	// <prefix>/bin/muster is a link, as `npm install -g` makes it. Reached
	// through a link to the platform binary the answer is the same; this is the
	// resolution the daemon already did for a standalone binary.
	t.Run("through a link", func(t *testing.T) {
		prefix := filepath.Join(root, "linked")
		exe := layoutExe(t, prefix, "lib", "node_modules", "@futurelastic", "muster-linux-x64", "bin", "muster")
		link := filepath.Join(prefix, "bin", "muster")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, link); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
		if got, want := defaultModulesDir(link, "linux"), modules(prefix); got != want {
			t.Errorf("through a link: %s, want %s", got, want)
		}
	})
}

// The standalone rule is untouched by the npm one: a binary that is not in a
// platform package, wherever else it sits, still gets <parent of its dir>/libexec.
func TestDefaultModulesDir_NotNpmKeepsTheStandaloneRule(t *testing.T) {
	root := resolvedTemp(t)
	for name, elems := range map[string][]string{
		"plain":            {"prefix", "bin", "muster"},
		"another scope":    {"lib", "node_modules", "@someone", "muster-linux-x64", "bin", "muster"},
		"another package":  {"lib", "node_modules", "@futurelastic", "other", "bin", "muster"},
		"the launcher":     {"lib", "node_modules", "@futurelastic", "muster", "bin", "muster"},
		"not under bin":    {"lib", "node_modules", "@futurelastic", "muster-linux-x64", "sbin", "muster"},
		"not node_modules": {"lib", "modules", "@futurelastic", "muster-linux-x64", "bin", "muster"},
	} {
		t.Run(name, func(t *testing.T) {
			base := filepath.Join(root, strings.ReplaceAll(name, " ", "-"))
			exe := layoutExe(t, base, elems...)
			want := filepath.Join(filepath.Dir(filepath.Dir(exe)), "libexec", "muster", "modules")
			if got := defaultModulesDir(exe, "linux"); got != want {
				t.Errorf("defaultModulesDir(%s) = %s, want %s", exe, got, want)
			}
		})
	}
}

// npmScope and npmPlatformPrefix restate names that live in two JavaScript
// files. If either file renames its packages this fails, instead of the npm
// layout silently going unrecognised and the module being skipped again.
func TestNpmPackageNamesMatchThePackagingScripts(t *testing.T) {
	launcher, err := os.ReadFile(filepath.Join("..", "..", "..", "npm", "launcher", "bin", "muster.js"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "const SCOPE = '" + npmScope + "/" + npmPlatformPrefix + "';"; !strings.Contains(string(launcher), want) {
		t.Errorf("npm/launcher/bin/muster.js no longer contains %q; update npmScope/npmPlatformPrefix in discover.go", want)
	}
	assemble, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "assemble-npm.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const SCOPE = '" + npmScope + "';",
		"`${SCOPE}/" + npmPlatformPrefix + "${suffix}`",
		"`" + npmPlatformPrefix + "${suffix}`",
	} {
		if !strings.Contains(string(assemble), want) {
			t.Errorf("scripts/assemble-npm.mjs no longer contains %q; update npmScope/npmPlatformPrefix in discover.go", want)
		}
	}
}
