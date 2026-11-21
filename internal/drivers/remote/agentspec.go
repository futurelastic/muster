package remote

import (
	"context"
	"fmt"

	fleet "github.com/futurelastic/muster"
)

// requireAgentSpec refuses forwarding a create that carries a portable agent
// definition (muster #509) to a peer runtime that has not declared it translates
// one — requireSandbox's pattern, with the same failure behind it: a peer on an
// older build ignores a create-body field it does not know, answers 201, and
// starts a session that follows NONE of the definition (no prompt, no knowledge,
// no access) while the caller believes it does.
//
// The evidence is the peer's own row for the runtime the create names, which is
// where supportsAgentSpec lives (it is per runtime, not per machine). Only a
// positive cached answer is trusted; anything else is asked again, so a peer that
// has since been upgraded is not refused on stale evidence. A peer that cannot be
// asked refuses the create: sending it blind is the outcome this exists to stop.
func (d *Driver) requireAgentSpec(ctx context.Context, req fleet.Request, runtime fleet.RuntimeId) error {
	if d.peerTranslatesAgentSpec(runtime) {
		return nil
	}
	if err := d.RefreshCapabilities(ctx, req); err != nil {
		return fmt.Errorf("remote: could not confirm %s translates an agentSpec, so the create was not sent: %w", d.machine, err)
	}
	if d.peerTranslatesAgentSpec(runtime) {
		return nil
	}
	return &fleet.Error{
		Kind:   fleet.ErrorUnsupported,
		Reason: fleet.ReasonAgentSpecUnsupported,
		Message: fmt.Sprintf("peer %s does not translate an agentSpec for this runtime (an older build, or a runtime that cannot); "+
			"the create was not sent, because that peer would accept it and start a session that follows none of the "+
			"definition; name an agent it already knows with `agent` instead (#509)", d.machine),
		Machine: d.machine,
	}
}

// peerTranslatesAgentSpec reports whether the cached row for the runtime a create
// names declares supportsAgentSpec true. A create that names none goes to the
// peer's default runtime, which is the first row the peer listed — the same row
// Capabilities answers with.
func (d *Driver) peerTranslatesAgentSpec(runtime fleet.RuntimeId) bool {
	caps := d.Capabilities()
	if runtime != "" {
		found := false
		for _, row := range d.PeerRuntimes() {
			if row.Runtime == runtime {
				caps, found = row.Capabilities, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return caps.SupportsAgentSpec != nil && *caps.SupportsAgentSpec
}
