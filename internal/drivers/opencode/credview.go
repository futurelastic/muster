package opencode

import (
	"sort"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// Which provider credentials this machine can give a session, as a client reads
// them (muster #497).
//
// A client that wants to know "can this runtime use provider X here" used to have
// two bad ways to find out: run the runtime's own `auth list` from its own PATH
// (a different program, a different home, a different account from the one the
// service starts), or copy the names of this service's sessionEnv entries. The
// service is the party that starts the runtime and holds the key files, the
// sign-in and the provider policy, so it answers.
//
// What is reported, and what is not:
//
//   - A NAME and a STATE, never a value, a key file's path or a token. A reason is
//     a sentence for a person and holds none of those either.
//   - Answered from what the driver already holds, with no request to the runtime
//     or a provider. The one read made is of each declared key file, to say
//     whether a session created now would find a usable value there; it is read
//     fresh, as a create reads it, so a rotation shows at once.
//   - The provider policy applies exactly as it does to the sign-in (grant.go): a
//     credential for a provider the policy does not permit is absent. A
//     credential makes a provider usable; it does not allow it.
//   - Own rows only, like `grants`. A peer's credentials are that peer's to say.
//
// Three sources exist on the wire and this driver reports two:
//
//   - operator-env: a sessionEnv entry that supplies a provider's API key.
//   - grant: the subscription sign-in this service stores and renews.
//   - engine-store (the runtime's own credential store) is NOT reported, because
//     sessions do not read it. Every session runs with an XDG_DATA_HOME of its
//     own (env.go), so the store the service user signed in to is not under any
//     session; listing it would promise a credential no session has. Shared mode
//     (one runtime this driver does not own) cannot say either, and reports none.

// keyEnvProviders is the runtime's own convention for which variable carries which
// provider's API key — the names the runtime reads. It is deliberately SHORT: the
// runtime's provider catalogue moves weekly and many providers share a variable
// (one name serves four providers, another two) or need several; a name this
// table does not list is not reported, because naming a provider the runtime may
// not read it for is the guess this view replaces.
//
// Measured 2026-11-17 against the runtime's own catalogue (its `env` field per
// provider): each name below is listed by exactly one provider, and that is the
// one given. A name added here needs the same check.
var keyEnvProviders = map[string]string{
	"OPENAI_API_KEY":               grant.ProviderOpenAI,
	"ANTHROPIC_API_KEY":            "anthropic",
	"DEEPSEEK_API_KEY":             "deepseek",
	"OPENROUTER_API_KEY":           "openrouter",
	"GROQ_API_KEY":                 "groq",
	"MISTRAL_API_KEY":              "mistral",
	"XAI_API_KEY":                  "xai",
	"CEREBRAS_API_KEY":             "cerebras",
	"COHERE_API_KEY":               "cohere",
	"TOGETHER_API_KEY":             "togetherai",
	"FIREWORKS_API_KEY":            "fireworks-ai",
	"DEEPINFRA_API_KEY":            "deepinfra",
	"GOOGLE_GENERATIVE_AI_API_KEY": "google",
	"GEMINI_API_KEY":               "google",
	"GOOGLE_API_KEY":               "google",
}

// Credentials implements driver.CredentialReporter: the provider credentials a
// session of this driver can be given, in provider order, the API keys before the
// sign-in within a provider.
func (d *Driver) Credentials() []fleet.RuntimeCredential {
	// Shared mode has no per-session environment and no per-session auth file: a
	// server this driver did not start holds whatever it holds, and the service
	// cannot read it without asking the runtime.
	if d.shared != nil {
		return nil
	}
	out := d.envCredentials()
	if g, ok := d.grantRow(); ok {
		out = append(out, fleet.RuntimeCredential{
			Provider: g.Provider,
			Kind:     fleet.CredentialSubscription,
			Source:   fleet.SourceGrant,
			State:    credentialStateOfGrant(g.State),
			Reason:   g.Reason,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Kind == fleet.CredentialAPIKey && out[j].Kind != fleet.CredentialAPIKey
	})
	return out
}

var _ driver.CredentialReporter = (*Driver)(nil)

// credentialStateOfGrant is the grant's state in the credential's vocabulary.
// A state it does not know is not usable, which is how the wire reads one too.
func credentialStateOfGrant(s fleet.GrantState) fleet.CredentialState {
	switch s {
	case fleet.GrantOK:
		return fleet.CredentialUsable
	case fleet.GrantDegraded:
		return fleet.CredentialDegraded
	case fleet.GrantRevoked:
		return fleet.CredentialRevoked
	}
	return fleet.CredentialSignedOut
}

// envCredentials is one row per provider whose API key a sessionEnv entry
// supplies to this runtime. An entry scoped to particular agents or markers still
// counts: the view is of the runtime, and a session in that scope gets it.
//
// Where two entries serve one provider (the names the runtime accepts for it),
// the row says the better of the two: a provider is usable when any of them is.
func (d *Driver) envCredentials() []fleet.RuntimeCredential {
	byProvider := map[string]fleet.RuntimeCredential{}
	entries, linked := d.effectiveSessionEnv()
	for _, e := range entries {
		provider, known := keyEnvProviders[envKey(e.Name)]
		if !known || e.FromFile == "" || !e.AppliesTo.ReachesRuntime(d.runtime) || !d.providers.permits(provider) {
			continue
		}
		row := fleet.RuntimeCredential{
			Provider: provider,
			Kind:     fleet.CredentialAPIKey,
			Source:   fleet.SourceOperatorEnv,
			State:    fleet.CredentialUsable,
		}
		if linked[e.Name] {
			row.Source = fleet.SourceLinked
		}
		if st, why := sessionenv.CheckFile(e.FromFile); st != sessionenv.FileUsable {
			row.State = fleet.CredentialDegraded
			row.Reason = keyFileReason(e, st, why, linked[e.Name])
		}
		if have, dup := byProvider[provider]; dup && have.State == fleet.CredentialUsable {
			continue
		}
		byProvider[provider] = row
	}
	out := make([]fleet.RuntimeCredential, 0, len(byProvider))
	for _, row := range byProvider {
		out = append(out, row)
	}
	return out
}

// keyFileReason says, in a sentence with no path and none of the value, why a
// key file gives a session nothing. The variable's own name is safe and is what the
// operator needs to find the entry. A linked key is not the operator's to repair:
// it was put there through the credentials routes, so the sentence says to link it
// again.
func keyFileReason(e sessionenv.Entry, st sessionenv.FileState, why string, linked bool) string {
	if linked {
		if st == sessionenv.FileAbsent {
			return "the key linked for " + e.Name + " is no longer stored; link it again"
		}
		return "the key linked for " + e.Name + " cannot be used: " + why + "; link it again"
	}
	var reason string
	if st == sessionenv.FileAbsent {
		reason = "the key file declared for " + e.Name + " does not exist"
	} else {
		reason = "the key file declared for " + e.Name + " cannot be used: " + why
	}
	if e.Required {
		return reason + "; a create is refused until an operator repairs it"
	}
	return reason + "; sessions start without it"
}
