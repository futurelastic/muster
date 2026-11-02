package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #392: a session whose configuration is only what the create names. The
// agent CLI takes two switches for it, and the driver passes them or refuses.

func TestIsolateConfigurationPassesTheNarrowingFlags(t *testing.T) {
	servers := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(servers, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "given", Cwd: "/work/x", IsolateConfiguration: true,
		McpConfig: []fleet.AbsolutePath{fleet.AbsolutePath(servers)},
	})
	agent := agentArgv(argv)
	if got, ok := flagValue(agent, "--setting-sources"); !ok || got != "project" {
		t.Errorf("--setting-sources = %q (present=%v), want project: %v", got, ok, agent)
	}
	if !containsArg(agent, "--strict-mcp-config") {
		t.Errorf("--strict-mcp-config missing: %v", agent)
	}
	// What the create named still reaches the agent: the narrowing must not eat it.
	if got, ok := flagValue(agent, "--mcp-config"); !ok || got != servers {
		t.Errorf("--mcp-config = %q (present=%v), want the named file: %v", got, ok, agent)
	}
}

// Without the field the argv is what it always was: nothing changes for a caller
// that never heard of it.
func TestWithoutIsolateConfigurationTheArgvIsUnchanged(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "inherit", Cwd: "/work/x"})
	agent := agentArgv(argv)
	for _, a := range agent {
		if a == "--setting-sources" || a == "--strict-mcp-config" {
			t.Errorf("%s appeared with no isolateConfiguration: %v", a, agent)
		}
	}
}

// The create's own settings are a flag layer, not a settings source, so they ride
// along; and the agent flag is passed through for the CLI to resolve.
func TestIsolateConfigurationKeepsTheCreatesOwnSettingsAndAgent(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "given-agent", Cwd: "/work/x", IsolateConfiguration: true,
		Agent: "triage", PermissionMode: fleet.PermissionModeBypass,
		Settings: []byte(`{"crossSessionInbound":"accept"}`),
	})
	agent := agentArgv(argv)
	if got, ok := flagValue(agent, "--settings"); !ok || got != `{"crossSessionInbound":"accept"}` {
		t.Errorf("--settings = %q (present=%v): %v", got, ok, agent)
	}
	if got, ok := flagValue(agent, "--agent"); !ok || got != "triage" {
		t.Errorf("--agent = %q (present=%v): %v", got, ok, agent)
	}
}

func TestDefaultBuilderDeclaresIsolatesConfiguration(t *testing.T) {
	if !New("testbox").Capabilities().IsolatesConfiguration {
		t.Error("the default builder passes the flags, so the driver must say it can")
	}
}

// A caller's own builder is not known to pass the flags, so the driver neither
// claims the capability nor starts a session it cannot confine.
func TestACustomBuilderDeclinesAndRefusesIsolateConfiguration(t *testing.T) {
	custom := func(spec fleet.SessionSpec, contextFile string) []string { return []string{"agent"} }
	d := New("testbox", WithCommandBuilder(custom))
	if d.Capabilities().IsolatesConfiguration {
		t.Error("a custom builder cannot be promised to pass the flags, yet the capability is declared")
	}
	_, err := d.Create(context.Background(), fleet.Request{}, "key-1", fleet.SessionSpec{Cwd: "/work/x", IsolateConfiguration: true})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("Create = %v, want an ErrorUnsupported", err)
	}
	if !strings.Contains(fe.Message, "configuration") {
		t.Errorf("message %q does not say what was refused", fe.Message)
	}
	// The harness wraps the real builder and says so.
	if !New("testbox", WithCommandBuilder(custom), withClaudeArgv()).Capabilities().IsolatesConfiguration {
		t.Error("withClaudeArgv did not restore the capability")
	}
}
