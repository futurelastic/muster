package opencode

import (
	"fmt"
	"strings"
)

// Which providers a session may use (muster #293).
//
// opencode ships a hosted provider of its own. With no credential configured it
// is the one that answers: a measured session with nothing configured ran its
// first turn on it, and muster reported a normal idle session. Prompts, file
// contents and tool output went to a third-party service that nobody chose.
//
// So the posture is fail-closed and it is the operator's, never the caller's:
//
//   - By default the runtime's built-in hosted provider is disabled
//     (disabled_providers in the session's own configuration).
//   - WithProviders names the providers a session may use and nothing else
//     (enabled_providers). Naming the built-in provider there is how an
//     operator who wants it asks for it, on purpose.
//   - A create the policy leaves with no usable provider is refused with the
//     reason, before any session exists (catalog.go).
//   - A turn that nonetheless ran on a provider outside the set is reported as
//     a failed turn naming the provider (state.go), never as an unremarkable
//     idle.
//
// The policy reaches the runtime through the session's OWN opencode.json
// (permission.go), written for every session this driver starts — at create and
// again at relaunch — so the file always matches what the operator configured
// now. Shared mode (one runtime this driver does not own) has no such file, so
// there the catalog check and the turn report are the whole of it.

// builtinProvider is the id of the hosted provider opencode ships with.
const builtinProvider = "opencode"

// providerPolicy is the set of providers a session may use. The zero value is
// the default: everything the runtime offers except builtinProvider.
type providerPolicy struct {
	// allow, when non-empty, is the whole set; nothing outside it is usable.
	allow []string
}

// newProviderPolicy builds a policy from operator-supplied ids, trimming blanks
// and repeats. It refuses an id that cannot be one: the runtime's provider ids
// are single tokens, and "provider/model" here would silently match nothing.
func newProviderPolicy(ids []string) (providerPolicy, error) {
	var p providerPolicy
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		if strings.ContainsAny(id, "/ \t") {
			return providerPolicy{}, fmt.Errorf("opencode: provider %q is not a provider id (a provider id has no \"/\" or space; "+
				"the model goes in a create's model, as provider/model)", id)
		}
		seen[id] = true
		p.allow = append(p.allow, id)
	}
	return p, nil
}

// permits reports whether a session may use provider id.
func (p providerPolicy) permits(id string) bool {
	if len(p.allow) == 0 {
		return id != builtinProvider
	}
	for _, a := range p.allow {
		if a == id {
			return true
		}
	}
	return false
}

// configKeys are the opencode.json entries that express the policy.
// enabled_providers is a whitelist and wins over disabled_providers in the
// runtime, so only one of the two is ever written.
func (p providerPolicy) configKeys() map[string]any {
	if len(p.allow) > 0 {
		return map[string]any{"enabled_providers": p.allow}
	}
	return map[string]any{"disabled_providers": []string{builtinProvider}}
}

// describe is the policy in words a refusal can quote.
func (p providerPolicy) describe() string {
	if len(p.allow) == 0 {
		return fmt.Sprintf("any provider except the runtime's built-in hosted provider %q (disabled by default)", builtinProvider)
	}
	return "only " + strings.Join(quoteAll(p.allow), ", ")
}

// providerOfModel is the provider half of a "provider/model" id.
func providerOfModel(model string) string {
	id, _, _ := strings.Cut(model, "/")
	return id
}
