package opencode

import (
	"runtime"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #484: the decision about the developer directory is pure, so it is
// checked on every platform with the paths of a macOS host stated outright. The
// stock profile reads /Library and /Applications/Xcode.app; a versioned Xcode
// app is neither, which is the case the defect lives in (a test that chose the
// stock app would pass for the wrong reason).

const (
	stockXcode    = "/Applications/Xcode.app/Contents/Developer"
	versionedDev  = "/Applications/Xcode-15.2.0.app/Contents/Developer"
	cltDir        = "/Library/Developer/CommandLineTools"
	macOnlyReason = "the paths are macOS's, and a path is matched with the host's separator"
)

func stockRead() []string { return append([]string(nil), systemReadRoots...) }

func TestDeveloperAccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(macOnlyReason)
	}
	cases := []struct {
		name         string
		selected     string
		clt          string
		read         []string
		wantEnv      string
		wantGrant    string
		whyItMatters string
	}{
		{"nothing selected", "", cltDir, stockRead(), "", "", "the shim falls back to a default under a root the profile reads"},
		{"the stock Xcode app", stockXcode, cltDir, stockRead(), "", "", "already readable: left to choose as it always did"},
		{"the Command Line Tools themselves", cltDir, cltDir, stockRead(), "", "", "under /Library, already readable"},
		{"a versioned Xcode app, tools installed", versionedDev, cltDir, stockRead(), cltDir, "", "pointed at a readable toolchain; the profile does not widen"},
		{"a versioned Xcode app, no tools", versionedDev, "", stockRead(), "", versionedDev, "the selected directory is added, read-only"},
		{"tools named but not readable", versionedDev, "/opt/elsewhere/CLT", stockRead(), "", versionedDev, "a directory the profile does not read is no fix"},
		{"selected already granted by the operator", versionedDev, cltDir, append(stockRead(), "/Applications/Xcode-15.2.0.app"), "", "", "an operator's own read grant counts as readable"},
	}
	for _, c := range cases {
		env, grant := developerAccess(c.selected, c.clt, c.read)
		if env != c.wantEnv || grant != c.wantGrant {
			t.Errorf("%s: developerAccess = (env %q, grant %q), want (%q, %q) — %s",
				c.name, env, grant, c.wantEnv, c.wantGrant, c.whyItMatters)
		}
	}
}

// planOnHostWith plans a sandboxed create on a host that selects `selected` and has the
// Command Line Tools at `clt` (either may be empty).
func planOnHostWith(t *testing.T, selected, clt string) *sandboxPlan {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(macOnlyReason)
	}
	real := developerToolchain
	developerToolchain = func() (string, string) { return selected, clt }
	t.Cleanup(func() { developerToolchain = real })
	d, _ := newIsolatedDriver(t)
	plan, err := d.planSandbox(fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func readsPath(read []string, path string) bool {
	for _, r := range read {
		if r == path {
			return true
		}
	}
	return false
}

func TestPlanPointsTheSessionAtTheCommandLineToolsWhenTheSelectedDirectoryIsUnreadable(t *testing.T) {
	plan := planOnHostWith(t, versionedDev, cltDir)
	if plan.developerDir != cltDir {
		t.Errorf("developerDir = %q, want %q", plan.developerDir, cltDir)
	}
	if readsPath(plan.read, versionedDev) {
		t.Errorf("the profile was widened to the selected directory although the tools were readable: %v", plan.read)
	}
}

func TestPlanGrantsTheSelectedDirectoryReadOnlyWhenThereAreNoCommandLineTools(t *testing.T) {
	plan := planOnHostWith(t, versionedDev, "")
	if plan.developerDir != "" {
		t.Errorf("developerDir = %q, want none: the shim reads the selection it already has", plan.developerDir)
	}
	if !readsPath(plan.read, versionedDev) {
		t.Errorf("the selected developer directory is not in the read set: %v", plan.read)
	}
	if readsPath(plan.write, versionedDev) {
		t.Errorf("the developer directory was made writable: %v", plan.write)
	}
	// The report is what is enforced: a grant that is not in it would be a hidden one.
	var reported bool
	for _, p := range plan.state().ReadPaths {
		if string(p) == versionedDev {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the grant is not in the sandbox state the session reports: %v", plan.state().ReadPaths)
	}
}

func TestPlanLeavesAReadableSelectionAlone(t *testing.T) {
	plan := planOnHostWith(t, stockXcode, cltDir)
	if plan.developerDir != "" {
		t.Errorf("developerDir = %q, want none for a readable selection", plan.developerDir)
	}
	if readsPath(plan.read, stockXcode) {
		t.Errorf("a directory already covered by a stock root was added again: %v", plan.read)
	}
}

// The names this change makes the driver's own are refused, so a caller cannot
// silently compete with them (#484) — and a name only macOS reads is refused
// only there.
func TestOwnedNamesIncludeTheToolchainAndTheHereDocumentPrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("neither variable is read on Windows")
	}
	if !ownedName("TMPPREFIX") {
		t.Error("TMPPREFIX is not owned: a create could point zsh's here-documents back outside the session")
	}
	if got, want := ownedName(developerDirEnv), runtime.GOOS == "darwin"; got != want {
		t.Errorf("DEVELOPER_DIR owned = %v on %s, want %v", got, runtime.GOOS, want)
	}
	err := checkOwnedEnv(map[string]string{"TMPPREFIX": "/tmp/zsh"})
	if err == nil || !strings.Contains(err.Error(), "TMPPREFIX") {
		t.Errorf("a create naming TMPPREFIX = %v, want a refusal that names it", err)
	}
}

func TestScopedEnvPutsTheHereDocumentPrefixInsideTheSessionsTmp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TMPPREFIX is zsh's, and Windows has no zsh to scope")
	}
	dir := t.TempDir()
	env := scopedEnv(dir)
	prefix := env["TMPPREFIX"]
	if !strings.HasPrefix(prefix, env["TMPDIR"]+"/") {
		t.Errorf("TMPPREFIX = %q is not inside TMPDIR = %q", prefix, env["TMPDIR"])
	}
}

// muster #505: the toolchain shim's lookup cache is moved inside the session by
// a variable the driver sets. Which value, and on which platform, is pure, so it
// is checked on every platform.
func TestXcrunCacheEnvIsAnAbsolutePathInsideTheSessionsTmpOnMacOSOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(macOnlyReason)
	}
	dir := "/var/session/abc"
	got := xcrunCacheEnv("darwin", dir)
	if len(got) != 1 {
		t.Fatalf("xcrunCacheEnv(darwin) = %v, want exactly the shim's cache variable", got)
	}
	v := got[xcrunDBEnv]
	// The library ignores a relative value and falls back to the directory the
	// profile refuses, so the value must be absolute; and it must be where the
	// session may write, which is its own tmp.
	if v != dir+"/tmp/xcrun_db" {
		t.Errorf("%s = %q, want the cache file inside the session's tmp %q", xcrunDBEnv, v, dir+"/tmp")
	}
	if xcrunDBEnv != "xcrun_db" {
		t.Errorf("the variable is spelled %q: the library reads the lower-case name and the environment is case-sensitive", xcrunDBEnv)
	}
	for _, goos := range []string{"linux", "windows", "freebsd"} {
		if got := xcrunCacheEnv(goos, dir); got != nil {
			t.Errorf("xcrunCacheEnv(%s) = %v: only macOS has the shim", goos, got)
		}
	}
}

func TestTheShimsCacheVariableIsOwnedOnMacOSOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(macOnlyReason)
	}
	if got, want := ownedName(xcrunDBEnv), runtime.GOOS == "darwin"; got != want {
		t.Errorf("%s owned = %v on %s, want %v", xcrunDBEnv, got, runtime.GOOS, want)
	}
	if runtime.GOOS == "darwin" {
		// A create that named it would point the cache back at the directory the
		// profile refuses, or at a place another session reads.
		err := checkOwnedEnv(map[string]string{xcrunDBEnv: "/somewhere/xcrun_db"})
		if err == nil || !strings.Contains(err.Error(), xcrunDBEnv) {
			t.Errorf("a create naming %s = %v, want a refusal that names it", xcrunDBEnv, err)
		}
	}
}
