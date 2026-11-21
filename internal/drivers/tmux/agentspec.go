package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/agentdef"
)

// A portable agent definition on a create (muster #509).
//
// `agentSpec` is a definition the create carries, where `agent` names one the
// runtime must already know. For the default agent CLI this driver renders it as a
// PLUGIN directory for that session only and starts the CLI with `--plugin-dir`
// pointing at it. The CLI's own help says what that flag is for: it loads a plugin
// "for this session only", so nothing is written into the user's own configuration
// (`~/.claude/agents`) or the project's, and a definition cannot outlive its
// session or be seen by another.
//
// # Why a plugin and not the flag that takes the definition inline
//
// `--agents` takes a JSON object on the command line, so the prompt would sit in an
// argv any process on the machine can read — exactly what §5.3 forbids for
// contextRef, and what a prompt carries as much as any context does. The plugin
// route puts the text in an owner-only file and only the file's path on the command
// line. Measured on the CLI this was written against: the agent defined that way
// answers as its prompt says, honours the model in its definition, loads under
// `--setting-sources project --strict-mcp-config` (isolateConfiguration), and comes
// back unchanged under `--resume`.
//
// # The name the CLI is given, and the name that is reported
//
// The agent is passed to `--agent` in the plugin's own namespace
// (`muster-session:<name>`). A bare name could resolve to an agent of the same name
// in the user's configuration or the project, and a session that runs as somebody
// else's persona under the name it asked for is the failure this avoids. The
// namespace is this driver's own and never reaches a caller: the agent a listing
// reports is the definition's name (agentFromArgs strips the prefix), and the
// create's own pin (spec.Agent) is that name too.
//
// # The knowledge index
//
// It is read from the caller's file when the session is created and rendered into
// the agent's definition, under a note naming the home, so a running session keeps
// what it started with and an edit to the index reaches the next session, never one
// in progress. A file that cannot be used refuses the create with the reason. The
// home is added to the directories the CLI may use (`--add-dir`), and to the
// sandbox's read (or write) grant when the session is confined.
//
// # What it does not do
//
// Nothing is kept for a resume: a client that resumes a conversation sends the
// definition again, and no prompt text is stored anywhere but the session's own
// directory, which is removed with the session.

const (
	// agentSpecPlugin is the plugin's name, and so the namespace of its agent. It is
	// this driver's: a create's own `agent` that spells it is read as the plugin's.
	agentSpecPlugin = "muster-session"
	// agentSpecDirName is where the plugin directories live, under the state directory.
	agentSpecDirName = "agentspec"
	// agentSpecDirGrace is how long a plugin directory with no live session is kept,
	// so a session created a moment ago and not yet in the multiplexer's listing is
	// not swept from under its own launch (the sandbox directories' rule).
	agentSpecDirGrace = 10 * time.Minute
	// agentSpecSweepEvery bounds how often the sweep reads the directory.
	agentSpecSweepEvery = time.Minute
)

// agentSpecRef is the reference the CLI is given for a definition's agent.
func agentSpecRef(name string) string { return agentSpecPlugin + ":" + name }

// stripAgentSpecNamespace is agentSpecRef reversed, for reporting: the agent a
// session runs as is the definition's name, not the namespace this driver gave it.
func stripAgentSpecNamespace(agent string) string {
	return strings.TrimPrefix(agent, agentSpecPlugin+":")
}

// agentSpecRoot is where the plugin directories live, or why this driver has none.
func (d *Driver) agentSpecRoot() (string, string) {
	if d.store == nil || d.store.Dir() == "" {
		return "", "this driver has no state directory to hold a session's agent definition"
	}
	return filepath.Join(d.store.Dir(), agentSpecDirName), ""
}

// agentSpecDirFor is a session's plugin directory. Named by a hash of the session
// name, like the sandbox's, so the name never has to be a safe path component.
func agentSpecDirFor(root, session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(root, "s-"+hex.EncodeToString(sum[:])[:24])
}

// supportsAgentSpec is whether this driver can translate a definition: only the
// default builder is known to pass `--plugin-dir` (the same reason
// IsolatesConfiguration is gated on it), and there must be a state directory to
// render it into.
func (d *Driver) supportsAgentSpec() bool {
	_, why := d.agentSpecRoot()
	return d.buildsClaudeArgv && why == ""
}

// checkAgentSpec is the driver's own refusal, made before any side effect, of what
// it cannot or must not translate. The service refuses most of these first; a
// driver's Create is a public method, so it checks again.
func (d *Driver) checkAgentSpec(spec fleet.SessionSpec) error {
	a := spec.AgentSpec
	if a == nil {
		return nil
	}
	if !d.supportsAgentSpec() {
		why := "its command line is built by a replacement builder that is not known to pass the flag that loads one"
		if _, w := d.agentSpecRoot(); w != "" {
			why = w
		}
		return &fleet.Error{
			Kind:   fleet.ErrorUnsupported,
			Reason: fleet.ReasonAgentSpecUnsupported,
			Message: "create: this driver cannot translate an agentSpec (" + why + "); " +
				"name an agent the CLI already knows with `agent` instead (#509)",
			Machine: d.machine,
		}
	}
	invalid := func(format string, args ...any) error {
		return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + fmt.Sprintf(format, args...) + " (#509)", Machine: d.machine}
	}
	if spec.Agent != "" {
		return invalid("agent and agentSpec are mutually exclusive")
	}
	if err := a.Validate(); err != nil {
		return invalid("%s", err.Error())
	}
	return nil
}

// agentSpecPrep is what one create's definition becomes.
type agentSpecPrep struct {
	// dir is the plugin directory, readable by a confined session.
	dir string
	// args are the flags that load it, to go after the program name.
	args []string
	// drop removes the directory again, for a create that did not start.
	drop func()
}

// prepareAgentSpec renders the definition for the session called name: reads the
// knowledge index, writes the plugin directory, and returns the flags that load it.
// built.Agent is set to the plugin's own reference, so the default builder's
// `--agent` is the namespaced one; the create's pin (spec.Agent) stays the bare name.
func (d *Driver) prepareAgentSpec(spec fleet.SessionSpec, name string, built *fleet.SessionSpec) (*agentSpecPrep, error) {
	def := *spec.AgentSpec
	root, why := d.agentSpecRoot()
	if why != "" {
		return nil, &fleet.Error{Kind: fleet.ErrorUnsupported, Reason: fleet.ReasonAgentSpecUnsupported, Message: "create: " + why, Machine: d.machine}
	}
	var index []byte
	if def.Knowledge != nil {
		text, err := agentdef.ReadIndex(def.Knowledge.Index)
		if err != nil {
			return nil, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + err.Error() + " (#509)", Machine: d.machine}
		}
		index = text
	}
	dir := agentSpecDirFor(root, name)
	// Rebuilt from nothing: a name reused after a session ended must not inherit the
	// files of the one before it.
	_ = os.RemoveAll(dir)
	if err := writeAgentPlugin(dir, def, index); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create: rendering the agent definition: %w", err)
	}
	built.Agent = fleet.AgentId(agentSpecRef(def.Name))
	args := []string{"--plugin-dir", dir}
	if def.Knowledge != nil {
		args = append(args, "--add-dir", string(def.Knowledge.Home))
	}
	return &agentSpecPrep{dir: dir, args: args, drop: func() { _ = os.RemoveAll(dir) }}, nil
}

// writeAgentPlugin writes the plugin: a manifest naming the namespace, and one
// agent file holding the definition. Owner-only, like every file this driver keeps
// a session's text in.
func writeAgentPlugin(dir string, def fleet.AgentSpec, index []byte) error {
	manifest, err := json.Marshal(map[string]string{
		"name": agentSpecPlugin, "version": "0.0.0", "description": "agent definitions for one muster session",
	})
	if err != nil {
		return err
	}
	for _, f := range []struct {
		rel  string
		data []byte
	}{
		{filepath.Join(".claude-plugin", "plugin.json"), manifest},
		{filepath.Join("agents", def.Name+".md"), []byte(agentMarkdown(def, index))},
	} {
		p := filepath.Join(dir, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// agentMarkdown renders a definition as the CLI's agent file: frontmatter naming it,
// then the body, which is the system prompt. The prompt comes first, then each
// instruction in the caller's order, then the note about the agent's memory with the
// index as it was read.
//
// The frontmatter values are written as JSON strings, which YAML reads as quoted
// scalars, so a colon, a quote or a leading symbol in a description cannot change
// what the field is. The model is not written here: the create's own `--model`
// carries it, validated where this driver validates models.
func agentMarkdown(def fleet.AgentSpec, index []byte) string {
	desc := def.Description
	if desc == "" {
		desc = "Agent " + def.Name
	}
	quoted, _ := json.Marshal(desc)
	var body []string
	if strings.TrimSpace(def.Prompt) != "" {
		body = append(body, strings.TrimRight(def.Prompt, "\n"))
	}
	for _, ins := range def.Instructions {
		body = append(body, strings.TrimRight(ins, "\n"))
	}
	if k := def.Knowledge; k != nil {
		body = append(body, strings.TrimRight(agentdef.Note(*k, index), "\n"))
	}
	if len(body) == 0 {
		body = append(body, "You are "+def.Name+".")
	}
	return "---\nname: " + def.Name + "\ndescription: " + string(quoted) + "\n---\n\n" + strings.Join(body, "\n\n") + "\n"
}

// insertAfterProgram puts args after argv's first element, the program: flags may
// come in any order, and this keeps them clear of anything a builder appended last.
func insertAfterProgram(argv, args []string) []string {
	if len(args) == 0 || len(argv) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv)+len(args))
	out = append(out, argv[0])
	out = append(out, args...)
	return append(out, argv[1:]...)
}

// withKnowledgeGrant returns a sandbox request that also lets the session use the
// knowledge home, and read its own plugin directory. The service already adds the
// home to a create it lowers; this makes the driver right on its own, because a
// driver's Create is a public method. Never mutates the caller's request.
func withKnowledgeGrant(sb *fleet.SandboxSpec, def fleet.AgentSpec, pluginDir string) *fleet.SandboxSpec {
	out := *sb
	out.ReadPaths = slices.Clone(sb.ReadPaths)
	out.WritePaths = slices.Clone(sb.WritePaths)
	add := func(list *[]fleet.AbsolutePath, p fleet.AbsolutePath) {
		if !slices.Contains(*list, p) {
			*list = append(*list, p)
		}
	}
	add(&out.ReadPaths, fleet.AbsolutePath(pluginDir))
	if k := def.Knowledge; k != nil {
		switch {
		case slices.Contains(out.WritePaths, k.Home):
		case k.Write:
			add(&out.WritePaths, k.Home)
		default:
			add(&out.ReadPaths, k.Home)
		}
	}
	return &out
}

// dropAgentSpec removes a session's plugin directory: it has no reader left.
func (d *Driver) dropAgentSpec(session string) {
	if root, why := d.agentSpecRoot(); why == "" {
		_ = os.RemoveAll(agentSpecDirFor(root, session))
	}
}

// sweepAgentSpecs removes plugin directories no live session owns: a session that
// ended without Close (its process exited and was reaped by another path, the
// service was down). A directory is kept for a grace period, as the sandbox's are.
func (d *Driver) sweepAgentSpecs(rows []paneRow) {
	root, why := d.agentSpecRoot()
	if why != "" {
		return
	}
	now := d.now()
	d.sandboxMu.Lock()
	if now.Sub(d.agentSpecSwept) < agentSpecSweepEvery {
		d.sandboxMu.Unlock()
		return
	}
	d.agentSpecSwept = now
	d.sandboxMu.Unlock()

	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, r := range rows {
		live[agentSpecDirFor(root, r.session)] = true
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if live[dir] {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < agentSpecDirGrace {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}
