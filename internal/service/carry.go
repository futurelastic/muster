package service

import (
	"net/http"

	fleet "github.com/futurelastic/muster"
)

// permissionModeDefaultRequest is the explicit value of a create's
// `permissionMode` that asks for the runtime's ordinary launch mode (muster
// #256).
//
// An absent permissionMode used to be the only way to say that, which was
// enough until a resume could carry a mode forward: with the carry, "absent"
// means "whatever the conversation last ran in", and a caller that wants the
// ordinary mode for a conversation that last ran in bypass needs a way to say
// so that is not silence. It asks for nothing beyond starting a session, so it
// does not need the `send` grant, and the handler turns it back into the empty
// mode before any local driver sees it — a driver's closed set is unchanged.
const permissionModeDefaultRequest = string(fleet.PermissionModeDefault)

// resumeLaunch is what handleCreateSession records about a create it carried
// for, and answers with.
type resumeLaunch struct {
	// carried is what the response reports. Nil when nothing was carried.
	carried *fleet.CarriedLaunch
	// facts is what a later resume of this conversation will read.
	facts *launchFacts
}

// carryResumeLaunch applies muster #256 to a create this machine is serving.
//
// When the body resumes a conversation and does not name a permissionMode, the
// conversation's previous session's last-reported mode is carried — but only
// `bypass`, the one mode a create can ask for (see fleet.CarriedLaunch). When
// it does not carry `settings`, the previous launch's are. An explicit value in
// the request always wins and is never reported as carried. Both are rewritten
// into body, so everything downstream — validation, the driver, the launch
// record — sees one request.
//
// # The authority
//
// Carrying a mode that acts without asking, or a settings object, is the same
// widening a create asks for explicitly, and it needs the same `send` grant
// (createNeedsSend). The grant is read from two places: the caller, or the
// original launch — recorded when it was made, only if it was made by a
// principal holding `send`. A bypass session this service did not launch has no
// such record, so only a caller holding `send` may carry it forward.
//
// A caller that may not is REFUSED, not silently downgraded: a resume that came
// back in the ordinary mode is the failure this exists to end, and a refusal
// names the way out (`permissionMode: "default"`).
func (s *Service) carryResumeLaunch(r *http.Request, machine fleet.MachineId, rt fleet.RuntimeId, body *createSessionBody) (resumeLaunch, *fleet.Error) {
	callerSend := true
	if p, ok := principalOf(r); ok {
		callerSend = p.Allows(GrantSend)
	}

	var rec launchRecord
	var found bool
	if body.Resume != "" {
		rec, found = s.history.launchFor(rt, body.Resume)
	}

	var carried fleet.CarriedLaunch
	sendAuth := callerSend
	if found {
		sendAuth = callerSend || rec.SendAuth
		if body.PermissionMode == "" && rec.Mode == fleet.PermissionModeBypass {
			if !sendAuth {
				return resumeLaunch{}, carryRefusal(machine, "permissionMode \""+fleet.PermissionModeBypass+"\"")
			}
			body.PermissionMode = fleet.PermissionModeBypass
			carried.PermissionMode = fleet.PermissionModeBypass
		}
		if !hasSettings(body) && len(rec.Settings) > 0 {
			st := rec.Settings
			if body.PermissionMode != fleet.PermissionModeBypass {
				st = fleet.LaunchSettingsAllowedOutsideBypass(st)
			}
			if len(st) > 0 {
				if !sendAuth {
					return resumeLaunch{}, carryRefusal(machine, "settings")
				}
				body.Settings = st
				carried.Settings = st
			}
		}
	}

	out := resumeLaunch{}
	if !carried.Empty() {
		out.carried = &carried
	}
	// What this launch gave the session, for the next resume. The explicit
	// "default" is the ordinary mode, which is recorded as the absence of one.
	mode := body.PermissionMode
	if mode == permissionModeDefaultRequest {
		mode = ""
	}
	launch := &launchFacts{
		Mode:         mode,
		Settings:     compactSettings(body),
		SendAuth:     (mode == fleet.PermissionModeBypass || hasSettings(body)) && sendAuth,
		Conversation: firstNonEmpty(body.Resume, body.ConversationId),
	}
	out.facts = launch
	return out, nil
}

func carryRefusal(machine fleet.MachineId, what string) *fleet.Error {
	return &fleet.Error{
		Kind: fleet.ErrorUnauthorized,
		Message: "resuming this conversation would carry " + what + " forward from its previous session, " +
			"which this service did not launch through a principal holding the " + string(GrantSend) +
			" grant, and this principal does not hold it either (§6, #256): send the grant, or ask for the " +
			"ordinary mode explicitly with permissionMode \"" + permissionModeDefaultRequest + "\"",
		Machine: machine,
	}
}

func hasSettings(body *createSessionBody) bool {
	return len(compactSettings(body)) > 0
}

// compactSettings is the body's settings as stored on a launch record: empty
// for an absent field or the JSON literal null, which createNeedsSend and
// ValidateLaunchSettings already read as absent.
func compactSettings(body *createSessionBody) []byte {
	out, err := fleet.ValidateLaunchSettings(body.PermissionMode, body.Settings)
	if err != nil || out == "" {
		return nil
	}
	return []byte(out)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
