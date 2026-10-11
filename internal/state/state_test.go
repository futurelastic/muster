package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/privfs"
)

// These tests run on every platform, and on Windows they are what turns
// "internal/state: ok" from a statement about the compiler into one about the
// store (scripts/gate-windows.sh, #303). The privacy assertions go through
// privfs.Check, which reads a POSIX mode on Unix and the access list on
// Windows, so one assertion is right on both.

type doc struct {
	Keys  map[string]string `json:"keys"`
	Epoch int               `json:"epoch"`
}

func TestOpenWithAnEmptyDirDisablesPersistence(t *testing.T) {
	s, err := Open("")
	if err != nil || s != nil {
		t.Fatalf("Open(\"\") = %v, %v; want nil, nil", s, err)
	}
	// Every method must be safe on the nil store: callers hold it that way.
	var v doc
	if ok, err := s.Load("anything", &v); ok || err != nil {
		t.Errorf("Load on a nil store = %v, %v", ok, err)
	}
	if err := s.Save("anything", v); err != nil {
		t.Errorf("Save on a nil store = %v", err)
	}
	if s.Dir() != "" {
		t.Errorf("Dir on a nil store = %q", s.Dir())
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "nested")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Dir() != dir {
		t.Errorf("Dir = %q, want %q", s.Dir(), dir)
	}

	var got doc
	if ok, err := s.Load("sessions", &got); ok || err != nil {
		t.Fatalf("Load before any Save = %v, %v; want false, nil (a first run is not an error)", ok, err)
	}

	want := doc{Keys: map[string]string{"k1": "/work/a", "k2": "/work/b"}, Epoch: 7}
	if err := s.Save("sessions", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	ok, err := s.Load("sessions", &got)
	if err != nil || !ok {
		t.Fatalf("Load after Save = %v, %v", ok, err)
	}
	if got.Epoch != want.Epoch || len(got.Keys) != 2 || got.Keys["k1"] != "/work/a" || got.Keys["k2"] != "/work/b" {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	// A second Save replaces the first in full and leaves no scratch file.
	if err := s.Save("sessions", doc{Epoch: 8}); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	var again doc
	if ok, err := s.Load("sessions", &again); err != nil || !ok || again.Epoch != 8 || len(again.Keys) != 0 {
		t.Errorf("after replace = %+v, %v, %v", again, ok, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "sessions.json" {
		t.Errorf("directory holds %d entries, want exactly sessions.json", len(entries))
	}
}

func TestStoreDirectoryAndDocumentAreOwnerOnly(t *testing.T) {
	// A directory that does not exist yet, so Open is the thing that creates it
	// (an existing one is left as it is, on both platforms).
	dir := filepath.Join(t.TempDir(), "fresh")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r := privfs.Check(dir); r.Verdict != privfs.OwnerOnly {
		t.Errorf("state directory is %s (%s), want owner-only", r.Verdict, r.Detail)
	}
	if err := s.Save("sessions", doc{Epoch: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The document is written to a temp file and renamed over its name; the
	// list it was created with must survive the rename.
	written := filepath.Join(dir, "sessions.json")
	if r := privfs.Check(written); r.Verdict != privfs.OwnerOnly {
		t.Errorf("written document is %s (%s), want owner-only", r.Verdict, r.Detail)
	}
}

func TestCorruptDocumentIsReportedAndNeverReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions.json")
	const torn = `{"keys": {"k1": `
	if err := os.WriteFile(path, []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	var v doc
	ok, err := s.Load("sessions", &v)
	if ok || err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Load of a torn file = %v, %v; want false and a corrupt error", ok, err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != torn {
		t.Errorf("the corrupt file was altered by a failed Load: %q", raw)
	}
}

func TestOpenRefusesAPathThatIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(file); err == nil {
		t.Errorf("Open on a file succeeded: %+v", s)
	}
}
