package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #396: agents the operator defines once per machine reach every isolated
// session. The file's shape is unit-tested on its own; what the sessions get is
// tested through the stand-in runtime (fakeopencode_test.go), which, like the
// real one, lists the agents its own opencode.json defines and does not start on
// a definition it rejects. How a project's agent of the same name meets these
// is a fact about the real runtime and is pinned by the gated integration test.

const operatorAgentsFile = `{
  "agent": {
    "persona": {"description": "The operator's persona", "mode": "primary", "prompt": "You are the persona."},
    "reviewer": {"mode": "subagent"}
  }
}`

func writeAgentsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sessionConfigOf is the opencode.json the driver wrote for a session.
func sessionConfigOf(t *testing.T, d *Driver, id string) map[string]any {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok || known.srv == nil {
		t.Fatalf("session %s is not running here", id)
	}
	raw, err := os.ReadFile(filepath.Join(known.srv.dir, permissionConfigRel))
	if err != nil {
		t.Fatalf("reading the session's config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the session's config is not JSON: %v", err)
	}
	return cfg
}

// promptBody is what a session sent the runtime, as the stand-in recorded it.
type promptBody struct {
	Agent string `json:"agent"`
}

// readPromptsOf is every prompt the session's runtime was sent, oldest first.
func readPromptsOf(t *testing.T, d *Driver, id string) []promptBody {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok || known.srv == nil {
		t.Fatalf("session %s is not running here", id)
	}
	raw, err := os.ReadFile(filepath.Join(known.srv.dir, "xdg", "data", "fake-prompts.jsonl"))
	if err != nil {
		t.Fatalf("reading what the session sent: %v", err)
	}
	var out []promptBody
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var p promptBody
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("a prompt is not JSON: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func agentNamesIn(cfg map[string]any) []string {
	m, _ := cfg["agent"].(map[string]any)
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestOperatorAgents_TheFileIsParsedAsWritten(t *testing.T) {
	got, err := parseOperatorAgents([]byte(operatorAgentsFile))
	if err != nil {
		t.Fatalf("parseOperatorAgents: %v", err)
	}
	if len(got) != 2 || got["persona"] == nil || got["reviewer"] == nil {
		t.Fatalf("agents = %v, want persona and reviewer", got)
	}
	// The definition reaches the runtime exactly as the operator wrote it: this
	// driver neither interprets nor rewrites it.
	var persona map[string]string
	if err := json.Unmarshal(got["persona"], &persona); err != nil {
		t.Fatalf("persona is not the object that was written: %v", err)
	}
	if persona["prompt"] != "You are the persona." || persona["mode"] != "primary" {
		t.Errorf("persona = %v, want the definition unchanged", persona)
	}

	none, err := parseOperatorAgents([]byte(`{"agent": {}}`))
	if err != nil || len(none) != 0 {
		t.Errorf("an empty agent object = %v, %v; want no agents and no error", none, err)
	}
}

func TestOperatorAgents_AFileThatIsNotUsableIsRefusedWithTheReason(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
	}{
		{"not JSON", `agents: persona`, "not a JSON object"},
		{"an array", `[]`, "not a JSON object"},
		{"empty", ``, "not a JSON object"},
		{"nothing at all in it", `{}`, `no "agent" entry`},
		{"agent is not an object", `{"agent": ["persona"]}`, `"agent" entry`},
		{"a definition is not an object", `{"agent": {"persona": "be nice"}}`, `"persona" is not a JSON object`},
		{"a definition is null", `{"agent": {"persona": null}}`, `"persona" is not a JSON object`},
		{"a provider rides along", `{"agent": {}, "enabled_providers": ["x"]}`, `"enabled_providers"`},
		{"a permission rides along", `{"permission": {"bash": "allow"}, "agent": {"p": {}}}`, `"permission"`},
		{"an agent defined twice", `{"agent": {"p": {}, "p": {"mode": "all"}}}`, `"p" appears twice`},
		{"the agent entry twice", `{"agent": {}, "agent": {}}`, `"agent" appears twice`},
		{"a blank name", `{"agent": {"": {}}}`, "must not be empty"},
		{"a padded name", `{"agent": {" p": {}}}`, "must not be empty"},
		{"a control character in a name", "{\"agent\": {\"p\\u0007\": {}}}", "control character"},
		{"something after the object", `{"agent": {}} {}`, "more after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseOperatorAgents([]byte(tc.body))
			if err == nil {
				t.Fatalf("parseOperatorAgents(%q) succeeded; want a refusal", tc.body)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %q, want it to say %q", err, tc.reason)
			}
		})
	}
}

func TestOperatorAgents_AFileThatCannotBeReadIsRefused(t *testing.T) {
	if _, err := readOperatorAgents(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing file was accepted")
	}
	if _, err := readOperatorAgents(t.TempDir()); err == nil {
		t.Error("a directory was accepted")
	}
	big := writeAgentsFile(t, `{"agent": {"p": {"prompt": "`+strings.Repeat("x", operatorAgentsMaxBytes)+`"}}}`)
	_, err := readOperatorAgents(big)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized file: %v, want a refusal naming the bound", err)
	}
}

func TestOperatorAgents_TheConfigCarriesThemOnlyWhenThereAreSome(t *testing.T) {
	dir := t.TempDir()
	if err := writeSessionConfig(dir, false, nil, providerPolicy{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, permissionConfigRel))
	if strings.Contains(string(raw), `"agent"`) {
		t.Errorf("a machine with no operator agents got an agent entry:\n%s", raw)
	}

	agents, err := parseOperatorAgents([]byte(operatorAgentsFile))
	if err != nil {
		t.Fatal(err)
	}
	// Alongside a bypass session's posture and the provider policy, neither moved.
	plan := &sandboxPlan{read: []string{"/work"}}
	if err := writeSessionConfig(dir, true, plan, providerPolicy{}, agents); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	raw, _ = os.ReadFile(filepath.Join(dir, permissionConfigRel))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(agentNamesIn(cfg)) != 2 {
		t.Errorf("agents in the config = %v, want persona and reviewer", agentNamesIn(cfg))
	}
	if _, ok := cfg["permission"]; !ok {
		t.Error("the bypass posture is gone once agents are written")
	}
	if _, ok := cfg["disabled_providers"]; !ok {
		t.Error("the provider policy is gone once agents are written")
	}
}

func TestOperatorAgents_AnIsolatedSessionListsAndRunsAsTheOperatorsAgent(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	file := writeAgentsFile(t, operatorAgentsFile)
	d, _ := newIsolatedDriver(t, WithOperatorAgents(file))

	spec := fleet.SessionSpec{Agent: "persona", Prompt: "go", Cwd: fleet.AbsolutePath(t.TempDir())}
	sess := mustCreate(t, d, "k1", spec)

	// The session's own configuration carries the definition, and the runtime
	// listed it: a create naming an agent it did not list is refused (below).
	cfg := sessionConfigOf(t, d, sess.ID)
	if names := agentNamesIn(cfg); len(names) != 2 {
		t.Errorf("agents in the session's config = %v, want persona and reviewer", names)
	}
	known, _ := d.wasSeen(sess.ID)
	if names, listedOK, err := d.readAgents(context.Background(), known.srv, spec.Cwd); err != nil || !listedOK ||
		!strings.Contains(strings.Join(names, ","), "persona") {
		t.Errorf("the runtime lists %v (listed %v, err %v), want persona among them", names, listedOK, err)
	}
	if prompts := readPromptsOf(t, d, sess.ID); len(prompts) != 1 || prompts[0].Agent != "persona" {
		t.Errorf("prompts = %+v, want the opening prompt sent as persona", prompts)
	}

	// The first turn ran as it: the repeat of the same create reads the runtime's
	// own record and reports the pin honoured.
	raw, _ := json.Marshal([]wireMessage{{Info: wireMessageInfo{Role: "assistant", Agent: "persona"}}})
	if err := os.WriteFile(filepath.Join(known.srv.dir, "xdg", "data", "fake-messages.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := d.Create(context.Background(), testReq, "k1", spec)
	if err != nil {
		t.Fatalf("repeat Create: %v", err)
	}
	if again.Pins == nil || again.Pins.Agent == nil || again.Pins.Agent.Honoured == nil || !*again.Pins.Agent.Honoured {
		t.Errorf("pins.agent = %+v, want honoured: true", again.Pins)
	}
}

func TestOperatorAgents_AnAgentTheFileDoesNotDefineIsStillRefused(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t, WithOperatorAgents(writeAgentsFile(t, operatorAgentsFile)))
	_, err := d.Create(context.Background(), testReq, "k1",
		fleet.SessionSpec{Agent: "perzona", Cwd: fleet.AbsolutePath(t.TempDir())})
	if kindOfErr(err) != fleet.ErrorInvalid || !strings.Contains(err.Error(), `closest: "persona"`) {
		t.Errorf("Create naming a misspelt operator agent = %v, want an invalid refusal naming persona", err)
	}
}

func TestOperatorAgents_EditingTheFileNeedsNoRestart(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	file := writeAgentsFile(t, operatorAgentsFile)
	d, _ := newIsolatedDriver(t, WithOperatorAgents(file))
	cwd := fleet.AbsolutePath(t.TempDir())

	first := mustCreate(t, d, "k1", fleet.SessionSpec{Agent: "persona", Cwd: cwd})
	if _, err := d.Create(context.Background(), testReq, "k2", fleet.SessionSpec{Agent: "newcomer", Cwd: cwd}); err == nil {
		t.Fatal("an agent nobody had defined yet was accepted")
	}

	if err := os.WriteFile(file, []byte(`{"agent": {"newcomer": {"mode": "primary"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second := mustCreate(t, d, "k3", fleet.SessionSpec{Agent: "newcomer", Cwd: cwd})
	if names := agentNamesIn(sessionConfigOf(t, d, second.ID)); len(names) != 1 || names[0] != "newcomer" {
		t.Errorf("the second session's agents = %v, want exactly what the file holds now", names)
	}
	// A session already running keeps the definition it started with: the
	// runtime read it at its own start.
	if names := agentNamesIn(sessionConfigOf(t, d, first.ID)); len(names) != 2 {
		t.Errorf("the first session's agents = %v, want the two it started with", names)
	}
}

func TestOperatorAgents_AFileThatIsNotUsableRefusesTheCreateAndStartsNothing(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone.json") }},
		{"not JSON", func(t *testing.T) string { return writeAgentsFile(t, "persona") }},
		{"carries a provider", func(t *testing.T) string {
			return writeAgentsFile(t, `{"agent": {"persona": {}}, "enabled_providers": ["opencode"]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, root := newIsolatedDriver(t, WithOperatorAgents(tc.path(t)))
			// Naming the agent, and naming none: neither runs on the default agent
			// as though nothing were wrong.
			for _, agent := range []string{"persona", ""} {
				_, err := d.Create(context.Background(), testReq, "k-"+agent,
					fleet.SessionSpec{Agent: fleet.AgentId(agent), Prompt: "go", Cwd: fleet.AbsolutePath(t.TempDir())})
				if kindOfErr(err) != fleet.ErrorUnsupported {
					t.Fatalf("Create(agent %q) = %v, want an unsupported refusal: the operator must repair the file", agent, err)
				}
				if !strings.Contains(err.Error(), "operator") {
					t.Errorf("refusal = %q, want it to say what is wrong and who repairs it", err)
				}
			}
			if left, _ := os.ReadDir(root); len(left) != 0 {
				t.Errorf("a refused create left %d entries under the session root", len(left))
			}
		})
	}
}

func TestOperatorAgents_ADefinitionTheRuntimeRejectsRefusesTheCreate(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, root := newIsolatedDriver(t, WithOperatorAgents(writeAgentsFile(t, `{"agent": {"persona": {"mode": "bogus"}}}`)))
	_, err := d.Create(context.Background(), testReq, "k1",
		fleet.SessionSpec{Agent: "persona", Cwd: fleet.AbsolutePath(t.TempDir())})
	if err == nil || !strings.Contains(err.Error(), "ConfigInvalidError") {
		t.Fatalf("Create = %v, want a refusal carrying the runtime's own name for what it rejected", err)
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Errorf("a refused create left %d entries under the session root", len(left))
	}
}

func TestOperatorAgents_ARelaunchedSessionGetsTheFileAsItIsThen(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	file := writeAgentsFile(t, operatorAgentsFile)
	state := t.TempDir()
	d1 := newStateDriver(t, state, WithOperatorAgents(file))
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Agent: "persona", Prompt: "one"})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(file, []byte(`{"agent": {"persona": {"mode": "primary"}, "added": {}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state, WithOperatorAgents(file))
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusIdle {
		t.Fatalf("after the restart the session is %+v (listed %v), want it running again", got.State, ok)
	}
	if names := agentNamesIn(sessionConfigOf(t, d2, sess.ID)); len(names) != 2 {
		t.Errorf("relaunched agents = %v, want persona and added: the file is read again at a relaunch", names)
	}
}

func TestOperatorAgents_ARelaunchWithAnUnusableFileLeavesTheSessionUnknownWithTheReason(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	file := writeAgentsFile(t, operatorAgentsFile)
	state := t.TempDir()
	d1 := newStateDriver(t, state, WithOperatorAgents(file))
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Agent: "persona"})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state, WithOperatorAgents(file))
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatal("the session vanished instead of being listed with a reason")
	}
	if got.State.Status != fleet.StatusUnknown || !strings.Contains(got.State.Evidence, "operator") {
		t.Errorf("state = %q (%s), want unknown, with the file named as the reason", got.State.Status, got.State.Evidence)
	}
}
