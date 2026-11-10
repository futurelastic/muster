//go:build darwin

package opencode

import (
	"path/filepath"
	"testing"
)

// muster #448: the real reader, on this host's own filesystem.

// A path that does not exist yet is on its nearest existing parent's volume, and
// the volume names a mount point a reader can recognise.
func TestVolumeOfPath_ANewPathIsOnItsNearestExistingParent(t *testing.T) {
	dir := resolvePath(t.TempDir())
	parent, err := volumeOfPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if parent.mount == "" || !filepath.IsAbs(parent.mount) {
		t.Fatalf("mount = %q; want the mount point of the volume", parent.mount)
	}
	child, err := volumeOfPath(filepath.Join(dir, "not", "yet", "pkgcache"))
	if err != nil {
		t.Fatal(err)
	}
	if child != parent {
		t.Fatalf("a path under %q is on %+v; want its parent's %+v", dir, child, parent)
	}
}

// Two directories on one volume compare equal, and the real check answers
// nothing for them.
func TestCheckSeedVolume_SameVolumeIsNotRefused(t *testing.T) {
	src, dst := resolvePath(t.TempDir()), resolvePath(t.TempDir())
	if err := checkSeedVolume(src, filepath.Join(dst, "pkgcache")); err != nil {
		t.Fatalf("two directories under one temp root were refused: %v", err)
	}
}
