package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/grant"
)

// muster #451: the machine's ChatGPT sign-in reaches each session as an access
// token only, and follows renewals into live sessions without a relaunch. The
// refresher itself is tested in internal/grant; here the source is a stand-in the
// test moves by hand, and what is asserted is what lands in a session's own
// directory.

type fakeGrants struct {
	mu   sync.Mutex
	acc  grant.Access
	err  error
	st   grant.State
	subs map[int]func(grant.Access)
	next int
}

func newFakeGrants(token string) *fakeGrants {
	return &fakeGrants{
		acc:  grant.Access{Token: token, ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Millisecond), AccountID: "acct-1"},
		st:   grant.State{Provider: grant.ProviderOpenAI, Phase: grant.PhaseOK, AccessExpiresAt: time.Now().Add(time.Hour)},
		subs: map[int]func(grant.Access){},
	}
}

func (f *fakeGrants) Access(context.Context) (grant.Access, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acc, f.err
}

func (f *fakeGrants) State() grant.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeGrants) Subscribe(fn func(grant.Access)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.next
	f.next++
	f.subs[id] = fn
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, id)
	}
}

func (f *fakeGrants) subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// rotate makes token current and tells every subscriber, as the refresher does.
func (f *fakeGrants) rotate(token string) {
	f.mu.Lock()
	f.acc.Token = token
	f.acc.ExpiresAt = time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	a := f.acc
	subs := make([]func(grant.Access), 0, len(f.subs))
	for _, s := range f.subs {
		subs = append(subs, s)
	}
	f.mu.Unlock()
	for _, s := range subs {
		s(a)
	}
}

type authEntry struct {
	Type      string `json:"type"`
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId"`
}

// authOf is the entry the session's own auth file holds for the provider.
func authOf(t *testing.T, dir string) (authEntry, map[string]json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(authPath(dir))
	if err != nil {
		t.Fatalf("reading the session's auth file: %v", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("the session's auth file is not JSON: %v", err)
	}
	var e authEntry
	if err := json.Unmarshal(all["openai"], &e); err != nil {
		t.Fatalf("no openai entry in %s: %v", raw, err)
	}
	return e, all
}

func sessionDir(t *testing.T, d *Driver, id string) string {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok || known.srv == nil {
		t.Fatalf("session %s is not running here", id)
	}
	return known.srv.dir
}

func TestGrant_ACreateGetsAnAccessTokenAndNoRefreshToken(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	dir := sessionDir(t, d, sess.ID)

	e, _ := authOf(t, dir)
	if e.Type != "oauth" || e.Access != "ACCESS-ONE" || e.AccountID != "acct-1" {
		t.Fatalf("entry = %+v, want an oauth entry carrying the access token and account", e)
	}
	if e.Refresh != grant.Placeholder {
		t.Fatalf("refresh slot = %q, want the placeholder: a session must never hold a refresh token", e.Refresh)
	}
	if e.Expires != g.acc.ExpiresAt.UnixMilli() {
		t.Fatalf("expires = %d, want %d", e.Expires, g.acc.ExpiresAt.UnixMilli())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(authPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("auth file mode %#o is not owner-only", perm)
		}
	}
}

func TestGrant_ARenewalIsWrittenInPlaceToEveryLiveSessionAndKeepsOtherEntries(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}))
	a := mustCreate(t, d, "k1", fleet.SessionSpec{})
	b := mustCreate(t, d, "k2", fleet.SessionSpec{})
	dirA, dirB := sessionDir(t, d, a.ID), sessionDir(t, d, b.ID)

	// Something else in one session's file (a key the runtime stored) survives.
	raw, _ := os.ReadFile(authPath(dirA))
	var all map[string]json.RawMessage
	_ = json.Unmarshal(raw, &all)
	all["other"] = json.RawMessage(`{"type":"api","key":"K"}`)
	raw, _ = json.Marshal(all)
	if err := os.WriteFile(authPath(dirA), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	idBefore, _ := os.Stat(authPath(dirB))
	g.rotate("ACCESS-TWO")

	for _, dir := range []string{dirA, dirB} {
		e, _ := authOf(t, dir)
		if e.Access != "ACCESS-TWO" || e.Refresh != grant.Placeholder {
			t.Fatalf("after a renewal %s holds %+v, want the new access token and still the placeholder", dir, e)
		}
	}
	_, kept := authOf(t, dirA)
	var other struct{ Type, Key string }
	if err := json.Unmarshal(kept["other"], &other); err != nil || other.Type != "api" || other.Key != "K" {
		t.Fatalf("another provider's entry was lost or changed: %s (%v)", kept["other"], err)
	}
	// Replaced by rename, never truncated in place: a reader never sees half a file.
	if idAfter, _ := os.Stat(authPath(dirB)); os.SameFile(idBefore, idAfter) {
		t.Fatal("the file was rewritten in place instead of replaced whole")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(authPath(dirB)), "auth-*.tmp")); len(left) > 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestGrant_NothingIsWrittenWhenThePolicyDoesNotPermitTheProvider(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	// The operator named a different provider: a sign-in makes openai USABLE, it never makes it ALLOWED.
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"deepseek"}))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	if _, err := os.Stat(authPath(sessionDir(t, d, sess.ID))); err == nil {
		t.Fatal("the credential was handed to a session whose policy does not permit the provider")
	}
	g.rotate("ACCESS-TWO")
	if _, err := os.Stat(authPath(sessionDir(t, d, sess.ID))); err == nil {
		t.Fatal("a renewal wrote the credential into a session whose policy does not permit the provider")
	}
	if got := d.Grants(); got != nil {
		t.Fatalf("Grants = %+v, want nothing for a provider the policy does not permit", got)
	}
}

func TestGrant_ARevokedOrAbsentSignInStartsTheSessionWithoutIt(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	for name, err := range map[string]error{
		"revoked":    &grant.RevokedError{Status: 401, Code: "token_expired"},
		"signed out": grant.ErrSignedOut,
	} {
		t.Run(name, func(t *testing.T) {
			g := newFakeGrants("X")
			g.err = err
			d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai", "deepseek"}))
			sess, cerr := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
			if cerr != nil {
				t.Fatalf("a session that needs no openai was refused because the sign-in is %s: %v", name, cerr)
			}
			if _, serr := os.Stat(authPath(sessionDir(t, d, sess.ID))); serr == nil {
				t.Fatalf("an auth file was written for a %s sign-in", name)
			}
		})
	}
}

func TestGrant_AWakeAfterAParkCarriesTheCurrentToken(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, clock, _, _ := newParkDriver(t, WithGrant(g), WithProviders([]string{"openai"}))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "sleeper"})
	dir := sessionDir(t, d, sess.ID)
	parkSession(t, d, clock, sess.ID)

	g.rotate("ACCESS-WHILE-PARKED")
	if _, err := sendText(t, d, sess.SessionRef, "wake up"); err != nil {
		t.Fatalf("send to a parked session: %v", err)
	}
	if e, _ := authOf(t, dir); e.Access != "ACCESS-WHILE-PARKED" || e.Refresh != grant.Placeholder {
		t.Fatalf("after the wake the session holds %+v, want the access token current at the relaunch", e)
	}
}

func TestGrant_AnUpdateNeverRecreatesASessionDirectoryThatIsGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	if err := writeSessionAuth(dir, grant.Access{Token: "T", ExpiresAt: time.Now().Add(time.Hour)}, false); err != nil {
		t.Fatalf("updating a closed session: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("an update recreated a closed session's directory")
	}
	// A start, by contrast, makes it.
	if err := writeSessionAuth(dir, grant.Access{Token: "T", ExpiresAt: time.Now().Add(time.Hour)}, true); err != nil {
		t.Fatal(err)
	}
	if e, _ := authOf(t, dir); e.Access != "T" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestGrant_ACatchUpWritesATokenThatMovedBetweenTheWriteAndTheTracking(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("ACCESS-ONE")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	dir := sessionDir(t, d, sess.ID)

	// A renewal that no subscription reached (the window the catch-up exists for).
	g.mu.Lock()
	g.acc.Token = "ACCESS-MISSED"
	g.mu.Unlock()
	d.catchUpGrant(context.Background(), dir)
	if e, _ := authOf(t, dir); e.Access != "ACCESS-MISSED" {
		t.Fatalf("entry = %+v, want the catch-up to have written the current token", e)
	}
}

func TestGrant_TheSubscriptionEndsWithTheDriver(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("X")
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai"}))
	if g.subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", g.subscribers())
	}
	_ = d.Shutdown()
	if g.subscribers() != 0 {
		t.Fatalf("subscribers = %d after Shutdown, want 0", g.subscribers())
	}
}

func TestGrant_GrantsReportsEachPhase(t *testing.T) {
	g := newFakeGrants("X")
	d := &Driver{grants: g, providers: providerPolicy{}}
	want := map[grant.Phase]fleet.GrantState{
		grant.PhaseOK:        fleet.GrantOK,
		grant.PhaseDegraded:  fleet.GrantDegraded,
		grant.PhaseRevoked:   fleet.GrantRevoked,
		grant.PhaseSignedOut: fleet.GrantSignedOut,
	}
	for phase, state := range want {
		g.mu.Lock()
		g.st = grant.State{Provider: grant.ProviderOpenAI, Phase: phase, Reason: "because"}
		g.mu.Unlock()
		got := d.Grants()
		if len(got) != 1 || got[0].State != state || got[0].Provider != "openai" || got[0].Reason != "because" {
			t.Fatalf("phase %s reported as %+v, want state %s", phase, got, state)
		}
	}
	// A driver with no sign-in reports nothing at all, not "signed out".
	if got := (&Driver{}).Grants(); got != nil {
		t.Fatalf("a driver with no sign-in reports %+v", got)
	}
	// And the report never carries a credential.
	raw, _ := json.Marshal(d.Grants())
	if strings.Contains(string(raw), "X\"") && strings.Contains(string(raw), "access\"") {
		t.Fatalf("the report may carry a credential: %s", raw)
	}
}

func TestGrant_ARefusalNamesTheSignInWhenItIsWhyTheProviderIsMissing(t *testing.T) {
	g := newFakeGrants("X")
	d := &Driver{grants: g, providers: providerPolicy{}}
	if hint := d.grantHint("openai/gpt-5.5"); hint != "" {
		t.Fatalf("a working sign-in produced the hint %q", hint)
	}
	g.mu.Lock()
	g.st = grant.State{Provider: "openai", Phase: grant.PhaseRevoked, Reason: (&grant.RevokedError{Status: 401}).Error()}
	g.mu.Unlock()
	hint := d.grantHint("openai/gpt-5.5")
	if !strings.Contains(hint, "revoked") || !strings.Contains(hint, "sign in again") {
		t.Fatalf("hint = %q, want it to say the sign-in is revoked and how to fix it", hint)
	}
	if hint := d.grantHint("deepseek/deepseek-flash"); hint != "" {
		t.Fatalf("a model on another provider got the hint %q", hint)
	}
}

func TestGrant_APassThroughOfARealErrorIsNotSwallowedAtCreate(t *testing.T) {
	// An error that is neither "signed out" nor a refusal of the sign-in is said,
	// and the session still starts: the sign-in being unavailable is not a reason to
	// refuse a session that may not use it.
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	g := newFakeGrants("X")
	g.err = errors.New("the store is locked by another process")
	var lines []string
	var mu sync.Mutex
	d, _ := newIsolatedDriver(t, WithGrant(g), WithProviders([]string{"openai", "deepseek"}), WithLogf(func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, f)
	}))
	mustCreate(t, d, "k1", fleet.SessionSpec{})
	mu.Lock()
	defer mu.Unlock()
	said := false
	for _, l := range lines {
		if strings.Contains(l, "without the openai sign-in") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the driver said nothing about starting a session without the sign-in: %v", lines)
	}
}

// Against the REAL runtime (opt-in, FLEET_OPENCODE_INTEGRATION=1; needs the binary,
// no network and no credential): the file this driver writes is one the runtime
// accepts as a sign-in. The proof is the refusal it prevents. A machine that allows
// only `openai` and gives a session no credential for it is refused at create
// ("no model provider is usable"); with the sign-in written the runtime lists the
// provider and the create succeeds. The second half is the measurement the design
// rests on — a running server reads the file again, so a renewal needs no relaunch —
// taken here only as far as the file goes; the turn-level proof is in the ADR.
func TestIntegration_ARealRuntimeAcceptsTheWrittenSignInAndTheFileFollowsARenewal(t *testing.T) {
	requireOpencodeIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cwd := fleet.AbsolutePath(t.TempDir())

	without, err := New(ctx, "test-machine", WithProviders([]string{"openai"}), WithSessionRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer without.Shutdown()
	if _, err := without.Create(ctx, testReq, "k0", fleet.SessionSpec{Cwd: cwd}); err == nil {
		t.Fatal("a session with no credential for the only allowed provider was created: the control for this test does not hold")
	}

	g := newFakeGrants("ACCESS-ONE")
	d, err := New(ctx, "test-machine", WithGrant(g), WithProviders([]string{"openai"}), WithSessionRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Shutdown()
	sess, err := d.Create(ctx, testReq, "k1", fleet.SessionSpec{Cwd: cwd})
	if err != nil {
		t.Fatalf("Create with the sign-in written: %v", err)
	}
	dir := sessionDir(t, d, sess.ID)
	if e, _ := authOf(t, dir); e.Access != "ACCESS-ONE" {
		t.Fatalf("entry = %+v", e)
	}
	g.rotate("ACCESS-TWO")
	if e, _ := authOf(t, dir); e.Access != "ACCESS-TWO" || e.Refresh != grant.Placeholder {
		t.Fatalf("after the renewal %+v", e)
	}
}
