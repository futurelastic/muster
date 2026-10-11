//go:build windows

package modtest

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// sidecarSuffix names the file beside a fake module's executable that carries
// its Behaviour.
const sidecarSuffix = ".behaviour.json"

// sidecarBehaviour returns the Behaviour Install wrote next to the running
// executable, or "" when there is none (an ordinary test run).
func sidecarBehaviour() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(exe + sidecarSuffix)
	if err != nil {
		return ""
	}
	return string(b)
}

// Install makes dir/name.exe a copy of the running test binary and writes
// behaviour b beside it, and returns its path. It is a copy and never a hard
// link: the test process itself is running from that file, and Windows refuses
// to delete ANY name of a file that has a running image, so a link would make
// the test's temporary directory impossible to remove. There is no
// /bin/sh to wrap it in on Windows, and the launcher gives the child a minimal
// environment, so the behaviour rides in a file the binary finds by its own
// name: MaybeServe, called first in TestMain, reads it and serves the protocol.
func Install(t testing.TB, dir, name string, b Behaviour) string {
	t.Helper()
	blob, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	path := filepath.Join(dir, name+".exe")
	if err := copyFile(exe, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+sidecarSuffix, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
