package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/privfs"
)

func TestOpenLogFileCreatesOwnerOnlyAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "muster.log")
	f, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if rep := privfs.Check(path); rep.Verdict == privfs.Loose {
		t.Errorf("a new log file is not owner-only: %s", rep.Detail)
	}

	f, err = openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, _ := os.ReadFile(path)
	if string(got) != "first\nsecond\n" {
		t.Errorf("file = %q, want both runs appended", got)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the console is gone") }

func TestTeeLogKeepsTheFileWhenTheOtherWriterFails(t *testing.T) {
	var file bytes.Buffer
	w := newTeeLog(&file, failingWriter{})
	n, err := w.Write([]byte("line\n"))
	if err != nil || n != 5 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if !strings.Contains(file.String(), "line") {
		t.Errorf("file = %q", file.String())
	}
}
