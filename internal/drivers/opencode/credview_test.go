package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #497: the service says which provider credentials a runtime on this
// machine can be given — kind, source, state, never a value. These tests build the
// driver without starting anything, because the view must be answerable from what
// the service already holds: no engine, no request to a provider.

var _ driver.CredentialReporter = (*Driver)(nil)

// keyFile writes value to a file of its own and returns its path.
func keyFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider-key")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// credDriver is a driver holding only what the view reads.
func credDriver(entries []sessionenv.Entry, opts ...func(*Driver)) *Driver {
	d := &Driver{runtime: DefaultRuntime, sessionEnv: entries, providers: providerPolicy{}}
	for _, o := range opts {
		o(d)
	}
	return d
}

func withGrantPhase(phase grant.Phase, reason string) func(*Driver) {
	return func(d *Driver) {
		g := newFakeGrants("X")
		g.st = grant.State{Provider: grant.ProviderOpenAI, Phase: phase, Reason: reason}
		d.grants = g
	}
}

func withPolicy(entries ...string) func(*Driver) {
	return func(d *Driver) {
		p, err := newProviderPolicy(entries)
		if err != nil {
			panic(err)
		}
		d.providers = p
	}
}

func row(provider string, kind fleet.CredentialKind, source fleet.CredentialSource, state fleet.CredentialState) fleet.RuntimeCredential {
	return fleet.RuntimeCredential{Provider: provider, Kind: kind, Source: source, State: state}
}

func TestCredentials_AnOperatorKeyIsListedWithoutTheEngineOnAnyPath(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	// The proof case of #497: an operator-env openai key, no grant, and the
	// runtime's binary off every PATH (the driver holds its absolute path; a
	// client that ran the engine's own program from its PATH would find nothing).
	t.Setenv("PATH", "")
	d, _ := newIsolatedDriver(t, WithSessionEnv([]sessionenv.Entry{{Name: openaiKeyEnv, FromFile: keyFile(t, "sk-REAL-VALUE")}}))
	got := d.Credentials()
	want := []fleet.RuntimeCredential{row("openai", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialUsable)}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Credentials = %+v, want %+v", got, want)
	}
}

func TestCredentials_EachSourceAndState(t *testing.T) {
	good := keyFile(t, "sk-GOOD")
	empty := keyFile(t, "")
	multi := keyFile(t, "line-one\nline-two")
	missing := filepath.Join(t.TempDir(), "never-written")
	unreadable := t.TempDir() // a directory where a file should be

	cases := []struct {
		name      string
		d         *Driver
		want      []fleet.RuntimeCredential
		reasonHas string
		reasonNot string
	}{
		{name: "a readable key file is usable",
			d:    credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: good}}),
			want: []fleet.RuntimeCredential{row("openai", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialUsable)}},
		{name: "a missing key file is degraded and an optional entry says sessions start without it",
			d:         credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: missing}}),
			want:      []fleet.RuntimeCredential{row("openai", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialDegraded)},
			reasonHas: "OPENAI_API_KEY does not exist", reasonNot: "never-written"},
		{name: "an empty key file is degraded",
			d:         credDriver([]sessionenv.Entry{{Name: "DEEPSEEK_API_KEY", FromFile: empty}}),
			want:      []fleet.RuntimeCredential{row("deepseek", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialDegraded)},
			reasonHas: "file is empty"},
		{name: "a key file with two lines is degraded",
			d:         credDriver([]sessionenv.Entry{{Name: "DEEPSEEK_API_KEY", FromFile: multi}}),
			want:      []fleet.RuntimeCredential{row("deepseek", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialDegraded)},
			reasonHas: "newline", reasonNot: "line-one"},
		{name: "a key path that cannot be read is degraded and names no path",
			d:         credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: unreadable}}),
			want:      []fleet.RuntimeCredential{row("openai", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialDegraded)},
			reasonHas: "cannot be used", reasonNot: unreadable},
		{name: "a required entry says a create is refused until it is repaired",
			d:         credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: missing, Required: true}}),
			want:      []fleet.RuntimeCredential{row("openai", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialDegraded)},
			reasonHas: "a create is refused"},
		{name: "a signed-in grant is a usable subscription",
			d:    credDriver(nil, withGrantPhase(grant.PhaseOK, "")),
			want: []fleet.RuntimeCredential{row("openai", fleet.CredentialSubscription, fleet.SourceGrant, fleet.CredentialUsable)}},
		{name: "a degraded grant",
			d:         credDriver(nil, withGrantPhase(grant.PhaseDegraded, "the provider is unreachable")),
			want:      []fleet.RuntimeCredential{row("openai", fleet.CredentialSubscription, fleet.SourceGrant, fleet.CredentialDegraded)},
			reasonHas: "unreachable"},
		{name: "a revoked grant",
			d:         credDriver(nil, withGrantPhase(grant.PhaseRevoked, "sign in again")),
			want:      []fleet.RuntimeCredential{row("openai", fleet.CredentialSubscription, fleet.SourceGrant, fleet.CredentialRevoked)},
			reasonHas: "sign in again"},
		{name: "a grant with nothing stored is signed out",
			d:         credDriver(nil, withGrantPhase(grant.PhaseSignedOut, "none stored")),
			want:      []fleet.RuntimeCredential{row("openai", fleet.CredentialSubscription, fleet.SourceGrant, fleet.CredentialSignedOut)},
			reasonHas: "none stored"},
		{name: "a key and a sign-in for one provider are two rows, the key first",
			d: credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: good}}, withGrantPhase(grant.PhaseOK, "")),
			want: []fleet.RuntimeCredential{
				row("openai", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialUsable),
				row("openai", fleet.CredentialSubscription, fleet.SourceGrant, fleet.CredentialUsable)}},
		{name: "providers are in provider order",
			d: credDriver([]sessionenv.Entry{
				{Name: "OPENROUTER_API_KEY", FromFile: good}, {Name: "ANTHROPIC_API_KEY", FromFile: good}}),
			want: []fleet.RuntimeCredential{
				row("anthropic", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialUsable),
				row("openrouter", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialUsable)}},
		{name: "two names for one provider say the better of the two",
			d: credDriver([]sessionenv.Entry{
				{Name: "GEMINI_API_KEY", FromFile: missing}, {Name: "GOOGLE_API_KEY", FromFile: good}}),
			want: []fleet.RuntimeCredential{row("google", fleet.CredentialAPIKey, fleet.SourceOperatorEnv, fleet.CredentialUsable)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.d.Credentials()
			if len(got) != len(tc.want) {
				t.Fatalf("Credentials = %+v, want %d rows", got, len(tc.want))
			}
			for i, w := range tc.want {
				g := got[i]
				reason := g.Reason
				g.Reason = ""
				if g != w {
					t.Fatalf("row %d = %+v, want %+v", i, g, w)
				}
				if w.State == fleet.CredentialUsable && reason != "" {
					t.Fatalf("a usable credential carries the reason %q", reason)
				}
				if w.State != fleet.CredentialUsable && reason == "" {
					t.Fatalf("a %s credential carries no reason", w.State)
				}
			}
			if tc.reasonHas == "" && tc.reasonNot == "" {
				return
			}
			reason := got[0].Reason
			if tc.reasonHas != "" && !strings.Contains(reason, tc.reasonHas) {
				t.Fatalf("reason %q does not say %q", reason, tc.reasonHas)
			}
			if tc.reasonNot != "" && strings.Contains(reason, tc.reasonNot) {
				t.Fatalf("reason %q names %q", reason, tc.reasonNot)
			}
		})
	}
}

func TestCredentials_AUsableCredentialCarriesNoReason(t *testing.T) {
	d := credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: keyFile(t, "sk-X")}}, withGrantPhase(grant.PhaseOK, ""))
	for _, c := range d.Credentials() {
		if c.State != fleet.CredentialUsable || c.Reason != "" {
			t.Fatalf("%+v: a usable credential has no reason", c)
		}
	}
}

func TestCredentials_TheProviderPolicyDecidesWhatIsListed(t *testing.T) {
	good := keyFile(t, "sk-GOOD")
	entries := []sessionenv.Entry{
		{Name: "OPENAI_API_KEY", FromFile: good},
		{Name: "DEEPSEEK_API_KEY", FromFile: good},
	}
	// A sign-in makes a provider usable, it does not allow it: a policy naming
	// deepseek alone hides both openai credentials.
	d := credDriver(entries, withPolicy("deepseek"), withGrantPhase(grant.PhaseOK, ""))
	got := d.Credentials()
	if len(got) != 1 || got[0].Provider != "deepseek" {
		t.Fatalf("Credentials = %+v, want deepseek alone", got)
	}
	// The default policy permits every provider but the runtime's hosted one.
	d = credDriver(entries, withGrantPhase(grant.PhaseOK, ""))
	if got := d.Credentials(); len(got) != 3 {
		t.Fatalf("Credentials = %+v, want deepseek, the openai key and the sign-in", got)
	}
	// A policy that names single models still permits the provider.
	d = credDriver(entries, withPolicy("openai/gpt-5.5"))
	if got := d.Credentials(); len(got) != 1 || got[0].Provider != "openai" {
		t.Fatalf("Credentials = %+v, want the openai key alone", got)
	}
}

func TestCredentials_OnlyAnEntryThatReachesThisRuntimeCounts(t *testing.T) {
	good := keyFile(t, "sk-GOOD")
	cases := []struct {
		name  string
		entry sessionenv.Entry
		want  int
	}{
		{"no scope reaches every runtime", sessionenv.Entry{Name: "OPENAI_API_KEY", FromFile: good}, 1},
		{"scoped to this runtime", sessionenv.Entry{Name: "OPENAI_API_KEY", FromFile: good,
			AppliesTo: sessionenv.Scope{Runtimes: []string{string(DefaultRuntime)}}}, 1},
		{"scoped to another runtime", sessionenv.Entry{Name: "OPENAI_API_KEY", FromFile: good,
			AppliesTo: sessionenv.Scope{Runtimes: []string{"claude-code"}}}, 0},
		{"scoped to an agent: a session in that scope gets it, so it is listed", sessionenv.Entry{Name: "OPENAI_API_KEY", FromFile: good,
			AppliesTo: sessionenv.Scope{Agents: []string{"worker"}}}, 1},
		{"a name the table does not know is not guessed at", sessionenv.Entry{Name: "SOME_VENDOR_TOKEN", FromFile: good}, 0},
		{"a name two providers share is not guessed at", sessionenv.Entry{Name: "PERPLEXITY_API_KEY", FromFile: good}, 0},
		{"a variable that is the session's own id is not a credential", sessionenv.Entry{Name: "OPENAI_API_KEY", FromSession: sessionenv.SessionID}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credDriver([]sessionenv.Entry{tc.entry}).Credentials(); len(got) != tc.want {
				t.Fatalf("Credentials = %+v, want %d rows", got, tc.want)
			}
		})
	}
}

func TestCredentials_SharedModeAndAnUnconfiguredDriverSayNothing(t *testing.T) {
	good := keyFile(t, "sk-GOOD")
	// Shared mode: a server this driver did not start. Neither the operator's
	// environment nor the sign-in reaches it, and its own store cannot be read
	// without asking it.
	d := credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: good}}, withGrantPhase(grant.PhaseOK, ""),
		func(d *Driver) { d.shared = &server{} })
	if got := d.Credentials(); got != nil {
		t.Fatalf("shared mode reported %+v", got)
	}
	// Nothing configured: absent, not an empty list, so a client never reads
	// "none held" out of "nobody said".
	if got := credDriver(nil).Credentials(); len(got) != 0 {
		t.Fatalf("an unconfigured driver reported %+v", got)
	}
}

func TestCredentials_NeitherAValueNorAPathLeavesTheService(t *testing.T) {
	dir := t.TempDir()
	secret := "sk-VERY-SECRET-VALUE-0123456789"
	path := filepath.Join(dir, "very-private-key-file")
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "second-key-file-that-is-not-there")
	g := newFakeGrants("ACCESS-TOKEN-VALUE")
	g.st = grant.State{Provider: grant.ProviderOpenAI, Phase: grant.PhaseOK}
	d := credDriver([]sessionenv.Entry{
		{Name: "OPENAI_API_KEY", FromFile: path},
		{Name: "DEEPSEEK_API_KEY", FromFile: broken},
	}, func(d *Driver) { d.grants = g })
	raw, err := json.Marshal(d.Credentials())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, "ACCESS-TOKEN-VALUE", path, broken, dir, "very-private-key-file", "second-key-file"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the credentials view carries %q: %s", leak, raw)
		}
	}
	if !strings.Contains(string(raw), `"operator-env"`) || !strings.Contains(string(raw), `"grant"`) {
		t.Fatalf("both sources are expected in this case: %s", raw)
	}
	// The key file is read fresh: a repaired file is usable at the next listing.
	if err := os.WriteFile(broken, []byte("sk-NOW-THERE"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Credentials() {
		if c.Provider == "deepseek" && c.State != fleet.CredentialUsable {
			t.Fatalf("a repaired key file still reads %+v", c)
		}
	}
}

func TestCredentials_NoSessionOfThisDriverReadsTheEnginesOwnStore(t *testing.T) {
	// engine-store is a source on the wire this driver never reports: every
	// session runs with a data directory of its own, so the store the service
	// user signed in to is under none of them.
	d := credDriver([]sessionenv.Entry{{Name: "OPENAI_API_KEY", FromFile: keyFile(t, "k")}}, withGrantPhase(grant.PhaseOK, ""))
	for _, c := range d.Credentials() {
		if c.Source == fleet.SourceEngineStore {
			t.Fatalf("reported %+v", c)
		}
	}
}

func TestCredentials_TheSignInRowAndTheGrantsRowAgree(t *testing.T) {
	for phase, state := range map[grant.Phase]fleet.GrantState{
		grant.PhaseOK: fleet.GrantOK, grant.PhaseDegraded: fleet.GrantDegraded,
		grant.PhaseRevoked: fleet.GrantRevoked, grant.PhaseSignedOut: fleet.GrantSignedOut,
	} {
		d := credDriver(nil, withGrantPhase(phase, "why"))
		grants, creds := d.Grants(), d.Credentials()
		if len(grants) != 1 || grants[0].State != state || len(creds) != 1 {
			t.Fatalf("phase %s: grants %+v creds %+v", phase, grants, creds)
		}
		agree := map[fleet.GrantState]fleet.CredentialState{
			fleet.GrantOK: fleet.CredentialUsable, fleet.GrantDegraded: fleet.CredentialDegraded,
			fleet.GrantRevoked: fleet.CredentialRevoked, fleet.GrantSignedOut: fleet.CredentialSignedOut,
		}
		if creds[0].State != agree[state] || creds[0].Reason != grants[0].Reason {
			t.Fatalf("phase %s: credential %+v disagrees with grant %+v", phase, creds[0], grants[0])
		}
	}
	// A state the credential vocabulary does not know reads as not usable.
	if got := credentialStateOfGrant("a-state-from-a-later-build"); got == fleet.CredentialUsable {
		t.Fatalf("an unknown grant state reads as %s", got)
	}
}
