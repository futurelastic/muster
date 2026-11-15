package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #487: an optional sessionEnv entry whose file is there but unusable is
// left out of the session, and every create says so — the opencode runtime's
// own config variable is the case that cost hours. A file that does not exist
// says nothing.
func TestOptionalSessionEnvWithAnUnusableFileIsLoggedOnEveryCreate(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "config.json")
	if err := os.WriteFile(bad, []byte("{\n  \"agent\": {}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "never-written")

	var mu sync.Mutex
	var lines []string
	d, _ := newIsolatedDriver(t,
		WithSessionEnv([]sessionenv.Entry{
			{Name: "OPENCODE_CONFIG_CONTENT", FromFile: bad},
			{Name: "GONE_VAR", FromFile: gone},
		}),
		WithLogf(func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		}))

	for i := 0; i < 2; i++ {
		env, err := d.sessionEnvFor(fleet.SessionSpec{})
		if err != nil {
			t.Fatalf("an optional entry must not refuse the create: %v", err)
		}
		if _, set := env["OPENCODE_CONFIG_CONTENT"]; set {
			t.Fatalf("the unusable file's value reached the session: %v", env)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("log lines = %q, want exactly one per create", lines)
	}
	for _, l := range lines {
		if !strings.Contains(l, "OPENCODE_CONFIG_CONTENT") || !strings.Contains(l, "newline") {
			t.Errorf("the line must name the entry and the reason: %q", l)
		}
		if strings.Contains(l, bad) || strings.Contains(l, "GONE_VAR") {
			t.Errorf("the line carries the path, or names an absent file: %q", l)
		}
	}
}
