package privfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll removes path and everything under it, as os.RemoveAll does, and
// also when the tree holds directories or files that something made read-only.
//
// A session's private directory is where the agent's tools run, and they leave
// things in it the way they leave them anywhere: a package cache sets its
// directories to r-x and its files to r--. Removing such a tree fails on the
// first entry with "permission denied" — unlinking needs write access to the
// DIRECTORY, not the file — so a close that removed the directory first could
// never finish, and the session stayed listed (muster #417).
//
// The plain removal is tried first; a tree nobody locked costs nothing extra.
// Only when it fails with a permission error is the tree made owner-writable —
// the equivalent of `chmod -R u+w` — and removed again. That second attempt's
// error is the one returned, so a directory that still cannot be removed (an
// entry another account owns, say) says so rather than being swallowed.
//
// No symlink is followed. The walk runs inside an os.Root, which refuses to
// resolve a path out of the directory, and a symlink inside the tree is never
// chmodded (chmod would act on its target); os.RemoveAll then unlinks the link
// itself. A path that is itself a symlink is removed as a link and its target
// is left alone. A path that does not exist is not an error.
//
// On Windows os.Chmod toggles only the read-only attribute, which is the same
// obstacle there: a file or directory carrying it cannot be deleted.
func RemoveAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if makeWritable(path) != nil {
		// Nothing could be loosened; the first failure is the honest one.
		return err
	}
	return os.RemoveAll(path)
}

// makeWritable adds owner write access to path and everything below it
// (owner read and search on directories too, or they could not be listed). It
// is best-effort below the top: an entry that cannot be changed is left for the
// removal to report, and the walk goes on, so one stubborn entry does not stop
// the rest from being loosened.
func makeWritable(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		// Removing a link needs only its parent; chmod would follow it.
		return errors.New("privfs: not changing the mode of a symbolic link")
	case !fi.IsDir():
		return chmodAdd(path, fi, 0o200)
	}
	// The directory itself first: opening it needs r and x, listing it too.
	if err := chmodAdd(path, fi, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			// A directory that could not be listed is reported by the removal;
			// the root was handled above.
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		want := fs.FileMode(0o200)
		if d.IsDir() {
			want = 0o700
		}
		if perm := info.Mode().Perm(); perm&want != want {
			// Chmod through the root: it will not leave the tree. A failure is
			// ignored on purpose (see above).
			_ = root.Chmod(filepath.FromSlash(name), perm|want)
		}
		return nil
	})
}

// chmodAdd sets the permission bits in want on path, if fi does not already
// carry them. fi must be an Lstat of path.
func chmodAdd(path string, fi fs.FileInfo, want fs.FileMode) error {
	perm := fi.Mode().Perm()
	if perm&want == want {
		return nil
	}
	return os.Chmod(path, perm|want)
}
