package opencode

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #498: link and unlink a runtime's provider credentials. The driver is
// built without starting anything for the rules (kinds, policy, conflict, value
// hygiene) and as a real isolated driver for the proof that a linked key reaches a
// session and an unlinked one does not.

var _ driver.CredentialManager = (*Driver)(nil)

const secretKey = "sk-SECRET-linked-value-0123456789"

// linkDriver is a driver that can hold linked keys.
func linkDriver(t *testing.T, entries []sessionenv.Entry, opts ...func(*Driver)) *Driver {
	t.Helper()
	d := credDriver(entries, opts...)
	d.linked = sessionenv.NewLinked(t.TempDir())
	d.machine = "test-machine"
	return d
}

func kindOf(err error) fleet.ErrorKind {
	var fe *fleet.Error
	if errors.As(err, &fe) {
		return fe.Kind
	}
	return ""
}

func TestLinkKey_ARowAppearsInTheViewAsLinkedAndUsable(t *testing.T) {
	d := linkDriver(t, nil)
	got, err := d.LinkKey("openai", []byte(secretKey))
	if err != nil {
		t.Fatal(err)
	}
	want := row("openai", fleet.CredentialAPIKey, fleet.SourceLinked, fleet.CredentialUsable)
	if got != want {
		t.Fatalf("LinkKey row = %+v, want %+v", got, want)
	}
	if view := d.Credentials(); len(view) != 1 || view[0] != want {
		t.Fatalf("the view = %+v, want %+v", view, want)
	}
	raw, _ := json.Marshal(d.Credentials())
	if strings.Contains(string(raw), secretKey) {
		t.Fatalf("the view carries the key: %s", raw)
	}
	// Unlink: gone from the view, and the call says it removed something, once.
	rem, err := d.UnlinkKey("openai")
	if err != nil || !rem.Removed || rem.Provider != "openai" || rem.Kind != fleet.CredentialAPIKey {
		t.Fatalf("UnlinkKey = %+v, %v", rem, err)
	}
	if view := d.Credentials(); len(view) != 0 {
		t.Fatalf("the view after unlink = %+v", view)
	}
	if rem, err := d.UnlinkKey("openai"); err != nil || rem.Removed {
		t.Fatalf("a second unlink must succeed and say nothing was removed: %+v, %v", rem, err)
	}
}

func TestLinkKey_RulesAndRefusals(t *testing.T) {
	declared := []sessionenv.Entry{{Name: "DEEPSEEK_API_KEY", FromFile: keyFile(t, "operator-key")}}
	cases := []struct {
		name     string
		d        *Driver
		provider string
		value    string
		kind     fleet.ErrorKind
		msgHas   string
		msgNot   string
	}{
		{name: "a provider with no variable in the table", d: linkDriver(t, nil), provider: "no-such-provider",
			value: secretKey, kind: fleet.ErrorInvalid, msgHas: "supported:", msgNot: secretKey},
		{name: "a provider the policy does not permit", d: linkDriver(t, nil, withPolicy("deepseek")), provider: "openai",
			value: secretKey, kind: fleet.ErrorInvalid, msgHas: "never allows it", msgNot: secretKey},
		{name: "a variable the operator declared", d: linkDriver(t, declared), provider: "deepseek",
			value: secretKey, kind: fleet.ErrorConflict, msgHas: "DEEPSEEK_API_KEY", msgNot: secretKey},
		{name: "an empty value", d: linkDriver(t, nil), provider: "openai", value: "", kind: fleet.ErrorInvalid},
		{name: "a value with a space", d: linkDriver(t, nil), provider: "openai", value: "sk-has space", kind: fleet.ErrorInvalid, msgNot: "sk-has"},
		{name: "a value with an inner newline", d: linkDriver(t, nil), provider: "openai", value: "sk-line\nsk-two", kind: fleet.ErrorInvalid, msgNot: "sk-line"},
		{name: "a value that is too long", d: linkDriver(t, nil), provider: "openai", value: strings.Repeat("k", maxKeyBytes+1), kind: fleet.ErrorInvalid},
		{name: "no linked store (no state directory)", d: credDriver(nil), provider: "openai", value: secretKey, kind: fleet.ErrorUnsupported, msgNot: secretKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.d.LinkKey(c.provider, []byte(c.value))
			if err == nil {
				t.Fatal("LinkKey succeeded")
			}
			if kindOf(err) != c.kind {
				t.Fatalf("kind = %q, want %q (%v)", kindOf(err), c.kind, err)
			}
			if c.msgHas != "" && !strings.Contains(err.Error(), c.msgHas) {
				t.Errorf("message %q lacks %q", err.Error(), c.msgHas)
			}
			if c.msgNot != "" && strings.Contains(err.Error(), c.msgNot) {
				t.Errorf("message %q carries %q", err.Error(), c.msgNot)
			}
			if c.d.linked != nil && c.d.linked.Has(c.d.runtime, "OPENAI_API_KEY") {
				t.Error("a refused link left a file behind")
			}
		})
	}
}

func TestLinkKey_ATrailingNewlineIsTolerated(t *testing.T) {
	d := linkDriver(t, nil)
	if _, err := d.LinkKey("groq", []byte("gsk-abc\n")); err != nil {
		t.Fatal(err)
	}
	env, err := sessionenv.Provision(func() []sessionenv.Entry { e, _ := d.effectiveSessionEnv(); return e }(), fleet.SessionSpec{}, "m", d.runtime, "", nil)
	if err != nil || env["GROQ_API_KEY"] != "gsk-abc" {
		t.Fatalf("env = %v, err = %v", env, err)
	}
}

func TestEffectiveSessionEnv_TheOperatorsDeclarationWinsOverALinkedKey(t *testing.T) {
	// Linked first, declared after (as a restart with a new config would): one
	// variable, one source, the operator's.
	d := linkDriver(t, nil)
	if _, err := d.LinkKey("deepseek", []byte(secretKey)); err != nil {
		t.Fatal(err)
	}
	d.sessionEnv = []sessionenv.Entry{{Name: "DEEPSEEK_API_KEY", FromFile: keyFile(t, "operator-key")}}
	entries, linked := d.effectiveSessionEnv()
	if len(entries) != 1 || len(linked) != 0 || entries[0].FromFile == "" {
		t.Fatalf("entries = %+v linked = %v", entries, linked)
	}
	view := d.Credentials()
	if len(view) != 1 || view[0].Source != fleet.SourceOperatorEnv {
		t.Fatalf("the view must show the operator's key, got %+v", view)
	}
	// Unlink still removes the shadowed linked file.
	if rem, err := d.UnlinkKey("deepseek"); err != nil || !rem.Removed {
		t.Fatalf("UnlinkKey = %+v, %v", rem, err)
	}
}

func TestLinkedKeyThatWentMissingIsDegradedAndSaysToLinkAgain(t *testing.T) {
	d := linkDriver(t, nil)
	if _, err := d.LinkKey("openai", []byte(secretKey)); err != nil {
		t.Fatal(err)
	}
	entries, _ := d.effectiveSessionEnv()
	if err := removeFile(entries[0].FromFile); err != nil {
		t.Fatal(err)
	}
	// With the file gone the entry is gone too (the directory is the truth), so the
	// view lists nothing rather than a row for something that is not stored.
	if view := d.Credentials(); len(view) != 0 {
		t.Fatalf("view = %+v", view)
	}
	// And the reason a linked key that is there but unusable gives names no path.
	reason := keyFileReason(sessionenv.Entry{Name: "OPENAI_API_KEY"}, sessionenv.FileUnusable, "file is empty", true)
	if !strings.Contains(reason, "link it again") || strings.Contains(reason, "declared") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestKeyEnvForProviderIsTheInverseOfTheViewTable(t *testing.T) {
	for provider, name := range keyEnvForProvider {
		if got := keyEnvProviders[name]; got != provider {
			t.Errorf("%s links as %s, which the view maps back to %q", provider, name, got)
		}
	}
}

func TestSubscription_StartPollSignOutAndTheRulesAroundThem(t *testing.T) {
	fl := &fakeLogins{}
	d := linkDriver(t, nil, withLogins(fl))
	started, err := d.StartLogin("openai")
	if err != nil || started.State != fleet.LoginPending || started.Code != "ABCD-1234" || started.URL == "" || started.ID == "" || started.ExpiresAt == nil {
		t.Fatalf("StartLogin = %+v, %v", started, err)
	}
	got, err := d.LoginStatus("openai", started.ID)
	if err != nil || got.State != fleet.LoginPending {
		t.Fatalf("LoginStatus = %+v, %v", got, err)
	}
	fl.finish(grant.LoginApproved, "")
	got, _ = d.LoginStatus("openai", started.ID)
	if got.State != fleet.LoginApproved || got.Code != "" || got.URL != "" {
		t.Fatalf("an approved sign-in must not show its code again: %+v", got)
	}
	if _, err := d.LoginStatus("openai", "someone-elses-id"); kindOf(err) != fleet.ErrorNotFound {
		t.Fatalf("an unknown attempt is not_found, got %v", err)
	}
	rem, err := d.SignOut("openai")
	if err != nil || !rem.Removed || rem.Kind != fleet.CredentialSubscription {
		t.Fatalf("SignOut = %+v, %v", rem, err)
	}
	if rem, _ := d.SignOut("openai"); rem.Removed {
		t.Fatal("a second sign-out removed something")
	}

	// Rules.
	if _, err := d.StartLogin("anthropic"); kindOf(err) != fleet.ErrorInvalid {
		t.Fatalf("only openai has a subscription here, got %v", err)
	}
	if _, err := linkDriver(t, nil, withPolicy("deepseek"), withLogins(fl)).StartLogin("openai"); kindOf(err) != fleet.ErrorInvalid {
		t.Fatalf("the policy does not permit openai, got %v", err)
	}
	if _, err := linkDriver(t, nil).StartLogin("openai"); kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("no login manager is unsupported, got %v", err)
	}
	// Sign-out is allowed whatever the policy says: cleaning up is never refused for it.
	if _, err := linkDriver(t, nil, withPolicy("deepseek"), withLogins(fl)).SignOut("openai"); err != nil {
		t.Fatalf("sign-out under a policy that excludes openai: %v", err)
	}
}

func withLogins(m LoginManager) func(*Driver) { return func(d *Driver) { d.logins = m } }

// fakeLogins is a LoginManager a test steers.
type fakeLogins struct {
	cur      *grant.LoginView
	signedIn bool
}

func (f *fakeLogins) Start() (grant.LoginView, bool, error) {
	if f.cur != nil && f.cur.State == grant.LoginPending {
		return *f.cur, false, nil
	}
	f.cur = &grant.LoginView{ID: "attempt-1", State: grant.LoginPending, URL: "https://example.invalid/device", Code: "ABCD-1234",
		ExpiresAt: time.Now().Add(15 * time.Minute)}
	return *f.cur, true, nil
}
func (f *fakeLogins) Get(id string) (grant.LoginView, bool) {
	if f.cur == nil || f.cur.ID != id {
		return grant.LoginView{}, false
	}
	return *f.cur, true
}
func (f *fakeLogins) SignOut() (bool, error) {
	was := f.signedIn
	f.signedIn = false
	return was, nil
}
func (f *fakeLogins) finish(state grant.LoginState, reason string) {
	f.cur.State, f.cur.Reason, f.cur.URL, f.cur.Code = state, reason, "", ""
	f.signedIn = state == grant.LoginApproved
}

func TestLinkedKeyReachesASessionAndAnUnlinkedOneDoesNot(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	linked := sessionenv.NewLinked(t.TempDir())
	d, _ := newIsolatedDriver(t, WithLinkedEnv(linked))

	if _, err := d.LinkKey("openai", []byte(secretKey)); err != nil {
		t.Fatal(err)
	}
	with := mustCreate(t, d, "with-key", fleet.SessionSpec{})
	if got := inside(t, d, with.ID).vars()[openaiKeyEnv]; got != secretKey {
		t.Fatalf("the session sees %q, want the linked key", got)
	}
	if rem, err := d.UnlinkKey("openai"); err != nil || !rem.Removed {
		t.Fatalf("UnlinkKey = %+v, %v", rem, err)
	}
	without := mustCreate(t, d, "without-key", fleet.SessionSpec{})
	if _, has := inside(t, d, without.ID).vars()[openaiKeyEnv]; has {
		t.Fatal("a session created after the unlink still gets the key")
	}
	// The earlier session keeps what it started with: unlinking removes the key from
	// what is given next, it does not reach into a process that already has it.
	if got := inside(t, d, with.ID).vars()[openaiKeyEnv]; got != secretKey {
		t.Fatalf("a running session's environment changed: %q", got)
	}
}

func removeFile(p string) error { return os.Remove(p) }
