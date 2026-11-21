package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/state"
)

// muster #509: a portable agent definition becomes a plugin directory the default
// agent CLI loads for the one session. The command line carries only paths.

func specDriver(t *testing.T, f *fakeMux, opts ...Option) (*Driver, string) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	all := append([]Option{withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }), WithState(st)}, opts...)
	d := New("testbox", all...)
	return d, st.Dir()
}

func defWithMemory(t *testing.T, index string) (fleet.AgentSpec, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(home, "MEMORY.md")
	if err := os.WriteFile(idx, []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	return fleet.AgentSpec{
		Name: "researcher", Description: "finds things out: carefully", Prompt: "UNIQUE-PROMPT you are the researcher",
		Instructions: []string{"UNIQUE-RULE-ONE", "UNIQUE-RULE-TWO"},
		Knowledge:    &fleet.AgentKnowledge{Index: fleet.AbsolutePath(idx), Home: fleet.AbsolutePath(home)},
	}, idx
}

func readAgentFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return string(b)
}

func TestAgentSpec_TheCommandLineCarriesOnlyPathsAndTheNameInTheNamespace(t *testing.T) {
	f := twoSessions()
	d, stateDir := specDriver(t, f)
	def, _ := defWithMemory(t, "UNIQUE-INDEX-LINE\n")
	if _, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "fresh", Cwd: "/work/x", AgentSpec: &def, Model: "opus"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	argv := newSessionArgv(f)
	agent := agentArgv(argv)
	dir, ok := flagValue(agent, "--plugin-dir")
	if !ok {
		t.Fatalf("no --plugin-dir in %v", agent)
	}
	if !strings.HasPrefix(dir, filepath.Join(stateDir, agentSpecDirName)) {
		t.Errorf("--plugin-dir %q is not under the state directory's agentspec directory", dir)
	}
	if got, _ := flagValue(agent, "--agent"); got != "muster-session:researcher" {
		t.Errorf("--agent = %q, want the name in the plugin's own namespace", got)
	}
	if got, _ := flagValue(agent, "--add-dir"); got != string(def.Knowledge.Home) {
		t.Errorf("--add-dir = %q, want the knowledge home", got)
	}
	if got, _ := flagValue(agent, "--model"); got != "opus" {
		t.Errorf("--model = %q, want the model hint", got)
	}
	// Nothing of the definition's text is on any command line (§5.3).
	var lines []string
	for _, c := range f.callsSnapshot() {
		lines = append(lines, strings.Join(c, " "))
	}
	all := strings.Join(lines, "\n")
	for _, secret := range []string{"UNIQUE-PROMPT", "UNIQUE-RULE", "UNIQUE-INDEX-LINE", "finds things out"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q reached a command line:\n%s", secret, all)
		}
	}

	// The plugin names its namespace, and the agent file holds the definition.
	if m := readAgentFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json")); !strings.Contains(m, `"name":"muster-session"`) {
		t.Errorf("manifest = %s", m)
	}
	md := readAgentFile(t, filepath.Join(dir, "agents", "researcher.md"))
	for _, want := range []string{
		"name: researcher", `description: "finds things out: carefully"`,
		"UNIQUE-PROMPT you are the researcher", "UNIQUE-RULE-ONE", "UNIQUE-RULE-TWO",
		"Your memory home is `" + string(def.Knowledge.Home) + "`", "UNIQUE-INDEX-LINE",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the agent file lacks %q:\n%s", want, md)
		}
	}
	if strings.Index(md, "UNIQUE-PROMPT") > strings.Index(md, "UNIQUE-RULE-ONE") ||
		strings.Index(md, "UNIQUE-RULE-ONE") > strings.Index(md, "UNIQUE-RULE-TWO") ||
		strings.Index(md, "UNIQUE-RULE-TWO") > strings.Index(md, "UNIQUE-INDEX-LINE") {
		t.Errorf("the prompt, the instructions in order, then the memory:\n%s", md)
	}
	if strings.Contains(md, "model:") {
		t.Errorf("the model is carried by --model, not the file:\n%s", md)
	}
	for _, p := range []string{filepath.Join(dir, "agents", "researcher.md"), filepath.Join(dir, ".claude-plugin", "plugin.json")} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want owner-only", p, info, err)
		}
	}
}

func TestAgentSpec_TheIndexIsReadLiveAtEveryCreate(t *testing.T) {
	f := twoSessions()
	d, _ := specDriver(t, f)
	def, idx := defWithMemory(t, "first line\n")
	create := func(key, name string) string {
		if _, err := d.Create(context.Background(), testCaller, key, fleet.SessionSpec{Name: name, Cwd: "/work/x", AgentSpec: &def}); err != nil {
			t.Fatalf("create: %v", err)
		}
		dir, _ := flagValue(agentArgv(lastNewSession(f)), "--plugin-dir")
		return readAgentFile(t, filepath.Join(dir, "agents", "researcher.md"))
	}
	a := create("k1", "one")
	if err := os.WriteFile(idx, []byte("second line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := create("k2", "two")
	if !strings.Contains(a, "first line") || strings.Contains(a, "second line") {
		t.Errorf("the first session's definition:\n%s", a)
	}
	if !strings.Contains(b, "second line") || strings.Contains(b, "first line") {
		t.Errorf("the second session did not get the index as it was then:\n%s", b)
	}
}

func lastNewSession(f *fakeMux) []string {
	var last []string
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && c[0] == "new-session" {
			last = c
		}
	}
	return last
}

func TestAgentSpec_ADefinitionWithNothingButANameStillHasABody(t *testing.T) {
	md := agentMarkdown(fleet.AgentSpec{Name: "bare"}, nil)
	if !strings.Contains(md, "You are bare.") || !strings.Contains(md, `description: "Agent bare"`) {
		t.Errorf("agent file:\n%s", md)
	}
}

func TestAgentSpec_Refusals(t *testing.T) {
	def, _ := defWithMemory(t, "x")
	missing := def
	missing.Knowledge = &fleet.AgentKnowledge{Index: fleet.AbsolutePath(filepath.Join(string(def.Knowledge.Home), "absent.md")), Home: def.Knowledge.Home}
	bad := fleet.AgentSpec{Name: "Bad Name"}
	cases := []struct {
		name string
		spec fleet.SessionSpec
		kind fleet.ErrorKind
		want string
	}{
		{"agent and agentSpec", fleet.SessionSpec{Agent: "x", AgentSpec: &def}, fleet.ErrorInvalid, "mutually exclusive"},
		{"a malformed definition", fleet.SessionSpec{AgentSpec: &bad}, fleet.ErrorInvalid, "agentSpec.name"},
		{"an index that cannot be read", fleet.SessionSpec{AgentSpec: &missing}, fleet.ErrorInvalid, "knowledge.index cannot be read"},
	}
	for i, c := range cases {
		f := twoSessions()
		d, stateDir := specDriver(t, f)
		c.spec.Name, c.spec.Cwd = "refused", "/work/x"
		_, err := d.Create(context.Background(), testCaller, "k-"+string(rune('a'+i)), c.spec)
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != c.kind || !strings.Contains(fe.Message, c.want) {
			t.Errorf("%s: %v, want %s naming %q", c.name, err, c.kind, c.want)
		}
		if newSessionArgv(f) != nil {
			t.Errorf("%s: the session was started anyway", c.name)
		}
		if entries, _ := os.ReadDir(filepath.Join(stateDir, agentSpecDirName)); len(entries) != 0 {
			t.Errorf("%s: a refused create left %v behind", c.name, entries)
		}
	}
}

func TestAgentSpec_ADriverThatCannotTranslateRefusesWithTheReason(t *testing.T) {
	def, _ := defWithMemory(t, "x")
	spec := fleet.SessionSpec{Name: "n", Cwd: "/work/x", AgentSpec: &def}
	check := func(label string, d *Driver) {
		t.Helper()
		_, err := d.Create(context.Background(), testCaller, "k", spec)
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || fe.Reason != fleet.ReasonAgentSpecUnsupported {
			t.Fatalf("%s: err = %v, want unsupported with reason agent-spec-unsupported", label, err)
		}
		if got := d.Capabilities().SupportsAgentSpec; got == nil || *got {
			t.Errorf("%s: declares supportsAgentSpec %v, want a declared false", label, got)
		}
	}
	// No state directory to render into.
	f := twoSessions()
	check("no state directory", New("testbox", withExec(f.exec), withNonce(func() string { return testNonce })))
	if newSessionArgv(f) != nil {
		t.Error("a refused create started a session")
	}
	// A replacement builder is not known to pass --plugin-dir.
	f2 := twoSessions()
	d, _ := specDriver(t, f2, WithCommandBuilder(func(fleet.SessionSpec, string) []string { return []string{"agent"} }))
	check("a replacement builder", d)
}

func TestAgentSpec_TheCapabilityIsDeclaredWhereTheDriverTranslates(t *testing.T) {
	d, _ := specDriver(t, twoSessions())
	if got := d.Capabilities().SupportsAgentSpec; got == nil || !*got {
		t.Errorf("supportsAgentSpec = %v, want a declared true", got)
	}
}

func TestAgentSpec_ASessionThatNeverStartsLeavesNothingBehind(t *testing.T) {
	f := twoSessions()
	f.failCreate = true
	d, stateDir := specDriver(t, f)
	def, _ := defWithMemory(t, "x")
	if _, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{Name: "doomed", Cwd: "/work/x", AgentSpec: &def}); err == nil {
		t.Fatal("the create succeeded although the multiplexer refused it")
	}
	if entries, _ := os.ReadDir(filepath.Join(stateDir, agentSpecDirName)); len(entries) != 0 {
		t.Errorf("a create that did not start left %v behind", entries)
	}
}

func TestAgentSpec_CloseRemovesTheSessionsDefinition(t *testing.T) {
	f := twoSessions()
	d, stateDir := specDriver(t, f)
	def, _ := defWithMemory(t, "x")
	if _, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{Name: "closeme", Cwd: "/work/x", AgentSpec: &def}); err != nil {
		t.Fatal(err)
	}
	dir := agentSpecDirFor(filepath.Join(stateDir, agentSpecDirName), sessionNameOf(newSessionArgv(f)))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the definition is not where the session's name says: %v", err)
	}
	d.dropAgentSpec(sessionNameOf(newSessionArgv(f)))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the definition outlived its session: %v", err)
	}
}

func TestAgentSpec_TheSweepRemovesOnlyWhatNoLiveSessionOwnsAndIsPastTheGrace(t *testing.T) {
	f := twoSessions()
	now := time.Unix(1785760000, 0)
	d, stateDir := specDriver(t, f, withClock(func() time.Time { return now }))
	root := filepath.Join(stateDir, agentSpecDirName)
	mk := func(session string, age time.Duration) string {
		dir := agentSpecDirFor(root, session)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	live := mk("live", time.Hour)
	young := mk("young", time.Minute)
	orphan := mk("orphan", time.Hour)
	d.sweepAgentSpecs([]paneRow{{session: "live"}})
	for name, c := range map[string]struct {
		dir  string
		kept bool
	}{"live": {live, true}, "young": {young, true}, "orphan": {orphan, false}} {
		_, err := os.Stat(c.dir)
		if (err == nil) != c.kept {
			t.Errorf("%s directory: kept = %v, want %v", name, err == nil, c.kept)
		}
	}
}

func TestAgentSpec_TheListingReportsTheDefinitionsNameNotTheNamespace(t *testing.T) {
	for in, want := range map[string]string{
		"claude --agent muster-session:researcher --model opus": "researcher",
		"claude --agent=muster-session:researcher":              "researcher",
		"claude --agent plain":                                  "plain",
	} {
		got, ok := agentFromArgs(in)
		if !ok || got != want {
			t.Errorf("agentFromArgs(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestAgentSpec_ASandboxRequestGainsThePluginDirectoryAndTheHome(t *testing.T) {
	def, _ := defWithMemory(t, "x")
	in := &fleet.SandboxSpec{ReadPaths: []fleet.AbsolutePath{"/srv/a"}, WritePaths: []fleet.AbsolutePath{"/srv/w"}}
	out := withKnowledgeGrant(in, def, "/state/agentspec/s-1")
	if len(in.ReadPaths) != 1 || len(in.WritePaths) != 1 {
		t.Fatalf("the caller's request was changed: %+v", in)
	}
	has := func(list []fleet.AbsolutePath, p string) bool {
		for _, x := range list {
			if string(x) == p {
				return true
			}
		}
		return false
	}
	if !has(out.ReadPaths, "/state/agentspec/s-1") || !has(out.ReadPaths, string(def.Knowledge.Home)) || has(out.WritePaths, string(def.Knowledge.Home)) {
		t.Errorf("read-only home: %+v", out)
	}
	def.Knowledge.Write = true
	out = withKnowledgeGrant(in, def, "/state/agentspec/s-1")
	if !has(out.WritePaths, string(def.Knowledge.Home)) || has(out.ReadPaths, string(def.Knowledge.Home)) {
		t.Errorf("writable home: %+v", out)
	}
	// Idempotent with what the service already added.
	again := withKnowledgeGrant(out, def, "/state/agentspec/s-1")
	if len(again.ReadPaths) != len(out.ReadPaths) || len(again.WritePaths) != len(out.WritePaths) {
		t.Errorf("a second grant added the paths again: %+v", again)
	}
}
