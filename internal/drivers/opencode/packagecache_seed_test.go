//go:build unix

package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #432: a package-cache seed a session starts from a copy of, and the
// variables each package manager reads to find that copy. What the host
// enforces is checked by running real commands from inside real sessions, as
// the other sandbox tests do.

// managerSeed makes a seed holding a warm file in the directory of each manager
// named, and returns its real path.
func managerSeed(t *testing.T, managers ...fleet.PackageManager) string {
	t.Helper()
	seed := resolvePath(t.TempDir())
	for _, m := range managers {
		dir := filepath.Join(seed, string(m))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "warm"), []byte("warm-"+string(m)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return seed
}

func seededSession(t *testing.T, d *Driver, key, seed string, mode fleet.PackageCacheMode, managers ...fleet.PackageManager) fleet.Session {
	t.Helper()
	sess, _ := sandboxedSession(t, d, key, fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{
		Path: fleet.AbsolutePath(seed), Mode: mode, Managers: managers,
	}})
	return sess
}

// sessionCache is where the report says the session's cache is.
func sessionCache(t *testing.T, d *Driver, ref fleet.SessionRef) *fleet.SandboxPackageCacheState {
	t.Helper()
	st, err := d.State(context.Background(), testReq, ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sandbox == nil || st.Sandbox.PackageCache == nil {
		t.Fatalf("the report carries no package cache: %+v", st.Sandbox)
	}
	return st.Sandbox.PackageCache
}

func env(t *testing.T, d *Driver, id, name string) string {
	t.Helper()
	r := sh(t, d, id, `printenv `+name)
	if r.Rc != 0 {
		return ""
	}
	return strings.TrimSpace(r.Out)
}

// Each manager is handed the variable it reads, pointing at its own directory of
// THIS session's copy — never at the seed.
func TestSeededCache_EachManagerIsPointedAtItsOwnDirectoryOfTheSessionsCopy(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	all := fleet.PackageManagers()
	seed := managerSeed(t, all...)

	for _, m := range all {
		t.Run(string(m), func(t *testing.T) {
			sess := seededSession(t, d, "k-seed-"+string(m), seed, "", m)
			pc := sessionCache(t, d, sess.SessionRef)
			if pc.Mode != fleet.PackageCachePrivate || string(pc.Path) == seed || len(pc.Managers) != 1 || pc.Managers[0] != m {
				t.Fatalf("the report must say a private copy of the seed serves %s: %+v", m, pc)
			}
			want := filepath.Join(string(pc.Path), string(m))
			if got := env(t, d, sess.ID, m.EnvVar()); got != want {
				t.Fatalf("%s = %q, want %q (the manager's directory of the session's copy)", m.EnvVar(), got, want)
			}
			// The copy is the warm cache: the manager finds what the seed held.
			if r := sh(t, d, sess.ID, `cat "$`+m.EnvVar()+`"/warm`); r.Rc != 0 || r.Out != "warm-"+string(m) {
				t.Fatalf("the session's copy lacks the seed's content: %+v", r)
			}
			// A manager the cache was not said to serve is left alone.
			for _, other := range all {
				if other != m && env(t, d, sess.ID, other.EnvVar()) != "" {
					t.Errorf("%s is set though the cache does not serve %s", other.EnvVar(), other)
				}
			}
		})
	}
}

func TestSeededCache_ServingEveryManagerSetsAllThree(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	seed := managerSeed(t, fleet.PackageManagers()...)
	sess := seededSession(t, d, "k-all", seed, "", fleet.PackageManagers()...)
	pc := sessionCache(t, d, sess.SessionRef)
	for _, m := range fleet.PackageManagers() {
		if got, want := env(t, d, sess.ID, m.EnvVar()), filepath.Join(string(pc.Path), string(m)); got != want {
			t.Errorf("%s = %q, want %q", m.EnvVar(), got, want)
		}
	}
	// The announcement every private cache already carried is still there.
	if got := env(t, d, sess.ID, "MUSTER_PACKAGE_CACHE"); got != string(pc.Path) {
		t.Errorf("MUSTER_PACKAGE_CACHE = %q, want %q", got, pc.Path)
	}
}

// A write in one session's copy reaches neither the seed nor another session's
// copy, and the session cannot write to the seed itself.
func TestSeededCache_AWriteNeverLeavesTheSessionsCopy(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	all := fleet.PackageManagers()
	seed := managerSeed(t, all...)
	a := seededSession(t, d, "k-a", seed, "", all...)
	b := seededSession(t, d, "k-b", seed, "", all...)
	pa, pb := sessionCache(t, d, a.SessionRef), sessionCache(t, d, b.SessionRef)
	if pa.Path == pb.Path {
		t.Fatalf("two sessions share a cache directory: %s", pa.Path)
	}

	for _, m := range all {
		v := `"$` + m.EnvVar() + `"`
		if r := sh(t, d, a.ID, `echo planted > `+v+`/planted && cat `+v+`/planted`); r.Rc != 0 || !strings.Contains(r.Out, "planted") {
			t.Fatalf("a session must be able to write its own copy of %s: %+v", m, r)
		}
		if fileExists(filepath.Join(seed, string(m), "planted")) {
			t.Errorf("a write to the %s copy reached the seed", m)
		}
		if fileExists(filepath.Join(string(pb.Path), string(m), "planted")) {
			t.Errorf("a write to one session's %s copy is visible in another session's", m)
		}
		if r := sh(t, d, b.ID, `test -e `+v+`/planted`); r.Rc == 0 {
			t.Errorf("the second session sees what the first wrote to %s", m)
		}
	}

	// The seed itself is out of reach of the sandbox: a write to it fails and
	// leaves nothing behind, from either session.
	for _, id := range []string{a.ID, b.ID} {
		for _, m := range all {
			target := filepath.Join(seed, string(m), "direct")
			if r := sh(t, d, id, `echo x > `+target); r.Rc == 0 {
				t.Errorf("a sandboxed session wrote to the seed's %s directory: %+v", m, r)
			}
			if fileExists(target) {
				t.Errorf("the seed's %s directory gained a file from a sandboxed session", m)
			}
		}
		if r := sh(t, d, id, `echo x > `+filepath.Join(seed, "direct")); r.Rc == 0 || fileExists(filepath.Join(seed, "direct")) {
			t.Errorf("a sandboxed session wrote to the seed's top directory: %+v", r)
		}
	}
}

// What the seed holds is read-only the way a package cache installs it. The copy
// keeps those modes, and closing the session still removes it.
func TestSeededCache_ACopyOfAReadOnlySeedIsRemovedWithItsSession(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not stop an unlink, so there is nothing to test")
	}
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	seed := managerSeed(t, fleet.PackageManagerGo)
	mod := filepath.Join(seed, "go", "example.com", "mod@v1.0.0")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "mod.go"), []byte("package mod"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mod, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(mod, 0o755) })

	sess := seededSession(t, d, "k-ro", seed, "", fleet.PackageManagerGo)
	known, ok := d.wasSeen(sess.ID)
	if !ok || known.srv == nil || known.srv.dir == "" {
		t.Fatalf("session not known with a directory: %+v", known)
	}
	dir := known.srv.dir
	if r := sh(t, d, sess.ID, `cat "$GOMODCACHE"/example.com/mod@v1.0.0/mod.go`); r.Rc != 0 || r.Out != "package mod" {
		t.Fatalf("the read-only module is not readable from the copy: %+v", r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.Close(ctx, testReq, sess.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the session directory, with its copy of the seed, survived Close: %v", err)
	}
}

// `shared` hands the seed itself to the session, writable, and points the
// managers into it: the trade-off docs/api.md states.
func TestSeededCache_ASharedSeedIsWhatTheManagersAreGiven(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	seed := managerSeed(t, fleet.PackageManagerCargo)
	sess := seededSession(t, d, "k-shared", seed, fleet.PackageCacheShared, fleet.PackageManagerCargo)
	pc := sessionCache(t, d, sess.SessionRef)
	if pc.Mode != fleet.PackageCacheShared || string(pc.Path) != seed {
		t.Fatalf("the report must say the cache is the shared seed: %+v", pc)
	}
	if got, want := env(t, d, sess.ID, "CARGO_HOME"), filepath.Join(seed, "cargo"); got != want {
		t.Fatalf("CARGO_HOME = %q, want %q", got, want)
	}
	if r := sh(t, d, sess.ID, `echo s > "$CARGO_HOME"/shared-write`); r.Rc != 0 || !fileExists(filepath.Join(seed, "cargo", "shared-write")) {
		t.Fatalf("a shared seed is writable by its sessions: %+v", r)
	}
}

// A cache that announces itself only (the shape before #432) is not pointed at by
// any manager, and a create may still set those variables itself.
func TestSeededCache_ACacheWithNoManagersSetsNoManagerVariable(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	seed := managerSeed(t, fleet.PackageManagers()...)
	sess := seededSession(t, d, "k-legacy", seed, "")
	for _, m := range fleet.PackageManagers() {
		if got := env(t, d, sess.ID, m.EnvVar()); got != "" {
			t.Errorf("%s = %q, though no manager was named", m.EnvVar(), got)
		}
	}
	if got := env(t, d, sess.ID, "MUSTER_PACKAGE_CACHE"); got == "" {
		t.Error("the announced cache is gone")
	}
}

func TestSeededCache_ACallerMaySetAManagerVariableTheCacheDoesNotServe(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	seed := managerSeed(t, fleet.PackageManagerGo)
	cwd := resolvePath(t.TempDir())
	sess := mustCreate(t, d, "k-own-cargo", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(cwd), Env: map[string]string{"CARGO_HOME": cwd + "/own-cargo"},
		Sandbox: &fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{
			Path: fleet.AbsolutePath(seed), Managers: []fleet.PackageManager{fleet.PackageManagerGo},
		}},
	})
	if got := env(t, d, sess.ID, "CARGO_HOME"); got != cwd+"/own-cargo" {
		t.Errorf("CARGO_HOME = %q: the caller's own value was replaced for a manager the cache does not serve", got)
	}
	if got, want := env(t, d, sess.ID, "GOMODCACHE"), filepath.Join(string(sessionCache(t, d, sess.SessionRef).Path), "go"); got != want {
		t.Errorf("GOMODCACHE = %q, want %q", got, want)
	}
}

// Every refusal is made before a process exists and leaves no directory behind
// — including the one that would have been a copy of the seed.
func TestSeededCache_RefusalsLeaveNothingRunningOrCopied(t *testing.T) {
	d, root := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	seed := managerSeed(t, fleet.PackageManagerCargo, fleet.PackageManagerGo)
	elsewhere := managerSeed(t, fleet.PackageManagerNpm)
	linked := resolvePath(t.TempDir())
	if err := os.Symlink(filepath.Join(elsewhere, "npm"), filepath.Join(linked, "npm")); err != nil {
		t.Fatal(err)
	}
	cache := func(path string, managers ...fleet.PackageManager) *fleet.SandboxSpec {
		return &fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{Path: fleet.AbsolutePath(path), Managers: managers}}
	}
	for name, c := range map[string]struct {
		spec fleet.SessionSpec
		want string
	}{
		"a manager the seed has no directory for": {
			fleet.SessionSpec{Sandbox: cache(seed, fleet.PackageManagerCargo, fleet.PackageManagerNpm)}, "npm"},
		"a manager directory that is a link": {
			fleet.SessionSpec{Sandbox: cache(linked, fleet.PackageManagerNpm)}, "not a link"},
		"an unknown manager": {
			fleet.SessionSpec{Sandbox: cache(seed, "pip")}, "managers"},
		"the caller naming a variable the cache sets": {
			fleet.SessionSpec{Sandbox: cache(seed, fleet.PackageManagerGo), Env: map[string]string{"GOMODCACHE": "/elsewhere"}}, "GOMODCACHE"},
	} {
		spec := c.spec
		spec.Cwd = fleet.AbsolutePath(t.TempDir())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := d.Create(ctx, testReq, "k-"+strings.ReplaceAll(name, " ", "-"), spec)
		cancel()
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid {
			t.Errorf("%s: want an invalid refusal, got %v", name, err)
			continue
		}
		if !strings.Contains(fe.Message, c.want) {
			t.Errorf("%s: refusal %q does not name %q", name, fe.Message, c.want)
		}
	}
	if n := len(d.live); n != 0 {
		t.Fatalf("refused creates left %d servers running", n)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("refused creates left session directories behind: %v", entries)
	}
}

// The capability says which managers a session can be pointed at, so a posture
// asking for one is refused before a create rather than answered with a session
// that downloads everything again.
func TestSeededCache_TheCapabilityNamesTheManagersItCanServe(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	sup := d.Capabilities().Sandbox
	if len(sup.PackageManagers) != len(fleet.PackageManagers()) {
		t.Fatalf("packageManagers = %v, want %v", sup.PackageManagers, fleet.PackageManagers())
	}
	ps := fleet.PostureSupportFrom(d.Capabilities())
	if len(ps.PackageManagers) != 3 || len(ps.PackageCache) != 2 {
		t.Errorf("posture support = %+v", ps)
	}
	if _, err := (fleet.Posture{PackageCache: &fleet.PosturePackageCache{
		Seed: "/srv/seed", Managers: fleet.PackageManagers(),
	}}).Lower(d.Capabilities()); err != nil {
		t.Errorf("a posture naming a seed must lower against this driver: %v", err)
	}
}
