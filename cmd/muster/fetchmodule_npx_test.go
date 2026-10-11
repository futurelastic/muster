package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// muster #246: an optional delivery module installs through npx.
//
// `FLEET_MODULE_SOURCES` accepts `<name>=npx:<spec>`. The installer runs
// `npx --yes <spec> install-module --dir <stage>/<name>` with the caller's own
// credentials, then - because the launcher is somebody else's program - asks
// the staged module to answer `health` on the host before it is enabled.
//
// Nothing here reaches a registry or a git host: `npx` is a stand-in script on
// PATH that records how it was called and copies a prepared module into the
// directory it was given, which is the whole contract a real launcher keeps.

const npxHelloLine = `{"event":"hello","module":"fake","protocol":1,"version":"0","ops":["prepare-launch","attach","send","confirm","close","health"]}`

// A module that speaks just enough of the wire for the health probe.
func npxModuleScript(hello, healthReply string) string {
	return "#!/bin/sh\n" +
		"[ \"${1-}\" = serve ] || exit 2\n" +
		hello +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in\n" +
		"  *'\"op\":\"health\"'*) " + healthReply + " ;;\n" +
		"  esac\n" +
		"done\n"
}

func npxHello() string { return "printf '%s\\n' '" + npxHelloLine + "'\n" }

func npxReply(body string) string { return "printf '%s\\n' '" + body + "'" }

const (
	npxHealthy   = `{"id":"probe","ok":true,"result":{"ok":true,"module":"fake","protocol":1,"version":"0"}}`
	npxSickInner = `{"id":"probe","ok":true,"result":{"ok":false,"module":"fake"}}`
	npxSickOuter = `{"id":"probe","ok":false,"error":{"code":"broken","message":"no","retryable":false}}`
)

type npxShim struct {
	dir    string // goes on PATH
	module string // the module file the launcher "installs"
	log    string // one line per invocation
}

// npxMakeShim writes a stand-in npx. module is the script it installs.
func npxMakeShim(t *testing.T, module string) npxShim {
	t.Helper()
	sh := npxShim{dir: t.TempDir(), module: filepath.Join(t.TempDir(), "module-src"), log: filepath.Join(t.TempDir(), "npx.log")}
	fmWrite(t, sh.module, module)
	fmWrite(t, filepath.Join(sh.dir, "npx"), `#!/bin/sh
# stand-in for: npx --yes SPEC install-module --dir DIR
printf '%s|GOOS=%s|GOARCH=%s\n' "$*" "${GOOS-}" "${GOARCH-}" >> "$FAKE_NPX_LOG"
echo "launcher noise on stdout"
echo "launcher noise on stderr" >&2
[ "$1" = --yes ] && [ "$3" = install-module ] && [ "$4" = --dir ] || exit 64
case "$2" in *unreachable*) exit 1 ;; *installs-nothing*) exit 0 ;; esac
dir=$5
name=$(basename "$dir")
cp "$FAKE_NPX_MODULE" "$dir/$name" && chmod 0755 "$dir/$name"
`)
	if err := os.Chmod(filepath.Join(sh.dir, "npx"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sh
}

func (s npxShim) env(t *testing.T, extra ...string) (env []string, tmp string) {
	env, tmp = fmEnv(t, extra...)
	env = fmSetEnv(env,
		"PATH="+s.dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_NPX_LOG="+s.log,
		"FAKE_NPX_MODULE="+s.module)
	return env, tmp
}

func (s npxShim) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func npxRequireSh(t *testing.T) {
	t.Helper()
	skipOnWindows(t, "the npx source path is exercised through POSIX shell shims")
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not on PATH")
	}
}

// A test module installs through an npx source: the launcher is called with the
// documented arguments, OUT is the executable it left, and the script stays
// silent however loud the launcher was.
func TestFetchModule_NpxSourceInstalls(t *testing.T) {
	npxRequireSh(t)
	shim := npxMakeShim(t, npxModuleScript(npxHello(), npxReply(npxHealthy)))
	env, tmp := shim.env(t, "GOOS=testos", "GOARCH=testarch")
	outDir := t.TempDir()
	out := filepath.Join(outDir, "fake")

	code, so, se := fmFetch(t, env, t.TempDir(), "fake", "npx:@example/muster-fake@1.2.3", out)
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a silent success", code, so, se)
	}
	info, err := os.Stat(out)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("OUT: err=%v info=%v, want an executable regular file", err, info)
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != string(mustRead(t, shim.module)) {
		t.Errorf("OUT is not what the launcher installed (err=%v)", err)
	}
	calls := shim.calls(t)
	if len(calls) != 1 {
		t.Fatalf("npx called %d times, want once: %v", len(calls), calls)
	}
	if !strings.HasPrefix(calls[0], "--yes @example/muster-fake@1.2.3 install-module --dir ") ||
		!strings.Contains(calls[0], "/fake|GOOS=testos|GOARCH=testarch") {
		t.Errorf("npx call = %q, want --yes <spec> install-module --dir <stage>/fake with the target platform in the environment", calls[0])
	}
	fmNoDebris(t, "out dir", outDir, "fake")
	fmNoLeakedScratch(t, "npx", tmp)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A github: spec is just as good as a registry one.
func TestFetchModule_NpxSourceAcceptsAGithubSpec(t *testing.T) {
	npxRequireSh(t)
	shim := npxMakeShim(t, npxModuleScript(npxHello(), npxReply(npxHealthy)))
	env, _ := shim.env(t)
	out := filepath.Join(t.TempDir(), "fake")
	code, so, se := fmFetch(t, env, t.TempDir(), "fake", "npx:github:example-org/some-repo#v1.2.3", out)
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, so, se)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("not installed: %v", err)
	}
	if c := shim.calls(t); len(c) != 1 || !strings.HasPrefix(c[0], "--yes github:example-org/some-repo#v1.2.3 install-module ") {
		t.Errorf("npx calls = %v", c)
	}
}

// A fetch failure is a clean skip, however it fails: the launcher cannot reach
// its source, the launcher succeeds and leaves nothing, the spec is not a spec,
// the name is not a name, or npx itself is absent. Nothing is printed, nothing
// is created, and an existing OUT is never touched.
func TestFetchModule_NpxSourceFailuresSkipCleanly(t *testing.T) {
	npxRequireSh(t)
	for _, tc := range []struct {
		label, name, source string
		wantCalls           int
	}{
		{"unreachable source", "fake", "npx:@example/unreachable@1.0.0", 1},
		{"launcher leaves nothing", "fake", "npx:@example/installs-nothing@1.0.0", 1},
		{"empty spec", "fake", "npx:", 0},
		{"spec that looks like an option", "fake", "npx:--registry=x", 0},
		{"spec with a shell metacharacter", "fake", "npx:pkg;touch-x", 0},
		{"spec with whitespace", "fake", "npx:pkg other", 0},
		{"reserved name", "terminal", "npx:@example/pkg@1.0.0", 0},
	} {
		t.Run(tc.label, func(t *testing.T) {
			shim := npxMakeShim(t, npxModuleScript(npxHello(), npxReply(npxHealthy)))
			env, tmp := shim.env(t)
			outDir := t.TempDir()
			out := filepath.Join(outDir, "fake")
			code, so, se := fmFetch(t, env, t.TempDir(), tc.name, tc.source, out)
			fmSilentSkip(t, tc.label, code, so, se, out)
			if n := len(shim.calls(t)); n != tc.wantCalls {
				t.Errorf("npx called %d times, want %d", n, tc.wantCalls)
			}
			fmNoDebris(t, tc.label, outDir)
			fmNoLeakedScratch(t, tc.label, tmp)
		})
	}

	t.Run("existing OUT survives a failed fetch", func(t *testing.T) {
		shim := npxMakeShim(t, npxModuleScript(npxHello(), npxReply(npxHealthy)))
		env, _ := shim.env(t)
		outDir := t.TempDir()
		out := filepath.Join(outDir, "fake")
		fmWrite(t, out, "#!/bin/sh\n# earlier install\n")
		code, so, se := fmFetch(t, env, t.TempDir(), "fake", "npx:@example/unreachable@1.0.0", out)
		if code != 0 || so != "" || se != "" {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, so, se)
		}
		if got := string(mustRead(t, out)); got != "#!/bin/sh\n# earlier install\n" {
			t.Errorf("a failed fetch changed the existing module: %q", got)
		}
	})
}

// No npx on this machine is the same quiet skip. Needs a PATH that has the
// basic tools but no npx, so it is skipped where npx lives in them.
func TestFetchModule_NpxSourceWithoutNpxSkips(t *testing.T) {
	npxRequireSh(t)
	if p, err := exec.LookPath("npx"); err == nil && (strings.HasPrefix(p, "/usr/bin/") || strings.HasPrefix(p, "/bin/")) {
		t.Skip("npx lives in the system directories this test needs on PATH")
	}
	env, _ := fmEnv(t, "PATH=/usr/bin:/bin")
	outDir := t.TempDir()
	out := filepath.Join(outDir, "fake")
	code, so, se := fmFetch(t, env, t.TempDir(), "fake", "npx:@example/pkg@1.0.0", out)
	fmSilentSkip(t, "no npx", code, so, se, out)
	fmNoDebris(t, "no npx", outDir)
}

// The target platform is the caller's to set; when it does not, the launcher
// is left to settle it, so GOOS/GOARCH are not invented for an npx source.
func TestFetchModule_NpxSourceDoesNotInventAPlatform(t *testing.T) {
	npxRequireSh(t)
	shim := npxMakeShim(t, npxModuleScript(npxHello(), npxReply(npxHealthy)))
	env, _ := shim.env(t)
	code, so, se := fmFetch(t, env, t.TempDir(), "fake", "npx:@example/pkg@1.0.0", filepath.Join(t.TempDir(), "fake"))
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, so, se)
	}
	if c := shim.calls(t); len(c) != 1 || !strings.HasSuffix(c[0], "|GOOS=|GOARCH=") {
		t.Errorf("npx calls = %v, want GOOS and GOARCH unset", c)
	}
}

// ---- scripts/deploy.sh -----------------------------------------------------

// Through the deploy section a healthy npx module is installed beside the
// daemon, executable, with no staging debris, and the git-source path is still
// accepted in the same list.
func TestFetchModule_DeploySectionInstallsAnNpxModule(t *testing.T) {
	npxRequireSh(t)
	shim := npxMakeShim(t, npxModuleScript(npxHello(), npxReply(npxHealthy)))
	base := t.TempDir()
	modules := filepath.Join(base, "modules")
	env, tmp := shim.env(t,
		"FLEET_MODULE_SOURCES=fake=npx:@example/pkg@1.0.0",
		"FLEET_MODULES_DIR="+modules,
		"FLEET_MODULE_HEALTH_GRACE=1")

	code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(base, "bin", "muster"))
	if code != 0 || se != "" || !strings.Contains(so, "installed delivery module fake") || !strings.Contains(so, "HARNESS-DONE") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, so, se)
	}
	info, err := os.Stat(filepath.Join(modules, "fake"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("installed module: err=%v info=%v, want mode 0755", err, info)
	}
	calls := shim.calls(t)
	if len(calls) != 1 || !strings.Contains(calls[0], "|GOOS="+runtime.GOOS+"|GOARCH="+runtime.GOARCH) {
		t.Errorf("npx calls = %v, want one, with the target platform", calls)
	}
	fmNoDebris(t, "modules dir", modules, "fake")
	fmNoDebris(t, "TMPDIR", tmp)
}

// A module that fails its health check is not enabled: one warning, nothing
// installed, no staging debris, and the module an earlier deploy put there is
// untouched. The deploy itself does not fail. The four ways to be unhealthy:
// answers not-ok at either level, never says hello, or dies on start.
func TestFetchModule_DeploySectionUnhealthyNpxModuleIsNotEnabled(t *testing.T) {
	npxRequireSh(t)
	for _, tc := range []struct{ label, script string }{
		{"result not ok", npxModuleScript(npxHello(), npxReply(npxSickInner))},
		{"response not ok", npxModuleScript(npxHello(), npxReply(npxSickOuter))},
		{"no hello", npxModuleScript("", npxReply(npxHealthy))},
		{"never answers health", npxModuleScript(npxHello(), ":")},
		{"dies on start", "#!/bin/sh\nexit 1\n"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			shim := npxMakeShim(t, tc.script)
			base := t.TempDir()
			modules := filepath.Join(base, "modules")
			previous := "#!/bin/sh\n# installed by an earlier deploy\n"
			fmWrite(t, filepath.Join(modules, "kept"), previous)
			env, tmp := shim.env(t,
				"FLEET_MODULE_SOURCES=fake=npx:@example/pkg@1.0.0 kept=npx:@example/pkg@1.0.0",
				"FLEET_MODULES_DIR="+modules,
				"FLEET_MODULE_HEALTH_GRACE=1")

			code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(base, "bin", "muster"))
			if code != 0 || !strings.Contains(so, "HARNESS-DONE") {
				t.Fatalf("an unhealthy module stopped the deploy: exit=%d stdout=%q stderr=%q", code, so, se)
			}
			if strings.Contains(so, "installed delivery module") {
				t.Errorf("reported an install of an unhealthy module: %q", so)
			}
			for _, name := range []string{"fake", "kept"} {
				want := "WARNING delivery module " + name + " failed its health check"
				if n := strings.Count(se, want); n != 1 {
					t.Errorf("want exactly one %q warning, got %d:\n%s", want, n, se)
				}
			}
			if strings.Contains(se, "@example") {
				t.Errorf("a warning echoed the source: %q", se)
			}
			if _, err := os.Lstat(filepath.Join(modules, "fake")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("an unhealthy module was enabled (err=%v)", err)
			}
			if got := string(mustRead(t, filepath.Join(modules, "kept"))); got != previous {
				t.Errorf("the earlier module was replaced by an unhealthy one: %q", got)
			}
			fmNoDebris(t, "modules dir", modules, "kept")
			fmNoDebris(t, "TMPDIR", tmp)
		})
	}
}

// A fetch failure through the deploy section is the same total silence the git
// forms have: nothing printed, nothing installed, the deploy continues.
func TestFetchModule_DeploySectionFailedNpxFetchIsSilent(t *testing.T) {
	npxRequireSh(t)
	shim := npxMakeShim(t, npxModuleScript(npxHello(), npxReply(npxHealthy)))
	base := t.TempDir()
	modules := filepath.Join(base, "modules")
	env, tmp := shim.env(t,
		"FLEET_MODULE_SOURCES=fake=npx:@example/unreachable@1.0.0",
		"FLEET_MODULES_DIR="+modules,
		"FLEET_MODULE_HEALTH_GRACE=1")
	code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(base, "bin", "muster"))
	if code != 0 || so != "HARNESS-DONE\n" || se != "" {
		t.Errorf("exit=%d stdout=%q stderr=%q, want nothing but the harness marker", code, so, se)
	}
	if _, err := os.Lstat(filepath.Join(modules, "fake")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a module appeared from a failed fetch (err=%v)", err)
	}
	fmNoDebris(t, "TMPDIR", tmp)
}

// The health check runs on the HOST, not here: over ssh the staged file is
// copied first and probed where it landed.
func TestFetchModule_DeploySectionHealthCheckRunsOnTheHost(t *testing.T) {
	npxRequireSh(t)
	fakeHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	hostLog := filepath.Join(t.TempDir(), "host.log")
	fmWrite(t, filepath.Join(shims, "ssh"), "#!/bin/sh\nshift\necho \"ssh: $1\" >> \"$FAKE_HOST_LOG\"\nHOME=\"$FAKE_REMOTE_HOME\" exec sh -c \"$1\"\n")
	fmWrite(t, filepath.Join(shims, "scp"), `#!/bin/sh
dst=${3#*:}
case "$dst" in
'~/'*) dst="$FAKE_REMOTE_HOME/${dst#\~/}" ;;
esac
exec cp "$2" "$dst"
`)
	for _, n := range []string{"ssh", "scp"} {
		if err := os.Chmod(filepath.Join(shims, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		label, script string
		installed     bool
	}{
		{"healthy", npxModuleScript(npxHello(), npxReply(npxHealthy)), true},
		{"unhealthy", npxModuleScript(npxHello(), npxReply(npxSickOuter)), false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			shim := npxMakeShim(t, tc.script)
			env, _ := shim.env(t,
				"FLEET_MODULE_SOURCES=fake=npx:@example/pkg@1.0.0",
				"FLEET_MODULES_DIR=~/mods",
				"FLEET_MODULE_HEALTH_GRACE=1",
				"FAKE_REMOTE_HOME="+fakeHome, "FAKE_HOST_LOG="+hostLog)
			env = fmSetEnv(env, "PATH="+shims+string(os.PathListSeparator)+shim.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := os.Remove(hostLog); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(fakeHome, "mods")); err != nil {
				t.Fatal(err)
			}
			code, so, se := fmRunDeploySection(t, env, "peer", "~/prefix/bin/muster")
			if code != 0 || !strings.Contains(so, "HARNESS-DONE") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, so, se)
			}
			_, statErr := os.Stat(filepath.Join(fakeHome, "mods", "fake"))
			if tc.installed != (statErr == nil) {
				t.Errorf("installed = %v, want %v (stat err=%v); stderr=%q", statErr == nil, tc.installed, statErr, se)
			}
			if _, err := os.Lstat(filepath.Join(fakeHome, "mods", "fake.incoming")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a .incoming file was left on the host (err=%v)", err)
			}
			if !strings.Contains(string(mustRead(t, hostLog)), "fake.incoming") {
				t.Errorf("the probe never ran on the host against the uploaded file:\n%s", mustRead(t, hostLog))
			}
		})
	}
}
