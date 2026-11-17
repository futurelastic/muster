package sessionenv

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

func TestLinkedPutIsReadBackThroughProvision(t *testing.T) {
	l := NewLinked(t.TempDir())
	if err := l.Put("opencode", "OPENAI_API_KEY", []byte("sk-linked-value")); err != nil {
		t.Fatal(err)
	}
	entries := l.Entries("opencode")
	if len(entries) != 1 || entries[0].Name != "OPENAI_API_KEY" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Required {
		t.Fatal("a linked entry must not be required: a file that goes missing leaves the session without it, like an optional declared one")
	}
	if got := entries[0].AppliesTo.Runtimes; len(got) != 1 || got[0] != "opencode" {
		t.Fatalf("a linked entry is scoped to its runtime, got %v", got)
	}
	// The same path a declared entry takes: Provision reads the file fresh.
	env, err := Provision(entries, fleet.SessionSpec{}, "m", "opencode", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["OPENAI_API_KEY"] != "sk-linked-value" {
		t.Fatalf("env = %v", env)
	}
	// A rotation is the next create's value, with nothing to reload.
	if err := l.Put("opencode", "OPENAI_API_KEY", []byte("sk-rotated")); err != nil {
		t.Fatal(err)
	}
	env, _ = Provision(l.Entries("opencode"), fleet.SessionSpec{}, "m", "opencode", "", nil)
	if env["OPENAI_API_KEY"] != "sk-rotated" {
		t.Fatalf("a rotation must reach the next create, env = %v", env)
	}
	// The other runtime meets nothing.
	if got := l.Entries("tmux"); len(got) != 0 {
		t.Fatalf("a linked key belongs to one runtime, tmux sees %+v", got)
	}
}

func TestLinkedFileIsOwnerOnlyAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	l := NewLinked(dir)
	if err := l.Put("opencode", "DEEPSEEK_API_KEY", []byte("k")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "linked", "opencode", "DEEPSEEK_API_KEY")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("the key file is %#o, want owner-only", perm)
		}
	}
	infos, _ := os.ReadDir(filepath.Dir(path))
	for _, i := range infos {
		if strings.HasSuffix(i.Name(), ".tmp") {
			t.Fatalf("a temporary file was left behind: %s", i.Name())
		}
	}
}

func TestLinkedRemoveIsIdempotentAndReportsWhichItWas(t *testing.T) {
	l := NewLinked(t.TempDir())
	if gone, err := l.Remove("opencode", "OPENAI_API_KEY"); err != nil || gone {
		t.Fatalf("nothing linked: removed=%v err=%v", gone, err)
	}
	_ = l.Put("opencode", "OPENAI_API_KEY", []byte("k"))
	if !l.Has("opencode", "OPENAI_API_KEY") {
		t.Fatal("Has is false for a linked key")
	}
	if gone, err := l.Remove("opencode", "OPENAI_API_KEY"); err != nil || !gone {
		t.Fatalf("linked: removed=%v err=%v", gone, err)
	}
	if l.Has("opencode", "OPENAI_API_KEY") || len(l.Entries("opencode")) != 0 {
		t.Fatal("the key is still there after Remove")
	}
}

func TestLinkedRefusesNamesThatCouldLeaveTheStore(t *testing.T) {
	l := NewLinked(t.TempDir())
	for _, c := range []struct{ rt, name string }{
		{"opencode", "../escape"}, {"opencode", "a/b"}, {"opencode", ""}, {"opencode", "1BAD"},
		{"../x", "OPENAI_API_KEY"}, {"", "OPENAI_API_KEY"}, {"a/b", "OPENAI_API_KEY"}, {"..", "OPENAI_API_KEY"},
	} {
		if err := l.Put(fleet.RuntimeId(c.rt), c.name, []byte("v")); err == nil {
			t.Errorf("Put(%q,%q) was accepted", c.rt, c.name)
		}
		if _, err := l.Remove(fleet.RuntimeId(c.rt), c.name); err == nil {
			t.Errorf("Remove(%q,%q) was accepted", c.rt, c.name)
		}
	}
}

func TestLinkedErrorsNeverHoldTheValue(t *testing.T) {
	dir := t.TempDir()
	// A file where the directory must be: Put fails, and says why without the value.
	if err := os.WriteFile(filepath.Join(dir, "linked"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := NewLinked(dir).Put("opencode", "OPENAI_API_KEY", []byte("sk-secret-do-not-print"))
	if err == nil {
		t.Fatal("Put succeeded where the directory cannot be made")
	}
	if strings.Contains(err.Error(), "sk-secret-do-not-print") {
		t.Fatalf("the error carries the value: %v", err)
	}
}
