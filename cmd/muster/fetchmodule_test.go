package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// muster #185: the optional-module installer.
//
// scripts/fetch-module.sh builds one optional delivery module from a source,
// with the caller's own access, or does nothing at all; scripts/deploy.sh has
// one section that drives it. What these tests pin is the contract that makes
// "a machine that cannot reach a module is simply built-in-only" true:
//
//   - the script always exits 0 and never prints, on success or on failure;
//   - a module is either fully there (an executable regular file, built to
//     completion) or absent - never partial - and a failed fetch never touches
//     a file that was already at the destination;
//   - nothing reaches the network: every test runs with GOPROXY=off (or a
//     proxy that is a directory on disk), so a source can only ever be
//     "unreachable".
//
// Everything sourced here is generic on purpose (a reserved .invalid host,
// temp directories): this repository is public.

// fmRepoRoot is the repository root; tests run in the package directory.
func fmRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// fmRequireTools skips unless a Go toolchain and a POSIX shell are on PATH.
func fmRequireTools(t *testing.T) {
	t.Helper()
	skipOnWindows(t, "scripts/fetch-module.sh and deploy.sh are POSIX shell scripts for Unix hosts")
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not on PATH")
	}
}

func fmGoEnvValue(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

// fmEnv is the child environment: minimal, offline by construction, and with a
// private TMPDIR so a test can see whether anything was left behind. It also
// returns that TMPDIR.
func fmEnv(t *testing.T, override ...string) (env []string, tmp string) {
	t.Helper()
	tmp = t.TempDir()
	env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + tmp,
		"GOCACHE=" + fmGoEnvValue(t, "GOCACHE"),
		"GOMODCACHE=" + fmGoEnvValue(t, "GOMODCACHE"),
		"GOPROXY=off",
		"GOTOOLCHAIN=local",
		"GOFLAGS=",
	}
	return fmSetEnv(env, override...), tmp
}

// fmSetEnv replaces (or adds) each KEY=VALUE in env.
func fmSetEnv(env []string, override ...string) []string {
	out := append([]string(nil), env...)
	for _, kv := range override {
		key := kv[:strings.Index(kv, "=")+1]
		replaced := false
		for i := range out {
			if strings.HasPrefix(out[i], key) {
				out[i] = kv
				replaced = true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

// fmExec runs cmd to completion and returns its exit code and both streams.
func fmExec(t *testing.T, cmd *exec.Cmd) (code int, stdout, stderr string) {
	t.Helper()
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("running %v: %v", cmd.Args, err)
	}
	return code, so.String(), se.String()
}

// fmFetch runs scripts/fetch-module.sh NAME SOURCE OUT in dir.
func fmFetch(t *testing.T, env []string, dir, name, source, out string) (code int, stdout, stderr string) {
	t.Helper()
	script := filepath.Join(fmRepoRoot(t), "scripts", "fetch-module.sh")
	cmd := exec.Command("sh", script, name, source, out)
	cmd.Env = env
	cmd.Dir = dir
	return fmExec(t, cmd)
}

// fmSilentSkip asserts the whole "skipped" outcome: exit 0, no output at all,
// and no OUT.
func fmSilentSkip(t *testing.T, label string, code int, stdout, stderr, out string) {
	t.Helper()
	if code != 0 {
		t.Errorf("%s: exit code = %d, want 0", label, code)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("%s: printed something; stdout=%q stderr=%q, want nothing", label, stdout, stderr)
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: OUT exists after a failed fetch (err=%v)", label, err)
	}
}

// fmNoDebris asserts dir holds exactly want (names), so a scratch directory
// or a partial file left beside OUT is a failure.
func fmNoDebris(t *testing.T, label, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: %s holds %v, want %v", label, dir, got, want)
	}
}

// fmNoLeakedScratch asserts the fetch's throwaway module directory is gone.
func fmNoLeakedScratch(t *testing.T, label, tmp string) {
	t.Helper()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fleet-module-fetch") {
			t.Errorf("%s: scratch %q left in TMPDIR", label, e.Name())
		}
	}
}

const fmMarker = "fetch-module-marker"

// fmTinyModule writes a Go module whose main prints fmMarker, and returns its
// directory.
func fmTinyModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fmWrite(t, filepath.Join(dir, "go.mod"), "module example.invalid/tiny\n\ngo 1.21\n")
	fmWrite(t, filepath.Join(dir, "main.go"), `package main

import "os"

func main() { os.Stdout.WriteString("`+fmMarker+`\n") }
`)
	return dir
}

func fmWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fmBrokenModule is a directory whose main package does not compile.
func fmBrokenModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fmWrite(t, filepath.Join(dir, "go.mod"), "module example.invalid/broken\n\ngo 1.21\n")
	fmWrite(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() { this does not compile }\n")
	return dir
}

// A source no one can reach: a well-formed package@version, with the proxy
// switched off, ends in a skip that says nothing.
func TestFetchModule_UnreachableSourceSilent(t *testing.T) {
	fmRequireTools(t)
	env, tmp := fmEnv(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "fake")

	code, so, se := fmFetch(t, env, t.TempDir(), "fake", "example.invalid/mod@v1.0.0", out)
	fmSilentSkip(t, "unreachable source", code, so, se, out)
	fmNoDebris(t, "unreachable source", outDir)
	fmNoLeakedScratch(t, "unreachable source", tmp)
}

func TestFetchModule_LocalDirectoryBuilds(t *testing.T) {
	fmRequireTools(t)
	env, tmp := fmEnv(t)
	src := fmTinyModule(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "fake")

	code, so, se := fmFetch(t, env, t.TempDir(), "fake", src, out)
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	info, err := os.Lstat(out)
	if err != nil {
		t.Fatalf("OUT missing after a successful fetch: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Errorf("OUT mode = %v, want a regular file with 0755", info.Mode())
	}
	got, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("the built module did not run: %v", err)
	}
	if strings.TrimSpace(string(got)) != fmMarker {
		t.Errorf("built module printed %q, want %q", got, fmMarker)
	}
	fmNoDebris(t, "success", outDir, "fake")
	fmNoLeakedScratch(t, "success", tmp)
}

// A relative OUT is resolved against the caller's working directory, not
// against wherever the build had to run.
func TestFetchModule_RelativeOutResolvesAgainstCallersDirectory(t *testing.T) {
	fmRequireTools(t)
	env, _ := fmEnv(t)
	src := fmTinyModule(t)
	cwd := t.TempDir()

	code, so, se := fmFetch(t, env, cwd, "fake", src, "fake-out")
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	if _, err := os.Stat(filepath.Join(cwd, "fake-out")); err != nil {
		t.Errorf("the module is not at the relative OUT: %v", err)
	}
	fmNoDebris(t, "relative OUT", cwd, "fake-out")
}

// The build is for the caller's GOOS/GOARCH, not the host's.
func TestFetchModule_HonoursCallerGOOSGOARCH(t *testing.T) {
	fmRequireTools(t)
	goarch := "arm64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		goarch = "amd64"
	}
	env, _ := fmEnv(t, "GOOS=linux", "GOARCH="+goarch)

	// No imports: keeps the cross-build to the runtime alone.
	src := t.TempDir()
	fmWrite(t, filepath.Join(src, "go.mod"), "module example.invalid/bare\n\ngo 1.21\n")
	fmWrite(t, filepath.Join(src, "main.go"), "package main\n\nfunc main() {}\n")
	out := filepath.Join(t.TempDir(), "fake")

	code, so, se := fmFetch(t, env, t.TempDir(), "fake", src, out)
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	head, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) < 4 || !bytes.Equal(head[:4], []byte("\x7fELF")) {
		t.Errorf("OUT does not start with an ELF header; GOOS=linux was not honoured")
	}
}

// Anything that is not one of the recognised shapes, and any name the service
// would not accept, is skipped without a word - even when what it names
// exists and would build.
func TestFetchModule_UnrecognisedFormSilent(t *testing.T) {
	fmRequireTools(t)
	src := fmTinyModule(t)

	// A directory that DOES build, but is only reachable by a relative path.
	cwd := t.TempDir()
	fmWrite(t, filepath.Join(cwd, "relative", "dir", "go.mod"), "module example.invalid/rel\n\ngo 1.21\n")
	fmWrite(t, filepath.Join(cwd, "relative", "dir", "main.go"), "package main\n\nfunc main() {}\n")

	longName := strings.Repeat("a", 33)
	cases := []struct{ label, name, source string }{
		{"relative directory", "fake", "relative/dir"},
		{"dot-relative directory", "fake", "./relative/dir"},
		{"url", "fake", "http://x"},
		{"url with userinfo", "fake", "https://user@example.invalid/mod"},
		{"empty source", "fake", ""},
		{"package with no version", "fake", "example.invalid/mod"},
		{"empty version", "fake", "example.invalid/mod@"},
		{"empty package", "fake", "@v1.0.0"},
		{"first element without a dot", "fake", "mod/cmd/x@v1.0.0"},
		{"leading dash", "fake", "-x.invalid/mod@v1.0.0"},
		{"second at-sign", "fake", "example.invalid/mod@v1@v2"},
		{"whitespace in source", "fake", "example.invalid/mod @v1.0.0"},
		{"shell metacharacter in source", "fake", "example.invalid/mod@v1.0.0;true"},
		{"absolute path that is not a directory", "fake", filepath.Join(src, "main.go")},
		{"absolute path that does not exist", "fake", filepath.Join(src, "missing")},
		{"empty name", "", src},
		{"uppercase name", "Fake", src},
		{"leading digit", "1fake", src},
		{"leading dash name", "-fake", src},
		{"underscore in name", "fa_ke", src},
		{"space in name", "fa ke", src},
		{"newline in name", "fake\nx", src},
		{"name too long", longName, src},
		{"reserved: auto", "auto", src},
		{"reserved: terminal", "terminal", src},
		{"reserved: inbox", "inbox", src},
		{"reserved: module", "module", src},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			env, tmp := fmEnv(t)
			outDir := t.TempDir()
			out := filepath.Join(outDir, "out")
			code, so, se := fmFetch(t, env, cwd, tc.name, tc.source, out)
			fmSilentSkip(t, tc.label, code, so, se, out)
			fmNoDebris(t, tc.label, outDir)
			fmNoLeakedScratch(t, tc.label, tmp)
		})
	}

	// The same source with a valid name proves the rows above were skipped for
	// the reason on their label, not because the fixture cannot build.
	env, _ := fmEnv(t)
	out := filepath.Join(t.TempDir(), "ok")
	if code, so, se := fmFetch(t, env, cwd, "ok", src, out); code != 0 || so != "" || se != "" {
		t.Fatalf("control: exit=%d stdout=%q stderr=%q", code, so, se)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("control: the fixture does not build: %v", err)
	}

	// Refused for OUT itself: a newline in the path, and a directory.
	env, _ = fmEnv(t)
	outDir := t.TempDir()
	code, so, se := fmFetch(t, env, cwd, "fake", src, filepath.Join(outDir, "a\nb"))
	fmSilentSkip(t, "newline in OUT", code, so, se, filepath.Join(outDir, "a\nb"))
	code, so, se = fmFetch(t, env, cwd, "fake", src, outDir)
	if code != 0 || so != "" || se != "" {
		t.Errorf("OUT is a directory: exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	fmNoDebris(t, "OUT is a directory", outDir)
	code, so, se = fmFetch(t, env, cwd, "fake", src, filepath.Join(outDir, "missing", "out"))
	fmSilentSkip(t, "OUT in a directory that does not exist", code, so, se, filepath.Join(outDir, "missing", "out"))
}

// A source that fetches but does not build to a main-package binary leaves
// nothing at OUT and nothing beside it.
func TestFetchModule_FailedBuildLeavesNoPartialOut(t *testing.T) {
	fmRequireTools(t)

	library := t.TempDir()
	fmWrite(t, filepath.Join(library, "go.mod"), "module example.invalid/lib\n\ngo 1.21\n")
	fmWrite(t, filepath.Join(library, "lib.go"), "package lib\n\nfunc F() {}\n")

	noGo := t.TempDir()

	cases := []struct{ label, source string }{
		{"compile error", fmBrokenModule(t)},
		{"library, not a main package", library},
		{"directory with no Go code", noGo},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			env, tmp := fmEnv(t)
			outDir := t.TempDir()
			out := filepath.Join(outDir, "fake")
			code, so, se := fmFetch(t, env, t.TempDir(), "fake", tc.source, out)
			fmSilentSkip(t, tc.label, code, so, se, out)
			fmNoDebris(t, tc.label, outDir)
			fmNoLeakedScratch(t, tc.label, tmp)
		})
	}
}

// A failed fetch is not permitted to destroy what an earlier fetch installed:
// whatever is at OUT stays byte-identical and keeps its mode, however the
// fetch failed.
func TestFetchModule_NeverOverwritesExistingOutOnFailure(t *testing.T) {
	fmRequireTools(t)
	previous := []byte("#!/bin/sh\n# a previously installed module\n")

	failures := []struct{ label, name, source string }{
		{"unreachable package", "fake", "example.invalid/mod@v1.0.0"},
		{"compile error", "fake", fmBrokenModule(t)},
		{"missing directory", "fake", "/nonexistent-fetch-module-source"},
		{"unrecognised form", "fake", "relative/dir"},
		{"reserved name", "auto", fmTinyModule(t)},
	}
	for _, tc := range failures {
		t.Run(tc.label, func(t *testing.T) {
			env, _ := fmEnv(t)
			outDir := t.TempDir()
			out := filepath.Join(outDir, "fake")
			if err := os.WriteFile(out, previous, 0o755); err != nil {
				t.Fatal(err)
			}
			code, so, se := fmFetch(t, env, t.TempDir(), tc.name, tc.source, out)
			if code != 0 || so != "" || se != "" {
				t.Errorf("exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("the existing OUT is gone after a failed fetch: %v", err)
			}
			if !bytes.Equal(got, previous) {
				t.Errorf("the existing OUT was modified: %q", got)
			}
			if info, _ := os.Stat(out); info.Mode().Perm() != 0o755 {
				t.Errorf("the existing OUT's mode changed to %v", info.Mode().Perm())
			}
			fmNoDebris(t, tc.label, outDir, "fake")
		})
	}
}

// The other half: a fetch that succeeds replaces what was there, whole.
func TestFetchModule_SuccessReplacesExistingOut(t *testing.T) {
	fmRequireTools(t)
	env, _ := fmEnv(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "fake")
	if err := os.WriteFile(out, []byte("an older build\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, so, se := fmFetch(t, env, t.TempDir(), "fake", fmTinyModule(t), out)
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	got, err := exec.Command(out).Output()
	if err != nil || strings.TrimSpace(string(got)) != fmMarker {
		t.Errorf("OUT was not replaced by the new build: out=%q err=%v", got, err)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o755 {
		t.Errorf("replaced OUT mode = %v, want 0755", info.Mode().Perm())
	}
	fmNoDebris(t, "replace", outDir, "fake")
}

// fmWriteProxy lays out a module proxy on disk (the GOPROXY=file:// layout)
// serving example.invalid/mod v1.0.0, which holds one main package at
// cmd/hello.
func fmWriteProxy(t *testing.T, root string) {
	t.Helper()
	const (
		mod = "example.invalid/mod"
		ver = "v1.0.0"
	)
	dir := filepath.Join(root, "example.invalid", "mod", "@v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	goMod := "module " + mod + "\n\ngo 1.21\n"
	fmWrite(t, filepath.Join(dir, ver+".mod"), goMod)
	fmWrite(t, filepath.Join(dir, ver+".info"), `{"Version":"`+ver+`","Time":"2020-01-01T00:00:00Z"}`)
	fmWrite(t, filepath.Join(dir, "list"), ver+"\n")

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{
		"go.mod":            goMod,
		"cmd/hello/main.go": "package main\n\nimport \"os\"\n\nfunc main() { os.Stdout.WriteString(\"" + fmMarker + "\\n\") }\n",
	} {
		w, err := zw.Create(mod + "@" + ver + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ver+".zip"), zb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The package@version form, end to end, against a proxy that is a directory:
// the one place a real `go get` of a versioned package is exercised without
// the network. The module cache is private (and writable, so it can be
// removed) so the run leaves nothing in the developer's own.
func TestFetchModule_PackageAtVersionBuildsFromLocalProxy(t *testing.T) {
	fmRequireTools(t)
	proxy := t.TempDir()
	fmWriteProxy(t, proxy)
	env, tmp := fmEnv(t,
		"GOPROXY=file://"+filepath.ToSlash(proxy),
		"GOSUMDB=off",
		"GOMODCACHE="+filepath.Join(t.TempDir(), "modcache"),
		"GOFLAGS=-modcacherw",
	)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "hello")

	code, so, se := fmFetch(t, env, t.TempDir(), "hello", "example.invalid/mod/cmd/hello@v1.0.0", out)
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	got, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("the built module did not run: %v", err)
	}
	if strings.TrimSpace(string(got)) != fmMarker {
		t.Errorf("built module printed %q, want %q", got, fmMarker)
	}
	fmNoDebris(t, "proxy build", outDir, "hello")
	fmNoLeakedScratch(t, "proxy build", tmp)

	// The same proxy, a version it does not have: skipped, and the module that
	// was already installed survives.
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	code, so, se = fmFetch(t, env, t.TempDir(), "hello", "example.invalid/mod/cmd/hello@v9.9.9", out)
	if code != 0 || so != "" || se != "" {
		t.Errorf("unknown version: exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	after, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("an unknown version disturbed the installed module (err=%v)", err)
	}
	fmNoDebris(t, "unknown version", outDir, "hello")
}

// With a `timeout` on PATH the script runs itself under it, bounded to 300
// seconds; whatever `timeout` reports - including its own 124 - the script
// still exits 0 and says nothing. A stand-in that just runs the command
// exercises this on machines that have no real one.
func TestFetchModule_RunsUnderTimeoutWhenAvailable(t *testing.T) {
	fmRequireTools(t)
	src := fmTinyModule(t)

	shimDir := t.TempDir()
	log := filepath.Join(t.TempDir(), "timeout.log")
	shim := "#!/bin/sh\necho \"$1\" >> '" + log + "'\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "timeout"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	env, tmp := fmEnv(t)
	env = fmSetEnv(env, "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	outDir := t.TempDir()
	out := filepath.Join(outDir, "fake")
	code, so, se := fmFetch(t, env, t.TempDir(), "fake", src, out)
	if code != 0 || so != "" || se != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a silent 0", code, so, se)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("no module built under the timeout wrapper: %v", err)
	}
	if got, _ := os.ReadFile(log); strings.TrimSpace(string(got)) != "300" {
		t.Errorf("timeout was invoked with %q, want a 300 second bound", got)
	}
	fmNoDebris(t, "timeout wrapper", outDir, "fake")
	fmNoLeakedScratch(t, "timeout wrapper", tmp)

	// A wrapper that reports a timeout (124) without the build having run.
	if err := os.WriteFile(filepath.Join(shimDir, "timeout"), []byte("#!/bin/sh\nexit 124\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outDir = t.TempDir()
	out = filepath.Join(outDir, "fake")
	code, so, se = fmFetch(t, env, t.TempDir(), "fake", src, out)
	fmSilentSkip(t, "timed out", code, so, se, out)
	fmNoDebris(t, "timed out", outDir)
}

// A machine with no Go toolchain at all is the plainest "cannot fetch" there
// is: still exit 0, still nothing said.
func TestFetchModule_NoToolchainSilent(t *testing.T) {
	fmRequireTools(t)
	env, _ := fmEnv(t, "PATH="+t.TempDir())
	outDir := t.TempDir()
	out := filepath.Join(outDir, "fake")

	code, so, se := fmFetch(t, env, t.TempDir(), "fake", fmTinyModule(t), out)
	fmSilentSkip(t, "no toolchain", code, so, se, out)
	fmNoDebris(t, "no toolchain", outDir)
}

// ---- scripts/deploy.sh -----------------------------------------------------

// Neither script is run for real here; this only keeps both parseable, which
// is the one failure a deploy would otherwise meet on a live machine.
func TestDeploy_ScriptParses(t *testing.T) {
	skipOnWindows(t, "deploy.sh and fetch-module.sh are POSIX shell scripts for Unix hosts")
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not on PATH")
	}
	for _, name := range []string{"deploy.sh", "fetch-module.sh"} {
		script := filepath.Join(fmRepoRoot(t), "scripts", name)
		code, so, se := fmExec(t, exec.Command("sh", "-n", script))
		if code != 0 {
			t.Errorf("sh -n scripts/%s: exit %d\n%s%s", name, code, so, se)
		}
	}
}

const (
	deploySectionBegin = "# >>> optional delivery modules (#185) >>>"
	deploySectionEnd   = "# <<< optional delivery modules (#185) <<<"
	deploySeamBegin    = "# --- local vs. remote, behind one seam"
	deploySeamEnd      = "# --- refuse to ship something that cannot be identified"
)

// fmDeployText returns the text of deploy.sh from begin up to (not including)
// end, failing loudly if the script no longer carries the markers.
func fmDeployText(t *testing.T, begin, end string, includeEnd bool) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(fmRepoRoot(t), "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	i := strings.Index(text, begin)
	j := strings.Index(text, end)
	if i < 0 || j < i {
		t.Fatalf("scripts/deploy.sh no longer carries the markers %q ... %q that this test extracts by", begin, end)
	}
	if includeEnd {
		j += len(end)
	}
	return text[i:j]
}

// fmRunDeploySection runs ONLY the optional-module section of deploy.sh
// against temp directories: the section is cut out of the script, together
// with the script's own local-vs-remote seam (run/put), and run under a
// stand-in for the rest (the target platform, the paths). Nothing here builds
// the daemon, restarts anything, or reaches another machine. host is "local"
// or a name that PATH's stand-in ssh/scp answer to.
func fmRunDeploySection(t *testing.T, env []string, host, remotePath string) (code int, stdout, stderr string) {
	t.Helper()
	harness := "set -eu\n" +
		"HOST='" + host + "'\n" +
		"REMOTE_PATH='" + remotePath + "'\n" +
		"GOOS='" + runtime.GOOS + "'\n" +
		"GOARCH='" + runtime.GOARCH + "'\n" +
		"TMPBIN=\n" +
		fmDeployText(t, deploySeamBegin, deploySeamEnd, false) + "\n" +
		fmDeployText(t, deploySectionBegin, deploySectionEnd, true) + "\n" +
		"echo HARNESS-DONE\n"
	path := filepath.Join(t.TempDir(), "harness.sh")
	fmWrite(t, path, harness)
	cmd := exec.Command("sh", path)
	cmd.Env = env
	cmd.Dir = fmRepoRoot(t)
	return fmExec(t, cmd)
}

// A deploy that names no modules is unchanged: the section neither prints nor
// creates anything.
func TestFetchModule_DeploySectionInactiveWithoutSources(t *testing.T) {
	fmRequireTools(t)
	base := t.TempDir()
	binDir := filepath.Join(base, "prefix", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, set := range [][]string{nil, {"FLEET_MODULE_SOURCES="}} {
		env, tmp := fmEnv(t, set...)
		code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(binDir, "muster"))
		if code != 0 || so != "HARNESS-DONE\n" || se != "" {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q, want nothing but the harness marker", set, code, so, se)
		}
		fmNoDebris(t, "inactive", tmp)
	}
	if _, err := os.Stat(filepath.Join(base, "prefix", "libexec")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the modules directory was created with nothing to install (err=%v)", err)
	}
}

// A malformed entry gets exactly one warning line and the rest still run. The
// warning names the entry's position (and its name only when the name side is
// itself a valid name), and never anything from the source side.
func TestFetchModule_DeploySectionMalformedEntryWarnsOnce(t *testing.T) {
	fmRequireTools(t)
	base := t.TempDir()
	modules := filepath.Join(base, "modules")
	src := fmTinyModule(t)

	entries := []string{
		"credential-in-shape-zzz", // 1: no '='
		"=source-zzz",             // 2: empty name
		"Bad_Name=source-zzz",     // 3: invalid name
		"good=",                   // 4: valid name, empty source
		"auto=source-zzz",         // 5: reserved name
		"fine=" + src,             // 6: well-formed - must still be installed
	}
	env, tmp := fmEnv(t,
		"FLEET_MODULE_SOURCES="+strings.Join(entries, " "),
		"FLEET_MODULES_DIR="+modules)
	code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(base, "bin", "muster"))
	if code != 0 || !strings.Contains(so, "HARNESS-DONE") {
		t.Fatalf("a malformed entry stopped the deploy: exit=%d stdout=%q stderr=%q", code, so, se)
	}

	lines := strings.Split(strings.TrimRight(se, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("stderr has %d lines, want exactly one warning per malformed entry (5):\n%s", len(lines), se)
	}
	for i, line := range lines {
		want := "module entry " + string(rune('1'+i)) + " "
		if !strings.Contains(line, "WARNING") || !strings.Contains(line, want) {
			t.Errorf("line %d = %q, want a WARNING naming %q", i+1, line, want)
		}
	}
	if !strings.Contains(lines[3], "(good)") {
		t.Errorf("entry 4 has a valid name and should be named: %q", lines[3])
	}
	if strings.Contains(se, "zzz") || strings.Contains(so, "zzz") {
		t.Errorf("a warning echoed part of an entry that was not a valid name:\nstdout=%q\nstderr=%q", so, se)
	}

	// The well-formed entry was installed, executable, and runs.
	installed := filepath.Join(modules, "fine")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("the well-formed entry was not installed: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("installed module mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(so, "installed delivery module fine") {
		t.Errorf("no report of the install on stdout: %q", so)
	}
	fmNoDebris(t, "modules dir", modules, "fine")
	entriesLeft, _ := os.ReadDir(tmp)
	for _, e := range entriesLeft {
		if strings.HasPrefix(e.Name(), "muster-modules") {
			t.Errorf("staging directory %q left behind", e.Name())
		}
	}
}

// With FLEET_MODULES_DIR unset the module lands where the daemon looks by
// default: <parent of the binary's directory>/libexec/muster/modules.
func TestFetchModule_DeploySectionDefaultsToTheDaemonsDirectory(t *testing.T) {
	fmRequireTools(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(base, "prefix", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env, _ := fmEnv(t, "FLEET_MODULE_SOURCES=fake="+fmTinyModule(t))

	code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(binDir, "muster"))
	if code != 0 || se != "" || !strings.Contains(so, "HARNESS-DONE") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, so, se)
	}
	installed := filepath.Join(base, "prefix", "libexec", "muster", "modules", "fake")
	got, err := exec.Command(installed).Output()
	if err != nil || strings.TrimSpace(string(got)) != fmMarker {
		t.Errorf("no working module at %s: out=%q err=%v", installed, got, err)
	}
	modulesDir := filepath.Dir(installed)
	if info, err := os.Stat(modulesDir); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("modules directory %s: err=%v info=%v, want mode 0755", modulesDir, err, info)
	}
	fmNoDebris(t, "default directory", modulesDir, "fake")
}

// The skip is total: a source that cannot be fetched installs nothing, prints
// nothing, does not fail the deploy, and does not remove the module an earlier
// deploy put there.
func TestFetchModule_DeploySectionFailedFetchInstallsNothingAndKeepsWhatWasThere(t *testing.T) {
	fmRequireTools(t)
	base := t.TempDir()
	modules := filepath.Join(base, "modules")
	if err := os.MkdirAll(modules, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte("#!/bin/sh\n# installed by an earlier deploy\n")
	if err := os.WriteFile(filepath.Join(modules, "kept"), previous, 0o755); err != nil {
		t.Fatal(err)
	}

	env, tmp := fmEnv(t,
		"FLEET_MODULE_SOURCES=kept=example.invalid/mod@v1.0.0 other="+fmBrokenModule(t)+" third=/nonexistent-fetch-module-source",
		"FLEET_MODULES_DIR="+modules)
	code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(base, "bin", "muster"))
	if code != 0 || so != "HARNESS-DONE\n" || se != "" {
		t.Errorf("exit=%d stdout=%q stderr=%q, want nothing but the harness marker", code, so, se)
	}
	got, err := os.ReadFile(filepath.Join(modules, "kept"))
	if err != nil || !bytes.Equal(got, previous) {
		t.Errorf("the previously installed module was disturbed (err=%v, content=%q)", err, got)
	}
	fmNoDebris(t, "modules dir", modules, "kept")
	fmNoDebris(t, "TMPDIR", tmp)
}

// A module that WAS fetched but cannot be put on the host is worth one line -
// and still never a failed deploy.
func TestFetchModule_DeploySectionInstallFailureWarnsAndContinues(t *testing.T) {
	fmRequireTools(t)
	base := t.TempDir()
	// A regular file where the modules directory's parent should be: mkdir
	// cannot succeed under it, however the tests are run.
	blocker := filepath.Join(base, "blocker")
	fmWrite(t, blocker, "not a directory\n")

	env, _ := fmEnv(t,
		"FLEET_MODULE_SOURCES=fake="+fmTinyModule(t),
		"FLEET_MODULES_DIR="+filepath.Join(blocker, "modules"))
	code, so, se := fmRunDeploySection(t, env, "local", filepath.Join(base, "bin", "muster"))
	if code != 0 || !strings.Contains(so, "HARNESS-DONE") {
		t.Fatalf("an install failure stopped the deploy: exit=%d stdout=%q stderr=%q", code, so, se)
	}
	if strings.Contains(so, "installed delivery module") {
		t.Errorf("reported an install that did not happen: %q", so)
	}
	if n := strings.Count(se, "WARNING delivery module fake was fetched but could not be installed"); n != 1 {
		t.Errorf("want exactly one install-failure warning, got %d:\n%s", n, se)
	}
}

// Over ssh the same section runs unchanged: what differs is only that a
// leading ~ is the HOST's to expand, and that the module is built here and
// copied there. Stand-ins for ssh and scp run the "remote" side in a fake home
// directory, so the paths the section hands to the host are exercised exactly
// as a real host would receive them.
func TestFetchModule_DeploySectionRemoteHostExpandsTildeItself(t *testing.T) {
	fmRequireTools(t)
	fakeHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fakeHome, "prefix", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	shims := t.TempDir()
	// ssh HOST COMMAND: run COMMAND in a shell whose HOME is the host's.
	fmWrite(t, filepath.Join(shims, "ssh"), "#!/bin/sh\nshift\nHOME=\"$FAKE_REMOTE_HOME\" exec sh -c \"$1\"\n")
	// scp -q SRC HOST:DST: copy, expanding a leading ~/ on the host's side.
	fmWrite(t, filepath.Join(shims, "scp"), `#!/bin/sh
dst=${3#*:}
case "$dst" in
'~/'*) dst="$FAKE_REMOTE_HOME/${dst#\~/}" ;;
esac
exec cp "$2" "$dst"
`)
	for _, name := range []string{"ssh", "scp"} {
		if err := os.Chmod(filepath.Join(shims, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		label, modulesDir, want string
	}{
		{"default directory", "", filepath.Join(fakeHome, "prefix", "libexec", "muster", "modules", "fake")},
		{"explicit directory", "~/elsewhere/modules", filepath.Join(fakeHome, "elsewhere", "modules", "fake")},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env, _ := fmEnv(t, "FLEET_MODULE_SOURCES=fake="+fmTinyModule(t), "FAKE_REMOTE_HOME="+fakeHome)
			env = fmSetEnv(env, "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))
			if tc.modulesDir != "" {
				env = fmSetEnv(env, "FLEET_MODULES_DIR="+tc.modulesDir)
			}
			code, so, se := fmRunDeploySection(t, env, "peer", "~/prefix/bin/muster")
			if code != 0 || se != "" || !strings.Contains(so, "HARNESS-DONE") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, so, se)
			}
			got, err := exec.Command(tc.want).Output()
			if err != nil || strings.TrimSpace(string(got)) != fmMarker {
				t.Errorf("no working module at %s: out=%q err=%v", tc.want, got, err)
			}
			if info, err := os.Stat(tc.want); err != nil || info.Mode().Perm() != 0o755 {
				t.Errorf("module at %s: err=%v info=%v, want mode 0755", tc.want, err, info)
			}
			fmNoDebris(t, tc.label, filepath.Dir(tc.want), "fake")
		})
	}
}
