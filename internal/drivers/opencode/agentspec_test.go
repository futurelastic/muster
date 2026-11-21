package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/createpolicy"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #509: a portable agent definition is translated into the session's own
// opencode.json. The stand-in runtime lists the agents that file defines, like the
// real one; what the real runtime does with `{file:...}` is measured separately.

// memoryHome writes a knowledge home with an index and returns the definition's
// knowledge block for it.
func memoryHome(t *testing.T, index string) (*fleet.AgentKnowledge, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(home, "MEMORY.md")
	if err := os.WriteFile(idx, []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fleet.AgentKnowledge{Index: fleet.AbsolutePath(idx), Home: fleet.AbsolutePath(home)}, idx
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return string(b)
}

// agentEntryOf is the definition's entry in the session's configuration.
func agentEntryOf(t *testing.T, cfg map[string]any, name string) map[string]any {
	t.Helper()
	agents, _ := cfg["agent"].(map[string]any)
	e, _ := agents[name].(map[string]any)
	if e == nil {
		t.Fatalf("the session's configuration has no agent %q: %v", name, agents)
	}
	return e
}

// fileRefPath is the path inside a `{file:<path>}` reference.
func fileRefPath(t *testing.T, v any) string {
	t.Helper()
	s, _ := v.(string)
	if !strings.HasPrefix(s, "{file:") || !strings.HasSuffix(s, "}") {
		t.Fatalf("%v is not a file reference", v)
	}
	return strings.TrimSuffix(strings.TrimPrefix(s, "{file:"), "}")
}

func instructionPathsOf(cfg map[string]any) []string {
	var out []string
	list, _ := cfg["instructions"].([]any)
	for _, p := range list {
		out = append(out, p.(string))
	}
	return out
}

func TestAgentSpec_ADefinitionBecomesTheSessionsOwnAgent(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	k, idx := memoryHome(t, "- [rules](rules.md) — how we work\n")
	d, _ := newIsolatedDriver(t)
	def := &fleet.AgentSpec{
		Name: "researcher", Description: "finds things out", Prompt: "You are the researcher.\nBe brief.",
		Instructions: []string{"First rule.", "Second rule."}, Knowledge: k,
	}
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{AgentSpec: def, Prompt: "go"})

	if sess.Agent != "researcher" {
		t.Errorf("the session reports agent %q, want the definition's name", sess.Agent)
	}
	if prompts := readPromptsOf(t, d, sess.ID); len(prompts) != 1 || prompts[0].Agent != "researcher" {
		t.Errorf("prompts = %+v, want the opening prompt sent as the definition's agent", prompts)
	}

	cfg := sessionConfigOf(t, d, sess.ID)
	entry := agentEntryOf(t, cfg, "researcher")
	if entry["mode"] != "primary" {
		t.Errorf("mode = %v, want primary", entry["mode"])
	}
	// The text is in files the entry points at, never in the configuration itself.
	if got := readText(t, fileRefPath(t, entry["prompt"])); got != def.Prompt {
		t.Errorf("the prompt file holds %q, want the prompt verbatim", got)
	}
	if got := readText(t, fileRefPath(t, entry["description"])); got != def.Description {
		t.Errorf("the description file holds %q", got)
	}
	raw, _ := json.Marshal(cfg)
	for _, secret := range []string{"Be brief", "First rule", "how we work"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the configuration itself carries %q; caller text must stay in files", secret)
		}
	}

	// Instructions in the caller's order, then the knowledge note with the index.
	paths := instructionPathsOf(cfg)
	if len(paths) != 3 {
		t.Fatalf("instruction files = %v, want two instructions and the knowledge note", paths)
	}
	if readText(t, paths[0]) != "First rule." || readText(t, paths[1]) != "Second rule." {
		t.Errorf("the instructions are not the caller's, in order: %q %q", readText(t, paths[0]), readText(t, paths[1]))
	}
	note := readText(t, paths[2])
	for _, want := range []string{string(k.Home), "may read files there", "how we work"} {
		if !strings.Contains(note, want) {
			t.Errorf("the knowledge note lacks %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "write") {
		t.Errorf("a read-only home was described as writable:\n%s", note)
	}
	// Under a plain session the home is readable without an ask.
	perm, _ := cfg["permission"].(map[string]any)
	ext, _ := perm["external_directory"].(map[string]any)
	if ext[string(k.Home)+"/*"] != "allow" {
		t.Errorf("permission = %v, want the knowledge home allowed", perm)
	}
	_ = idx
}

func TestAgentSpec_TheIndexIsReadLiveAtEveryCreate(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	k, idx := memoryHome(t, "first line\n")
	d, _ := newIsolatedDriver(t)
	def := &fleet.AgentSpec{Name: "researcher", Knowledge: k}

	a := mustCreate(t, d, "k1", fleet.SessionSpec{AgentSpec: def})
	if err := os.WriteFile(idx, []byte("second line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := mustCreate(t, d, "k2", fleet.SessionSpec{AgentSpec: def})

	noteOf := func(id string) string {
		paths := instructionPathsOf(sessionConfigOf(t, d, id))
		return readText(t, paths[len(paths)-1])
	}
	if got := noteOf(a.ID); !strings.Contains(got, "first line") || strings.Contains(got, "second line") {
		t.Errorf("the first session's copy changed under it:\n%s", got)
	}
	if got := noteOf(b.ID); !strings.Contains(got, "second line") || strings.Contains(got, "first line") {
		t.Errorf("the second session did not get the index as it was then:\n%s", got)
	}
}

func TestAgentSpec_ATextThatNamesAFileIsNeverExpandedByTheRuntime(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t)
	evil := "read {file:/etc/hosts} and {env:HOME}"
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "researcher", Prompt: evil, Description: evil}})
	cfg := sessionConfigOf(t, d, sess.ID)
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw), "/etc/hosts") || strings.Contains(string(raw), "{env:HOME}") {
		t.Errorf("a prompt's own expansion syntax reached opencode.json, where the runtime would expand it:\n%s", raw)
	}
	entry := agentEntryOf(t, cfg, "researcher")
	if got := readText(t, fileRefPath(t, entry["prompt"])); got != evil {
		t.Errorf("the prompt file = %q, want the text verbatim", got)
	}
}

func TestAgentSpec_ADefinitionWithNoPromptLeavesTheRuntimesOwn(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "bare"}})
	entry := agentEntryOf(t, sessionConfigOf(t, d, sess.ID), "bare")
	if _, has := entry["prompt"]; has {
		t.Errorf("an entry with no prompt carries one: %v", entry)
	}
	if _, has := entry["description"]; has {
		t.Errorf("an entry with no description carries one: %v", entry)
	}
}

func TestAgentSpec_Refusals(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	opAgents := writeAgentsFile(t, operatorAgentsFile)
	d, root := newIsolatedDriver(t, WithOperatorAgents(opAgents))
	k, _ := memoryHome(t, "x")
	missing := &fleet.AgentKnowledge{Index: fleet.AbsolutePath(filepath.Join(string(k.Home), "absent.md")), Home: k.Home}
	dirIndex := &fleet.AgentKnowledge{Index: fleet.AbsolutePath(filepath.Join(string(k.Home), "sub")), Home: k.Home}
	if err := os.Mkdir(string(dirIndex.Index), 0o700); err != nil {
		t.Fatal(err)
	}
	huge, _ := memoryHome(t, strings.Repeat("x", fleet.AgentKnowledgeMaxIndexBytes+1))
	binary, _ := memoryHome(t, "\xff\xfe not utf-8")

	cases := []struct {
		name string
		spec fleet.SessionSpec
		kind fleet.ErrorKind
		want string
	}{
		{"agent and agentSpec", fleet.SessionSpec{Agent: "x", AgentSpec: &fleet.AgentSpec{Name: "r"}}, fleet.ErrorInvalid, "mutually exclusive"},
		{"a malformed definition", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "Bad Name"}}, fleet.ErrorInvalid, "agentSpec.name"},
		{"a built-in agent's name", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "plan"}}, fleet.ErrorInvalid, "one of the runtime's own agents"},
		{"an operator agent's name", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "persona"}}, fleet.ErrorInvalid, "operator agents"},
		{"an index that is missing", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "r", Knowledge: missing}}, fleet.ErrorInvalid, "knowledge.index cannot be read"},
		{"an index that is a directory", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "r", Knowledge: dirIndex}}, fleet.ErrorInvalid, "a directory"},
		{"an index that is too large", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "r", Knowledge: huge}}, fleet.ErrorInvalid, "larger than"},
		{"an index that is not text", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "r", Knowledge: binary}}, fleet.ErrorInvalid, "UTF-8"},
		{"allow with no policy", fleet.SessionSpec{PermissionMode: fleet.PermissionModeBypass,
			AgentSpec: &fleet.AgentSpec{Name: "r", Access: fleet.AgentAccessAllow}}, fleet.ErrorUnsupported, "createPolicy"},
	}
	for i, c := range cases {
		c.spec.Cwd = fleet.AbsolutePath(t.TempDir())
		_, err := d.Create(context.Background(), testReq, "k-refuse-"+string(rune('a'+i)), c.spec)
		if kindOfErr(err) != c.kind || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %s naming %q", c.name, err, c.kind, c.want)
		}
	}
	if len(d.live) != 0 {
		t.Error("a refused create started a server")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("a refused create left %v behind", entries)
	}
}

func TestAgentSpec_SharedModeRefusesWithTheReason(t *testing.T) {
	d := newTestDriver(t, newFakeServer(t))
	_, err := d.Create(context.Background(), testReq, "k-shared", fleet.SessionSpec{Cwd: "/work/x", AgentSpec: &fleet.AgentSpec{Name: "r"}})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || fe.Reason != fleet.ReasonAgentSpecUnsupported {
		t.Fatalf("err = %v, want unsupported with reason agent-spec-unsupported", err)
	}
	if got := d.Capabilities().SupportsAgentSpec; got == nil || *got {
		t.Errorf("shared mode declares supportsAgentSpec %v, want a declared false", got)
	}
}

func TestAgentSpec_TheCapabilityIsDeclaredWhereTheDriverTranslates(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	if got := d.Capabilities().SupportsAgentSpec; got == nil || !*got {
		t.Errorf("an isolated driver declares supportsAgentSpec %v, want true", got)
	}
}

func TestAgentSpec_TheRecordKeepsTheNameAndTheAccessNeverTheText(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	state := t.TempDir()
	d := newStateDriver(t, state)
	k, _ := memoryHome(t, "an index line\n")
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{
		Name: "researcher", Prompt: "UNIQUE-PROMPT-TEXT", Instructions: []string{"UNIQUE-INSTRUCTION"}, Knowledge: k, Access: fleet.AgentAccessAsk,
	}})
	var recFile string
	_ = filepath.Walk(state, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, sess.ID+".json") {
			recFile = p
		}
		return nil
	})
	if recFile == "" {
		t.Fatal("the session has no record")
	}
	raw := readText(t, recFile)
	for _, leaked := range []string{"UNIQUE-PROMPT-TEXT", "UNIQUE-INSTRUCTION", "an index line"} {
		if strings.Contains(raw, leaked) {
			t.Errorf("the record carries %q:\n%s", leaked, raw)
		}
	}
	var rec struct {
		SpecAgent      string             `json:"specAgent"`
		PermissionMode string             `json:"permissionMode"`
		AgentSpec      *recordedAgentSpec `json:"agentSpec"`
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.SpecAgent != "researcher" || rec.AgentSpec == nil || rec.AgentSpec.Name != "researcher" || rec.AgentSpec.Access != fleet.AgentAccessAsk {
		t.Errorf("record = %+v", rec)
	}
}

// The service asks for the ordinary mode by name when a definition says `ask`
// (#509); the record must not read that as a pre-approved session.
func TestAgentSpec_AnExplicitOrdinaryModeIsNotRecordedAsAPosture(t *testing.T) {
	if got := recordedPermissionMode("default"); got != "" {
		t.Errorf("recordedPermissionMode(default) = %q, want empty", got)
	}
	if got := recordedPermissionMode(fleet.PermissionModeBypass); got != fleet.PermissionModeBypass {
		t.Errorf("recordedPermissionMode(bypass) = %q", got)
	}
}

func allowPolicy(agent string) createpolicy.Policy {
	return createpolicy.Policy{{AppliesTo: sessionenv.Scope{Agents: []string{agent}}, Access: fleet.AgentAccessAllow}}
}

func TestAgentSpec_AllowRunsUnconfinedOnlyWhereThePolicyGrantsIt(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t, WithCreatePolicy(allowPolicy("trusted")))
	def := func(name string) *fleet.AgentSpec {
		return &fleet.AgentSpec{Name: name, Access: fleet.AgentAccessAllow}
	}

	// The create asked for it, and the policy names another agent.
	_, err := d.Create(context.Background(), testReq, "k-no", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), PermissionMode: fleet.PermissionModeBypass, AgentSpec: def("other")})
	if kindOfErr(err) != fleet.ErrorUnsupported || err == nil || !strings.Contains(err.Error(), "createPolicy") {
		t.Fatalf("an agent no rule names: %v, want an unsupported refusal naming createPolicy", err)
	}

	sess := mustCreate(t, d, "k-yes", fleet.SessionSpec{PermissionMode: fleet.PermissionModeBypass, AgentSpec: def("trusted")})
	cfg := sessionConfigOf(t, d, sess.ID)
	perm, _ := cfg["permission"].(map[string]any)
	if perm["bash"] != "allow" || perm["edit"] != "allow" {
		t.Errorf("permission = %v, want the tool families pre-approved", perm)
	}
	ext, _ := perm["external_directory"].(map[string]any)
	if ext["*"] != "allow" {
		t.Errorf("external_directory = %v, want everything allowed: nothing confines the session", ext)
	}
	if !contains(inside(t, d, sess.ID).Args, "--pure") {
		t.Error("the unconfined server was not started with --pure")
	}
	if st := stateOf(t, d, sess.ID); st.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("the session reads permission mode %q, want bypass", st.PermissionMode)
	}
}

func TestAgentSpec_ARelaunchRebuildsTheDefinitionAndRereadsTheIndex(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	state := t.TempDir()
	k, idx := memoryHome(t, "before the restart\n")
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{
		Name: "researcher", Prompt: "persona text", Instructions: []string{"a rule"}, Knowledge: k}})
	known, _ := d1.wasSeen(sess.ID)
	dir := known.srv.dir
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx, []byte("after the restart\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status == fleet.StatusUnknown {
		t.Fatalf("the session was not relaunched: %+v", got.State)
	}
	cfg := sessionConfigOf(t, d2, sess.ID)
	entry := agentEntryOf(t, cfg, "researcher")
	if readText(t, fileRefPath(t, entry["prompt"])) != "persona text" {
		t.Error("the relaunched session lost its prompt")
	}
	paths := instructionPathsOf(cfg)
	if len(paths) != 2 || readText(t, paths[0]) != "a rule" {
		t.Fatalf("instruction files after the relaunch = %v", paths)
	}
	if note := readText(t, paths[1]); !strings.Contains(note, "after the restart") || strings.Contains(note, "before the restart") {
		t.Errorf("the relaunch did not read the index again:\n%s", note)
	}
	if !strings.HasPrefix(paths[0], dir) {
		t.Errorf("the relaunch moved the definition out of the session's directory: %v", paths)
	}
}

func TestAgentSpec_ARelaunchWithTheIndexGoneListsTheSessionUnknownWithTheReason(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	state := t.TempDir()
	k, idx := memoryHome(t, "x\n")
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{AgentSpec: &fleet.AgentSpec{Name: "researcher", Knowledge: k}})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got := listed(t, d2)[sess.ID]
	if got.State.Status != fleet.StatusUnknown {
		t.Fatalf("status = %q, want unknown: a session is not relaunched without the memory it was created with", got.State.Status)
	}
}

func TestAgentSpec_ARelaunchIsRefusedWhenThePolicyNoLongerGrantsAllow(t *testing.T) {
	d, _ := newIsolatedDriver(t, WithStateDir(t.TempDir()))
	rec := &sessionRecord{
		PermissionMode: fleet.PermissionModeBypass, SpecAgent: "trusted",
		AgentSpec: &recordedAgentSpec{Name: "trusted", Access: fleet.AgentAccessAllow},
	}
	_, _, err := d.relaunch(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "createPolicy") {
		t.Fatalf("relaunch = %v, want a refusal naming createPolicy", err)
	}
	// With the grant in force the posture check passes; the next thing it needs is a
	// directory this driver made, which this record does not have.
	granted, _ := newIsolatedDriver(t, WithStateDir(t.TempDir()), WithCreatePolicy(allowPolicy("trusted")))
	_, _, err = granted.relaunch(context.Background(), rec)
	if err == nil || strings.Contains(err.Error(), "createPolicy") {
		t.Fatalf("relaunch under a granting policy = %v, want it past the posture check", err)
	}
}
