//go:build unix

package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #448: a private package-cache seed that is on a different volume than
// the session's directory cannot be cloned, so it is refused before anything is
// copied unless the request opts in to the full copy. Two volumes are not
// something a test machine has to hand, so the volume reader and the host's
// ability to clone are seams, as the clone call and the copy program are.

// useVolumes stands in for the host: whether it can clone, and which volume
// each path is on (answered by the last matching prefix in vols).
func useVolumes(t *testing.T, clones bool, vol func(path string) (volume, error)) {
	t.Helper()
	oh, ov := hostClone, volumeOf
	t.Cleanup(func() { hostClone, volumeOf = oh, ov })
	hostClone = func() bool { return clones }
	volumeOf = vol
}

// seedElsewhere puts the seed on volume "/Volumes/seed" and everything else on
// "/".
func seedElsewhere(seed string) func(string) (volume, error) {
	return func(p string) (volume, error) {
		if p == seed || strings.HasPrefix(p, seed+string(filepath.Separator)) {
			return volume{id: 2, mount: "/Volumes/seed"}, nil
		}
		return volume{id: 1, mount: "/"}, nil
	}
}

// plainCopyOnly is a copy program that refuses `-cR` as a filesystem which cannot
// clone does, and copies plainly otherwise, so a test can see the report "copy"
// on a machine whose own filesystem clones.
func plainCopyOnly(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "cp")
	body := "#!/bin/sh\ncase \"$1\" in -cR) exit 1;; esac\nexec /bin/cp \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func planWith(t *testing.T, d *Driver, seed string, pc fleet.SandboxPackageCache) (*sandboxPlan, string, error) {
	t.Helper()
	pc.Path = fleet.AbsolutePath(seed)
	dir := t.TempDir()
	spec := fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{PackageCache: &pc}}
	plan, err := d.planSandboxFor(context.Background(), spec, dir, "")
	return plan, dir, err
}

// Across volumes the create is refused naming both, before any copy is tried and
// with nothing left in the session's directory.
func TestSeedVolume_AnotherVolumeIsRefusedBeforeAnyCopy(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	seed := managerSeed(t, fleet.PackageManagerGo)
	useVolumes(t, true, seedElsewhere(seed))
	useSeams(t, func(string, string) error {
		t.Error("the clone was tried for a seed on another volume")
		return errCloneUnsupported
	}, "/nonexistent/cp")

	_, dir, err := planWith(t, d, seed, fleet.SandboxPackageCache{})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid {
		t.Fatalf("want an invalid refusal, got %v", err)
	}
	for _, want := range []string{seed, "/Volumes/seed", `"/"`, "allowFullCopy"} {
		if !strings.Contains(fe.Message, want) {
			t.Errorf("refusal %q does not name %q", fe.Message, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "pkgcache")); !os.IsNotExist(err) {
		t.Fatalf("a refused plan left the session's cache behind: %v", err)
	}
}

// The opt-in lets it through as the full copy it is, reported as such.
func TestSeedVolume_AllowFullCopyTakesTheCopyAndSaysSo(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	seed := managerSeed(t, fleet.PackageManagerGo)
	useVolumes(t, true, seedElsewhere(seed))
	useSeams(t, noClone, plainCopyOnly(t))

	plan, dir, err := planWith(t, d, seed, fleet.SandboxPackageCache{AllowFullCopy: true})
	if err != nil {
		t.Fatalf("an opted-in copy across volumes was refused: %v", err)
	}
	if plan.cache == nil || plan.cache.Copy != "copy" {
		t.Fatalf("copy = %+v; want a plain copy reported as %q", plan.cache, "copy")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pkgcache", "go", "warm")); err != nil || string(b) != "warm-go" {
		t.Fatalf("the copy does not hold the seed: %q, %v", b, err)
	}
}

// What the check does not touch: a seed on the session directory's volume, a
// host with no clone call (every create is a copy there), a volume that cannot
// be read, the shared mode (nothing is copied), and a relaunch that already has
// its copy.
func TestSeedVolume_NothingElseIsRefused(t *testing.T) {
	seed := managerSeed(t, fleet.PackageManagerGo)
	across := seedElsewhere(seed)
	same := func(string) (volume, error) { return volume{id: 1, mount: "/"}, nil }
	unreadable := func(string) (volume, error) { return volume{}, errors.New("no such volume") }

	for name, c := range map[string]struct {
		clones bool
		vol    func(string) (volume, error)
		pc     fleet.SandboxPackageCache
		prior  string
		want   string // the copy report, "" for the shared mode
	}{
		"the same volume":              {true, same, fleet.SandboxPackageCache{}, "", "clone"},
		"a host with no clone call":    {false, across, fleet.SandboxPackageCache{}, "", "copy"},
		"a volume that cannot be read": {true, unreadable, fleet.SandboxPackageCache{}, "", "clone"},
		"the shared mode":              {true, across, fleet.SandboxPackageCache{Mode: fleet.PackageCacheShared}, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := newIsolatedDriver(t)
			useVolumes(t, c.clones, c.vol)
			useSeams(t, func(src, dst string) error {
				if !c.clones {
					return errCloneUnsupported
				}
				return os.MkdirAll(dst, 0o755)
			}, "")
			if !c.clones {
				useSeams(t, noClone, plainCopyOnly(t))
			}
			plan, _, err := planWith(t, d, seed, c.pc)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if plan.cache == nil || plan.cache.Copy != c.want {
				t.Fatalf("copy = %+v; want %q", plan.cache, c.want)
			}
		})
	}

	t.Run("a relaunch that has its copy", func(t *testing.T) {
		d, _ := newIsolatedDriver(t)
		useVolumes(t, true, across)
		useSeams(t, func(string, string) error {
			t.Error("a relaunch copied the seed again")
			return errCloneUnsupported
		}, "/nonexistent/cp")
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "pkgcache", "go"), 0o755); err != nil {
			t.Fatal(err)
		}
		spec := fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{
			PackageCache: &fleet.SandboxPackageCache{Path: fleet.AbsolutePath(seed), Managers: []fleet.PackageManager{fleet.PackageManagerGo}},
		}}
		plan, err := d.planSandboxFor(context.Background(), spec, dir, "copy")
		if err != nil {
			t.Fatalf("a relaunch was refused for the volume its copy was made across: %v", err)
		}
		if plan.cache == nil || plan.cache.Copy != "copy" {
			t.Fatalf("copy = %+v; want the recorded %q", plan.cache, "copy")
		}
	})
}
