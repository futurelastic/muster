package privfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAll_ATreeNobodyLockedIsRemoved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := MkdirAll(filepath.Join(dir, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory survived: %v", err)
	}
}

func TestRemoveAll_APathThatIsNotThereIsNotAnError(t *testing.T) {
	if err := RemoveAll(filepath.Join(t.TempDir(), "never-made")); err != nil {
		t.Errorf("RemoveAll of a missing path = %v, want nil", err)
	}
}
