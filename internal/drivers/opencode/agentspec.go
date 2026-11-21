package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
)

// A portable agent definition on a create (muster #509).
//
// `agentSpec` is a definition the create carries, where `agent` names one the
// runtime must already know. This driver translates it into the runtime's own
// form for THAT SESSION ONLY, through the same path the operator's agents
// (operatoragents.go, #396) and instruction files (operatorinstructions.go, #504)
// already take: the session's own opencode.json, under its own directory. Nothing
// is written into any account's or project's configuration.
//
// # Where the text goes, and why never into opencode.json
//
// The runtime expands `{env:NAME}` and `{file:path}` inside the STRING VALUES of a
// configuration, after the JSON is parsed — so JSON escaping does not stop it
// (measured on 1.18.3: an inline prompt carrying `{file:/some/path}` read that
// file into the agent's prompt, escaped or not). A definition's text comes from a
// caller, and a caller that may only start sessions must not be able to read a file
// by naming it in a prompt. So none of the caller's text is put in the
// configuration:
//
//   - the prompt and the description are written to owner-only files in the
//     session's own directory, and the agent entry carries only `{file:<path>}`
//     references to them. The body of a referenced file is inserted verbatim and
//     is not expanded again (measured);
//   - each instruction, and the knowledge index with a note naming the home, is an
//     instruction file in the same directory, listed under the runtime's own
//     `instructions` key. An instruction file is plain text the runtime never
//     expands.
//
// # The knowledge index is read when the session starts
//
// The index is read from the caller's file on every start — the create and every
// relaunch — and the session gets a copy, so a running session keeps what it
// started with (the argument operatorinstructions.go makes for the operator's
// files) and an edit to the index reaches the next session, never one in progress.
// A file that cannot be used refuses the start with the reason; it is never
// skipped, because a session that was meant to know what its memory says and
// silently did not is the failure this exists to prevent.
//
// # What a relaunch needs, and where it is kept
//
// The record keeps only the name and the access (recordedAgentSpec). The rest of
// the definition is in the session's own directory (definition.json, owner-only),
// beside the other files a session keeps there, and goes when the directory goes.
//
// # Names
//
// The runtime merges an entry over a built-in agent of the same name field by
// field, so a definition called `plan` would run as the runtime's `plan` agent with
// some of its fields replaced. Those names are refused (builtinAgentNames), as is a
// name the operator's agent file already defines: which of the two should win is not
// a guess this driver makes.

const (
	agentSpecDirName        = "agentspec"
	agentSpecDefinitionFile = "definition.json"
	agentSpecPromptFile     = "prompt.md"
	agentSpecDescFile       = "description.txt"
	agentSpecInstructions   = "instructions"
	agentSpecKnowledgeFile  = "knowledge.md"

	// agentSpecDefinitionMaxBytes bounds the stored definition read at a relaunch.
	// fleet.AgentSpecMaxTotalBytes bounds what a create may carry; this adds room
	// for the JSON around it.
	agentSpecDefinitionMaxBytes = fleet.AgentSpecMaxTotalBytes + 64<<10
)

// builtinAgentNames are the agents the runtime lists with nothing configured
// (measured on 1.18.3, `opencode agent list` in an empty configuration). A newer
// runtime may add more; the runtime's own agent list, which checkAgent reads at
// create, still decides what exists.
var builtinAgentNames = []string{"build", "compaction", "explore", "general", "plan", "summary", "title"}

// recordedAgentSpec is what the session's record keeps of a definition: enough to
// rebuild the spec a relaunch judges (the agent name, which an operator rule is
// scoped by, and the access), and nothing of the text.
type recordedAgentSpec struct {
	Name   string            `json:"name"`
	Access fleet.AgentAccess `json:"access,omitempty"`
}

func recordAgentSpec(spec fleet.SessionSpec) *recordedAgentSpec {
	if spec.AgentSpec == nil {
		return nil
	}
	return &recordedAgentSpec{Name: spec.AgentSpec.Name, Access: spec.AgentSpec.Access}
}

// renderedAgentSpec is a definition as one session start read it.
type renderedAgentSpec struct {
	def   fleet.AgentSpec
	index []byte // the knowledge index as it was when this start read it
}

// agentSpecInvalid is a refusal of the request itself.
func (d *Driver) agentSpecInvalid(format string, args ...any) error {
	return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + fmt.Sprintf(format, args...) + " (#509)", Machine: d.machine}
}

// checkAgentSpec is the driver's own refusal, made before any side effect, of what
// it cannot or must not translate. The service refuses most of these first; a
// driver's Create is a public method, so it checks again.
func (d *Driver) checkAgentSpec(spec fleet.SessionSpec) error {
	a := spec.AgentSpec
	if a == nil {
		return nil
	}
	if d.shared != nil {
		return &fleet.Error{
			Kind:   fleet.ErrorUnsupported,
			Reason: fleet.ReasonAgentSpecUnsupported,
			Message: "create: shared mode runs one runtime for every session, so a definition cannot be given to one of them; " +
				"name an agent the runtime already knows with `agent` instead (#509)",
			Machine: d.machine,
		}
	}
	if spec.Agent != "" {
		return d.agentSpecInvalid("agent and agentSpec are mutually exclusive")
	}
	if err := a.Validate(); err != nil {
		return d.agentSpecInvalid("%s", err.Error())
	}
	for _, b := range builtinAgentNames {
		if a.Name == b {
			return d.agentSpecInvalid("agentSpec.name %q is one of the runtime's own agents, and an entry of that name would be merged over it "+
				"rather than defining a new one; choose another name", a.Name)
		}
	}
	return nil
}

// agentSpecFor reads the definition one session start needs: from the create's own
// spec, or, at a relaunch (existingDir is the session's directory), from the copy
// the first start kept there. It reads the knowledge index live. Nil when the
// spec carries no definition.
func (d *Driver) agentSpecFor(spec fleet.SessionSpec, existingDir string, agents operatorAgents) (*renderedAgentSpec, error) {
	if spec.AgentSpec == nil {
		return nil, nil
	}
	def := *spec.AgentSpec
	if existingDir != "" {
		stored, err := loadAgentSpecDefinition(existingDir)
		if err != nil {
			return nil, fmt.Errorf("create: the session's stored agent definition cannot be used: %w", err)
		}
		if stored.Name != def.Name {
			return nil, fmt.Errorf("create: the session's stored agent definition is for %q, not %q", stored.Name, def.Name)
		}
		stored.Access = def.Access
		def = stored
	}
	if _, taken := agents[def.Name]; taken {
		return nil, d.agentSpecInvalid("agentSpec.name %q is also defined in this machine's operator agents; choose another name", def.Name)
	}
	r := &renderedAgentSpec{def: def}
	if k := def.Knowledge; k != nil {
		text, err := readKnowledgeIndex(k.Index)
		if err != nil {
			if existingDir != "" {
				return nil, fmt.Errorf("create: %w", err)
			}
			return nil, d.agentSpecInvalid("%s", err.Error())
		}
		r.index = text
	}
	return r, nil
}

// readKnowledgeIndex reads the index a definition names. The error says what is
// wrong in words a caller can act on; it never quotes the file's content.
func readKnowledgeIndex(path fleet.AbsolutePath) ([]byte, error) {
	p := string(path)
	info, err := os.Stat(p) // follows a link: a client may keep one
	if err != nil {
		return nil, fmt.Errorf("agentSpec.knowledge.index cannot be read: %w", err)
	}
	// Before opening: opening a named pipe blocks until something writes to it.
	if !info.Mode().IsRegular() {
		kind := "not a regular file"
		if info.IsDir() {
			kind = "a directory, not a file"
		}
		return nil, fmt.Errorf("agentSpec.knowledge.index %q is %s", p, kind)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("agentSpec.knowledge.index cannot be read: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, fleet.AgentKnowledgeMaxIndexBytes+1))
	if err != nil {
		return nil, fmt.Errorf("agentSpec.knowledge.index cannot be read: %w", err)
	}
	switch {
	case len(raw) > fleet.AgentKnowledgeMaxIndexBytes:
		return nil, fmt.Errorf("agentSpec.knowledge.index %q is larger than %d bytes", p, fleet.AgentKnowledgeMaxIndexBytes)
	case !utf8.Valid(raw):
		return nil, fmt.Errorf("agentSpec.knowledge.index %q is not UTF-8 text", p)
	}
	return raw, nil
}

// loadAgentSpecDefinition reads the definition the first start of a session kept in
// its directory.
func loadAgentSpecDefinition(dir string) (fleet.AgentSpec, error) {
	var def fleet.AgentSpec
	f, err := os.Open(filepath.Join(dir, agentSpecDirName, agentSpecDefinitionFile))
	if err != nil {
		return def, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, agentSpecDefinitionMaxBytes+1))
	if err != nil {
		return def, err
	}
	if len(raw) > agentSpecDefinitionMaxBytes {
		return def, errors.New("the stored definition is larger than any create could have carried")
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		return def, fmt.Errorf("the stored definition is not readable: %w", err)
	}
	if err := def.Validate(); err != nil {
		return def, err
	}
	return def, nil
}

// knowledgeNote is the file the runtime reads the index from: a note naming the
// home, then the index as this start read it.
func knowledgeNote(k fleet.AgentKnowledge, index []byte) []byte {
	var b strings.Builder
	b.WriteString("# Your memory\n\n")
	fmt.Fprintf(&b, "Your memory home is `%s`. ", string(k.Home))
	if k.Write {
		b.WriteString("This session may read and write files there.")
	} else {
		b.WriteString("This session may read files there.")
	}
	b.WriteString(" Its index, as it stood when this session started, follows.\n\n---\n\n")
	b.Write(index)
	if len(index) > 0 && index[len(index)-1] != '\n' {
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// writeAgentSpecFiles puts the definition in dir's own agentspec directory and
// returns the agent entry for the session's configuration and the paths of the
// instruction files, in the order the runtime reads them. A relaunch passes the
// directory the previous process used, so the directory is rebuilt first: what is
// there is exactly this start's reading.
func writeAgentSpecFiles(dir string, r *renderedAgentSpec) (json.RawMessage, []string, error) {
	base := filepath.Join(dir, agentSpecDirName)
	// The runtime reads a file through `{file:<path>}`, whose syntax a path with a
	// brace in it would break; the directory is this driver's own, so this is a
	// check that it stays that way and not a guess about a caller.
	if strings.ContainsAny(base, "{}") {
		return nil, nil, fmt.Errorf("opencode: the session directory %q cannot be named in a file reference", base)
	}
	if err := privfs.RemoveAll(base); err != nil {
		return nil, nil, fmt.Errorf("opencode: clearing the session's agent definition: %w", err)
	}
	if err := privfs.MkdirAll(base); err != nil {
		return nil, nil, fmt.Errorf("opencode: creating the session's agent definition directory: %w", err)
	}
	write := func(rel string, data []byte) (string, error) {
		p := filepath.Join(base, rel)
		if err := privfs.MkdirAll(filepath.Dir(p)); err != nil {
			return "", fmt.Errorf("opencode: creating the session's agent definition directory: %w", err)
		}
		if err := privfs.WriteFile(p, data); err != nil {
			return "", fmt.Errorf("opencode: writing the session's agent definition: %w", err)
		}
		return p, nil
	}
	stored, err := json.Marshal(r.def)
	if err != nil {
		return nil, nil, fmt.Errorf("opencode: encoding the session's agent definition: %w", err)
	}
	if _, err := write(agentSpecDefinitionFile, stored); err != nil {
		return nil, nil, err
	}

	entry := map[string]string{"mode": "primary"}
	if r.def.Description != "" {
		p, err := write(agentSpecDescFile, []byte(r.def.Description))
		if err != nil {
			return nil, nil, err
		}
		entry["description"] = "{file:" + p + "}"
	}
	if r.def.Prompt != "" {
		p, err := write(agentSpecPromptFile, []byte(r.def.Prompt))
		if err != nil {
			return nil, nil, err
		}
		entry["prompt"] = "{file:" + p + "}"
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return nil, nil, fmt.Errorf("opencode: encoding the session's agent entry: %w", err)
	}

	var paths []string
	for i, text := range r.def.Instructions {
		p, err := write(filepath.Join(agentSpecInstructions, fmt.Sprintf("%02d-instruction.md", i+1)), []byte(text))
		if err != nil {
			return nil, nil, err
		}
		paths = append(paths, p)
	}
	if k := r.def.Knowledge; k != nil {
		p, err := write(agentSpecKnowledgeFile, knowledgeNote(*k, r.index))
		if err != nil {
			return nil, nil, err
		}
		paths = append(paths, p)
	}
	return raw, paths, nil
}

// policyGrantsAllow reports whether this machine's operator policy gives the
// definition the unconfined `allow` access: the one posture this driver honours
// outside a sandbox, because the operator, who knows what this host can enforce, said
// so for this agent. The create's own `access: allow` is never enough: it is the
// caller asking, and this driver pre-approves every tool only inside a boundary
// something other than the approval enforces (permission.go). Checked at create and
// again at every relaunch, so a policy that has since been withdrawn does not leave a
// parked session coming back unconfined.
func (d *Driver) policyGrantsAllow(spec fleet.SessionSpec) bool {
	if spec.AgentSpec == nil || spec.AgentSpec.Access != fleet.AgentAccessAllow {
		return false
	}
	got, ok := d.createPolicy.AccessFor(d.runtime, spec)
	return ok && got == fleet.AgentAccessAllow
}

// recordedPermissionMode is the permission mode a record keeps: "bypass" or empty
// (sessionRecord.PermissionMode says so). The ordinary mode is what an absent value
// already is (#329), and the service now asks for it by name when a definition says
// `access: "ask"` (#509), so keeping it would make a relaunch read the record as a
// pre-approved session that asked for no sandbox.
func recordedPermissionMode(mode string) string {
	if mode == fleet.PermissionModeBypass {
		return mode
	}
	return ""
}
