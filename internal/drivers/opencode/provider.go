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
//
// The policy can also name single models (muster #401): an entry may be a
// provider (every model on it), provider/<model> or provider/<glob> (only the
// matching models), or !provider/<glob> (a deny that beats any allow). The
// operator-facing form to prefer is an allowlist of EXACT models, because a name
// pattern is not a reliable mark of anything: a suffix such as "-free" does not
// pick out the zero-cost models of a hosted provider, which also serves some
// without it. A deny glob is for the models an operator knows by a stable name,
// never a stand-in for a pricing or data-retention filter.
//
// Model rules never reach the session's opencode.json. The file can only enable
// whole providers, and a glob could only be expanded against a catalog that is
// read from the very server the file configures. They are enforced where this
// driver can act on them: when a create pins a model, when a create pins none
// and the runtime's default must be learned (catalog.go), and on the turn's own
// record (state.go). The provider-level file stays the outer wall.

// builtinProvider is the id of the hosted provider opencode ships with.
const builtinProvider = "opencode"

// modelRule is one entry of a policy that names models: the provider, a pattern
// over the model id on it, and the entry as the operator wrote it ("!" included),
// which is what a refusal quotes.
type modelRule struct {
	provider string
	pattern  string
	raw      string
}

// matches reports whether the rule's pattern matches model.
func (r modelRule) matches(model string) bool { return matchGlob(r.pattern, model) }

// providerPolicy is what a session may use. The zero value is the default:
// everything the runtime offers except builtinProvider.
type providerPolicy struct {
	// allow is the providers that are allowed whole, in config order.
	allow []string
	// models is the provider/<model> and provider/<glob> entries: only the models
	// they match are allowed on their provider.
	models []modelRule
	// deny is the !provider/<glob> entries. A deny beats any allow.
	deny []modelRule
	// enabled is every provider the runtime must have on for the policy to be
	// usable: the whole-allowed ones and the ones only partly allowed, each once,
	// in the order the entries first named them. For a policy of providers alone
	// it is allow.
	enabled []string
	// allowed is the allow entries as written (providers and provider/<pattern>),
	// in config order, for describe.
	allowed []string
}

// open reports whether the policy allows no provider by name — the default
// posture, where everything but the built-in provider is usable (denies, if any,
// then carve models out of that).
func (p providerPolicy) open() bool { return len(p.allow) == 0 && len(p.models) == 0 }

// narrowsModels reports whether the policy judges single models, as opposed to
// whole providers only. It decides whether a check that needs the runtime's
// default model has anything to do: a provider-only policy never does.
func (p providerPolicy) narrowsModels() bool { return len(p.models) > 0 || len(p.deny) > 0 }

// newProviderPolicy builds a policy from operator-supplied entries, trimming
// blanks and repeats. An entry that cannot be one is refused, naming it:
//
//	entry   = [ "!" ] provider [ "/" pattern ]
//	provider: non-empty; none of  / * ? [ ] \ !  and no whitespace
//	pattern : non-empty; "*" is the only wildcard; none of  ? [ ] \  and no whitespace
//
// "provider" allows every model on it (today's meaning, unchanged).
// "provider/<model>" and "provider/<glob>" allow only the matching models;
// "provider/*" is the same as "provider". "!provider/<glob>" denies the matching
// models whatever else allows them. A deny without a model part is refused:
// keeping a whole provider off is what leaving it out of the allowed list does.
//
// The model part is everything after the first "/", the split providerOfModel
// uses, so a model id that contains a "/" of its own is written as it is.
func newProviderPolicy(entries []string) (providerPolicy, error) {
	var p providerPolicy
	seen := map[string]bool{}
	enable := func(provider string) {
		for _, e := range p.enabled {
			if e == provider {
				return
			}
		}
		p.enabled = append(p.enabled, provider)
	}
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		refuse := func(why string) error {
			return fmt.Errorf("opencode: provider policy entry %q: %s (an entry is a provider id, provider/<model>, "+
				"provider/<glob>, or !provider/<glob>)", entry, why)
		}
		body, deny := strings.CutPrefix(entry, "!")
		provider, pattern, hasModel := strings.Cut(body, "/")
		if provider == "" {
			return providerPolicy{}, refuse("it names no provider")
		}
		if strings.ContainsAny(provider, "*?[]\\!") || strings.ContainsAny(provider, " \t\r\n") {
			return providerPolicy{}, refuse("a provider id has no whitespace or any of * ? [ ] \\ !")
		}
		if hasModel {
			if pattern == "" {
				return providerPolicy{}, refuse("nothing follows the \"/\"")
			}
			if strings.ContainsAny(pattern, "?[]\\") || strings.ContainsAny(pattern, " \t\r\n") {
				return providerPolicy{}, refuse("a model pattern has no whitespace or any of ? [ ] \\ (\"*\" is the only wildcard)")
			}
		}
		switch {
		case deny && !hasModel:
			return providerPolicy{}, refuse("a deny names models; write !" + provider + "/<glob>, " +
				"and to keep the whole provider off leave it out of the allowed list")
		case deny:
			p.deny = append(p.deny, modelRule{provider: provider, pattern: pattern, raw: entry})
		case !hasModel || pattern == "*":
			p.allow = append(p.allow, provider)
			p.allowed = append(p.allowed, entry)
			enable(provider)
		default:
			p.models = append(p.models, modelRule{provider: provider, pattern: pattern, raw: entry})
			p.allowed = append(p.allowed, entry)
			enable(provider)
		}
	}
	return p, nil
}

// DescribeProviderPolicy parses the operator's policy entries and returns the
// policy in words, or the error naming the entry that cannot be one. It is the
// check the composition root and `muster doctor` run, so a malformed entry stops
// the start and shows as a failed row, rather than only leaving the runtime
// unavailable.
func DescribeProviderPolicy(entries []string) (string, error) {
	p, err := newProviderPolicy(entries)
	if err != nil {
		return "", err
	}
	return p.describe(), nil
}

// NarrowsModels reports whether the entries name single models (an allow of
// provider/<model> or provider/<glob>, or a deny), as opposed to whole providers
// only. A malformed list reports false: the error is DescribeProviderPolicy's to
// give.
func NarrowsModels(entries []string) bool {
	p, err := newProviderPolicy(entries)
	return err == nil && p.narrowsModels()
}

// matchGlob reports whether s matches pattern, where "*" matches any run of
// bytes, including none and including "/", and every other byte matches itself
// exactly (the runtime's ids are case-sensitive and so is this). The standard
// library's matchers are not used: theirs stop a "*" at "/", which a model id
// may contain, and give "?", "[" and "\" meanings the grammar refuses.
func matchGlob(pattern, s string) bool {
	px, sx := 0, 0
	star, mark := -1, 0
	for sx < len(s) {
		switch {
		case px < len(pattern) && pattern[px] == '*':
			star, mark = px, sx
			px++
		case px < len(pattern) && pattern[px] == s[sx]:
			px++
			sx++
		case star >= 0:
			mark++
			px, sx = star+1, mark
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// verdict is the answer to "may a session use this model".
type verdict int

const (
	// permitted: the policy allows it.
	permitted verdict = iota
	// providerNotAllowed: the provider is not one the policy allows.
	providerNotAllowed
	// modelNotAllowed: the provider is allowed only for some models, and none
	// matches this one.
	modelNotAllowed
	// modelDenied: a deny matches it. The rule that did is returned with it.
	modelDenied
)

// judge decides whether a session may use model on provider. A deny beats every
// allow, whatever order the entries were written in. The rule returned is the
// matching deny, as written, for a refusal to quote.
func (p providerPolicy) judge(provider, model string) (verdict, string) {
	for _, d := range p.deny {
		if d.provider == provider && d.matches(model) {
			return modelDenied, d.raw
		}
	}
	if p.open() {
		if provider == builtinProvider {
			return providerNotAllowed, ""
		}
		return permitted, ""
	}
	for _, a := range p.allow {
		if a == provider {
			return permitted, ""
		}
	}
	named := false
	for _, r := range p.models {
		if r.provider != provider {
			continue
		}
		named = true
		if r.matches(model) {
			return permitted, ""
		}
	}
	if named {
		return modelNotAllowed, ""
	}
	return providerNotAllowed, ""
}

// permits reports whether a session may use provider at all: the provider is
// allowed whole, or allowed for some models. Denies do not enter into it — a
// provider with every model denied is still one the runtime must have on.
func (p providerPolicy) permits(id string) bool {
	if p.open() {
		return id != builtinProvider
	}
	for _, a := range p.allow {
		if a == id {
			return true
		}
	}
	for _, r := range p.models {
		if r.provider == id {
			return true
		}
	}
	return false
}

// permitsModel reports whether a session may use the "provider/model" id.
func (p providerPolicy) permitsModel(id string) bool {
	provider, model, _ := strings.Cut(id, "/")
	v, _ := p.judge(provider, model)
	return v == permitted
}

// configKeys are the opencode.json entries that express the policy.
// enabled_providers is a whitelist and wins over disabled_providers in the
// runtime, so only one of the two is ever written. A provider allowed for some
// models only is enabled like any other: the runtime cannot serve one model of
// a provider that is off. Model rules themselves are not in the file.
func (p providerPolicy) configKeys() map[string]any {
	if !p.open() {
		return map[string]any{"enabled_providers": p.enabled}
	}
	return map[string]any{"disabled_providers": []string{builtinProvider}}
}

// describe is the policy in words a refusal can quote.
func (p providerPolicy) describe() string {
	var out string
	if p.open() {
		out = fmt.Sprintf("any provider except the runtime's built-in hosted provider %q (disabled by default)", builtinProvider)
	} else {
		out = "only " + strings.Join(quoteAll(p.allowed), ", ")
	}
	if len(p.deny) > 0 {
		raws := make([]string, len(p.deny))
		for i, d := range p.deny {
			raws[i] = d.raw
		}
		out += "; denied: " + strings.Join(quoteAll(raws), ", ")
	}
	return out
}

// providerOfModel is the provider half of a "provider/model" id.
func providerOfModel(model string) string {
	id, _, _ := strings.Cut(model, "/")
	return id
}
