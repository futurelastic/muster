package opencode

import (
	"sort"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// Linking and unlinking a provider's credentials over HTTP (muster #498).
//
// This service is the one party that starts the runtime, so it is the one that
// should write the runtime's credentials; before this a client had to run the
// runtime's own program or this service's command line on the machine. Two kinds:
//
//   - subscription: the device-code sign-in `muster grant login` runs, as a start
//     that returns the address and code to show a person and a poll that says
//     whether they approved (grant.Logins); and sign-out.
//   - API key: the value is written owner-only into this service's own state
//     (sessionenv.Linked) and reaches a session as a sessionEnv entry the service
//     itself maintains. It is never read back: the credentials view says it
//     exists and works, and nothing says what it is.
//
// # Why the key is a state-managed entry and not a line in the config file
//
// A sessionEnv entry the operator declares is read at start and needs a restart to
// change. A client cannot wait for that, and rewriting the operator's own config
// file would make the service an author of it. So a linked key lives beside the
// config, and effectiveSessionEnv is the one place the two are put together. The
// operator's declaration always wins over a linked key: linking a variable the
// operator declared is refused, and a declaration added later shadows the linked
// key without deleting it.
//
// # What linking does not do
//
// Linking a credential makes a provider USABLE; it never makes it ALLOWED. The
// provider policy (provider.go) is applied first, and a provider it does not permit
// is refused at link time rather than accepted into a place no session reads from.

// LoginManager is what the driver needs of the subscription sign-in: begin one,
// look one up, and forget the stored one. *grant.Logins is one.
type LoginManager interface {
	Start() (view grant.LoginView, started bool, err error)
	Get(id string) (grant.LoginView, bool)
	SignOut() (removed bool, err error)
}

// keyEnvForProvider is the one variable a link writes for a provider: the inverse
// of keyEnvProviders, restricted to a single canonical name where the runtime
// accepts several (google). A provider not here cannot be linked — the table is
// short on purpose, for the reason keyEnvProviders is (credview.go): naming a
// variable the runtime may not read for a provider is the guess this replaces.
var keyEnvForProvider = map[string]string{
	grant.ProviderOpenAI: "OPENAI_API_KEY",
	"anthropic":          "ANTHROPIC_API_KEY",
	"deepseek":           "DEEPSEEK_API_KEY",
	"openrouter":         "OPENROUTER_API_KEY",
	"groq":               "GROQ_API_KEY",
	"mistral":            "MISTRAL_API_KEY",
	"xai":                "XAI_API_KEY",
	"cerebras":           "CEREBRAS_API_KEY",
	"cohere":             "COHERE_API_KEY",
	"togetherai":         "TOGETHER_API_KEY",
	"fireworks-ai":       "FIREWORKS_API_KEY",
	"deepinfra":          "DEEPINFRA_API_KEY",
	"google":             "GOOGLE_GENERATIVE_AI_API_KEY",
}

// maxKeyBytes bounds a linked key. Provider keys are tens to a few hundred bytes.
const maxKeyBytes = 4096

// effectiveSessionEnv is the entries a create provisions from: the operator's
// declared ones, then the linked ones for this runtime whose variable the operator
// did not declare. linked names the ones that came from the linked store, by their
// folded name. Read afresh each call, so a link or an unlink shows at the next
// create and the next listing with nothing to reload.
func (d *Driver) effectiveSessionEnv() (entries []sessionenv.Entry, linked map[string]bool) {
	if d.linked == nil {
		return d.sessionEnv, nil
	}
	extra := foldEntryNames(d.linked.Entries(d.runtime))
	if len(extra) == 0 {
		return d.sessionEnv, nil
	}
	entries = append(make([]sessionenv.Entry, 0, len(d.sessionEnv)+len(extra)), d.sessionEnv...)
	linked = map[string]bool{}
	for _, e := range extra {
		if d.declares(e.Name) || ownedName(e.Name) {
			continue
		}
		entries = append(entries, e)
		linked[e.Name] = true
	}
	return entries, linked
}

// declares reports whether the operator declared name for this runtime. name is
// already folded; d.sessionEnv is folded at New.
func (d *Driver) declares(name string) bool {
	for _, e := range d.sessionEnv {
		if e.Name == name && e.AppliesTo.ReachesRuntime(d.runtime) {
			return true
		}
	}
	return false
}

var _ driver.CredentialManager = (*Driver)(nil)

func (d *Driver) credErr(kind fleet.ErrorKind, msg string) error {
	return &fleet.Error{Kind: kind, Message: msg, Machine: d.machine}
}

// supportedKeyProviders names the providers a key can be linked for, in order.
func supportedKeyProviders() string {
	ids := make([]string, 0, len(keyEnvForProvider))
	for id := range keyEnvForProvider {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// keyTarget resolves the variable a provider's key is linked as, refusing a
// runtime that cannot hold one and a provider that has no variable here.
func (d *Driver) keyTarget(provider string) (string, error) {
	if d.shared != nil {
		return "", d.credErr(fleet.ErrorUnsupported,
			"this runtime is a server the service did not start, so the service cannot give it a credential")
	}
	if d.linked == nil {
		return "", d.credErr(fleet.ErrorUnsupported,
			"this machine has no state directory, so there is nowhere to keep a linked key")
	}
	name, ok := keyEnvForProvider[provider]
	if !ok {
		return "", d.credErr(fleet.ErrorInvalid,
			"no API key can be linked for provider "+quoteID(provider)+" on this runtime (supported: "+supportedKeyProviders()+")")
	}
	return envKey(name), nil
}

// quoteID renders a caller-supplied id in a message, cut short and with anything
// but a plain id character dropped, so a message never carries a stray value.
func quoteID(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		}
	}
	return `"` + string(b) + `"`
}

// validKey reports whether b can be a provider API key: one printable token with no
// space, newline or control character (a newline would split the value the staging
// format carries), and a bounded length. One trailing newline is tolerated, because
// a key pasted from a file ends with one, and is not stored.
func validKey(b []byte) ([]byte, bool) {
	b = []byte(strings.TrimRight(string(b), "\r\n"))
	if len(b) == 0 || len(b) > maxKeyBytes {
		return nil, false
	}
	for _, c := range b {
		if c <= ' ' || c >= 0x7f {
			return nil, false
		}
	}
	return b, true
}

// LinkKey implements driver.CredentialManager.
func (d *Driver) LinkKey(provider string, value []byte) (fleet.RuntimeCredential, error) {
	name, err := d.keyTarget(provider)
	if err != nil {
		return fleet.RuntimeCredential{}, err
	}
	if !d.providers.permits(provider) {
		return fleet.RuntimeCredential{}, d.credErr(fleet.ErrorInvalid,
			"this machine's provider policy does not permit "+quoteID(provider)+
				": a linked key makes a provider usable, it never allows it (FLEET_OPENCODE_PROVIDERS)")
	}
	if d.declares(name) {
		return fleet.RuntimeCredential{}, d.credErr(fleet.ErrorConflict,
			name+" is declared in this machine's sessionEnv, which decides it: change or remove that entry there, not here")
	}
	key, ok := validKey(value)
	if !ok {
		return fleet.RuntimeCredential{}, d.credErr(fleet.ErrorInvalid,
			"the value is not a usable API key: it must be 1 to 4096 printable characters with no space or line break")
	}
	if err := d.linked.Put(d.runtime, name, key); err != nil {
		// err names the cause (a permission, a full disk) and never the value.
		return fleet.RuntimeCredential{}, d.credErr(fleet.ErrorUnreachable, "the key could not be stored: "+err.Error())
	}
	for _, c := range d.Credentials() {
		if c.Provider == provider && c.Kind == fleet.CredentialAPIKey {
			return c, nil
		}
	}
	return fleet.RuntimeCredential{Provider: provider, Kind: fleet.CredentialAPIKey, Source: fleet.SourceLinked, State: fleet.CredentialUsable}, nil
}

// UnlinkKey implements driver.CredentialManager.
func (d *Driver) UnlinkKey(provider string) (fleet.CredentialRemoval, error) {
	name, err := d.keyTarget(provider)
	if err != nil {
		return fleet.CredentialRemoval{}, err
	}
	// Whether the operator also declares the variable is not asked: this removes a
	// LINKED key, which a declaration only shadows, and a declared one is not in the
	// store to remove.
	gone, err := d.linked.Remove(d.runtime, name)
	if err != nil {
		return fleet.CredentialRemoval{}, d.credErr(fleet.ErrorUnreachable, "the key could not be removed: "+err.Error())
	}
	return fleet.CredentialRemoval{Provider: provider, Kind: fleet.CredentialAPIKey, Removed: gone}, nil
}

// subscriptionTarget refuses a provider this service holds no sign-in for, and a
// machine with nowhere to keep one.
func (d *Driver) subscriptionTarget(provider string) error {
	if d.shared != nil {
		return d.credErr(fleet.ErrorUnsupported,
			"this runtime is a server the service did not start, so the service cannot give it a credential")
	}
	if provider != grant.ProviderOpenAI {
		return d.credErr(fleet.ErrorInvalid,
			"no subscription sign-in exists for provider "+quoteID(provider)+" on this runtime (supported: "+grant.ProviderOpenAI+")")
	}
	if d.logins == nil {
		return d.credErr(fleet.ErrorUnsupported,
			"this machine has no state directory, so there is nowhere to keep a subscription sign-in")
	}
	return nil
}

// StartLogin implements driver.CredentialManager.
func (d *Driver) StartLogin(provider string) (fleet.CredentialLogin, error) {
	if err := d.subscriptionTarget(provider); err != nil {
		return fleet.CredentialLogin{}, err
	}
	if !d.providers.permits(provider) {
		return fleet.CredentialLogin{}, d.credErr(fleet.ErrorInvalid,
			"this machine's provider policy does not permit "+quoteID(provider)+
				": a sign-in makes a provider usable, it never allows it (FLEET_OPENCODE_PROVIDERS)")
	}
	v, _, err := d.logins.Start()
	if err != nil {
		// The package's own text: the provider's status or a network cause, no token.
		return fleet.CredentialLogin{}, d.credErr(fleet.ErrorUnreachable, "the sign-in could not be started: "+err.Error())
	}
	return loginOf(v), nil
}

// LoginStatus implements driver.CredentialManager.
func (d *Driver) LoginStatus(provider, id string) (fleet.CredentialLogin, error) {
	if err := d.subscriptionTarget(provider); err != nil {
		return fleet.CredentialLogin{}, err
	}
	v, ok := d.logins.Get(id)
	if !ok {
		return fleet.CredentialLogin{}, d.credErr(fleet.ErrorNotFound,
			"no sign-in attempt with that id is held here (only the latest attempt is kept, and a restart forgets it)")
	}
	return loginOf(v), nil
}

// SignOut implements driver.CredentialManager.
func (d *Driver) SignOut(provider string) (fleet.CredentialRemoval, error) {
	if err := d.subscriptionTarget(provider); err != nil {
		return fleet.CredentialRemoval{}, err
	}
	gone, err := d.logins.SignOut()
	if err != nil {
		return fleet.CredentialRemoval{}, d.credErr(fleet.ErrorUnreachable, "the sign-in could not be removed: "+err.Error())
	}
	return fleet.CredentialRemoval{Provider: provider, Kind: fleet.CredentialSubscription, Removed: gone}, nil
}

// loginOf is a sign-in attempt in the wire's shape. It never holds a token: a
// grant.LoginView has no field for one.
func loginOf(v grant.LoginView) fleet.CredentialLogin {
	out := fleet.CredentialLogin{ID: v.ID, Reason: v.Reason}
	switch v.State {
	case grant.LoginApproved:
		out.State = fleet.LoginApproved
	case grant.LoginExpired:
		out.State = fleet.LoginExpired
	case grant.LoginFailed:
		out.State = fleet.LoginFailed
	default:
		out.State = fleet.LoginPending
		out.URL, out.Code = v.URL, v.Code
		if !v.ExpiresAt.IsZero() {
			t := v.ExpiresAt.UTC().Truncate(time.Second)
			out.ExpiresAt = &t
		}
	}
	return out
}
