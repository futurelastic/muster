package fleet

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// AgentAccess is how much a session may do unattended, in the vocabulary a
// portable agent definition uses (muster #509). It is not a driver's own
// posture: the service lowers it onto the engine-neutral fields every driver
// already honours (PermissionMode for allow, Posture for sandboxed), so a
// driver never learns a third spelling of the same request.
type AgentAccess string

const (
	// AgentAccessAsk is the runtime's ordinary posture: it asks before acting.
	AgentAccessAsk AgentAccess = "ask"
	// AgentAccessAllow lets the session act without asking and without a
	// sandbox. It is refused unless the caller holds the grant that bypass
	// needs, or this machine's operator policy gives it for that agent.
	AgentAccessAllow AgentAccess = "allow"
	// AgentAccessSandboxed lets the session act without asking INSIDE an
	// operating-system sandbox. A runtime that cannot confine refuses the create
	// (ReasonPostureUnenforceable) — never a session that acts unattended and
	// unconfined under a name that promised otherwise.
	AgentAccessSandboxed AgentAccess = "sandboxed"
)

// ReasonAgentSpecUnsupported is the machine-readable reason on the `unsupported`
// answer to a create carrying an agentSpec the runtime cannot translate
// (DriverCapabilities.SupportsAgentSpec is not true). A client falls back to
// `agent` by name on it.
const ReasonAgentSpecUnsupported = "agent-spec-unsupported"

// Bounds on an AgentSpec. They are honest limits rather than incidental ones: a
// definition is rendered into files and a runtime's own configuration, so an
// unbounded one is a way to make this service write an unbounded amount.
const (
	// AgentSpecMaxNameLen is the longest agent name.
	AgentSpecMaxNameLen = 64
	// AgentSpecMaxDescriptionBytes bounds the one-line description.
	AgentSpecMaxDescriptionBytes = 1 << 10
	// AgentSpecMaxPromptBytes bounds the prompt, and each instruction.
	AgentSpecMaxPromptBytes = 64 << 10
	// AgentSpecMaxInstructions bounds the number of instruction layers.
	AgentSpecMaxInstructions = 32
	// AgentSpecMaxTotalBytes bounds description + prompt + every instruction.
	AgentSpecMaxTotalBytes = 256 << 10
	// AgentSpecMaxModelBytes bounds the model hint.
	AgentSpecMaxModelBytes = 256
	// AgentKnowledgeMaxIndexBytes bounds the knowledge index a driver reads at
	// create. An index larger than this is refused, not truncated: a session that
	// silently read half of its memory index would look informed and not be.
	AgentKnowledgeMaxIndexBytes = 256 << 10
)

// agentNamePattern is what an agent name may be. It is the intersection of what
// every runtime accepts as an agent's file name or key and what is safe as one
// command-line value: lower-case letters, digits and hyphens, not starting or
// ending with a hyphen, and no namespace separator.
var agentNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// AgentSpec is a portable agent definition a create carries (muster #509).
//
// # Why a definition rather than a name
//
// `agent` at create is a name the runtime must already know — an agent file in a
// user's configuration, an entry in an operator's file. A client that wants "this
// persona, on whichever runtime and model the person picks" then has to render
// and keep one native copy per runtime, and a hand-made copy drifts. Turning a
// definition into a running session is this service's job, because it is the
// party that knows each runtime.
//
// # What a driver does with it
//
// Each driver translates the definition into its runtime's native form for THAT
// SESSION ONLY and writes nothing into any account's or project's configuration:
// the prompt and instructions become the agent's system prompt and instruction
// layers; the knowledge index is read from the file at create (never a copy that
// outlives the session) and put in front of the agent, which is also told the
// home path; the model is validated where the driver validates models; and the
// access is lowered by the service (see AgentAccess).
//
// A driver that cannot do all of that refuses the create `unsupported` with
// ReasonAgentSpecUnsupported — DriverCapabilities.SupportsAgentSpec says so in
// advance — and never starts a session that follows less of the definition than
// the client sent.
//
// # What it is not
//
// It is not secret storage: the text is rendered into files on the machine and
// never into a command line (§5.3), but it is not meant to carry credentials.
// It is mutually exclusive with `agent` (two ways to name the same persona), with
// the top-level `model` (one model hint, not two), and — through `access` — with
// the per-runtime posture fields (two sources of truth for one decision).
type AgentSpec struct {
	// Name is what the session reports as its agent. It need not exist anywhere
	// beforehand. Lower-case letters, digits and hyphens.
	Name string `json:"name"`
	// Description is one line saying what the agent is for.
	Description string `json:"description,omitempty"`
	// Prompt is the agent's system prompt.
	Prompt string `json:"prompt,omitempty"`
	// Instructions are shared rules layered after the prompt, in order.
	Instructions []string `json:"instructions,omitempty"`
	// Knowledge is the agent's memory home: where it is, and its index.
	Knowledge *AgentKnowledge `json:"knowledge,omitempty"`
	// Model is a hint like the top-level `model`, validated where the driver
	// validates models.
	Model string `json:"model,omitempty"`
	// Access is how much the session may do unattended. Empty means the runtime's
	// ordinary posture. A machine's operator policy may set or raise it per agent
	// name, and then wins over this value.
	Access AgentAccess `json:"access,omitempty"`
}

// AgentKnowledge is an agent's memory home, given as absolute paths by the client
// (this service assumes no folder layout; where a client keeps memory is its own
// configuration).
type AgentKnowledge struct {
	// Index is the file whose content the session is given at start. It is read
	// live from the file at create. It must be inside Home so one grant covers
	// both.
	Index AbsolutePath `json:"index"`
	// Home is the directory the agent is told about and may read under any
	// sandbox or posture.
	Home AbsolutePath `json:"home"`
	// Write also grants write under Home. Absent asks for read only.
	Write bool `json:"write,omitempty"`
}

// Validate checks the shape of an agent definition. It does not decide whether
// any runtime can translate it, nor read the index — those are the service's and
// the driver's, at the point they can say so truthfully.
func (a AgentSpec) Validate() error {
	if !agentNamePattern.MatchString(a.Name) {
		return fmt.Errorf("agentSpec.name %q must be 1-%d lower-case letters, digits or hyphens, "+
			"not starting or ending with a hyphen", a.Name, AgentSpecMaxNameLen)
	}
	if len(a.Description) > AgentSpecMaxDescriptionBytes {
		return fmt.Errorf("agentSpec.description is %d bytes; the limit is %d", len(a.Description), AgentSpecMaxDescriptionBytes)
	}
	if strings.ContainsAny(a.Description, "\r\n") {
		return fmt.Errorf("agentSpec.description must be one line")
	}
	if err := checkNoControl("agentSpec.description", a.Description, false); err != nil {
		return err
	}
	if len(a.Prompt) > AgentSpecMaxPromptBytes {
		return fmt.Errorf("agentSpec.prompt is %d bytes; the limit is %d", len(a.Prompt), AgentSpecMaxPromptBytes)
	}
	if err := checkNoControl("agentSpec.prompt", a.Prompt, true); err != nil {
		return err
	}
	if len(a.Instructions) > AgentSpecMaxInstructions {
		return fmt.Errorf("agentSpec.instructions has %d entries; the limit is %d", len(a.Instructions), AgentSpecMaxInstructions)
	}
	total := len(a.Description) + len(a.Prompt)
	for i, ins := range a.Instructions {
		field := fmt.Sprintf("agentSpec.instructions[%d]", i)
		if strings.TrimSpace(ins) == "" {
			return fmt.Errorf("%s is empty", field)
		}
		if len(ins) > AgentSpecMaxPromptBytes {
			return fmt.Errorf("%s is %d bytes; the limit is %d", field, len(ins), AgentSpecMaxPromptBytes)
		}
		if err := checkNoControl(field, ins, true); err != nil {
			return err
		}
		total += len(ins)
	}
	if total > AgentSpecMaxTotalBytes {
		return fmt.Errorf("agentSpec description, prompt and instructions total %d bytes; the limit is %d", total, AgentSpecMaxTotalBytes)
	}
	if err := checkModelHint(a.Model); err != nil {
		return err
	}
	switch a.Access {
	case "", AgentAccessAsk, AgentAccessAllow, AgentAccessSandboxed:
	default:
		return fmt.Errorf("agentSpec.access %q is not one of %q, %q, %q",
			string(a.Access), AgentAccessAsk, AgentAccessAllow, AgentAccessSandboxed)
	}
	if k := a.Knowledge; k != nil {
		if err := k.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (k AgentKnowledge) validate() error {
	if err := checkSandboxPath("agentSpec.knowledge.home", k.Home); err != nil {
		return err
	}
	if err := checkSandboxPath("agentSpec.knowledge.index", k.Index); err != nil {
		return err
	}
	if sharedTemp(k.Home) {
		return fmt.Errorf("agentSpec.knowledge.home: %q is, or contains, a directory shared between sessions", string(k.Home))
	}
	rel, err := filepath.Rel(string(k.Home), string(k.Index))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("agentSpec.knowledge.index %q must be a file inside agentSpec.knowledge.home %q",
			string(k.Index), string(k.Home))
	}
	return nil
}

// checkModelHint bounds a model hint the way every create does for a value that
// may reach a command line: no leading dash (it would read as a flag), no
// whitespace or control characters.
func checkModelHint(m string) error {
	if m == "" {
		return nil
	}
	if len(m) > AgentSpecMaxModelBytes {
		return fmt.Errorf("agentSpec.model is %d bytes; the limit is %d", len(m), AgentSpecMaxModelBytes)
	}
	if strings.HasPrefix(m, "-") {
		return fmt.Errorf("agentSpec.model %q must not start with a hyphen", m)
	}
	for _, r := range m {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("agentSpec.model %q must not contain whitespace or a control character", m)
		}
	}
	return nil
}

// checkNoControl refuses control characters other than, when multiline, the line
// breaks and tabs prose legitimately has. A NUL or an escape in a prompt is never
// prose.
func checkNoControl(field, s string, multiline bool) error {
	for _, r := range s {
		if !unicode.IsControl(r) {
			continue
		}
		if multiline && (r == '\n' || r == '\r' || r == '\t') {
			continue
		}
		return fmt.Errorf("%s contains a control character (U+%04X)", field, r)
	}
	return nil
}
