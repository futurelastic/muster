package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
// carries the three answers Respond takes (below), and the ask's own id as its
// nonce.
//
// A failed read of that list is not "nothing pending" (§5.7): the state stays
// `working` and its evidence says the question could not be asked.
//
// # Answering an ask (muster #296)
//
// Respond answers the ask a session is parked on through the runtime's own
// permission-reply endpoint, with one of three replies the prompt's options name
// in order: 1 "Allow once", 2 "Allow always", 3 "Reject". "Always" is the
// runtime's own meaning of it — the patterns the ask offers are approved for the
// rest of that session — and is never narrowed or widened here.
//
// What is refused, and why each is a refusal and not a guess:
//
//   - No choice (0). On a menu that is "accept whatever is highlighted"; this
//     prompt highlights nothing, and the one default a driver could pick is to run
//     a tool nobody read. An unattended caller must name its answer.
//   - Cancel. Dismissing the ask is rejecting it, and rejecting is a decision the
//     caller makes by naming option 3, not one inferred from a dismissal.
//   - Choices and Text. The ask is single-choice with no free-text row.
//   - A nonce that is not the ask now pending. The nonce is the ask's id, which
//     the runtime never reuses, so an answer meant for an ask that has since been
//     resolved can never be applied to the next one. Nothing is sent.
//   - Nothing pending. Answering a session that is not parked is refused, not
//     queued: the runtime has no ask to attach it to.
//
// The reply names the ask by id, so even an answer sent without a nonce lands on
// the ask that was read a moment ago and on no other; what the missing nonce
// loses is the caller's check that it is the one they SHOWED somebody, and the
// receipt says so (the tmux driver's wording, §5.6).
//
// A runtime "question" (its structured ask-the-user surface, a different list
// from permissions) is not mapped onto this shape. A session parked on one is
// answered `unsupported` with the reason, never a guessed choice.

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

// writeSessionConfig writes the session's own opencode.json under dir. Called
// for a fresh session and again for a relaunch, so the file always matches the
// record and the operator's current settings.
//
// Every session this driver starts gets one: it carries the provider policy
// (provider.go, muster #293) and, for a bypass session only, the tool posture
// (muster #283). The two never share a decision — a session with no bypass has
// no `permission` entry at all.
func writeSessionConfig(dir string, bypass bool, providers providerPolicy) error {
	path := filepath.Join(dir, permissionConfigRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("opencode: creating the session's config directory: %w", err)
	}
	cfg := map[string]any{"$schema": "https://opencode.ai/config.json"}
	for k, v := range providers.configKeys() {
		cfg[k] = v
	}
	if bypass {
		cfg["permission"] = bypassPermissions
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
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

// Answers an ask takes (muster #296): the prompt's options, in the order a
// caller's 1-based choice indexes them, and the reply each one sends.
var (
	askOptions = []string{"Allow once", "Allow always", "Reject"}
	askReplies = []string{"once", "always", "reject"}
)

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
		// What Respond takes, in order (muster #296). No Selected: nothing is
		// highlighted, so there is no default for a bare answer to accept.
		Options: append([]string(nil), askOptions...),
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

// wireQuestionAsk is one entry of GET /question: a structured question the
// runtime is waiting on. Only the owner is decoded — this driver never answers
// one, it only needs to know that one is the reason a session is parked.
type wireQuestionAsk struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
}

// pendingQuestion reports whether the runtime holds a question for session id. A
// failed read is "do not know", which the caller treats as no question: the only
// use of the answer is to word a refusal more precisely.
func (d *Driver) pendingQuestion(ctx context.Context, srv *server, id string) bool {
	var qs []wireQuestionAsk
	if err := d.do(ctx, srv, "GET", "/question", nil, &qs); err != nil {
		return false
	}
	for i := range qs {
		if qs[i].SessionID == id {
			return true
		}
	}
	return false
}

// answerAsk applies resp to the ask the runtime is holding for session id.
// Everything that can be decided from the request alone is decided first, so a
// malformed answer costs no round trip and never reaches the runtime.
func (d *Driver) answerAsk(ctx context.Context, srv *server, id string, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	refuse := func(reason string) (fleet.DeliveryReceipt, error) {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
	}
	if err := resp.Validate(); err != nil {
		return refuse(err.Error())
	}
	switch {
	case resp.Cancel:
		return refuse("cancel is not accepted on a tool-permission ask: dismissing it is rejecting it, " +
			"and that is a decision to name. Answer with choice 3 (Reject) if that is what is meant")
	case len(resp.Choices) > 0 || resp.Text != nil:
		return refuse("a tool-permission ask is a single choice with no free-text row: answer it with " +
			"choice (1 Allow once, 2 Allow always, 3 Reject), not choices or text")
	case resp.Choice < 1:
		return refuse("a tool-permission ask highlights nothing, so there is no default to accept: " +
			"answer with choice 1 (Allow once), 2 (Allow always) or 3 (Reject)")
	case resp.Choice > len(askOptions):
		return refuse("no such option on this prompt")
	}

	ask, err := d.pendingAsk(ctx, srv, id)
	if err != nil {
		return fleet.DeliveryReceipt{}, err // an unreadable list is not "nothing pending" (§5.7)
	}
	if ask == nil {
		if d.pendingQuestion(ctx, srv, id) {
			return fleet.DeliveryReceipt{}, &fleet.Error{
				Kind: fleet.ErrorUnsupported,
				Message: "respond: the runtime is holding a structured question for this session, not a " +
					"tool-permission ask. This driver answers tool-permission asks only: a question is never " +
					"mapped onto a guessed choice (§5.6). Interrupt the session to abandon the turn",
				Machine: d.machine,
			}
		}
		return refuse("session is not waiting on a tool-permission ask; nothing was sent")
	}
	if resp.Nonce != "" && resp.Nonce != ask.ID {
		return refuse("the ask changed since it was read; answering now would answer a different " +
			"tool request. Nothing was sent")
	}

	reply := askReplies[resp.Choice-1]
	var applied *bool
	path := "/permission/" + url.PathEscape(ask.ID) + "/reply"
	switch err := d.do(ctx, srv, "POST", path, map[string]string{"reply": reply}, &applied); {
	case err == nil, errors.Is(err, io.EOF):
		// io.EOF: an empty body on a 2xx is the runtime accepting without a verdict.
	case isNotFound(err):
		// Resolved between the read and the reply — by a human at the runtime, by
		// another client, or by the turn being aborted. Refused, not a missing session.
		return refuse("the ask was resolved before this answer arrived; nothing was applied")
	default:
		return fleet.DeliveryReceipt{}, err
	}

	answered := "chose option " + strconv.Itoa(resp.Choice) + " (" + askOptions[resp.Choice-1] + ")"
	if resp.Nonce == "" {
		answered += "; answered without a nonce, so nothing verified the ask had not changed " +
			"since it was read"
	}
	if applied != nil && !*applied {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason:  answered + "; the runtime reported that it did not process the reply",
		}, nil
	}
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted, Reason: answered}, nil
}
