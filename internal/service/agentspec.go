package service

import (
	"fmt"
	"net/http"
	"slices"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/createpolicy"
)

// This file is the service's half of a portable agent definition on a create
// (muster #509): the checks that are true on any machine, and the lowering of the
// definition's `access` onto the fields every driver already honours. What a
// definition MEANS to a runtime — a plugin directory, a session config entry — is
// each driver's own (docs/spec/session-abstraction.md §2.1).

// SetCreatePolicy installs the operator's per-machine policy over what a create
// may be given (muster #509). Meant to be called at most once, at startup, the
// one-shot rule the other machine settings follow: a rule that cannot be one is a
// message an operator reads once at boot (the same createpolicy.Validate `muster
// doctor` and a reload's refusal run), never a refusal every later create meets.
func (s *Service) SetCreatePolicy(p createpolicy.Policy) error {
	if err := createpolicy.Validate(p, nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.createPolicy = p
	s.mu.Unlock()
	return nil
}

func (s *Service) createPolicySnapshot() createpolicy.Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.createPolicy
}

// checkAgentSpecShape is the part of an agentSpec that is refused on any machine,
// before a driver or a peer is involved: a definition that contradicts the rest of
// the request, or is malformed. A relayed create is pre-checked against it before
// the call crosses to a peer, so a self-contradictory request never pays for a
// round trip it was always going to lose.
//
// Each pair refused here answers one question in two ways: `agent` and `agentSpec`
// both name the persona; the two `model` hints both name the model; `access` and
// the per-runtime posture fields both decide how much the session may do
// unattended. The request is refused rather than letting one silently win.
func checkAgentSpecShape(body createSessionBody, machine fleet.MachineId) *fleet.Error {
	spec := body.AgentSpec
	if spec == nil {
		return nil
	}
	invalid := func(msg string) *fleet.Error {
		return &fleet.Error{Kind: fleet.ErrorInvalid, Message: msg + " (#509)", Machine: machine}
	}
	if body.Agent != "" {
		return invalid("agent and agentSpec are mutually exclusive: one names an agent the runtime already knows, " +
			"the other defines one for this session; send one of the two")
	}
	if body.Model != "" && spec.Model != "" {
		return invalid("model and agentSpec.model are mutually exclusive: send one model hint")
	}
	if spec.Access != "" && (body.Posture != nil || body.Sandbox != nil || body.PermissionMode != "") {
		return invalid("agentSpec.access cannot be combined with posture, sandbox or permissionMode: " +
			"it decides how much the session may do unattended, which each of those also decides; send one of the two")
	}
	if err := spec.Validate(); err != nil {
		return invalid(err.Error())
	}
	return nil
}

// agentSpecNeedsSend reports whether the create's own `access` asks for a session
// that acts without asking, which needs the send grant like permissionMode bypass
// and an unattended posture do (createNeedsSend).
func agentSpecNeedsSend(spec *fleet.AgentSpec) bool {
	return spec != nil && (spec.Access == fleet.AgentAccessAllow || spec.Access == fleet.AgentAccessSandboxed)
}

// applyAgentSpec runs on a create THIS machine serves, after the runtime is
// resolved and before the posture is lowered. It refuses a runtime that does not
// translate a definition, settles the effective access (the operator's policy
// wins over the create's), lowers it, grants the knowledge home under any
// confinement, and hands the model hint to the field every driver already checks.
//
// A relayed create never comes here: the peer that runs the session applies ITS
// operator policy to the definition it is sent, so a policy on this machine cannot
// decide a session another machine runs.
func (s *Service) applyAgentSpec(r *http.Request, machine fleet.MachineId, runtime fleet.RuntimeId, caps fleet.DriverCapabilities, body *createSessionBody) *fleet.Error {
	spec := body.AgentSpec
	if spec == nil {
		return nil
	}
	if caps.SupportsAgentSpec == nil || !*caps.SupportsAgentSpec {
		return &fleet.Error{
			Kind:   fleet.ErrorUnsupported,
			Reason: fleet.ReasonAgentSpecUnsupported,
			Message: fmt.Sprintf("create: runtime %q cannot translate an agentSpec into its own form; "+
				"name an agent it already knows with `agent` instead (#509)", string(runtime)),
			Machine: machine,
		}
	}

	// The effective access: the operator's rule for this runtime and agent name if
	// one matches, else the create's own.
	access, byOperator := spec.Access, false
	match := fleet.SessionSpec{Marker: body.Marker, AgentSpec: spec}
	if op, ok := s.createPolicySnapshot().AccessFor(runtime, match); ok {
		access, byOperator = op, true
	}
	if byOperator && (body.Posture != nil || body.Sandbox != nil || body.PermissionMode != "") {
		// The operator's value wins, so the create's own posture cannot stand beside
		// it; and quietly dropping a stricter posture the caller asked for in favour
		// of a looser value would be a downgrade nobody chose.
		return &fleet.Error{
			Kind: fleet.ErrorInvalid,
			Message: fmt.Sprintf("this machine's createPolicy sets the access of agent %q (%s), which cannot be combined "+
				"with posture, sandbox or permissionMode in the same create; send the definition without them (#509)",
				spec.Name, access),
			Machine: machine,
		}
	}

	// A session that acts without asking needs the send grant whoever chose that:
	// a policy keyed on a name the CLIENT picks must not hand a principal who may
	// only start sessions a session that needs no permission for anything.
	if access == fleet.AgentAccessAllow || access == fleet.AgentAccessSandboxed {
		if p, ok := principalOf(r); ok && !p.Allows(GrantSend) {
			field := "agentSpec.access"
			if byOperator {
				field = "agentSpec.access (set by this machine's createPolicy)"
			}
			return &fleet.Error{
				Kind: fleet.ErrorUnauthorized,
				Message: "principal " + p.Name + " does not hold the " + string(GrantSend) +
					" grant, which " + field + " requires (§6)",
				Machine: machine,
			}
		}
	}

	spec.Access = access
	switch access {
	case fleet.AgentAccessAsk:
		// The explicit ordinary mode, so a resumed conversation that ran in a
		// wider one is not carried forward over an agent that was told to ask.
		body.PermissionMode = string(fleet.PermissionModeDefault)
	case fleet.AgentAccessAllow:
		body.PermissionMode = fleet.PermissionModeBypass
		body.Consents = withConsent(body.Consents, fleet.PromptBypassAcceptance)
	case fleet.AgentAccessSandboxed:
		// Unattended always means confined: the existing posture lowering refuses a
		// runtime that cannot confine, with its own reason. Kept as a posture so the
		// dry run and this create cannot disagree about what "sandboxed" asks for.
		body.Posture = &fleet.Posture{
			Attendance:          fleet.AttendanceUnattended,
			Files:               &fleet.PostureFiles{},
			IsolatedEnvironment: body.IsolateEnvironment,
		}
		if body.IsolateConfiguration {
			body.Posture.Configuration = fleet.ConfigurationGiven
		}
		body.Consents = withConsent(body.Consents, fleet.PromptBypassAcceptance)
	}

	if k := spec.Knowledge; k != nil {
		if ferr := grantKnowledgeHome(machine, body, k); ferr != nil {
			return ferr
		}
	}
	if spec.Model != "" {
		body.Model = spec.Model
	}
	return nil
}

// withConsent adds one boot-question consent, once. A caller that asked for
// `allow` or `sandboxed` has asked for a session that acts without asking, and the
// acceptance screen that mode raises is the question that session cannot be left
// parked in front of.
func withConsent(have []fleet.PromptKind, k fleet.PromptKind) []fleet.PromptKind {
	if slices.Contains(have, k) {
		return have
	}
	return append(slices.Clone(have), k)
}

// grantKnowledgeHome puts the agent's memory home among the paths a confined
// session may read (and write, when the definition asks) — under a sandbox the
// create carries, a posture that confines, or the one `access: sandboxed` just
// built. A session that is not confined can read the home already, so there is
// nothing to grant, and nothing is invented for it.
func grantKnowledgeHome(machine fleet.MachineId, body *createSessionBody, k *fleet.AgentKnowledge) *fleet.Error {
	home := k.Home
	switch {
	case body.Sandbox != nil:
		sb := *body.Sandbox
		sb.ReadPaths = slices.Clone(sb.ReadPaths)
		sb.WritePaths = slices.Clone(sb.WritePaths)
		grantPath(&sb.ReadPaths, &sb.WritePaths, home, k.Write)
		if err := sb.Validate(); err != nil {
			return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "agentSpec.knowledge: " + err.Error() + " (#509)", Machine: machine}
		}
		body.Sandbox = &sb
	case body.Posture != nil && body.Posture.Confines():
		p := *body.Posture
		var files fleet.PostureFiles
		if p.Files != nil {
			files = *p.Files
		}
		files.ReadPaths = slices.Clone(files.ReadPaths)
		files.WritePaths = slices.Clone(files.WritePaths)
		grantPath(&files.ReadPaths, &files.WritePaths, home, k.Write)
		p.Files = &files
		if err := p.Validate(); err != nil {
			return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "agentSpec.knowledge: " + err.Error() + " (#509)", Machine: machine}
		}
		body.Posture = &p
	}
	return nil
}

// grantPath adds home to the write list when write is asked for and to the read
// list otherwise, unless it is already granted at that level or above.
func grantPath(read, write *[]fleet.AbsolutePath, home fleet.AbsolutePath, wantWrite bool) {
	if slices.Contains(*write, home) {
		return
	}
	if wantWrite {
		*write = append(*write, home)
		return
	}
	if !slices.Contains(*read, home) {
		*read = append(*read, home)
	}
}
