// Package credrule holds the operator's rule for which credential a session runs
// on when a provider can be reached two ways (muster #458).
//
// Since #451 a machine can hold two credentials for the same provider id: a
// subscription sign-in the service renews, and an API key in a session's
// environment. They are separate accounts with separate metering, and the
// runtime, left to itself, takes whichever it finds first (measured: the
// sign-in wins over the key, and the whole model list becomes the sign-in's).
// That is not a decision anyone made, so the operator makes it here, in the
// configuration file, per machine and per runtime / agent / marker with the same
// selector shape a sessionEnv entry has.
//
// A rule only CHOOSES between credentials. Whether the provider may be used at
// all is the provider policy's, and nothing here widens it.
package credrule

import (
	"fmt"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// Providers lists the provider ids that can be reached two ways. Today that is
// one, and a rule naming any other is refused rather than silently inert.
var Providers = []string{"openai"}

// Rule is one operator-declared choice.
type Rule struct {
	// Provider is the runtime's own id for the provider ("openai").
	Provider string
	// Use is the credential kind sessions in scope run on.
	Use fleet.CredentialKind
	// AppliesTo scopes the rule; a scope naming no axis matches every session.
	AppliesTo sessionenv.Scope
}

// Rules is an ordered list. The first rule whose scope matches a session decides:
// a rule is a decision, so the order the operator wrote it in is the operator's
// own statement of which one wins, and no other order exists to disagree with it.
type Rules []Rule

// Validate checks every rule's shape once, at startup, so a mistake is a message
// an operator reads once rather than a refusal every later create meets.
//
// A rule after one that matches every session can never apply, and is refused:
// the operator wrote something that does nothing, which is the same fault a
// misspelt runtime id is.
//
// knownRuntimes is the set of runtime ids this build can register; nil skips
// that check, for a caller that wants only the shape.
func Validate(rules Rules, knownRuntimes []fleet.RuntimeId) error {
	for i, r := range rules {
		at := fmt.Sprintf("providerCredentials[%d]", i)
		if !knownProvider(r.Provider) {
			return fmt.Errorf("%s: provider %q has no choice of credential here (want one of: %s)",
				at, r.Provider, strings.Join(Providers, ", "))
		}
		if r.Use != fleet.CredentialSubscription && r.Use != fleet.CredentialAPIKey {
			return fmt.Errorf("%s: use %q must be %q or %q", at, r.Use,
				fleet.CredentialSubscription, fleet.CredentialAPIKey)
		}
		for _, id := range r.AppliesTo.Runtimes {
			if id == "" {
				return fmt.Errorf("%s: appliesTo.runtimes has an empty runtime id", at)
			}
			if knownRuntimes != nil && !containsRuntime(knownRuntimes, id) {
				return fmt.Errorf("%s: appliesTo.runtimes names %q, which is not a runtime this service has", at, id)
			}
		}
		for j := 0; j < i; j++ {
			if rules[j].Provider == r.Provider && covers(rules[j].AppliesTo, r.AppliesTo) {
				return fmt.Errorf("%s can never apply: providerCredentials[%d] already decides every session it would match",
					at, j)
			}
		}
	}
	return nil
}

// covers reports whether every session that later can match is already matched by
// earlier, so a rule written after it is unreachable. Only a scope naming no agent
// and no marker covers anything: it matches every session on its runtimes (or on
// every runtime when it names none).
func covers(earlier, later sessionenv.Scope) bool {
	if len(earlier.Agents) > 0 || len(earlier.Markers) > 0 {
		return false
	}
	if len(earlier.Runtimes) == 0 {
		return true
	}
	if len(later.Runtimes) == 0 {
		return false
	}
	for _, id := range later.Runtimes {
		found := false
		for _, e := range earlier.Runtimes {
			if e == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Choose returns the credential kind the operator's rules name for a session on
// runtime with spec, and whether any rule did. Without a rule the caller applies
// its own documented default; this package does not invent one.
func (rs Rules) Choose(provider string, runtime fleet.RuntimeId, spec fleet.SessionSpec) (fleet.CredentialKind, bool) {
	for _, r := range rs {
		if r.Provider == provider && r.AppliesTo.Matches(runtime, spec) {
			return r.Use, true
		}
	}
	return "", false
}

// Describe is the rules in words, in the order they are tried, for a refusal or
// the doctor to quote. Empty for no rules.
func (rs Rules) Describe() string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%s on %s for %s", r.Provider, r.Use, describeScope(r.AppliesTo)))
	}
	return strings.Join(parts, "; ")
}

func describeScope(s sessionenv.Scope) string {
	if matchesAll(s) {
		return "every session"
	}
	var parts []string
	if len(s.Runtimes) > 0 {
		parts = append(parts, "runtimes "+strings.Join(s.Runtimes, ", "))
	}
	if len(s.Agents) > 0 {
		parts = append(parts, "agents "+strings.Join(s.Agents, ", "))
	}
	if len(s.Markers) > 0 {
		parts = append(parts, "markers "+strings.Join(s.Markers, ", "))
	}
	return strings.Join(parts, " and ")
}

func matchesAll(s sessionenv.Scope) bool {
	return len(s.Runtimes) == 0 && len(s.Agents) == 0 && len(s.Markers) == 0
}

func knownProvider(id string) bool {
	for _, p := range Providers {
		if p == id {
			return true
		}
	}
	return false
}

func containsRuntime(known []fleet.RuntimeId, id string) bool {
	for _, k := range known {
		if string(k) == id {
			return true
		}
	}
	return false
}
