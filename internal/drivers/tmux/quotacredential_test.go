package tmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

// muster #490: one machine runs sessions on several credentials, and a usage
// limit belongs to the credential, not to the machine. The measured failure: one
// credential hit its limit and every session on the machine read quota_blocked,
// including the ones on credentials that still had quota, so a supervisor that
// obeyed machine.quota stopped the whole machine.
//
// What these tests drive: two sessions started with two different tokens. alpha
// is refused, beta is not, and nothing on beta's credential says otherwise.

// The tokens are put together at run time so that no secret scanner, reading
// this file, has a token-shaped string to find.
var (
	secretA = "sk-ant-" + "oat01-ALPHA-SECRET-VALUE-do-not-leak-0123456789"
	secretB = "sk-ant-" + "oat01-BETA-SECRET-VALUE-do-not-leak-9876543210"
)

var (
	credA = credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": secretA})
	credB = credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": secretB})
)

// credentialFixture is two sessions on two credentials, a state store, and a
// record for each. alpha's record ends in the refusal
// at refusedAt and its screen still shows the notice; beta served a turn before
// the refusal only.
type credentialFixture struct {
	f         *fakeMux
	root      string
	d         *Driver
	ctl       *ctlRegistry
	store     *state.Store
	storeDir  string
	refusedAt time.Time
	logs      *bytes.Buffer
}

func newCredentialFixture(t *testing.T) *credentialFixture {
	t.Helper()
	f := twoSessions()
	// What each session's create left on it: the name of the credential it was
	// started with (credentialOption).
	f.sessions[0].credential = credA
	f.sessions[1].credential = credB
	f.captures["%1"] = refusalScreen
	f.captures["%2"] = idleFixtureFor("beta")
	root := t.TempDir()
	refusedAt := sessionStart.Add(10 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		rateLimitEntry("conv-alpha", refusedAt, "9:50pm (Asia/Saigon)"))
	writeConversationWithAPIError(t, root, "/work/beta", "conv-beta", "beta", sessionStart,
		cleanTurnEntry("conv-beta", sessionStart.Add(5*time.Minute)))
	storeDir := t.TempDir()
	store, err := state.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	r := &ctlRegistry{}
	d := New("testbox", withExec(f.exec), withCtlDialer(r.dial), WithState(store), WithRecordRoot(root),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }))
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &credentialFixture{f: f, root: root, d: d, ctl: r, store: store, storeDir: storeDir, refusedAt: refusedAt, logs: &logs}
}

func (c *credentialFixture) list(t *testing.T) fleet.Collection[fleet.Session] {
	t.Helper()
	col, err := c.d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return col
}

func (c *credentialFixture) blocked(credential string) bool {
	q, _ := c.d.quotaBlockFor(credential)
	return q != nil
}

func quotaCredentials(col fleet.Collection[fleet.Session]) []string {
	var out []string
	for _, src := range col.Sources() {
		for _, q := range src.Quotas {
			out = append(out, q.Credential)
		}
	}
	return out
}

// The headline (#490): the session whose credential was refused reads
// quota_blocked, and a session started with another credential does not, though
// both are on one machine and both look idle.
func TestAQuotaBlockReachesOnlyTheSessionsOnTheCredentialThatWasRefused(t *testing.T) {
	c := newCredentialFixture(t)
	col := c.list(t)

	alpha, beta := sessionOf(t, col, "alpha💬"), sessionOf(t, col, "beta")
	if alpha.State.Status != fleet.StatusQuotaBlocked {
		t.Fatalf("alpha's credential was refused and alpha reads %q", alpha.State.Status)
	}
	if q := alpha.State.Quota; q == nil || q.Credential != credA || !q.Since.Equal(c.refusedAt) {
		t.Fatalf("alpha's block = %+v, want credential %q dated at the refusal %v", q, credA, c.refusedAt)
	}
	if beta.State.Status == fleet.StatusQuotaBlocked || beta.State.Quota != nil {
		t.Fatalf("beta runs on a credential nobody refused and reads %q (quota %+v)", beta.State.Status, beta.State.Quota)
	}
	if got := quotaCredentials(col); len(got) != 1 || got[0] != credA {
		t.Errorf("the source lists blocked credentials %v, want only %q", got, credA)
	}
	if c.blocked(credB) {
		t.Error("beta's credential was blocked by a refusal on another")
	}
}

// A turn another credential served proves nothing about this one. Clearing is
// per credential: beta serving after the refusal leaves alpha's block, and alpha
// serving clears it.
func TestEvidenceFromAnotherCredentialClearsNothing(t *testing.T) {
	c := newCredentialFixture(t)
	c.list(t)
	if !c.blocked(credA) {
		t.Fatal("fixture never blocked alpha's credential; the test proves nothing")
	}

	appendRecordLine(t, c.root, "/work/beta", "conv-beta",
		cleanTurnEntry("conv-beta", c.refusedAt.Add(20*time.Minute)))
	col := c.list(t)
	if !c.blocked(credA) {
		t.Fatal("a turn served on beta's credential cleared the block on alpha's")
	}
	if got := sessionOf(t, col, "alpha💬").State.Status; got != fleet.StatusQuotaBlocked {
		t.Fatalf("alpha reads %q after a turn on another credential, want quota_blocked", got)
	}

	// And a session seen working on the other credential is no proof either.
	c.f.setCapture("%2", fixtureWorking)
	c.list(t)
	if !c.blocked(credA) {
		t.Fatal("a session working on another credential cleared the block")
	}
	c.f.setCapture("%2", idleFixtureFor("beta"))

	appendRecordLine(t, c.root, "/work/alpha", "conv-alpha",
		cleanTurnEntry("conv-alpha", c.refusedAt.Add(30*time.Minute)))
	col = c.list(t)
	if c.blocked(credA) {
		t.Fatal("a turn served on alpha's own credential after the refusal did not clear its block")
	}
	if got := sessionOf(t, col, "alpha💬").State.Status; got == fleet.StatusQuotaBlocked {
		t.Fatal("alpha still reads quota_blocked after its credential served")
	}
	if got := quotaCredentials(col); len(got) != 0 {
		t.Errorf("the source still lists blocked credentials %v", got)
	}
}

// A single-session read applies the block of the session's own credential, the
// way a listing does. This is the read a create's 201 is built from: a session
// started on a refused credential must not come back as an unqualified idle, and
// one started on another must not be told its credential is refused.
func TestAOneSessionReadAppliesTheBlockOfItsOwnCredential(t *testing.T) {
	c := newCredentialFixture(t)
	// Two idle sessions that have never shown a notice: one on the refused
	// credential, one on neither.
	c.f.sessions = append(c.f.sessions,
		fakeSession{name: "gamma", paneID: "%3", cwd: "/work/gamma", pid: 300, created: 1785600002, title: "2_1_220", credential: credA},
		fakeSession{name: "delta", paneID: "%4", cwd: "/work/delta", pid: 400, created: 1785600003, title: "2_1_220"})
	c.f.captures["%3"] = idleFixtureFor("gamma")
	c.f.captures["%4"] = idleFixtureFor("delta")
	c.list(t)
	if !c.blocked(credA) {
		t.Fatal("fixture never blocked alpha's credential; the test proves nothing")
	}

	read := func(id string) fleet.SessionState {
		st, err := c.d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id, Name: id})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := read("gamma"); st.Status != fleet.StatusQuotaBlocked || st.Quota == nil || st.Quota.Credential != credA {
		t.Errorf("gamma shares the refused credential and reads %q (quota %+v)", st.Status, st.Quota)
	}
	for _, id := range []string{"beta", "delta"} {
		if st := read(id); st.Status == fleet.StatusQuotaBlocked || st.Quota != nil {
			t.Errorf("%s runs on a credential nobody refused and reads %q (quota %+v)", id, st.Status, st.Quota)
		}
	}
	// A session whose own screen shows the notice names its credential too.
	if st := read("alpha💬"); st.Status != fleet.StatusQuotaBlocked || st.Quota == nil || st.Quota.Credential != credA {
		t.Errorf("alpha reads %q (quota %+v), want quota_blocked naming %q", st.Status, st.Quota, credA)
	}
}

// The event a supervisor acts on names the credential. Told once when alpha's
// credential is refused, never about beta's, and told explicitly when it serves
// again, once, for that credential alone.
func TestMachineQuotaNamesOnlyTheCredentialThatChanged(t *testing.T) {
	c := newCredentialFixture(t)
	s, err := c.d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fire(c.ctl.contentFor("alpha💬"), "output")
	p, ok := awaitQuota(t, s)
	if !ok {
		t.Fatal("a credential was refused and the stream never said so")
	}
	if p.Credential != credA || !p.Blocked || p.Quota == nil || p.Quota.Credential != credA {
		t.Fatalf("payload = %+v, want blocked for %q", p, credA)
	}
	if len(p.Quotas) != 1 || p.Quotas[0].Credential != credA {
		t.Errorf("quotas = %+v, want the one blocked credential %q", p.Quotas, credA)
	}
	if p.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", p.Machine)
	}

	// beta serves a turn: not a transition of anything, and not evidence about
	// alpha's credential, so no event at all.
	appendRecordLine(t, c.root, "/work/beta", "conv-beta",
		cleanTurnEntry("conv-beta", c.refusedAt.Add(20*time.Minute)))
	fire(c.ctl.contentFor("alpha💬"), "output")
	if extra := quotaEventsWithin(t, s, 700*time.Millisecond); len(extra) != 0 {
		t.Fatalf("a turn on another credential produced machine.quota events %+v", extra)
	}

	appendRecordLine(t, c.root, "/work/alpha", "conv-alpha",
		cleanTurnEntry("conv-alpha", c.refusedAt.Add(30*time.Minute)))
	fire(c.ctl.contentFor("alpha💬"), "output")
	events := quotaEventsWithin(t, s, 2500*time.Millisecond, 1)
	if len(events) != 1 {
		t.Fatalf("clearing one credential produced %d machine.quota events %+v, want exactly one", len(events), events)
	}
	if got := events[0]; got.Credential != credA || got.Blocked || got.Quota != nil {
		t.Errorf("payload = %+v, want an explicit blocked:false for %q alone", got, credA)
	}
	if got := events[0]; got.Quotas == nil || len(got.Quotas) != 0 {
		t.Errorf("quotas = %#v, want present and empty: nothing is blocked any more", got.Quotas)
	}
	raw, _ := json.Marshal(events[0])
	if !strings.Contains(string(raw), `"quotas":[]`) {
		t.Errorf("the wire form %s does not carry an empty quotas list", raw)
	}
}

// Two blocked credentials are two blocks: clearing the first leaves the second
// in force, and each transition is its own event.
func TestOneCredentialRecoveringLeavesTheOtherBlocked(t *testing.T) {
	c := newCredentialFixture(t)
	c.f.setCapture("%2", refusalScreen)
	writeConversationWithAPIError(t, c.root, "/work/beta", "conv-beta", "beta", sessionStart,
		rateLimitEntry("conv-beta", c.refusedAt.Add(time.Minute), "9:50pm (Asia/Saigon)"))

	s, err := c.d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fire(c.ctl.contentFor("alpha💬"), "output")
	announced := map[string]bool{}
	for _, p := range quotaEventsWithin(t, s, 2500*time.Millisecond, 2) {
		if !p.Blocked {
			t.Fatalf("an unblocked event before anything recovered: %+v", p)
		}
		announced[p.Credential] = true
	}
	if !announced[credA] || !announced[credB] || len(announced) != 2 {
		t.Fatalf("announced %v, want one event for each of %q and %q", announced, credA, credB)
	}

	// Only alpha's credential serves again.
	c.f.setCapture("%1", idleFixtureFor("alpha"))
	appendRecordLine(t, c.root, "/work/alpha", "conv-alpha",
		cleanTurnEntry("conv-alpha", c.refusedAt.Add(30*time.Minute)))
	fire(c.ctl.contentFor("alpha💬"), "output")
	events := quotaEventsWithin(t, s, 2500*time.Millisecond, 1)
	if len(events) != 1 || events[0].Credential != credA || events[0].Blocked {
		t.Fatalf("events = %+v, want the clear of %q alone", events, credA)
	}
	if q := events[0].Quotas; len(q) != 1 || q[0].Credential != credB {
		t.Errorf("the clear's quotas = %+v, want %q still blocked", q, credB)
	}
	col := c.list(t)
	if got := quotaCredentials(col); len(got) != 1 || got[0] != credB {
		t.Errorf("the source lists %v, want only %q still blocked", got, credB)
	}
	if got := sessionOf(t, col, "beta").State.Status; got != fleet.StatusQuotaBlocked {
		t.Errorf("beta reads %q, its credential is still refused", got)
	}
}

// A session started with none of its own credential, a run this driver did not
// start, and one started before the option existed are all the ambient
// credential: one block among them, as the machine-wide block was.
func TestSessionsWithoutACredentialShareTheAmbientOne(t *testing.T) {
	f := twoSessions() // neither session carries credentialOption
	f.captures["%1"] = refusalScreen
	f.captures["%2"] = idleFixtureFor("beta")
	root := t.TempDir()
	refusedAt := sessionStart.Add(10 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		rateLimitEntry("conv-alpha", refusedAt, "9:50pm (Asia/Saigon)"))
	writeConversationWithAPIError(t, root, "/work/beta", "conv-beta", "beta", sessionStart,
		cleanTurnEntry("conv-beta", sessionStart.Add(5*time.Minute)))
	d := New("testbox", withExec(f.exec), WithRecordRoot(root),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }))

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha💬", "beta"} {
		sess := sessionOf(t, col, name)
		if sess.State.Status != fleet.StatusQuotaBlocked || sess.State.Quota == nil || sess.State.Quota.Credential != credentialDefault {
			t.Errorf("%s: status %q quota %+v, want quota_blocked on %q", name, sess.State.Status, sess.State.Quota, credentialDefault)
		}
	}
}

// A create names the credential it started the session with, on the session
// itself, and never writes the credential: not on a command line, not in any file
// the driver wrote, not in anything it logged. A create with none of its own
// leaves the session unmarked: an unset option is the ambient credential.
func TestCreateNamesTheCredentialOnTheSessionAndNeverWritesTheValue(t *testing.T) {
	f := twoSessions()
	storeDir := t.TempDir()
	store, err := state.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	d := New("testbox", withExec(f.exec), WithState(store), withNonce(func() string { return testNonce }))
	tokened, err := d.Create(context.Background(), testCaller, "key-1", fleet.SessionSpec{
		Name: "tokened", Cwd: "/work/x",
		Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": secretA},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	plain, err := d.Create(context.Background(), testCaller, "key-2", fleet.SessionSpec{Name: "plain", Cwd: "/work/y"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	set := map[string]string{} // session -> the credential option's value
	for _, call := range f.callsSnapshot() {
		if len(call) == 5 && call[0] == "set-option" && call[1] == "-t" && call[3] == credentialOption {
			set[call[2]] = call[4]
		}
	}
	if set[tokened.ID] != credA {
		t.Errorf("the token session's credential option = %q, want %q (calls: %v)", set[tokened.ID], credA, f.callsSnapshot())
	}
	if v, marked := set[plain.ID]; marked {
		t.Errorf("a session with no credential of its own was marked %q; unset must mean the ambient credential", v)
	}

	surfaces := map[string]string{
		"the created session": fmt.Sprintf("%+v", tokened),
		"the log":             logs.String(),
		"a command line":      fmt.Sprint(f.callsSnapshot()),
	}
	files, err := filepath.Glob(filepath.Join(storeDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		surfaces[filepath.Base(name)] = string(raw)
	}
	for where, text := range surfaces {
		if strings.Contains(text, secretA) {
			t.Errorf("the credential's value is in %s", where)
		}
	}
}

// No secret reaches a listing, a source, an event or the store through the
// blocks this fixture produces, whichever credential they name.
func TestNoCredentialValueAppearsInStateEventsOrFiles(t *testing.T) {
	c := newCredentialFixture(t)
	s, err := c.d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fire(c.ctl.contentFor("alpha💬"), "output")
	var events []fleet.MachineQuotaPayload
	if p, ok := awaitQuota(t, s); ok {
		events = append(events, p)
	}
	col := c.list(t)

	surfaces := map[string][]byte{}
	for name, v := range map[string]any{"the listing": col, "the events": events} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		surfaces[name] = raw
	}
	files, _ := filepath.Glob(filepath.Join(c.storeDir, "*"))
	for _, name := range files {
		if raw, err := os.ReadFile(name); err == nil {
			surfaces[filepath.Base(name)] = raw
		}
	}
	surfaces["the log"] = c.logs.Bytes()
	if len(events) == 0 {
		t.Fatal("no machine.quota event to inspect; the test proves nothing")
	}
	for where, raw := range surfaces {
		for _, secret := range []string{secretA, secretB} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("a credential's value is in %s", where)
			}
		}
	}
}

// Blocks survive a restart, per credential.
func TestPerCredentialBlocksSurviveARestart(t *testing.T) {
	c := newCredentialFixture(t)
	c.list(t)
	if !c.blocked(credA) {
		t.Fatal("fixture never blocked alpha's credential; the test proves nothing")
	}

	again := New("testbox", withExec(c.f.exec), WithState(c.store), WithRecordRoot(c.root),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }))
	if q, _ := again.quotaBlockFor(credA); q == nil || !q.Since.Equal(c.refusedAt) || q.Credential != credA {
		t.Fatalf("after a restart alpha's credential reads %+v, want its block back dated at the refusal", q)
	}
	if q, _ := again.quotaBlockFor(credB); q != nil {
		t.Errorf("a restart blocked beta's credential: %+v", q)
	}
}

// What an older build wrote — one block for the machine, `since` at the top of
// the object — is still read. It is attributed to the ambient credential, so a
// restart onto this build does not silently forget a limit that has days to run.
func TestAQuotaFileFromBeforeCredentialsStillLoads(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	since := sessionStart.Add(time.Hour).UTC()
	if err := os.WriteFile(filepath.Join(store.Dir(), "quota.json"),
		[]byte(`{"since":"`+since.Format(time.RFC3339)+`","resetHint":"Aug 10 at 12am","sinceObserved":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New("testbox", WithState(store))
	q, observed := d.quotaBlockFor(credentialDefault)
	if q == nil || !q.Since.Equal(since) || q.ResetHint != "Aug 10 at 12am" || q.Credential != credentialDefault || !observed {
		t.Fatalf("an old single-block file read as %+v (record-confirmed %v), want the block on %q", q, observed, credentialDefault)
	}
	if blocks := d.quotaBlocks(); len(blocks) != 1 {
		t.Errorf("blocks = %+v, want exactly the migrated one", blocks)
	}

	// And an old file that held no block (the cleared form: a zero since) is no
	// block.
	cleared, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleared.Dir(), "quota.json"), []byte(`{"since":"0001-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if blocks := New("testbox", WithState(cleared)).quotaBlocks(); len(blocks) != 0 {
		t.Errorf("a cleared old file read as %+v, want no block", blocks)
	}
}

// The new shape is written, and read back, per credential.
func TestTheQuotaFileIsWrittenPerCredential(t *testing.T) {
	c := newCredentialFixture(t)
	c.list(t)
	raw, err := os.ReadFile(filepath.Join(c.storeDir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Credentials map[string]struct {
			Since string `json:"since"`
		} `json:"credentials"`
		Since string `json:"since"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if len(got.Credentials) != 1 || got.Credentials[credA].Since == "" {
		t.Errorf("quota.json = %s, want one entry, for %q", raw, credA)
	}
	if got.Since != "" {
		t.Errorf("quota.json still carries the old machine-wide since: %s", raw)
	}
}

// quotaEventsWithin collects the machine.quota events the stream delivers.
//
// It returns as soon as want of them have arrived and a short grace has passed
// without another (a test that expects exactly one must see a second one if it
// is coming), or when the window is over. want 0 waits out the whole window:
// the shape of "nothing must be said".
func quotaEventsWithin(t *testing.T, s driver.EventStream, window time.Duration, want ...int) []fleet.MachineQuotaPayload {
	t.Helper()
	const grace = 300 * time.Millisecond
	var out []fleet.MachineQuotaPayload
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		ev, ok := nextWithin(t, s, time.Until(deadline))
		if !ok {
			break
		}
		if ev.Kind == fleet.EventMachineQuota {
			p, ok := ev.Payload.(fleet.MachineQuotaPayload)
			if !ok {
				t.Fatalf("payload type %T, want MachineQuotaPayload", ev.Payload)
			}
			out = append(out, p)
		}
		if len(want) > 0 && len(out) >= want[0] {
			if d := time.Now().Add(grace); d.Before(deadline) {
				deadline = d
			}
		}
	}
	return out
}
