package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/credrule"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #458: which of the provider's two credentials a session runs on is the
// operator's rule, enforced by what is ABSENT from the session. These tests start the
// stand-in runtime as a real child, so what is asserted is what the operating system
// handed it and what its own auth file holds.

const testKey = "sk-TEST-KEY"

// keyEntry is a sessionEnv entry delivering the key from a file, as an operator does.
func keyEntry(t *testing.T) sessionenv.Entry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openai-key")
	if err := os.WriteFile(path, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	return sessionenv.Entry{Name: openaiKeyEnv, FromFile: path}
}

func rule(use fleet.CredentialKind, scope sessionenv.Scope) credrule.Rule {
	return credrule.Rule{Provider: grant.ProviderOpenAI, Use: use, AppliesTo: scope}
}

// hasSignIn reports whether the session's auth file holds an openai entry.
func hasSignIn(t *testing.T, dir string) bool {
	t.Helper()
	raw, err := os.ReadFile(authPath(dir))
	if err != nil {
		return false
	}
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		t.Fatalf("auth file is not JSON: %s", raw)
	}
	_, ok := all[grant.ProviderOpenAI]
	return ok
}

// runsOn creates a session and reports what it was given: the key in its environment
// and the sign-in in its auth file.
func runsOn(t *testing.T, d *Driver, key string, spec fleet.SessionSpec) (keyInEnv, signIn bool, id string) {
	t.Helper()
	sess := mustCreate(t, d, key, spec)
	_, keyInEnv = inside(t, d, sess.ID).vars()[openaiKeyEnv]
	return keyInEnv, hasSignIn(t, sessionDir(t, d, sess.ID)), sess.ID
}

func TestDecide_TheWholePolicy(t *testing.T) {
	sub, key := fleet.CredentialSubscription, fleet.CredentialAPIKey
	cases := []struct {
		name        string
		in          credInputs
		wantKind    fleet.CredentialKind
		wantMissing fleet.CredentialKind
		wantStrips  bool
	}{
		{"rule subscription, both held", credInputs{rule: sub, haveKey: true, haveGrant: true}, sub, "", true},
		{"rule api-key, both held", credInputs{rule: key, haveKey: true, haveGrant: true}, key, "", false},
		{"rule subscription, no sign-in: the key is not a fallback", credInputs{rule: sub, haveKey: true}, "", sub, true},
		{"rule api-key, no key: the sign-in is not a fallback", credInputs{rule: key, haveGrant: true}, "", key, false},
		{"no rule, both held: the default is the subscription", credInputs{haveKey: true, haveGrant: true}, sub, "", true},
		{"no rule, only the sign-in", credInputs{haveGrant: true}, sub, "", true},
		{"no rule, only the key", credInputs{haveKey: true}, key, "", false},
		{"no rule, neither", credInputs{}, "", "", false},
	}
	for _, tc := range cases {
		got := decide(tc.in)
		if got.kind != tc.wantKind || got.missing != tc.wantMissing || got.stripsKey() != tc.wantStrips {
			t.Errorf("%s: decide = %+v (strips %v), want kind %q missing %q strips %v",
				tc.name, got, got.stripsKey(), tc.wantKind, tc.wantMissing, tc.wantStrips)
		}
	}
}

func TestCredential_ASubscriptionRuleGivesTheSignInAndWithholdsTheKey(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialSubscription, sessionenv.Scope{})}))

	keyInEnv, signIn, id := runsOn(t, d, "k1", fleet.SessionSpec{})
	if keyInEnv {
		t.Fatal("the subscription rule left the API key in the session's environment")
	}
	if !signIn {
		t.Fatal("the subscription rule did not give the session the sign-in")
	}
	if got := d.credentialOf(id); got != fleet.CredentialSubscription {
		t.Fatalf("credentialOf = %q", got)
	}
}

func TestCredential_AnAPIKeyRuleGivesTheKeyAndNoSignIn(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{})}))

	// The machine is signed in and holds the key: before the rule the runtime would
	// have taken the sign-in and ignored the key. With the rule the sign-in is not given.
	keyInEnv, signIn, id := runsOn(t, d, "k1", fleet.SessionSpec{})
	if !keyInEnv {
		t.Fatal("the api-key rule withheld the key")
	}
	if signIn {
		t.Fatal("the api-key rule still gave the session the sign-in")
	}
	if got := d.credentialOf(id); got != fleet.CredentialAPIKey {
		t.Fatalf("credentialOf = %q", got)
	}
}

func TestCredential_WithNoRuleASignedInMachineUsesTheSubscriptionAndSaysSo(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}))

	keyInEnv, signIn, id := runsOn(t, d, "k1", fleet.SessionSpec{})
	// The default is the runtime's measured order, decided here instead of left to it:
	// the key is withheld so a runtime that changed its order could not flip the account.
	if keyInEnv || !signIn {
		t.Fatalf("default: key in env %v, sign-in %v; want the subscription alone", keyInEnv, signIn)
	}
	if got := d.credentialOf(id); got != fleet.CredentialSubscription {
		t.Fatalf("credentialOf = %q", got)
	}
}

func TestCredential_WithNoRuleAndNoSignInTheKeyIsUsed(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("")
	g.err = grant.ErrSignedOut
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}))
	keyInEnv, signIn, id := runsOn(t, d, "k1", fleet.SessionSpec{})
	if !keyInEnv || signIn {
		t.Fatalf("key in env %v, sign-in %v; want the key alone", keyInEnv, signIn)
	}
	if got := d.credentialOf(id); got != fleet.CredentialAPIKey {
		t.Fatalf("credentialOf = %q", got)
	}
}

func TestCredential_TheRuleIsPerMarkerAndTheDefaultCoversTheRest(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{Markers: []string{"metered"}})}))

	keyA, signA, idA := runsOn(t, d, "k1", fleet.SessionSpec{Marker: "metered"})
	keyB, signB, idB := runsOn(t, d, "k2", fleet.SessionSpec{Marker: "other"})
	if !keyA || signA || d.credentialOf(idA) != fleet.CredentialAPIKey {
		t.Fatalf("the marked session: key %v sign-in %v kind %q; want the key alone", keyA, signA, d.credentialOf(idA))
	}
	if keyB || !signB || d.credentialOf(idB) != fleet.CredentialSubscription {
		t.Fatalf("the other session: key %v sign-in %v kind %q; want the sign-in alone", keyB, signB, d.credentialOf(idB))
	}
}

func TestCredential_ANamedCredentialThatIsMissingRefusesASessionBoundForTheProvider(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	t.Run("subscription named, signed out", func(t *testing.T) {
		g := newFakeGrants("")
		g.err = grant.ErrSignedOut
		d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
			WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
			WithCredentialRules(credrule.Rules{rule(fleet.CredentialSubscription, sessionenv.Scope{})}))
		_, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Model: "openai/gpt-5"})
		if kindOfErr(err) != fleet.ErrorInvalid {
			t.Fatalf("Create = %v, want a refusal", err)
		}
		for _, want := range []string{"subscription", "sign in", "key is not used in its place"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not say %q", err, want)
			}
		}
	})
	t.Run("api-key named, no key", func(t *testing.T) {
		g := newFakeGrants("ACCESS-ONE")
		d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
			WithCredentialRules(credrule.Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{})}))
		_, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Model: "openai/gpt-5"})
		if kindOfErr(err) != fleet.ErrorInvalid {
			t.Fatalf("Create = %v, want a refusal", err)
		}
		for _, want := range []string{"API key", "OPENAI_API_KEY", "sign-in is not used in its place"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not say %q", err, want)
			}
		}
	})
	t.Run("a session that can run elsewhere is not refused and gets neither", func(t *testing.T) {
		g := newFakeGrants("ACCESS-ONE")
		// The policy allows two providers and the create pins no model, so nothing says
		// the session needs openai: it starts without the credential its rule cannot supply.
		d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai", "deepseek"}),
			WithCredentialRules(credrule.Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{})}))
		keyInEnv, signIn, id := runsOn(t, d, "k1", fleet.SessionSpec{})
		if keyInEnv || signIn {
			t.Fatalf("key in env %v, sign-in %v; the unmet rule must not fall back to the other account", keyInEnv, signIn)
		}
		if got := d.credentialOf(id); got != "" {
			t.Fatalf("credentialOf = %q, want none", got)
		}
	})
}

func TestCredential_ACreatesOwnKeyIsRefusedUnderASubscriptionRule(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialSubscription, sessionenv.Scope{})}))
	_, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Env: map[string]string{openaiKeyEnv: "sk-CALLER"}})
	if kindOfErr(err) != fleet.ErrorInvalid || !strings.Contains(err.Error(), openaiKeyEnv) {
		t.Fatalf("Create = %v, want the caller's key refused by name", err)
	}
}

func TestCredential_ARuleNeverAllowsAProviderThePolicyRefuses(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"deepseek"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialSubscription, sessionenv.Scope{})}))
	// openai is not allowed here, so the rule is moot: nothing is written, nothing is
	// stripped, and nothing is refused.
	keyInEnv, signIn, id := runsOn(t, d, "k1", fleet.SessionSpec{})
	if signIn {
		t.Fatal("the rule handed the sign-in to a session whose policy does not permit the provider")
	}
	if !keyInEnv {
		t.Fatal("the rule removed a variable from a session it does not apply to")
	}
	if got := d.credentialOf(id); got != "" {
		t.Fatalf("credentialOf = %q, want none", got)
	}
}

func TestCredential_ARelaunchUnderAChangedRuleRemovesTheSignInAnEarlierStartWrote(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(authPath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionAuth(dir, grant.Access{Token: "OLD", ExpiresAt: time.Now().Add(time.Hour)}, true); err != nil {
		t.Fatal(err)
	}
	// Something else the runtime keeps in the same file survives.
	raw, _ := os.ReadFile(authPath(dir))
	var all map[string]json.RawMessage
	_ = json.Unmarshal(raw, &all)
	all["other"] = json.RawMessage(`{"type":"api","key":"K"}`)
	raw, _ = json.Marshal(all)
	if err := os.WriteFile(authPath(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	d := &Driver{providers: providerPolicy{}, grants: g} // isolated mode, policy open: the rule applies
	if err := d.applyCredential(dir, credentialPlan{kind: fleet.CredentialAPIKey}); err != nil {
		t.Fatalf("applyCredential: %v", err)
	}
	if hasSignIn(t, dir) {
		t.Fatal("the stale sign-in is still in the file")
	}
	_, kept := authOf2(t, dir)
	if _, ok := kept["other"]; !ok {
		t.Fatalf("another entry was lost: %v", kept)
	}
	// And a directory with no auth file at all is not an error.
	if err := d.applyCredential(t.TempDir(), credentialPlan{}); err != nil {
		t.Fatalf("applyCredential on a fresh directory: %v", err)
	}
}

// authOf2 reads the whole auth file without requiring an openai entry.
func authOf2(t *testing.T, dir string) (struct{}, map[string]json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(authPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	return struct{}{}, all
}

func TestCredential_ARenewalReachesOnlySessionsOnTheSubscription(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}),
		WithSessionEnv([]sessionenv.Entry{keyEntry(t)}),
		WithCredentialRules(credrule.Rules{rule(fleet.CredentialAPIKey, sessionenv.Scope{Markers: []string{"metered"}})}))
	_, _, onKey := runsOn(t, d, "k1", fleet.SessionSpec{Marker: "metered"})
	_, _, onSub := runsOn(t, d, "k2", fleet.SessionSpec{})

	g.rotate("ACCESS-TWO")

	if hasSignIn(t, sessionDir(t, d, onKey)) {
		t.Fatal("a renewal handed the sign-in to a session the rule put on the API key")
	}
	if e, _ := authOf(t, sessionDir(t, d, onSub)); e.Access != "ACCESS-TWO" {
		t.Fatalf("the subscription session holds %q after a renewal", e.Access)
	}
}

func TestUsage_NamesTheCredentialItRanOn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	f.setLastMessage(ref.ID, userMsg("u1", 1))
	a1 := asstMsg("a1", 2, 3, step(tok(10, 1, nil, 0, 0), nil))
	a1.Info.ProviderID = "openai"
	f.setLastMessage(ref.ID, a1)

	if u := readUsage(t, d, ref.ID).Usage; u == nil || u.Credential != "" {
		t.Fatalf("a session the service chose no credential for reports %+v; want none named", u)
	}
	for _, kind := range []fleet.CredentialKind{fleet.CredentialSubscription, fleet.CredentialAPIKey} {
		d.shared.credential = kind
		u := readUsage(t, d, ref.ID).Usage
		if u == nil || u.Credential != kind {
			t.Fatalf("usage = %+v, want credential %q", u, kind)
		}
		raw, _ := json.Marshal(u)
		if !strings.Contains(string(raw), `"credential":"`+string(kind)+`"`) {
			t.Fatalf("the wire form %s does not carry the kind", raw)
		}
	}
	// Figures spent on another provider are on neither openai account.
	other := asstMsg("a2", 4, 5, step(tok(7, 1, nil, 0, 0), nil))
	other.Info.ProviderID = "deepseek"
	f.setLastMessage(ref.ID, other)
	if u := readUsage(t, d, ref.ID).Usage; u == nil || u.Credential != "" {
		t.Fatalf("usage = %+v, want no credential named for a request that ran on another provider", u)
	}
	// A listing names it too, from what the last read learned, once the newest request is
	// on openai again.
	a3 := asstMsg("a3", 6, 7, step(tok(3, 1, nil, 0, 0), nil))
	a3.Info.ProviderID = "openai"
	f.setLastMessage(ref.ID, a3)
	readUsage(t, d, ref.ID)
	d.shared.credential = fleet.CredentialSubscription
	if u := d.usageSnapshot(ref.ID, true); u == nil || u.Credential != fleet.CredentialSubscription {
		t.Fatalf("snapshot = %+v", u)
	}
}

// ---- the grant's quota state -------------------------------------------------

type limitSink struct {
	*fakeGrants
	mu2    sync.Mutex
	notes  []string
	clears int
	fail   error
}

func (l *limitSink) NoteLimit(since time.Time, msg string) error {
	l.mu2.Lock()
	defer l.mu2.Unlock()
	l.notes = append(l.notes, msg)
	return l.fail
}

func (l *limitSink) ClearLimit() error {
	l.mu2.Lock()
	defer l.mu2.Unlock()
	l.clears++
	return l.fail
}

func (l *limitSink) counts() (notes, clears int) {
	l.mu2.Lock()
	defer l.mu2.Unlock()
	return len(l.notes), l.clears
}

// testClock is a clock the test moves by hand, safe to read from the driver's goroutines.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newQuotaDriver is a driver on the shared-mode fake runtime whose one server is
// marked as running on kind, so the tracker can be driven by moving the fake's status.
func newQuotaDriver(t *testing.T, kind fleet.CredentialKind) (*Driver, *fakeServer, *limitSink, *testClock) {
	t.Helper()
	f := newFakeServer(t)
	sink := &limitSink{fakeGrants: newFakeGrants("T")}
	clock := &testClock{t: time.Now()}
	d, err := newDriverWithOptions(t, f, WithGrant(sink), WithProviders([]string{"openai"}), withClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	d.shared.credential = kind
	return d, f, sink, clock
}

func TestQuota_SessionsStuckInRetryOnTheSubscriptionAreOneStateOfTheGrant(t *testing.T) {
	d, f, sink, clock := newQuotaDriver(t, fleet.CredentialSubscription)
	a := createOne(t, d, "/work/a", "k1")
	b := createOne(t, d, "/work/b", "k2")
	read := func(ref fleet.SessionRef) { readUsage(t, d, ref.ID) }

	// Attempt 1 and 2 are a blip: the runtime retries a hiccup the same way.
	f.setRetry(a.ID, 2, "The usage limit has been reached")
	read(a)
	if q := d.liveQuota(); q != nil {
		t.Fatalf("a second attempt already reads as a limit: %+v", q)
	}

	f.setRetry(a.ID, 3, "The usage limit has been reached")
	read(a)
	f.setRetry(b.ID, 5, "The usage limit has been reached")
	read(b)
	q := d.liveQuota()
	if q == nil || q.State != fleet.QuotaLimited || q.Sessions == nil || *q.Sessions != 2 ||
		q.Message != "The usage limit has been reached" {
		t.Fatalf("quota = %+v, want limited with both sessions and the provider's own words", q)
	}
	if n, _ := sink.counts(); n != 1 {
		t.Fatalf("the store was told %d times, want once for the episode", n)
	}

	// Still true a minute later: the heartbeat refreshes the mark so the doctor can tell
	// a live episode from one a stopped service left behind.
	clock.advance(limitHeartbeat + time.Second)
	read(a)
	if n, _ := sink.counts(); n != 2 {
		t.Fatalf("after the heartbeat interval the store was told %d times, want 2", n)
	}

	// One session recovers: the grant is still limited. Both: it is not.
	f.clearStatus(a.ID)
	read(a)
	if q := d.liveQuota(); q == nil || *q.Sessions != 1 {
		t.Fatalf("quota = %+v after one session recovered", q)
	}
	f.clearStatus(b.ID)
	read(b)
	if q := d.liveQuota(); q != nil {
		t.Fatalf("quota = %+v after every session recovered, want none", q)
	}
	if _, c := sink.counts(); c != 1 {
		t.Fatalf("the store was cleared %d times, want once, when the episode ended", c)
	}
}

func TestQuota_AClosedSessionDoesNotHoldAnEpisodeOpen(t *testing.T) {
	d, f, sink, _ := newQuotaDriver(t, fleet.CredentialSubscription)
	a := createOne(t, d, "/work/a", "k1")
	f.setRetry(a.ID, 4, "limit")
	readUsage(t, d, a.ID)
	if d.liveQuota() == nil {
		t.Fatal("no quota state to close")
	}
	d.forgetUsage(a.ID) // what a session's end calls
	if q := d.liveQuota(); q != nil {
		t.Fatalf("quota = %+v after the only refused session ended", q)
	}
	if _, c := sink.counts(); c != 1 {
		t.Fatalf("cleared %d times, want once, when the episode ended", c)
	}
}

func TestQuota_ASessionOnAnAPIKeyIsNotTheSubscriptionsLimit(t *testing.T) {
	d, f, sink, _ := newQuotaDriver(t, fleet.CredentialAPIKey)
	a := createOne(t, d, "/work/a", "k1")
	f.setRetry(a.ID, 9, "Rate limit reached for gpt-5 on tokens per min")
	readUsage(t, d, a.ID)
	if q := d.liveQuota(); q != nil {
		t.Fatalf("a per-token rate limit on a key was reported as the subscription's: %+v", q)
	}
	if n, _ := sink.counts(); n != 0 {
		t.Fatalf("the store was told %d times", n)
	}
}

func TestQuota_AFailingStoreNeverFailsARead(t *testing.T) {
	d, f, sink, _ := newQuotaDriver(t, fleet.CredentialSubscription)
	sink.fail = errors.New("disk full")
	a := createOne(t, d, "/work/a", "k1")
	f.setRetry(a.ID, 4, "limit")
	if st := readUsage(t, d, a.ID); st.Status != fleet.StatusWorking {
		t.Fatalf("status = %q, want the read unaffected", st.Status)
	}
	if d.liveQuota() == nil {
		t.Fatal("the live view should not depend on the store")
	}
}

func TestQuota_TheProvidersMessageIsCutToOneShortLine(t *testing.T) {
	long := strings.Repeat("word ", 200)
	got := cutMessage("  first\n\nsecond\t" + long)
	if strings.ContainsAny(got, "\n\t") || len([]rune(got)) > limitMessageMax {
		t.Fatalf("cutMessage = %q (%d runes)", got, len([]rune(got)))
	}
	if !strings.HasPrefix(got, "first second word") {
		t.Fatalf("cutMessage = %q", got)
	}
}

func TestQuota_TheGrantsListingCarriesTheLiveStateAndAStartClearsAStaleMark(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	sink := &limitSink{fakeGrants: newFakeGrants("ACCESS-ONE")}
	d, _ := newIsolatedDriver(t, WithGrant(sink), WithProviders([]string{"openai"}))
	if _, c := sink.counts(); c != 1 {
		t.Fatalf("a start cleared the store's mark %d times, want once: a mark an earlier process left is not known to be true now", c)
	}
	if q := d.Grants()[0].Quota; q != nil {
		t.Fatalf("quota = %+v on a machine nobody is refusing", q)
	}
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	d.observeStatus(known.srv, sess.ID, true, wireStatus{Type: "retry", Attempt: 6, Message: "The usage limit has been reached"})
	g := d.Grants()[0]
	if g.State != fleet.GrantOK {
		t.Fatalf("state = %q: a limited plan is still a good sign-in", g.State)
	}
	if g.Quota == nil || g.Quota.State != fleet.QuotaLimited || g.Quota.Message != "The usage limit has been reached" {
		t.Fatalf("quota = %+v", g.Quota)
	}
	raw, _ := json.Marshal(g)
	if !strings.Contains(string(raw), `"quota":{"state":"limited"`) {
		t.Fatalf("the wire form %s does not carry the quota", raw)
	}
}

func TestQuota_ASessionNobodyHasSeenForALongTimeNoLongerHoldsTheEpisodeOpen(t *testing.T) {
	d, f, sink, clock := newQuotaDriver(t, fleet.CredentialSubscription)
	a := createOne(t, d, "/work/a", "k1")
	f.setRetry(a.ID, 4, "limit")
	readUsage(t, d, a.ID)
	if d.liveQuota() == nil {
		t.Fatal("no quota state to age out")
	}
	// Its server died while it was retrying: nothing reports it again.
	clock.advance(limitStale + time.Second)
	if q := d.liveQuota(); q != nil {
		t.Fatalf("quota = %+v ten minutes after the last sighting, want none", q)
	}
	// The store's mark is cleared the next time anything is observed.
	b := createOne(t, d, "/work/b", "k2")
	readUsage(t, d, b.ID)
	if _, c := sink.counts(); c != 1 {
		t.Fatalf("cleared %d times, want once", c)
	}
}

func TestCredential_ARelaunchJudgesBoundnessByTheModelTheCreateAskedFor(t *testing.T) {
	rec := &sessionRecord{Cwd: "/work", SpecModel: "openai/gpt-5", SpecMarker: "m"}
	if got := rec.spec(); got.Model != "openai/gpt-5" || got.Marker != "m" {
		t.Fatalf("spec = %+v, want the pinned model carried so a relaunch refuses as the create did", got)
	}
}
