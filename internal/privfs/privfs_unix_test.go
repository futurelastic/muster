//go:build !windows

package privfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestUnix_ModesAreExactlyTheOnesTheCallSitesUsedBefore(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b")
	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %#o, want 0700", fi.Mode().Perm())
	}
	file := filepath.Join(dir, "f")
	if err := WriteFile(file, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %#o, want 0600", fi.Mode().Perm())
	}
	f, err := OpenExcl(filepath.Join(dir, "g"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := OpenExcl(filepath.Join(dir, "g")); !errors.Is(err, fs.ErrExist) {
		t.Errorf("a second OpenExcl = %v, want fs.ErrExist", err)
	}
	tmp, err := CreateTemp(dir, ".t-*")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	if fi, _ := os.Stat(tmp.Name()); fi.Mode().Perm() != 0o600 {
		t.Errorf("temp file mode = %#o, want 0600", fi.Mode().Perm())
	}
	td, err := MkdirTemp(dir, "d-*")
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(td); fi.Mode().Perm() != 0o700 {
		t.Errorf("temp directory mode = %#o, want 0700", fi.Mode().Perm())
	}
}

func TestUnix_CheckReadsTheMode(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(dir, "private")
	if err := os.WriteFile(private, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := Check(loose); r.Verdict != Loose || r.Detail == "" {
		t.Errorf("Check(0644) = %+v, want loose with a reason", r)
	}
	if r := Check(private); r.Verdict != OwnerOnly {
		t.Errorf("Check(0600) = %+v, want owner-only", r)
	}
	if r := Check(filepath.Join(dir, "absent")); r.Verdict != Unknown {
		t.Errorf("Check(absent) = %+v, want unknown", r)
	}
}
