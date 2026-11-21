package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

func validAgentSpec() AgentSpec {
	return AgentSpec{
		Name:         "researcher",
		Description:  "finds things out",
		Prompt:       "You are a researcher.",
		Instructions: []string{"Be brief.", "Cite sources."},
		Knowledge: &AgentKnowledge{
			Index: "/srv/agents/researcher/memory/MEMORY.md",
			Home:  "/srv/agents/researcher/memory",
		},
		Model:  "provider/model-id",
		Access: AgentAccessSandboxed,
	}
}

func TestAgentSpecValidate_Valid(t *testing.T) {
	if err := validAgentSpec().Validate(); err != nil {
		t.Fatalf("a well-formed agent definition was refused: %v", err)
	}
	if err := (AgentSpec{Name: "a"}).Validate(); err != nil {
		t.Fatalf("a name alone is a valid definition: %v", err)
	}
	for _, a := range []AgentAccess{"", AgentAccessAsk, AgentAccessAllow, AgentAccessSandboxed} {
		s := validAgentSpec()
		s.Access = a
		if err := s.Validate(); err != nil {
			t.Fatalf("access %q refused: %v", a, err)
		}
	}
}

func TestAgentSpecValidate_Refusals(t *testing.T) {
	big := strings.Repeat("x", AgentSpecMaxPromptBytes+1)
	cases := []struct {
		name   string
		mutate func(*AgentSpec)
		want   string
	}{
		{"empty name", func(a *AgentSpec) { a.Name = "" }, "agentSpec.name"},
		{"upper-case name", func(a *AgentSpec) { a.Name = "Researcher" }, "agentSpec.name"},
		{"namespace separator in name", func(a *AgentSpec) { a.Name = "ns:researcher" }, "agentSpec.name"},
		{"leading hyphen name", func(a *AgentSpec) { a.Name = "-researcher" }, "agentSpec.name"},
		{"trailing hyphen name", func(a *AgentSpec) { a.Name = "researcher-" }, "agentSpec.name"},
		{"space in name", func(a *AgentSpec) { a.Name = "re searcher" }, "agentSpec.name"},
		{"slash in name", func(a *AgentSpec) { a.Name = "../x" }, "agentSpec.name"},
		{"name too long", func(a *AgentSpec) { a.Name = strings.Repeat("a", AgentSpecMaxNameLen+1) }, "agentSpec.name"},
		{"description too long", func(a *AgentSpec) { a.Description = strings.Repeat("d", AgentSpecMaxDescriptionBytes+1) }, "agentSpec.description"},
		{"description two lines", func(a *AgentSpec) { a.Description = "one\ntwo" }, "one line"},
		{"description control", func(a *AgentSpec) { a.Description = "a\x00b" }, "control"},
		{"prompt too long", func(a *AgentSpec) { a.Prompt = big }, "agentSpec.prompt"},
		{"prompt NUL", func(a *AgentSpec) { a.Prompt = "a\x00b" }, "control"},
		{"33 instructions", func(a *AgentSpec) { a.Instructions = make([]string, AgentSpecMaxInstructions+1) }, "instructions"},
		{"empty instruction", func(a *AgentSpec) { a.Instructions = []string{"ok", "  "} }, "instructions[1]"},
		{"instruction too long", func(a *AgentSpec) { a.Instructions = []string{big} }, "instructions[0]"},
		{"total too long", func(a *AgentSpec) {
			chunk := strings.Repeat("y", AgentSpecMaxPromptBytes)
			a.Prompt = chunk
			a.Instructions = []string{chunk, chunk, chunk, chunk}
		}, "total"},
		{"model leading hyphen", func(a *AgentSpec) { a.Model = "--evil" }, "agentSpec.model"},
		{"model whitespace", func(a *AgentSpec) { a.Model = "a b" }, "agentSpec.model"},
		{"model too long", func(a *AgentSpec) { a.Model = strings.Repeat("m", AgentSpecMaxModelBytes+1) }, "agentSpec.model"},
		{"unknown access", func(a *AgentSpec) { a.Access = "yolo" }, "agentSpec.access"},
		{"relative home", func(a *AgentSpec) { a.Knowledge.Home = "memory" }, "knowledge.home"},
		{"unclean home", func(a *AgentSpec) { a.Knowledge.Home = "/srv/agents/../x" }, "knowledge.home"},
		{"root home", func(a *AgentSpec) { a.Knowledge.Home = "/" }, "knowledge.home"},
		{"shared temp home", func(a *AgentSpec) {
			a.Knowledge.Home = "/tmp"
			a.Knowledge.Index = "/tmp/MEMORY.md"
		}, "shared between sessions"},
		{"relative index", func(a *AgentSpec) { a.Knowledge.Index = "MEMORY.md" }, "knowledge.index"},
		{"index outside home", func(a *AgentSpec) { a.Knowledge.Index = "/srv/other/MEMORY.md" }, "inside agentSpec.knowledge.home"},
		{"index is home", func(a *AgentSpec) { a.Knowledge.Index = a.Knowledge.Home }, "inside agentSpec.knowledge.home"},
		{"index in a sibling with the home as a prefix", func(a *AgentSpec) {
			a.Knowledge.Index = AbsolutePath(string(a.Knowledge.Home) + "-other/MEMORY.md")
		}, "inside agentSpec.knowledge.home"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validAgentSpec()
			k := *s.Knowledge
			s.Knowledge = &k
			c.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("a malformed definition was accepted: %+v", s)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the refusal %q does not name %q", err, c.want)
			}
		})
	}
}

func TestAgentSpec_WireShape(t *testing.T) {
	var spec SessionSpec
	body := `{"machine":"m","runtime":"r","cwd":"/w","agentSpec":{"name":"researcher","instructions":["a","b"],` +
		`"knowledge":{"index":"/h/MEMORY.md","home":"/h","write":true},"model":"p/m","access":"allow"}}`
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.AgentSpec == nil || spec.AgentSpec.Name != "researcher" || spec.AgentSpec.Access != AgentAccessAllow ||
		spec.AgentSpec.Knowledge == nil || !spec.AgentSpec.Knowledge.Write {
		t.Fatalf("decoded %+v", spec.AgentSpec)
	}
	out, err := json.Marshal(SessionSpec{Machine: "m", Runtime: "r", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "agentSpec") {
		t.Fatalf("an absent agentSpec must not be written: %s", out)
	}
}

func TestSupportsAgentSpec_TriState(t *testing.T) {
	c := DriverCapabilities{Source: CapabilitiesObserved}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "supportsAgentSpec") {
		t.Fatalf("an undeclared capability must be absent, not false: %s", b)
	}
	c.SupportsAgentSpec = Declared(false)
	b, _ = json.Marshal(c)
	if !strings.Contains(string(b), `"supportsAgentSpec":false`) {
		t.Fatalf("a declared false must be on the wire: %s", b)
	}
}
