package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"

	fleet "github.com/futurelastic/muster"
)

// Operator agents (muster #396).
//
// An isolated session has its own HOME and XDG directories (env.go), so it never
// reads the service user's opencode configuration, and the agent a create pins is
// checked against what the runtime lists for that session (agents.go): its
// built-ins plus the project agents under the working directory. An operator who
// wants one persona on every session of a machine, without asking each repo to
// commit it, therefore has nowhere to define it.
//
// WithOperatorAgents names a JSON file for that. It is the machine's own, and:
//
//   - It is a configuration FRAGMENT with exactly one top-level key,
//     {"agent": {"<name>": {<an opencode agent definition>}}} — the same object
//     opencode.json carries under `agent`, so a file an operator already keeps for
//     an inline-configuration workaround is usable as it is. Any other top-level
//     key is refused: a provider, a permission or a tool server in this file would
//     be a way around the provider policy (provider.go) and the permission posture
//     (permission.go), which are the driver's to write, not this file's.
//   - It is read on EVERY create and every relaunch, never cached. The declaration
//     (which path) is fixed at start, like a sessionEnv entry; what the file holds
//     is read afresh, so editing an agent needs no restart.
//   - It reaches the runtime the way the provider policy does: through the
//     session's OWN opencode.json (writeSessionConfig), so the runtime's agent
//     list, the create's agent check and the per-message pin all see it.
//   - A file that cannot be used REFUSES the create, with the reason. It is never
//     skipped: a session that was meant to run as the operator's agent and
//     silently ran as the default one is the failure this exists to prevent.
//
// # Which wins when a project defines the same name
//
// Measured against opencode 1.18.3 with an isolated HOME (the integration test
// pins it): the operator's definition is the session's global one, so a project
// agent of the same name — a `.opencode/agents/<name>.md` file or an `agent` entry
// in the project's opencode.json — wins. The merge is field by field, not a
// replacement: a project agent that sets only a description and a prompt leaves
// the operator's model and temperature in force. An inline OPENCODE_CONFIG_CONTENT
// (a sessionEnv entry can still supply one) ranks above both, field by field, and
// a blob with no `agent` entry leaves operator agents alone.
//
// Why JSON and not a directory of markdown agent files, the runtime's other
// spelling: the runtime LISTS an agent whose markdown frontmatter is malformed,
// missing or empty (as a "mode all" agent whose prompt is the whole text), so a
// broken definition would run silently as the wrong agent. The JSON form is
// validated strictly, and a project's agent then beats it whichever spelling the
// project used; operator markdown beats a project's opencode.json entry instead.
//
// The runtime validates each definition itself and does not start with an invalid
// one, so a bad field (a mode that is none of subagent, primary and all, say)
// surfaces as the existing refusal for a server that exits before it answers.
// The definition is visible to the session like any agent prompt: it is not a
// secret channel.

// operatorAgentsMaxBytes bounds the file. A definition is a prompt and a few
// settings; a megabyte is far past any real one and keeps a wrong path (a log, a
// database) from being read into memory.
const operatorAgentsMaxBytes = 1 << 20

// operatorAgents are the definitions to write, keyed by agent name. Each value is
// the definition exactly as the operator wrote it: this driver neither interprets
// nor rewrites it, the runtime does.
type operatorAgents map[string]json.RawMessage

// readOperatorAgents reads and checks the operator's agent file. The error says
// what is wrong with it in words an operator can act on; it never quotes the
// file's content.
func readOperatorAgents(path string) (operatorAgents, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the operator's agent file cannot be read: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, operatorAgentsMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("the operator's agent file cannot be read: %w", err)
	}
	if len(raw) > operatorAgentsMaxBytes {
		return nil, fmt.Errorf("the operator's agent file is larger than %d bytes", operatorAgentsMaxBytes)
	}
	return parseOperatorAgents(raw)
}

// parseOperatorAgents is the file's shape, separate from reading it.
func parseOperatorAgents(raw []byte) (operatorAgents, error) {
	top, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("the operator's agent file is not a JSON object: %w", err)
	}
	var agentRaw json.RawMessage
	var extra []string
	for _, kv := range top {
		if kv.key == "agent" {
			agentRaw = kv.value
			continue
		}
		extra = append(extra, kv.key)
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("the operator's agent file may carry only an \"agent\" entry, and it also has %s "+
			"(providers, permissions and tool servers are set by the service, not by this file)", strings.Join(quoteAll(extra), ", "))
	}
	if agentRaw == nil {
		return nil, errors.New("the operator's agent file has no \"agent\" entry")
	}
	defs, err := decodeObject(agentRaw)
	if err != nil {
		return nil, fmt.Errorf("the \"agent\" entry of the operator's agent file is not a JSON object: %w", err)
	}
	out := make(operatorAgents, len(defs))
	for _, kv := range defs {
		if err := checkAgentName(kv.key); err != nil {
			return nil, err
		}
		if !isJSONObject(kv.value) {
			return nil, fmt.Errorf("operator agent %q is not a JSON object", kv.key)
		}
		out[kv.key] = kv.value
	}
	return out, nil
}

// checkAgentName refuses a name the runtime could not be asked for: an agent is
// named in a create and in every message, so a blank or control-carrying name
// could never be pinned and would only look defined.
func checkAgentName(name string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("operator agent name %q must not be empty or start or end with a space", name)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("operator agent name %q must not contain a control character", name)
		}
	}
	return nil
}

// isJSONObject reports whether v is a JSON object (not null, an array or a scalar).
func isJSONObject(v json.RawMessage) bool {
	return len(bytes.TrimLeft(v, " \t\r\n")) > 0 && bytes.TrimLeft(v, " \t\r\n")[0] == '{'
}

// member is one entry of a JSON object, in the order it was written.
type member struct {
	key   string
	value json.RawMessage
}

// decodeObject reads raw as ONE JSON object and returns its members in order. It
// is used instead of decoding into a map because a map silently keeps the last of
// two equal keys, and an agent defined twice in a file is a mistake to report,
// not to resolve by position.
func decodeObject(raw []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("it is not an object")
	}
	var out []member
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		if seen[key] {
			return nil, fmt.Errorf("%q appears twice", key)
		}
		seen[key] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, member{key: key, value: v})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("there is more after the object")
	}
	return out, nil
}

// operatorAgentsFor reads this machine's operator agents for one session start.
// Called before any side effect, at create and again at relaunch. Nil when none
// are configured.
func (d *Driver) operatorAgentsFor() (operatorAgents, error) {
	if d.agentsFile == "" {
		return nil, nil
	}
	agents, err := readOperatorAgents(d.agentsFile)
	if err != nil {
		// The operator's file is at fault, not the request: retrying the same
		// create cannot help until it is repaired (the same kind a required
		// sessionEnv entry that cannot be provisioned answers with).
		return nil, &fleet.Error{
			Kind: fleet.ErrorUnsupported,
			Message: "create: " + err.Error() + " (the machine's opencode agents, " + d.agentsFile + "); " +
				"an operator must repair that, and retrying this create will not help until they do",
			Machine: d.machine,
		}
	}
	return agents, nil
}
