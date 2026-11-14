package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/credrule"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/grant"
)

// Which credential an openai session runs on (muster #458).
//
// Since #451 a machine can hold two credentials for the one provider id: a
// subscription sign-in this service renews, and an API key in a session's
// environment. They are separate accounts with separate metering. Measured on the
// real runtime (docs/adr/458-*): with both present it uses the sign-in, ignores the
// key, and offers the sign-in's smaller model list. That order is the runtime's and
// no one chose it, so the service chooses, and enforces the choice by what is ABSENT:
//
//   - subscription: the session's auth file gets the sign-in's access token and the
//     key is removed from its environment;
//   - api-key: the session's auth file gets no sign-in (and loses one an earlier
//     launch wrote) and the key stays.
//
// The other credential never reaches the session, so there is no fallback to
// whichever the runtime would have found: a rule naming a credential that is missing
// leaves the session without the provider, and a create that was bound for the
// provider is refused with the reason.
//
// The choice is the operator's: credrule.Rules, first match wins. With no rule:
// a usable sign-in is used (today's measured behaviour, pinned so a runtime that
// changed its order could not flip it, and it adds no per-token spend), else the
// key. The rule only chooses between credentials; whether `openai` may be used at
// all stays the provider policy's, and where the policy does not permit it nothing
// here applies.

// openaiKeyEnv is the variable the runtime reads an openai API key from.
const openaiKeyEnv = "OPENAI_API_KEY"

// WithCredentialRules sets the operator's rule for which credential a session runs
// on (muster #458). Empty leaves the default.
func WithCredentialRules(rules credrule.Rules) Option {
	return func(d *Driver) { d.credRules = rules }
}

// credInputs is what a decision is made from.
type credInputs struct {
	// rule is the kind the operator's rules name for this session; "" when none does.
	rule fleet.CredentialKind
	// haveKey: the session's environment, as built, carries the key.
	haveKey bool
	// haveGrant: a usable sign-in is held and its provider is reachable by it.
	haveGrant bool
}

// credDecision is the outcome.
type credDecision struct {
	// kind is the credential the session runs on; "" when it has none to choose.
	kind fleet.CredentialKind
	// missing is the credential the rule names that is not there; "" when none.
	missing fleet.CredentialKind
}

// decide is the whole policy, free of I/O so the table can be read and tested whole.
func decide(in credInputs) credDecision {
	switch in.rule {
	case fleet.CredentialSubscription:
		if in.haveGrant {
			return credDecision{kind: fleet.CredentialSubscription}
		}
		return credDecision{missing: fleet.CredentialSubscription}
	case fleet.CredentialAPIKey:
		if in.haveKey {
			return credDecision{kind: fleet.CredentialAPIKey}
		}
		return credDecision{missing: fleet.CredentialAPIKey}
	}
	switch {
	case in.haveGrant:
		return credDecision{kind: fleet.CredentialSubscription}
	case in.haveKey:
		return credDecision{kind: fleet.CredentialAPIKey}
	}
	return credDecision{}
}

// stripsKey reports whether the session's environment must not carry the key: it
// runs on the subscription, or is meant to and has none (no fallback to the key).
func (c credDecision) stripsKey() bool {
	return c.kind == fleet.CredentialSubscription || c.missing == fleet.CredentialSubscription
}

// credentialPlan is a decided credential, ready to apply to a session directory.
type credentialPlan struct {
	kind   fleet.CredentialKind
	access grant.Access // meaningful only for the subscription
}

// credentialApplies reports whether this driver chooses between credentials for a
// session at all: the isolated mode (shared mode has no per-session directory) and a
// provider policy that permits openai. Where it is false nothing changes.
func (d *Driver) credentialApplies() bool {
	return d.shared == nil && d.providers.permits(grant.ProviderOpenAI)
}

// envHasKey reports whether env carries a non-empty openai key.
func envHasKey(env map[string]string) bool {
	return env[envKey(openaiKeyEnv)] != ""
}

// ruleFor is the kind the operator's rules name for a session, "" for none.
func (d *Driver) ruleFor(spec fleet.SessionSpec) fleet.CredentialKind {
	kind, _ := d.credRules.Choose(grant.ProviderOpenAI, d.runtime, spec)
	return kind
}

// planCredential decides, at a create or a relaunch, which credential the session
// runs on, and removes the one it does not from env (the merged variables the
// process will receive). It runs before any directory or process exists, so a
// refusal costs nothing.
func (d *Driver) planCredential(ctx context.Context, spec fleet.SessionSpec, env map[string]string) (credentialPlan, error) {
	if !d.credentialApplies() {
		return credentialPlan{}, nil
	}
	rule := d.ruleFor(spec)
	if rule == fleet.CredentialSubscription {
		// The caller's own key is refused rather than dropped: the rule is the
		// operator's and a create does not override it, and a variable that
		// vanishes without a word is the surprise this rule exists to remove.
		if folded, err := normalizeEnvNames(spec.Env, envKey); err == nil && envHasKey(folded) {
			return credentialPlan{}, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: d.machine, Message: "create: env: " +
				openaiKeyEnv + " is not accepted: this machine's rule runs " + grant.ProviderOpenAI +
				" sessions like this one on the subscription, and a create does not choose the credential"}
		}
	}
	in := credInputs{rule: rule, haveKey: envHasKey(env)}
	var access grant.Access
	var grantErr error
	if d.grants != nil && rule != fleet.CredentialAPIKey {
		access, grantErr = d.grants.Access(ctx)
		if grantErr != nil && ctx.Err() != nil {
			// muster #476: the renewal was cut short by the request's own deadline
			// (or the caller leaving), so "no usable sign-in" would be untrue; the
			// sign-in was never found wanting. That is time running out, not a
			// refusal, and a retry gives the renewal its full time.
			return credentialPlan{}, &driver.StepTimeout{Step: stepCredential, Err: ctx.Err()}
		}
		in.haveGrant = grantErr == nil
		if grantErr != nil && !errors.Is(grantErr, grant.ErrSignedOut) {
			d.say("opencode: this session was started without the openai sign-in: %v", grantErr)
		}
	}
	dec := decide(in)
	if dec.stripsKey() {
		delete(env, envKey(openaiKeyEnv))
	}
	if dec.missing != "" && d.boundForOpenAI(spec) {
		return credentialPlan{}, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: d.machine,
			Message: "create: " + d.missingCredentialReason(dec.missing, grantErr)}
	}
	return credentialPlan{kind: dec.kind, access: access}, nil
}

// missingCredentialReason is the sentence a refused create carries.
func (d *Driver) missingCredentialReason(missing fleet.CredentialKind, grantErr error) string {
	rules := d.credRules.Describe()
	switch missing {
	case fleet.CredentialSubscription:
		why := "no usable sign-in is held"
		if grantErr != nil {
			why = grantErr.Error()
		} else if d.grants == nil {
			why = "this service holds no sign-in (it needs a state directory and `muster grant login`)"
		}
		return "this machine's rule runs " + grant.ProviderOpenAI + " sessions like this one on the subscription (" +
			rules + "), and " + why + "; sign in, or change the rule. The key is not used in its place"
	default:
		return "this machine's rule runs " + grant.ProviderOpenAI + " sessions like this one on an API key (" + rules +
			"), and no " + openaiKeyEnv + " reaches the session (a sessionEnv entry in scope supplies it); " +
			"add one, or change the rule. The sign-in is not used in its place"
	}
}

// boundForOpenAI reports whether the session is certain to need the provider: its
// create pins a model on it, or the policy names it alone. A session that can run on
// something else is not refused for a credential it may never use; it simply has none
// of the other.
func (d *Driver) boundForOpenAI(spec fleet.SessionSpec) bool {
	if spec.Model != "" {
		return providerOfModel(spec.Model) == grant.ProviderOpenAI
	}
	return !d.providers.open() && len(d.providers.enabled) == 1 && d.providers.enabled[0] == grant.ProviderOpenAI
}

// applyCredential puts the plan into the session directory before its server
// starts. The subscription gets the sign-in's access token; anything else gets no
// sign-in, including none left by an earlier launch of the same directory (a
// relaunch under a changed rule).
func (d *Driver) applyCredential(dir string, p credentialPlan) error {
	if !d.credentialApplies() {
		return nil
	}
	if p.kind == fleet.CredentialSubscription {
		return writeSessionAuth(dir, p.access, true)
	}
	return removeSessionAuth(dir)
}

// removeSessionAuth drops the provider's entry from the session's auth file, if
// there is one. The rest of the file, and a directory that does not exist yet, are
// left alone.
func removeSessionAuth(dir string) error {
	path := authPath(dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opencode: reading the session's auth file: %w", err)
	}
	entries := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &entries) == nil && entries != nil {
		if _, has := entries[grant.ProviderOpenAI]; !has {
			return nil
		}
		delete(entries, grant.ProviderOpenAI)
	} else {
		// Not a file the runtime could use either; it is this driver's own, so it
		// is replaced rather than kept.
		entries = map[string]json.RawMessage{}
	}
	return writeAuthFile(path, entries)
}

// credentialOfRecord is the credential a session rebuilt from its record is on,
// without starting anything: used for a parked session, whose server is not running
// but whose usage is still reported. The sign-in counts as held when its state is
// not signed-out or revoked, which is what a start would find unless a refresh lapses
// in between.
func (d *Driver) credentialOfRecord(spec fleet.SessionSpec, env map[string]string) fleet.CredentialKind {
	if !d.credentialApplies() {
		return ""
	}
	in := credInputs{rule: d.ruleFor(spec), haveKey: envHasKey(env)}
	if d.grants != nil && in.rule != fleet.CredentialAPIKey {
		switch d.grants.State().Phase {
		case grant.PhaseOK, grant.PhaseDegraded:
			in.haveGrant = true
		}
	}
	return decide(in).kind
}

// credentialOf is the credential a known session runs on, "" when not known.
func (d *Driver) credentialOf(id string) fleet.CredentialKind {
	k, ok := d.wasSeen(id)
	if !ok || k.srv == nil {
		return ""
	}
	return k.srv.credential
}
