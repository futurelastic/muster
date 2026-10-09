package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// Tool permissions (muster #283).
//
// opencode asks before it runs a tool the configuration does not already allow,
// and a session nobody is watching then stops at the ask for ever. This driver
// answers that in two separate halves, and keeps them apart on purpose.
//
// # Pre-approval: only inside a boundary
//
// `permissionMode: "bypass"` is honoured ONLY when the same create asks for a
// sandbox this driver enforces (sandbox.go). Approving every tool is safe
// exactly when something other than the approval is the boundary; with no such
// boundary the request is refused `unsupported` with a reason saying so, never
// narrowed to a weaker posture under the same name (§2.1: refuse rather than
// drop a hint silently). Shared mode refuses it too: one runtime serves every
// session there, so the posture could not belong to one of them.
//
// The posture reaches the runtime through the session's OWN configuration —
// opencode.json under that session's XDG_CONFIG_HOME, which is inside the
// per-session directory env.go builds — and `--pure` on its command line, which
// keeps external plugins out. Neither is ever taken from a machine-wide file
// another session would also read: the environment is built, so the user's
// config is not in it.
//
// # A parked ask reads as blocked, never as busy
//
// A session can still park on an ask the configuration did not cover (a tool
// outside the three it names, or a project file in the working directory that
// sets its own rules). The status endpoint calls such a session `busy` — its turn
// has not ended — so a supervisor reading only status would wait for a turn that
// never will. GET /permission lists what the runtime is waiting to be told;
// readState consults it whenever the status is busy and, finding an ask for this
// session, reports `waiting_input` with a `tool-permission` prompt. The prompt
// carries no options, because Respond stays unsupported here: this is a way to
// SEE the block, not to answer it.
//
// A failed read of that list is not "nothing pending" (§5.7): the state stays
// `working` and its evidence says the question could not be asked.

// permissionConfigRel is where the runtime reads the session's own configuration,
// relative to the session directory.
var permissionConfigRel = filepath.Join("xdg", "config", "opencode", "opencode.json")

// pureFlag keeps external plugins out of a pre-approved session: a plugin loaded
// from a user's package list would run with every tool already approved.
const pureFlag = "--pure"

// bypassPermissions is the posture a bypass session is configured with: the three
// tool families that ask by default, allowed.
var bypassPermissions = map[string]string{
	"edit":     "allow",
	"bash":     "allow",
	"webfetch": "allow",
}

// bypassArgs are the command-line arguments a bypass session's server is started
// with, beyond the ones every session gets.
func bypassArgs() []string { return []string{pureFlag} }

// writeBypassConfig writes the session's own opencode.json under dir. Called for a
// fresh session and again for a relaunch, so the file always matches the record.
func writeBypassConfig(dir string) error {
	path := filepath.Join(dir, permissionConfigRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("opencode: creating the session's config directory: %w", err)
	}
	raw, err := json.MarshalIndent(map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"permission": bypassPermissions,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode: encoding the session's config: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("opencode: writing the session's config: %w", err)
	}
	return nil
}

// checkBypass decides whether a create asking for permissionMode may proceed.
// Nil when it did not ask. A non-nil error is the refusal to return as-is.
func (d *Driver) checkBypass(spec fleet.SessionSpec) error {
	if spec.PermissionMode == "" {
		return nil
	}
	refuse := func(why string) error {
		return &fleet.Error{
			Kind:    fleet.ErrorUnsupported,
			Message: "create: permissionMode " + fmt.Sprintf("%q", spec.PermissionMode) + " is refused: " + why,
			Machine: d.machine,
		}
	}
	if spec.PermissionMode != fleet.PermissionModeBypass {
		return refuse("this driver knows only \"bypass\"")
	}
	if d.shared != nil {
		return refuse("shared mode runs one runtime for every session, so a permission posture cannot be given to one of them")
	}
	if spec.Sandbox == nil {
		return refuse("this driver pre-approves every tool only inside a sandbox it enforces, and this create asked for none " +
			"(add a sandbox to the same create, or leave permissionMode out)")
	}
	// A sandbox that was asked for but cannot be enforced is refused earlier in
	// Create, with the capability's own reason: bypass never reaches a session
	// that is not actually confined.
	return nil
}

// wirePermissionAsk is one entry of GET /permission: a tool the runtime is
// waiting to be told it may run. Only what a supervisor needs is decoded.
type wirePermissionAsk struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionID"`
	Permission string   `json:"permission"`
	Patterns   []string `json:"patterns"`
}

// pendingAsk returns the oldest ask the runtime is holding for session id, or
// nil when the read succeeded and there is none. An error means the question
// could not be asked, which is not the same as "no".
func (d *Driver) pendingAsk(ctx context.Context, srv *server, id string) (*wirePermissionAsk, error) {
	var asks []wirePermissionAsk
	if err := d.do(ctx, srv, "GET", "/permission", nil, &asks); err != nil {
		return nil, err
	}
	for i := range asks {
		if asks[i].SessionID == id {
			return &asks[i], nil
		}
	}
	return nil, nil
}

// askQuestionMax bounds the question text: a pattern is a command line and can be long.
const askQuestionMax = 240

// blockedOnAsk is the state of a session whose turn is stopped on ask.
func blockedOnAsk(ask *wirePermissionAsk) fleet.SessionState {
	q := "the runtime is asking permission to use " + ask.Permission
	if len(ask.Patterns) > 0 {
		q += ": " + strings.Join(ask.Patterns, ", ")
	}
	if r := []rune(q); len(r) > askQuestionMax {
		q = string(r[:askQuestionMax]) + "…"
	}
	st := fleet.ObservedState(fleet.StatusWaitingInput,
		"the runtime holds a tool-permission ask for this session; its turn cannot end until the ask is answered", nil)
	st.WaitingOn = fleet.WaitingPrompt
	st.Prompt = &fleet.SessionPrompt{
		Question: q,
		// No options: Respond is unsupported by this driver, so there is nothing
		// a caller could pick. The prompt exists so the block is SEEN.
		Options: []string{},
		Kind:    fleet.PromptToolPermission,
		Nonce:   ask.ID,
	}
	return st
}

// withPermission refines a state classify gave a session that is not idle: if the
// runtime is holding an ask for it, the session is blocked, not busy. Any other
// status is returned unchanged.
func (d *Driver) withPermission(ctx context.Context, srv *server, id string, st fleet.SessionState) fleet.SessionState {
	if st.Status != fleet.StatusWorking {
		return st
	}
	ask, err := d.pendingAsk(ctx, srv, id)
	if err != nil {
		st.Evidence += " (whether the runtime is waiting on a permission ask could not be read: " + err.Error() + ")"
		return st
	}
	if ask == nil {
		return st
	}
	blocked := blockedOnAsk(ask)
	blocked.Since, blocked.LastTurn = st.Since, st.LastTurn
	return blocked
}
