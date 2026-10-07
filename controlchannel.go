package fleet

import (
	"encoding/json"
	"fmt"
)

// ControlChannelState is what a runtime says about ITS OWN remote-control
// connection: the channel by which something outside this machine can drive the
// session.
//
// # The boundary, stated first because it is the whole reason this is careful
//
// This layer does not model bridges. Whether a far end is listening, which
// account owns it, whether it was archived from another device — none of that is
// visible here and none of it should be. What IS this layer's business is the
// runtime describing its own state, and "my control channel is down" is exactly
// that: a fact the process reports about itself, on its own screen, in the same
// place it reports every other fact this driver reads.
//
// The distinction is not academic. It decides what the field may be read as: a
// session reporting Failed is one whose RUNTIME believes it is disconnected. It
// is not a statement that the far end agrees, that reconnecting will work, or
// that anybody is waiting. A supervisor that reads more than that into it has
// made the same mistake CredentialGeneration's doc comment warns about — every
// local source ultimately quotes the process's own announcement about itself.
//
// # Why a closed set, and why exactly these five
//
// Four are the runtime's own, read out of its binary rather than sampled from a
// screen (the fifth, off, is a driver-established fact — see ControlChannelOff): the status label it renders is computed from one function with four
// outcomes, and it emits one of `/rc active`, `/rc connecting…`,
// `/rc reconnecting` or `/rc failed`. Nothing else. That is the same standard
// PromptBypassAcceptance's doc comment sets — read the runtime, do not guess
// from a capture — and it is why this can be a closed set at all rather than
// prose somebody matches loosely.
type ControlChannelState string

const (
	// ControlChannelActive: the runtime reports the channel up.
	ControlChannelActive ControlChannelState = "active"
	// ControlChannelConnecting: first connection in progress. Ordinary during
	// startup and not a fault.
	ControlChannelConnecting ControlChannelState = "connecting"
	// ControlChannelReconnecting: the channel dropped and the runtime is
	// retrying by itself. A supervisor that acts on this rather than waiting
	// will interrupt a recovery already under way.
	ControlChannelReconnecting ControlChannelState = "reconnecting"
	// ControlChannelFailed: the runtime has given up. This is the state that
	// motivated the whole field — 37 of 67 sessions came back from a
	// fleet-wide recovery in exactly it, every one of them reporting an
	// otherwise perfectly healthy session, and the only way to find them was
	// to read pane text.
	ControlChannelFailed ControlChannelState = "failed"
	// ControlChannelOff: the runtime has no remote control, and the driver has
	// positive evidence of it (muster #269) — not merely the absence of a label.
	//
	// It is the one value here that is NOT read off the runtime's status label:
	// a session without remote control renders no label at all, so a label can
	// never say "off". The driver establishes it from facts it holds: how the
	// session was launched, and the runtime's own durable record of the last
	// enable or disconnect. A driver that holds neither leaves the field absent,
	// and absent still means "not read" — never "off".
	ControlChannelOff ControlChannelState = "off"
)

func (c ControlChannelState) valid() bool {
	switch c {
	case ControlChannelActive, ControlChannelConnecting,
		ControlChannelReconnecting, ControlChannelFailed, ControlChannelOff:
		return true
	default:
		return false
	}
}

// MarshalJSON rejects a value outside the closed set rather than emitting it.
// A state nobody defined is worse than none: a client branching on four names
// silently falls through to its default for a fifth.
func (c ControlChannelState) MarshalJSON() ([]byte, error) {
	if !c.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid ControlChannelState", string(c))
	}
	return json.Marshal(string(c))
}

// UnmarshalJSON rejects anything outside the set, the empty string included —
// the same discipline Status follows, and for the same reason: an absent value
// and a state named "" are different facts and must not collapse.
func (c *ControlChannelState) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := ControlChannelState(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid ControlChannelState", raw)
	}
	*c = v
	return nil
}

// ControlChannel is a session's remote-control channel as the RUNTIME reports
// it (§2.3). Nil on SessionState means the driver saw nothing to report, which
// covers two situations a caller must not merge:
//
//   - the runtime does not have a control channel at all — a session started
//     without one renders no label, and reporting that as "failed" would invent
//     a fault out of a configuration;
//   - the driver cannot observe one. Ask
//     DriverCapabilities.ObservesControlChannel, which is the field that tells
//     those two apart, and note that an unreached peer reports it `assumed`
//     rather than a bare false (§4.3).
//
// This is §5.7 in the place it is easiest to get wrong: the absence of a
// disconnection notice is not evidence of a connection.
type ControlChannel struct {
	State ControlChannelState `json:"state"`

	// Reason is the runtime's own words about why the channel failed —
	// sourced from its own durable record (muster #69), never from a
	// screen, which is the forgeable region this whole type exists to stay
	// out of. For humans and logs; do not branch on it, the same discipline
	// TurnEnd.Reason already holds itself to. In particular this carries no
	// transient-versus-terminal claim: the runtime's close codes are not
	// classified here (#65's declined inference), so a caller sees the
	// runtime's own sentence — code number included, when the runtime put
	// one in it — and decides for itself.
	//
	// Empty whenever State is not Failed, or when it is Failed but the
	// record could not explain why: no record, an unreadable one, or no
	// matching entry within it. §5.7 applies here exactly as it does to
	// State itself — an empty Reason is "we don't know why", never evidence
	// that there was no reason.
	Reason string `json:"reason,omitempty"`
}
