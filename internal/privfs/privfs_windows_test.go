//go:build windows

package privfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// ownerOnlyProtected asserts p carries the protected list naming only the
// current user — the descriptor itself, not just the verdict about it.
func ownerOnlyProtected(t *testing.T, p string, wantInherit bool) {
	t.Helper()
	text, err := daclText(p)
	if err != nil {
		t.Fatalf("reading the access list of %s: %v", p, err)
	}
	d, err := parseSDDL(text)
	if err != nil {
		t.Fatalf("%s: %v (text %q)", p, err, text)
	}
	sid, err := userSID()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Protected {
		t.Errorf("%s: the list is not protected: %q", p, text)
	}
	if len(d.ACEs) != 1 {
		t.Fatalf("%s: %d entries, want exactly one: %q", p, len(d.ACEs), text)
	}
	a := d.ACEs[0]
	if a.Type != "A" || a.Trustee != sid || a.Rights&0x1F01FF != 0x1F01FF {
		t.Errorf("%s: entry = %+v, want full control for %s: %q", p, a, sid, text)
	}
	if got := a.Flags["OI"] && a.Flags["CI"]; got != wantInherit {
		t.Errorf("%s: inheritable = %v, want %v: %q", p, got, wantInherit, text)
	}
	if r := Check(p); r.Verdict != OwnerOnly {
		t.Errorf("%s: Check = %+v, want owner-only", p, r)
	}
}

func TestCreatedFileHasOwnerOnlyProtectedDACL(t *testing.T) {
	root := t.TempDir()

	// The temporary directory's own list is the inherited one: it is NOT
	// owner-only, which is what makes the rest of this test mean something.
	if r := Check(root); r.Verdict == OwnerOnly {
		t.Logf("note: the test's temporary directory is already owner-only (%+v)", r)
	}

	dir := filepath.Join(root, "state", "nested")
	if err := MkdirAll(dir); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	ownerOnlyProtected(t, dir, true)
	ownerOnlyProtected(t, filepath.Join(root, "state"), true)
	if err := MkdirAll(dir); err != nil {
		t.Errorf("MkdirAll of an existing directory: %v", err)
	}

	file := filepath.Join(dir, "token")
	if err := WriteFile(file, []byte("secret\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ownerOnlyProtected(t, file, false)
	if b, err := os.ReadFile(file); err != nil || string(b) != "secret\n" {
		t.Errorf("read back %q, %v", b, err)
	}

	f, err := OpenExcl(filepath.Join(dir, "backup"))
	if err != nil {
		t.Fatalf("OpenExcl: %v", err)
	}
	if _, err := f.WriteString("b"); err != nil {
		t.Errorf("writing the OpenExcl file: %v", err)
	}
	f.Close()
	ownerOnlyProtected(t, filepath.Join(dir, "backup"), false)
	if _, err := OpenExcl(filepath.Join(dir, "backup")); !errors.Is(err, fs.ErrExist) {
		t.Errorf("a second OpenExcl = %v, want fs.ErrExist", err)
	}

	tmp, err := CreateTemp(dir, ".config.*.tmp")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := tmp.WriteString("t"); err != nil {
		t.Errorf("writing the temp file: %v", err)
	}
	tmp.Close()
	ownerOnlyProtected(t, tmp.Name(), false)
	renamed := filepath.Join(dir, "config.json")
	if err := os.Rename(tmp.Name(), renamed); err != nil {
		t.Fatal(err)
	}
	ownerOnlyProtected(t, renamed, false)

	td, err := MkdirTemp(dir, "muster-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	ownerOnlyProtected(t, td, true)
}

func TestExistingLooseFileIsFixedByWriteFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "old")
	// Created the ordinary way: it inherits the parent's list.
	if err := os.WriteFile(p, []byte("old contents, much longer than the new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := Check(p); r.Verdict == OwnerOnly {
		t.Skipf("the runner's temporary directory is already owner-only (%+v); nothing to fix (windows, #303)", r)
	}
	if err := WriteFile(p, []byte("new")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ownerOnlyProtected(t, p, false)
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Errorf("contents = %q, want them replaced and truncated", b)
	}
}

func TestCheckOfAnAbsentPathIsUnknown(t *testing.T) {
	if r := Check(filepath.Join(t.TempDir(), "nope")); r.Verdict != Unknown {
		t.Errorf("Check(absent) = %+v, want unknown", r)
	}
}
