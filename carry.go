package fleet

import (
	"bytes"
	"encoding/json"
)

// CarriedLaunch is what a create took from the conversation it resumed, in
// place of what the request left unsaid (muster #256).
//
// # Why the service carries it
//
// A session restarted through `resume` is a new process: it launches in the
// runtime's ordinary mode unless the create says otherwise. A caller that
// starts sessions unattended in bypass mode so they never stall on a prompt
// nobody is there to answer would have to remember to say so again on every
// relaunch — and every client that relaunches (a recovery tool, a dashboard's
// rescue path) would have to remember it separately. The one party that saw
// both the old session and the new one is this service, so it carries the
// launch posture across the resume itself and says what it carried.
//
// # What is carried, and what never is
//
// PermissionMode is carried only when the conversation's previous session last
// reported `bypass` — the one non-ordinary mode a create can ask for. Any other
// mode (the ordinary one, accept-edits, plan, auto) is not a launch request,
// so nothing is carried and the new session starts as an absent permissionMode
// always starts. Settings are the launch-time settings the previous session was
// created with (#247/#254); outside bypass only the allow-listed keys travel.
// Consents are never carried: a consent answers one boot question on one
// session, and a standing one would be exactly the permission it refuses to be.
//
// An explicit permissionMode in the request wins over the carried mode, and an
// explicit settings object wins over carried settings; a field the request named
// is never reported here.
//
// An empty CarriedLaunch is never sent: the field is nil instead.
type CarriedLaunch struct {
	// PermissionMode is the mode this create launched in because the
	// conversation's previous session ran in it. Empty when no mode was carried.
	PermissionMode string `json:"permissionMode,omitempty"`

	// Settings is the launch-time settings object this create was given
	// because the conversation's previous session carried it, as the compact
	// JSON the driver put on argv. Empty when no settings were carried.
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Empty reports whether nothing was carried.
func (c CarriedLaunch) Empty() bool {
	return c.PermissionMode == "" && len(c.Settings) == 0
}

// LaunchSettingsAllowedOutsideBypass returns the part of a settings object a
// session that is NOT in bypass mode may carry: the top-level keys on the
// allow-list ValidateLaunchSettings enforces (#254). Nil when raw is absent,
// not an object, or holds none of them.
//
// It exists for the one place a settings object outlives the mode it was valid
// under: a conversation whose previous session ran in bypass, resumed with an
// explicit non-bypass mode. The bypass-only keys stop travelling with the
// mode; the allow-listed ones keep the session's argv as it was.
func LaunchSettingsAllowedOutsideBypass(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &obj); err != nil || obj == nil {
		return nil
	}
	kept := map[string]json.RawMessage{}
	for k, v := range obj {
		if launchSettingsOutsideBypass[k] {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		return nil
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return out
}
