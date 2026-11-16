package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// CoversHome reports whether granting root would include the service user's
// home directory: root is home, or one of its ancestors. It is the guard every
// grant and working directory passes before it is allowed near a profile.
//
// CoversOrIs cannot be that guard. It compares bytes, and a path that has been
// through ResolvePath keeps the letter case its caller typed for every
// component that is not a symlink. On a case-insensitive volume (the macOS
// default) "/USERS/SVC" names the same directory as "/Users/svc" and is
// granted by a profile that matches paths case-insensitively, yet it shares no
// prefix with it as a string (muster #488).
//
// So the question is asked of the directories, not of their spellings: root
// covers home when it is the same directory as home or as any ancestor of home
// (os.SameFile), which holds in any letter case and on any volume. A root that
// does not exist cannot be the same directory as anything, so that case is
// compared as text — case-folded, unless the volume holding it is shown to be
// case-sensitive. A volume that cannot be shown either way is folded: the guard
// is a refusal, and a spurious one costs a caller a more specific path while a
// missing one hands a session its home.
func CoversHome(root, home string) bool {
	if CoversOrIs(root, home) {
		return true
	}
	if ri, err := os.Stat(root); err == nil {
		for a := home; ; {
			if ai, err := os.Stat(a); err == nil && os.SameFile(ri, ai) {
				return true
			}
			parent := filepath.Dir(a)
			if parent == a {
				return false
			}
			a = parent
		}
	}
	if !foldsCase(nearestExisting(root)) {
		return false
	}
	return CoversOrIs(strings.ToLower(root), strings.ToLower(home))
}

// nearestExisting returns the deepest ancestor of p, or p itself, that exists.
func nearestExisting(p string) string {
	for d := p; ; {
		if _, err := os.Stat(d); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return d
		}
		d = parent
	}
}

// foldsCase reports whether the volume holding the existing directory dir
// treats two names that differ only in letter case as one entry, by looking up
// dir's own name — or, when it has no letters, the nearest ancestor's — in the
// opposite case. When nothing can be looked up it reports true (see CoversHome).
func foldsCase(dir string) bool {
	for d := dir; ; {
		if alt, ok := flipped(d); ok {
			return sameDirectory(d, alt)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	// A path of nothing but separators and digits: try an entry of the
	// topmost directory instead.
	top := dir
	for {
		parent := filepath.Dir(top)
		if parent == top {
			break
		}
		top = parent
	}
	f, err := os.Open(top)
	if err != nil {
		return true
	}
	defer f.Close()
	names, _ := f.Readdirnames(64)
	for _, n := range names {
		if alt, ok := flipped(filepath.Join(top, n)); ok {
			return sameDirectory(filepath.Join(top, n), alt)
		}
	}
	return true
}

// flipped returns p with the letter case of its last component swapped, and
// false when that component has no letter to swap.
func flipped(p string) (string, bool) {
	base := filepath.Base(p)
	swapped := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsUpper(r):
			return unicode.ToLower(r)
		case unicode.IsLower(r):
			return unicode.ToUpper(r)
		}
		return r
	}, base)
	if swapped == base {
		return "", false
	}
	return filepath.Join(filepath.Dir(p), swapped), true
}

// sameDirectory reports whether looking p up as alt finds p: true when alt is
// the same entry, false when it is absent or a different one, and true when
// the lookup failed for a reason that says nothing about case.
func sameDirectory(p, alt string) bool {
	pi, err := os.Stat(p)
	if err != nil {
		return true
	}
	ai, err := os.Stat(alt)
	switch {
	case err == nil:
		return os.SameFile(pi, ai)
	case errors.Is(err, fs.ErrNotExist):
		return false
	}
	return true
}
