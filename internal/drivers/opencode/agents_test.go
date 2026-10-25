package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #362: the agent pin. In this runtime the agent is chosen per message, so
// a create that names an agent pins nothing by itself, and the create response
// echoes whatever name it was given. These tests hold the three halves of the fix:
// every message names the session's agent, a name the runtime does not list is
// refused, and a pin is honoured only on the runtime's own record of a turn.

func sendTo(t *testing.T, d *Driver, ref fleet.SessionRef, text string) {
	t.Helper()
	if _, err := d.Send(context.Background(), testReq, ref, text, driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("Send(%q): %v", text, err)
	}
}

func TestAgent_EveryMessageNamesTheSessionsAgent(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build", "coder"}
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), testReq, "k1",
		fleet.SessionSpec{Cwd: "/work/x", Agent: "coder", Prompt: "first"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sendTo(t, d, sess.SessionRef, "second")

	got := f.promptsSnapshot()
	if len(got) != 2 {
		t.Fatalf("prompts = %+v, want the opening prompt and the Send", got)
	}
	for i, want := range []string{"first", "second"} {
		p := got[i]
		if p.text != want {
			t.Errorf("prompt %d text = %q, want %q", i, p.text, want)
		}
		if !p.hasAgent || p.agent != "coder" {
			t.Errorf("prompt %d (%q) agent = %q (present: %v), want coder: a message without it runs as the default agent",
				i, want, p.agent, p.hasAgent)
		}
	}
}

func TestAgent_ASessionThatAskedForNoneSendsNone(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build"}
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: "/work/x", Prompt: "first"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sendTo(t, d, sess.SessionRef, "second")

	for i, p := range f.promptsSnapshot() {
		if p.hasAgent {
			t.Errorf("prompt %d named agent %q; a session that asked for none runs as the default by its own choice", i, p.agent)
		}
	}
	for _, r := range f.requestsSnapshot() {
		if r.path == "/agent" {
			t.Errorf("GET /agent was called for a create that named no agent")
		}
	}
}

func TestAgent_AnUnlistedAgentIsRefusedAtCreate(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build", "coder", "plan"}
	d := newTestDriver(t, f)

	for _, tc := range []struct {
		agent       string
		wantClosest string
	}{
		{"sid", ""},         // nothing near enough to be a misspelling: no suggestion
		{"codr", `"coder"`}, // near enough to suggest
	} {
		_, err := d.Create(context.Background(), testReq, "k-"+tc.agent,
			fleet.SessionSpec{Cwd: "/work/x", Agent: fleet.AgentId(tc.agent), Prompt: "go"})
		if err == nil {
			t.Fatalf("Create(agent %q) succeeded; want a refusal: the runtime lists no such agent", tc.agent)
		}
		if kind := kindOfErr(err); kind != fleet.ErrorInvalid {
			t.Errorf("Create(agent %q) error kind = %q, want invalid (%v)", tc.agent, kind, err)
		}
		if want := `agent "` + tc.agent + `" is not in the runtime's own agent list`; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to say %q", err, want)
		}
		if tc.wantClosest == "" && strings.Contains(err.Error(), "closest") {
			t.Errorf("error = %q; a wrong suggestion is worse than none", err)
		} else if tc.wantClosest != "" && !strings.Contains(err.Error(), "closest: "+tc.wantClosest) {
			t.Errorf("error = %q, want the nearest listed agent %s suggested", err, tc.wantClosest)
		}
	}
	for _, r := range f.requestsSnapshot() {
		if r.method == "POST" && (r.path == "/session" || strings.HasSuffix(r.path, "/prompt_async")) {
			t.Errorf("%s %s reached the runtime; a refused create must leave no session and send no prompt", r.method, r.path)
		}
	}
	if got := f.promptsSnapshot(); len(got) != 0 {
		t.Errorf("prompts = %+v, want none", got)
	}
}

// The runtime lists a project's own agents only for a request that names the
// project's directory, so the check must send the session's cwd: without it the
// one way a persona reaches an isolated session is refused as unknown.
func TestAgent_AProjectAgentIsSeenThroughTheSessionsDirectory(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build"}
	f.projectAgents = map[string][]string{"/work/x": {"tester"}}
	d := newTestDriver(t, f)

	if _, err := d.Create(context.Background(), testReq, "k1",
		fleet.SessionSpec{Cwd: "/work/x", Agent: "tester", Prompt: "go"}); err != nil {
		t.Fatalf("Create in the project that defines the agent: %v", err)
	}
	var query string
	for _, r := range f.requestsSnapshot() {
		if r.path == "/agent" {
			query = r.query
		}
	}
	if query != "directory=%2Fwork%2Fx" {
		t.Errorf("GET /agent query = %q, want the session's directory", query)
	}

	_, err := d.Create(context.Background(), testReq, "k2", fleet.SessionSpec{Cwd: "/work/y", Agent: "tester"})
	if err == nil || kindOfErr(err) != fleet.ErrorInvalid {
		t.Errorf("Create in another project = %v, want an invalid refusal: that project defines no such agent", err)
	}
}

// A runtime that gives no usable answer is "could not tell", never "no such
// agent": the create goes ahead as it did before the check existed, and the pin
// stays unresolved.
func TestAgent_ARuntimeThatCannotListAgentsDoesNotRefuse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fakeServer)
	}{
		{"endpoint absent", func(f *fakeServer) {}},
		{"server error", func(f *fakeServer) { f.agentsStatus = 500 }},
		{"not a list", func(f *fakeServer) { f.agentsBody = `{"error":"nope"}` }},
		{"empty list", func(f *fakeServer) { f.agentsBody = `[]` }},
		{"entries naming nothing", func(f *fakeServer) { f.agentsBody = `[{"id":"x"}]` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServer(t)
			tc.setup(f)
			d := newTestDriver(t, f)
			sess, err := d.Create(context.Background(), testReq, "k1",
				fleet.SessionSpec{Cwd: "/work/x", Agent: "whatever", Prompt: "go"})
			if err != nil {
				t.Fatalf("Create: %v; an unreadable agent list is not evidence the agent is missing", err)
			}
			if sess.Pins == nil || sess.Pins.Agent == nil || sess.Pins.Agent.Honoured != nil {
				t.Errorf("Pins = %+v, want the agent pin present and unresolved", sess.Pins)
			}
			if got := f.promptsSnapshot(); len(got) != 1 || got[0].agent != "whatever" {
				t.Errorf("prompts = %+v, want the agent still named on the opening prompt", got)
			}
		})
	}
}

// The pin resolves only from the runtime's own record of a turn that ran: the
// newest assistant message names the agent it was handled by. A user message is
// what was asked for, not what ran, and so is not evidence.
func TestAgent_ThePinResolvesFromTheNewestAssistantMessage(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build", "tester"}
	d := newTestDriver(t, f)
	spec := fleet.SessionSpec{Cwd: "/work/x", Agent: "tester", Prompt: "go"}

	sess, err := d.Create(context.Background(), testReq, "k1", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	replay := func() *fleet.PinResult {
		t.Helper()
		again, err := d.Create(context.Background(), testReq, "k1", spec)
		if err != nil {
			t.Fatalf("replayed Create: %v", err)
		}
		if again.ID != sess.ID {
			t.Fatalf("replayed Create returned session %s, want %s", again.ID, sess.ID)
		}
		if again.Pins == nil || again.Pins.Agent == nil {
			t.Fatalf("Pins = %+v, want an Agent entry", again.Pins)
		}
		return again.Pins.Agent
	}
	msg := func(role, agent string) wireMessage {
		return wireMessage{Info: wireMessageInfo{Role: role, Agent: agent}}
	}

	// Nothing has run: the user message only says what was asked for.
	f.setLastMessage(sess.ID, msg("user", "tester"))
	if p := replay(); p.Honoured != nil {
		t.Errorf("with only a user message Honoured = %v, want nil", *p.Honoured)
	}

	// The turn ran as the default agent: the silent fallback this issue is about.
	f.setLastMessage(sess.ID, msg("assistant", "build"))
	p := replay()
	if p.Honoured == nil || *p.Honoured {
		t.Fatalf("a turn that ran as build: Honoured = %v, want false", p.Honoured)
	}
	if p.Applied != "build" || p.Source != fleet.PinObserved || p.Requested != "tester" {
		t.Errorf("pin = %+v, want requested tester, applied build, source observed", p)
	}

	// The newest turn ran as the requested agent.
	f.setLastMessage(sess.ID, msg("user", "tester"))
	f.setLastMessage(sess.ID, msg("assistant", "tester"))
	p = replay()
	if p.Honoured == nil || !*p.Honoured {
		t.Fatalf("a turn that ran as tester: Honoured = %v, want true", p.Honoured)
	}
	if p.Applied != "tester" || p.Source != fleet.PinObserved {
		t.Errorf("pin = %+v, want applied tester, source observed", p)
	}

	// An assistant message that names no agent is no evidence either way.
	f.setLastMessage(sess.ID, msg("assistant", ""))
	if p := replay(); p.Honoured != nil || p.Applied != "" {
		t.Errorf("a record naming no agent: pin = %+v, want unresolved", p)
	}
}

// The session's agent is what its messages are sent as, and it survives a restart
// of the service: a relaunched session that dropped it would silently fall back
// to the default agent on its next message.
func TestAgent_ARelaunchedSessionStillNamesItsAgent(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "survivor", Agent: "build", Prompt: "one"})
	prompts := filepath.Join(inside(t, d1, sess.ID).vars()["XDG_DATA_HOME"], "fake-prompts.jsonl")
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	sendTo(t, d2, sess.SessionRef, "two")

	raw, err := os.ReadFile(prompts)
	if err != nil {
		t.Fatalf("reading what the session sent: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("the session sent %d prompts, want 2:\n%s", len(lines), raw)
	}
	for i, line := range lines {
		var body struct {
			Agent *string `json:"agent"`
		}
		if err := json.Unmarshal([]byte(line), &body); err != nil {
			t.Fatalf("prompt %d is not JSON: %v", i, err)
		}
		if body.Agent == nil || *body.Agent != "build" {
			t.Errorf("prompt %d (%s) does not name agent build; after a restart it must", i, line)
		}
	}
}
