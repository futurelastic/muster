//go:build windows

package modclient_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/delivery/modclient"
)

// A module the Users group can modify is refused with a "writable by others"
// reason that names the class; one nobody but the owner (and the machine's own
// accounts) can write is found (#310).
func TestDiscover_RefusesAModuleOthersCanWrite(t *testing.T) {
	dir := t.TempDir()
	good := writeModule(t, dir, "fine.exe", 0o755)
	loose := writeModule(t, dir, "loose.exe", 0o755)

	// Make the good one explicitly tight, so the test does not depend on what
	// the runner's temporary directory grants.
	user := os.Getenv("USERNAME")
	if out, err := exec.Command("icacls", good, "/inheritance:r", "/grant:r", user+":(F)", "/grant:r", "*S-1-5-18:(F)").CombinedOutput(); err != nil {
		t.Fatalf("icacls on the good module: %v\n%s", err, out)
	}
	if out, err := exec.Command("icacls", loose, "/grant", "*S-1-5-32-545:(M)").CombinedOutput(); err != nil {
		t.Fatalf("icacls on the loose module: %v\n%s", err, out)
	}

	found, problems := modclient.Discover(dir, []string{"fine.exe", "loose.exe"})
	if len(found) != 1 || found[0].Name != "fine.exe" {
		t.Fatalf("found = %+v, want only fine.exe", found)
	}
	p, ok := problemFor(problems, "loose.exe")
	if !ok || !strings.Contains(p.Reason, "writable by others") || !strings.Contains(p.Reason, "Users") {
		t.Errorf("loose.exe: problem = %+v, want a \"writable by others (Users)\" refusal", p)
	}
}
